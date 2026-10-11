#!/usr/bin/env bash
# Tests for check-paths.sh. Every case builds a throw-away git repository and
# asserts the exit code (and, where it matters, which paths are named).
set -u

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-paths.sh"

# Isolate from the caller's git configuration and from a surrounding hook run.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=hygiene-test GIT_AUTHOR_EMAIL=hygiene-test@example.invalid
export GIT_COMMITTER_NAME=hygiene-test GIT_COMMITTER_EMAIL=hygiene-test@example.invalid
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

failures=0
OUT=""
CODE=0

report() { # name ok|no detail
  if [ "$2" = ok ]; then
    echo "PASS $1"
  else
    echo "FAIL $1: $3"
    failures=$((failures + 1))
  fi
}

# verdict name want-exit [must-contain [must-not-contain]] judges OUT and CODE of the last run.
verdict() {
  local name=$1 want=$2 contains=${3:-} absent=${4:-}
  if [ "$CODE" -ne "$want" ]; then
    report "$name" no "exit $CODE, want $want (output: $OUT)"
  elif [ -n "$contains" ] && [[ $OUT != *"$contains"* ]]; then
    report "$name" no "output does not name '$contains' (output: $OUT)"
  elif [ -n "$absent" ] && [[ $OUT == *"$absent"* ]]; then
    report "$name" no "output names '$absent' but must not (output: $OUT)"
  else
    report "$name" ok
  fi
}

new_repo() {
  local dir
  dir=$(mktemp -d "$WORK/repo.XXXXXX")
  git -C "$dir" init -q -b main
  git -C "$dir" commit -q --allow-empty -m init
  echo "$dir"
}

put() { # repo path... : create each file with content
  local dir=$1 p
  shift
  for p in "$@"; do
    mkdir -p "$dir/$(dirname "$p")"
    printf 'x\n' >"$dir/$p"
  done
}

run_in() { # repo args... : run check-paths.sh from inside the repo
  local dir=$1
  shift
  OUT=$(cd "$dir" && bash "$SCRIPT" "$@" 2>&1)
  CODE=$?
}

# commit_files message path... : force-add and commit without hooks
commit_files() {
  local dir=$1 msg=$2
  shift 2
  put "$dir" "$@"
  git -C "$dir" add -f -- "$@"
  git -C "$dir" commit -q --no-verify -m "$msg"
}

# staged name want path... : stage the paths, expect the exit code; exit 1 must name the first path.
staged() {
  local name=$1 want=$2 repo
  shift 2
  repo=$(new_repo)
  put "$repo" "$@"
  git -C "$repo" add -f -- "$@"
  run_in "$repo" --staged
  if [ "$want" -eq 1 ]; then verdict "$name" 1 "$1"; else verdict "$name" "$want"; fi
}

# --- path cases: must be refused (exit 1, offending path named) --------------
staged "docs/x.md" 1 docs/x.md
staged "docs/ nested" 1 docs/superpowers/plans/p.md
staged "docs/ path with spaces" 1 "docs/a b.md"
staged ".env" 1 .env
staged ".env.local" 1 .env.local
staged "apps/web/.env" 1 apps/web/.env
staged "kubeconfig-prod" 1 kubeconfig-prod
staged "kubeconfig" 1 kubeconfig
staged "dev.kubeconfig" 1 deploy/dev.kubeconfig
staged "DESIGN.md" 1 DESIGN.md
staged "PRODUCT.md" 1 PRODUCT.md
staged ".impeccable/x.json" 1 .impeccable/x.json
staged "server.pem" 1 deploy/tls/server.pem
staged "server.key" 1 deploy/tls/server.key
staged "client.p12" 1 client.p12
staged "x-credentials.json" 1 x-credentials.json
staged "x-sa-key.json" 1 x-sa-key.json
staged "service-account-prod.json" 1 service-account-prod.json
staged "application_default_credentials.json" 1 application_default_credentials.json
staged "terraform.tfstate" 1 terraform.tfstate
staged "terraform.tfstate.backup" 1 terraform.tfstate.backup
staged "plan.tfplan" 1 plan.tfplan
staged "tfplan" 1 tfplan
staged "a.tfvars" 1 a.tfvars
staged ".terraform/ at the root" 1 .terraform/providers/x
staged ".terraform/ nested" 1 deploy/terraform/.terraform/providers/x

# --- path cases: must be allowed (exit 0) -------------------------------------
staged ".env.example" 0 .env.example
staged "terraform.tfvars.example" 0 terraform.tfvars.example
staged "README.md + backend/main.go" 0 README.md backend/main.go
staged "backend/internal/docs/service.go (Go package named docs)" 0 backend/internal/docs/service.go
staged "packages/ui/docs/README.md (only the root docs/ is private)" 0 packages/ui/docs/README.md
staged "packages/ui/DESIGN.md (only the root DESIGN.md is private)" 0 packages/ui/DESIGN.md
staged "deploy/.env.example" 0 deploy/.env.example

