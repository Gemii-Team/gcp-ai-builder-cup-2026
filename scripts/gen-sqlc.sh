#!/usr/bin/env bash
# Runs sqlc for every domain in backend/sqlc.yaml whose query file exists.
#
# sqlc stops with an error on a query file that does not exist yet and has no
# glob, so the domains that have not written theirs are left out of a temporary
# copy of the config. Every skipped domain is printed. A skipped domain whose
# store directory already holds Go files (generated code whose queries were
# deleted or renamed) fails the run instead of going stale unnoticed.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)/backend"

# Relative paths in a sqlc config resolve against the config's directory, so
# the copy lives next to the original and is removed however the script ends.
cfg="$(mktemp .sqlc.XXXXXX)"
trap 'rm -f "$cfg"' EXIT

keep=""
conflict=0
while read -r queries out; do
  domain="${out#internal/}"
  domain="${domain%/internal/store}"
  if [ -f "$queries" ]; then
    keep="$keep $queries"
    continue
  fi
  echo "sqlc: skipping $domain (no $queries)"
  if compgen -G "$out/*.go" >/dev/null; then
    echo "sqlc: $domain has generated code in $out but $queries is missing" >&2
    conflict=1
  fi
done < <(awk '$1 == "queries:" { q = $2 } $1 == "out:" { print q, $2 }' sqlc.yaml)

if [ "$conflict" = 1 ]; then
  exit 1
fi
if [ -z "$keep" ]; then
  echo "sqlc: no query files in backend/db/queries yet; nothing to generate"
  exit 0
fi

awk -v keep="$keep " '
  function flush() {
    if (inblock && index(keep, " " queries " ")) printf "%s", block
    block = ""
    queries = ""
  }
  /^  - engine:/ { flush(); inblock = 1 }
  !inblock { print; next }
  { block = block $0 "\n" }
  $1 == "queries:" { queries = $2 }
  END { flush() }
' sqlc.yaml >"$cfg"

sqlc generate -f "$cfg"
