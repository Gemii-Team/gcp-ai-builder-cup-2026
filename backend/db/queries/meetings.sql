-- Queries of the meetings domain (sqlc generates internal/meetings/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security binds
-- each statement to one workspace, except ListPollableMeetings: the poll enumerates
-- meetings of every workspace through db.System, whose role may only SELECT
-- meetings.meetings, and writes each meeting back through InWorkspace.
--
-- A meeting moves forward only: pending, then ingesting, then ready, failed or
-- no_transcript, and failed goes back to pending (a recording) or ingesting (a Meet
-- transcript) on a retry. Each transition names the statuses it leaves, so a
-- repeated or concurrent one finds no row and the caller sees that the meeting has
-- already moved.

-- name: InsertMeeting :one
-- A Meet meeting has a space and its link, a recording meeting has a file; both start
-- pending.
INSERT INTO meetings.meetings (
    id, workspace_id, channel_id, owner_user_id, ref, title, meet_space, meet_uri, recording_file_id, status
)
VALUES (
    sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(channel_id), sqlc.arg(owner_user_id), sqlc.arg(ref),
    sqlc.arg(title), sqlc.narg(meet_space), sqlc.narg(meet_uri), sqlc.narg(recording_file_id), 'pending'
)
RETURNING *;

-- name: GetMeeting :one
SELECT * FROM meetings.meetings WHERE id = sqlc.arg(id);

-- name: LockMeeting :one
-- Serialises the writers of one meeting until the transaction ends.
SELECT * FROM meetings.meetings WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: LockRecording :exec
-- Serialises the creation of meetings from one recording file until the transaction
-- ends, so a double click makes one meeting, not two.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(recording_key)::text, 0));

-- name: GetMeetingByRecording :one
-- The meeting a recording of a channel was already made into, unless it failed: a failed
-- meeting is retried, and a recording that failed can be made into a new meeting.
SELECT * FROM meetings.meetings
WHERE channel_id = sqlc.arg(channel_id)
  AND recording_file_id = sqlc.arg(recording_file_id)::uuid
  AND status <> 'failed'
ORDER BY created_at, id
LIMIT 1;

-- name: ListPollableMeetings :many
-- The meetings the poll asks Meet about: a space, still waiting for its summary, made
-- since the cut-off. It reads every workspace, so it runs through db.System. It
-- carries what the poll has recorded so far, so a tick that learns nothing new needs
-- no transaction at all.
SELECT id, workspace_id, owner_user_id, meet_space, status, conference_record, started_at, ended_at
FROM meetings.meetings
WHERE meet_space IS NOT NULL
  AND status IN ('pending', 'ingesting')
  AND created_at > sqlc.arg(since)::timestamptz
ORDER BY created_at, id;

-- name: RecordConference :one
-- Remembers the latest conference of the space and its start; the end is kept once
-- it is known (a later poll never moves it).
UPDATE meetings.meetings
SET conference_record = sqlc.arg(conference_record),
    started_at = sqlc.arg(started_at),
    ended_at = COALESCE(ended_at, sqlc.narg(ended_at))
WHERE id = sqlc.arg(id) AND status IN ('pending', 'ingesting')
RETURNING *;

-- name: StartIngest :one
UPDATE meetings.meetings
SET status = 'ingesting'
WHERE id = sqlc.arg(id) AND status = 'pending'
RETURNING *;

-- name: SetLastEntryCount :one
UPDATE meetings.meetings
SET last_entry_count = sqlc.arg(last_entry_count)
WHERE id = sqlc.arg(id) AND status = 'ingesting'
RETURNING *;

-- name: MarkMeetingReady :one
UPDATE meetings.meetings
SET status = 'ready', failure_reason = NULL
WHERE id = sqlc.arg(id) AND status = 'ingesting'
RETURNING *;

-- name: MarkMeetingFailed :one
-- A meeting fails while it waits (pending) or while it is read (ingesting); a ready,
-- failed or transcript-less meeting is left alone.
UPDATE meetings.meetings
SET status = 'failed', failure_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id) AND status IN ('pending', 'ingesting')
RETURNING *;

-- name: MarkMeetingNoTranscript :one
UPDATE meetings.meetings
SET status = 'no_transcript', failure_reason = NULL
WHERE id = sqlc.arg(id) AND status IN ('pending', 'ingesting')
RETURNING *;

-- name: RestartMeeting :one
-- A failed meeting goes back to the status its source starts from, with its read
-- count forgotten so the transcript is judged afresh.
UPDATE meetings.meetings
SET status = sqlc.arg(status), failure_reason = NULL, last_entry_count = NULL
WHERE id = sqlc.arg(id) AND status = 'failed'
RETURNING *;

-- name: GetSummary :one
SELECT * FROM meetings.summaries WHERE meeting_id = sqlc.arg(meeting_id);

-- name: InsertSummary :exec
INSERT INTO meetings.summaries (meeting_id, workspace_id, summary, decisions, open_questions)
VALUES (
    sqlc.arg(meeting_id), sqlc.arg(workspace_id), sqlc.arg(summary),
    sqlc.arg(decisions)::jsonb, sqlc.arg(open_questions)::jsonb
);

-- name: InsertTranscriptEntries :exec
-- entries is a JSON array of {idx, speaker, start_ms, end_ms, text}; a null speaker or
-- timing stays NULL. One statement, whatever the length of the transcript.
INSERT INTO meetings.transcript_entries (meeting_id, workspace_id, idx, speaker, start_ms, end_ms, text)
SELECT sqlc.arg(meeting_id), sqlc.arg(workspace_id), e.idx, e.speaker, e.start_ms, e.end_ms, e.text
FROM jsonb_to_recordset(sqlc.arg(entries)::jsonb)
    AS e(idx int, speaker text, start_ms bigint, end_ms bigint, text text);

-- name: ListTranscriptEntries :many
SELECT * FROM meetings.transcript_entries WHERE meeting_id = sqlc.arg(meeting_id) ORDER BY idx;

-- name: ListTranscriptWindow :many
SELECT * FROM meetings.transcript_entries
WHERE meeting_id = sqlc.arg(meeting_id) AND idx BETWEEN sqlc.arg(lo) AND sqlc.arg(hi)
ORDER BY idx;
