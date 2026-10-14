#!/usr/bin/env bash
# Tests for README.md, the one document that is pushed to the public repository:
# its commands must be real, and no key, private endpoint or real address may be in it.
#
# README, TASKFILE and ENV_FILE override the files read (default: the repository's).
# The checks that need the git-ignored .env (private endpoint hostnames, secret
# values) are skipped, loudly, when it is absent, as in CI. Nothing read from .env
# is ever printed: a finding names README lines, never the text that matched.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
README="${README:-$ROOT/README.md}"
TASKFILE="${TASKFILE:-$ROOT/Taskfile.yml}"
ENV_FILE="${ENV_FILE:-$ROOT/.env}"

umask 077
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

failures=0
checks=0

report() { # name ok|no detail
  checks=$((checks + 1))
  if [ "$2" = ok ]; then
    echo "PASS $1"
  else
    echo "FAIL $1: $3"
    failures=$((failures + 1))
  fi
}

# lines_of: the line numbers of the "N:text" lines on stdin, as "N, M".
lines_of() { cut -d: -f1 | paste -sd, - | sed 's/,/, /g'; }

if [ ! -f "$README" ]; then
  echo "FAIL README exists: $README not found"
  exit 1
fi

# task_names prints "LINE NAME" for every `task NAME` command in the README: one per
# command of a code fence line, and one per inline code span outside the fences. A
# command may follow VAR=value words (OVERLAY=local task local:up) and ; && | separators.
task_names() {
  awk '
    function commands(text, line,    n, parts, i, seg, name) {
      n = split(text, parts, /&&|\|\||[;|]/)
      for (i = 1; i <= n; i++) {
        seg = parts[i]
        sub(/^[ \t]+/, "", seg)
        sub(/^\$ +/, "", seg)
        while (match(seg, /^[A-Za-z_][A-Za-z0-9_]*=[^ \t]*[ \t]+/)) seg = substr(seg, RLENGTH + 1)
        if (match(seg, /^task[ \t]+[A-Za-z0-9][A-Za-z0-9:_-]*/)) {
          name = substr(seg, 1, RLENGTH)
          sub(/^task[ \t]+/, "", name)
          print line, name
        }
      }
    }
    /^[ \t]*(```|~~~)/ { fence = !fence; next }
    fence { commands($0, NR); next }
    {
      rest = $0
      # commands() calls match() too: save the position of this match before calling it.
      while (match(rest, /`[^`]+`/)) {
        start = RSTART
        len = RLENGTH
        commands(substr(rest, start + 1, len - 2), NR)
        rest = substr(rest, start + len)
      }
    }
  ' "$README"
}

