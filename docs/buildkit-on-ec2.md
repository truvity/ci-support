# A BuildKit builder on a machine of its own

`pkg/buildkit/ec2` runs one BuildKit daemon per architecture on an EC2 Auto
Scaling group. It exists because a builder sharing a Kubernetes node with the
kubelet can starve that node under a burst of builds, and a wedged pod in a
StatefulSet is repaired by a person, not by the platform. On its own machine
the daemon is the only tenant, and the repair path is the load balancer's
health check, a local watchdog and the group replacing the instance.

## What runs on the machine

| Piece | From | What it does |
|---|---|---|
| `buildkitd.service` | `pkg/buildkit` | `rootlesskit --net=host buildkitd` as the user `buildkit`; mutual TLS on :1234, the unix socket for local checks, `/metrics` and pprof on 127.0.0.1:6060 only. `MemoryHigh` 85% and `MemoryMax` 92% of the machine, CPU weight 50, `Restart=always`. Not enabled: only a boot into service starts it. |
| `buildkitd.toml` | `pkg/buildkit` | the cache root on the cache volume, `max-parallelism`, garbage collection (`reservedSpace` 10GB, `maxUsedSpace` 80GB, `minFreeSpace` 10GB by default) |
| `buildkit-imds-guard.service` | `pkg/buildkit` | an nftables table that drops traffic to the instance metadata service (IPv4 and IPv6) from every uid but root. A build step shares the host's network, so the hop limit alone would not keep it from the instance's role. |
| The cache volume | `pkg/buildkit` | `/dev/sdf`, formatted xfs on the first boot and mounted at `/var/lib/buildkit` from fstab on every boot. A warm-pool stop keeps it. |
| `asg-ssmcert-buildkit` | asg `modules/ssmcert` | the server certificate, its key and the client CA from SSM SecureStrings into `/run/asg-ssmcert/buildkit` (tmpfs, owned by `buildkit`) on every boot into service; an hourly version check; the nightly rotation through Standby, because buildkitd never reloads its certificate |
| `asg-watchdog` | asg `modules/watchdog` | `buildctl debug workers` over the unix socket every 30 s; after 3 minutes of failure it restarts the daemon (twice), then marks the instance Unhealthy so the group replaces it |
| `asg-in-service`, `bootstrap-ready` | asg | in service: load the certificate, start the daemon, wait for `debug workers` (up to 10 minutes on a large cache), then complete the launch hook. A launch that never gets there is abandoned. |

## What the group looks like

- One group per architecture, `Min` 1 / `Max` 3, a warm pool of one stopped
  instance that takes a scaled-in instance back with its cache
  (`ReuseOnScaleIn`), and the termination policy `NewestInstance`, so a
  scale-in keeps the instance with the warm cache.
- ELB health: the target group's TCP check on 1234 (a wedged daemon that
  still accepts TCP is the watchdog's job).
- Instance refresh launches before it terminates (`MinHealthyPercentage`
  100, `MaxHealthyPercentage` 200); a new image or user-data rolls one
  instance at a time with no gap.
- The security group admits 1234 from the NLB's security group only. On the
  NLB, the listener's port admits only the sources the consumer names (the
  runners' security group).
- A replacement starts with an empty cache; the registry cache the clients
  name is its warm source. The daemon needs no registry permission: the
  client forwards its credentials over the BuildKit session.

## The role

Exactly what the user-data reads (`ssm:GetParameter` on the three
certificate parameters), the group actions of the lifecycle, the watchdog and
the rotation on its own group, `autoscaling:DescribeAutoScalingGroups`, and
`kms:Decrypt` of the parameters' key through SSM, for those parameters only.
The key is named by alias (`kms:ResourceAliases`), so it need not exist when
the stack is previewed; the key's own policy must also admit the role.
`ec2.KeyPolicyStatement` is that half: the statement the caller puts in the
key's policy, one per builder, which admits the role by `aws:PrincipalArn`
(so the key may exist before the role) to `kms:Decrypt` through SSM for the
same three parameters as the role's own grant.

The certificate, its key and the client CA come from the chart's
`externalBuilders` (docs/mtls.md, "Builders outside the cluster"):
cert-manager issues the server certificate in a cluster and External Secrets
pushes it, with the client CA, under the parameter names this package reads.

## The certificate rotation

