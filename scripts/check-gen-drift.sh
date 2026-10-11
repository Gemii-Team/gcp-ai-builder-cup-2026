#!/usr/bin/env bash
# Fails when committed generated code differs from what `task gen` produces.
# Compares the working tree with the index, so stage generated files (or run it
# on a clean checkout, as CI does). Untracked generated files also count as drift.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

paths=(backend/gen packages/api-client/src/gen)

task gen

if ! git diff --exit-code -- "${paths[@]}"; then
  echo "check-gen-drift: generated code is out of date; run 'task gen' and commit the result" >&2
  exit 1
fi

untracked="$(git ls-files --others --exclude-standard -- "${paths[@]}")"
if [ -n "$untracked" ]; then
  echo "check-gen-drift: generated files are not tracked by git (git add them):" >&2
  echo "$untracked" >&2
  exit 1
fi

echo "check-gen-drift: generated code is up to date"
