-- +goose Up

CREATE SCHEMA platform;

-- Transactional outbox. Infrastructure shared by every domain, so no
-- workspace_id and no RLS: the payload (protojson) carries the workspace.
CREATE TABLE platform.outbox (
    id            bigserial PRIMARY KEY,
    topic         text NOT NULL,
    key           text NOT NULL DEFAULT '',
    payload       jsonb NOT NULL,
    trace_context text NOT NULL DEFAULT '',
    -- The only delay mechanism: handlers never sleep, they enqueue with a later not_before.
    not_before    timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz
);

-- The relay claims the oldest unpublished row whose not_before has passed.
CREATE INDEX outbox_unpublished_idx ON platform.outbox (id) WHERE published_at IS NULL;

-- Effects are exactly-once because the consumer inserts here in the same
-- transaction as the effect.
CREATE TABLE platform.processed_events (
    consumer     text NOT NULL,
    event_id     text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- REF allocator: MTG-1001, MTG-1002, ... per workspace and kind. Tenant data,
-- so it has RLS like any table with a workspace_id.
CREATE TABLE platform.ref_counters (
    workspace_id uuid NOT NULL REFERENCES identity.workspaces (id),
    kind         text NOT NULL CHECK (kind ~ '^[A-Z]{3}$'),
    last         int NOT NULL DEFAULT 1000,
    PRIMARY KEY (workspace_id, kind)
);
