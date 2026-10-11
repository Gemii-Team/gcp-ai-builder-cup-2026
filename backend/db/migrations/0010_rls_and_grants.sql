-- +goose Up

-- Tenant isolation, fail-closed. Every table with a workspace_id is bound to
-- the transaction's kiwi.workspace_id (db.InWorkspace sets it with SET LOCAL),
-- and FORCE applies the policy to the table owner too. Without the setting the
-- policy sees NULL and the query returns no rows. NULLIF is not decoration: on a
-- pooled connection that already ran SET LOCAL the setting reverts to '', not
-- NULL, and ''::uuid would raise an error instead of returning no rows.
-- identity.workspaces has no workspace_id and is bound by its own id.
--
-- The platform queue tables (outbox, processed_events) are infrastructure and
-- have no policy. The roles themselves are created by the environment
-- bootstrap, because they carry passwords; this migration only grants.

ALTER TABLE identity.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.users FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON identity.users
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE identity.oauth_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.oauth_tokens FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON identity.oauth_tokens
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE chat.channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat.channels FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON chat.channels
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE chat.channel_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat.channel_members FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON chat.channel_members
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE chat.messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat.messages FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON chat.messages
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE files.files ENABLE ROW LEVEL SECURITY;
ALTER TABLE files.files FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON files.files
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE meetings.meetings ENABLE ROW LEVEL SECURITY;
ALTER TABLE meetings.meetings FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON meetings.meetings
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE meetings.transcript_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE meetings.transcript_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON meetings.transcript_entries
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE meetings.summaries ENABLE ROW LEVEL SECURITY;
ALTER TABLE meetings.summaries FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON meetings.summaries
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE meetings.space_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE meetings.space_subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON meetings.space_subscriptions
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE tasks.tasks ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks.tasks FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON tasks.tasks
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE knowledge.chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge.chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON knowledge.chunks
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE agents.runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents.runs FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON agents.runs
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE agents.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents.sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON agents.sessions
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE agents.session_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents.session_events FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON agents.session_events
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE platform.ref_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.ref_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON platform.ref_counters
    USING (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

ALTER TABLE identity.workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.workspaces FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON identity.workspaces
    USING (id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid)
    WITH CHECK (id = NULLIF(current_setting('kiwi.workspace_id', true), '')::uuid);

-- kiwi_app: what the services connect as. DML on the domain schemas; the
-- platform schema only as the spec lists it.
GRANT USAGE ON SCHEMA identity, chat, files, meetings, tasks, knowledge, agents, platform TO kiwi_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity, chat, files, meetings, tasks, knowledge, agents TO kiwi_app;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA agents TO kiwi_app;
GRANT SELECT, INSERT, UPDATE ON platform.ref_counters TO kiwi_app;
GRANT INSERT ON platform.outbox TO kiwi_app;
GRANT USAGE ON SEQUENCE platform.outbox_id_seq TO kiwi_app;
GRANT SELECT, INSERT ON platform.processed_events TO kiwi_app;

-- kiwi_system: BYPASSRLS, reachable only through db.System. DML on the domain
-- schemas and the REF counters (it already sees every tenant), and the relay's
-- SELECT/UPDATE on the outbox; nothing else in platform.
GRANT USAGE ON SCHEMA identity, chat, files, meetings, tasks, knowledge, agents, platform TO kiwi_system;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity, chat, files, meetings, tasks, knowledge, agents TO kiwi_system;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA agents TO kiwi_system;
GRANT SELECT, INSERT, UPDATE ON platform.ref_counters TO kiwi_system;
GRANT SELECT, UPDATE ON platform.outbox TO kiwi_system;

-- kiwi_ro: realtime membership checks, still bound by RLS.
GRANT USAGE ON SCHEMA chat TO kiwi_ro;
GRANT SELECT ON chat.channels, chat.channel_members TO kiwi_ro;

-- Tables and sequences added by later migrations (run as kiwi_owner) get the
-- same grants; the catalog test still fails any that forgets its policy.
ALTER DEFAULT PRIVILEGES IN SCHEMA identity, chat, files, meetings, tasks, knowledge, agents
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kiwi_app, kiwi_system;
ALTER DEFAULT PRIVILEGES IN SCHEMA identity, chat, files, meetings, tasks, knowledge, agents
    GRANT USAGE ON SEQUENCES TO kiwi_app, kiwi_system;
