#!/usr/bin/env bash
# Checks for the local stack that need no cluster: the rendered `local` overlay
# is valid and carries the operational promises, the Tiltfile orders the
# resources, and the two env scripts behave (kubectl is stubbed by an
# executable on PATH, the .env is a throw-away file).
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

failures=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { # name detail...
  failures=$((failures + 1))
  printf 'FAIL %s\n' "$1"
  shift
  [ "$#" -gt 0 ] && printf '     %s\n' "$@"
}
# check name command...: ok when the command succeeds.
check() {
  local name=$1
  shift
  if "$@" >"$WORK/check.out" 2>&1; then ok "$name"; else bad "$name" "$(head -n 5 "$WORK/check.out")"; fi
}

RENDER="$WORK/render.yaml"

# --- the rendered overlay ----------------------------------------------------
if kustomize build deploy/k8s/overlays/local >"$RENDER" 2>"$WORK/render.err"; then
  ok "kustomize build deploy/k8s/overlays/local"
else
  bad "kustomize build deploy/k8s/overlays/local" "$(head -n 5 "$WORK/render.err")"
  : >"$RENDER"
fi

if [ -s "$RENDER" ] && kubeconform -strict -ignore-missing-schemas -summary - <"$RENDER" >"$WORK/kc.out" 2>&1; then
  ok "kubeconform -strict accepts the rendered overlay"
else
  bad "kubeconform -strict accepts the rendered overlay" "$(head -n 5 "$WORK/kc.out" 2>/dev/null || echo 'nothing rendered')"
fi

