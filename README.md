# ci-support

The CI services every GitHub Actions runner uses (caches and docker builders), independent of how the runner is hosted.

| Part | Where | What it is |
|---|---|---|
| `ci-builders` chart | `charts/ci-builders` | the Helm chart that deploys the services below |
| cache-redirect | `image/cache-redirect` | a pull-through cache for Go modules, Nix, Maven, Gradle and npm that answers a hit with a redirect to a presigned URL of its bucket |
| Builder trust | the chart's `buildkitd.mtls.clientCA`, `externalBuilders` | the client CA, the client certificates of runners outside the cluster, and the builders' server certificates pushed to a store (see [docs/mtls.md](docs/mtls.md)) |
| Other caches | the chart's `bazelRemote`, `npmCache` | bazel-remote and the npm read-through cache |
| Docker builders | `pkg/buildkit`, `pkg/buildkit/ec2` | Go libraries: the BuildKit daemon rendered for a machine of its own (provider-neutral), and Pulumi components that run it as one on-demand EC2 instance per architecture behind a wake proxy on a shared network load balancer, with [truvity/asg](https://github.com/truvity/asg) |

The chart and cache-redirect moved here from
[truvity/ci-plane](https://github.com/truvity/ci-plane). One `v*` tag
publishes the image at `ghcr.io/truvity/ci-support/cache-redirect` and the
chart at `oci://ghcr.io/truvity/ci-support/charts/ci-builders`, with every
image digest stamped into the chart defaults.

## Who it is for

Anyone who runs GitHub Actions runners and wants the services around them to
outlive the runner technology. The services here sit beside the runners, not
in them: a runner on a Kubernetes cluster, on a serverless container platform
or on bare metal talks to the same caches and builders over the network.

It assumes a Kubernetes cluster to run the services on. It deliberately does
not install the runners themselves (see [Neighbours](#neighbours)), and it
holds no estate values: account ids, hostnames, registries and regions are
inputs supplied by the consumer.

## The model

Three nouns:

- **Cache**: a service a build reads from and writes to so that repeated work
  is paid for once (compiled Bazel actions, npm packages, Go modules and Nix
  store paths).
- **Builder**: a service that does the build work on behalf of a runner, so
  the runner needs no privileged container runtime (a docker builder, a Nix
  worker).
- **Runner**: not in this repository. Whatever executes the workflow job,
  reaching the caches and builders as a client.

The services are indifferent to which runner calls them; that independence is
the reason for the split from the runner images.

## Install and a worked example

Install the chart from the registry with an explicit `--version` and a
values file that names your own cluster facts. The chart refuses to render
without the ones it requires (for example the namespaces allowed to reach
each cache):

```bash
helm install ci-builders oci://ghcr.io/truvity/ci-support/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set npmCache.networkPolicy.consumerNamespaces={<runner namespace>}
```

[docs/day-1-install.md](docs/day-1-install.md) walks through a fresh
install, [docs/mtls.md](docs/mtls.md) the builders' mutual TLS.

The builders on EC2 are a Go library, pinned in the consumer's `go.mod`:

```bash
go get github.com/truvity/ci-support@v1.0.0
```

```go
proxy, err := ec2.NewProxy(ctx, "buildkit-proxy", &ec2.ProxyArgs{
    Name: "<env>-buildkit-proxy", Region: region, VPCID: vpc, SubnetIDs: private,
    NLB: sharedNLB, Sources: []front.Source{{SecurityGroupID: runnerSG}},
    Routes: []ec2.ProxyRoute{{Name: "arm64", Port: 1235, TargetTagValue: "<env>-buildkit-arm64"}},
})

b, err := ec2.New(ctx, "buildkit-arm64", &ec2.Args{
    Arch: "arm64", Name: "<env>-buildkit-arm64", InstanceType: "m7g.xlarge",
    VPCID: vpc, PermissionsBoundary: boundaryARN,
    Port: 1235, // from the consumer's port registry
    Certificate: ec2.Certificate{
        Cert: "/<prefix>/arm64/cert", Key: "/<prefix>/arm64/key", ClientCA: "/<prefix>/client-ca",
        KeyAlias: "alias/<key of those parameters>",
    },
    Daemon: buildkit.Config{MaxParallelism: 6},
    OnDemand: &ec2.OnDemandArgs{
        SubnetID: subnet, Zone: "<zone>", ProxySecurityGroupID: proxy.SecurityGroupID(),
    },
})
```

[docs/buildkit-on-ec2.md](docs/buildkit-on-ec2.md) describes the machine. Each
builder is a single **stopped** instance with a persistent cache volume,
behind an always-on wake proxy (`ec2.NewProxy`) that starts it when a client
connects and stops it after 20 idle minutes. v1.0.0 removed the group mode
(an Auto Scaling group with a warm pool per builder); see
[CHANGELOG.md](CHANGELOG.md).

## Consumers

Who uses this, and through which surface (2026-10-25):

| Consumer | Through |
|---|---|
| GitHub Actions runners on serverless containers, see [truvity/grun](https://github.com/truvity/grun) | the cache and builder endpoints |
| A second, non-AWS estate | the `ci-builders` chart |
| The builders on EC2, in the consumer's own deploy program | `pkg/buildkit/ec2` |

## Neighbours

- **[truvity/ci-plane](https://github.com/truvity/ci-plane)**: where this
  content came from. It still publishes the runner images and the archived
  ARC runner scale-set chart.
- **[truvity/grun](https://github.com/truvity/grun)**: runners on serverless
  containers; a client of the services here.
- **[truvity/ci-workflows](https://github.com/truvity/ci-workflows)** and
  **[truvity/ci-actions](https://github.com/truvity/ci-actions)**: the shared
  workflows and steps that wire a job to these services.
- **[truvity/policy](https://github.com/truvity/policy)**: the component
  contract this repository is held to.

## Documentation

- [docs/architecture.md](docs/architecture.md): the load-bearing decisions.
- [docs/day-1-install.md](docs/day-1-install.md): installing on a fresh estate.
- [docs/day-2-operations.md](docs/day-2-operations.md): releasing and operating.
- [docs/mtls.md](docs/mtls.md): the builders' mutual TLS.
- [docs/buildkit-on-ec2.md](docs/buildkit-on-ec2.md): a builder on a machine of its own.

The rules for working in this repository are in
[CONTRIBUTING.md](CONTRIBUTING.md), the reporting policy in
[SECURITY.md](SECURITY.md) and the change history in
[CHANGELOG.md](CHANGELOG.md).

## The rule that makes this repository public

Mechanism only. Account ids, ARNs, hostnames, registries, bucket names and
organisation-specific configuration are inputs supplied by the consumer and
live in the consumer's own configuration repository, never here.
`hack/leak-canary.sh` runs in CI on every pull request and fails the build if
a particular slips in.

## Status

True on 2026-10-26: v0.1.0 is the first release. The `ci-builders` chart and
cache-redirect moved here from ci-plane with byte-identical goldens.
`pkg/buildkit/ec2` is tested against Pulumi mocks and fake commands; no
builder made by it has booted yet, so the first deploy is its first real
test.

## Development

```
devbox shell
just check
```

`just check` needs no credentials and no cluster. `just --list` shows the
recipes.

## Releasing

A `v*` tag is the release act (`.github/workflows/release.yaml`); see
[docs/day-2-operations.md](docs/day-2-operations.md). The same tag versions
the Go module. Releases are hand-cut tags; the CI `policy-conformance` job is
a gate. Auto-release is not armed.

## Licence

MIT, see [LICENSE](LICENSE).
