-- Queries of the chat domain (sqlc generates internal/chat/internal/store).
--
-- Everything runs as kiwi_app inside db.InWorkspace, where row-level security
-- binds each statement to one workspace. The two member listings join
-- identity.users read-only: the composite foreign key already ties a member to a
-- user of the same workspace, and the member pickers and mentions need the name.

-- name: InsertChannel :one
INSERT INTO chat.channels (id, workspace_id, name, kind)
VALUES ($1, $2, $3, $4)
RETURNING id, workspace_id, name, kind, last_seq, created_at;

-- name: GetChannel :one
SELECT id, workspace_id, name, kind, last_seq, created_at
FROM chat.channels
WHERE id = $1;

-- name: ListVisibleChannels :many
-- Public channels, and the private ones the user belongs to.
SELECT c.id, c.workspace_id, c.name, c.kind, c.last_seq, c.created_at
FROM chat.channels c
WHERE c.kind = 'public'
   OR EXISTS (
       SELECT 1
       FROM chat.channel_members m
       WHERE m.channel_id = c.id AND m.user_id = sqlc.arg(user_id)
   )
ORDER BY c.name, c.id;

-- name: LockChannel :one
-- Serialises the writers of one channel until the transaction ends, so a retry
-- that raced its original finds the original instead of burning a seq.
SELECT id
FROM chat.channels
WHERE id = $1
FOR UPDATE;

-- name: BumpChannelSeq :one
UPDATE chat.channels
SET last_seq = last_seq + 1
WHERE id = $1
RETURNING last_seq;

-- name: InsertMember :execrows
INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role)
VALUES ($1, $2, $3, $4)
ON CONFLICT (channel_id, user_id) DO NOTHING;

-- name: DeleteMember :execrows
DELETE FROM chat.channel_members
WHERE channel_id = $1 AND user_id = $2;

-- name: ListMemberIDs :many
SELECT user_id
FROM chat.channel_members
WHERE channel_id = $1;

-- name: ListMembers :many
SELECT m.user_id, u.name, u.avatar_url, m.role
FROM chat.channel_members m
JOIN identity.users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
WHERE m.channel_id = $1
ORDER BY u.name, m.user_id;

-- name: ListMemberNames :many
-- The members of the channel among user_ids, with the names the directory holds.
-- user_ids is text[]: the pools use pgx's QueryExecModeExec, which sends no parameter
-- types and so cannot encode a []uuid.UUID; the cast happens on the server.
SELECT m.user_id, u.name
FROM chat.channel_members m
JOIN identity.users u ON u.workspace_id = m.workspace_id AND u.id = m.user_id
WHERE m.channel_id = sqlc.arg(channel_id) AND m.user_id = ANY(sqlc.arg(user_ids)::text[]::uuid[]);

-- name: ListMemberChannelIDs :many
SELECT channel_id
FROM chat.channel_members
WHERE workspace_id = $1 AND user_id = $2
ORDER BY channel_id;

-- name: GetMessageByClientID :one
SELECT id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id, created_at
FROM chat.messages
WHERE author_id = $1 AND client_msg_id = $2;

-- name: InsertMessage :one
INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id, created_at;

-- name: ListLatestMessages :many
SELECT id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id, created_at
FROM chat.messages
WHERE channel_id = $1
ORDER BY seq DESC
LIMIT $2;

-- name: ListMessagesBefore :many
SELECT id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id, created_at
FROM chat.messages
WHERE channel_id = $1 AND seq < $2
ORDER BY seq DESC
LIMIT $3;

-- name: ListMessagesBetween :many
-- The messages a client is missing: seq after its cursor, up to the head read in
-- the same transaction, oldest first.
SELECT id, workspace_id, channel_id, seq, author_kind, author_id, body, mentions, attachments, artifact, client_msg_id, created_at
FROM chat.messages
WHERE channel_id = sqlc.arg(channel_id) AND seq > sqlc.arg(after_seq) AND seq <= sqlc.arg(through_seq)
ORDER BY seq ASC;
