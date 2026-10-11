-- Queries of the docs domain (sqlc generates internal/docs/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security
-- binds each statement to one workspace. A file moves forward only: uploaded, then
-- analyzing, then ready or failed, and failed goes back to analyzing on a retry.
-- Each transition names the status it leaves, so a concurrent or repeated one finds
-- no row and the caller sees that someone else already moved the file.

-- name: InsertFile :one
-- Registers an upload that has been signed but not yet validated.
INSERT INTO files.files (id, workspace_id, channel_id, uploaded_by, name, blob_key, mime, size, status)
VALUES (
    sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(channel_id), sqlc.arg(uploaded_by),
    sqlc.arg(name), sqlc.arg(blob_key), sqlc.arg(mime), sqlc.arg(size), 'uploaded'
)
RETURNING *;

-- name: GetFile :one
SELECT * FROM files.files WHERE id = sqlc.arg(id);

-- name: LockFile :one
-- Serialises the writers of one file until the transaction ends.
SELECT * FROM files.files WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: FinishUpload :one
-- Records the validated object: its real size, and for an attachment the message that
-- carries it and the status analyzing. A recording keeps the status uploaded for the
-- meetings domain to take over.
UPDATE files.files
SET size = sqlc.arg(size), status = sqlc.arg(status), message_id = sqlc.narg(message_id)
WHERE id = sqlc.arg(id) AND status = 'uploaded'
RETURNING *;

-- name: MarkFileReady :one
UPDATE files.files
SET status = 'ready', ref = sqlc.arg(ref), pages = sqlc.narg(pages), failure_reason = NULL
WHERE id = sqlc.arg(id) AND status = 'analyzing'
RETURNING *;

-- name: MarkFileFailed :one
-- A file fails on the way in (uploaded) or during analysis (analyzing); a ready or
-- already failed file is left alone.
UPDATE files.files
SET status = 'failed', failure_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id) AND status IN ('uploaded', 'analyzing')
RETURNING *;

-- name: RestartAnalysis :one
-- Only a file whose upload was accepted (it has its message) has anything to retry.
UPDATE files.files
SET status = 'analyzing', failure_reason = NULL
WHERE id = sqlc.arg(id) AND status = 'failed' AND message_id IS NOT NULL
RETURNING *;
