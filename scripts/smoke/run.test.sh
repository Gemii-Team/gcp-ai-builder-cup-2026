#!/usr/bin/env bash
# Tests of scripts/smoke/run.sh and scripts/smoke/reset.sh that need no cluster: the scripts
# run in a throw-away copy of the tree, with every program they call (kubectl, tilt, task,
# pnpm, nix, node, docker and the scripts of scripts/local) replaced by a stub that logs
# its command line and fails when the case says so. What is checked is the order of what
# the scripts do, what they refuse to do, their exit status and the last line they print.
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

TREE="$WORK/tree" BIN="$WORK/bin" LOG="$WORK/log" STDIN="$WORK/stdin" TLOG="$WORK/tlog"
# The real timeout runs the harness's own cases; the scripts find a stub of that name that logs
# its arguments first.
REAL_TIMEOUT="$(command -v timeout || command -v gtimeout)" || { echo "run.test.sh: needs GNU coreutils timeout (gtimeout on macOS)"; exit 2; }
BROWSERS="$WORK/browsers"
mkdir -p "$TREE/scripts/smoke" "$TREE/scripts/local" "$TREE/e2e" "$BIN" "$BROWSERS/chromium_headless_shell-1194"

# One stub for every program: it logs "name args", fails when STUB_FAIL (patterns separated
# by |; one that starts with = must be the whole line) matches that line, and answers the
# few questions the scripts ask.
cat >"$WORK/generic" <<'EOF'
#!/usr/bin/env bash
name=${STUB_NAME:-$(basename "$0")}
line="$name $*"
printf '%s\n' "$line" >>"$STUB_LOG"
case " $* " in *" exec -i "*) cat >>"$STUB_STDIN" ;; esac
# The suite call: the base URL it was given.
case "$line" in "pnpm --filter e2e smoke"*) printf '%s\n' "${KIWI_BASE_URL-unset}" >>"$STUB_LOG.baseurl" ;; esac
IFS='|'
for pattern in $STUB_FAIL; do
  case "$pattern" in
    =*) [ "$line" = "${pattern#=}" ] && exit 1 ;;
    *) case "$line" in *"$pattern"*) exit 1 ;; esac ;;
  esac
done
unset IFS
case "$name" in
  node) echo 1194 ;;
  nix) echo "$STUB_BROWSERS" ;;
  kubectl)
    case " $* " in
      *" get configmap "*) printf 'configmap/kiwi-config\nconfigmap/llmreplay-fixtures-abc\n' ;;
      # The question reset.sh asks first: the full stack has a Valkey Deployment, the minimal one does not.
      *" get deploy/valkey "*) [ "${STUB_NO_VALKEY:-}" = 1 ] || echo deployment.apps/valkey ;;
    esac
    ;;
  docker) case "$1" in images) printf '%s\n' '127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi tilt-new' '127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi tilt-old' 'registry 2' ;; esac ;;
esac
exit 0
EOF
chmod +x "$WORK/generic"
for program in kubectl tilt task pnpm nix node docker; do
  printf '#!/usr/bin/env bash\nSTUB_NAME=%s exec "%s" "$@"\n' "$program" "$WORK/generic" >"$BIN/$program"
  chmod +x "$BIN/$program"
done
cat >"$BIN/timeout" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_TLOG"
exec "$REAL_TIMEOUT" "$@"
EOF
chmod +x "$BIN/timeout"
for script in scripts/local/k3d-up.sh scripts/local/gen-env.sh scripts/local/load-env-secret.sh scripts/smoke/reset.sh; do
  printf '#!/usr/bin/env bash\nSTUB_NAME=%s exec "%s" "$@"\n' "$(basename "$script")" "$WORK/generic" >"$TREE/$script"
done
cp scripts/smoke/run.sh "$TREE/scripts/smoke/run.sh"
cp scripts/smoke/bounded.sh "$TREE/scripts/smoke/bounded.sh"

# Fixture .env files: the names the script looks for, with made-up values, or none.
{
  for name in KIWI_LLM_BASE_URL KIWI_LLM_API_KEY KIWI_LLM_MODEL_FLASH KIWI_LLM_MODEL_PRO KIWI_ASR_MODEL; do
    printf '%s=fixture-%s\n' "$name" "$name"
  done
} >"$WORK/env.keyed"
printf 'KIWI_LLM_BASE_URL=\nKIWI_LLM_API_KEY=\nKIWI_DB_PASSWORD=x\n' >"$WORK/env.keyless"

