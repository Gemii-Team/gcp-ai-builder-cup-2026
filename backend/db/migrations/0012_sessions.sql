-- +goose Up

-- Browser sessions, for a deployment whose cache is in the process
-- (KIWI_CACHE_PROVIDER=memory) and so cannot hold them: a redeploy must not sign
-- everyone out. With Valkey this table stays empty.
--
-- Expand-only: a new table that no older binary reads or writes, so the binary
-- before it keeps working against this schema.
--
-- The primary key is the SHA-256 of the session id, never the id. The id is a bearer
-- credential (256 random bits, the value of the session cookie); a copy of this table,
-- a backup or a read-only SQL injection must not be able to sign anyone in, and the id
-- has too much entropy for a hash without a salt to be guessed back.
--
-- A session is looked up by its id alone, before any workspace is known, so the table
-- cannot be read through a workspace-bound transaction. It is reached only through
-- db.System, from identity/callback.go. kiwi_app gets nothing on it, although it is a
-- tenant table with the usual policy as a second line of defence.
--
-- created_at starts the absolute lifetime. expires_at is the sliding one, pushed
-- forward on every read and never past created_at + the absolute lifetime. Expired rows
-- are deleted at the next sign-in.

CREATE TABLE identity.sessions (
    id_hash      bytea PRIMARY KEY CHECK (octet_length(id_hash) = 32),
    user_id      uuid NOT NULL,
    workspace_id uuid NOT NULL,
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    -- Deleting a user, or a workspace through its users, ends their sessions.
    FOREIGN KEY (workspace_id, user_id) REFERENCES identity.users (workspace_id, id) ON DELETE CASCADE
);

-- The cascade above looks sessions up by user.
CREATE INDEX sessions_user_idx ON identity.sessions (workspace_id, user_id);

ALTER TABLE identity.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON identity.sessions
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

-- The default privileges of 0010 gave kiwi_app DML on every new identity table.
-- Sessions are not its business.
REVOKE ALL ON identity.sessions FROM kiwi_app;
-- kiwi_system: the session store (identity/callback.go) reads, slides, creates and
-- deletes sessions, and nothing else on this table or any other. This is the only
-- DELETE kiwi_system holds. The grant is for the role, not for the file: the relay and
-- the Meet poll share it, so a bug in their SQL could mint or delete a session. That is
-- the price of looking a session up before its workspace is known, and the db.System
-- allowlist is what keeps the callers to three files.
GRANT SELECT, INSERT, UPDATE, DELETE ON identity.sessions TO kiwi_system;

-- +goose Down

DROP TABLE identity.sessions;
