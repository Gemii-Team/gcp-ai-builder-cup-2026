#!/usr/bin/env bash
# Offline checks of deploy/terraform: formatting, every module's init/validate/test, tflint, a
# configuration scan (trivy), and a render, init and validate of the whole Terragrunt stack
# against mock outputs. Nothing here plans, applies, or talks to Google Cloud:
#
#   * the script re-runs itself with every Google credential source removed, so a mistake could
#     not reach Google Cloud even if one were made;
#   * modules are initialised with -backend=false and tested with mocked providers;
#   * Terragrunt runs against env.check.hcl (fake values) with KIWI_IAC_OFFLINE=true, which makes
#     root.hcl skip reading dependency outputs from remote state (the mock outputs stand in) and
#     skip initialising the state backend; --backend-bootstrap is never passed.
#
# It needs network access to the Terraform registry (providers) and, for tflint --init, GitHub
# (the google ruleset). Needs terraform (>= 1.11), terragrunt, tflint and trivy on PATH.
#
#   scripts/iac/check.sh            run everything; exit 1 if any check failed
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [ -z "${KIWI_IAC_SANITISED:-}" ]; then
  CLOUDSDK_CONFIG="$(mktemp -d)"
  export CLOUDSDK_CONFIG
  exec env -u GOOGLE_APPLICATION_CREDENTIALS -u GOOGLE_CREDENTIALS -u GOOGLE_OAUTH_ACCESS_TOKEN \
    -u GOOGLE_CLOUD_PROJECT -u GOOGLE_PROJECT -u GCLOUD_PROJECT -u CLOUDSDK_CORE_PROJECT \
    -u GOOGLE_IMPERSONATE_SERVICE_ACCOUNT -u GOOGLE_BACKEND_CREDENTIALS \
    KIWI_IAC_SANITISED=1 CHECKPOINT_DISABLE=1 bash "$0" "$@"
fi

TF_DIR="$ROOT/deploy/terraform"
LIVE="$TF_DIR/live"

umask 077
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# One provider download for every module and unit.
export TF_PLUGIN_CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-$WORK/plugin-cache}"
mkdir -p "$TF_PLUGIN_CACHE_DIR"
export TF_INPUT=0

# The Terragrunt stack, offline.
export KIWI_IAC_ENV_FILE="$LIVE/env.check.hcl"
export KIWI_IAC_OFFLINE=true
export TG_TF_PATH=terraform
export TG_NON_INTERACTIVE=true
export TG_NO_COLOR=true

failures=0
checks=0

# check NAME CMD...: run CMD, print PASS or FAIL with the tail of its output.
check() {
  local name=$1
  shift
  local out="$WORK/out.$checks"
  checks=$((checks + 1))
  if "$@" >"$out" 2>&1; then
    echo "PASS $name"
  else
    echo "FAIL $name"
    tail -n 40 "$out" | sed 's/^/     /'
    failures=$((failures + 1))
  fi
}

# need TOOL: the tool must be on PATH; there is no skipping a check silently.
need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "FAIL tool $1 is not on PATH" >&2
    exit 2
  fi
}
for tool in terraform terragrunt tflint trivy; do need "$tool"; done

tf_version="$(terraform version -json | sed -n 's/.*"terraform_version": *"\([0-9.]*\)".*/\1/p' | head -n 1)"
check "terraform $tf_version is at least 1.11 (write-only arguments, ephemeral resources)" \
  bash -c 'printf "%s\n1.11.0\n" "$1" | sort -V -C -r' _ "$tf_version"

# --- formatting -----------------------------------------------------------------------------
check "terraform fmt -check -recursive deploy/terraform" terraform fmt -check -recursive "$TF_DIR"
check "terragrunt hcl fmt --check (live)" terragrunt hcl fmt --check --working-dir "$LIVE"

# --- every module: init, validate, test -------------------------------------------------------
for dir in "$TF_DIR"/modules/*/; do
  m="$(basename "$dir")"
  check "module $m: init -backend=false, validate" bash -c '
    terraform -chdir="$1" init -backend=false -input=false -lockfile=readonly >/dev/null &&
    terraform -chdir="$1" validate'  _ "$dir"
  if [ -d "$dir/tests" ]; then
    check "module $m: terraform test (mocked google provider)" terraform -chdir="$dir" test
  fi
done
check "guard module: init, validate" bash -c '
  terraform -chdir="$1" init -backend=false -input=false >/dev/null && terraform -chdir="$1" validate' _ "$TF_DIR/guards/forbidden-patterns"

# --- tflint (terraform and google rulesets) -------------------------------------------------------
check "tflint --init" bash -c 'cd "$1" && tflint --init' _ "$TF_DIR"
check "tflint --recursive" bash -c 'cd "$1" && tflint --recursive --minimum-failure-severity=notice' _ "$TF_DIR"

# --- configuration scan -------------------------------------------------------------------------
# Findings are fixed in the modules or justified in a #trivy:ignore comment next to the resource.
check "trivy config (no findings at any severity)" \
  trivy config "$TF_DIR" --skip-check-update --skip-version-check \
  --skip-dirs '**/.terraform' --skip-dirs '**/.terragrunt-cache' \
  --severity LOW,MEDIUM,HIGH,CRITICAL --exit-code 1

# --- the whole Terragrunt stack against mock outputs ----------------------------------------------
units="$(find "$LIVE" -mindepth 2 -maxdepth 2 -name terragrunt.hcl | wc -l | tr -d ' ')"
check "terragrunt hcl validate --inputs --strict: inputs match the modules' variables" \
  terragrunt hcl validate --all --inputs --strict --working-dir "$LIVE"
check "terragrunt render --all: $units units resolve with their dependencies" bash -c '
  out="$(terragrunt render --all --json --working-dir "$1" 2>&1)" || { echo "$out"; exit 1; }
  rendered="$(printf "%s\n" "$out" | grep -c "Rendering config")"
  [ "$rendered" -eq "$2" ] || { echo "rendered $rendered of $2 units"; exit 1; }' _ "$LIVE" "$units"
check "terragrunt run --all -- init -backend=false" \
  terragrunt run --all --working-dir "$LIVE" -- init -backend=false -input=false
check "terragrunt run --all -- validate" \
  terragrunt run --all --no-auto-init --working-dir "$LIVE" -- validate
check "terragrunt dag graph resolves without a cycle" terragrunt dag graph --working-dir "$LIVE"

# The optional budget unit: part of the stack with a billing account, left out without one (the
# unit's exclude block; terragrunt list does not evaluate it, run --all does).
cp "$LIVE/env.check.hcl" "$WORK/env.with.hcl"
grep -v billing_account "$LIVE/env.check.hcl" >"$WORK/env.without.hcl"
check "budget unit is part of the stack when a billing account is set" bash -c '
  KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1 | grep -q "Unit ./budget"' _ "$LIVE" "$WORK/env.with.hcl"
check "budget unit is left out of the stack when no billing account is set" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1)" || exit 1
  ! printf "%s\n" "$out" | grep -q "Unit ./budget" && printf "%s\n" "$out" | grep -Eq "Excluded +1"' _ "$LIVE" "$WORK/env.without.hcl"

# Provider lock files are tracked: the runs above must not have changed one.
if git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  check "provider lock files are up to date" git -C "$ROOT" diff --exit-code -- 'deploy/terraform/**/.terraform.lock.hcl'
fi

echo
if [ "$failures" -ne 0 ]; then
  echo "iac check: $failures of $checks check(s) failed"
  exit 1
fi
echo "iac check: all $checks checks passed"
