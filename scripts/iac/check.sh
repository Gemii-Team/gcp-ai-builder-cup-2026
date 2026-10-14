#!/usr/bin/env bash
# Offline checks of deploy/terraform: formatting, every module's init/validate/test, tflint, a
# configuration scan (trivy), and a render, init and validate of the whole Terragrunt stack
# against mock outputs, once for each profile ("minimal", the default, and "full"). Nothing here
# plans, applies, or talks to Google Cloud:
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

# --- the whole Terragrunt stack against mock outputs, once per profile ----------------------------
# env.check.hcl names no profile, which is the default: minimal. The full profile is the same file
# with profile = "full" added. "minimal" is what an owner applies by default and "full" must stay
# what it was before that profile existed, so both are rendered, initialised and validated, and
# the inputs each passes to its modules are asserted.
ENV_MINIMAL="$LIVE/env.check.hcl"
ENV_FULL="$WORK/env.full.hcl"
awk '{ print } /^locals \{$/ { print "  profile           = \"full\"" }' "$ENV_MINIMAL" >"$ENV_FULL"
units="$(find "$LIVE" -mindepth 2 -maxdepth 2 -name terragrunt.hcl | wc -l | tr -d ' ')"

# unit_json UNIT: the rendered configuration of one unit (inputs included) as one line of JSON.
# It reads KIWI_IAC_ENV_FILE.
unit_json() { terragrunt render --json --working-dir "$LIVE/$1" 2>/dev/null; }

# number_input JSON NAME: the value of a numeric input (Terragrunt renders them as strings).
number_input() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\{0,1\}\([0-9][0-9]*\)\"\{0,1\}[,}].*/\1/p" | head -n 1; }

