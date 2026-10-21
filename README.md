# ci-support

The CI services every GitHub Actions runner uses (caches, docker and nix builders), independent of how the runner is hosted.

| Part | Status | What it is |
|---|---|---|
| Build caches | arrives from ci-plane | bazel-remote, the npm read-through cache, cache-redirect |
| Docker builders | arrives from ci-plane | buildkitd |
| Nix workers | arrives from ci-plane | the nix worker service and its images |
| `ci-builders` chart | arrives from ci-plane | the Helm chart that deploys the services above |

Nothing is published from this repository yet. The content arrives from
[truvity/ci-plane](https://github.com/truvity/ci-plane) in a later migration;
until then the repository holds only its scaffolding: the licence, the
toolchain, the gate and the CI workflow.

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

Nothing is published yet, so there is nothing to install. Each service will
be installed by the `ci-builders` chart from the registry, with an explicit
`--version`, and a values file that names the consumer's own cluster facts;
the first release adds the worked example here.

## Consumers

Who uses this, and through which surface. As of 2026-10-21 the services are
still released from truvity/ci-plane, and the consumers are:

| Consumer | Through |
|---|---|
| GitHub Actions runners on Kubernetes (ARC), see truvity/ci-plane | the `ci-builders` chart |
| GitHub Actions runners on serverless containers, see [truvity/grun](https://github.com/truvity/grun) | the cache and builder endpoints |
| A second, non-AWS estate | the `ci-builders` chart |

## Neighbours

- **[truvity/ci-plane](https://github.com/truvity/ci-plane)**: where this
  content lives today, and the home of the ARC runner scale-set chart, which
  stays there.
- **[truvity/grun](https://github.com/truvity/grun)**: runners on serverless
  containers; a client of the services here.
- **[truvity/ci-workflows](https://github.com/truvity/ci-workflows)** and
  **[truvity/ci-actions](https://github.com/truvity/ci-actions)**: the shared
  workflows and steps that wire a job to these services.
- **[truvity/policy](https://github.com/truvity/policy)**: the component
  contract this repository is held to.

## Documentation

`docs/` arrives with the content. Until then the rules for working in this
repository are in [CONTRIBUTING.md](CONTRIBUTING.md), the reporting policy in
[SECURITY.md](SECURITY.md) and the change history in
[CHANGELOG.md](CHANGELOG.md).

## The rule that makes this repository public

Mechanism only. Account ids, ARNs, hostnames, registries, bucket names and
organisation-specific configuration are inputs supplied by the consumer and
live in the consumer's own configuration repository, never here.
`hack/leak-canary.sh` runs in CI on every pull request and fails the build if
a particular slips in.

## Status

True on 2026-10-21: the repository exists with its scaffolding and a gate that
passes (`just check`); it holds no chart, image or tool, and has no release.

## Development

```
devbox shell
just check
```

`just check` needs no credentials and no cluster. `just --list` shows the
recipes.

## Releasing

No release has been cut. The first release is a hand-cut tag; it also turns
the CI `policy-conformance` job from a report into a gate. Auto-release is
armed per repository, deliberately, and only for patches: every minor and
major stays a hand-cut tag.

## Licence

MIT, see [LICENSE](LICENSE).