# --- --staged edge cases -------------------------------------------------------
repo=$(new_repo)
put "$repo" README.md docs/x.md
git -C "$repo" add -f -- README.md docs/x.md
run_in "$repo" --staged
verdict "only the offending path is printed" 1 docs/x.md README.md

repo=$(new_repo)
commit_files "$repo" "add docs" docs/x.md
git -C "$repo" rm -q -- docs/x.md
run_in "$repo" --staged
verdict "staged deletion of docs/x.md is allowed" 0

repo=$(new_repo)
run_in "$repo" --staged
verdict "nothing staged" 0

# --- --range -------------------------------------------------------------------
repo=$(new_repo)
commit_files "$repo" "readme" README.md
commit_files "$repo" "docs, hooks skipped" docs/x.md
run_in "$repo" --range HEAD~1..HEAD
verdict "--range catches docs/x.md committed with --no-verify" 1 docs/x.md

repo=$(new_repo)
commit_files "$repo" "docs, hooks skipped" docs/x.md
commit_files "$repo" "readme" README.md
run_in "$repo" --range HEAD~1..HEAD
verdict "--range ignores commits outside the range" 0

repo=$(new_repo)
commit_files "$repo" "readme" README.md
commit_files "$repo" "backend" backend/main.go
run_in "$repo" --range HEAD~2..HEAD
verdict "--range over clean commits" 0

repo=$(new_repo)
commit_files "$repo" "docs, hooks skipped" docs/x.md
git -C "$repo" rm -q -- docs/x.md
git -C "$repo" commit -q --no-verify -m "remove docs"
run_in "$repo" --range HEAD~2..HEAD
verdict "--range catches a docs file added and removed inside the range" 1 docs/x.md

repo=$(new_repo)
commit_files "$repo" "docs, hooks skipped" "docs/a b.md"
run_in "$repo" --range HEAD~1..HEAD
verdict "--range names a path with spaces" 1 "docs/a b.md"

repo=$(new_repo)
commit_files "$repo" "docs, hooks skipped" docs/x.md
printf 'y\n' >"$repo/docs/x.md"
git -C "$repo" add -f -- docs/x.md
git -C "$repo" commit -q --no-verify -m "docs again"
run_in "$repo" --range HEAD~2..HEAD
verdict "a path touched by several commits is reported once" 1 "check-paths: 1 path(s)"

# --- --range HEAD --not --remotes: the range a push scans when the branch has no upstream ---
# A remote-tracking ref is simulated with update-ref; only commits it does not contain are in scope.
repo=$(new_repo)
commit_files "$repo" "docs, hooks skipped" docs/x.md
run_in "$repo" --range HEAD --not --remotes
verdict "no remote-tracking ref: HEAD --not --remotes catches docs/x.md" 1 docs/x.md

repo=$(new_repo)
commit_files "$repo" "readme" README.md
run_in "$repo" --range HEAD --not --remotes
verdict "no remote-tracking ref: HEAD --not --remotes over clean history" 0

repo=$(new_repo)
commit_files "$repo" "docs, already on the remote" docs/old.md
git -C "$repo" update-ref refs/remotes/origin/main HEAD
commit_files "$repo" "docs, unpushed" docs/x.md
run_in "$repo" --range HEAD --not --remotes
verdict "HEAD --not --remotes names the unpushed docs/x.md, not the pushed docs/old.md" 1 docs/x.md docs/old.md

repo=$(new_repo)
commit_files "$repo" "docs, already on the remote" docs/old.md
git -C "$repo" update-ref refs/remotes/origin/main HEAD
commit_files "$repo" "readme" README.md
run_in "$repo" --range HEAD --not --remotes
verdict "HEAD --not --remotes does not rescan history the remote already has" 0

# --- usage errors fail closed (exit 2), never pass ------------------------------
repo=$(new_repo)
run_in "$repo" --range no-such-rev..HEAD
verdict "--range with an unknown revision" 2
run_in "$repo" --range
verdict "--range without a value" 2
run_in "$repo"
verdict "no arguments" 2
run_in "$repo" --bogus
verdict "unknown flag" 2
run_in "$repo" --range HEAD --bogus
verdict "--range with an unknown option" 2
run_in "$repo" --range --all
verdict "--range --all is not accepted" 2
run_in "$repo" --range --not --remotes
verdict "--range with no positive revision" 2

if [ "$failures" -ne 0 ]; then
  echo "check-paths.test.sh: $failures case(s) failed"
  exit 1
fi
echo "check-paths.test.sh: all cases passed"
