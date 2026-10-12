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

RENDER="$WORK/render.yaml" # the `local` overlay

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

# doc kind name [file]: print that one document of the rendered stream (the
# `local` render unless a file is given). kustomize sorts keys, so kind is a
# top-level line and metadata.name the first two-space-indented `name:`.
doc() {
  awk -v kind="$1" -v name="$2" '
    function flush() { if (k == kind && n == name) printf "%s", buf; buf = ""; k = ""; n = "" }
    /^---$/ { flush(); next }
    { buf = buf $0 "\n" }
    /^kind: / { k = $2 }
    /^  name: / && n == "" { n = $2; gsub(/"/, "", n) }
    END { flush() }
  ' "${3:-$RENDER}"
}

no_latest() { # [file]
  local f=${1:-$RENDER}
  if grep -n ':latest' "$f"; then return 1; fi
  # every image carries a tag or a digest
  local untagged
  untagged=$(awk '/^ *(- )?image: / { img = $NF; sub(/^[^\/]*\//, "", img); if (img !~ /[:@]/) print $NF }' "$f")
  [ -z "$untagged" ] || { printf 'untagged image: %s\n' "$untagged"; return 1; }
  grep -qE '^ *(- )?image: ' "$f"
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

# web: Caddy runs as the unprivileged user on 8080 with a read-only root filesystem, like kiwi,
# mockoidc and llmreplay. Its own state (/data, /config) and /tmp are emptyDirs; the Ingress
# still sends / to Service port 80, which targets the named container port.
web_workload() {
  local dep svc rc=0 mount
  dep=$(doc Deployment web)
  svc=$(doc Service web)
  [ -n "$dep" ] || { echo "no Deployment web"; return 1; }
  [ -n "$svc" ] || { echo "no Service web"; return 1; }
  printf '%s\n' "$dep" | grep -qE '^ *(- )?containerPort: 8080$' || { echo "the container does not listen on 8080"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *(- )?containerPort: 80$' && { echo "the container still declares port 80"; rc=1; }
  printf '%s\n' "$svc" | awk '/^ *port: 80$/ { p = 1 } /^ *targetPort: http$/ { t = 1 } END { exit !(p && t) }' || { echo "the Service is not port 80 -> targetPort http"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *runAsNonRoot: true$' || { echo "the container may run as root"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *runAsUser: 65532$' || { echo "the user is not 65532"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *runAsGroup: 65532$' || { echo "the group is not 65532"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *readOnlyRootFilesystem: true$' || { echo "the root filesystem is not read-only"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *allowPrivilegeEscalation: false$' || { echo "the container may escalate privileges"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *type: RuntimeDefault$' || { echo "no seccomp profile"; rc=1; }
  printf '%s\n' "$dep" | awk '/^ *drop:$/ { d = 1; next } d && /^ *- ALL$/ { ok = 1 } END { exit !ok }' || { echo "the capabilities are not dropped"; rc=1; }
  [ "$(printf '%s\n' "$dep" | grep -cE '^ *port: http$')" -eq 2 ] || { echo "the readiness and liveness probes do not both use the named port"; rc=1; }
  for mount in /data /config /tmp; do
    printf '%s\n' "$dep" | awk -v mp="$mount" '
      $0 ~ "^ *- mountPath: " mp "$" { want = 1; next }
      want && /^ *name: / { vol = $2; want = 0 }
      /^ *- emptyDir: \{\}$/ { pend = 1; next }
      pend && /^ *name: / { empty[$2] = 1; pend = 0 }
      END { exit !(vol != "" && empty[vol]) }
    ' || { echo "$mount is not an emptyDir mount"; rc=1; }
  done
  return "$rc"
}
check "web: Deployment on 8080 as 65532, read-only root with emptyDirs at /data /config /tmp, no capabilities, seccomp; Service 80 -> http" web_workload

# The image the Deployment runs: what Dockerfile and Caddyfile promise must match the manifest.
web_image_sources() {
  local rc=0 code
  grep -qxE 'USER 65532(:65532)?' apps/web/Dockerfile || { echo "the Dockerfile does not run as 65532"; rc=1; }
  grep -qxE 'EXPOSE 8080' apps/web/Dockerfile || { echo "the Dockerfile does not expose 8080"; rc=1; }
  grep -qE '^EXPOSE 80$' apps/web/Dockerfile && { echo "the Dockerfile still exposes 80"; rc=1; }
  # caddy:2-alpine puts cap_net_bind_service=ep on the binary: once the container drops ALL
  # capabilities, exec of such a binary fails with EPERM. The base tag floats, so the removal
  # runs only where getcap finds a capability (a later base without one still builds), and a
  # setcap that fails is never swallowed.
  local docker
  docker=$(grep -vE '^[[:space:]]*#' apps/web/Dockerfile)
  printf '%s\n' "$docker" | grep -qF 'setcap -r /usr/bin/caddy' || { echo "the Dockerfile leaves the file capability on /usr/bin/caddy"; rc=1; }
  printf '%s\n' "$docker" | grep -qF 'getcap /usr/bin/caddy' || { echo "the capability removal does not first ask getcap whether there is one"; rc=1; }
  printf '%s\n' "$docker" | grep -E 'setcap|getcap' | grep -qE '\|\| *(true|:)' && { echo "a failing setcap/getcap is swallowed with || true"; rc=1; }
  code=$(grep -vE '^[[:space:]]*#' apps/web/Caddyfile)
  printf '%s\n' "$code" | grep -qE '^:8080 \{$' || { echo "the Caddyfile does not serve :8080"; rc=1; }
  printf '%s\n' "$code" | grep -qE '^:80 \{$' && { echo "the Caddyfile still serves :80"; rc=1; }
  printf '%s\n' "$code" | grep -qE '^[[:space:]]+X-Content-Type-Options +nosniff$' || { echo "no X-Content-Type-Options: nosniff"; rc=1; }
  printf '%s\n' "$code" | grep -qE '^[[:space:]]+X-Frame-Options +DENY$' || { echo "no X-Frame-Options: DENY"; rc=1; }
  printf '%s\n' "$code" | grep -qE '^[[:space:]]+Referrer-Policy +same-origin$' || { echo "no Referrer-Policy: same-origin"; rc=1; }
  # A Content-Security-Policy needs a pass in a real browser (PWA, service worker, fonts): not added blind.
  printf '%s\n' "$code" | grep -qi 'Content-Security-Policy' && { echo "a Content-Security-Policy that no browser has been tried with"; rc=1; }
  return "$rc"
}
check "web image: Dockerfile and Caddyfile run as 65532 on 8080 without the file capability, with the safe headers and no CSP" web_image_sources

# The Caddyfile is validated with the caddy binary. Where it is missing the check is skipped,
# loudly; in GitHub Actions a missing caddy is a failure, because the scripts job installs it.
if command -v caddy >/dev/null 2>&1; then
  check "web image: caddy validate accepts the Caddyfile" caddy validate --config apps/web/Caddyfile --adapter caddyfile
elif [ "${GITHUB_ACTIONS:-}" = true ]; then
  bad "web image: caddy validate accepts the Caddyfile" "caddy is not installed in CI: the scripts job must install it"
else
  printf 'SKIP web image: caddy is not installed, the Caddyfile is not validated\n'
fi

# The Tilt live_update copies the bundle into the running container, which a read-only root
# filesystem refuses: the web image is rebuilt instead.
tilt_web_has_no_live_update() { ! grep -vE '^[[:space:]]*#' Tiltfile | grep -q 'live_update'; }
check "Tiltfile: the web image has no live_update (its root filesystem is read-only)" tilt_web_has_no_live_update

# apps/web is in the tree: the Tiltfile has no switch for its absence, and always builds the web image.
tilt_has_no_web_switch() { ! grep -vE '^[[:space:]]*#' Tiltfile | grep -q 'web_enabled'; }
check "Tiltfile: no web_enabled switch (apps/web is in the tree)" tilt_has_no_web_switch
tilt_builds_web_image() {
  awk '/^[ \t]*#/ { next } /^docker_build\($/ { b = 1; next } b && /REGISTRY \+ \047\/web\047/ { ok = 1 } /^\)$/ { b = 0 } END { exit !ok }' Tiltfile
}
check "Tiltfile: the web image is built unconditionally (docker_build at the top level)" tilt_builds_web_image

# The kiwi binary is built for the architecture of the host's Docker, whichever that is.
tilt_goarch_follows_host() {
  local code
  code=$(grep -vE '^[[:space:]]*#' Tiltfile)
  printf '%s\n' "$code" | grep -qF "GOARCH=\$(go env GOARCH)" || { echo "the kiwi build does not use GOARCH=\$(go env GOARCH)"; return 1; }
  ! printf '%s\n' "$code" | grep -qE 'GOARCH=(amd64|arm64)' || { echo "a hard-coded GOARCH is left"; return 1; }
}
check "Tiltfile: the kiwi binary is built for the host architecture (GOARCH from go env)" tilt_goarch_follows_host

# --- the mockauth overlay -------------------------------------------------------------
# The default stack signs in through the mock provider (backend/tools/mockoidc):
# `local` plus mockoidc, and a kiwi that differs from `local` only in the two
# OIDC addresses. `local` itself, the real Google issuer, must not carry the mock.
MOCK="$WORK/mockauth.yaml"
if kustomize build deploy/k8s/overlays/mockauth >"$MOCK" 2>"$WORK/mock.err"; then
  ok "kustomize build deploy/k8s/overlays/mockauth"
else
  bad "kustomize build deploy/k8s/overlays/mockauth" "$(head -n 5 "$WORK/mock.err")"
  : >"$MOCK"
fi
if [ -s "$MOCK" ] && kubeconform -strict -ignore-missing-schemas -summary - <"$MOCK" >"$WORK/kc-mock.out" 2>&1; then
  ok "kubeconform -strict accepts the rendered mockauth overlay"
else
  bad "kubeconform -strict accepts the rendered mockauth overlay" "$(head -n 5 "$WORK/kc-mock.out" 2>/dev/null || echo 'nothing rendered')"
fi
check "mockauth: no :latest and every image tagged or pinned by digest" no_latest "$MOCK"
check "mockauth: no literal secret value in any manifest" no_literal_secrets "$MOCK"

local_has_no_mock() {
  [ -s "$RENDER" ] || { echo "nothing rendered"; return 1; }
  if grep -n -i mockoidc "$RENDER"; then echo "the local overlay renders the mock sign-in provider"; return 1; fi
}
check "local: renders no mockoidc workload, Service, Ingress path or address" local_has_no_mock

# manifest_env kind name file VAR: the literal value of an env entry.
manifest_env() {
  doc "$1" "$2" "$3" | awk -v v="$4" '
    /^ *- name: / { cur = $3 }
    cur == v && /^ *value: / { sub(/^ *value: /, ""); gsub(/^"|"$/, ""); print; exit }
  '
}
# config_data file: the data of ConfigMap kiwi-config as sorted KEY=value lines.
config_data() {
  doc ConfigMap kiwi-config "$1" | awk '
    /^data:$/ { d = 1; next }
    /^[a-z]/ { d = 0 }
    d && /^  [A-Z0-9_]+: / { k = $1; sub(/:$/, "", k); v = $0; sub(/^  [A-Z0-9_]+: /, "", v); gsub(/^"|"$/, "", v); print k "=" v }
  ' | sort
}

mockoidc_workload() {
  local dep svc rc=0
  dep=$(doc Deployment mockoidc "$MOCK")
  svc=$(doc Service mockoidc "$MOCK")
  [ -n "$dep" ] || { echo "no Deployment mockoidc"; return 1; }
  [ -n "$svc" ] || { echo "no Service mockoidc"; return 1; }
  printf '%s\n' "$dep" | grep -qE '^ *image: kiwi-registry\.localhost:5001/mockoidc:[^ :]+$' || { echo "the image is not kiwi-registry.localhost:5001/mockoidc with a tag"; rc=1; }
  printf '%s\n' "$dep" | grep -qxE ' *- /app/mockoidc' || { echo "the container does not run /app/mockoidc"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *(- )?containerPort: 8090$' || { echo "the container does not listen on 8090"; rc=1; }
  printf '%s\n' "$svc" | grep -qE '^ *port: 8090$' || { echo "the Service does not expose 8090"; rc=1; }
  # The mock holds no secret: it never reads kiwi-env, and takes KIWI_ENV from
  # the ConfigMap so that it refuses to run the day that says prod.
  if printf '%s\n' "$dep" | grep -qE 'kiwi-env|secretKeyRef|secretRef'; then echo "the mock reads a Secret"; rc=1; fi
  printf '%s\n' "$dep" | awk '
    /^ *- name: / { f = ($3 == "KIWI_ENV"); c = 0 }
    f && /configMapKeyRef:/ { c = 1 }
    f && c && /^ *name: kiwi-config$/ { ok = 1 }
    END { exit !ok }
  ' || { echo "KIWI_ENV does not come from ConfigMap kiwi-config"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *path: /mockoidc/\.well-known/openid-configuration$' || { echo "the probes do not ask the discovery document"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *readOnlyRootFilesystem: true$' || { echo "the root filesystem is not read-only"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *runAsNonRoot: true$' || { echo "the container may run as root"; rc=1; }
  return "$rc"
}
check "mockauth: Deployment and Service mockoidc on 8090, pinned image, no Secret, KIWI_ENV from the ConfigMap" mockoidc_workload

mockoidc_ingress() {
  local got want rc=0
  got=$(doc Ingress mockoidc "$MOCK" | awk '
    /^ *- backend:$/ { svc = "" }
    /^ *service:$/ { insvc = 1; next }
    insvc && /^ *name: / { svc = $2; insvc = 0 }
    /^ *number: / { port = $2; print "port " port }
    /^ *path: / { print $2 " " svc }
    /^ *ingressClassName: / { print "class " $2 }
  ' | sort)
  want=$(printf '%s\n' '/mockoidc mockoidc' 'port 8090' 'class traefik' | sort)
  [ "$got" = "$want" ] || { printf 'got:\n%s\nwant:\n%s\n' "$got" "$want"; rc=1; }
  # The routes of kiwi are the ones of `local`: /mockoidc is the more specific
  # prefix, so Traefik sends it to the mock and everything else as before.
  [ "$(doc Ingress kiwi)" = "$(doc Ingress kiwi "$MOCK")" ] || { echo "Ingress kiwi differs between local and mockauth"; rc=1; }
  return "$rc"
}
check "mockauth: Ingress (class traefik) routes /mockoidc to mockoidc:8090 and leaves the routes of kiwi alone" mockoidc_ingress

mockoidc_matches_kiwi() {
  local issuer public mock_issuer mock_redirect value name rc=0
  issuer=$(config_data "$MOCK" | sed -n 's/^KIWI_OIDC_ISSUER=//p')
  public=$(config_data "$MOCK" | sed -n 's/^KIWI_PUBLIC_URL=//p')
  [ -n "$issuer" ] && [ -n "$public" ] || { echo "kiwi-config has no KIWI_OIDC_ISSUER or KIWI_PUBLIC_URL"; return 1; }
  mock_issuer=$(manifest_env Deployment mockoidc "$MOCK" MOCKOIDC_ISSUER)
  mock_redirect=$(manifest_env Deployment mockoidc "$MOCK" MOCKOIDC_REDIRECT_URIS)
  [ "$mock_issuer" = "$issuer" ] || { echo "MOCKOIDC_ISSUER is not the KIWI_OIDC_ISSUER $issuer"; rc=1; }
  [ "$mock_redirect" = "$public/api/auth/callback" ] || { echo "MOCKOIDC_REDIRECT_URIS is not the callback of KIWI_PUBLIC_URL $public"; rc=1; }
  for name in MOCKOIDC_HD MOCKOIDC_USERS MOCKOIDC_OUTSIDER; do
    value=$(manifest_env Deployment mockoidc "$MOCK" "$name")
    [ -n "$value" ] || { echo "$name is not set"; rc=1; }
  done
  return "$rc"
}
check "mockauth: the mock mints the issuer kiwi verifies and redirects only to kiwi's callback" mockoidc_matches_kiwi

kiwi_env_differs_only_in_oidc() {
  local changed rc=0 pair kind name
  { [ -s "$RENDER" ] && [ -s "$MOCK" ]; } || { echo "nothing rendered"; return 1; }
  changed=$(diff <(config_data "$RENDER") <(config_data "$MOCK") | sed -nE 's/^[<>] ([A-Z0-9_]+)=.*/\1/p' | sort -u | tr '\n' ' ')
  [ "$changed" = "KIWI_OIDC_INTERNAL_URL KIWI_OIDC_ISSUER KIWI_WORKSPACE_DOMAIN KIWI_WORKSPACE_NAME " ] || { printf 'kiwi-config differs in: %s\nwant: KIWI_OIDC_INTERNAL_URL KIWI_OIDC_ISSUER KIWI_WORKSPACE_DOMAIN KIWI_WORKSPACE_NAME\n' "$changed"; rc=1; }
  # The workloads that read it are the same documents.
  for pair in "Deployment kiwi" "Job migrate" "Deployment valkey" "Deployment postgres"; do
    read -r kind name <<<"$pair"
    [ "$(doc "$kind" "$name")" = "$(doc "$kind" "$name" "$MOCK")" ] || { echo "$kind $name differs between local and mockauth"; rc=1; }
  done
  config_data "$MOCK" | grep -qxF 'KIWI_OIDC_ISSUER=http://localhost:8080/mockoidc' || { echo "KIWI_OIDC_ISSUER is not http://localhost:8080/mockoidc"; rc=1; }
  config_data "$MOCK" | grep -qxF 'KIWI_OIDC_INTERNAL_URL=http://mockoidc:8090/mockoidc' || { echo "KIWI_OIDC_INTERNAL_URL is not http://mockoidc:8090/mockoidc"; rc=1; }
  config_data "$MOCK" | grep -qxF 'KIWI_WORKSPACE_DOMAIN=demo.example' || { echo "KIWI_WORKSPACE_DOMAIN is not demo.example"; rc=1; }
  # The mock addresses are accepted only because this stays local.
  config_data "$MOCK" | grep -qxF 'KIWI_ENV=local' || { echo "KIWI_ENV is not local"; rc=1; }
  return "$rc"
}
check "mockauth: the kiwi environment differs from local only in the OIDC addresses and the workspace heading" kiwi_env_differs_only_in_oidc

# --- the smoke overlay ----------------------------------------------------------------
# mockauth with the model replaced by recorded fixtures (backend/tools/llmreplay): the
# replay workload, and a kiwi whose explicit env sends its LLM and embedding calls to
# it. local and mockauth, which talk to LiteLLM and Ollama, must not carry the replay.
SMOKE="$WORK/smoke.yaml"
if kustomize build deploy/k8s/overlays/smoke >"$SMOKE" 2>"$WORK/smoke.err"; then
  ok "kustomize build deploy/k8s/overlays/smoke"
else
  bad "kustomize build deploy/k8s/overlays/smoke" "$(head -n 5 "$WORK/smoke.err")"
  : >"$SMOKE"
fi
if [ -s "$SMOKE" ] && kubeconform -strict -ignore-missing-schemas -summary - <"$SMOKE" >"$WORK/kc-smoke.out" 2>&1; then
  ok "kubeconform -strict accepts the rendered smoke overlay"
else
  bad "kubeconform -strict accepts the rendered smoke overlay" "$(head -n 5 "$WORK/kc-smoke.out" 2>/dev/null || echo 'nothing rendered')"
fi
check "smoke: no :latest and every image tagged or pinned by digest" no_latest "$SMOKE"
check "smoke: no literal secret value in any manifest" no_literal_secrets "$SMOKE"

# no_llmreplay file: neither the workload, the Service, the address nor the fixtures.
no_llmreplay() {
  [ -s "$1" ] || { echo "nothing rendered"; return 1; }
  if grep -n -i llmreplay "$1"; then echo "the overlay renders the recorded LLM"; return 1; fi
}
check "local: renders no llmreplay workload, Service, fixtures ConfigMap or address" no_llmreplay "$RENDER"
check "mockauth: renders no llmreplay workload, Service, fixtures ConfigMap or address" no_llmreplay "$MOCK"

llmreplay_workload() {
  local dep svc cm rc=0
  dep=$(doc Deployment llmreplay "$SMOKE")
  svc=$(doc Service llmreplay "$SMOKE")
  [ -n "$dep" ] || { echo "no Deployment llmreplay"; return 1; }
  [ -n "$svc" ] || { echo "no Service llmreplay"; return 1; }
  printf '%s\n' "$dep" | grep -qE '^ *image: kiwi-registry\.localhost:5001/llmreplay:[^ :]+$' || { echo "the image is not kiwi-registry.localhost:5001/llmreplay with a tag"; rc=1; }
  printf '%s\n' "$dep" | grep -qxE ' *- /app/llmreplay' || { echo "the container does not run /app/llmreplay"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *(- )?containerPort: 8089$' || { echo "the container does not listen on 8089"; rc=1; }
  printf '%s\n' "$svc" | grep -qE '^ *port: 8089$' || { echo "the Service does not expose 8089"; rc=1; }
  # It reads fixtures from a ConfigMap and no Secret.
  if printf '%s\n' "$dep" | grep -qE 'kiwi-env|secretKeyRef|secretRef'; then echo "the replay reads a Secret"; rc=1; fi
  [ "$(printf '%s\n' "$dep" | grep -cE '^ *path: /healthz$')" -eq 2 ] || { echo "the readiness and liveness probes do not both ask /healthz"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *readOnlyRootFilesystem: true$' || { echo "the root filesystem is not read-only"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *runAsNonRoot: true$' || { echo "the container may run as root"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *allowPrivilegeEscalation: false$' || { echo "the container may escalate privileges"; rc=1; }
  printf '%s\n' "$dep" | grep -qE '^ *type: RuntimeDefault$' || { echo "no seccomp profile"; rc=1; }
  printf '%s\n' "$dep" | awk '/^ *drop:$/ { d = 1; next } d && /^ *- ALL$/ { ok = 1 } END { exit !ok }' || { echo "the capabilities are not dropped"; rc=1; }
  # The fixtures volume is the generated ConfigMap, mounted read-only at /fixtures.
  cm=$(printf '%s\n' "$dep" | awk '/^ *(- )?configMap:$/ { c = 1; next } c && /^ *name: / { print $2; exit }')
  case $cm in
    llmreplay-fixtures-?*) ;;
    *) echo "the fixtures volume is the ConfigMap '$cm', want llmreplay-fixtures-<hash>"; rc=1 ;;
  esac
  [ -n "$cm" ] && [ -n "$(doc ConfigMap "$cm" "$SMOKE")" ] || { echo "the ConfigMap '$cm' of the fixtures volume is not rendered"; rc=1; }
  printf '%s\n' "$dep" | awk '/^ *- mountPath: \/fixtures$/ { m = 1; next } m && /^ *name: fixtures$/ { ok = 1 } m && /^ *readOnly: true$/ { ro = 1 } END { exit !(ok && ro) }' || { echo "the fixtures are not mounted read-only at /fixtures"; rc=1; }
  return "$rc"
}
check "smoke: Deployment and Service llmreplay on 8089, pinned image, probes on /healthz, hardened, no Secret, fixtures ConfigMap read-only at /fixtures" llmreplay_workload

# explicit_env kind name file: NAME=value of the literal env entries of the first container.
explicit_env() {
  doc "$1" "$2" "$3" | awk '
    /^ *(- )?env:$/ { match($0, /env:/); col = RSTART; inenv = 1; next }
    inenv {
      match($0, /^ */)
      if (RLENGTH == col - 1 && $0 !~ /^ *- /) { inenv = 0; next }
    }
    inenv && /^ *- name: / { cur = $3; next }
    inenv && /^ *value: / { v = $0; sub(/^ *value: /, "", v); gsub(/^"|"$/, "", v); print cur "=" v }
  '
}
# effective_env file: the environment of kiwi, as KEY=value lines. The ConfigMap
# kiwi-config reaches it through envFrom, and an explicit env entry wins over envFrom.
effective_env() {
  { config_data "$1"; explicit_env Deployment kiwi "$1"; } | awk -F= '{ k = $1; m[k] = substr($0, length(k) + 2) } END { for (k in m) print k "=" m[k] }' | sort
}

kiwi_talks_to_llmreplay() {
  local changed rc=0 pair kind name allowed
  { [ -s "$RENDER" ] && [ -s "$MOCK" ] && [ -s "$SMOKE" ]; } || { echo "nothing rendered"; return 1; }
  # Against mockauth, only the two addresses of the model change.
  changed=$(diff <(effective_env "$MOCK") <(effective_env "$SMOKE") | sed -nE 's/^[<>] ([A-Z0-9_]+)=.*/\1/p' | sort -u | tr '\n' ' ')
  [ "$changed" = "KIWI_EMBED_BASE_URL KIWI_LLM_BASE_URL " ] || { printf 'the kiwi environment differs from mockauth in: %s\nwant: KIWI_EMBED_BASE_URL KIWI_LLM_BASE_URL\n' "$changed"; rc=1; }
  # Against local, those two and what mockauth changes.
  changed=$(diff <(effective_env "$RENDER") <(effective_env "$SMOKE") | sed -nE 's/^[<>] ([A-Z0-9_]+)=.*/\1/p' | sort -u | tr '\n' ' ')
  [ "$changed" = "KIWI_EMBED_BASE_URL KIWI_LLM_BASE_URL KIWI_OIDC_INTERNAL_URL KIWI_OIDC_ISSUER KIWI_WORKSPACE_DOMAIN KIWI_WORKSPACE_NAME " ] || { printf 'the kiwi environment differs from local in: %s\n' "$changed"; rc=1; }
  for name in KIWI_LLM_BASE_URL KIWI_EMBED_BASE_URL; do
    effective_env "$SMOKE" | grep -qxF "$name=http://llmreplay:8089" || { echo "$name is not http://llmreplay:8089"; rc=1; }
  done
  # The URLs are the origin without /v1: the adapter appends the path.
  effective_env "$SMOKE" | grep -E '^KIWI_(LLM|EMBED)_BASE_URL=.*/v1' && { echo "a base URL carries /v1"; rc=1; }
  # The addresses are set by an explicit env entry on kiwi, which is what wins over the Secret.
  for name in KIWI_LLM_BASE_URL KIWI_EMBED_BASE_URL; do
    explicit_env Deployment kiwi "$SMOKE" | grep -qxF "$name=http://llmreplay:8089" || { echo "$name is not an explicit env entry of Deployment kiwi"; rc=1; }
  done
  # Nothing else of the stack moves: the same documents as mockauth, and a kiwi
  # Deployment that differs only by that env stanza.
  for pair in "ConfigMap kiwi-config" "Job migrate" "Deployment postgres" "Deployment valkey" "Deployment ollama" "Deployment web" "Deployment mockoidc" "Service kiwi" "Ingress kiwi" "Ingress mockoidc"; do
    read -r kind name <<<"$pair"
    [ "$(doc "$kind" "$name" "$MOCK")" = "$(doc "$kind" "$name" "$SMOKE")" ] || { echo "$kind $name differs between mockauth and smoke"; rc=1; }
  done
  allowed='^[<>] +(- )?(env|envFrom):$|^> +- name: KIWI_(LLM|EMBED)_BASE_URL$|^> +value: http://llmreplay:8089$'
  diff <(doc Deployment kiwi "$MOCK") <(doc Deployment kiwi "$SMOKE") | grep -E '^[<>]' | grep -vE "$allowed" >"$WORK/kiwi-diff.out"
  [ ! -s "$WORK/kiwi-diff.out" ] || { printf 'Deployment kiwi differs from mockauth beyond its env:\n'; head -n 5 "$WORK/kiwi-diff.out"; rc=1; }
  # Still a local stack: the replay is no way to run prod.
  config_data "$SMOKE" | grep -qxF 'KIWI_ENV=local' || { echo "KIWI_ENV is not local"; rc=1; }
  return "$rc"
}
check "smoke: kiwi sends its LLM and embedding calls to llmreplay, and its environment differs from mockauth in nothing else" kiwi_talks_to_llmreplay

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

# The app port carries the passwordless mock chooser and a Secret holding the real LLM key:
# it must not be reachable from the network. k3d's Simple config hands the port string to
# the docker nat parser, which reads [HOST_IP:]HOST_PORT:CONTAINER_PORT.
check "k3d app port is published on loopback only (127.0.0.1:8080:80)" grep -qE '^ +- port: 127\.0\.0\.1:8080:80$' deploy/local/k3d.yaml
check "k3d publishes no port without a loopback address" bash -c "! grep -E '^ +- port: ' deploy/local/k3d.yaml | grep -vE '^ +- port: 127\\.0\\.0\\.1:'"
# The Kubernetes API port of the load balancer is published on a host address too: k3d's default is
# every interface. kubeAPI.hostIP moves it to loopback (kubectl reaches it through 127.0.0.1).
k3d_api_on_loopback() { awk '/^kubeAPI:$/ { b = 1; next } /^[^ #]/ { b = 0 } b && /^  hostIP: "?127\.0\.0\.1"?$/ { ok = 1 } END { exit !ok }' deploy/local/k3d.yaml; }
check "k3d Kubernetes API port is published on loopback only (kubeAPI.hostIP 127.0.0.1)" k3d_api_on_loopback

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

# An `if overlay not in [...]:` line, outside comments, whose next line calls fail(.
tilt_overlay_guard() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    want { if ($0 ~ /^[ \t]+fail\(/) ok = 1; want = 0 }
    /^if overlay not in \[.*\]:$/ { want = 1; found = 1 }
    END { exit !(found && ok) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: an overlay other than local, mockauth or smoke is refused with fail()" tilt_overlay_guard

# The overlays the guard accepts, as the Tiltfile lists them.
tilt_accepts_overlays() { # [Tiltfile]
  grep -vE '^[ \t]*#' "${1:-Tiltfile}" | grep -qxF "if overlay not in ['local', 'mockauth', 'smoke']:"
}
check "Tiltfile: accepts exactly the overlays local, mockauth and smoke" tilt_accepts_overlays

# mockauth and smoke build the mock provider from the tools stage of backend/Dockerfile.
tilt_builds_mockoidc() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    /^if overlay in \[\047mockauth\047, \047smoke\047\]:$/ { inblk = 1; next }
    /^[^ \t]/ { inblk = 0 }
    inblk && /REGISTRY \+ \047\/mockoidc\047/ { img = 1 }
    inblk && /command=.*docker build .*--target tools/ { tgt = 1 }
    END { exit !(img && tgt) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: the mockauth and smoke overlays build the mockoidc image from the tools target of backend/Dockerfile" tilt_builds_mockoidc

# kiwi verifies tokens against the mock at startup, so under mockauth and smoke it waits for it.
tilt_kiwi_waits_for_mock() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    /^auth_deps = \[\047mockoidc\047\] if overlay in \[\047mockauth\047, \047smoke\047\] else \[\]$/ { def = 1 }
    /^k8s_resource\($/ { incall = 1; name = ""; next }
    incall && name == "" { n = $0; gsub(/[ \047,"]/, "", n); name = n; next }
    incall && name == "kiwi" && /resource_deps=.*\+ auth_deps( \+ llm_deps)?,?$/ { use = 1 }
    /^\)$/ { incall = 0 }
    END { exit !(def && use) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: kiwi waits for mockoidc under the mockauth and smoke overlays" tilt_kiwi_waits_for_mock

# smoke builds the recorded LLM from the same tools stage and gives it a resource.
tilt_builds_llmreplay() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    /^if overlay == \047smoke\047:$/ { inblk = 1; next }
    /^[^ \t]/ { inblk = 0 }
    inblk && /REGISTRY \+ \047\/llmreplay\047/ { img = 1 }
    inblk && /command=.*docker build .*--target tools/ { tgt = 1 }
    inblk && /^[ \t]+\047llmreplay\047,$/ { res = 1 }
    END { exit !(img && tgt && res) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: the smoke overlay builds the llmreplay image from the tools target of backend/Dockerfile and runs it as a resource" tilt_builds_llmreplay

# kiwi sends its model calls to the replay, so under smoke it waits for it, and under no other overlay.
tilt_kiwi_waits_for_llmreplay() { # [Tiltfile]
  awk '
    /^[ \t]*#/ { next }
    /^llm_deps = \[\047llmreplay\047\] if overlay == \047smoke\047 else \[\]$/ { def = 1 }
    /^k8s_resource\($/ { incall = 1; name = ""; next }
    incall && name == "" { n = $0; gsub(/[ \047,"]/, "", n); name = n; next }
    incall && name == "kiwi" && /resource_deps=.*\+ llm_deps,?$/ { use = 1 }
    /^\)$/ { incall = 0 }
    END { exit !(def && use) }
  ' "${1:-Tiltfile}"
}
check "Tiltfile: kiwi waits for llmreplay under the smoke overlay only" tilt_kiwi_waits_for_llmreplay

# The guards themselves: each mutated copy of the Tiltfile must be refused.
tilt_mutant() { # name check-function sed-script
  local f="$WORK/Tiltfile.mutant"
  sed -E "$3" Tiltfile >"$f"
  if cmp -s Tiltfile "$f"; then bad "$1" "the mutation changed nothing"; return; fi
  if "$2" "$f" >/dev/null 2>&1; then bad "$1" "the mutated Tiltfile is still accepted"; else ok "$1"; fi
}
tilt_mutant "migrations dependency removed (the comment naming it stays) is refused" tilt_migrations_dep "/^ *'backend\/db\/migrations',\$/d"
tilt_mutant "migrations dependency commented out is refused" tilt_migrations_dep "s/^( *)'backend\/db\/migrations',\$/\1# 'backend\/db\/migrations',/"
tilt_mutant "overlay guard deleted is refused" tilt_overlay_guard "/^if overlay not in \[.*\]:\$/,+1d"
tilt_mutant "overlay guard that no longer calls fail() is refused" tilt_overlay_guard "s/^( +)fail\(/\1print(/"
tilt_mutant "overlay guard commented out is refused" tilt_overlay_guard "s/^if overlay not in \[.*\]:\$/# &/"
tilt_mutant "an overlay list that admits another overlay is refused" tilt_accepts_overlays "s/^(if overlay not in .*)'smoke'\]/\1'smoke', 'e2e']/"
tilt_mutant "an overlay list that drops smoke is refused" tilt_accepts_overlays "s/^(if overlay not in \['local', 'mockauth'), 'smoke'\]/\1]/"
tilt_mutant "mockoidc built from another target is refused" tilt_builds_mockoidc "s/--target tools/--target kiwi/"
tilt_mutant "mockoidc image build removed is refused" tilt_builds_mockoidc "s/REGISTRY \+ '\/mockoidc'/REGISTRY + '\/other'/"
tilt_mutant "mockoidc built under mockauth only is refused" tilt_builds_mockoidc "s/^if overlay in \['mockauth', 'smoke'\]:\$/if overlay == 'mockauth':/"
tilt_mutant "kiwi not waiting for mockoidc is refused" tilt_kiwi_waits_for_mock "s/ \+ auth_deps//"
tilt_mutant "auth_deps that no longer names mockoidc is refused" tilt_kiwi_waits_for_mock "s/auth_deps = \['mockoidc'\]/auth_deps = []/"
tilt_mutant "auth_deps that leaves smoke out is refused" tilt_kiwi_waits_for_mock "s/^(auth_deps = .*) in \['mockauth', 'smoke'\]/\1 == 'mockauth'/"
tilt_mutant "llmreplay built from another target is refused" tilt_builds_llmreplay "s/--target tools/--target kiwi/"
tilt_mutant "llmreplay image build removed is refused" tilt_builds_llmreplay "s/REGISTRY \+ '\/llmreplay'/REGISTRY + '\/other'/"
tilt_mutant "llmreplay without a resource is refused" tilt_builds_llmreplay "s/^( +)'llmreplay',\$/\1'other',/"
tilt_mutant "llmreplay built under every overlay is refused" tilt_builds_llmreplay "s/^if overlay == 'smoke':\$/if overlay != 'x':/"
tilt_mutant "kiwi not waiting for llmreplay is refused" tilt_kiwi_waits_for_llmreplay "s/ \+ llm_deps//"
tilt_mutant "llm_deps that no longer names llmreplay is refused" tilt_kiwi_waits_for_llmreplay "s/llm_deps = \['llmreplay'\]/llm_deps = []/"
tilt_mutant "llm_deps that applies to every overlay is refused" tilt_kiwi_waits_for_llmreplay "s/^(llm_deps = .*) if overlay == 'smoke' else \[\]/\1 if overlay != 'x' else []/"
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

local_up_defaults_to_mockauth() {
  awk '
    /^  local:up:$/ { inblk = 1; next }
    /^  [a-z]/ { inblk = 0 }
    inblk && /OVERLAY: .\{\{\.OVERLAY \| default "mockauth"\}\}.$/ { ok = 1 }
    END { exit !ok }
  ' Taskfile.yml
}
check "Taskfile: local:up defaults to the mockauth overlay" local_up_defaults_to_mockauth

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

load_secret mockauth KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_ID=mock' "$COPY" || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_SECRET=mock' "$COPY" || verdict=bad
expect "load-env-secret mockauth: succeeds with no OAuth variable in .env at all" "$verdict" "rc=$RC" "$OUT"

load_secret mockauth KIWI_OAUTH_CLIENT_ID=client-id-from-env
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_ID=mock' "$COPY" || verdict=bad
expect "load-env-secret mockauth: a client id set in .env is replaced by the dummy (the mock takes whichever it is presented)" "$verdict" "rc=$RC" "$OUT"

load_secret local KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" KIWI_OAUTH_CLIENT_ID || verdict=bad
contains "$OUT" KIWI_OAUTH_CLIENT_SECRET || verdict=bad
[ ! -s "$LOG" ] || verdict=bad
expect "load-env-secret local: absent OAuth variables are named and nothing is applied (the real client is required)" "$verdict" "rc=$RC" "$OUT"

load_secret smoke KIWI_LLM_BASE_URL= KIWI_LLM_API_KEY= KIWI_LLM_MODEL_FLASH= KIWI_LLM_MODEL_PRO= KIWI_ASR_MODEL= KIWI_OAUTH_CLIENT_ID= KIWI_OAUTH_CLIENT_SECRET=
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
for v in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_ASR_MODEL KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET; do
  grep -qx "$v=mock" "$COPY" || verdict=bad
done
# The adapter sends json_object with no schema name for a request whose model is the pro
# one, so the two dummy model ids must differ or the replay cannot tell the calls apart.
grep -qx 'KIWI_LLM_MODEL_FLASH=mock-flash' "$COPY" || verdict=bad
grep -qx 'KIWI_LLM_MODEL_PRO=mock-pro' "$COPY" || verdict=bad
expect "load-env-secret smoke: LLM, ASR and OAuth variables default to mock" "$verdict" "rc=$RC" "$OUT"

# Secret hygiene: the .env of a developer holds the real Google client and the real LLM
# key, and an overlay that does not reach those services must not carry them into the
# cluster, whatever .env says. The values below are made up; write_env gives the LLM
# variables their own fixture values ($SECRET_LLM_URL, $PROBE_LLM, flash-model, ...).
REAL_OAUTH_ID='oauth-id-fixture-0123'
REAL_OAUTH_SECRET='oauth-secret-fixture-0123'

load_secret smoke "KIWI_OAUTH_CLIENT_ID=$REAL_OAUTH_ID" "KIWI_OAUTH_CLIENT_SECRET=$REAL_OAUTH_SECRET"
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
for v in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_ASR_MODEL KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET; do
  grep -qx "$v=mock" "$COPY" || { verdict=bad; printf 'smoke Secret: %s is not the dummy\n' "$v"; }
done
grep -qx 'KIWI_LLM_MODEL_FLASH=mock-flash' "$COPY" || { verdict=bad; printf 'smoke Secret: the flash model is not its dummy\n'; }
grep -qx 'KIWI_LLM_MODEL_PRO=mock-pro' "$COPY" || { verdict=bad; printf 'smoke Secret: the pro model is not its dummy\n'; }
for real in "$SECRET_LLM_URL" "$PROBE_LLM" flash-model pro-model asr-model "$REAL_OAUTH_ID" "$REAL_OAUTH_SECRET"; do
  grep -qF -- "$real" "$COPY" && { verdict=bad; printf 'smoke Secret holds a real-looking value of .env\n'; }
done
grep -qx "KIWI_DB_PASSWORD=$SECRET_DB_PW" "$COPY" || verdict=bad
expect "load-env-secret smoke: forces the dummies for the LLM, ASR and OAuth variables whatever .env says" "$verdict" "rc=$RC" "$OUT"

load_secret mockauth "KIWI_OAUTH_CLIENT_ID=$REAL_OAUTH_ID" "KIWI_OAUTH_CLIENT_SECRET=$REAL_OAUTH_SECRET"
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_ID=mock' "$COPY" || verdict=bad
grep -qx 'KIWI_OAUTH_CLIENT_SECRET=mock' "$COPY" || verdict=bad
for real in "$REAL_OAUTH_ID" "$REAL_OAUTH_SECRET"; do
  grep -qF -- "$real" "$COPY" && verdict=bad
done
grep -qx "KIWI_LLM_BASE_URL=$SECRET_LLM_URL" "$COPY" || verdict=bad
grep -qx "KIWI_LLM_API_KEY=$PROBE_LLM" "$COPY" || verdict=bad
grep -qx 'KIWI_LLM_MODEL_FLASH=flash-model' "$COPY" || verdict=bad
grep -qx 'KIWI_LLM_MODEL_PRO=pro-model' "$COPY" || verdict=bad
grep -qx 'KIWI_ASR_MODEL=asr-model' "$COPY" || verdict=bad
expect "load-env-secret mockauth: forces the dummy OAuth pair but keeps the real LLM values (the live smoke uses them)" "$verdict" "rc=$RC" "$OUT"

load_secret local "KIWI_OAUTH_CLIENT_ID=$REAL_OAUTH_ID" "KIWI_OAUTH_CLIENT_SECRET=$REAL_OAUTH_SECRET"
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
grep -qx "KIWI_OAUTH_CLIENT_ID=$REAL_OAUTH_ID" "$COPY" || verdict=bad
grep -qx "KIWI_OAUTH_CLIENT_SECRET=$REAL_OAUTH_SECRET" "$COPY" || verdict=bad
grep -qx "KIWI_LLM_API_KEY=$PROBE_LLM" "$COPY" || verdict=bad
grep -qx "KIWI_LLM_BASE_URL=$SECRET_LLM_URL" "$COPY" || verdict=bad
expect "load-env-secret local: keeps every real value (the Google client and the LLM)" "$verdict" "rc=$RC" "$OUT"

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

# k3d-up.sh: the cpuset hook decision and the warning about an old, public cluster (k3d, docker
# and kubectl are stubs) --------------------------------------------------------------------
K3D_STUB="$WORK/k3dstub"
mkdir -p "$K3D_STUB"
cat >"$K3D_STUB/k3d" <<'EOF'
#!/usr/bin/env bash
printf 'k3d %s\n' "$*" >>"$K3D_LOG"
if [ "$1 $2" = "cluster get" ]; then [ "${STUB_CLUSTER_EXISTS:-0}" = 1 ] && exit 0; exit 1; fi
EOF
cat >"$K3D_STUB/docker" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  info)
    # Rootless in its security options only when STUB_ROOTLESS=1; STUB_INFO_BULK=1 follows with
    # more output than a pipe holds, as the real `docker info` has.
    [ "${STUB_ROOTLESS:-0}" = 1 ] && echo "  rootless"
    [ "${STUB_INFO_BULK:-0}" = 1 ] && { head -c 400000 /dev/zero | tr '\0' x; echo; }
    ;;
  inspect)
    # The load balancer's bindings of one port, one HOST_IP|HOST_PORT per line: 6443 (the Kubernetes
    # API) from STUB_API_BINDINGS, anything else (80) from STUB_LB_BINDINGS; "!" = no such container.
    case "$*" in
      *'"6443/tcp"'*) printf '%b' "${STUB_API_BINDINGS:-}" ;;
      *)
        [ "${STUB_LB_BINDINGS:-}" != '!' ] || exit 1
        printf '%b' "${STUB_LB_BINDINGS:-}"
        ;;
    esac
    ;;
esac
exit 0
EOF
cat >"$K3D_STUB/kubectl" <<'EOF'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"$K3D_LOG"
EOF
chmod +x "$K3D_STUB/k3d" "$K3D_STUB/docker" "$K3D_STUB/kubectl"

# k3d_up mode rootless controllers: sets RC, OUT and K3D_LOG_FILE. The cluster does not exist
# unless LB_BINDINGS (or API_BINDINGS) is set: then it does, and its load balancer reports those
# bindings of port 80 (the app) and 6443 (the Kubernetes API) (HOST_IP|HOST_PORT lines; an empty
# HOST_IP is every interface; "!" = not inspectable). INFO_BULK=1 makes `docker info` long.
k3d_up() {
  local mode=$1 rootless=$2 controllers=$3 stubs=()
  CASE=$((CASE + 1))
  local d="$WORK/k3dup$CASE"
  mkdir -p "$d"
  printf '%s\n' "$controllers" >"$d/controllers"
  K3D_LOG_FILE="$d/log"
  : >"$K3D_LOG_FILE"
  if [ "${LB_BINDINGS+set}" = set ] || [ "${API_BINDINGS+set}" = set ]; then
    stubs=(STUB_CLUSTER_EXISTS=1 "STUB_LB_BINDINGS=${LB_BINDINGS-}" "STUB_API_BINDINGS=${API_BINDINGS-}")
  fi
  [ "${INFO_BULK:-0}" != 1 ] || stubs+=(STUB_INFO_BULK=1)
  env -i PATH="$K3D_STUB:$PATH" HOME="$WORK" K3D_LOG="$K3D_LOG_FILE" STUB_ROOTLESS="$rootless" ${stubs[@]+"${stubs[@]}"} \
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

# A `docker info` longer than a pipe holds must not hide the rootless line (grep -q exits at the
# match, and under pipefail the writer's SIGPIPE used to fail the whole test).
INFO_BULK=1 k3d_up "" 1 "cpu io memory pids"
unset INFO_BULK
verdict=ok
[ "$RC" -eq 0 ] && hook_applied && contains "$OUT" "cpuset hook: on" || verdict=bad
expect "k3d-up auto: a long docker info still reports rootless" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

# An existing cluster keeps the port binding it was created with; k3d-up says when that is public.
warned() { contains "$OUT" "WARNING" && contains "$OUT" "8080" && contains "$OUT" "task local:down" && contains "$OUT" "task local:up"; }
started() { grep -qxF 'k3d cluster start kiwi' "$K3D_LOG_FILE" && ! grep -q 'cluster create' "$K3D_LOG_FILE"; }

LB_BINDINGS='|8080\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && warned && contains "$OUT" "all interfaces:8080" || verdict=bad
expect "k3d-up: an existing cluster that publishes 8080 on every interface (empty host IP) is started and warned about" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

LB_BINDINGS='0.0.0.0|8080\n::|8080\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && warned && contains "$OUT" "0.0.0.0:8080" || verdict=bad
expect "k3d-up: 0.0.0.0 and :: bindings are warned about" "$verdict" "rc=$RC" "$OUT"

LB_BINDINGS='127.0.0.1|8080\n|8080\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && warned || verdict=bad
expect "k3d-up: one loopback binding does not excuse another that is public" "$verdict" "rc=$RC" "$OUT"

LB_BINDINGS='127.0.0.1|8080\n::1|8080\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && ! contains "$OUT" "WARNING" || verdict=bad
expect "k3d-up: an existing cluster published on loopback only is started without a warning" "$verdict" "rc=$RC" "$OUT"

LB_BINDINGS='!' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && ! contains "$OUT" "WARNING" || verdict=bad
expect "k3d-up: a load balancer that cannot be inspected is no reason to fail or to warn" "$verdict" "rc=$RC" "$OUT"

# The Kubernetes API port: an old cluster has it on 0.0.0.0, the new config binds loopback.
api_warned() { contains "$OUT" "WARNING" && contains "$OUT" "Kubernetes API" && contains "$OUT" "task local:down" && contains "$OUT" "task local:up"; }

API_BINDINGS='0.0.0.0|37587\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && api_warned && contains "$OUT" "0.0.0.0:37587" || verdict=bad
contains "$OUT" "spend the model quota" && verdict=bad
expect "k3d-up: an old cluster whose Kubernetes API is on 0.0.0.0 is warned about, without the app-port warning" "$verdict" "rc=$RC" "$OUT"

API_BINDINGS='127.0.0.1|37587\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && ! contains "$OUT" "WARNING" || verdict=bad
expect "k3d-up: a Kubernetes API published on loopback is started without a warning" "$verdict" "rc=$RC" "$OUT"

LB_BINDINGS='|8080\n' API_BINDINGS='|37587\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && warned && api_warned && contains "$OUT" "all interfaces:37587" || verdict=bad
expect "k3d-up: both public ports of an old cluster are each warned about" "$verdict" "rc=$RC" "$OUT"

LB_BINDINGS='127.0.0.1|8080\n' API_BINDINGS='0.0.0.0|37587\n' k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && started && api_warned && ! contains "$OUT" "spend the model quota" || verdict=bad
expect "k3d-up: a loopback app port does not hide a public Kubernetes API" "$verdict" "rc=$RC" "$OUT"

k3d_up 0 0 ""
verdict=ok
[ "$RC" -eq 0 ] && grep -q 'cluster create --config deploy/local/k3d.yaml' "$K3D_LOG_FILE" && ! contains "$OUT" "WARNING" || verdict=bad
expect "k3d-up: a cluster that does not exist yet is created from the config, with no warning" "$verdict" "rc=$RC" "$OUT" "$(cat "$K3D_LOG_FILE")"

# --- the guard suites are wired ----------------------------------------------------------------
# A suite that nothing runs guards nothing. Every scripts/**/*.test.sh is part of `task
# check:scripts` and of a CI job (the hygiene suites in the hygiene workflow, the others in ci.yml),
# and the generated-code drift check has its own task and job.
# suites_wired [Taskfile [ci.yml [hygiene.yml]]]
suites_wired() {
  local taskfile=${1:-Taskfile.yml} ci=${2:-.github/workflows/ci.yml} hygiene=${3:-.github/workflows/hygiene.yml}
  local rc=0 suite block workflow
  block=$(awk '/^  check:scripts:$/ { b = 1; next } /^  [^ ]/ { b = 0 } b' "$taskfile")
  [ -n "$block" ] || { echo "$taskfile has no task check:scripts"; return 1; }
  for suite in $(find scripts -name '*.test.sh' | sort); do
    printf '%s\n' "$block" | grep -qxF "      - bash $suite" || { echo "task check:scripts does not run $suite"; rc=1; }
    case $suite in scripts/hygiene/*) workflow=$hygiene ;; *) workflow=$ci ;; esac
    grep -qE "(run: |^ +)bash $suite\$" "$workflow" || { echo "$workflow does not run $suite"; rc=1; }
  done
  grep -qxF "      - bash scripts/check-gen-drift.sh" <(awk '/^  gen:check:$/ { b = 1; next } /^  [^ ]/ { b = 0 } b' "$taskfile") || { echo "task gen:check does not run scripts/check-gen-drift.sh"; rc=1; }
  grep -qE "run: bash scripts/check-gen-drift\.sh\$" "$ci" || { echo "$ci does not run scripts/check-gen-drift.sh"; rc=1; }
  return "$rc"
}
check "every guard suite runs from task check:scripts and from CI; the drift check from task gen:check and CI" suites_wired

# The CI jobs build their tools with `go install` at an exact release; it must be a release of the
# minor that mise.toml pins, so that CI and a developer's mise run the same tools.
ci_tools_match_mise() { # [mise.toml [ci.yml]]
  local mise=${1:-mise.toml} ci=${2:-.github/workflows/ci.yml} rc=0 pair tool var pin want
  for pair in kustomize:KUSTOMIZE_VERSION kubeconform:KUBECONFORM_VERSION task:TASK_VERSION buf:BUF_VERSION sqlc:SQLC_VERSION caddy:CADDY_VERSION; do
    tool=${pair%%:*}
    var=${pair##*:}
    pin=$(awk -F'"' -v t="$tool" '$1 ~ "^" t " *= *$" { print $2; exit }' "$mise")
    want=$(awk -v v="$var:" '$1 == v { print $2; exit }' "$ci")
    [ -n "$pin" ] || { echo "$mise pins no $tool"; rc=1; continue; }
    [ -n "$want" ] || { echo "$ci sets no $var"; rc=1; continue; }
    case $want in v"$pin".*) ;; *) echo "$var=$want is not a release of $tool $pin (mise.toml)"; rc=1 ;; esac
  done
  return "$rc"
}
check "the tool versions CI installs are releases of the minors mise.toml pins" ci_tools_match_mise

# render.test.sh validates the Caddyfile with caddy: the scripts job builds it too (go install, no action).
ci_installs_caddy() { grep -qF "go install \"github.com/caddyserver/caddy/v2/cmd/caddy@\${CADDY_VERSION}\"" .github/workflows/ci.yml; }
check "ci.yml: the scripts job installs caddy with go install at CADDY_VERSION, so the Caddyfile is validated" ci_installs_caddy

# Prove the two guards: each fails when what it guards is taken out.
mkdir -p "$WORK/wired"
sed '/scripts\/hygiene\/gitleaks.test.sh/d' .github/workflows/hygiene.yml >"$WORK/wired/hygiene.yml"
if suites_wired Taskfile.yml .github/workflows/ci.yml "$WORK/wired/hygiene.yml" >"$WORK/wired/out" 2>&1; then
  bad "suites_wired accepts a hygiene workflow that no longer runs gitleaks.test.sh"
elif grep -q 'hygiene.yml does not run scripts/hygiene/gitleaks.test.sh' "$WORK/wired/out"; then ok "suites_wired refuses a hygiene workflow that no longer runs gitleaks.test.sh"
else bad "suites_wired refused the hygiene workflow for another reason" "$(head -n 3 "$WORK/wired/out")"; fi
sed '/- bash scripts\/smoke\/run.test.sh/d' Taskfile.yml >"$WORK/wired/Taskfile.yml"
if suites_wired "$WORK/wired/Taskfile.yml" >"$WORK/wired/out" 2>&1; then
  bad "suites_wired accepts a Taskfile whose check:scripts lacks run.test.sh"
elif grep -q 'task check:scripts does not run scripts/smoke/run.test.sh' "$WORK/wired/out"; then ok "suites_wired refuses a Taskfile whose check:scripts lacks run.test.sh"
else bad "suites_wired refused the Taskfile for another reason" "$(head -n 3 "$WORK/wired/out")"; fi
sed 's/^  KUSTOMIZE_VERSION: v5\.8\.0$/  KUSTOMIZE_VERSION: v5.7.1/' .github/workflows/ci.yml >"$WORK/wired/ci.yml"
if ci_tools_match_mise mise.toml "$WORK/wired/ci.yml" >"$WORK/wired/out" 2>&1; then
  bad "ci_tools_match_mise accepts kustomize v5.7.1 against mise.toml 5.8"
elif grep -q 'KUSTOMIZE_VERSION=v5.7.1 is not a release of kustomize 5.8' "$WORK/wired/out"; then ok "ci_tools_match_mise refuses kustomize v5.7.1 against mise.toml 5.8"
else bad "ci_tools_match_mise refused ci.yml for another reason" "$(head -n 3 "$WORK/wired/out")"; fi

if [ "$failures" -ne 0 ]; then
  printf '\nrender.test.sh: %d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nrender.test.sh: all checks passed\n'
