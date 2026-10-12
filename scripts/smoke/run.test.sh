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

TREE="$WORK/tree" BIN="$WORK/bin" LOG="$WORK/log" STDIN="$WORK/stdin"
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
  kubectl) case " $* " in *" get configmap "*) printf 'configmap/kiwi-config\nconfigmap/llmreplay-fixtures-abc\n' ;; esac ;;
  docker) case "$1" in images) printf '%s\n' '127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi tilt-new' '127.0.0.1:5001/kiwi-registry.localhost_5001_kiwi tilt-old' 'registry 2' ;; esac ;;
esac
exit 0
EOF
chmod +x "$WORK/generic"
for program in kubectl tilt task pnpm nix node docker; do
  printf '#!/usr/bin/env bash\nSTUB_NAME=%s exec "%s" "$@"\n' "$program" "$WORK/generic" >"$BIN/$program"
  chmod +x "$BIN/$program"
done
for script in scripts/local/k3d-up.sh scripts/local/gen-env.sh scripts/local/load-env-secret.sh scripts/smoke/reset.sh; do
  printf '#!/usr/bin/env bash\nSTUB_NAME=%s exec "%s" "$@"\n' "$(basename "$script")" "$WORK/generic" >"$TREE/$script"
done
cp scripts/smoke/run.sh "$TREE/scripts/smoke/run.sh"

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
last_line() { printf '%s\n' "$OUT" | tail -n 1; }

# run_case ENV_FILE FAIL SCRIPT [args...]: runs the script of the throw-away tree. Sets RC, OUT, LOG.
run_case() {
  local envfile=$1 fail=$2 script=$3
  shift 3
  : >"$LOG"
  : >"$STDIN"
  OUT=$(env -i PATH="$BIN:$PATH" HOME="$WORK" ENV_FILE="$envfile" STUB_LOG="$LOG" STUB_STDIN="$STDIN" STUB_FAIL="$fail" \
    STUB_BROWSERS="$BROWSERS" PLAYWRIGHT_BROWSERS_PATH="${BROWSERS_PATH-$BROWSERS}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} \
    timeout 120 bash "$TREE/scripts/$script" "$@" 2>&1)
  RC=$?
}
EXTRA_ENV=()

SMOKE='tilt ci --timeout 20m -- --overlay smoke'
MOCKAUTH='tilt ci --timeout 20m -- --overlay mockauth'
SUITE='pnpm --filter e2e smoke'
DEL_DEPLOY='delete deployment/llmreplay service/llmreplay'
DEL_CM='delete configmap/llmreplay-fixtures-abc'

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

mkdir "$WORK/no-browsers"
BROWSERS_PATH="$WORK/no-browsers"
run_case "$WORK/env.keyed" "" smoke/run.sh
msg="run.sh: browsers without the headless shell of the pinned revision stop the run before the cluster"
if [ "$RC" -ne 0 ] && ! logged 'tilt' && contains "$OUT" 'chromium_headless_shell-1194'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi
BROWSERS_PATH= # an empty PLAYWRIGHT_BROWSERS_PATH is "not set": the browsers are built with nix
run_case "$WORK/env.keyed" "" smoke/run.sh
unset BROWSERS_PATH
msg="run.sh: with no PLAYWRIGHT_BROWSERS_PATH it builds the browsers with nix"
if [ "$RC" -eq 0 ] && logged 'nix build nixpkgs#playwright-driver.browsers --no-link --print-out-paths'; then ok "$msg"; else bad "$msg" "rc=$RC" "$(cat "$LOG")" "$OUT"; fi

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
if mutant 'kiwi is not scaled back after a failed reset' 's/--replicas=1 >\/dev\/null$/--replicas=0 >\/dev\/null/' reset.real.sh; then
  run_case "" "exec -i deploy/postgres" smoke/reset.real.sh
  if contains "$(grep -n 'scale deploy/kiwi' "$LOG" | tail -n 1)" '--replicas=0'; then ok "mutant caught: a reset that does not scale kiwi back leaves it at 0"; else bad "mutant: the scale-back mutation changed nothing observable"; fi
fi

if [ "$failures" -gt 0 ]; then
  printf '\nrun.test.sh: %d check(s) failed\n' "$failures"
  exit 1
fi
printf '\nrun.test.sh: all checks passed\n'
