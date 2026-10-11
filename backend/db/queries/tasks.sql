-- Queries of the tasks domain (sqlc generates internal/tasks/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security
-- binds each statement to one workspace. "position" is a text column with the
-- "C" collation, so ORDER BY position is the byte order PositionBetween sorts by.
--
-- The pools use pgx's QueryExecModeExec, which cannot encode a []uuid.UUID:
-- id lists travel as text[] and are cast on the server.

-- name: LockLane :exec
-- Serialises the writers of one lane (a status of a channel's board) until the
-- transaction ends, so two cards never take the same position.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lane)::text, 0));

-- name: LastPosition :one
-- The position of the last card of a lane other than excluding_id; no row for an
-- empty lane.
SELECT position
FROM tasks.tasks
WHERE channel_id = sqlc.arg(channel_id) AND status = sqlc.arg(status) AND id <> sqlc.arg(excluding_id)
ORDER BY position DESC
LIMIT 1;

-- name: InsertTask :one
INSERT INTO tasks.tasks (
    id, workspace_id, channel_id, ref, title, description, state, status, assignee_id, due_at, position,
    source_kind, source_id, source_anchor, source_meta, from_ref, created_by_kind, created_by_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
RETURNING *;

-- name: GetTask :one
SELECT *
FROM tasks.tasks
WHERE id = $1;

-- name: ListTasks :many
-- The tasks of the given channels, in Board order: by channel, then To do, Doing
-- and Done, then position. An empty states array means every state.
SELECT *
FROM tasks.tasks
WHERE channel_id = ANY(sqlc.arg(channel_ids)::text[]::uuid[])
  AND (cardinality(sqlc.arg(states)::text[]) = 0 OR state = ANY(sqlc.arg(states)::text[]))
ORDER BY channel_id,
         CASE status WHEN 'todo' THEN 0 WHEN 'doing' THEN 1 ELSE 2 END,
         position,
         id;

-- name: UpdateTask :one
-- Only the fields the caller sets change; clear_* removes a value.
UPDATE tasks.tasks
SET title       = COALESCE(sqlc.narg(title), title),
    description = COALESCE(sqlc.narg(description), description),
    assignee_id = CASE WHEN sqlc.arg(clear_assignee)::bool THEN NULL ELSE COALESCE(sqlc.narg(assignee_id), assignee_id) END,
    due_at      = CASE WHEN sqlc.arg(clear_due)::bool THEN NULL ELSE COALESCE(sqlc.narg(due_at), due_at) END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: MoveTask :one
-- Writes the moved row and nothing else. done_at is when the task became Done: a
-- move within Done keeps it, any other status clears it.
UPDATE tasks.tasks
SET status   = sqlc.arg(status)::text,
    position = sqlc.arg(position),
    done_at  = CASE WHEN sqlc.arg(status)::text = 'done' THEN COALESCE(done_at, now()) ELSE NULL END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: ConfirmDraft :one
-- The state predicate is what makes two concurrent confirms seal the draft once:
-- the second UPDATE waits for the first, re-reads the row and finds no draft.
UPDATE tasks.tasks
SET state = 'confirmed'
WHERE id = $1 AND state = 'draft' AND assignee_id IS NOT NULL
RETURNING *;

-- name: DeleteDraft :execrows
DELETE FROM tasks.tasks
WHERE id = $1 AND state = 'draft';
