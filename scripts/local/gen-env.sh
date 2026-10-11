#!/usr/bin/env bash
# Prepares .env for the local stack: creates it from .env.example when absent,
# appends the names the example lists and the file lacks, and fills the values
# nobody should have to invent (the two base64 keys and the database password)
# when they are empty. A value that is already set is never touched. The file
# is kept at mode 600 and no value is ever printed, only names.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE="${ENV_FILE:-$ROOT/.env}"
EXAMPLE="$ROOT/.env.example"
GENERATED=(KIWI_TOKEN_ENC_KEY KIWI_BLOB_SIGNING_KEY KIWI_DB_PASSWORD)

umask 077

if [ ! -f "$ENV_FILE" ]; then
  cp "$EXAMPLE" "$ENV_FILE"
  echo "gen-env: created .env from .env.example"
fi
chmod 600 "$ENV_FILE"

# A final line without a newline would swallow the first appended name.
if [ -s "$ENV_FILE" ] && [ -n "$(tail -c 1 "$ENV_FILE")" ]; then
  echo >>"$ENV_FILE"
fi

while IFS= read -r name; do
  if ! grep -q "^${name}=" "$ENV_FILE"; then
    printf '%s=\n' "$name" >>"$ENV_FILE"
    echo "gen-env: added $name"
  fi
done < <(sed -n 's/^\([A-Z][A-Z0-9_]*\)=.*/\1/p' "$EXAMPLE")

# value_of NAME: the value of the first NAME= line, without surrounding quotes or blanks.
value_of() {
  sed -n "/^$1=/{s/^$1=//;p;q;}" "$ENV_FILE" | tr -d "\"' \t\r"
}

for name in "${GENERATED[@]}"; do
  if [ -z "$(value_of "$name")" ]; then
    GEN_VALUE="$(openssl rand -base64 32)"
    export GEN_VALUE
    tmp="$(mktemp "$ENV_FILE.XXXXXX")"
    awk -v name="$name" '
      !done && index($0, name "=") == 1 { print name "=" ENVIRON["GEN_VALUE"]; done = 1; next }
      { print }
    ' "$ENV_FILE" >"$tmp"
    mv "$tmp" "$ENV_FILE"
    unset GEN_VALUE
    echo "gen-env: generated $name"
  fi
done
