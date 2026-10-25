# ci-support

The CI services every GitHub Actions runner uses (caches, docker and nix builders), independent of how the runner is hosted.

| Part | Where | What it is |
|---|---|---|
| `ci-builders` chart | `charts/ci-builders` | the Helm chart that deploys the services below |
| cache-redirect | `image/cache-redirect` | a pull-through cache for Go modules, Nix, Maven, Gradle and npm that answers a hit with a redirect to a presigned URL of its bucket |
| Docker builders | the chart's `buildkitd` | rootless BuildKit per architecture, optionally behind mutual TLS |
| Other caches | the chart's `bazelRemote`, `npmCache` | bazel-remote and the npm read-through cache |
| Nix workers | the chart's `nixWorkers` | persistent remote Nix builders; their image is still published by [truvity/ci-plane](https://github.com/truvity/ci-plane) |

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
each builder):

```bash
helm install ci-builders oci://ghcr.io/truvity/ci-support/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set buildkitd.networkPolicy.consumerNamespaces={<runner namespace>} \
  --set npmCache.networkPolicy.consumerNamespaces={<runner namespace>}
```

[docs/day-1-install.md](docs/day-1-install.md) walks through a fresh
install, [docs/mtls.md](docs/mtls.md) the builders behind mutual TLS.

## Consumers

Who uses this, and through which surface (2026-10-25):

| Consumer | Through |
|---|---|
| GitHub Actions runners on serverless containers, see [truvity/grun](https://github.com/truvity/grun) | the cache and builder endpoints |
| A second, non-AWS estate | the `ci-builders` chart |

## Neighbours

- **[truvity/ci-plane](https://github.com/truvity/ci-plane)**: where this
  content came from. It still publishes the runner and nix-worker images
  the chart names, and the archived ARC runner scale-set chart.
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
- [docs/mtls.md](docs/mtls.md): the builders behind mutual TLS.

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

True on 2026-10-25: the `ci-builders` chart and cache-redirect have moved
here from ci-plane and pass the gate (`just check`); no release has been cut
from this repository yet.

## Development

```
devbox shell
just check
```

`just check` needs no credentials and no cluster. `just --list` shows the
recipes.

## Releasing

A `v*` tag is the release act (`.github/workflows/release.yaml`); see
[docs/day-2-operations.md](docs/day-2-operations.md). No release has been cut
yet. The first release is a hand-cut tag; it also turns the CI
`policy-conformance` job from a report into a gate. Auto-release is
armed per repository, deliberately, and only for patches: every minor and
major stays a hand-cut tag.

## Licence

MIT, see [LICENSE](LICENSE).