On a new parameter version the box waits for the window (02:00 to 05:00
UTC), checks that the group is quiet, enters Standby without decrementing the
desired capacity (the warm pool's instance goes in service), waits for its own
connections to drain, loads the new files, restarts the daemon and waits for
`debug workers`, leaves Standby and sets the desired capacity back. A failed
restart terminates only this instance. See asg's ADR 0005.

## On demand

The group mode keeps a builder per architecture running around the clock,
and the machines sit below 1% CPU most of the day. `Mode: ModeOnDemand` keeps
them **stopped** and wakes one when a client connects. The default stays the
group mode: a consumer that does not set `Mode` renders exactly what it did.

```
runner --TCP--> NLB (box-front) --TCP--> wake proxy (always on) --TCP--> builder (stopped until needed)
                  listener :1235 / :1236     :1235 / :1236                  :1234, mutual TLS end to end
```

### The pieces

| Piece | What it is |
|---|---|
| Builder | `ec2.New` with `Mode: ModeOnDemand`: one plain instance (asg `Args.Single`; Pulumi does not manage its run state, so a stopped builder is no diff) with the same launch template as before. A change of image or user-data **replaces** the instance (deleted first, so the volume is free); the replacement comes up running and the proxy stops it after the idle time. Pinned to `OnDemand.Zone`; asg checks at deploy time that `OnDemand.SubnetID` is in it. |
| Cache | asg `Args.Data`: an encrypted, protected, retain-on-delete gp3 volume (`OnDemand.DataVolumeGiB`, default 100) in that one zone, mounted by asg's `datavolume` module at `/var/lib/buildkit`, the daemon's state directory. It survives stops, starts, image and user-data upgrades; the first boot formats it (xfs, label `bkcache`) once and never again. Removing it takes `pulumi state unprotect` and a manual delete. |
| Proxy | `ec2.NewProxy`: asg's `cmd/wake-proxy` (`WakeProxyVersion`, binary and sha256 pinned) on a `t4g.nano`-class appliance in the **group mode** (desired 1, one stopped warm-pool spare, launch hook, ELB health, `ReuseOnScaleIn`, launch-before-terminate refresh), with the `wakeproxy` and `watchdog` modules. One route per builder: it listens on the builder's registered port and finds the builder by the tag `role` = the builder's `Name`. |
| NLB | The proxy owns the shared NLB's listeners and target groups: per route a TCP listener on the builder's registered port (idle timeout 3600 s), an HTTP health check `GET /healthz` on the proxy's health port (200 whenever the proxy runs, never a function of a builder: a sleeping builder is normal). The builders are no longer targets of anything. |
| Security | The builder's security group admits port 1234 from **the proxy's security group** only (`OnDemand.ProxySecurityGroupID`). The proxy's admits the route ports and the health port from the NLB's group only. The proxy holds no certificate and no key: TLS is the client's to the daemon. |

### What replaces what the group mode had

A single instance has no Auto Scaling group, so asg refuses everything that
needs one (launch hook, warm pool, in-service steps, ready checks, group
actions). Each piece the group-mode builder used is replaced or dropped:

| Group mode | On demand |
|---|---|
| Launch hook, in-service steps (load the certificate, start the daemon), ready check (`debug workers`), `bootstrap-ready` | **systemd**: `buildkitd.service` is enabled and starts on every boot, requiring `buildkit-imds-guard.service`, `buildkit-cert.service` and (asg's drop-in) `asg-data-volume.service`. There is no hook to complete. |
| Cache volume in the launch template, formatted and mounted from fstab by the install step | asg `datavolume` module: waits for the device on every boot, formats it only if it carries no signature, mounts it, and the daemon requires it. The unit hands the mount point to `buildkit` before each start. |
| `ssmcert` load as an in-service step | `buildkit-cert.service`, a oneshot that runs the module's own `load` on **every boot** (the instance boots on every wake, and `/run` is empty after a stop). A certificate renewed while the builder slept needs no rotation: the next wake loads it. |
| Nightly rotation through Standby (02:00 to 05:00, replacement from the warm pool) | In place and idle-gated: `buildkit-cert-check.timer` (hourly) runs the module's own `check`; on a new parameter version it waits until port 1234 has had **no established connection for 5 minutes** (a running build keeps one; so does the proxy's pipe), loads the files, restarts the daemon and waits for `debug workers`. The window is 00:00 to 23:00 UTC by default (the idle test is the guard, not the hour); `Rotation.WindowStartHour` / `WindowEndHour` / `IdleMinutes` / `CheckIntervalMinutes` tune it. A builder that is never idle keeps its certificate until it is; the proxy stops an idle one after 20 minutes anyway and the next wake loads the current version. |
| `asg-watchdog` (restart twice, then mark Unhealthy so the group replaces the instance) | **Dropped.** There is no group to replace the instance and nothing to mark Unhealthy. `Restart=always` on the unit covers a crash. A wedged daemon that still holds the port shows to the client as a connection that never answers; the alert is the proxy's `wake failures` and `dial failures` metrics, and the fix is to stop the instance (the proxy starts a fresh boot on the next connect) or replace it with a deploy. |
| Instance refresh, warm pool, `NewestInstance`, deregistration delay | Dropped: one instance; a launch template change replaces it. |
| Role: SSM read, `kms:Decrypt` of the certificate key, group actions | SSM read and `kms:Decrypt` only. No `autoscaling:*` at all. |

### The proxy's role and the key trap

`wakeproxy.Statements`: `ec2:DescribeInstances` on `*` (the action cannot be
scoped); `ec2:StartInstances`, `ec2:StopInstances` and `ec2:CreateTags`
(the activity tag key only) on instances that carry one of the builders' role
tags, by condition. With a **customer-managed key** on the data volumes
(`OnDemand.DataKMSKeyARN`), pass the same ARN as `ProxyArgs.KMSKeyARNs`: the
proxy then also gets `kms:CreateGrant` (AWS resources only), `kms:Decrypt` and
`kms:GenerateDataKeyWithoutPlaintext` on that key. Without them `StartInstances`
succeeds and the instance never leaves `pending`. The key's own policy must
admit the proxy's role as well. The default EBS key needs none of this.

### Timing

A stopped builder is running about 33 s after the start request, and the daemon
needs a few more seconds before its port answers. The proxy holds the client's
connection open for that long (`WakeTimeout`, default 180 s); the listener's
TCP idle timeout (3600 s, the group mode's value) is far above it, and
`NewProxy` refuses a wake timeout that is not below it. `buildx` gives up
after about 40 s, so the first build after a quiet period needs a retry loop
longer than its own connect timeout: that is the caller's.

### Wiring and migration order

1. Create the proxy and the on-demand builders in one stack: `NewProxy` first,
   then one `New` per architecture with `OnDemand.ProxySecurityGroupID:
   proxy.SecurityGroupID()`. Give each route the builder's registered `Port`,
   `Name` (the architecture) and `TargetTagValue` (the builder's `Name`).
   The listener ports clash with the old group's on the same NLB, so either
   give the new builders new registered ports (and switch the runners'
   endpoint), or stage the swap as below.
2. The new data volume starts **empty**: the first builds are cold, which is
   what a replaced group instance was already. The registry cache the clients
   name is the warm source.
3. Verify the new path (a build through the proxy; `wake`, `stop` and `dial`
   counters; the builder stops itself after the idle time and the next build
   wakes it) **before** removing anything.
4. Only then delete the old groups and their warm pools (and the listeners
   they held, if the ports were reused). Until then the old and the new
   builders run side by side and nothing is lost by stopping.
5. Switch one environment first (devel) and the other later: `Mode` is per
   builder, and `ModeGroup` renders byte-identically to before.

### Outputs

`Builder.InstanceID()`, `Builder.DataVolumeID()`, `Builder.SecurityGroupID()`,
`Builder.RoleTagKey` / `RoleTagValue`; `Proxy.GroupName`, `Proxy.SecurityGroupID()`,
`Proxy.HealthPort`, `Proxy.MetricsAddress` (set `ProxyArgs.MetricsAddress` to
serve Prometheus text on the proxy: active connections, wakes, wake failures,
wake duration, stops, dial and resolve failures per route) and
`Proxy.Front` (the listeners' target group ARNs).

## Not built yet

- Telemetry from the machine (an OpenTelemetry collector scraping the
  daemon's `/metrics` and shipping the journal). Until then the consumer's
  probe of the listener is the signal.
- A terminate lifecycle hook that waits for running builds: today the target
  group's deregistration delay (900 s) is the only drain on scale-in.
