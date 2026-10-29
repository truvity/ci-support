# Day 1 — installing the builders and caches on a fresh estate

Order matters: the builders and caches before the runners that use them.

This guide installs `ci-builders`. The `arc-runners` chart (the ARC
runner scale sets) is archived, last released as 7.8.3; its install
steps are in
[this guide at `v7.8.3`](https://github.com/truvity/ci-plane/blob/v7.8.3/docs/day-1-install.md).

## Prerequisites

1. **A namespace for the build plane**, written `<namespace>` below,
   and the **runner namespaces** allowed to reach it. The chart takes
   the namespace from the release; two things outside it still assume
   the maintainers' name `ci-cache`, or want a cache URL:
   - the published runner images list only
     `https://cache.nixos.org/` as their Nix substituter (v7.0.0; earlier
     images baked an in-cluster name). To use the cache-redirect `/nix`
     front-end, give the runners a `NIX_CONFIG` with
     `substituters = http://cache-redirect.<namespace>.svc.cluster.local/nix https://cache.nixos.org/`,
     which beats the image's own nix.conf;
   - truvity/ci-workflows' `node-cache: true` probes
     `npm-cache.ci-cache.svc` by that fixed name.
2. Optional but recommended: a **registry pull-through cache** per
   hosting account, so image pulls are same-region and unthrottled. The
   charts' image references are split `{registry}/{repository}` so you
   override only the registry.
3. Optional: `cacheRedirect`
   (`cacheRedirect.enabled: true`), one service for the Nix cache, the
   Go module proxy, the Maven repositories and the npm registry (and, opt-in, a Gradle
   remote build cache) whose hits on NARs, module zips, jars and npm tarballs are
   redirects to presigned URLs of its bucket, so the bytes flow from the object store
   to the runner directly
   ([architecture](architecture.md#cache-redirect-a-hit-is-a-redirect-not-a-stream)).
   It needs `cacheRedirect.s3.bucket` and `region`,
   `cacheRedirect.networkPolicy.consumerNamespaces`, an identity on its
   ServiceAccount (`cacheRedirect.serviceAccount.name`, default
   `cache-redirect`; runneridentity's `CacheRedirect` builds the AWS role,
   for one prefix `cacheRedirect.nix.prefix`, `.goproxy.prefix`,
   `.maven.prefix`, `.gradle.prefix` and `.npm.prefix` must all sit under: the Maven,
   Gradle and npm defaults, `maven/`, `gradle/` and `npm/`, do not), and a route from the
   runners to the bucket's endpoint. Point runners at it with
   `substituters = http://cache-redirect.<ns>.svc.cluster.local/nix https://cache.nixos.org/`
   and
   `GOPROXY=http://cache-redirect.<ns>.svc.cluster.local/goproxy|https://proxy.golang.org,direct`.
   Gradle and Maven builds use `.../maven/central` (and `/google`,
   `/gradle-plugins`) as their repositories; the Gradle build cache, a
   WRITABLE cache and so off by default (`cacheRedirect.gradle.enabled`,
   on in `presets/preview.yaml` and `presets/stable.yaml`, each tier its own
   deployment and bucket), is `HttpBuildCache` with the url
   `http://cache-redirect.<ns>.svc.cluster.local/gradle/cache/`. npm, yarn and
   pnpm use `http://cache-redirect.<ns>.svc.cluster.local/npm` as their
   registry (`cacheRedirect.npm.enabled`, read-only, on in both presets; it
   replaces the verdaccio of `npmCache`). Behind a
   gateway that routes by name, `cacheRedirect.hosts.{nix,maven,gradle,npm}`
   serve each of those at the root of a host name of its own, so the gateway
   rewrites no path.

## Placement: node classes

The chart knows no node pool, label or taint of any platform. `nodeClasses`,
which you define, carries them.

`nodeClasses` maps a name you choose to a `nodeSelector` and `tolerations`.
`nodeClass` (the whole release) refers to the name, and places the caches
(bazel-remote and the npm cache). The tier presets refer to one class name,
`cache-plane`.

A class a preset or a values file names but you did not define is a render
error, so a typo cannot schedule a pod anywhere. A class that places nothing
is defined empty (`cache-plane: {}`).

- **Talos, kind, fixed nodes.** Map a class to labels you put on the
  nodes (Talos: `machine.nodeLabels`, and `registerWithTaints` for a taint),
  or to nothing. `tests/cases/ci-builders/talos` is a complete values file;
  CI renders it and fails if the render mentions Karpenter or EKS.
- **EKS with Karpenter or Auto Mode.** Map a class to the pool's label and
  taint, e.g. `nodeClasses.cache-plane.nodeSelector:
  {karpenter.sh/nodepool: <pool>}`, and list `presets/karpenter.yaml`.

An explicit release-wide `nodeSelector` / `tolerations`, when not empty,
wins over the release-wide class's own for that one key.

### The Karpenter preset

`presets/karpenter.yaml` is the opt-in for the one assumption a platform
adds beyond placement: `storageClassName: gp3` (EKS's EBS class).

An empty `storageClassName` renders no `storageClassName` field on a PVC
at all, which Kubernetes reads as "use the cluster's default
StorageClass" — not the same as an explicit empty string, which
Kubernetes reads as "no class".

### Presets: the same inputs, as opt-in files

Tier presets are plain values files under `presets/`, so Argo CD takes them
as `helm.valueFiles` (with `helm.valuesObject` after them for the estate's
own facts, which win) and Helm as `-f`:

| preset | what it sets |
|---|---|
| `presets/karpenter.yaml` | the Karpenter assumptions above; opt-in |
| `presets/preview.yaml` | all caches on, on class `cache-plane` |
| `presets/stable.yaml` | pull-through caches only, on class `cache-plane` |

A preset holds no registry, organization, CA key, OpenBao address,
namespace or bucket: those stay the caller's values, and the chart still
refuses to render without the ones it requires.

## Install the cache plane

```bash
helm install ci-builders oci://ghcr.io/truvity/ci-support/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set npmCache.networkPolicy.consumerNamespaces={<runner namespace>}
  # storage class, node classes, registry overrides as needed
```

**Required, with no default:** each enabled component's
`networkPolicy.consumerNamespaces` — `npmCache`, `bazelRemote` and
`cacheRedirect` — while its `networkPolicy.enabled` is true
(the default). A cache behind a policy that admits nobody cannot work,
so the chart's `values.schema.json` refuses the empty list instead of
rendering it. List your runner namespaces, or set that component's
`networkPolicy.enabled: false`.

Key values (see the chart's values.yaml for the full annotated set):
`nodeClasses`, `storageClassName` (empty by default; see "Placement" above).

The builders themselves are not in this chart: they are
`pkg/buildkit/ec2`'s ([buildkit-on-ec2.md](buildkit-on-ec2.md)). What the
chart renders for them (the client CA, the client certificates and the
builders' server certificates) is [mtls.md](mtls.md).

## Wiring workflows

Runners in the cluster reach the caches by cluster DNS (the builders are
outside it, behind mutual TLS, [mtls.md](mtls.md)):
`cache-redirect.<namespace>.svc/nix` (nix substituter), `/goproxy` (GOPROXY) and `/maven/<repo>` (Gradle and Maven repositories),
`bazel-remote.<namespace>.svc:9092` (moon remote cache; the port declares `appProtocol: kubernetes.io/h2c`, so a gateway can publish it over an HTTPRoute),
`npm-cache.<namespace>.svc` (yarn/npm/pnpm `npmRegistryServer`, public
packages, behind a health-probe fallback to registry.npmjs.org).
