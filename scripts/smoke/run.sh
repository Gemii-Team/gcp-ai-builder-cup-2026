#!/usr/bin/env bash
# Usage: scripts/smoke/run.sh [playwright arguments]      (task smoke -- --list)
#
# The five flows in headless Chromium against the k3d stack, from a clean state:
#   1. resolves the Playwright browsers (nix, unless PLAYWRIGHT_BROWSERS_PATH is set);
#   2. creates the k3d cluster if absent, loads .env into Secret kiwi-env for the overlay
#      (smoke forces dummies for the LLM and the Google client; mockauth keeps the real LLM values)
#      and brings the overlay up with `tilt ci` (it leaves every resource deployed);
#   3. resets the application data (below) and seeds the demo workspace through the replay;
#   4. runs `pnpm --filter e2e smoke` against http://localhost:8080;
#   5. returns the cluster to overlay mockauth, whatever the result, and says so on the last line.
#
# KIWI_LIVE_E2E=1 runs the @live tests against the real LiteLLM and Ollama (overlay mockauth) instead
# of the recorded LLM (overlay smoke). The exit status is that of Playwright (or of the first step that failed).
#
# The reset: as the Postgres superuser one DO block truncates every table of the application schemas
# (agents, chat, files, identity, knowledge, meetings, platform, tasks; goose's version table in public
# stays, no migration inserts rows into those tables), Valkey is flushed (sessions, tickets, caches) and
# kiwi is restarted. So every run starts from what `kiwi seed` makes under the overlay of the run, never
# from hand-made data or from vectors a real embedder stored (the model name would not re-embed them).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ROOT="$PWD"
[ "${1:-}" != -- ] || shift

live=0
[ "${KIWI_LIVE_E2E:-}" != 1 ] || live=1
overlay=smoke
[ "$live" = 0 ] || overlay=mockauth

kc=(kubectl --context k3d-kiwi -n kiwi)
say() { printf 'smoke: %s\n' "$*"; }

# 1. The browsers. Playwright's own download does not run on NixOS: the nix package matches the
# version of @playwright/test pinned in e2e/package.json. A machine with a normal install sets
# PLAYWRIGHT_BROWSERS_PATH and nothing here changes it.
if [ -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]; then
  say "PLAYWRIGHT_BROWSERS_PATH is not set: building nixpkgs#playwright-driver.browsers (nix build --no-link)"
  PLAYWRIGHT_BROWSERS_PATH="$(nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths)"
  export PLAYWRIGHT_BROWSERS_PATH
  export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
fi
say "Playwright browsers: $PLAYWRIGHT_BROWSERS_PATH"

up() { # overlay: Secret, then the stack
  bash scripts/local/load-env-secret.sh "$1"
  tilt ci --timeout 20m -- --overlay "$1"
}

# Whatever the result, the cluster goes back to mockauth: `task local:up` defaults to it, and the
# overlay switch does not prune what the smoke overlay added (the replay server and its fixtures), so
# those are deleted by name. Armed before the first change to the cluster.
restore() {
  local rc=$?
  trap - EXIT
  set +e
  if [ "$overlay" = smoke ]; then
    say "returning the cluster to overlay mockauth"
    up mockauth
    "${kc[@]}" delete deployment/llmreplay service/llmreplay --ignore-not-found
    for cm in $("${kc[@]}" get configmap -o name | grep '^configmap/llmreplay-fixtures-' || true); do
      "${kc[@]}" delete "$cm" --ignore-not-found
    done
    say "deleted the llmreplay Deployment, Service and fixtures ConfigMap (Tilt does not prune)"
  fi
  say "done (exit $rc); the cluster is on overlay mockauth"
  exit "$rc"
}
trap restore EXIT

# 2. The cluster and the overlay.
bash scripts/local/k3d-up.sh
bash scripts/local/gen-env.sh
say "bringing up overlay $overlay"
up "$overlay"

# 3. The reset, then the seed (the real services; under smoke every model call is answered by the replay).
say "resetting the application data"
"${kc[@]}" exec -i deploy/postgres -- psql -U postgres -d kiwi -v ON_ERROR_STOP=1 -q <<'SQL'
DO $$
DECLARE stmt text;
BEGIN
  SELECT 'TRUNCATE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE'
    INTO stmt FROM pg_tables
   WHERE schemaname IN ('agents','chat','files','identity','knowledge','meetings','platform','tasks');
  EXECUTE stmt;
END $$;
SQL
"${kc[@]}" exec deploy/valkey -- valkey-cli FLUSHALL >/dev/null
"${kc[@]}" rollout restart deploy/kiwi >/dev/null
"${kc[@]}" rollout status deploy/kiwi --timeout=180s
say "seeding demo.example with alice and bob"
task seed -- --domain demo.example --members alice@demo.example,bob@demo.example

# The suite runs here; the trap above has already been armed.
KIWI_BASE_URL=http://localhost:8080 pnpm --filter e2e smoke "$@"
