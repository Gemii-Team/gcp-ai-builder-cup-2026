-- +goose Up

CREATE SCHEMA agents;

CREATE TABLE agents.runs (
    id                 uuid PRIMARY KEY,
    workspace_id       uuid NOT NULL REFERENCES identity.workspaces (id),
    agent              text NOT NULL,
    -- Null for an Ask-panel run, which has a question and no triggering message.
    channel_id         uuid, -- typed reference to chat.channels
    trigger_message_id uuid, -- typed reference to chat.messages
    question           text NOT NULL DEFAULT '',
    on_behalf_of       uuid NOT NULL,
    status             text NOT NULL CHECK (status IN ('queued', 'running', 'done', 'failed')),
    failure_reason     text,
    -- ANS-1001 style REF; issued whenever an answer exists, found or not, never for a failure.
    ref                text,
    -- protojson of the chat.v1.Artifact the run produced.
    result             jsonb,
    tool_calls         jsonb NOT NULL DEFAULT '[]',
    input_tokens       int NOT NULL DEFAULT 0,
    output_tokens      int NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, ref),
    FOREIGN KEY (workspace_id, on_behalf_of) REFERENCES identity.users (workspace_id, id)
);

-- ADK session persistence: one session per (user, channel | "ask"), keyed by
-- the session id the runner hands out.
CREATE TABLE agents.sessions (
    id           text PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES identity.workspaces (id),
    app_name     text NOT NULL,
    user_id      uuid NOT NULL,
    state        jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES identity.users (workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE agents.session_events (
    id           bigserial PRIMARY KEY, -- append order within a session
    session_id   text NOT NULL,
    workspace_id uuid NOT NULL,
    event        jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, session_id) REFERENCES agents.sessions (workspace_id, id) ON DELETE CASCADE
);

CREATE INDEX session_events_session_idx ON agents.session_events (session_id, id);
