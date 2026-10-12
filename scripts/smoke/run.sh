#!/usr/bin/env bash
# Usage: scripts/smoke/run.sh [playwright arguments]      (task smoke -- --list)
#
# The five flows in headless Chromium against the k3d stack, from a clean state.
#
# WARNING: this WIPES the workspace data of the cluster (every table of the application
# schemas and Valkey: see reset.sh), twice: before the run, so that it starts from what the
# seed makes, and after it, so that the overlay the cluster is left on is usable at once. A
# developer's real-Google "Team" workspace and its Meet grant are gone after it.
#
#   1. checks the Playwright browsers (nix, unless PLAYWRIGHT_BROWSERS_PATH is set);
#   2. creates the k3d cluster if absent, loads .env into Secret kiwi-env for the overlay
#      (smoke forces dummies for the LLM and the Google client; mockauth keeps the real LLM
#      values and forces a dummy Google client) and brings the overlay up with `tilt ci`
#      (it leaves every resource deployed);
#   3. wipes the data and seeds demo.example with alice and bob (through the replay);
#   4. runs `pnpm --filter e2e smoke` against http://localhost:8080;
#   5. returns the cluster to overlay mockauth, deletes what the smoke overlay added (Tilt
#      does not prune), wipes the data again and runs the default `task seed` under
#      mockauth (real LLM and Ollama: the replay's vectors must not stay under the real
#      embedding model's name). The last line says which overlay the cluster is on.
#
# `-- --list` only lists the suite: it touches neither the cluster nor the browsers.
#
# KIWI_LIVE_E2E=1 runs the @live tests against the real LiteLLM and Ollama (overlay
# mockauth throughout) instead of the recorded LLM (overlay smoke); it needs the real LLM
# values in .env. A machine whose .env has none (the keyless machine the smoke overlay is
# for) cannot start mockauth: the replay run then leaves the cluster ON SMOKE (replay-seeded),
# says so on its last line, and fails only if the suite failed.
#
# The exit status is that of the suite (or of the first step that failed); a restore that
# failed (back to mockauth, wipe or seed) makes it non-zero too, and the last line says
# what state the cluster is really in.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ENV_FILE="${ENV_FILE:-$PWD/.env}"
[ "${1:-}" != -- ] || shift

for arg in "$@"; do
  if [ "$arg" = --list ]; then
    exec pnpm --filter e2e smoke "$@"
  fi
done

live=0
[ "${KIWI_LIVE_E2E:-}" != 1 ] || live=1
overlay=smoke
[ "$live" = 0 ] || overlay=mockauth

say() { printf 'smoke: %s\n' "$*"; }
die() {
  say "$1" >&2
  exit 1
}

# keyed: whether .env holds a value for each real LLM name (checked by name only; no value
# is read into a variable, printed or compared).
keyed() {
  local name
  for name in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_LLM_MODEL_FLASH KIWI_LLM_MODEL_PRO KIWI_ASR_MODEL; do
    grep -Eq "^[[:space:]]*(export[[:space:]]+)?${name}=[\"']?[^\"'[:space:]]" "$ENV_FILE" 2>/dev/null || return 1
  done
}

# 1. The browsers. Playwright's own download does not run on NixOS: the nix package has the
# chromium revision of the @playwright/test pinned in e2e/package.json. A machine with a
# normal install sets PLAYWRIGHT_BROWSERS_PATH and nothing here changes it.
pnpm install --frozen-lockfile >/dev/null
if [ -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]; then
  say "PLAYWRIGHT_BROWSERS_PATH is not set: building nixpkgs#playwright-driver.browsers (nix build --no-link)"
  PLAYWRIGHT_BROWSERS_PATH="$(nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths)"
  export PLAYWRIGHT_BROWSERS_PATH
  export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
