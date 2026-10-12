# shellcheck shell=bash
# Sourced by run.sh and reset.sh (not run on its own).
#
# bounded SECONDS COMMAND [ARGS...] runs COMMAND for at most SECONDS. GNU coreutils `timeout` is a
# prerequisite (on macOS it is `gtimeout`, from `brew install coreutils`); a machine with neither
# stops here with exit 2, before anything has been changed.
#
# --foreground keeps COMMAND in the terminal's foreground process group. Without it timeout moves
# COMMAND into a group of its own, which the terminal never signals: Ctrl-C stops the script and
# leaves the command it waits for running. The price is that when the time is up only COMMAND is
# signalled, not what it started.
if command -v timeout >/dev/null 2>&1; then
  KIWI_TIMEOUT_BIN=timeout
elif command -v gtimeout >/dev/null 2>&1; then
  KIWI_TIMEOUT_BIN=gtimeout
else
  echo "smoke: GNU coreutils timeout is required (macOS: brew install coreutils, which provides gtimeout)" >&2
  exit 2
fi

bounded() {
  "$KIWI_TIMEOUT_BIN" --foreground "$@"
}
