#!/usr/bin/env bash
# One clean take, then the render. Resets what a take changes in the Team workspace (the two chat messages it posts and
# the draft task it accepts), records, and tries again up to three times when a take is abandoned (a slow or failed Ask).
set -uo pipefail
cd "$(dirname "$0")/.."
reset() {
  kubectl --context k3d-kiwi -n kiwi exec -i deploy/postgres -- sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" psql -U "$POSTGRES_USER" -d kiwi -qAt' <<'SQL'
DELETE FROM chat.messages WHERE workspace_id=(SELECT id FROM identity.workspaces WHERE name='Team')
  AND (body LIKE 'Capacity note is updated%' OR body LIKE 'Sending this one while offline%');
UPDATE tasks.tasks SET state='draft' WHERE workspace_id=(SELECT id FROM identity.workspaces WHERE name='Team')
  AND title='Draft the delivery promise for the landing page' AND state='confirmed';
SQL
}
for attempt in 1 2 3; do
  echo "== take $attempt"
  reset
  if npx tsx scripts/record.ts "$@"; then
    echo "== rendering"
    exec npx remotion render src/index.ts KiwiDemo out/kiwi-demo.mp4 --codec h264 --crf 16 --log=error
  fi
done
echo "== no clean take in 3 attempts"
exit 1
