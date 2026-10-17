-- +goose Up

CREATE SCHEMA calendar;

-- A scheduled meeting: a Google Calendar event on the organizer's own calendar
-- with a Meet link. The row is what ListUpcoming reads; gcal_event_id is what
-- CancelEvent names to Google.
CREATE TABLE calendar.events (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL,
    channel_id     uuid NOT NULL, -- typed reference to chat.channels
    -- The organizer, whose Google credentials created (and may cancel) the event.
    owner_user_id  uuid NOT NULL,
    -- EVT-1001 style REF.
    ref            text,
    title          text NOT NULL,
    gcal_event_id  text NOT NULL,
    html_link      text NOT NULL DEFAULT '',
    meet_uri       text NOT NULL DEFAULT '',
    status         text NOT NULL DEFAULT 'scheduled'
                   CHECK (status IN ('scheduled', 'canceled')),
    start_at       timestamptz NOT NULL,
    end_at         timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (start_at < end_at),
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, ref),
    FOREIGN KEY (workspace_id) REFERENCES identity.workspaces (id),
    FOREIGN KEY (workspace_id, owner_user_id) REFERENCES identity.users (workspace_id, id)
);

-- ListUpcoming reads by channel and start; the workspace-wide list filters by end_at.
CREATE INDEX events_channel_start_idx ON calendar.events (workspace_id, channel_id, start_at);

ALTER TABLE calendar.events ENABLE ROW LEVEL SECURITY;
ALTER TABLE calendar.events FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON calendar.events
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

-- kiwi_app reaches the new schema exactly as it reaches the other domain schemas
-- (0010 pins the grant sets; TestRoles_GrantsAreExact sees this widening).
-- kiwi_system and kiwi_ro get nothing.
GRANT USAGE ON SCHEMA calendar TO kiwi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA calendar TO kiwi_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA calendar
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kiwi_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA calendar
    GRANT USAGE ON SEQUENCES TO kiwi_app;

-- +goose Down

-- The default ACLs hang off the schema and must go before it does.
ALTER DEFAULT PRIVILEGES IN SCHEMA calendar
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM kiwi_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA calendar
    REVOKE USAGE ON SEQUENCES FROM kiwi_app;
DROP SCHEMA calendar CASCADE;
