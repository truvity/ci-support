# ci-support: the CI services every runner uses (caches, docker and nix
# builders), independent of the runner technology. No code has arrived yet,
# so the gate is the shell scripts and the leak canary; each recipe below
# appears when the thing it checks does. `just --list` for an overview.

default:
    @just --list

# Every shell script parses and passes shellcheck
lint:
    bash -n hack/leak-canary.sh
    shellcheck hack/*.sh

# Why this repository can be public: no particulars in the tracked files.
# Its own CI job.
leak-canary:
    bash hack/leak-canary.sh

# All the checks CI runs
check: lint leak-canary
