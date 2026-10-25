# ci-support: the CI services every runner uses (caches, docker and nix
# builders), independent of the runner technology. Tools come from
# devbox.json (`devbox shell`, or direnv). CI runs `lint`, `golden`,
# `schema` and `leak-canary` as recipes of the shared check workflow
# (truvity/ci-workflows), each as its own job; the chart assertions, the
# image builds and the Go job are ci.yaml's own jobs. `just --list` for an
# overview.

charts := "ci-builders"
modules := "image/cache-redirect"

default:
    @just --list

# Lint the chart, from the CI base values (it does not render bare: it
# refuses to guess the estate facts it needs), and every shell script:
# it parses and passes shellcheck at warning level.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    for c in {{ charts }}; do
        helm lint "charts/$c" -f "tests/ci/$c.yaml"
    done
    for f in hack/*.sh; do bash -n "$f"; done
    shellcheck -S warning hack/*.sh

# Render every tests/cases/<chart>/<case>/values.yaml and compare it with
# tests/golden/<chart>/<case>.yaml; prove every tests/invalid/<chart>/
# fixture is refused, for the reason its first line names.
golden:
    bash hack/golden.sh

# Rewrite the goldens after a template change. Review the diff.
golden-update:
    bash hack/golden.sh update

# Schema-validate the golden renders (kubeconform -strict against the
# Kubernetes API): the goldens compare text, this proves the API server
# would accept what they say.
schema:
    bash hack/schema.sh

# Why this repository can be public: no particulars in the tracked files.
# Its own CI job.
leak-canary:
    bash hack/leak-canary.sh

# The Go modules under image/, each its own module.
test:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{ modules }}; do
        (cd "$m" && test -z "$(gofmt -l .)" && go vet ./... && go test -count=1 ./...)
    done

# Reachable Go advisories in the modules. Runs from security.yaml, daily
# and on every change, and is deliberately NOT part of `check`: an advisory
# published overnight must not turn every pull request red.
vuln:
    #!/usr/bin/env bash
    set -euo pipefail
    for m in {{ modules }}; do
        (cd "$m" && govulncheck ./...)
    done

# All the checks CI runs as recipes (the pull-request gate on a laptop).
check: lint golden schema leak-canary
