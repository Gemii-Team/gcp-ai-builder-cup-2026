-- +goose Up

-- A workspace may have no Google domain. A team whose Google accounts carry no
-- hd claim (personal addresses) has one workspace without a domain, which
-- identity.EnsureSeedWorkspace creates; its members sign in by invitation.
-- Domains stay unique, now whatever their letter case, because sign-in compares
-- lower(hd) with lower(google_domain). Expand-only: every reader and writer of the
-- column from before keeps working.

ALTER TABLE identity.workspaces ALTER COLUMN google_domain DROP NOT NULL;
ALTER TABLE identity.workspaces DROP CONSTRAINT workspaces_google_domain_key;
CREATE UNIQUE INDEX workspaces_google_domain_lower_key
    ON identity.workspaces (lower(google_domain))
    WHERE google_domain IS NOT NULL;

-- +goose Down

-- Fails while a workspace without a domain exists (SET NOT NULL refuses it), and
-- is meant to: inventing a domain for it would be worse than stopping.
DROP INDEX identity.workspaces_google_domain_lower_key;
ALTER TABLE identity.workspaces ADD CONSTRAINT workspaces_google_domain_key UNIQUE (google_domain);
ALTER TABLE identity.workspaces ALTER COLUMN google_domain SET NOT NULL;
