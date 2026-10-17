-- Queries of the calendar domain (sqlc generates internal/calendar/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security binds
-- each statement to one workspace. An event moves one way: scheduled, then canceled;
-- the cancel names the status it leaves, so a repeated one finds no row and the
-- caller sees that the event has already moved.

-- name: InsertEvent :one
INSERT INTO calendar.events (
    id, workspace_id, channel_id, owner_user_id, ref, title, gcal_event_id, html_link, meet_uri,
    start_at, end_at, org_wide
)
VALUES (
    sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(channel_id), sqlc.arg(owner_user_id), sqlc.arg(ref),
    sqlc.arg(title), sqlc.arg(gcal_event_id), sqlc.arg(html_link), sqlc.arg(meet_uri),
    sqlc.arg(start_at), sqlc.arg(end_at), sqlc.arg(org_wide)
)
RETURNING *;

-- name: GetEvent :one
SELECT * FROM calendar.events WHERE id = sqlc.arg(id);

-- name: ListUpcomingByChannel :many
-- Events of one channel that have not ended at `now`, soonest first.
SELECT * FROM calendar.events
WHERE channel_id = sqlc.arg(channel_id)
  AND status = 'scheduled'
  AND end_at > sqlc.arg(now)::timestamptz
ORDER BY start_at, id;

-- name: ListUpcomingInChannels :many
-- The workspace-wide list: events that have not ended at `now` and are either in one
-- of the caller's channels or marked org-wide (which the whole workspace sees). The id
-- list travels as text[] and is cast on the server, so it encodes under the pools'
-- QueryExecModeExec (no prepared statements to infer a uuid[] element type).
SELECT * FROM calendar.events
WHERE (channel_id = ANY(sqlc.arg(channel_ids)::text[]::uuid[]) OR org_wide)
  AND status = 'scheduled'
  AND end_at > sqlc.arg(now)::timestamptz
ORDER BY start_at, id;

-- name: CancelEvent :one
-- Only a scheduled event cancels; a repeated cancel finds no row.
UPDATE calendar.events
SET status = 'canceled'
WHERE id = sqlc.arg(id) AND status = 'scheduled'
RETURNING *;
