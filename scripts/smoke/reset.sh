#!/usr/bin/env bash
# Usage: scripts/smoke/reset.sh                                   (task reset)
#
# Wipes the workspace data of the running k3d cluster. First it asks whether the cluster has a
# Valkey Deployment (the full stack has one, the minimal stack does not); it does that before
# changing anything, so a cluster that cannot answer is never left half wiped. Then, in this
# order:
#   1. kiwi is scaled to 0 and its pod waited for: the outbox relay holds a row in a
#      transaction while a model call runs, and the TRUNCATE below needs a lock on that
#      table, so it would wait behind it (a hung model call once held it for 29 minutes);
#   2. as the Postgres superuser one DO block truncates every table of the application
#      schemas (agents, chat, files, identity, knowledge, meetings, platform, tasks; goose's
#      version table in public stays, and no migration inserts rows into those tables),
#      with lock_timeout 30 s, so it can fail but never wait for ever;
#   3. Valkey is flushed (sessions, tickets, caches) when deploy/valkey exists. The minimal
#      stack has none: its tickets, OAuth state and cache live in the kiwi process, which
#      step 1 stopped, and its sessions are in the identity schema, which step 2 truncated;
#   4. kiwi is scaled back to 1 and waited for.
# Whatever fails, kiwi is scaled back to 1 before the script exits non-zero: a failed reset
# never leaves the cluster with no kiwi, and when even the scale-back fails the script prints
# the command that does it by hand. Every step that can wait on kiwi, the database or a model
# is bounded: the kubectl waits, and the three timeouts below (the Valkey lookup, the TRUNCATE
# and the flush; GNU coreutils `timeout`, gtimeout on macOS, run with --foreground so that
# Ctrl-C reaches kubectl: see bounded.sh).
# Everything of the workspace is gone, whoever made it (a developer's real-Google "Team"
# workspace and its Meet grant included); run `task seed` afterwards. Nothing is printed of a
# connection string or a value.
set -euo pipefail

kc=(kubectl --context k3d-kiwi -n kiwi)

# shellcheck source=scripts/smoke/bounded.sh
. "$(dirname "${BASH_SOURCE[0]}")/bounded.sh"

restore_kiwi() {
  local status=$?
  trap - EXIT
  if [ "$status" -ne 0 ]; then
    set +e
    echo "reset: failed (exit $status); scaling kiwi back to 1" >&2
    if ! "${kc[@]}" scale deploy/kiwi --replicas=1 >/dev/null; then
      echo "reset: COULD NOT scale kiwi back to 1: run ${kc[*]} scale deploy/kiwi --replicas=1" >&2
    fi
  fi
  exit "$status"
}
trap restore_kiwi EXIT

# --ignore-not-found turns "no Valkey" into an empty answer and leaves every other failure
# (no API server, no permission) a failure, which stops the script here, before anything is changed.
valkey="$(bounded 30 "${kc[@]}" get deploy/valkey --ignore-not-found -o name)"

"${kc[@]}" scale deploy/kiwi --replicas=0 >/dev/null
"${kc[@]}" wait --for=delete pod -l app.kubernetes.io/name=kiwi --timeout=120s

bounded 150 "${kc[@]}" exec -i deploy/postgres -- psql -U postgres -d kiwi -v ON_ERROR_STOP=1 -q <<'SQL'
SET lock_timeout = '30s';
DO $$
DECLARE stmt text;
BEGIN
  SELECT 'TRUNCATE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE'
    INTO stmt FROM pg_tables
   WHERE schemaname IN ('agents','chat','files','identity','knowledge','meetings','platform','tasks');
  EXECUTE stmt;
END $$;
SQL
if [ -n "$valkey" ]; then
  bounded 60 "${kc[@]}" exec deploy/valkey -- valkey-cli FLUSHALL >/dev/null
else
  echo "reset: no Valkey in this cluster (minimal stack): nothing to flush"
fi

"${kc[@]}" scale deploy/kiwi --replicas=1 >/dev/null
"${kc[@]}" rollout status deploy/kiwi --timeout=180s