# profile_inputs PROFILE ENVFILE: what the units of a profile pass to their modules. The module
# tests prove what a module builds from such inputs; this proves the units pass them.
profile_inputs() {
  local profile=$1 envfile=$2 failed=0 run sql net
  run="$(KIWI_IAC_ENV_FILE="$envfile" unit_json cloudrun)"
  sql="$(KIWI_IAC_ENV_FILE="$envfile" unit_json cloudsql)"
  net="$(KIWI_IAC_ENV_FILE="$envfile" unit_json network)"
  [ -n "$run" ] && [ -n "$sql" ] && [ -n "$net" ] || { echo "a unit did not render"; return 1; }
  # want JSON REGEX MESSAGE / reject JSON REGEX MESSAGE: the pattern must / must not be in the JSON.
  want() { printf '%s' "$1" | grep -Eq "$2" || { echo "missing: $3"; failed=1; }; }
  reject() { ! printf '%s' "$1" | grep -Eq "$2" || { echo "present: $3"; failed=1; }; }

  if [ "$profile" = minimal ]; then
    want "$run" '"single_container":"?true"?' "cloudrun: single_container = true (one container, no Caddy)"
    reject "$run" '"valkey_url"' "cloudrun: valkey_url (the minimal profile has no Valkey; the cache is in-process)"
    reject "$run" '"web_image"' "cloudrun: web_image (the minimal profile has one image)"
    want "$run" '"db_app_max_conns"' "cloudrun: db_app_max_conns"
    want "$run" '"db_system_max_conns"' "cloudrun: db_system_max_conns"
    want "$run" '"db_max_conn_idle"' "cloudrun: db_max_conn_idle"
    want "$sql" '"tier":"db-f1-micro"' "cloudsql: db-f1-micro (the owner's decision of 2026-10-14; the way back is db-g1-small in root.hcl)"
    reject "$sql" '"edition"' "cloudsql: an edition (the module default, ENTERPRISE, is what shared-core tiers need)"
    want "$sql" '"max_connections"' "cloudsql: max_connections pinned"
    want "$net" '"valkey_enabled":"?false"?' "network: valkey_enabled = false"
    # Vertex AI: generation on the global endpoint, embeddings stay in Singapore (gemini-embedding-001
    # is not served by the global endpoint, and the module refuses a global embedding location).
    want "$run" '"vertex_location":"global"' "cloudrun: vertex_location = global"
    want "$run" '"vertex_model_flash":"gemini-3.8-flash"' "cloudrun: vertex_model_flash = gemini-3.8-flash"
    want "$run" '"vertex_model_pro":"gemini-3.1-pro-preview"' "cloudrun: vertex_model_pro = gemini-3.1-pro-preview"
    want "$run" '"vertex_embedding_location":"asia-southeast1"' "cloudrun: vertex_embedding_location = asia-southeast1"
    want "$run" '"vertex_embedding_model":"gemini-embedding-001"' "cloudrun: vertex_embedding_model = gemini-embedding-001"

    # The connection budget (modules/cloudsql/main.tf): two overlapping revisions of the service,
    # the seed Job's own pools, the migrate Job's connections and the reserved ones, against
    # max_connections. The system pool is at least 6 (the relay holds 4, plus 2 of headroom).
    local app sys max need
    app="$(number_input "$run" db_app_max_conns)"
    sys="$(number_input "$run" db_system_max_conns)"
    max="$(number_input "$sql" max_connections)"
    if [ -z "$app" ] || [ -z "$sys" ] || [ -z "$max" ]; then
      echo "could not read the pool sizes or max_connections"
      failed=1
    else
      need=$((2 * (app + sys) + (app + sys) + 2 + 8))
      echo "connection budget: 2 x (app $app + system $sys) + seed Job $((app + sys)) + migrate Job 2 + reserved 8 = $need of max_connections $max"
      [ "$sys" -ge 6 ] || { echo "the system pool $sys is below 6"; failed=1; }
      [ "$need" -le "$max" ] || { echo "the connection budget $need exceeds max_connections $max"; failed=1; }
    fi
  else
    reject "$run" '"single_container"' "cloudrun: single_container (the full profile is the Caddy ingress plus the sidecar)"
    want "$run" '"valkey_url"' "cloudrun: valkey_url"
    want "$run" '"web_image"' "cloudrun: web_image"
    reject "$run" '"db_app_max_conns"|"db_system_max_conns"|"db_max_conn_idle"' "cloudrun: pool inputs (the full profile keeps the app's own pools)"
    reject "$sql" '"tier"|"max_connections"|"edition"' "cloudsql: tier, max_connections or edition (the full profile takes the module's defaults)"
    want "$net" '"valkey_enabled":"?true"?' "network: valkey_enabled = true"
    want "$run" '"vertex_location":"asia-southeast1"' "cloudrun: vertex_location = asia-southeast1 (the full profile keeps the Singapore endpoint)"
    want "$run" '"vertex_model_flash":"gemini-3.5-flash"' "cloudrun: vertex_model_flash = gemini-3.5-flash"
    want "$run" '"vertex_model_pro":null' "cloudrun: vertex_model_pro null (the module then uses Flash)"
  fi
  return "$failed"
}

for entry in "minimal:$ENV_MINIMAL" "full:$ENV_FULL"; do
  profile="${entry%%:*}"
  envfile="${entry#*:}"
  # render --all leaves out an excluded unit: in the minimal profile that is valkey.
  expected="$units"
  [ "$profile" = minimal ] && expected=$((units - 1))
  check "[$profile] terragrunt hcl validate --inputs --strict: inputs match the modules' variables" \
    env KIWI_IAC_ENV_FILE="$envfile" terragrunt hcl validate --all --inputs --strict --working-dir "$LIVE"
  check "[$profile] terragrunt render --all: $expected of $units units resolve with their dependencies" \
    env KIWI_IAC_ENV_FILE="$envfile" bash -c '
      out="$(terragrunt render --all --json --working-dir "$1" 2>&1)" || { echo "$out"; exit 1; }
      rendered="$(printf "%s\n" "$out" | grep -c "Rendering config")"
      [ "$rendered" -eq "$2" ] || { echo "rendered $rendered of $2 units"; exit 1; }' _ "$LIVE" "$expected"
  check "[$profile] the units pass the inputs of the $profile profile" profile_inputs "$profile" "$envfile"
  check "[$profile] terragrunt run --all -- init -backend=false" \
    env KIWI_IAC_ENV_FILE="$envfile" terragrunt run --all --working-dir "$LIVE" -- init -backend=false -input=false
  check "[$profile] terragrunt run --all -- validate" \
    env KIWI_IAC_ENV_FILE="$envfile" terragrunt run --all --no-auto-init --working-dir "$LIVE" -- validate
  check "[$profile] terragrunt dag graph resolves without a cycle" \
    env KIWI_IAC_ENV_FILE="$envfile" terragrunt dag graph --working-dir "$LIVE"
