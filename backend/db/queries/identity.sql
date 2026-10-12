-- Queries of the identity domain (sqlc generates internal/identity/internal/store).
--
-- Two roles run them. callback.go runs the sign-in queries as kiwi_system inside
-- db.System, which holds only SELECT+INSERT on identity.workspaces and
-- SELECT+INSERT+UPDATE on identity.users, so every System query below stays within
-- that. Everything else runs as kiwi_app inside db.InWorkspace, where row-level
-- security binds it to one workspace.

-- name: FindWorkspaceByDomain :one
-- The workspace of a hosted domain, compared without regard to case.
SELECT id, name, google_domain, created_at
FROM identity.workspaces
WHERE lower(google_domain) = lower(sqlc.arg(domain)::text);

-- name: GetWorkspace :one
SELECT id, name, google_domain, created_at
FROM identity.workspaces
WHERE id = $1;

-- name: ListWorkspaceIDs :many
-- Every workspace id: what an operator command that must visit each tenant (`kiwi
-- reindex`) starts from. A System query that only SELECTs identity.workspaces.
SELECT id FROM identity.workspaces ORDER BY id;

-- name: InsertWorkspaceIfAbsent :exec
-- google_domain is NULL for the one workspace without a domain, whose id is fixed. The
-- conflict is untargeted on purpose: that workspace is found by its fixed id (the
-- primary key) and the others by lower(google_domain) (a partial unique index), and
-- one ON CONFLICT clause cannot name both. The price is that DO NOTHING swallows ANY
-- unique violation of the row, so callers must read the workspace back afterwards and
-- not assume that this statement inserted it.
INSERT INTO identity.workspaces (id, name, google_domain)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: GetUserBySub :one
SELECT id, workspace_id, google_sub, email, name, avatar_url, created_at
FROM identity.users
WHERE google_sub = $1;

-- name: GetUser :one
SELECT id, workspace_id, google_sub, email, name, avatar_url, created_at
FROM identity.users
WHERE id = $1;

-- name: GetUserWorkspace :one
SELECT workspace_id
FROM identity.users
WHERE id = $1;

-- name: GetUserByEmail :one
SELECT id, workspace_id, google_sub, email, name, avatar_url, created_at
FROM identity.users
WHERE workspace_id = $1 AND email = $2;

-- name: ListUsers :many
SELECT id, workspace_id, google_sub, email, name, avatar_url, created_at
FROM identity.users
ORDER BY name, email;

-- name: FindUsersByEmail :many
-- Every user, in any workspace, whose address is email (lowercased by the caller):
-- what the sign-in rule for a token without hd decides on. A System query, since the
-- workspace is not known yet; it reads three columns and writes nothing.
SELECT id, workspace_id, google_sub
FROM identity.users
WHERE lower(email) = sqlc.arg(email)::text;

-- name: InsertSeedUserIfAbsent :exec
INSERT INTO identity.users (id, workspace_id, google_sub, email, name)
VALUES (sqlc.arg(id), sqlc.arg(workspace_id), 'seed:' || sqlc.arg(email)::text, sqlc.arg(email)::text, sqlc.arg(name))
ON CONFLICT (workspace_id, email) DO NOTHING;

-- name: AdoptSeedUser :one
-- Gives the placeholder a seed left behind (google_sub = 'seed:' || email) its real
-- Google subject on the first sign-in. It matches placeholders only.
UPDATE identity.users
SET google_sub = sqlc.arg(google_sub), name = sqlc.arg(name), avatar_url = sqlc.arg(avatar_url)
WHERE workspace_id = sqlc.arg(workspace_id)
  AND email = sqlc.arg(email)
  AND google_sub = 'seed:' || email
RETURNING id, workspace_id, google_sub, email, name, avatar_url, created_at;

-- name: LockSignIn :exec
-- Serialises the sign-ins of one Google account for the rest of the transaction:
-- two first sign-ins of the same person would otherwise both look for a user,
-- find none and both insert one (the second then fails on the unique email).
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(google_sub)::text, 0));

-- name: InsertUser :one
INSERT INTO identity.users (id, workspace_id, google_sub, email, name, avatar_url)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, workspace_id, google_sub, email, name, avatar_url, created_at;

-- name: UpdateUserProfile :one
UPDATE identity.users
SET email = $2, name = $3, avatar_url = $4
WHERE id = $1
RETURNING id, workspace_id, google_sub, email, name, avatar_url, created_at;

-- name: GetOAuthToken :one
SELECT user_id, workspace_id, refresh_token_enc, scopes, updated_at
FROM identity.oauth_tokens
WHERE user_id = $1;

-- name: ReplaceOAuthToken :exec
-- A refresh token Google just issued is a new grant: it and the scopes it covers
-- replace whatever was stored, including scopes of a grant since revoked.
INSERT INTO identity.oauth_tokens (user_id, workspace_id, refresh_token_enc, scopes)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
SET refresh_token_enc = EXCLUDED.refresh_token_enc, scopes = EXCLUDED.scopes, updated_at = now();

-- name: MergeOAuthScopes :exec
-- Consent without a new refresh token widens the grant already stored: scopes
-- are unioned, sorted bytewise and never duplicated.
UPDATE identity.oauth_tokens
SET scopes = ARRAY(
        SELECT d.s
        FROM (SELECT DISTINCT unnest(identity.oauth_tokens.scopes || sqlc.arg(scopes)::text[]) AS s) AS d
        ORDER BY d.s COLLATE "C"
    ),
    updated_at = now()
WHERE user_id = $1;
