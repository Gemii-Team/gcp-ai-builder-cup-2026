#!/usr/bin/env bash
# Usage: scripts/smoke/run.sh [playwright arguments]      (task smoke -- --list)
#        KIWI_SMOKE_PROFILE=minimal scripts/smoke/run.sh   (KIWI_SMOKE_PROFILE=minimal task smoke)
#
# The five flows in headless Chromium against the k3d stack, from a clean state.
#
# Two profiles, chosen by KIWI_SMOKE_PROFILE. Unset or `default` is the full stack: overlay
# smoke for the replay run, mockauth to return to, a web pod and Valkey beside kiwi.
# `minimal` is the one-process stack (kiwi serves the web app, no Valkey, sessions in
# Postgres): overlay minimal-smoke for the replay run, minimal to return to. Everything below
# says "the replay overlay" and "the base overlay" for the two of the profile in use. Any other
# value is refused before anything is touched (exit 2). `task smoke` passes the variable on.
#
# WARNING: this WIPES the workspace data of the cluster (every table of the application
# schemas, and Valkey where the stack has one: see reset.sh), twice: before the run, so that
# it starts from what the seed makes, and after it, so that the overlay the cluster is left on
# is usable at once. A developer's real-Google "Team" workspace and its Meet grant are gone
# after it.
#
#   1. installs the JavaScript dependencies as the lockfile has them (pnpm install
#      --frozen-lockfile) and checks the Playwright browsers (nix, unless
#      PLAYWRIGHT_BROWSERS_PATH is set: they must have the chromium revision of the
#      @playwright/test of e2e/package.json);
#   2. creates the k3d cluster if absent, loads .env into Secret kiwi-env for the replay overlay
#      (it forces dummies for the LLM and the Google client; the base overlay keeps the real LLM
#      values and forces a dummy Google client) and brings the overlay up with `tilt ci`
#      (it leaves every resource deployed);
#   3. wipes the data and seeds demo.example with alice and bob (through the replay);
#   4. runs `pnpm --filter e2e smoke` against http://localhost:8080;
#   5. returns the cluster to the base overlay, deletes what the replay overlay added (Tilt
#      does not prune), wipes the data again and runs the default `task seed` under the base
#      overlay (real LLM and Ollama: the replay's vectors must not stay under the real
#      embedding model's name), then removes the older Tilt-built images the runs leave on
#      the host (the newest of each, and every image a container uses, are kept). The last
#      line says which overlay the cluster is on and what its data is.
#
# `-- --list` only lists the suite: it touches neither the cluster nor the browsers.
#
# KIWI_LIVE_E2E=1 runs the @live tests against the real LiteLLM and Ollama (the base overlay
# throughout) instead of the recorded LLM (the replay overlay); it needs the real LLM
# values in .env. A machine whose .env has none (the keyless machine the replay overlay is
# for) cannot start the base overlay: the replay run then leaves the cluster ON THE REPLAY
# OVERLAY (replay-seeded), says so on its last line, and fails only if the suite failed.
#
# Every step that can wait on kiwi, the database or a model is bounded: the timeouts below,
# tilt's own, reset.sh's. The timeouts are GNU coreutils `timeout` (gtimeout on macOS: see
# bounded.sh), a prerequisite, run with --foreground so that Ctrl-C reaches the command.
# The exit status is that of the suite (or of the first step that failed); a restore that
# failed (back to the base overlay, removing the replay server, wipe or seed) makes it
# non-zero too, and the last line says what state the cluster is really in.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
ENV_FILE="${ENV_FILE:-$PWD/.env}"
[ "${1:-}" != -- ] || shift

# The profile decides which overlays the run uses. It is read before anything else, so that a
# value this script does not know touches nothing. local_up is how the base overlay is started
# by hand, for the messages that tell a person what to run.
profile="${KIWI_SMOKE_PROFILE:-default}"
case "$profile" in
  default)
    base_overlay=mockauth
    replay_overlay=smoke
    local_up="task local:up"
    ;;
  minimal)
    base_overlay=minimal
    replay_overlay=minimal-smoke
    local_up="OVERLAY=minimal task local:up"
    ;;
  *)
    echo "smoke: unknown KIWI_SMOKE_PROFILE '$profile' (want default or minimal); nothing was touched" >&2
    exit 2
    ;;
