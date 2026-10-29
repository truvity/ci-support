# Day 2 — operating the builders and caches

## Releasing

**A `v*` tag is the release act.** Tag creation is ruleset-restricted;
who can tag is who can publish. A patch tag on an unchanged tree is the
legitimate "pure refresh" release (base-image/CVE pickup — renovate has
already moved the pins on master).

```bash
git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z
```

The Release workflow does the rest: the cache-redirect image built
natively per architecture and merged into a manifest list, its digest
stamped into the chart defaults by key, the chart stamped with the tag's version
and published to `oci://ghcr.io/truvity/ci-support/charts/ci-builders`. Semver intent: patch = refresh/mechanics, minor = chart
features or new tools in the image, major = breaking values. Add the
version's `## vX.Y.Z` heading to [CHANGELOG.md](../CHANGELOG.md) before
tagging.

## The automatic chain (optional, not armed here yet)

This repository has no `auto-release` workflow yet: every release is a
hand-cut tag. The chain below is how ci-plane ran it, and what arming it
here takes. With renovate automerging pin bumps on master, a weekly
`auto-release` workflow can cut the patch tag itself. It is doubly
guarded and dark by default; enabling it takes:

1. a way for the job to get the CI automation App's token. This repo
   uses the keyless one: `auto-release.yaml` exchanges the run's own
   GitHub OIDC identity at the access-roster issuer for an installation
   token of the catalogue App (`token-source: access-roster`,
   `github-app: truvity-ci-automation`), so what has to be in place is
   `vars.ACCESS_ROSTER_ISSUER` and an issuer grant covering this
   repository's auto-release job. **No App private key lives on this
   repo**: the `CI_AUTOMATION_PRIVATE_KEY` secret was deleted on
   2026-09-18 when the exchange went live, and the
   `CI_AUTOMATION_APP_ID` variable that outlived it is read by nothing.
   Do not recreate either — an empty exchange is a configuration fix at
   the issuer, never a new key here. (The shared workflow still offers
   the classic `app-key` source, App id variable + private-key secret,
   for callers with no issuer to exchange against.)
2. the App as a bypass actor on the `v*` tag ruleset;
3. `gh variable set AUTO_RELEASE --body true`.

Why an App and not the built-in token: an App-pushed tag *triggers* the
Release workflow (a `GITHUB_TOKEN` push is inert), and the tagging
privilege stays one named, revocable actor instead of "every workflow
in the repo". Why an exchange and not the App's key: the token is
minted per run and narrowed to this repository, so there is nothing
stored to rotate or leak, and taking the privilege away is a grant edit
at the issuer rather than a secret deletion in every repo that tags.

## Consumer-side promotion

The producer releases and stops. In the consuming estate's repository:

1. Annotate the pin —
   ```yaml
   # renovate: datasource=docker depName=ghcr.io/truvity/ci-support/charts/ci-builders
   ciPlane: "X.Y.Z"
   ```
   The chart's version is the plane's version: one tag releases the
   images and the chart together.
2. A renovate custom regex manager over that file, plus a packageRule
   with `automerge: true` and `postUpgradeTasks` running whatever
   regenerates derived files (renders, goldens) **inside the update
   branch**, so the PR is born green.
3. Hard-won renovate settings that make this work:
   - `platformCommit: "disabled"` — with an App token, renovate's
     GraphQL commit can fail with "unknown error" AFTER postUpgradeTasks
     succeed, silently dropping the branch; plain git commits are the
     provably-working path.
   - the shared renovate workflow must run renovate via **npx, not the
     docker-mode action** (daemonless runners), on **Node ≥ 24**
     (renovate 44 uses `RegExp.escape`), with `allowed-commands`
     matching the postUpgradeTasks commands EXACTLY.
   - the renovate App's variable/secret must actually be scoped to the
     repo — a missing entitlement skips silently.

## Rollback

Revert the consumer's pin PR. The previous charts and image digests are
immutable and still published; ArgoCD walks the fleet back on sync.
Never delete published versions.

## Cache operations

Every cache is disposable: delete the PVC, it re-warms. The builders'
warm layer lives in the registry (`cache-to type=registry`) and
survives builder replacement — expect one slower build cycle after a
cache volume reset, not breakage. The nix, npm and Bazel caches degrade to
upstream when down.

The Gradle build cache of cache-redirect (preview only) is the one cache a job can write. If an entry is ever wrong, delete the object under
`<gradle prefix><key>` (or the whole prefix: every build is then one cold
build, then warm again); the key is the one Gradle prints as
`Build cache key for task ':<task>' is <key>`. `rejected` and `stored` in
`cache_redirect_requests_total{frontend="gradle"}` count the writes.

The npm front-end of cache-redirect keeps a stored copy of every packument
under `<npm prefix>_meta/`: it is what installs fall back to when the registry
fails (`result="stale"` in `cache_redirect_requests_total{frontend="npm"}` says it
is happening; the `upstream_error` and warnings in the log say why). To drop a
bad document, delete its object (or the whole `_meta/` prefix: the next request
fetches it again). Tarballs are immutable and never need it. A project that
installs fine and still shows `registry.npmjs.org` in `yarn.lock`
(`__archiveUrl`) or `pnpm-lock.yaml` (`tarball:`) was resolved from a metadata cache
of the client that predates the cache (yarn keeps one under `~/.yarn/berry/metadata`,
pnpm under its `cache-dir`), not from this service.

cache-redirect's bucket is disposable the same way: an object that
expires or is deleted is only a miss. Its health is in
`cache_redirect_requests_total`: a rising `store_error` is the
bucket's identity or reachability (a 403 is never taken for a miss), a
rising `upstream_error` is the upstream, and `uploads_total{result="failed"}`
counts objects discarded rather than committed half-written. A runner
that cannot follow a redirect (no route to the bucket's endpoint) fails
its download while the service looks healthy: check the runner side
first.

## npm read-through (verdaccio)

Adoption is one line: ARC-pooled repos pass `node-cache: true` to the
shared `check.yaml` (ci-workflows ≥ v2.13.0). The job probes the fixed
name `npm-cache.ci-cache.svc/-/ping` (2s) — so this works only where
ci-builders is installed into a namespace called `ci-cache` — and, when
it answers, sets `npm_config_registry` (npm, yarn classic) and
`YARN_NPM_REGISTRY_SERVER` (berry) for the job. A down cache degrades
the job to *slow* (direct npmjs, one warning line), never to *broken*.
Both paths proven live 2026-08-27: override active on an ARC job;
graceful fallback on a hosted job.

Know your consumer before expecting traffic: **hosted runners cannot
reach the service at all** (public repos ride hosted — do not opt them
in, the warning is pure noise), and a **zero-install yarn repo
(committed `.yarn/cache`) never fetches from any registry during
install** — the override there only catches ad-hoc fetches (`npx`,
`npm exec`, toolchain downloads honoring npm config). The cache earns
its keep when a non-zero-install Node repo lands on the ARC pool.
