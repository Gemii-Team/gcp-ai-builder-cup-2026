-- +goose Up

CREATE SCHEMA knowledge;

CREATE TABLE knowledge.chunks (
    -- UUIDv5(workspace_id, source_kind/source_id/anchor): indexing is idempotent.
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES identity.workspaces (id),
    -- The channel whose members may retrieve the chunk; null = workspace-wide.
    channel_id      uuid,
    source_kind     text NOT NULL,
    source_id       uuid NOT NULL,
    source_anchor   text NOT NULL,
    lang            text NOT NULL,
    content         text NOT NULL,
    content_sha256  text NOT NULL,
    -- to_tsvector('simple', content), written by the indexer.
    tsv             tsvector NOT NULL,
    -- Display snapshot {label, at?, offset_ms?} of the source, so a citation
    -- renders without a cross-domain call.
    source_meta     jsonb,
    -- Part of the vector: retrieval filters on it, vectors of different models
    -- are never compared, and changing it needs `kiwi reindex`.
    embedding_model text NOT NULL,
    embedding       vector(768) NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chunks_embedding_hnsw_idx ON knowledge.chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX chunks_tsv_idx ON knowledge.chunks USING gin (tsv);
-- Re-indexing a source finds its chunks, and deletes the stale ones, by source.
CREATE INDEX chunks_source_idx ON knowledge.chunks (workspace_id, source_kind, source_id);
CREATE INDEX chunks_channel_idx ON knowledge.chunks (workspace_id, channel_id);
