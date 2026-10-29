# A BuildKit builder on a machine of its own

`pkg/buildkit/ec2` runs one BuildKit daemon per architecture on its own EC2
instance. It exists because a builder sharing a Kubernetes node with the
kubelet can starve that node under a burst of builds, and a wedged pod in a
StatefulSet is repaired by a person, not by the platform. On its own machine
the daemon is the only tenant. The machine is **stopped** when nobody builds,
and a small always-on wake proxy starts it when a client connects.

Since v1.0.0 this is the only mode. The group mode of earlier releases (an
Auto Scaling group per builder, a warm pool, a launch hook, the group
watchdog and the Standby rotation) was removed; see [CHANGELOG.md](../CHANGELOG.md).

```
runner --TCP--> NLB (box-front) --TCP--> wake proxy (always on) --TCP--> builder (stopped until needed)
                  listener :1235 / :1236     :1235 / :1236                  :1234, mutual TLS end to end
```

## What runs on the machine

| Piece | From | What it does |
|---|---|---|
| `buildkitd.service` | `pkg/buildkit` | `rootlesskit --net=host buildkitd` as the user `buildkit`; mutual TLS on :1234, the unix socket for local checks, `/metrics` and pprof on 127.0.0.1:6060 only. `MemoryHigh` 85% and `MemoryMax` 92% of the machine, CPU weight 50, `Restart=always`. Enabled: it starts on every boot, requiring `buildkit-imds-guard.service`, `buildkit-cert.service` and (asg's drop-in) `asg-data-volume.service`. |
| `buildkitd.toml` | `pkg/buildkit` | the cache root on the data volume, `max-parallelism`, garbage collection (`reservedSpace` 10GB, `maxUsedSpace` 80GB, `minFreeSpace` 10GB by default) |
| `buildkit-imds-guard.service` | `pkg/buildkit` | an nftables table that drops traffic to the instance metadata service (IPv4 and IPv6) from every uid but root. A build step shares the host's network, so the hop limit alone would not keep it from the instance's role. |
| The cache volume | asg `modules/datavolume` | waits for the device on every boot, formats it (xfs, label `bkcache`) only if it carries no signature, mounts it at `/var/lib/buildkit`, and the daemon requires it. The unit hands the mount point to `buildkit` before each start. |
| `buildkit-cert.service`, `buildkit-cert-check.timer` | asg `modules/ssmcert` | the server certificate, its key and the client CA from SSM SecureStrings into `/run/asg-ssmcert/buildkit` (tmpfs, owned by `buildkit`) on **every boot**, and an hourly version check that rotates in place when idle |

A replacement starts with an empty cache; the registry cache the clients name
is its warm source. The daemon needs no registry permission: the client
forwards its credentials over the BuildKit session.

## The role

Exactly what the user-data reads (`ssm:GetParameter` on the three
certificate parameters) and `kms:Decrypt` of the parameters' key through SSM,
for those parameters only. There is no `autoscaling:*` action. The key is
named by alias (`kms:ResourceAliases`), so it need not exist when the stack
is previewed; the key's own policy must also admit the role.
`ec2.KeyPolicyStatement` is that half: the statement the caller puts in the
key's policy, one per builder, which admits the role by `aws:PrincipalArn`
(so the key may exist before the role) to `kms:Decrypt` through SSM for the
same three parameters as the role's own grant.

The certificate, its key and the client CA come from the chart's
`externalBuilders` (docs/mtls.md, "Builders outside the cluster"):
cert-manager issues the server certificate in a cluster and External Secrets
pushes it, with the client CA, under the parameter names this package reads.

## The pieces

| Piece | What it is |
|---|---|
| Builder | `ec2.New`: one plain instance (asg `Args.Single`; Pulumi does not manage its run state, so a stopped builder is no diff). A change of image or user-data **replaces** the instance (deleted first, so the volume is free); the replacement comes up running and the proxy stops it after the idle time. Pinned to `OnDemand.Zone`; asg checks at deploy time that `OnDemand.SubnetID` is in it. |
| Cache | asg `Args.Data`: an encrypted, protected, retain-on-delete gp3 volume (`OnDemand.DataVolumeGiB`, default 100) in that one zone, mounted at `/var/lib/buildkit`, the daemon's state directory. It survives stops, starts, image and user-data upgrades. Removing it takes `pulumi state unprotect` and a manual delete. |
| Proxy | `ec2.NewProxy`: asg's `cmd/wake-proxy` (`WakeProxyVersion`, binary and sha256 pinned) on a `t4g.nano`-class appliance in its own Auto Scaling group (desired 1, one stopped warm-pool spare, launch hook, ELB health, `ReuseOnScaleIn`, launch-before-terminate refresh), with the `wakeproxy` and `watchdog` modules. One route per builder: it listens on the builder's registered port and finds the builder by the tag `role` = the builder's `Name`. The proxy's group is asg's and is unchanged; it is the only Auto Scaling group this package creates. |
| NLB | The proxy owns the shared NLB's listeners and target groups: per route a TCP listener on the builder's registered port (idle timeout 3600 s), an HTTP health check `GET /healthz` on the proxy's health port (200 whenever the proxy runs, never a function of a builder: a sleeping builder is normal). The builders are not targets of anything. |
| Security | The builder's security group admits port 1234 from **the proxy's security group** only (`OnDemand.ProxySecurityGroupID`). The proxy's admits the route ports and the health port from the NLB's group only. The proxy holds no certificate and no key: TLS is the client's to the daemon. |

### Certificate fetch and rotation

The instance boots on every wake and `/run` is empty after a stop, so
`buildkit-cert.service`, a oneshot, runs the module's own `load` on **every
boot**, before the daemon. A certificate renewed while the builder slept needs
no rotation: the next wake loads it.

For a builder that stays up, `buildkit-cert-check.timer` (hourly) runs the
module's own `check`. On a new parameter version it waits until port 1234 has
had **no established connection for 5 minutes** (a running build keeps one; so
does the proxy's pipe), loads the files, restarts the daemon and waits for
`debug workers`. The window is 00:00 to 23:00 UTC by default (the idle test is
the guard, not the hour); `Rotation.WindowStartHour` / `WindowEndHour` /
`IdleMinutes` / `CheckIntervalMinutes` tune it. A builder that is never idle
keeps its certificate until it is; the proxy stops an idle one after 20
minutes anyway and the next wake loads the current version.

There is no watchdog: with no group there is nothing to replace the instance.
`Restart=always` on the unit covers a crash. A wedged daemon that still holds
the port shows to the client as a connection that never answers; the alert is
the proxy's `wake failures` and `dial failures` metrics, and the fix is to stop
the instance (the proxy starts a fresh boot on the next connect) or replace it
with a deploy.

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
TCP idle timeout (3600 s) is far above it, and `NewProxy` refuses a wake
timeout that is not below it. `buildx` gives up after about 40 s, so the first
build after a quiet period needs a retry loop longer than its own connect
timeout: that is the caller's.

### Wiring

Create the proxy and the builders in one stack: `NewProxy` first, then one
`New` per architecture with `OnDemand.ProxySecurityGroupID:
proxy.SecurityGroupID()`. Give each route the builder's registered `Port`,
`Name` (the architecture) and `TargetTagValue` (the builder's `Name`). The
data volume starts **empty**: the first builds are cold; the registry cache the
clients name is the warm source. Verify the path (a build through the proxy;
the `wake`, `stop` and `dial` counters; the builder stops itself after the idle
time and the next build wakes it) before removing anything it replaces.

### Outputs

`Builder.InstanceID()`, `Builder.DataVolumeID()`, `Builder.SecurityGroupID()`,
`Builder.RoleTagKey` / `RoleTagValue`; `Proxy.GroupName`, `Proxy.SecurityGroupID()`,
`Proxy.HealthPort`, `Proxy.MetricsAddress` (set `ProxyArgs.MetricsAddress` to
serve Prometheus text on the proxy: active connections, wakes, wake failures,
wake duration, stops, dial and resolve failures per route),
`Proxy.MetricsListenPort` and `Proxy.Front` (the listeners' target group ARNs).

### Scraping the proxy's metrics across the VPC

`MetricsAddress` alone (`127.0.0.1:9102`) is reachable only from the proxy
instance, and the instance is replaced by every refresh. To scrape it from
inside the VPC by a stable name, publish it on the shared NLB:

```go
ec2.ProxyArgs{
    // ...
    MetricsListenPort: 9102, // the port registry's entry on the NLB
    MetricsSources: []front.Source{{SecurityGroupID: scraperSG, Description: "metrics scraper"}},
}
```

This adds a TCP listener on the port, a target group forwarding to the same
port on the proxy instances (the group is attached to it, so a replacement is
registered by itself) and a rule admitting the NLB's security group to that
port. The proxy binds `0.0.0.0:<port>` (leave `MetricsAddress` empty, or give
a non-loopback host with the same port). The target group's health check is the
proxy's `/healthz` on the health port, like the routes': the group replaces an
instance when the proxy is down, not when only the metrics server is. Scrape
`http://<NLB DNS name>:<port>/metrics`, for example with a static scrape
target on the NLB's stable record. The endpoint has no authentication or TLS:
list only the scrapers in `MetricsSources` (`MetricsListener` renames the
listener, default `buildkit-proxy-metrics`). With `MetricsListenPort` unset
nothing changes.

### Alerts

The `ci-builders` chart ships the proxy's alert rules as one
`VMRule` (VictoriaMetrics operator, `operator.victoriametrics.com/v1beta1`,
group `wake-proxy`). Off by default; per cluster:

```yaml
wakeProxy:
  alerts:
    enabled: true
    clusterName: my-cluster      # required: labels every alert
    # clusterLabel: k8s_cluster_name
    # job: wake-proxy            # the scrape job of the proxy's /metrics
    # name: wake-proxy-alerts    # the VMRule, in the release namespace
    # thresholds:
    #   down: {for: 5m}
    #   absent: {for: 15m}
    #   wakeFailures: {window: 15m}
    #   builderUnreachable: {window: 15m}
    #   slowWake: {window: 30m, seconds: 120}
```

The VMRule is rendered only when the cluster serves the operator's API
(`.Capabilities.APIVersions`); without it the chart renders nothing for it and
does not fail. The scrape itself (a static target on the metrics listener, path
`/metrics`, job `wake-proxy`) is the estate's own: the chart renders rules, not
scrape objects, because the scraper owns the labels it stamps on a series. The
rules need the series to carry `clusterLabel`.

| Alert | Severity | Fires when | Metric |
| --- | --- | --- | --- |
| `WakeProxyDown` | critical | the scrape fails for `down.for` (5m) | `up{job}` |
| `WakeProxyAbsent` | critical | no `up` series for the cluster for `absent.for` (15m) | `up{job,<clusterLabel>}` |
| `WakeProxyWakeFailures` | warning | any failed wake in `wakeFailures.window` (15m), per `route` | `wake_proxy_wake_failures_total` |
| `WakeProxyBuilderUnreachable` | warning | any dial or resolve failure after a wake in `builderUnreachable.window` (15m), per `route` | `wake_proxy_dial_failures_total`, `wake_proxy_resolve_failures_total` |
| `WakeProxySlowWake` | warning | the mean wake takes more than `slowWake.seconds` (120) over `slowWake.window` (30m) | `wake_proxy_wake_duration_seconds_sum` / `_count` |

The counters restart with every proxy replacement, so the rules read
`increase()` and `rate()`, never the raw counter. The `route` label is the
architecture the builder serves.

## Not built yet

- Telemetry from the machine (an OpenTelemetry collector scraping the
  daemon's `/metrics` and shipping the journal). Until then the consumer's
  probe of the listener is the signal.
- A terminate lifecycle hook for the proxy that waits for running builds:
  today the target group's deregistration delay (300 s) is the only drain
  when the proxy's instance is replaced.
