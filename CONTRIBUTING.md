# Contributing

## Ground rules for a public repository

This repository is public and its history cannot be unpublished. Nothing in
it may name a real organisation, cluster, account, environment, team, person,
incident or internal ticket — in code, documents, tests, commit messages or
pull request text. Say "a consuming estate", "an environment", "the source
estate".

`hack/leak-canary.sh` catches the mechanical half of that and runs as part of
`just check` and again in CI. It cannot read prose, so the rest is a review
rule. Quote a placeholder, never a real value, including in a commit message:
a message is as public as a file and cannot be edited after the push.

## The gate

`just check` is the gate and needs no credentials and no cluster. It runs:

- `lint`: the chart lints from its CI values, and the shell scripts parse
  and pass shellcheck at warning level
- `golden`: every case under `tests/cases/` renders to its committed golden,
  and every fixture under `tests/invalid/` is refused for the reason it names
  (`just golden-update` rewrites the goldens; review the diff)
- `schema`: the goldens validate against the Kubernetes API schemas
- `leak-canary`

`just test` runs the Go modules' vet and tests and `just vuln` their
govulncheck; CI runs both, the chart assertions and the image builds in
their own jobs.

## Component contract

This repository is held to the
[component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md);
its `policy-conformance` check runs in CI. It reports as warnings until the
first release (the CHANGELOG rule needs a released version heading) and
fails the build after it.

## Changelog

A pull request that changes what a consumer sees adds its bullet to
`CHANGELOG.md` under `## Unreleased`, or under the version it will be tagged
as. Every tag cut by hand has exactly one heading. A breaking bullet starts
with **Breaking:** and names the adoption step.

## Tooling

Tools come from `devbox.json` through direnv. Never hand-roll a PATH; add a
missing tool with `devbox add <pkg>@<version>`.

## Commits and pull requests

Small, reviewable pull requests. Pull requests merge by rebase, so a branch
carries no merge commits.
