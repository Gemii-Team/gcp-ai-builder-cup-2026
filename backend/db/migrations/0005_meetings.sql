-- +goose Up

CREATE SCHEMA meetings;

CREATE TABLE meetings.meetings (
    id                uuid PRIMARY KEY,
    workspace_id      uuid NOT NULL,
    channel_id        uuid NOT NULL, -- typed reference to chat.channels
    -- The user whose Google credentials talk to Meet; the uploader for a recording.
    owner_user_id     uuid NOT NULL,
    -- MTG-1001 style REF.
    ref               text,
    title             text NOT NULL DEFAULT '',
    meet_space        text, -- Meet space resource name; null for a recording
    meet_uri          text,
    conference_record text,
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'ingesting', 'ready', 'failed', 'no_transcript')),
    failure_reason    text,
    last_entry_count  int, -- null until the first read; ingest waits for two equal reads
    started_at        timestamptz,
    ended_at          timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, ref),
    FOREIGN KEY (workspace_id) REFERENCES identity.workspaces (id),
    FOREIGN KEY (workspace_id, owner_user_id) REFERENCES identity.users (workspace_id, id)
);

CREATE INDEX meetings_channel_idx ON meetings.meetings (workspace_id, channel_id);

CREATE TABLE meetings.transcript_entries (
    meeting_id   uuid NOT NULL,
    workspace_id uuid NOT NULL,
    idx          int NOT NULL,
    speaker      text,   -- null when the source has no speaker labels
    start_ms     bigint, -- null without timing
    end_ms       bigint,
    text         text NOT NULL,
    PRIMARY KEY (meeting_id, idx),
    FOREIGN KEY (workspace_id, meeting_id) REFERENCES meetings.meetings (workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE meetings.summaries (
    meeting_id     uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL,
    summary        text NOT NULL,
    -- [{text, entry_refs: [idx, ...]}]
    decisions      jsonb NOT NULL DEFAULT '[]',
    open_questions jsonb NOT NULL DEFAULT '[]',
    created_at     timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, meeting_id) REFERENCES meetings.meetings (workspace_id, id) ON DELETE CASCADE
);

-- Unused while Meet is polled; the Workspace Events subscription of the GCP phase.
CREATE TABLE meetings.space_subscriptions (
    id                uuid PRIMARY KEY,
    workspace_id      uuid NOT NULL,
    meeting_id        uuid NOT NULL,
    user_id           uuid NOT NULL,
    subscription_name text NOT NULL UNIQUE,
    expires_at        timestamptz NOT NULL,
    FOREIGN KEY (workspace_id, meeting_id) REFERENCES meetings.meetings (workspace_id, id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, user_id) REFERENCES identity.users (workspace_id, id)
);

CREATE INDEX space_subscriptions_expiry_idx ON meetings.space_subscriptions (expires_at);
