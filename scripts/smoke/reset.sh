#!/usr/bin/env bash
# Usage: scripts/smoke/reset.sh                                   (task reset)
#
# Wipes the workspace data of the running k3d cluster: as the Postgres superuser one DO
# block truncates every table of the application schemas (agents, chat, files, identity,
# knowledge, meetings, platform, tasks; goose's version table in public stays, and no
# migration inserts rows into those tables), Valkey is flushed (sessions, tickets, caches)
# and kiwi is restarted. Everything of the workspace is gone, whoever made it (a developer's
# real-Google "Team" workspace and its Meet grant included); run `task seed` afterwards.
# Nothing is printed of a connection string or a value.
set -euo pipefail

kc=(kubectl --context k3d-kiwi -n kiwi)

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
