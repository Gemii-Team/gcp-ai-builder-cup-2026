#!/usr/bin/env bash
# Tests for .gitleaks.toml. Every sample is assembled at runtime from fragments,
# so this file holds no key-shaped literal and needs no allowlist entry.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONFIG="$ROOT/.gitleaks.toml"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

failures=0
cases=0

report() { # name ok|no detail
  if [ "$2" = ok ]; then
    echo "PASS $1"
  else
    echo "FAIL $1: $3"
    failures=$((failures + 1))
  fi
}

# litellm_key prints a key-shaped value: "sk-" followed by 24 characters.
litellm_key() { printf 'sk-%s' "$(printf 'a%.0s' $(seq 24))"; }

# assign NAME [VALUE] prints "KIWI_NAME=VALUE" (VALUE may be empty).
assign() { printf '%s%s=%s\n' KIWI_ "$1" "${2:-}"; }

# scan name want rule : scans $WORK/sample (built by the caller) with the project config.
# want 0 = clean; want 2 = a leak, reported under rule id $3. Exit 2 is requested for
# leaks so a broken or missing config (gitleaks exits 1) can never pass as "leak found".
scan() {
  local name=$1 want=$2 rule=${3:-} out code report_file="$WORK/report.json"
  rm -f "$report_file"
  out=$(gitleaks dir "$WORK/sample" -c "$CONFIG" --redact --no-banner --no-color \
    --exit-code 2 --log-level error -f json -r "$report_file" 2>&1)
  code=$?
  cases=$((cases + 1))
  if [ "$code" -ne "$want" ]; then
    report "$name" no "gitleaks exit $code, want $want (output: $out)"
  elif [ -n "$rule" ] && ! grep -q "\"RuleID\": \"$rule\"" "$report_file" 2>/dev/null; then
    report "$name" no "leak not reported under rule $rule"
  else
    report "$name" ok
  fi
}

sample() { # builds a fresh sample dir whose single file receives stdin
  rm -rf "$WORK/sample"
  mkdir -p "$WORK/sample"
  cat >"$WORK/sample/sample.env"
}

# --- litellm-sk-key ---------------------------------------------------------
{ printf 'token = '; litellm_key; printf '\n'; } | sample
scan "sk- key of 24 characters is a leak" 2 litellm-sk-key

printf 'token = sk-%s\n' "$(printf 'b%.0s' $(seq 16))" | sample
scan "sk- key of exactly 16 characters is a leak" 2 litellm-sk-key

printf 'token = sk-%s\n' "$(printf 'b%.0s' $(seq 8))" | sample
scan "sk- value of 8 characters is not a key" 0

printf 'see the task-%s runner\n' "$(printf 'c%.0s' $(seq 24))" | sample
scan "sk- inside a longer word is not a key" 0

# --- kiwi-env-assignment-with-value -------------------------------------------
vars="LLM_API_KEY LLM_BASE_URL OAUTH_CLIENT_SECRET TOKEN_ENC_KEY DATABASE_URL DATABASE_RO_URL BLOB_SIGNING_KEY DB_PASSWORD"
for v in $vars; do
  assign "$v" abc | sample
  scan "KIWI_$v=<value> is a leak" 2 kiwi-env-assignment-with-value
  assign "$v" | sample
  scan "KIWI_$v= (empty) is clean" 0
done

assign LLM_API_KEY abc | sed 's/^/  /' | sample
scan "indented KIWI_LLM_API_KEY=<value> is a leak" 2 kiwi-env-assignment-with-value

printf '%sLLM_API_KEY = abc\n' KIWI_ | sample
scan "spaces around = are still a leak" 2 kiwi-env-assignment-with-value

{ for v in $vars; do assign "$v"; done; } | sample
scan ".env.example shape (every name, empty value) is clean" 0

{ assign LLM_API_KEY; printf '# comment\n'; assign ENV local; assign ROLES api,worker; } | sample
scan "empty value followed by other lines and non-secret names is clean" 0

if [ "$failures" -ne 0 ]; then
  echo "gitleaks.test.sh: $failures of $cases case(s) failed"
  exit 1
fi
echo "gitleaks.test.sh: all $cases cases passed"