fi
say "Playwright browsers: $PLAYWRIGHT_BROWSERS_PATH"
# The chromium revision @playwright/test wants: its playwright-core is two hops away under pnpm's layout.
revision="$(cd e2e && node -e "
const path = require('path');
const from = (id, dir) => path.dirname(require.resolve(id + '/package.json', { paths: [dir] }));
const core = from('playwright-core', from('playwright', from('@playwright/test', process.cwd())));
console.log(require(path.join(core, 'browsers.json')).browsers.find((b) => b.name === 'chromium').revision);
")"
if [ ! -d "$PLAYWRIGHT_BROWSERS_PATH/chromium-$revision" ] && [ ! -d "$PLAYWRIGHT_BROWSERS_PATH/chromium_headless_shell-$revision" ]; then
  die "the browsers in $PLAYWRIGHT_BROWSERS_PATH have no chromium revision $revision, which the @playwright/test of e2e/package.json needs: use a nixpkgs whose playwright-driver version equals it (nix eval nixpkgs#playwright-driver.version says which this machine has) or unset PLAYWRIGHT_BROWSERS_PATH"
fi

up() { # overlay: its Secret, then the stack
  bash scripts/local/load-env-secret.sh "$1" && tilt ci --timeout 20m -- --overlay "$1"
}

bash scripts/local/k3d-up.sh
bash scripts/local/gen-env.sh
if [ "$live" = 1 ] && ! keyed; then
  die "KIWI_LIVE_E2E=1 needs the real LLM values in .env (KIWI_LLM_BASE_URL, KIWI_LLM_API_KEY, KIWI_LLM_MODEL_FLASH, KIWI_LLM_MODEL_PRO, KIWI_ASR_MODEL)"
fi

# 5, armed before the first change to the cluster: whatever happens, the cluster is put back
# on mockauth when it can be, and the last line says what state it is really in.
touched=0
cluster_on=unknown # the overlay the cluster is known to be on
restore_failed=0
wiped=0
finish() {
  local status=$?
  trap - EXIT
  set +e
  if [ "$touched" = 1 ]; then
    if [ "$overlay" = smoke ]; then
      if keyed; then
        say "returning the cluster to overlay mockauth"
        if up mockauth; then
          cluster_on=mockauth
          # Tilt does not prune: the replay server and its fixtures stay until they are deleted by name.
          kubectl --context k3d-kiwi -n kiwi delete deployment/llmreplay service/llmreplay --ignore-not-found
          for cm in $(kubectl --context k3d-kiwi -n kiwi get configmap -o name | grep '^configmap/llmreplay-fixtures-'); do
            kubectl --context k3d-kiwi -n kiwi delete "$cm" --ignore-not-found
          done
          say "deleted the llmreplay Deployment, Service and fixtures ConfigMap (Tilt does not prune)"
        else
          cluster_on=unknown
          restore_failed=1
          say "could not return the cluster to mockauth (tilt ci failed): it may be half way between smoke and mockauth; fix the cause, then run task local:up"
        fi
      else
        say "the cluster STAYS on overlay smoke: .env has no real LLM values, so mockauth cannot start on this machine"
      fi
    fi
    if [ "$cluster_on" = mockauth ]; then
      say "wiping the workspace data and seeding the default workspace (task seed), so that mockauth is usable at once"
      if ! { bash scripts/smoke/reset.sh && task seed; }; then
        restore_failed=1
        say "the wipe or the seed failed: run task reset, then task seed"
      fi
    fi
  fi
  local code=$status
  [ "$code" -ne 0 ] || code=$restore_failed
  local note=
  [ "$wiped" = 0 ] || note="; its workspace data was WIPED by this run (a real-Google Team workspace and its grant included)"
  if [ "$restore_failed" = 0 ] && [ "$cluster_on" = mockauth ] && [ "$wiped" = 1 ]; then
    note="$note, and it is freshly seeded with the default seed (demo.example: alice, bob); to wipe and seed again: task reset && task seed"
  fi
  say "done (exit $code); the cluster is on overlay $cluster_on$note"
  exit "$code"
}
trap finish EXIT

# 2. The cluster and the overlay.
touched=1
say "bringing up overlay $overlay"
up "$overlay"
cluster_on=$overlay

# 3. The wipe, then the seed (the real services; under smoke every model call is answered by the replay).
say "wiping the workspace data"
bash scripts/smoke/reset.sh
wiped=1
say "seeding demo.example with alice and bob"
task seed -- --domain demo.example --members alice@demo.example,bob@demo.example

# 4. The suite; its status is the script's (see finish).
KIWI_BASE_URL=http://localhost:8080 pnpm --filter e2e smoke "$@"