contains() { case $1 in *"$2"*) return 0 ;; *) return 1 ;; esac; }
logged() { grep -qF -- "$1" "$LOG"; }
lineno() { grep -nF -- "$1" "$LOG" | head -n 1 | cut -d: -f1; }
# before A B: both were logged, A first.
before() {
  local a b
  a=$(lineno "$1")
  b=$(lineno "$2")
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
# The same for whole lines: the overlay names minimal and minimal-smoke are prefixes of each other.
logged_x() { grep -qxF -- "$1" "$LOG"; }
lineno_x() { grep -nxF -- "$1" "$LOG" | head -n 1 | cut -d: -f1; }
before_x() {
  local a b
  a=$(lineno_x "$1")
  b=$(lineno_x "$2")
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
last_line() { printf '%s\n' "$OUT" | tail -n 1; }

# Every call of timeout the script made went through `bounded`: --foreground, then the seconds.
all_foreground() { [ -s "$TLOG" ] && ! grep -qvE '^--foreground [0-9]+ ' "$TLOG"; }
timed() { grep -qxF -- "--foreground $1" "$TLOG"; } # timed '900 bash scripts/local/k3d-up.sh'

# run_case ENV_FILE FAIL SCRIPT [args...]: runs the script of the throw-away tree. Sets RC, OUT, LOG.
run_case() {
  local envfile=$1 fail=$2 script=$3
  shift 3
  : >"$LOG"
  : >"$STDIN"
  : >"$TLOG"
  : >"$LOG.baseurl"
  OUT=$(env -i PATH="$BIN:$PATH" HOME="$WORK" ENV_FILE="$envfile" STUB_LOG="$LOG" STUB_STDIN="$STDIN" STUB_TLOG="$TLOG" REAL_TIMEOUT="$REAL_TIMEOUT" STUB_FAIL="$fail" \
    STUB_BROWSERS="$BROWSERS" PLAYWRIGHT_BROWSERS_PATH="${BROWSERS_PATH-$BROWSERS}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
    "$REAL_TIMEOUT" 120 bash "$TREE/scripts/$script" "$@" 2>&1)
  RC=$?
}
EXTRA_ENV=()

SMOKE='tilt ci --timeout 20m -- --overlay smoke'
MOCKAUTH='tilt ci --timeout 20m -- --overlay mockauth'
SUITE='pnpm --filter e2e smoke'
DEL_DEPLOY='delete deployment/llmreplay service/llmreplay'
DEL_CM='delete configmap/llmreplay-fixtures-abc'
MINIMAL='tilt ci --timeout 20m -- --overlay minimal'
MINSMOKE='tilt ci --timeout 20m -- --overlay minimal-smoke'
KDEL_DEPLOY="kubectl --context k3d-kiwi -n kiwi $DEL_DEPLOY --ignore-not-found --timeout=60s"
KDEL_CM="kubectl --context k3d-kiwi -n kiwi $DEL_CM --ignore-not-found --timeout=60s"

# --- a replay run on a keyed machine ------------------------------------------------------
run_case "$WORK/env.keyed" "" smoke/run.sh
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
before 'pnpm install --frozen-lockfile' "$SMOKE" || verdict=bad
before "$SMOKE" 'reset.sh' || verdict=bad
before 'reset.sh' 'task seed -- --domain demo.example --members alice@demo.example,bob@demo.example' || verdict=bad
before 'task seed -- --domain demo.example' "$SUITE" || verdict=bad
before "$SUITE" "$MOCKAUTH" || verdict=bad
before "$MOCKAUTH" "$DEL_DEPLOY" || verdict=bad
before "$DEL_DEPLOY" "$DEL_CM" || verdict=bad
logged 'kubectl --context k3d-kiwi -n kiwi get configmap -o name' || verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 2 ] || verdict=bad
[ "$(grep -c '^task seed' "$LOG")" -eq 2 ] || verdict=bad
grep -qxF 'task seed' "$LOG" || verdict=bad
contains "$(last_line)" 'the cluster is on overlay mockauth' || verdict=bad
contains "$(last_line)" 'freshly seeded with the default seed' || verdict=bad
contains "$(last_line)" 'WIPED' || verdict=bad
logged 'docker rmi 127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi:tilt-old' || verdict=bad
logged 'docker rmi 127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi:tilt-new' && verdict=bad
logged 'docker rmi registry' && verdict=bad
logged 'docker system prune' && verdict=bad
# Nothing of the host beyond the Tilt-built images is touched: no host-wide prune of dangling images.
logged 'docker image prune' && verdict=bad
# Ctrl-C must reach the command a timeout waits for: every bounded call is --foreground.
all_foreground || verdict=bad
timed '900 bash scripts/local/k3d-up.sh' || verdict=bad
timed '700 bash scripts/smoke/reset.sh' || verdict=bad
timed '300 task seed -- --domain demo.example --members alice@demo.example,bob@demo.example' || verdict=bad
timed '300 task seed' || verdict=bad
timed '30 kubectl --context k3d-kiwi -n kiwi get configmap -o name' || verdict=bad
timed '1800 env KIWI_BASE_URL=http://localhost:8080 pnpm --filter e2e smoke' || verdict=bad
[ "$(cat "$LOG.baseurl")" = "http://localhost:8080" ] || verdict=bad
ok_msg="run.sh (keyed, replay): install, smoke up, wipe, seed, suite, back to mockauth, replay objects deleted, wipe and default seed, older Tilt images only"
if [ "$verdict" = ok ]; then ok "$ok_msg"; else bad "$ok_msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# --- the suite fails: its status is kept, the cluster is still put back --------------------
run_case "$WORK/env.keyed" "$SUITE" smoke/run.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged "$MOCKAUTH" || verdict=bad
logged "$DEL_DEPLOY" || verdict=bad
contains "$(last_line)" 'done (exit 1)' || verdict=bad
contains "$(last_line)" 'overlay mockauth' || verdict=bad
msg="run.sh: a failing suite exits non-zero and the cluster still goes back to mockauth"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(last_line)"; fi

# --- no real LLM values: mockauth cannot start, the cluster stays on smoke ------------------
run_case "$WORK/env.keyless" "" smoke/run.sh
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
logged "$MOCKAUTH" && verdict=bad
logged 'load-env-secret.sh mockauth' && verdict=bad
logged 'kubectl --context k3d-kiwi -n kiwi delete' && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 1 ] || verdict=bad
[ "$(grep -c '^task seed' "$LOG")" -eq 1 ] || verdict=bad
contains "$OUT" 'STAYS on overlay smoke' || verdict=bad
contains "$(last_line)" 'the cluster is on overlay smoke' || verdict=bad
contains "$(last_line)" 'freshly seeded' && verdict=bad
msg="run.sh (keyless): the cluster stays on smoke and says so, nothing is deleted, no second wipe, exit 0"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

