#!/usr/bin/env bash
# This repository is public and its history cannot be unpublished — a
# rewrite changes the SHAs but not what was already fetched. So the rule
# ("mechanism only; particulars are caller inputs or org variables") is
# enforced mechanically rather than remembered.
#
# Vendored from the component contract's canonical copy (truvity/policy,
# hack/leak-canary.sh), without the one path exception for that repository's
# example chart, and with two patterns added: the name of a private
# organisation and an internal ticket key. Add a pattern here the first
# time something new turns out to be a particular. Never add an exception
# without one.
set -uo pipefail

# The 12-digit patterns are anchored on word boundaries. Without them,
# `[0-9]{12}` also matches a 12-digit run that happens to fall inside a
# longer hex string -- and a nixpkgs commit SHA is exactly that. `\b` keeps
# every real shape (bare, in an ARN, as an ECR host: each is bounded by a
# non-word character) and drops the hex-embedded ones.
#
# The parameter-store pattern excludes `/var/run/secrets/`, which is where
# Kubernetes mounts a pod's own credentials and is therefore in any manifest
# that reads one. A pattern that fires on the single most common path
# convention in the ecosystem does not make anyone safer: it teaches the next
# person to rename their mount to get past it, and the one after that to stop
# reading the output.
patterns=(
  '\b[0-9]{12}\b'                          # AWS account id
  'arn:aws'                                # any ARN
  '\b[0-9]{12}\.dkr\.ecr\.'                # ECR registry host
  '\.svc\.cluster\.local'                  # in-cluster DNS
  '(?<!/var/run)/secrets/'                 # parameter-store paths, but not Kubernetes' own
  'truvity-[a-z0-9-]*-(ci-cache|artifacts|state)'   # S3 buckets
  '\.truvity\.(xyz|com|co)'                # internal hostnames
  'glpat-|ghp_|github_pat_'                # tokens, in case of an accident
  '(?i)op[w]erm'                           # a private organisation's name (spelled so the script does not contain it)
  '\bINF-[0-9]+\b'                       # an internal ticket key
)

# What a hit may look like and still be mechanism, one regex per pattern
# above (same order; empty allows nothing), as ci-plane's copy has it since
# the ci-builders chart arrived from there. Matched against the whole
# `file:line:text` hit, so an allowance can name its file.
allows=(
  '123456789012'                       # the documented placeholder account
  ''
  '123456789012\.dkr\.ecr\.'           # the documented placeholder registry
  # A templated or placeholder namespace is how the chart NAMES a Service,
  # not which one; kube-dns in kube-system is Kubernetes' own, not an
  # estate's.
  '(%s|example|<namespace>|<ns>)\.svc\.cluster\.local|kube-dns\.kube-system\.svc\.cluster\.local'
  ''
  ''
  ''
  ''
  ''
  ''
)

if [ "${#patterns[@]}" -ne "${#allows[@]}" ]; then
  echo "leak-canary.sh: ${#patterns[@]} patterns but ${#allows[@]} allowances" >&2
  exit 2
fi

fail=0

# Scan TRACKED FILES ONLY. The point of this canary is to stop particulars
# being committed, so git's index is exactly the right scope -- and a
# recursive walk of the working tree is not. It descended into generated,
# gitignored directories: .devbox/state.json carries a
# `nix_print_dev_env_hash` whose hex contains a 12-digit run, which matched
# the AWS-account-id pattern. That made the canary fail on a clean checkout
# for a value that is neither committed nor secret.
#
# This matters more than a nuisance: a canary that cries wolf is one people
# learn to skip, and this one is what stands between us and publishing
# particulars from a public repo.
mapfile -d '' tracked < <(git ls-files -z)

for i in "${!patterns[@]}"; do
  p=${patterns[$i]}
  allow=${allows[$i]}
  # Exclude this script (it necessarily contains the patterns it bans) and
  # the lock file (public dependency data by definition).
  hits=$(printf '%s\0' "${tracked[@]}" \
           | grep -zZv -e '^hack/leak-canary\.sh$' -e '^devbox\.lock$' \
           | xargs -0 -r grep -InP "$p" 2>/dev/null)
  if [ -n "$allow" ] && [ -n "$hits" ]; then
    hits=$(grep -vE "$allow" <<< "$hits")
  fi
  if [ -n "$hits" ]; then
    echo "LEAK: pattern /$p/ matched — particulars belong in caller inputs or org variables:"
    echo "$hits" | head -5 | sed 's/^/    /'
    fail=1
  fi
done

if [ "$fail" = 0 ]; then
  echo "leak canary clean — ${#patterns[@]} patterns checked, no particulars found"
fi
exit $fail
