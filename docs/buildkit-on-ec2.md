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

## The certificate rotation

On a new parameter version the box waits for the window (02:00 to 05:00
UTC), checks that the group is quiet, enters Standby without decrementing the
desired capacity (the warm pool's instance goes in service), waits for its own
connections to drain, loads the new files, restarts the daemon and waits for
`debug workers`, leaves Standby and sets the desired capacity back. A failed
restart terminates only this instance. See asg's ADR 0005.

## Not built yet

- Telemetry from the machine (an OpenTelemetry collector scraping the
  daemon's `/metrics` and shipping the journal). Until then the consumer's
  probe of the listener is the signal.
- A terminate lifecycle hook that waits for running builds: today the target
  group's deregistration delay (900 s) is the only drain on scale-in.