run_case "$WORK/env.keyless" "$SUITE" smoke/run.sh
msg="run.sh (keyless): a failing suite is still non-zero"
if [ "$RC" -ne 0 ] && contains "$(last_line)" 'overlay smoke'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(last_line)"; fi

# --- the overlay itself does not come up ---------------------------------------------------
run_case "$WORK/env.keyless" "$SMOKE" smoke/run.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" 'STAYS on overlay smoke' && verdict=bad
contains "$OUT" 'did not come up' || verdict=bad
logged 'reset.sh' && verdict=bad
contains "$(last_line)" 'unknown' || verdict=bad
msg="run.sh: a failed tilt ci of smoke is never reported as staying on smoke, and nothing is wiped"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

# --- the way back to mockauth fails --------------------------------------------------------
run_case "$WORK/env.keyed" "$MOCKAUTH" smoke/run.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged "$DEL_DEPLOY" && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 1 ] || verdict=bad
contains "$OUT" 'could not return the cluster to mockauth' || verdict=bad
contains "$(last_line)" 'unknown' || verdict=bad
msg="run.sh: a failed return to mockauth is non-zero even when the suite passed, keeps the replay server and says the state is unknown"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# --- removing the replay server fails ------------------------------------------------------
run_case "$WORK/env.keyed" "$DEL_DEPLOY" smoke/run.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" 'could not delete: deployment/llmreplay service/llmreplay' || verdict=bad
contains "$OUT" 'deleted the llmreplay' && verdict=bad
msg="run.sh: a failed delete of the replay server is reported, not claimed, and is non-zero"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

run_case "$WORK/env.keyed" "$DEL_CM" smoke/run.sh
msg="run.sh: a failed delete of the fixtures ConfigMap is reported and non-zero"
if [ "$RC" -ne 0 ] && contains "$OUT" 'could not delete: configmap/llmreplay-fixtures-abc'; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

