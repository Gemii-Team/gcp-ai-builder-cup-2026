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
-- Records the validated object: its real size and the sha256 of its bytes, which is
-- what every later read of it is compared with, and for an attachment the message that
-- carries it and the status analyzing. A recording keeps the status uploaded for the
-- meetings domain to take over. A row with a sha256 is a row that was accepted.
UPDATE files.files
SET size = sqlc.arg(size), sha256 = sqlc.arg(sha256), status = sqlc.arg(status), message_id = sqlc.narg(message_id)
WHERE id = sqlc.arg(id) AND status = 'uploaded' AND sha256 = ''
RETURNING *;

-- name: MarkFileReady :one
UPDATE files.files
SET status = 'ready', ref = sqlc.arg(ref), pages = sqlc.narg(pages), failure_reason = NULL
WHERE id = sqlc.arg(id) AND status = 'analyzing'
RETURNING *;

-- name: RejectUpload :one
-- Fails an upload that did not pass validation. It leaves only an upload that is still
-- waiting: one that a concurrent completion has accepted is not flipped.
UPDATE files.files
SET status = 'failed', failure_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id) AND status = 'uploaded' AND sha256 = ''
RETURNING *;

-- name: FailAnalysis :one
-- Fails an analysis that cannot finish. It leaves only a file that is being analysed:
-- one that is ready, or already failed, is not touched.
UPDATE files.files
SET status = 'failed', failure_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id) AND status = 'analyzing'
RETURNING *;

-- name: RestartAnalysis :one
-- Only a file whose upload was accepted (it has its message) has anything to retry.
UPDATE files.files
SET status = 'analyzing', failure_reason = NULL
WHERE id = sqlc.arg(id) AND status = 'failed' AND message_id IS NOT NULL
RETURNING *;
