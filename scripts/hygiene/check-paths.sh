#!/usr/bin/env bash
# Refuses paths that must never reach the public remote: local docs, design
# records, and credential-shaped files (spec section 7.6).
#
#   check-paths.sh --staged            paths staged in the index
#   check-paths.sh --range <rev-range> paths touched by any commit in the range
#
# Exit 0: nothing offending. Exit 1: offending paths printed, one per line, to
# stderr. Exit 2: usage or git error (never a silent pass).
#
# Anchoring mirrors .gitignore: docs/, .impeccable/, DESIGN.md and PRODUCT.md are
# private only at the repository root (backend/internal/docs is a Go package);
# credential-shaped file names are refused at any depth.
set -euo pipefail

usage() {
  echo "usage: check-paths.sh --staged | --range <rev-range>" >&2
  exit 2
}

# blocked PATH succeeds when PATH (repository-relative) must not be committed.
blocked() {
  local path=$1 base=${1##*/}
  case $path in
    docs/* | .impeccable/* | DESIGN.md | PRODUCT.md) return 0 ;;
    .terraform/* | */.terraform/*) return 0 ;;
  esac
  case $base in
    .env.example) return 1 ;;
    .env | .env.* | kubeconfig* | *.kubeconfig | *.pem | *.key | *.p12) return 0 ;;
    *-credentials.json | *-sa-key.json | service-account*.json | application_default_credentials.json) return 0 ;;
    *.tfstate* | *.tfplan | tfplan | *.tfvars) return 0 ;;
  esac
  return 1
}

[ $# -ge 1 ] || usage

list=$(mktemp)
trap 'rm -f "$list"' EXIT

# Deleted paths are skipped: removing a private file is the cure, not the offence.
# A range lists every commit's paths, not just the net diff, so a file added and
# deleted within one push (still in the remote's history) is caught.
case $1 in
  --staged)
    [ $# -eq 1 ] || usage
    git diff --cached --name-only -z --diff-filter=d >"$list" || exit 2
    ;;
  --range)
    [ $# -eq 2 ] && [[ $2 != -* ]] || usage
    git log --name-only -z --format= --diff-filter=d "$2" -- >"$list" || exit 2
    ;;
  *) usage ;;
esac

declare -A seen=()
offending=0
while IFS= read -r -d '' path; do
  [ -n "$path" ] || continue
  [ -z "${seen[$path]+x}" ] || continue
  seen[$path]=1
  if blocked "$path"; then
    echo "blocked: $path" >&2
    offending=$((offending + 1))
  fi
done <"$list"

if [ "$offending" -gt 0 ]; then
  echo "check-paths: $offending path(s) must stay off the remote (spec section 7.6)" >&2
  exit 1
fi