# --- the wipe fails ------------------------------------------------------------------------
run_case "$WORK/env.keyed" "reset.sh" smoke/run.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged 'task seed' && verdict=bad
logged "$SUITE" && verdict=bad
contains "$OUT" 'the wipe or the seed failed' || verdict=bad
contains "$(last_line)" 'freshly seeded' && verdict=bad
msg="run.sh: a failing wipe stops the run before the seed and the suite, and the end-of-run repair says it failed"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

run_case "$WORK/env.keyed" "=task seed" smoke/run.sh
msg="run.sh: a failing default seed after the run is non-zero and not reported as seeded"
if [ "$RC" -ne 0 ] && ! contains "$(last_line)" 'freshly seeded'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(last_line)"; fi

# --- listing, live without keys, the browsers ----------------------------------------------
run_case "$WORK/env.keyed" "" smoke/run.sh -- --list
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
[ "$(cat "$LOG")" = "pnpm --filter e2e smoke --list" ] || verdict=bad
msg="run.sh -- --list only lists: it calls nothing but pnpm"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")"; fi

EXTRA_ENV=(KIWI_LIVE_E2E=1)
run_case "$WORK/env.keyless" "" smoke/run.sh
EXTRA_ENV=()
msg="run.sh: KIWI_LIVE_E2E=1 without the real LLM values stops before touching the cluster"
if [ "$RC" -ne 0 ] && ! logged 'tilt' && ! logged 'kubectl' && contains "$OUT" 'needs the real LLM values'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")"; fi

EXTRA_ENV=(KIWI_LIVE_E2E=1)
run_case "$WORK/env.keyed" "" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
logged "$MOCKAUTH" || verdict=bad
logged "$SMOKE" && verdict=bad
logged 'delete deployment/llmreplay' && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 2 ] || verdict=bad
contains "$(last_line)" 'freshly seeded' || verdict=bad
msg="run.sh (live): overlay mockauth throughout, no replay objects to delete, wipe and default seed at the end"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# --- the minimal profile: KIWI_SMOKE_PROFILE=minimal ---------------------------------------------
# The replay run uses overlay minimal-smoke and the cluster goes back to overlay minimal, never to
# smoke or mockauth. minimal is a prefix of minimal-smoke, so these cases compare whole lines.
EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
run_case "$WORK/env.keyed" "" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
before 'pnpm install --frozen-lockfile' 'load-env-secret.sh minimal-smoke' || verdict=bad
before_x 'load-env-secret.sh minimal-smoke' "$MINSMOKE" || verdict=bad
# The stub logs "name args", so a script called with no arguments leaves "reset.sh " (with a space).
before_x "$MINSMOKE" 'reset.sh ' || verdict=bad
before_x 'reset.sh ' 'task seed -- --domain demo.example --members alice@demo.example,bob@demo.example' || verdict=bad
before_x 'task seed -- --domain demo.example --members alice@demo.example,bob@demo.example' "$SUITE" || verdict=bad
before_x "$SUITE" 'load-env-secret.sh minimal' || verdict=bad
before_x 'load-env-secret.sh minimal' "$MINIMAL" || verdict=bad
before_x "$MINIMAL" "$KDEL_DEPLOY" || verdict=bad
before_x "$KDEL_DEPLOY" "$KDEL_CM" || verdict=bad
# Never the overlays of the default profile.
for line in "$SMOKE" "$MOCKAUTH" 'load-env-secret.sh smoke' 'load-env-secret.sh mockauth'; do
  logged_x "$line" && { verdict=bad; printf 'minimal profile ran: %s\n' "$line"; }
done
[ "$(grep -c '^reset.sh' "$LOG")" -eq 2 ] || verdict=bad
[ "$(grep -c '^task seed' "$LOG")" -eq 2 ] || verdict=bad
grep -qxF 'task seed' "$LOG" || verdict=bad
contains "$(last_line)" 'the cluster is on overlay minimal;' || verdict=bad
contains "$(last_line)" 'freshly seeded with the default seed' || verdict=bad
all_foreground || verdict=bad
msg="run.sh (minimal profile, keyed, replay): minimal-smoke up, wipe, seed, suite, back to minimal, replay objects deleted, wipe and default seed; never smoke or mockauth"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
run_case "$WORK/env.keyed" "$SUITE" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged_x "$MINIMAL" || verdict=bad
logged "$DEL_DEPLOY" || verdict=bad
contains "$(last_line)" 'done (exit 1)' || verdict=bad
contains "$(last_line)" 'overlay minimal;' || verdict=bad
msg="run.sh (minimal profile): a failing suite exits non-zero and the cluster still goes back to minimal"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(last_line)"; fi

EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
run_case "$WORK/env.keyless" "" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
logged_x "$MINIMAL" && verdict=bad
logged_x 'load-env-secret.sh minimal' && verdict=bad
logged 'kubectl --context k3d-kiwi -n kiwi delete' && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 1 ] || verdict=bad
contains "$OUT" 'STAYS on overlay minimal-smoke' || verdict=bad
contains "$OUT" 'so minimal cannot start on this machine' || verdict=bad
contains "$(last_line)" 'the cluster is on overlay minimal-smoke' || verdict=bad
contains "$(last_line)" 'freshly seeded' && verdict=bad
msg="run.sh (minimal profile, keyless): the cluster stays on minimal-smoke and says so, nothing is deleted, no second wipe, exit 0"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
run_case "$WORK/env.keyless" "=$MINSMOKE" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" 'overlay minimal-smoke did not come up' || verdict=bad
contains "$OUT" 'STAYS on overlay' && verdict=bad
logged 'reset.sh' && verdict=bad
contains "$(last_line)" 'unknown' || verdict=bad
msg="run.sh (minimal profile): a failed tilt ci of minimal-smoke is never reported as staying on it, and nothing is wiped"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
run_case "$WORK/env.keyed" "=$MINIMAL" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged "$DEL_DEPLOY" && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 1 ] || verdict=bad
contains "$OUT" 'could not return the cluster to minimal' || verdict=bad
contains "$OUT" 'half way between minimal-smoke and minimal' || verdict=bad
contains "$OUT" 'run OVERLAY=minimal task local:up' || verdict=bad
contains "$(last_line)" 'unknown' || verdict=bad
msg="run.sh (minimal profile): a failed return to minimal is non-zero, keeps the replay server, names the minimal way to start it and says the state is unknown"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal KIWI_LIVE_E2E=1)
run_case "$WORK/env.keyed" "" smoke/run.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
logged_x "$MINIMAL" || verdict=bad
logged 'minimal-smoke' && verdict=bad
logged_x "$SMOKE" && verdict=bad
logged_x "$MOCKAUTH" && verdict=bad
logged 'delete deployment/llmreplay' && verdict=bad
[ "$(grep -c '^reset.sh' "$LOG")" -eq 2 ] || verdict=bad
contains "$(last_line)" 'the cluster is on overlay minimal;' || verdict=bad
contains "$(last_line)" 'freshly seeded' || verdict=bad
msg="run.sh (minimal profile, live): overlay minimal throughout, no replay objects to delete, wipe and default seed at the end"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# An unknown profile is refused before anything is touched: nothing is called, not even
# pnpm for `-- --list`, and the exit status is 2.
for profile in bogus Minimal full smoke mockauth minimal-smoke; do
  for args in "" "-- --list"; do
    EXTRA_ENV=(KIWI_SMOKE_PROFILE="$profile")
    # shellcheck disable=SC2086
    run_case "$WORK/env.keyed" "" smoke/run.sh $args
    EXTRA_ENV=()
    verdict=ok
    [ "$RC" -eq 2 ] || verdict=bad
    [ ! -s "$LOG" ] || verdict=bad
    contains "$OUT" "unknown KIWI_SMOKE_PROFILE '$profile'" || verdict=bad
    contains "$OUT" 'want default or minimal' || verdict=bad
    contains "$OUT" 'nothing was touched' || verdict=bad
    msg="run.sh: KIWI_SMOKE_PROFILE='$profile'${args:+ with $args} is refused with exit 2 before any program runs"
    if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi
  done
done

# `default`, spelled out or empty, is the unset profile: today's overlays.
for profile in default ""; do
  EXTRA_ENV=(KIWI_SMOKE_PROFILE="$profile")
  run_case "$WORK/env.keyed" "" smoke/run.sh
  EXTRA_ENV=()
  verdict=ok
  [ "$RC" -eq 0 ] || verdict=bad
  logged_x "$SMOKE" || verdict=bad
  logged_x "$MOCKAUTH" || verdict=bad
  logged 'minimal' && verdict=bad
  contains "$(last_line)" 'the cluster is on overlay mockauth' || verdict=bad
  msg="run.sh: KIWI_SMOKE_PROFILE='$profile' runs the default profile (smoke, back to mockauth)"
  if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi
done

