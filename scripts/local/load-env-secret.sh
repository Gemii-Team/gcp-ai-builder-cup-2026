#!/usr/bin/env bash
# Usage: load-env-secret.sh [overlay]
#   overlay: local (default) | mockauth | smoke | minimal | minimal-smoke
#
# Builds the Secret kiwi-env in namespace kiwi from .env and applies it to the
# local k3d cluster. It prints no value: the Secret is built from a private
# temporary env file, never put on a command line, and kubectl's output is not
# shown. Every missing required variable is named in one error. Overlays that do not
# reach the real LLM or the real Google client get dummies for them (see below), so
# the real values of .env never leave it for those overlays. minimal gets what mockauth
# gets and minimal-smoke what smoke gets: the minimal stack differs from them in the pods it
# runs, not in what its Secret holds.
set -euo pipefail

# Only .env may supply a value. A KIWI_* exported in the calling shell would
# otherwise satisfy a variable missing from .env and end up in the Secret.
while IFS= read -r inherited; do
  unset "$inherited"
done < <(compgen -v KIWI_)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE="${ENV_FILE:-$ROOT/.env}"
# Always the k3d cluster, never the current context or the caller's environment:
# a Secret holding the LLM key must not reach another cluster.
KUBE_CONTEXT=k3d-kiwi
NAMESPACE=kiwi
overlay="${1:-local}"

die() {
  echo "load-env-secret: $1" >&2
  exit "${2:-1}"
}

case "$overlay" in
  local | mockauth | smoke | minimal | minimal-smoke) ;;
  *) die "unknown overlay '$overlay' (want local, mockauth, smoke, minimal or minimal-smoke)" 2 ;;
esac
[ -f "$ENV_FILE" ] || die "$ENV_FILE not found; run scripts/local/gen-env.sh and fill in the LLM values"

# NAME=value lines (optionally "export NAME=value") are read, never evaluated: a
# value may hold $, quotes or backticks.
while IFS= read -r line || [ -n "$line" ]; do
  line="${line%$'\r'}"
  line="${line#"${line%%[![:space:]]*}"}"
  case "$line" in '' | '#'*) continue ;; esac
  if [[ "$line" =~ ^export[[:space:]]+ ]]; then
    line="${line#"${BASH_REMATCH[0]}"}"
  fi
  name="${line%%=*}"
  [[ "$name" =~ ^KIWI_[A-Z0-9_]+$ ]] || continue
  value="${line#*=}"
  case "$value" in
    \"*\") value="${value#\"}"; value="${value%\"}" ;;
    \'*\') value="${value#\'}"; value="${value%\'}" ;;
  esac
  printf -v "$name" '%s' "$value"
done <"$ENV_FILE"

# Overlays that never reach a real service need no real value. smoke and minimal-smoke
# replay the LLM too; mockauth and minimal sign in through the mock provider, which takes
# whatever client id and secret it is presented. The developer's .env holds the real Google
# client and the real LLM key, so for these overlays the values of the services they
# do not reach are not read from it at all: smoke and minimal-smoke get the dummies for the
# LLM, ASR and OAuth variables, mockauth and minimal the dummies for the OAuth pair (their
# LLM values stay real: the live smoke run uses them), local keeps every real value.
# printf -v keeps the placeholder from looking like an assignment of a secret.
# The two model ids must differ: the adapter treats a request whose model equals
# KIWI_LLM_MODEL_PRO as a pro-tier one and sends json_object with no schema name
# (so the replay could not tell a file summary from an answer); with both set to
# `mock` every call would take that path.
dummy=mock
case "$overlay" in
  smoke | minimal-smoke)
    for name in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_ASR_MODEL KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET; do
      printf -v "$name" '%s' "$dummy"
    done
    printf -v KIWI_LLM_MODEL_FLASH '%s' "$dummy-flash"
    printf -v KIWI_LLM_MODEL_PRO '%s' "$dummy-pro"
    ;;
  mockauth | minimal)
    for name in KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET; do
      printf -v "$name" '%s' "$dummy"
    done
    ;;
esac

required=(
  KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_LLM_MODEL_FLASH KIWI_LLM_MODEL_PRO KIWI_ASR_MODEL
  KIWI_OAUTH_CLIENT_ID KIWI_OAUTH_CLIENT_SECRET
  KIWI_TOKEN_ENC_KEY KIWI_BLOB_SIGNING_KEY KIWI_DB_PASSWORD
)
missing=()
for name in "${required[@]}"; do
  [ -n "${!name:-}" ] || missing+=("$name")
done
if [ "${#missing[@]}" -gt 0 ]; then
  echo "load-env-secret: $ENV_FILE has no value for: ${missing[*]}" >&2
  echo "  scripts/local/gen-env.sh generates KIWI_TOKEN_ENC_KEY, KIWI_BLOB_SIGNING_KEY and KIWI_DB_PASSWORD;" >&2
  echo "  the others are yours to fill in (see .env.example)." >&2
  exit 1
fi

# urlencode STRING: percent-encodes everything but unreserved characters, so
# an `openssl rand -base64` password (+ / =) survives inside a connection URL.
urlencode() {
  local LC_ALL=C s="$1" out='' c i
  for ((i = 0; i < ${#s}; i++)); do
    c="${s:i:1}"
    case "$c" in
      [A-Za-z0-9._~-]) out+="$c" ;;
      *) out+="$(printf '%%%02X' "'$c")" ;;
    esac
  done
  printf '%s' "$out"
}

umask 077
envfile="$(mktemp)"
trap 'rm -f "$envfile"' EXIT

encoded_password="$(urlencode "$KIWI_DB_PASSWORD")"
dsn() { printf 'postgres://%s:%s@postgres:5432/kiwi?sslmode=disable' "$1" "$encoded_password"; }

{
  for name in "${required[@]}"; do
    printf '%s=%s\n' "$name" "${!name}"
  done
  printf 'KIWI_DATABASE_URL=%s\n' "$(dsn kiwi_app)"
  printf 'KIWI_DATABASE_SYSTEM_URL=%s\n' "$(dsn kiwi_system)"
  printf 'KIWI_MIGRATE_DATABASE_URL=%s\n' "$(dsn kiwi_owner)"
  # Presentational and optional: only when set.
  for name in KIWI_WORKSPACE_NAME KIWI_WORKSPACE_DOMAIN; do
    if [ -n "${!name:-}" ]; then
      printf '%s=%s\n' "$name" "${!name}"
    fi
  done
} >"$envfile"

kc=(kubectl --context "$KUBE_CONTEXT")
"${kc[@]}" create namespace "$NAMESPACE" --dry-run=client -o yaml | "${kc[@]}" apply -f - >/dev/null
"${kc[@]}" -n "$NAMESPACE" create secret generic kiwi-env --from-env-file="$envfile" --dry-run=client -o yaml |
  "${kc[@]}" apply -f - >/dev/null

echo "load-env-secret: Secret kiwi-env applied in namespace $NAMESPACE on context $KUBE_CONTEXT ($(wc -l <"$envfile" | tr -d ' ') keys, overlay $overlay)"