# taskfile_names prints the name of every task in Taskfile.yml (keys under tasks:).
taskfile_names() {
  awk '
    /^tasks:/ { in_tasks = 1; next }
    in_tasks && /^[^ #]/ { in_tasks = 0 }
    in_tasks && /^  [A-Za-z0-9][A-Za-z0-9:_-]*:/ { name = $1; sub(/:$/, "", name); print name }
  ' "$TASKFILE"
}

# --- commands ---------------------------------------------------------------
known=$(taskfile_names)
found=$(task_names)

if [ -z "$known" ]; then
  report "Taskfile.yml lists its tasks" no "no task found in $TASKFILE"
fi

if [ -z "$found" ]; then
  report "every task command in the README is a task of Taskfile.yml" no "the README has no task command"
else
  unknown=""
  while read -r line name; do
    printf '%s\n' "$known" | grep -qxF -- "$name" || unknown="$unknown [line $line: task $name]"
  done <<EOF
$found
EOF
  if [ -n "$unknown" ]; then
    report "every task command in the README is a task of Taskfile.yml" no "not in Taskfile.yml:$unknown"
  else
    report "every task command in the README is a task of Taskfile.yml" ok
  fi
fi

# The commands a reader needs to run and test the stack are not left out.
for need in setup local:up seed test smoke; do
  if printf '%s\n' "$found" | awk '{ print $2 }' | grep -qxF -- "$need"; then
    report "the README documents task $need" ok
  else
    report "the README documents task $need" no "no 'task $need' command"
  fi
done

# --- secrets ----------------------------------------------------------------
hits=$(grep -nE '(^|[^A-Za-z0-9_])sk-' "$README" | lines_of)
if [ -n "$hits" ]; then
  report "no sk- key" no "sk- on line(s) $hits"
else
  report "no sk- key" ok
fi

# A KIWI_ variable is named, never given a value, except the two opt-in switches that are not secrets:
# KIWI_LIVE_E2E=1 and KIWI_SMOKE_PROFILE=minimal (which stack `task smoke` runs on).
hits=$(grep -noE 'KIWI_[A-Z0-9_]+[[:space:]]*=[[:space:]]*[^[:space:]]+' "$README" |
  grep -vE '^[0-9]+:KIWI_(LIVE_E2E=1|SMOKE_PROFILE=minimal)[^0-9A-Za-z_]*$' | lines_of)
if [ -n "$hits" ]; then
  report "no KIWI_ variable assigned a value" no "an assignment on line(s) $hits"
else
  report "no KIWI_ variable assigned a value" ok
fi

# --- the private values of .env ---------------------------------------------
if [ ! -f "$ENV_FILE" ]; then
  echo "SKIP the private endpoint hostname and secret value checks: $ENV_FILE is absent (they run where a .env exists)"
  # In GitHub Actions the skip also shows as a warning on the run.
  [ "${GITHUB_ACTIONS:-}" != true ] || echo "::warning::readme.test.sh skipped the checks against the private .env values: there is no .env in CI"
else
  : >"$WORK/hosts"
  : >"$WORK/secrets"
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    line=${line#"${line%%[![:space:]]*}"}
    case $line in '' | '#'*) continue ;; esac
    line=${line#export }
    name=${line%%=*}
    value=${line#*=}
    [[ $name =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    value=${value#[\"\']}
    value=${value%[\"\']}
    if [[ $value =~ ^[A-Za-z][A-Za-z0-9+.-]*://([^/?#]*) ]]; then
      host=${BASH_REMATCH[1]##*@}
      host=$(printf '%s' "${host%:[0-9]*}" | tr 'A-Z' 'a-z')
      # A name without a dot (postgres, valkey) and the loopback are not private.
      case $host in
        '' | localhost | *.localhost | 127.0.0.1) ;;
        *.*) printf '%s\n' "$host" >>"$WORK/hosts" ;;
      esac
    fi
    # Short values (the placeholder `mock`) would match ordinary words.
    if [[ $name =~ (KEY|SECRET|PASSWORD|TOKEN) ]] && [ "${#value}" -ge 12 ]; then
      printf '%s\n' "$value" >>"$WORK/secrets"
    fi
  done <"$ENV_FILE"

  hits=""
  if [ -s "$WORK/hosts" ]; then
    hits=$(grep -nFi -f "$WORK/hosts" "$README" | lines_of)
  fi
  if [ -n "$hits" ]; then
    report "no private endpoint hostname of .env" no "a hostname from .env on line(s) $hits"
  else
    report "no private endpoint hostname of .env ($(wc -l <"$WORK/hosts" | tr -d ' ') checked)" ok
  fi

  hits=""
  if [ -s "$WORK/secrets" ]; then
    hits=$(grep -nF -f "$WORK/secrets" "$README" | lines_of)
  fi
  if [ -n "$hits" ]; then
    report "no secret value of .env" no "a secret value from .env on line(s) $hits"
  else
    report "no secret value of .env ($(wc -l <"$WORK/secrets" | tr -d ' ') checked)" ok
  fi
fi

# --- people -----------------------------------------------------------------
# The repository is public: only placeholders may stand for an address.
hits=$(grep -noE '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$README" |
  grep -viE '^[0-9]+:(you|teammate)@|@(example\.com|demo\.example|elsewhere\.example|kiwi-demo\.invalid)$' | lines_of)
if [ -n "$hits" ]; then
  report "no email address but placeholders" no "an address on line(s) $hits"
else
  report "no email address but placeholders" ok
fi

# --- links ------------------------------------------------------------------
# README.md is the only document pushed: pointing at the local-only ones leaves dead links.
hits=$(grep -nE '(^|[[:space:](`"<[])(\./)?(docs/|DESIGN\.md|PRODUCT\.md|\.impeccable)' "$README" | lines_of)
if [ -n "$hits" ]; then
  report "no reference to a document that is not pushed" no "a reference on line(s) $hits"
else
  report "no reference to a document that is not pushed" ok
fi

# --- statements the repository can check ---------------------------------------
# kubeconform downloads the Kubernetes schemas (from raw.githubusercontent.com), so the guard
# suites need no cluster and no Docker, but they are not offline.
desc=$(awk '/^  check:scripts:$/ { b = 1; next } /^  [^ ]/ { b = 0 } b && /desc:/' "$TASKFILE")
mention=$(grep -F 'task check:scripts` runs' "$README")
if [ -z "$desc" ] || [ -z "$mention" ]; then
  report "task check:scripts is described by the Taskfile and the README" no "the Taskfile desc or the README bullet is missing"
elif printf '%s\n%s\n' "$desc" "$mention" | grep -qiE '(^|[^a-z])offline([^a-z]|$)'; then
  report "task check:scripts is not called offline (kubeconform needs the network)" no "the Taskfile desc or the README bullet says offline"
elif ! printf '%s\n' "$desc" | grep -qi 'network' || ! printf '%s\n' "$mention" | grep -qi 'network'; then
  report "task check:scripts says it needs network access" no "the Taskfile desc or the README bullet does not mention the network"
else
  report "task check:scripts says it needs no cluster and no Docker but network access" ok
fi

# Only some steps of the smoke scripts are timed with timeout; tilt and the kubectl waits
# bring their own --timeout. No sentence claims that every step is.
hits=$(grep -nE 'bound(ed|s)? every step' "$README" | lines_of)
if [ -n "$hits" ]; then
  report "the README does not claim every step of the smoke scripts is bounded by timeout" no "on line(s) $hits"
else
  report "the README does not claim every step of the smoke scripts is bounded by timeout" ok
fi

if [ "$failures" -ne 0 ]; then
  echo "readme.test.sh: $failures of $checks check(s) failed"
  exit 1
fi
echo "readme.test.sh: all $checks checks passed"