done

# The valkey unit: part of the full stack, left out of the minimal one (the unit's exclude block;
# terragrunt list does not evaluate it, run --all does). The env file has a billing account, so the
# budget unit is in both and the only unit excluded from the minimal stack is valkey.
check "valkey unit is part of the full stack" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1)" || exit 1
  printf "%s\n" "$out" | grep -q "Unit ./valkey" && ! printf "%s\n" "$out" | grep -Eq "Excluded +[1-9]"' _ "$LIVE" "$ENV_FULL"
check "valkey unit is left out of the minimal stack" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1)" || exit 1
  ! printf "%s\n" "$out" | grep -q "Unit ./valkey" && printf "%s\n" "$out" | grep -q "Unit ./cloudrun" && printf "%s\n" "$out" | grep -Eq "Excluded +1"' _ "$LIVE" "$ENV_MINIMAL"

# The optional budget unit: part of the stack with a billing account, left out without one. Checked
# on the full profile, where it is the only unit that can be excluded; the minimal profile without a
# billing account leaves out the budget and valkey units both.
grep -v billing_account "$ENV_FULL" >"$WORK/env.full.nobilling.hcl"
grep -v billing_account "$ENV_MINIMAL" >"$WORK/env.minimal.nobilling.hcl"
check "budget unit is part of the stack when a billing account is set" bash -c '
  KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1 | grep -q "Unit ./budget"' _ "$LIVE" "$ENV_FULL"
check "budget unit is left out of the stack when no billing account is set" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1)" || exit 1
  ! printf "%s\n" "$out" | grep -q "Unit ./budget" && printf "%s\n" "$out" | grep -Eq "Excluded +1"' _ "$LIVE" "$WORK/env.full.nobilling.hcl"
check "minimal profile without a billing account leaves out both the budget and the valkey units" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt run --all --no-auto-init --working-dir "$1" -- validate 2>&1)" || exit 1
  ! printf "%s\n" "$out" | grep -Eq "Unit ./(budget|valkey)" && printf "%s\n" "$out" | grep -Eq "Excluded +2"' _ "$LIVE" "$WORK/env.minimal.nobilling.hcl"

# Two mistakes the profile setting must not let through: an unknown profile, and a web image in the
# minimal profile, which has none (it would be silently ignored). The error text is matched so a
# failure for another reason does not pass.
awk '{ print } /^locals \{$/ { print "  profile = \"huge\"" }' "$ENV_MINIMAL" >"$WORK/env.huge.hcl"
awk '{ print } /^locals \{$/ { print "  web_image = \"example/web:1\"" }' "$ENV_MINIMAL" >"$WORK/env.minimal.web.hcl"
check "an unknown profile stops every unit" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt render --json --working-dir "$1/network" 2>&1)" && exit 1
  printf "%s\n" "$out" | grep -q "local.env.profile as \"huge\""' _ "$LIVE" "$WORK/env.huge.hcl"
check "a web_image in the minimal profile stops every unit" bash -c '
  out="$(KIWI_IAC_ENV_FILE="$2" terragrunt render --json --working-dir "$1/network" 2>&1)" && exit 1
  printf "%s\n" "$out" | grep -q "remove_web_image_from_env_hcl_the_minimal_profile_has_one_image"' _ "$LIVE" "$WORK/env.minimal.web.hcl"

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
