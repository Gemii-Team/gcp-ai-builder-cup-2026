-- +goose Up

CREATE SCHEMA tasks;

CREATE TABLE tasks.tasks (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES identity.workspaces (id),
    channel_id      uuid NOT NULL, -- typed reference to chat.channels
    -- TSK-1001 style REF.
    ref             text,
    title           text NOT NULL,
    description     text NOT NULL DEFAULT '',
    state           text NOT NULL CHECK (state IN ('draft', 'confirmed')),
    status          text NOT NULL DEFAULT 'todo' CHECK (status IN ('todo', 'doing', 'done')),
    assignee_id     uuid,
    due_at          timestamptz,
    done_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Fractional index within (channel_id, status). Byte order, not the
    -- database collation, is what PositionBetween sorts by.
    position        text COLLATE "C" NOT NULL,
    -- The typed reference to what the task came from, all null for a task made
    -- by hand, plus its display snapshot {label, at?, offset_ms?, page?, quote?}.
    source_kind     text,
    source_id       uuid,
    source_anchor   text,
    source_meta     jsonb,
    -- REF of the answer the task was made from (ANS-2209), if any.
    from_ref        text,
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('user', 'agent')),
    created_by_id   uuid NOT NULL, -- a user, or authz.AgentID(name)
    UNIQUE (workspace_id, ref),
    FOREIGN KEY (workspace_id, assignee_id) REFERENCES identity.users (workspace_id, id)
);

CREATE INDEX tasks_board_idx ON tasks.tasks (workspace_id, channel_id, status, position);
