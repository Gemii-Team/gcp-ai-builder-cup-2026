-- Queries of the agents domain (sqlc generates internal/agents/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security binds
-- each statement to one workspace, so no query names a workspace except to write it.
-- agents.sessions.id is the primary key by itself (the ADK addresses a session by its
-- id); the ids are built from a user's UUID, which no two workspaces share.

-- name: InsertQueuedRun :one
-- The run of an Ask-panel question, waiting for agent.invoke.
INSERT INTO agents.runs (id, workspace_id, agent, channel_id, question, on_behalf_of, status)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(agent), sqlc.narg(channel_id), sqlc.arg(question), sqlc.arg(on_behalf_of), 'queued')
RETURNING *;

-- name: UpsertRunningRun :one
-- Starts, or restarts after a failed delivery, the run agent.invoke names. An Ask-panel
-- run is already there and keeps its question and its owner; a mention run is written
-- from the event. A run that has ended stays as it ended.
INSERT INTO agents.runs (id, workspace_id, agent, channel_id, trigger_message_id, question, on_behalf_of, status)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(agent), sqlc.narg(channel_id), sqlc.narg(trigger_message_id), sqlc.arg(question), sqlc.arg(on_behalf_of), 'running')
ON CONFLICT (id) DO UPDATE SET
    status     = CASE WHEN agents.runs.status IN ('done', 'failed') THEN agents.runs.status ELSE 'running' END,
    updated_at = now()
RETURNING *;

-- name: GetRun :one
SELECT * FROM agents.runs WHERE id = sqlc.arg(id);

-- name: CompleteRun :execrows
-- Ends a run with its answer. ref and result are null for a run that produced no slip.
-- A run that already ended is left as it ended and nothing is affected, so a delivery that
-- lost a race cannot overwrite the REF and the answer of the one that won; the caller
-- treats that as the end of its own attempt.
UPDATE agents.runs
SET status        = 'done',
    failure_reason = NULL,
    ref           = sqlc.narg(ref),
    result        = sqlc.narg(result),
    input_tokens  = sqlc.arg(input_tokens),
    output_tokens = sqlc.arg(output_tokens),
    updated_at    = now()
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running');

-- name: AppendToolCall :execrows
-- Records one tool call of a run, as it ends, so that the calls of a run are known to its
-- later attempts (they share one budget) and survive an attempt that never finished.
-- call is a JSON array of the one call.
UPDATE agents.runs
SET tool_calls = tool_calls || sqlc.arg(call)::jsonb,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running');

-- name: FailRun :execrows
-- Ends a run that will not be tried again. A run that already ended keeps its ending.
UPDATE agents.runs
SET status         = 'failed',
    failure_reason = sqlc.arg(failure_reason),
    updated_at     = now()
WHERE id = sqlc.arg(id) AND status IN ('queued', 'running');

-- name: InsertSession :exec
-- A session is created once; a second create of the same one finds it there.
INSERT INTO agents.sessions (id, workspace_id, app_name, user_id, state)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(app_name), sqlc.arg(user_id), sqlc.arg(state))
ON CONFLICT (id) DO NOTHING;

-- name: GetSession :one
-- A session is found only by the app and the user it belongs to.
SELECT * FROM agents.sessions
WHERE id = sqlc.arg(id) AND app_name = sqlc.arg(app_name) AND user_id = sqlc.arg(user_id);

-- name: ListSessions :many
SELECT * FROM agents.sessions
WHERE app_name = sqlc.arg(app_name) AND (sqlc.narg(user_id)::text IS NULL OR user_id = sqlc.narg(user_id)::text::uuid)
ORDER BY created_at, id;

-- name: DeleteSession :exec
DELETE FROM agents.sessions
WHERE id = sqlc.arg(id) AND app_name = sqlc.arg(app_name) AND user_id = sqlc.arg(user_id);

-- name: ListRecentSessionEvents :many
-- The newest max_events events of a session, oldest first.
SELECT id, event FROM (
    SELECT e.id, e.event
    FROM agents.session_events e
    WHERE e.session_id = sqlc.arg(session_id)
    ORDER BY e.id DESC
    LIMIT sqlc.arg(max_events)::int
) newest
ORDER BY id;

-- name: InsertSessionEvent :exec
INSERT INTO agents.session_events (session_id, workspace_id, event)
VALUES (sqlc.arg(session_id), sqlc.arg(workspace_id), sqlc.arg(event));

-- name: TouchSession :execrows
-- Merges a state delta into the session's state and notes the time of its last event.
UPDATE agents.sessions
SET state      = state || sqlc.arg(state_delta)::jsonb,
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: SetRunQuestion :exec
-- Records what a mention run answers, which is known only after the message that
-- started it was read.
UPDATE agents.runs SET question = sqlc.arg(question), updated_at = now() WHERE id = sqlc.arg(id);