mkdir "$WORK/no-browsers"
BROWSERS_PATH="$WORK/no-browsers"
run_case "$WORK/env.keyed" "" smoke/run.sh
msg="run.sh: browsers without the headless shell of the pinned revision stop the run before the cluster"
if [ "$RC" -ne 0 ] && ! logged 'tilt' && contains "$OUT" 'chromium_headless_shell-1194'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi
BROWSERS_PATH= # an empty PLAYWRIGHT_BROWSERS_PATH is "not set": the browsers are built with nix
run_case "$WORK/env.keyed" "" smoke/run.sh
unset BROWSERS_PATH
msg="run.sh: with no PLAYWRIGHT_BROWSERS_PATH it builds the browsers with nix, bounded and --foreground"
if [ "$RC" -eq 0 ] && logged 'nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths' && all_foreground && timed '900 nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$(cat "$TLOG")" "$OUT"; fi

# --- reset.sh --------------------------------------------------------------------------------
cp scripts/smoke/reset.sh "$TREE/scripts/smoke/reset.real.sh"
KC='kubectl --context k3d-kiwi -n kiwi'
run_case "" "" smoke/reset.real.sh
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
before "$KC scale deploy/kiwi --replicas=0" "$KC wait --for=delete pod -l app.kubernetes.io/name=kiwi --timeout=120s" || verdict=bad
before "$KC wait --for=delete" "$KC exec -i deploy/postgres -- psql" || verdict=bad
before "$KC exec -i deploy/postgres" "$KC exec deploy/valkey -- valkey-cli FLUSHALL" || verdict=bad
before "FLUSHALL" "$KC scale deploy/kiwi --replicas=1" || verdict=bad
before "$KC scale deploy/kiwi --replicas=1" "$KC rollout status deploy/kiwi --timeout=180s" || verdict=bad
[ "$(head -n 1 "$STDIN")" = "SET lock_timeout = '30s';" ] || verdict=bad
grep -q 'TRUNCATE' "$STDIN" || verdict=bad
msg="reset.sh: kiwi down and gone first, then the TRUNCATE with a lock timeout, then Valkey, then kiwi up"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$(cat "$STDIN")"; fi

for failing in "wait --for=delete" "exec -i deploy/postgres" "valkey-cli" "rollout status"; do
  run_case "" "$failing" smoke/reset.real.sh
  verdict=ok
  [ "$RC" -ne 0 ] || verdict=bad
  last_scale=$(grep -n 'scale deploy/kiwi' "$LOG" | tail -n 1)
  contains "$last_scale" '--replicas=1' || verdict=bad
  msg="reset.sh: when '$failing' fails kiwi is scaled back to 1 and the exit status is non-zero"
  if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")"; fi
done
run_case "" "exec -i deploy/postgres" smoke/reset.real.sh
msg="reset.sh: a failing TRUNCATE flushes nothing"
if ! logged 'FLUSHALL'; then ok "$msg"; else bad "$msg" "$(cat "$LOG")"; fi

run_case "" "" smoke/reset.real.sh
verdict=ok
all_foreground || verdict=bad
timed '150 kubectl --context k3d-kiwi -n kiwi exec -i deploy/postgres -- psql -U postgres -d kiwi -v ON_ERROR_STOP=1 -q' || verdict=bad
timed '60 kubectl --context k3d-kiwi -n kiwi exec deploy/valkey -- valkey-cli FLUSHALL' || verdict=bad
msg="reset.sh: its bounded calls are --foreground, so Ctrl-C reaches kubectl"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "$(cat "$TLOG")"; fi

# The scale-back is what keeps a failed reset from leaving the cluster with no kiwi: when it
# fails too, the script says so and how to do it by hand.
SCALE_UP='kubectl --context k3d-kiwi -n kiwi scale deploy/kiwi --replicas=1'
run_case "" "valkey-cli|=$SCALE_UP" smoke/reset.real.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" "reset: COULD NOT scale kiwi back to 1: run $SCALE_UP" || verdict=bad
msg="reset.sh: a failed scale-back is announced with the command that does it by hand, and the exit is non-zero"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

run_case "" "valkey-cli" smoke/reset.real.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
contains "$OUT" "scaling kiwi back to 1" || verdict=bad
contains "$OUT" "COULD NOT" && verdict=bad
msg="reset.sh: a scale-back that works is not announced as failed"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

