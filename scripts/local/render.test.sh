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

# no_literal_secrets [file]: no credential value is written into a manifest.
# Env entries, args/commands, scripts and SQL read secrets from a reference.
no_literal_secrets() {
  local f=${1:-$RENDER} hits
  [ -s "$f" ] || { echo "nothing rendered"; return 1; }
  hits=$(awk '
    function sens(s) { s = toupper(s); return s ~ /PASSWORD|PASSWD|API_?KEY|API-KEY|SECRET|TOKEN|SIGNING_KEY|ENC_KEY|REQUIREPASS|MASTERAUTH/ }
    # env entries: a sensitive name must not carry a literal value
    /^ *- name: / { esens = sens($3); nm = $3; inargs = 0; next }
    esens && /^ *value: / { print "literal value for env " nm; esens = 0 }
    /^ *valueFrom:/ { esens = 0 }
    # ConfigMap data keys that look sensitive
    /^  [A-Z][A-Z0-9_]*(PASSWORD|API_?KEY|SECRET|TOKEN|SIGNING_KEY|ENC_KEY)[A-Z0-9_]*: ./ { print "literal value for " $1 }
    # args and command lists: the item after a credential flag is a reference
    /^ *(- )?(args|command):[ ]*$/ { inargs = 1; prevflag = 0; next }
    inargs && /^ *- / {
      item = $0; sub(/^ *- /, "", item); gsub(/^["\047]|["\047]$/, "", item)
      if (prevflag && item !~ /^\$/) print "literal value after a credential flag in args"
      prevflag = 0
      if (item ~ /^--?[A-Za-z-]+$/ && sens(item)) prevflag = 1
      next
    }
    { inargs = 0 }
  ' "$f")
  # A Secret is created from .env at run time, never rendered.
  grep -qE '^kind: Secret$' "$f" && hits="$hits"$'\n'"a Secret object is rendered"
  # Credentials inside connection URLs.
  grep -qE '://[^/@[:space:]"]+:[^/@[:space:]"]+@' "$f" && hits="$hits"$'\n'"URL with literal credentials"
  # SQL PASSWORD 'x' or PASSWORD E'x' (the bootstrap interpolates :'pw' instead).
  grep -qiE -e "password[[:space:]]+[eE]?'" "$f" && hits="$hits"$'\n'"quoted literal password"
  # A credential flag with its value attached: --requirepass=x, --password x.
  grep -qiE -e "--?(requirepass|masterauth|password|passwd|token|api-?key|secret)[= ]+[^\$\"' ]" "$f" && hits="$hits"$'\n'"credential flag with a literal value"
  # An assignment inside a script: PGPASSWORD=x.
  grep -qE -e "[A-Z_]*(PASSWORD|SECRET|TOKEN|API_?KEY)=[\"']?[^\$\"'[:space:]]" "$f" && hits="$hits"$'\n'"literal credential assigned in a script"
  [ -z "$(printf '%s' "$hits" | tr -d '[:space:]')" ] || { printf '%s\n' "$hits"; return 1; }
}
check "no literal secret value in any manifest" no_literal_secrets

# The guard itself: each fragment below carries a literal credential and must be refused.
secrets_refused() { # name  (fragment on stdin)
  local f="$WORK/secret-fragment.yaml"
  cat >"$f"
  if no_literal_secrets "$f" >/dev/null 2>&1; then bad "no_literal_secrets refuses $1"; else ok "no_literal_secrets refuses $1"; fi
}
secrets_refused "a credential flag followed by its value in args" <<'EOF'
kind: Deployment
spec:
  containers:
  - args:
    - --save
    - ""
    - --requirepass
    - hunter2
EOF
secrets_refused "a credential flag with its value attached in args" <<'EOF'
kind: Deployment
spec:
  containers:
  - args:
    - --requirepass=hunter2
EOF
secrets_refused "an assignment inside a command script" <<'EOF'
kind: Job
spec:
  containers:
  - command:
    - sh
    - -c
    - |
      PGPASSWORD=hunter2 psql -h postgres
EOF
secrets_refused "a credential flag with its value in a command script" <<'EOF'
kind: Job
spec:
  containers:
  - command:
    - sh
    - -c
    - |
      valkey-cli --password hunter2 ping
EOF
secrets_refused "PASSWORD E'...' in SQL" <<'EOF'
kind: ConfigMap
data:
  init.sql: |
    CREATE ROLE kiwi_app LOGIN PASSWORD E'hunter2';
EOF
secrets_refused "PASSWORD '...' in SQL" <<'EOF'
kind: ConfigMap
data:
  init.sql: |
    CREATE ROLE kiwi_app LOGIN PASSWORD 'hunter2';
EOF
secrets_refused "a literal value on a sensitive env entry" <<'EOF'
kind: Deployment
spec:
  containers:
  - env:
    - name: POSTGRES_PASSWORD
      value: hunter2
EOF
# ... and references pass.
cat >"$WORK/secret-clean.yaml" <<'EOF'
kind: Deployment
spec:
  containers:
  - args:
    - --requirepass
    - $(VALKEY_PASS)
    env:
    - name: POSTGRES_PASSWORD
      valueFrom:
        secretKeyRef:
          key: KIWI_DB_PASSWORD
          name: kiwi-env
    command:
    - sh
    - -c
    - |
      psql -c "CREATE ROLE x LOGIN PASSWORD :'pw'"
EOF
check "no_literal_secrets accepts references to a Secret" no_literal_secrets "$WORK/secret-clean.yaml"

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

# --- the Postgres bootstrap and its probe ----------------------------------------
# Pinned here and, from the other side, by backend/internal/testkit/dbtest
# (TestRoles_MatchLocalPostgresInit): a role added or widened in only one of the
# two copies of the bootstrap is a red build.
postgres_bootstrap() {
  local sql roles want
  sql=$(doc ConfigMap postgres-init)
  [ -n "$sql" ] || { echo "ConfigMap postgres-init is not rendered"; return 1; }
  roles=$(printf '%s\n' "$sql" | sed -nE "s/^ *CREATE ROLE ([a-z_]+)( [A-Z ]*) PASSWORD :'pw';\$/\1\2/p" | sort)
  want=$(printf '%s\n' 'kiwi_app LOGIN' 'kiwi_owner LOGIN' 'kiwi_ro LOGIN' 'kiwi_system LOGIN BYPASSRLS' | sort)
  [ "$roles" = "$want" ] || { printf 'roles got:\n%s\nwant:\n%s\n' "$roles" "$want"; return 1; }
  [ "$(printf '%s\n' "$sql" | grep -c 'CREATE ROLE')" -eq 4 ] || { echo "a CREATE ROLE statement has an unexpected shape"; return 1; }
  printf '%s\n' "$sql" | grep -qE '^ *CREATE DATABASE kiwi OWNER kiwi_owner;$' || { echo "database kiwi is not created owned by kiwi_owner"; return 1; }
  printf '%s\n' "$sql" | awk '/^ *\\connect kiwi$/ { c = 1 } /^ *CREATE EXTENSION vector;$/ { if (c) ok = 1 } END { exit !ok }' || { echo "CREATE EXTENSION vector must run connected to database kiwi"; return 1; }
}
check "postgres-init: exactly kiwi_owner, kiwi_app, kiwi_system (BYPASSRLS), kiwi_ro, all LOGIN; database kiwi owned by kiwi_owner; extension vector" postgres_bootstrap

postgres_init_logging() {
  doc ConfigMap postgres-init | awk '
    /SET log_min_error_statement = panic;/ { a = 1 }
    /\\set VERBOSITY terse/ { b = 1 }
    /CREATE ROLE/ { exit !(a && b) }
  '
}
check "postgres-init: a failing statement is neither logged nor echoed (log_min_error_statement = panic, VERBOSITY terse) before the first CREATE ROLE" postgres_init_logging

postgres_startup_probe() {
  doc Deployment postgres | awk '
    /^ *startupProbe:/ { match($0, /^ */); ind = RLENGTH; inblk = 1; next }
    inblk { match($0, /^ */); if (RLENGTH <= ind) inblk = 0; else { if ($1 == "failureThreshold:") ft = $2; if ($1 == "periodSeconds:") ps = $2 } }
    END { if (ps == "") ps = 10; if (ft == "") ft = 3; exit !(ft * ps >= 120) }
  '
}
check "postgres: a startupProbe gives initdb and the bootstrap at least 120 s before liveness applies" postgres_startup_probe

check "k3d registry listens on loopback only" grep -qE '^ +host: 127\.0\.0\.1$' deploy/local/k3d.yaml

# --- the Tiltfile --------------------------------------------------------------
# These read the Tiltfile as text, in the layout it keeps: every k8s_resource( and
# custom_build( call opens at column 0 and closes with ")" at column 0, the
# resource name is the first line of a k8s_resource call, and resource_deps /
# deps are written as the file writes them. Comment lines are never evidence.
tilt_deps() { # [Tiltfile]: "name: dep dep" per k8s_resource call
  awk '
    /^k8s_resource\($/ { incall = 1; name = ""; next }
    incall && name == "" { n = $0; gsub(/[ \047,"]/, "", n); name = n; next }
    incall && /resource_deps=\[/ {
      l = $0; sub(/.*resource_deps=\[/, "", l); sub(/\].*/, "", l); gsub(/[\047" ]/, "", l); gsub(/,/, " ", l)
      print name ": " l
    }
    /^\)$/ { incall = 0 }
  ' "${1:-Tiltfile}"
}

tilt_orders() { # [Tiltfile]
  local deps
  deps=$(tilt_deps "${1:-Tiltfile}") || return 1
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

# The items of the deps=[ ... ] list of custom_build(, comments excluded.
tilt_build_deps() { # [Tiltfile]
  awk '
    /^custom_build\($/ { incall = 1; next }
    incall && /^\)$/ { incall = 0 }
    incall && /^[ \t]*#/ { next }
    incall && /^[ \t]*deps=\[$/ { indeps = 1; next }
    incall && indeps && /^[ \t]*\],?$/ { indeps = 0 }
    incall && indeps { sub(/^[ \t]+/, ""); sub(/[ \t]+$/, ""); print }
  ' "${1:-Tiltfile}"
}
tilt_migrations_dep() { tilt_build_deps "${1:-Tiltfile}" | grep -qxF "'backend/db/migrations',"; }
check "Tiltfile: backend/db/migrations is a dependency of the kiwi image build, so a migration change re-creates the migrate Job" tilt_migrations_dep

# An `if overlay != 'local':` line, outside comments, whose next line calls fail(.
tilt_overlay_guard() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    want { if ($0 ~ /^[ \t]+fail\(/) ok = 1; want = 0 }
    /^if overlay != \047local\047:$/ { want = 1; found = 1 }
    END { exit !(found && ok) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: an overlay other than local is refused with fail()" tilt_overlay_guard

# The guards themselves: each mutated copy of the Tiltfile must be refused.
tilt_mutant() { # name check-function sed-script
  local f="$WORK/Tiltfile.mutant"
  sed -E "$3" Tiltfile >"$f"
  if cmp -s Tiltfile "$f"; then bad "$1" "the mutation changed nothing"; return; fi
  if "$2" "$f" >/dev/null 2>&1; then bad "$1" "the mutated Tiltfile is still accepted"; else ok "$1"; fi
}
tilt_mutant "migrations dependency removed (the comment naming it stays) is refused" tilt_migrations_dep "/^ *'backend\/db\/migrations',\$/d"
tilt_mutant "migrations dependency commented out is refused" tilt_migrations_dep "s/^( *)'backend\/db\/migrations',\$/\1# 'backend\/db\/migrations',/"
tilt_mutant "overlay guard deleted is refused" tilt_overlay_guard "/^if overlay != 'local':\$/,+1d"
tilt_mutant "overlay guard that no longer calls fail() is refused" tilt_overlay_guard "s/^( +)fail\(/\1print(/"
tilt_mutant "overlay guard commented out is refused" tilt_overlay_guard "s/^if overlay != 'local':\$/# &/"
tilt_mutant "kiwi not waiting for ollama-pull is refused" tilt_orders "s/'migrate', 'ollama-pull', 'valkey'/'migrate', 'valkey'/"
tilt_mutant "migrate not waiting for postgres is refused" tilt_orders "s/resource_deps=\['postgres'\]/resource_deps=[]/"

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

# clean_env cmd...: run with PATH (stubs first) and HOME only, so a KIWI_* exported
# in the calling shell can never satisfy or change a check.
clean_env() { env -i PATH="$STUB:$PATH" HOME="$WORK" "$@"; }

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
EXPORTED=()      # NAME=value pairs exported to the script, as a calling shell would
EXPORT_PREFIX=   # non-empty: every .env line is written with an "export " prefix
# load_secret overlay [name=value]... : run load-env-secret.sh against a fresh .env. Sets RC, OUT, LOG, COPY.
load_secret() {
  local overlay=$1
  shift
  CASE=$((CASE + 1))
  local d="$WORK/load$CASE"
  mkdir -p "$d"
  write_env "$d/.env" "$@"
  [ -z "$EXPORT_PREFIX" ] || sed -i 's/^KIWI_/export KIWI_/' "$d/.env"
  LOG="$d/log"
  COPY="$d/envcopy"
  : >"$LOG"
  : >"$COPY"
  clean_env ${EXPORTED[@]+"${EXPORTED[@]}"} STUB_LOG="$LOG" STUB_ENV_COPY="$COPY" ENV_FILE="$d/.env" \
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
clean_env STUB_LOG="$d/log" STUB_ENV_COPY="$d/copy" ENV_FILE="$d/missing.env" bash scripts/local/load-env-secret.sh >"$d/out" 2>&1
rc=$?
verdict=ok
[ "$rc" -eq 1 ] && grep -q '\.env' "$d/out" || verdict=bad
expect "load-env-secret: a missing .env stops" "$verdict" "rc=$rc" "$(cat "$d/out")"

EXPORTED=(KIWI_ASR_MODEL=exported-in-the-calling-shell)
load_secret local KIWI_ASR_MODEL
EXPORTED=()
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" KIWI_ASR_MODEL || verdict=bad
[ ! -s "$LOG" ] || verdict=bad
expect "load-env-secret: a KIWI_* exported in the calling shell does not stand in for a variable missing from .env" "$verdict" "rc=$RC" "$OUT"

EXPORT_PREFIX=1
load_secret local
EXPORT_PREFIX=
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx "KIWI_LLM_API_KEY=$PROBE_LLM" "$COPY" || verdict=bad
grep -qx "KIWI_DB_PASSWORD=$SECRET_DB_PW" "$COPY" || verdict=bad
expect "load-env-secret: accepts an optional 'export ' prefix on .env lines" "$verdict" "rc=$RC" "$OUT"

# gen-env.sh ---------------------------------------------------------------------
b64_32() { printf '%s' "$1" | grep -Eq '^[A-Za-z0-9+/]{43}=$'; }
env_value() { grep -m1 "^$2=" "$1" 2>/dev/null | cut -d= -f2-; }

d="$WORK/gen1"
mkdir -p "$d"
clean_env ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out" 2>&1
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
clean_env ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out2" 2>&1
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
clean_env ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out" 2>&1
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

d="$WORK/gen3"
mkdir -p "$d"
printf 'export %s=%s\n' KIWI_DB_PASSWORD chosen-by-hand >"$d/.env"
clean_env ENV_FILE="$d/.env" bash scripts/local/gen-env.sh >"$d/out" 2>&1
rc=$?
verdict=ok
[ "$rc" -eq 0 ] || verdict=bad
[ "$(grep -c 'KIWI_DB_PASSWORD' "$d/.env")" -eq 1 ] || verdict=bad
grep -qx 'export KIWI_DB_PASSWORD=chosen-by-hand' "$d/.env" || verdict=bad
expect "gen-env: an 'export NAME=value' line counts as set and is left alone" "$verdict" "rc=$rc" "$(cat "$d/out")"

# k3d-up.sh: the cpuset hook decision (k3d, docker and kubectl are stubs) -------------
K3D_STUB="$WORK/k3dstub"
mkdir -p "$K3D_STUB"
cat >"$K3D_STUB/k3d" <<'EOF'
#!/usr/bin/env bash
printf 'k3d %s\n' "$*" >>"$K3D_LOG"
if [ "$1 $2" = "cluster get" ]; then exit 1; fi
EOF
cat >"$K3D_STUB/docker" <<'EOF'
#!/usr/bin/env bash
# `docker info` reports rootless in its security options only when STUB_ROOTLESS=1.
[ "${STUB_ROOTLESS:-0}" = 1 ] && echo "  rootless"
exit 0
EOF
cat >"$K3D_STUB/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$K3D_LOG"
EOF
chmod +x "$K3D_STUB/k3d" "$K3D_STUB/docker" "$K3D_STUB/kubectl"

# k3d_up mode rootless controllers: sets RC, OUT and K3D_LOG_FILE.
k3d_up() {
  local mode=$1 rootless=$2 controllers=$3
  CASE=$((CASE + 1))
  local d="$WORK/k3dup$CASE"
  mkdir -p "$d"
  printf '%s\n' "$controllers" >"$d/controllers"
  K3D_LOG_FILE="$d/log"
  : >"$K3D_LOG_FILE"
  env -i PATH="$K3D_STUB:$PATH" HOME="$WORK" K3D_LOG="$K3D_LOG_FILE" STUB_ROOTLESS="$rootless" \
    KIWI_K3D_USER_CONTROLLERS="$d/controllers" ${mode:+KIWI_K3D_CPUSET_HOOK="$mode"} \
    bash scripts/local/k3d-up.sh >"$d/out" 2>&1
  RC=$?
  OUT=$(cat "$d/out")
}
hook_applied() { grep -q 'k3d-entrypoint-zz-cpuset.sh' "$K3D_LOG_FILE" && grep -q 'KubeletInUserNamespace' "$K3D_LOG_FILE"; }
hook_absent() { ! grep -q 'k3d-entrypoint-zz-cpuset.sh' "$K3D_LOG_FILE" && ! grep -q 'KubeletInUserNamespace' "$K3D_LOG_FILE"; }

k3d_up "" 1 "cpu io memory pids"
verdict=ok
[ "$RC" -eq 0 ] && hook_applied && contains "$OUT" "cpuset hook: on" || verdict=bad
expect "k3d-up auto: rootless Docker without cpuset delegation gets the hook, and says so" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

k3d_up "" 1 "cpuset cpu io memory pids"
verdict=ok
[ "$RC" -eq 0 ] && hook_absent && contains "$OUT" "cpuset hook: off" || verdict=bad
expect "k3d-up auto: rootless Docker that delegates cpuset needs no hook" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

k3d_up "" 0 "cpu io memory pids"
verdict=ok
[ "$RC" -eq 0 ] && hook_absent && contains "$OUT" "cpuset hook: off" || verdict=bad
expect "k3d-up auto: rootful Docker needs no hook" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

k3d_up 1 0 "cpuset cpu io memory pids"
verdict=ok
[ "$RC" -eq 0 ] && hook_applied && contains "$OUT" "cpuset hook: on" || verdict=bad
expect "k3d-up KIWI_K3D_CPUSET_HOOK=1 forces the hook" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

k3d_up 0 1 "cpu io memory pids"
verdict=ok
[ "$RC" -eq 0 ] && hook_absent && contains "$OUT" "cpuset hook: off" || verdict=bad
expect "k3d-up KIWI_K3D_CPUSET_HOOK=0 disables the hook" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

k3d_up maybe 1 "cpu io memory pids"
verdict=ok
[ "$RC" -eq 2 ] && contains "$OUT" KIWI_K3D_CPUSET_HOOK && ! grep -q 'cluster create' "$K3D_LOG_FILE" || verdict=bad
expect "k3d-up: an unknown KIWI_K3D_CPUSET_HOOK value is refused before anything is created" "$verdict" "rc=$RC" "$OUT"

if [ "$failures" -ne 0 ]; then
  printf '\nrender.test.sh: %d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nrender.test.sh: all checks passed\n'