esac

for arg in "$@"; do
  if [ "$arg" = --list ]; then
    exec pnpm --filter e2e smoke "$@"
  fi
done
# shellcheck source=scripts/smoke/bounded.sh
. scripts/smoke/bounded.sh

live=0
[ "${KIWI_LIVE_E2E:-}" != 1 ] || live=1
overlay=$replay_overlay
[ "$live" = 0 ] || overlay=$base_overlay

kc=(kubectl --context k3d-kiwi -n kiwi)
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

# 1. The dependencies and the browsers. Playwright's own download does not run on NixOS: the
# nix package has the chromium revision of the @playwright/test pinned in e2e/package.json.
# A machine with a normal install sets PLAYWRIGHT_BROWSERS_PATH and nothing here changes it.
pnpm install --frozen-lockfile >/dev/null
if [ -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]; then
  say "PLAYWRIGHT_BROWSERS_PATH is not set: building nixpkgs#playwright-driver.browsers (nix build --no-link)"
  PLAYWRIGHT_BROWSERS_PATH="$(bounded 900 nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths)"
  export PLAYWRIGHT_BROWSERS_PATH
  export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1
fi
say "Playwright browsers: $PLAYWRIGHT_BROWSERS_PATH"
# The revision of the headless shell @playwright/test runs (its playwright-core is two hops
# away under pnpm's layout) must be among the browsers.
revision="$(cd e2e && node -e "
const path = require('path');
const from = (id, dir) => path.dirname(require.resolve(id + '/package.json', { paths: [dir] }));
const core = from('playwright-core', from('playwright', from('@playwright/test', process.cwd())));
console.log(require(path.join(core, 'browsers.json')).browsers.find((b) => b.name === 'chromium-headless-shell').revision);
")"
if [ ! -d "$PLAYWRIGHT_BROWSERS_PATH/chromium_headless_shell-$revision" ]; then
  die "the browsers in $PLAYWRIGHT_BROWSERS_PATH have no chromium_headless_shell-$revision, which the @playwright/test of e2e/package.json runs: use a nixpkgs whose playwright-driver version equals it (nix eval nixpkgs#playwright-driver.version says which this machine has) or unset PLAYWRIGHT_BROWSERS_PATH"
fi

# up OVERLAY: its Secret, then the stack. The cluster counts as touched once the Secret is applied.
touched=0
up() {
  bash scripts/local/load-env-secret.sh "$1" || return
  touched=1
  tilt ci --timeout 20m -- --overlay "$1"
}

# The live run needs the real values: that is known before anything is created.
if [ "$live" = 1 ] && ! keyed; then
  die "KIWI_LIVE_E2E=1 needs the real LLM values in .env (KIWI_LLM_BASE_URL, KIWI_LLM_API_KEY, KIWI_LLM_MODEL_FLASH, KIWI_LLM_MODEL_PRO, KIWI_ASR_MODEL)"
fi
bounded 900 bash scripts/local/k3d-up.sh
bash scripts/local/gen-env.sh

# drop_replay: Tilt does not prune, so the replay server and its fixtures stay until they
# are deleted by name. Prints what could not be deleted and fails when there is any.
drop_replay() {
  local left=() cms cm
  "${kc[@]}" delete deployment/llmreplay service/llmreplay --ignore-not-found --timeout=60s || left+=(deployment/llmreplay service/llmreplay)
  if ! cms="$(bounded 30 "${kc[@]}" get configmap -o name)"; then
    left+=("configmaps (could not be listed: look for llmreplay-fixtures-*)")
  else
    for cm in $(printf '%s\n' "$cms" | grep '^configmap/llmreplay-fixtures-' || true); do
      "${kc[@]}" delete "$cm" --ignore-not-found --timeout=60s || left+=("$cm")
    done
  fi
  if [ "${#left[@]}" -gt 0 ]; then
    say "could not delete: ${left[*]} (delete them by hand with kubectl -n kiwi delete, or task local:up leaves a replay server running)"
    return 1
  fi
  say "deleted the llmreplay Deployment, Service and fixtures ConfigMap (Tilt does not prune)"
}