# --- reset.sh and Valkey: flushed where the cluster has one, skipped where it has not ---------
# The full stack has a Valkey Deployment and the minimal stack has none (its in-process state goes
# with the kiwi restart, its sessions are in the identity schema the TRUNCATE covers).
GET_VALKEY="$KC get deploy/valkey --ignore-not-found -o name"
run_case "" "" smoke/reset.real.sh
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
before "$GET_VALKEY" "$KC scale deploy/kiwi --replicas=0" || verdict=bad
before "$KC exec -i deploy/postgres" "$KC exec deploy/valkey -- valkey-cli FLUSHALL" || verdict=bad
timed "30 $GET_VALKEY" || verdict=bad
all_foreground || verdict=bad
contains "$OUT" 'no Valkey' && verdict=bad
msg="reset.sh (full stack): asks first whether Valkey exists, before kiwi is touched, and flushes it after the TRUNCATE"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

EXTRA_ENV=(STUB_NO_VALKEY=1)
run_case "" "" smoke/reset.real.sh
EXTRA_ENV=()
verdict=ok
[ "$RC" -eq 0 ] || verdict=bad
logged 'valkey-cli' && verdict=bad
logged 'exec deploy/valkey' && verdict=bad
before "$KC scale deploy/kiwi --replicas=0" "$KC exec -i deploy/postgres" || verdict=bad
before "$KC exec -i deploy/postgres" "$KC scale deploy/kiwi --replicas=1" || verdict=bad
before "$KC scale deploy/kiwi --replicas=1" "$KC rollout status deploy/kiwi --timeout=180s" || verdict=bad
grep -q 'TRUNCATE' "$STDIN" || verdict=bad
contains "$OUT" 'no Valkey in this cluster' || verdict=bad
msg="reset.sh (minimal stack, no Valkey): wipes the tables, flushes nothing, says so, and kiwi is back up"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# A cluster that cannot say whether it has Valkey is not wiped half way: nothing is changed and
# nothing is flushed, and the exit status is non-zero (an absent Deployment is an empty answer, any
# other failure of the question is a failure).
run_case "" "get deploy/valkey" smoke/reset.real.sh
verdict=ok
[ "$RC" -ne 0 ] || verdict=bad
logged 'replicas=0' && verdict=bad
logged 'exec -i deploy/postgres' && verdict=bad
logged 'FLUSHALL' && verdict=bad
[ ! -s "$STDIN" ] || verdict=bad
contains "$(grep -n 'scale deploy/kiwi' "$LOG" | tail -n 1)" '--replicas=1' || verdict=bad
msg="reset.sh: a failing Valkey lookup stops the reset before anything is truncated or flushed, with kiwi left at 1"
if [ "$verdict" = ok ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

# --- bounded.sh: GNU timeout, or gtimeout on macOS, always with --foreground ----------------
for dir in only-timeout only-gtimeout both none; do mkdir -p "$WORK/p-$dir"; done
cat >"$WORK/tlogstub" <<'EOF'
#!@BASH@
printf '%s %s\n' "${0##*/}" "$*" >>"$STUB_TLOG"
EOF
for pair in only-timeout:timeout only-gtimeout:gtimeout both:timeout both:gtimeout; do
  sed "s|@BASH@|$BASH|" "$WORK/tlogstub" >"$WORK/p-${pair%%:*}/${pair##*:}"
  chmod +x "$WORK/p-${pair%%:*}/${pair##*:}"
done
lib_run() { # dir: sources bounded.sh with a PATH of that directory only and runs `bounded 5 true`
  : >"$TLOG"
  OUT=$(env -i PATH="$WORK/p-$1" STUB_TLOG="$TLOG" "$BASH" -c '. scripts/smoke/bounded.sh && bounded 5 true' 2>&1)
  RC=$?
}
lib_run only-timeout
msg="bounded.sh: runs GNU timeout with --foreground"
if [ "$RC" -eq 0 ] && [ "$(cat "$TLOG")" = "timeout --foreground 5 true" ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT" "$(cat "$TLOG")"; fi
lib_run only-gtimeout
msg="bounded.sh: falls back to gtimeout (coreutils on macOS), still with --foreground"
if [ "$RC" -eq 0 ] && [ "$(cat "$TLOG")" = "gtimeout --foreground 5 true" ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT" "$(cat "$TLOG")"; fi
lib_run both
msg="bounded.sh: prefers timeout when both exist"
if [ "$RC" -eq 0 ] && [ "$(cat "$TLOG")" = "timeout --foreground 5 true" ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT" "$(cat "$TLOG")"; fi
lib_run none
msg="bounded.sh: without timeout or gtimeout it stops with exit 2 and names GNU coreutils"
if [ "$RC" -eq 2 ] && contains "$OUT" "GNU coreutils" && [ ! -s "$TLOG" ]; then ok "$msg"; else bad "$msg" "rc=$RC" "$OUT"; fi

# --- mutants: the tests above notice when a guard is taken out --------------------------------
mutant() { # name sed-script file-in-tree check-description
  local name=$1 script=$2 file=$3
  cp "$ROOT/scripts/smoke/$(basename "$file" | sed 's/\.real//')" "$TREE/scripts/smoke/$file"
  sed -i "$script" "$TREE/scripts/smoke/$file"
  cmp -s "$ROOT/scripts/smoke/$(basename "$file" | sed 's/\.real//')" "$TREE/scripts/smoke/$file" && { bad "mutant $name: the sed script changed nothing"; return 1; }
  return 0
}
if mutant 'a failed return to mockauth is ignored' 's/^          restore_failed=1$/          :/' run.sh; then
  run_case "$WORK/env.keyed" "$MOCKAUTH" smoke/run.sh
  if [ "$RC" -eq 0 ]; then ok "mutant caught: ignoring a failed return to mockauth makes the run exit 0 (the test above fails on it)"; else bad "mutant: removing restore_failed after a failed return still exits non-zero" "$(last_line)"; fi
fi
if mutant 'kiwi is not scaled back after a failed reset' 's/--replicas=1 >\/dev\/null/--replicas=0 >\/dev\/null/' reset.real.sh; then
  run_case "" "exec -i deploy/postgres" smoke/reset.real.sh
  if contains "$(grep -n 'scale deploy/kiwi' "$LOG" | tail -n 1)" '--replicas=0'; then ok "mutant caught: a reset that does not scale kiwi back leaves it at 0"; else bad "mutant: the scale-back mutation changed nothing observable"; fi
fi
if mutant 'the host-wide prune is back' 's/^  local removed=0 image$/&\n  docker image prune -f >\/dev\/null 2>\&1 || true/' run.sh; then
  run_case "$WORK/env.keyed" "" smoke/run.sh
  if logged 'docker image prune'; then ok "mutant caught: a run that prunes host-wide logs docker image prune (the keyed-run test fails on it)"; else bad "mutant: the prune mutation changed nothing observable"; fi
fi
if mutant 'bounded drops --foreground' 's/ --foreground//' bounded.sh; then
  run_case "$WORK/env.keyed" "" smoke/run.sh
  if ! all_foreground; then ok "mutant caught: a bounded call without --foreground is seen (the keyed-run test fails on it)"; else bad "mutant: dropping --foreground changed nothing observable" "$(cat "$TLOG")"; fi
fi

if mutant 'reset.sh flushes Valkey whether or not the cluster has one' 's/^if \[ -n "\$valkey" \]; then$/if true; then/' reset.real.sh; then
  EXTRA_ENV=(STUB_NO_VALKEY=1)
  run_case "" "" smoke/reset.real.sh
  EXTRA_ENV=()
  if logged 'valkey-cli FLUSHALL'; then ok "mutant caught: a reset that flushes unconditionally calls valkey-cli on a cluster without Valkey (the minimal-stack test fails on it)"; else bad "mutant: the unconditional flush changed nothing observable" "$(cat "$LOG")"; fi
fi
if mutant 'the minimal profile returns to mockauth' 's/^    base_overlay=minimal$/    base_overlay=mockauth/' run.sh; then
  EXTRA_ENV=(KIWI_SMOKE_PROFILE=minimal)
  run_case "$WORK/env.keyed" "" smoke/run.sh
  EXTRA_ENV=()
  if logged_x "$MOCKAUTH"; then ok "mutant caught: a minimal profile that returns to mockauth logs overlay mockauth (the minimal-profile test fails on it)"; else bad "mutant: the base overlay mutation changed nothing observable" "$(cat "$LOG")"; fi
fi
if mutant 'an unknown profile is accepted' 's/^    exit 2$/    :/' run.sh; then
  EXTRA_ENV=(KIWI_SMOKE_PROFILE=bogus)
  run_case "$WORK/env.keyed" "" smoke/run.sh
  EXTRA_ENV=()
  if [ "$RC" -ne 2 ]; then ok "mutant caught: a run that does not refuse an unknown profile does not exit 2 (the refusal tests fail on it)"; else bad "mutant: accepting an unknown profile still exits 2"; fi
fi

if [ "$failures" -gt 0 ]; then
  printf '\nrun.test.sh: %d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nrun.test.sh: all checks passed\n'
