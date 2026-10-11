package cache

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func membersKey(channelID uuid.UUID) string { return "members:" + channelID.String() }

// Members returns the cached member ids of a channel. hit is false when nothing
// is cached, and the caller reads Postgres and calls SetMembers. A cached empty
// set is stored as no key, so it also reads as a miss.
func (c *Cache) Members(ctx context.Context, channelID uuid.UUID) (ids []uuid.UUID, hit bool, err error) {
	vals, err := c.rdb.SMembers(ctx, membersKey(channelID)).Result()
	if err != nil {
		return nil, false, fmt.Errorf("cache: read members: %w", err)
	}
	if len(vals) == 0 {
		return nil, false, nil
	}
	ids = make([]uuid.UUID, len(vals))
	for i, v := range vals {
		if ids[i], err = uuid.Parse(v); err != nil {
			return nil, false, fmt.Errorf("cache: members of %s hold %q: %w", channelID, v, err)
		}
	}
	return ids, true, nil
}

// SetMembers replaces the cached member set of a channel atomically.
func (c *Cache) SetMembers(ctx context.Context, channelID uuid.UUID, ids []uuid.UUID) error {
	key := membersKey(channelID)
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, key)
		if len(ids) > 0 {
			vals := make([]any, len(ids))
			for i, id := range ids {
				vals[i] = id.String()
			}
			p.SAdd(ctx, key, vals...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: set members: %w", err)
	}
	return nil
}

// InvalidateMembers drops the cached member set, so the next read goes to
// Postgres. Call it on every membership change.
func (c *Cache) InvalidateMembers(ctx context.Context, channelID uuid.UUID) error {
	if err := c.rdb.Del(ctx, membersKey(channelID)).Err(); err != nil {
		return fmt.Errorf("cache: invalidate members: %w", err)
	}
	return nil
}
