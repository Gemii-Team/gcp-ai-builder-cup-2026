#!/usr/bin/env bash
# Prints a complete environment for this stack: the public host, fresh secrets and the Gemini key.
#   ./make-env.sh <public-host[:port]> <file holding GEMINI_API_KEY=...> > .env
# Paste the output into Dokploy's Environment tab, or save it as .env for `docker compose --profile standalone up`.
set -euo pipefail
host="$1"; keyfile="$2"
gemini=$(sed -n 's/^GEMINI_API_KEY=//p' "$keyfile")
[ -n "$gemini" ] || { echo "no GEMINI_API_KEY in $keyfile" >&2; exit 1; }
port=""; case "$host" in *:*) port="${host##*:}" ;; esac
echo "PUBLIC_HOST=$host"
echo "PUBLIC_URL=https://$host"
if [ -n "$port" ]; then echo "HTTPS_PORT=$port"; echo "HTTP_PORT=8081"; fi
echo "GEMINI_API_KEY=$gemini"
echo "LITELLM_MASTER_KEY=sk-$(openssl rand -hex 16)"
echo "POSTGRES_SUPERUSER_PASSWORD=$(openssl rand -hex 24)"
echo "KIWI_DB_PASSWORD=$(openssl rand -hex 24)"
echo "KIWI_TOKEN_ENC_KEY=$(openssl rand -base64 32)"
echo "KIWI_BLOB_SIGNING_KEY=$(openssl rand -base64 32)"
