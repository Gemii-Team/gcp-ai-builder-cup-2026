-- Queries of the knowledge domain (sqlc generates internal/knowledge/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security binds
-- each statement to one workspace, except ListWorkspaceIDs, which `kiwi reindex` runs
-- through db.System to find the workspaces whose chunks it re-embeds. The text[]
-- parameters are cast to uuid[] on the server: the pools use pgx's QueryExecModeExec,
-- which sends no parameter types and so cannot encode a []uuid.UUID.
--
-- A chunk's id is UUIDv5(workspace_id, source_kind/source_id/anchor), so an upsert by
-- id is what makes indexing idempotent. The last four queries read what the indexer
-- needs to label a chunk (a title, a file name, an author's name) and to find the
-- channel a source belongs to, from tables of other domains: the services of those
-- domains grant such reads to a person's actor and not to the indexer's system actor.
-- They only read, and row-level security still bounds them to the workspace.

-- name: ListChunkStates :many
-- What is already stored for the chunks among ids: one row per chunk that exists.
SELECT id, content_sha256, embedding_model
FROM knowledge.chunks
WHERE id = ANY(sqlc.arg(ids)::text[]::uuid[]);

-- name: UpsertChunk :exec
-- Writes a chunk with the vector of its content. tsv is derived here, from the same
-- content, so the two can never disagree.
INSERT INTO knowledge.chunks (
    id, workspace_id, channel_id, source_kind, source_id, source_anchor, lang,
    content, content_sha256, tsv, source_meta, embedding_model, embedding
) VALUES (
    sqlc.arg(id), sqlc.arg(workspace_id), sqlc.narg(channel_id), sqlc.arg(source_kind), sqlc.arg(source_id),
    sqlc.arg(source_anchor), sqlc.arg(lang), sqlc.arg(content)::text, sqlc.arg(content_sha256),
    to_tsvector('simple', sqlc.arg(content)::text), sqlc.arg(source_meta)::jsonb, sqlc.arg(embedding_model),
    sqlc.arg(embedding)::vector
)
ON CONFLICT (id) DO UPDATE SET
    channel_id      = EXCLUDED.channel_id,
    source_anchor   = EXCLUDED.source_anchor,
    lang            = EXCLUDED.lang,
    content         = EXCLUDED.content,
    content_sha256  = EXCLUDED.content_sha256,
    tsv             = EXCLUDED.tsv,
    source_meta     = EXCLUDED.source_meta,
    embedding_model = EXCLUDED.embedding_model,
    embedding       = EXCLUDED.embedding,
    updated_at      = now();

-- name: DeleteStaleSourceChunks :execrows
-- Removes the chunks of a source that its current text no longer produces.
DELETE FROM knowledge.chunks
WHERE source_kind = sqlc.arg(source_kind)
  AND source_id = sqlc.arg(source_id)
  AND NOT (id = ANY(sqlc.arg(keep)::text[]::uuid[]));

-- name: DeleteStaleWindowChunks :execrows
-- Removes the chunks of one chat window that its current messages no longer produce.
-- A chat source is the whole channel, so the window is found by the time of the
-- chunk's first message, which source_meta keeps as "at".
DELETE FROM knowledge.chunks
WHERE source_kind = 'chat'
  AND source_id = sqlc.arg(source_id)
  AND (source_meta ->> 'at')::timestamptz >= sqlc.arg(window_start)::timestamptz
  AND (source_meta ->> 'at')::timestamptz < sqlc.arg(window_end)::timestamptz
  AND NOT (id = ANY(sqlc.arg(keep)::text[]::uuid[]));

-- name: ListChunksToReembed :many
-- The next chunks whose vector was made by another model than embedding_model. A
-- chunk leaves this list once it is re-embedded, so a run that stops half way
-- continues where it was.
SELECT id, content, content_sha256
FROM knowledge.chunks
WHERE embedding_model <> sqlc.arg(embedding_model)
ORDER BY id
LIMIT sqlc.arg(row_limit);

-- name: UpdateChunkEmbedding :execrows
-- Stores a new vector for a chunk, unless indexing changed its content since it was
-- read: the vector belongs to the content that was embedded.
UPDATE knowledge.chunks
SET embedding = sqlc.arg(embedding)::vector, embedding_model = sqlc.arg(embedding_model), updated_at = now()
WHERE id = sqlc.arg(id) AND content_sha256 = sqlc.arg(content_sha256);

-- name: ListWorkspaceIDs :many
SELECT id FROM identity.workspaces ORDER BY id;

-- name: GetMeetingSource :one
SELECT id, channel_id, owner_user_id, title, status
FROM meetings.meetings
WHERE id = sqlc.arg(id);

-- name: GetMeetingSummary :one
SELECT summary, decisions, open_questions
FROM meetings.summaries
WHERE meeting_id = sqlc.arg(meeting_id);

-- name: GetFileSource :one
SELECT id, channel_id, uploaded_by, name
FROM files.files
WHERE id = sqlc.arg(id);

-- name: ListUserNames :many
SELECT id, name
FROM identity.users
WHERE id = ANY(sqlc.arg(ids)::text[]::uuid[]);
