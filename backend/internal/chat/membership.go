package chat

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/chat/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
)

// membership answers "is this user in this channel" from members:{channel} in
// Valkey and, on a miss, from Postgres.
//
// The cache is cache-aside with no write-through, so the one dangerous interleaving
// is a reader that read Postgres before a membership change committed and fills the
// cache after the change invalidated it: the removed user would be a member again
// until the next change. IsMember closes it with the channel's epoch: it samples
// the epoch before the Postgres read and the fill is refused if an invalidation
// (which advances the epoch) happened since. A change commits before it invalidates,
// so any read that began after the commit sees the change, and any read that began
// before it is refused its fill.
type membership struct {
	cache *cache.Cache
	// load reads a channel's member ids from Postgres in workspace ws. It is a field
	// so a test can stretch the window between that read and the cache fill.
	load func(ctx context.Context, ws, channelID uuid.UUID) ([]uuid.UUID, error)
}

// NewMembership returns the authz.Membership the realtime gateway and the services
// use: a cache-aside over db.InWorkspace on members:{channel}. The context of every
// lookup must carry the workspace (authz.WithWorkspace); authz.Authz does that for
// the actor it checks.
func NewMembership(d *db.DB, c *cache.Cache) authz.Membership { return newMembership(d, c) }

func newMembership(d *db.DB, c *cache.Cache) *membership {
	return &membership{
		cache: c,
		load: func(ctx context.Context, ws, channelID uuid.UUID) (ids []uuid.UUID, err error) {
			err = d.InWorkspace(ctx, ws, func(ctx context.Context, tx pgx.Tx) (err error) {
				ids, err = store.New(tx).ListMemberIDs(ctx, channelID)
				return err
			})
			return ids, err
		},
	}
}

// IsMember implements authz.Membership. A cache that cannot be read or filled does
// not change the answer: Postgres decides, and the failure is logged.
func (m *membership) IsMember(ctx context.Context, channelID, userID uuid.UUID) (bool, error) {
	ws := authz.WorkspaceFrom(ctx)
	if ws == uuid.Nil {
		return false, fmt.Errorf("chat: membership lookup without a workspace in the context: %w", errs.ErrInvalid)
	}

	ids, hit, err := m.cache.Members(ctx, channelID)
	if err != nil {
		slog.Warn("chat: members cache unreadable, reading Postgres", "channel_id", channelID, "error", err)
	} else if hit {
		return slices.Contains(ids, userID), nil
	}

	// Sampled before the read below: see the type comment.
	epoch, epochErr := m.cache.MembersEpoch(ctx, channelID)
	if epochErr != nil && err == nil {
		slog.Warn("chat: members cache epoch unreadable, not filling the cache", "channel_id", channelID, "error", epochErr)
	}
	if ids, err = m.load(ctx, ws, channelID); err != nil {
		return false, fmt.Errorf("chat: read members of %s: %w", channelID, err)
	}
	if epochErr == nil && len(ids) > 0 {
		if _, err := m.cache.SetMembersAtEpoch(ctx, channelID, epoch, ids); err != nil {
			slog.Warn("chat: members cache not filled", "channel_id", channelID, "error", err)
		}
	}
	return slices.Contains(ids, userID), nil
}
