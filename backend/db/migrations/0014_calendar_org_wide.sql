-- +goose Up

-- org_wide marks an event the whole workspace sees, not only the channel's (team's)
-- members. The channel still hosts the invitation slip; this only widens who may read
-- the event in ListUpcoming. Existing events are team events.
ALTER TABLE calendar.events ADD COLUMN org_wide boolean NOT NULL DEFAULT false;

-- The workspace-wide list reads org-wide events across every channel, soonest first,
-- so it is indexed by (workspace, org_wide, start) alongside the per-channel index.
CREATE INDEX events_org_wide_start_idx ON calendar.events (workspace_id, start_at)
    WHERE org_wide;

-- +goose Down

DROP INDEX calendar.events_org_wide_start_idx;
ALTER TABLE calendar.events DROP COLUMN org_wide;