# drop_images: each run leaves the images Tilt built on the host (a registry copy is what the
# cluster runs). The older Tilt-built images of each repo go; the newest of each is kept (Tilt
# reuses it) and docker refuses to remove one a container uses. Never the registry or the k3d
# images, and no host-wide prune: only repos named kiwi-registry.localhost_5001_*.
drop_images() {
  local removed=0 image
  for image in $(docker images --format '{{.Repository}} {{.Tag}}' 2>/dev/null | grep '/kiwi-registry\.localhost_5001_' | awk '{ if (seen[$1]++) print $1 ":" $2 }'); do
    if docker rmi "$image" >/dev/null 2>&1; then
      removed=$((removed + 1))
    fi
  done
  say "removed $removed older Tilt-built images from the host (the newest of each are kept)"
}

# 5, armed before the first change to the cluster: whatever happens, the cluster is put back
# on the base overlay when it can be, and the last line says what state it is really in.
cluster_on=unknown # the overlay the cluster is known to be on
restore_failed=0
wiped=0 # set before the first wipe, so that a wipe that fails half way is still repaired
reseeded=0
finish() {
  local status=$?
  trap - EXIT
  set +e
  if [ "$touched" = 1 ]; then
    if [ "$overlay" = "$replay_overlay" ]; then
      if [ "$cluster_on" != "$replay_overlay" ]; then
        say "overlay $replay_overlay did not come up (tilt ci failed): what the cluster runs is unknown; fix the cause, then run task smoke again"
      elif keyed; then
        say "returning the cluster to overlay $base_overlay"
        if up "$base_overlay"; then
          cluster_on=$base_overlay
          drop_replay || restore_failed=1
        else
          cluster_on=unknown
          restore_failed=1
          say "could not return the cluster to $base_overlay (secret or tilt ci failed): it may be half way between $replay_overlay and $base_overlay; the replay server is left in place; fix the cause, then run $local_up"
        fi
      else
        say "the cluster STAYS on overlay $replay_overlay: .env has no real LLM values, so $base_overlay cannot start on this machine"
      fi
    fi
    if [ "$cluster_on" = "$base_overlay" ] && [ "$wiped" = 1 ]; then
      say "wiping the workspace data and seeding the default workspace (task seed), so that $base_overlay is usable at once"
      if bounded 700 bash scripts/smoke/reset.sh && bounded 300 task seed; then
        reseeded=1
      else
        restore_failed=1
        say "the wipe or the seed failed: run task reset, then task seed"
      fi
    fi
    drop_images
  fi
  local code=$status note
  [ "$code" -ne 0 ] || code=$restore_failed
  if [ "$touched" = 0 ]; then
    note="the cluster was not touched"
  elif [ "$cluster_on" = unknown ]; then
    note="the overlay the cluster is on is unknown"
  else
    note="the cluster is on overlay $cluster_on"
  fi
  [ "$wiped" = 0 ] || note="$note; its workspace data was WIPED by this run (a real-Google Team workspace and its grant included)"
  if [ "$reseeded" = 1 ]; then
    note="$note, and it is freshly seeded with the default seed (demo.example: alice, bob); to wipe and seed again: task reset && task seed"
  elif [ "$wiped" = 1 ] && [ "$cluster_on" = "$replay_overlay" ]; then
    note="$note; the data is the replay's seed (hashed vectors): run task reset && $local_up and task seed on a machine with the real LLM values"
  fi
  say "done (exit $code); $note"
  exit "$code"
}
trap finish EXIT

# 2. The cluster and the overlay.
say "bringing up overlay $overlay"
up "$overlay"
cluster_on=$overlay

# 3. The wipe, then the seed (the real services; under the replay overlay every model call is answered by the replay).
say "wiping the workspace data"
wiped=1
bounded 700 bash scripts/smoke/reset.sh
say "seeding demo.example with alice and bob"
bounded 300 task seed -- --domain demo.example --members alice@demo.example,bob@demo.example

# 4. The suite; its status is the script's (see finish).
bounded 1800 env KIWI_BASE_URL=http://localhost:8080 pnpm --filter e2e smoke "$@"
