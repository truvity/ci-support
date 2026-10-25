# Changelog

Tags `vX.Y.Z` version the repository: charts, images and tools are released
together under one version.

## Unreleased

- Create the repository: the licence, the README, the contributing and security notes, the toolchain (`devbox.json`), the `Justfile` gate (`lint`, `leak-canary`), the leak canary and the CI workflow that holds the repository to the component contract. No chart, image or tool is published yet; the caches, the docker and nix builders and the `ci-builders` chart move here from truvity/ci-plane in a later migration, with their own entries.
- The leak canary's pattern for a private organisation's name is spelled so that the script itself does not contain the name.
- Move the `ci-builders` chart and cache-redirect here from truvity/ci-plane, with the chart's CI values, golden renders, refused fixtures and their checks (`golden`, `schema`), the digest stamper, the release-image and Renovate-coverage guards, the shared Go lint configuration and the chart's documents. The goldens are byte-identical to ci-plane's at the move.
- The chart's `cacheRedirectImage.repository` default is now `truvity/ci-support/cache-redirect`; the goldens change in exactly those image lines. cache-redirect's Go module path is `github.com/truvity/ci-support/image/cache-redirect`, its User-Agent names this repository, and its release image is labelled MIT (the repository's licence; the ci-plane label said Apache-2.0).
- The release workflow publishes the image to `ghcr.io/truvity/ci-support/cache-redirect` and the chart to `oci://ghcr.io/truvity/ci-support/charts/ci-builders` (not `ghcr.io/truvity/charts/ci-builders`, which keeps ci-plane's version line). It stamps `nixWorkerImage` and `runnerImage` with the digests of the ci-plane release `CI_PLANE_IMAGES` names (v8.0.0), so the chart stays digest-pinned to every image it can run.
- The gate gains `golden`, `schema`, `test` and `vuln` recipes; `lint` adds the chart lint and runs shellcheck at warning level. The leak canary gains ci-plane's per-pattern allowances for the documented placeholder account and registry and for templated or placeholder in-cluster Service names.
