#!/usr/bin/env bash
# Fails when committed generated code differs from what `task gen` produces:
# the protobuf output and the sqlc store of every domain. Compares the working
# tree with the index, so stage generated files first (`task gen:check` runs it
# locally; the gen-drift job of .github/workflows/ci.yml runs it on a clean
# checkout, where nothing is unstaged). Untracked generated files also count as drift.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# The glob magic keeps `*` to one path segment, so only a domain's own
# internal/store is covered (backend/internal/<domain>/internal/store/**).
paths=(backend/gen packages/api-client/src/gen ':(glob)backend/internal/*/internal/store/**')

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
