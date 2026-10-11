-- +goose Up

CREATE SCHEMA files;

CREATE TABLE files.files (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES identity.workspaces (id),
    channel_id     uuid NOT NULL, -- typed reference to chat.channels
    message_id     uuid,          -- the attachment message, once posted
    uploaded_by    uuid NOT NULL,
    -- DOC-1001 style REF; null until analysis succeeds, a failed attempt issues none.
    ref            text,
    name           text NOT NULL,
    blob_key       text NOT NULL,
    mime           text NOT NULL,
    size           bigint NOT NULL,
    sha256         text NOT NULL DEFAULT '',
    pages          int, -- null = unknown
    status         text NOT NULL CHECK (status IN ('uploaded', 'analyzing', 'ready', 'failed')),
    failure_reason text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, ref),
    FOREIGN KEY (workspace_id, uploaded_by) REFERENCES identity.users (workspace_id, id)
);

CREATE INDEX files_channel_idx ON files.files (workspace_id, channel_id);