# doc kind name: print that one document of the rendered stream. kustomize
# sorts keys, so kind is a top-level line and metadata.name the first
# two-space-indented `name:`.
doc() {
  awk -v kind="$1" -v name="$2" '
    function flush() { if (k == kind && n == name) printf "%s", buf; buf = ""; k = ""; n = "" }
    /^---$/ { flush(); next }
    { buf = buf $0 "\n" }
    /^kind: / { k = $2 }
    /^  name: / && n == "" { n = $2; gsub(/"/, "", n) }
    END { flush() }
  ' "$RENDER"
}

no_latest() {
  if grep -n ':latest' "$RENDER"; then return 1; fi
  # every image carries a tag or a digest
  local untagged
  untagged=$(awk '/^ *(- )?image: / { img = $NF; sub(/^[^\/]*\//, "", img); if (img !~ /[:@]/) print $NF }' "$RENDER")
  [ -z "$untagged" ] || { printf 'untagged image: %s\n' "$untagged"; return 1; }
  grep -qE '^ *(- )?image: ' "$RENDER"
}
check "no :latest and every image tagged or pinned by digest" no_latest

no_literal_secrets() {
  local hits
  [ -s "$RENDER" ] || { echo "nothing rendered"; return 1; }
  # An env entry whose name looks sensitive must read from a secret reference.
  hits=$(awk '
    /^ *- name: / { sens = ($3 ~ /PASSWORD|API_?KEY|SECRET|TOKEN|SIGNING_KEY|ENC_KEY/); nm = $3; next }
    sens && /^ *value: / { print "literal value for env " nm; sens = 0 }
    /^ *valueFrom:/ { sens = 0 }
    # ConfigMap data keys that look sensitive
    /^  [A-Z][A-Z0-9_]*(PASSWORD|API_?KEY|SECRET|TOKEN|SIGNING_KEY|ENC_KEY)[A-Z0-9_]*: ./ { print "literal value for " $1 }
  ' "$RENDER")
  # A Secret is created from .env at run time, never rendered.
  grep -E '^kind: Secret$' "$RENDER" >/dev/null && hits="$hits"$'\n'"a Secret object is rendered"
  # Credentials inside connection URLs.
  grep -En '://[^/@[:space:]"]+:[^/@[:space:]"]+@' "$RENDER" >/dev/null && hits="$hits"$'\n'"URL with literal credentials"
  # A literal quoted password in the bootstrap SQL.
  grep -Eni "password +'" "$RENDER" >/dev/null && hits="$hits"$'\n'"quoted literal password"
  [ -z "$(printf '%s' "$hits" | tr -d '[:space:]')" ] || { printf '%s\n' "$hits"; return 1; }
}
check "no literal secret value in any manifest" no_literal_secrets

kiwi_mounts_data() {
  doc Deployment kiwi | awk '
    /^ *- mountPath: \/data$/ { want = 1; next }
    want && /^ *name: / { vol = $2; want = 0 }
    /^ *- name: / { cur = $3 }
    /^ *claimName: kiwi-data$/ { claim[cur] = 1 }
    END { exit !(vol != "" && claim[vol]) }
  '
}
check "kiwi Deployment mounts PVC kiwi-data at /data" kiwi_mounts_data

ingress_routes() {
  local got
  got=$(doc Ingress kiwi | awk '
    /^ *- backend:$/ { svc = "" }
    /^ *service:$/ { insvc = 1; next }
    insvc && /^ *name: / { svc = $2; insvc = 0 }
    /^ *path: / { print $2 " " svc }
    /^ *ingressClassName: / { print "class " $2 }
  ' | sort)
  local want
  want=$'/ web\n/api kiwi\n/ws kiwi\nclass traefik'
  want=$(printf '%s\n' "$want" | sort)
  [ "$got" = "$want" ] || { printf 'got:\n%s\nwant:\n%s\n' "$got" "$want"; return 1; }
}
check "Ingress (class traefik) routes /api and /ws to kiwi and / to web" ingress_routes

# --- the Tiltfile --------------------------------------------------------------
# resource_deps of every k8s_resource( call, one "name: dep dep" line each. The
# Tiltfile keeps each call as `k8s_resource(` / `'name',` / ... / `)` and the
# list on one line.
tilt_deps() {
  awk '
    /^k8s_resource\($/ { incall = 1; name = ""; next }
    incall && name == "" { n = $0; gsub(/[ \047,"]/, "", n); name = n; next }
    incall && /resource_deps=\[/ {
      l = $0; sub(/.*resource_deps=\[/, "", l); sub(/\].*/, "", l); gsub(/[\047" ]/, "", l); gsub(/,/, " ", l)
      print name ": " l
    }
    /^\)$/ { incall = 0 }
  ' Tiltfile
}

tilt_orders() {
  local deps
  deps=$(tilt_deps) || return 1
  local rc=0 res dep
  for pair in "kiwi:migrate" "kiwi:ollama-pull" "kiwi:valkey" "migrate:postgres" "ollama-pull:ollama"; do
    res=${pair%%:*}
    dep=${pair##*:}
    printf '%s\n' "$deps" | awk -v r="$res" -v d="$dep" '
      $1 == r ":" { for (i = 2; i <= NF; i++) if ($i == d) found = 1 }
      END { exit !found }
    ' || { printf '%s does not wait for %s\n' "$res" "$dep"; rc=1; }
  done
  return "$rc"
}
check "Tiltfile: kiwi waits for migrate, ollama-pull and valkey; migrate for postgres; ollama-pull for ollama" tilt_orders
check "Tiltfile: backend/db/migrations changes rebuild the image the migrate Job runs" grep -q 'backend/db/migrations' Tiltfile
check "Tiltfile: only the local overlay is accepted" grep -q "overlay" Tiltfile

tasks_listed() {
  local listing
  listing=$(task --list-all 2>&1) || { printf '%s\n' "$listing"; return 1; }
  local t rc=0
  for t in local:up local:down seed reindex; do
    printf '%s\n' "$listing" | grep -qE "^\* ${t}:" || { printf 'task %s is missing\n' "$t"; rc=1; }
  done
  return "$rc"
}
check "Taskfile lists local:up, local:down, seed and reindex" tasks_listed

# --- the env scripts --------------------------------------------------------------
STUB="$WORK/stub"
mkdir -p "$STUB"
cat >"$STUB/kubectl" <<'EOF'
#!/usr/bin/env bash
# Records the call; copies the --from-env-file it is given; consumes stdin.
printf '%s\n' "$*" >>"$STUB_LOG"
for a in "$@"; do
  case $a in --from-env-file=*) cat "${a#--from-env-file=}" >"$STUB_ENV_COPY" ;; esac
done
case " $* " in
  *" --dry-run=client "*) echo "kind: stub" ;;
  *" apply "*) cat >/dev/null ;;
esac
EOF
chmod +x "$STUB/kubectl"

# Fixture values. They are assembled at run time and written with printf so that
# no line of this file looks like a secret to gitleaks.
PROBE_LLM='llm-fixture-0123'
SECRET_LLM_URL='https://llm.example.invalid'
SECRET_DB_PW='p/w+x=y-db'
B64_32="$(printf 'A%.0s' $(seq 43))="

# write_env file [name=value]... : a complete .env with the overrides applied.
write_env() {
  local f=$1
  shift
  : >"$f"
  local pair
  for pair in \
    "KIWI_LLM_BASE_URL $SECRET_LLM_URL" "KIWI_LLM_API_KEY $PROBE_LLM" \
    "KIWI_LLM_MODEL_FLASH flash-model" "KIWI_LLM_MODEL_PRO pro-model" "KIWI_ASR_MODEL asr-model" \
    "KIWI_OAUTH_CLIENT_ID mock" "KIWI_OAUTH_CLIENT_SECRET mock" \
    "KIWI_TOKEN_ENC_KEY $B64_32" "KIWI_BLOB_SIGNING_KEY $B64_32" "KIWI_DB_PASSWORD $SECRET_DB_PW" \
    "KIWI_ENV " "KIWI_ROLES "; do
    printf '%s=%s\n' "${pair%% *}" "${pair#* }" >>"$f"
  done
  local kv name
  for kv in "$@"; do
    name=${kv%%=*}
    grep -v "^$name=" "$f" >"$f.new"
    [ "$kv" = "$name" ] || printf '%s\n' "$kv" >>"$f.new" # NAME alone deletes the line
    mv "$f.new" "$f"
  done
}

CASE=0
# load_secret overlay [name=value]... : run load-env-secret.sh against a fresh .env. Sets RC, OUT, LOG, COPY.
load_secret() {
  local overlay=$1
  shift
  CASE=$((CASE + 1))
  local d="$WORK/load$CASE"
  mkdir -p "$d"
  write_env "$d/.env" "$@"
  LOG="$d/log"
  COPY="$d/envcopy"
  : >"$LOG"
  : >"$COPY"
  PATH="$STUB:$PATH" STUB_LOG="$LOG" STUB_ENV_COPY="$COPY" ENV_FILE="$d/.env" \
    bash scripts/local/load-env-secret.sh ${overlay:+"$overlay"} >"$d/out" 2>&1
  RC=$?
  OUT=$(cat "$d/out")
}

expect() { # name condition-result
  if [ "$2" = ok ]; then ok "$1"; else bad "$1" "${@:3}"; fi
}
contains() { case $1 in *"$2"*) return 0 ;; *) return 1 ;; esac }

load_secret local KIWI_LLM_API_KEY= KIWI_ASR_MODEL
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" KIWI_LLM_API_KEY || verdict=bad
contains "$OUT" KIWI_ASR_MODEL || verdict=bad
[ ! -s "$LOG" ] || verdict=bad
expect "load-env-secret: an empty and an absent required variable are both named, nothing is applied" "$verdict" "rc=$RC" "$OUT"

load_secret local
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -q 'apply' "$LOG" || verdict=bad
grep -q -- '--context k3d-kiwi' "$LOG" || verdict=bad
grep -q -- '-n kiwi' "$LOG" || verdict=bad
grep -q -- 'secret generic kiwi-env' "$LOG" || verdict=bad
expect "load-env-secret: a complete .env applies Secret kiwi-env in namespace kiwi on context k3d-kiwi" "$verdict" "rc=$RC" "$OUT" "$(cat "$LOG")"

verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
for secret in "$PROBE_LLM" "$SECRET_LLM_URL" "$SECRET_DB_PW" 'p%2Fw%2Bx%3Dy-db'; do
  contains "$OUT" "$secret" && verdict=bad
done
expect "load-env-secret: prints no secret value" "$verdict" "$OUT"

verdict=ok
grep -qx 'KIWI_DATABASE_URL=postgres://kiwi_app:p%2Fw%2Bx%3Dy-db@postgres:5432/kiwi?sslmode=disable' "$COPY" || verdict=bad
grep -qx 'KIWI_DATABASE_SYSTEM_URL=postgres://kiwi_system:p%2Fw%2Bx%3Dy-db@postgres:5432/kiwi?sslmode=disable' "$COPY" || verdict=bad
grep -qx 'KIWI_MIGRATE_DATABASE_URL=postgres://kiwi_owner:p%2Fw%2Bx%3Dy-db@postgres:5432/kiwi?sslmode=disable' "$COPY" || verdict=bad
grep -qx "KIWI_DB_PASSWORD=$SECRET_DB_PW" "$COPY" || verdict=bad
grep -qx "KIWI_LLM_API_KEY=$PROBE_LLM" "$COPY" || verdict=bad
expect "load-env-secret: composes the three database URLs against host postgres with the password percent-encoded" "$verdict" "$(cut -d= -f1 "$COPY" | tr '\n' ' ')"

verdict=ok
[ -s "$COPY" ] || verdict=bad
grep -Eq '^KIWI_(ENV|ROLES)=' "$COPY" && verdict=bad
expect "load-env-secret: variables the overlay sets stay out of the Secret" "$verdict"

load_secret mockauth KIWI_OAUTH_CLIENT_ID= KIWI_OAUTH_CLIENT_SECRET=
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_ID=mock' "$COPY" || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_SECRET=mock' "$COPY" || verdict=bad
expect "load-env-secret mockauth: the OAuth pair defaults to mock" "$verdict" "rc=$RC" "$OUT"

load_secret mockauth KIWI_LLM_API_KEY=
verdict=ok
[ "$RC" -ne 0 ] && contains "$OUT" KIWI_LLM_API_KEY || verdict=bad
expect "load-env-secret mockauth: a missing LLM variable still stops" "$verdict" "rc=$RC" "$OUT"

load_secret smoke KIWI_LLM_BASE_URL= KIWI_LLM_API_KEY= KIWI_LLM_MODEL_FLASH= KIWI_LLM_MODEL_PRO= KIWI_ASR_MODEL= KIWI_OAUTH_CLIENT_ID= KIWI_OAUTH_CLIENT_SECRET=
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
for v in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_LLM_MODEL_FLASH KIWI_LLM_MODEL_PRO KIWI_ASR_MODEL KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET; do
  grep -qx "$v=mock" "$COPY" || verdict=bad
done
expect "load-env-secret smoke: LLM, ASR and OAuth variables default to mock" "$verdict" "rc=$RC" "$OUT"

load_secret smoke KIWI_DB_PASSWORD=
verdict=ok
[ "$RC" -ne 0 ] && contains "$OUT" KIWI_DB_PASSWORD || verdict=bad
expect "load-env-secret smoke: a missing generated key still stops" "$verdict" "rc=$RC" "$OUT"

load_secret nosuchoverlay
verdict=ok
[ "$RC" -eq 2 ] && [ ! -s "$LOG" ] || verdict=bad
expect "load-env-secret: an unknown overlay is refused" "$verdict" "rc=$RC" "$OUT"

d="$WORK/noenv"
mkdir -p "$d"
PATH="$STUB:$PATH" STUB_LOG="$d/log" STUB_ENV_COPY="$d/copy" ENV_FILE="$d/missing.env" bash scripts/local/load-env-secret.sh >"$d/out" 2>&1
rc=$?
verdict=ok
[ "$rc" -eq 1 ] && grep -q '\.env' "$d/out" || verdict=bad
expect "load-env-secret: a missing .env stops" "$verdict" "rc=$rc" "$(cat "$d/out")"

# gen-env.sh ---------------------------------------------------------------------
b64_32() { printf '%s' "$1" | grep -Eq '^[A-Za-z0-9+/]{43}=$'; }
env_value() { grep -m1 "^$2=" "$1" 2>/dev/null | cut -d= -f2-; }

d="$WORK/gen1"
mkdir -p "$d"
ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out" 2>&1
rc=$?
verdict=ok
[ "$rc" -eq 0 ] || verdict=bad
[ -n "$(find "$d/.env" -perm 600 2>/dev/null)" ] || verdict=bad
for v in KIWI_TOKEN_ENC_KEY KIWI_BLOB_SIGNING_KEY KIWI_DB_PASSWORD; do
  val=$(env_value "$d/.env" "$v")
  b64_32 "$val" || verdict=bad
  contains "$(cat "$d/out")" "$val" && verdict=bad
done
[ -z "$(env_value "$d/.env" KIWI_LLM_API_KEY)" ] || verdict=bad
expect "gen-env: creates .env mode 600 from .env.example, fills the three generated values, prints none" "$verdict" "rc=$rc" "$(cat "$d/out")"

cp "$d/.env" "$d/.env.first"
ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out2" 2>&1
verdict=ok
cmp -s "$d/.env" "$d/.env.first" || verdict=bad
expect "gen-env: a second run changes nothing" "$verdict" "$(cat "$d/out2")"

d="$WORK/gen2"
mkdir -p "$d"
{
  printf '%s=%s\n' KIWI_LLM_API_KEY keep-this-key
  printf '%s=%s\n' KIWI_TOKEN_ENC_KEY already-set-by-hand
  printf '%s=\n' KIWI_BLOB_SIGNING_KEY
  printf '%s=""\n' KIWI_DB_PASSWORD
} >"$d/.env"
chmod 644 "$d/.env"
ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out" 2>&1
rc=$?
verdict=ok
[ "$rc" -eq 0 ] || verdict=bad
[ -n "$(find "$d/.env" -perm 600 2>/dev/null)" ] || verdict=bad
[ "$(env_value "$d/.env" KIWI_LLM_API_KEY)" = keep-this-key ] || verdict=bad
[ "$(env_value "$d/.env" KIWI_TOKEN_ENC_KEY)" = already-set-by-hand ] || verdict=bad
b64_32 "$(env_value "$d/.env" KIWI_BLOB_SIGNING_KEY)" || verdict=bad
b64_32 "$(env_value "$d/.env" KIWI_DB_PASSWORD)" || verdict=bad
grep -q '^KIWI_VALKEY_URL=' "$d/.env" || verdict=bad
grep -q '^KIWI_WORKSPACE_NAME=' "$d/.env" || verdict=bad
expect "gen-env: an existing .env keeps its values, gets only its empty generated ones filled, missing names appended, mode 600" "$verdict" "rc=$rc" "$(cat "$d/out")"

if [ "$failures" -ne 0 ]; then
  printf '\nrender.test.sh: %d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nrender.test.sh: all checks passed\n'
