-- +goose Up

CREATE SCHEMA identity;

CREATE TABLE identity.workspaces (
    id            uuid PRIMARY KEY,
    name          text NOT NULL,
    google_domain text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE identity.users (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES identity.workspaces (id),
    -- Seeded placeholder users carry "seed:" || email until their first sign-in.
    google_sub   text NOT NULL UNIQUE,
    email        text NOT NULL,
    name         text NOT NULL,
    avatar_url   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    UNIQUE (workspace_id, email)
);

CREATE TABLE identity.oauth_tokens (
    user_id           uuid PRIMARY KEY,
    workspace_id      uuid NOT NULL,
    refresh_token_enc bytea NOT NULL,
    scopes            text[] NOT NULL DEFAULT '{}',
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, user_id) REFERENCES identity.users (workspace_id, id) ON DELETE CASCADE
);
