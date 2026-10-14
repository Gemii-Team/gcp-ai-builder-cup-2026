package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// membersBackstopTTL bounds how long a set written by SetMembersAtEpoch can live.
// The epoch is what keeps a stale fill out; the TTL only caps the damage if the
// epoch is ever lost (Valkey restarted or failed over between a reader's sample
// and its fill), so an entry that should not exist disappears by itself.
const membersBackstopTTL = 10 * time.Minute

func membersKey(channelID uuid.UUID) string { return "members:" + channelID.String() }

// membersEpochKey counts the invalidations of a channel's member set.
func membersEpochKey(channelID uuid.UUID) string { return membersKey(channelID) + ":epoch" }

// Members returns the cached member ids of a channel. hit is false when nothing
// is cached, and the caller reads Postgres and fills the set with
// SetMembersAtEpoch. A cached empty set is stored as no key, so it also reads as
// a miss.
func (c *Valkey) Members(ctx context.Context, channelID uuid.UUID) (ids []uuid.UUID, hit bool, err error) {
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

// SetMembers replaces the cached member set of a channel atomically and
// unconditionally. A reader that filled the cache from Postgres must use
// SetMembersAtEpoch instead: this write, landing after an InvalidateMembers, would
// bring back a member the invalidation removed.
func (c *Valkey) SetMembers(ctx context.Context, channelID uuid.UUID, ids []uuid.UUID) error {
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

// MembersEpoch returns how many times the channel's member set has been
// invalidated (0 for a channel never invalidated). A cache-aside reader samples it
// before it reads Postgres and hands it to SetMembersAtEpoch with the result.
func (c *Valkey) MembersEpoch(ctx context.Context, channelID uuid.UUID) (int64, error) {
	n, err := c.rdb.Get(ctx, membersEpochKey(channelID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cache: read members epoch: %w", err)
	}
	return n, nil
}

// setMembersAtEpoch compares and sets in one step on the server: KEYS[1] is the
// set, KEYS[2] the epoch, ARGV[1] the epoch the reader sampled, ARGV[2] the TTL in
// milliseconds and ARGV[3..] the members. It replies 1 when the set was written
// (or empty and nothing needed writing) and 0 when the epoch had moved on.
var setMembersAtEpoch = redis.NewScript(`
local current = redis.call('GET', KEYS[2])
if not current then current = '0' end
if current ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
for i = 3, #ARGV do redis.call('SADD', KEYS[1], ARGV[i]) end
if #ARGV >= 3 then redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
return 1`)

// SetMembersAtEpoch replaces the cached member set with ids, but only if no
// InvalidateMembers happened since the reader sampled epoch (MembersEpoch) before
// reading them from Postgres; otherwise the reader's data may predate a membership
// change and is dropped. stored reports whether the set was written (an empty ids
// writes nothing and counts as stored when the epoch still matches). The set
// expires after a backstop TTL; see membersBackstopTTL.
//
// The set and the epoch are two keys read by one server-side script, so this
// assumes a single Valkey, as the rest of the package does.
func (c *Valkey) SetMembersAtEpoch(ctx context.Context, channelID uuid.UUID, epoch int64, ids []uuid.UUID) (stored bool, err error) {
	args := make([]any, 0, 2+len(ids))
	args = append(args, epoch, membersBackstopTTL.Milliseconds())
	for _, id := range ids {
		args = append(args, id.String())
	}
	n, err := setMembersAtEpoch.Run(ctx, c.rdb, []string{membersKey(channelID), membersEpochKey(channelID)}, args...).Int()
	if err != nil {
		return false, fmt.Errorf("cache: set members at epoch: %w", err)
	}
	return n == 1, nil
}

// InvalidateMembers drops the cached member set and advances the channel's epoch
// in one transaction, so the next read goes to Postgres and any fill still in
// flight from before this call is refused. Call it on every membership change,
// after the change has committed.
func (c *Valkey) InvalidateMembers(ctx context.Context, channelID uuid.UUID) error {
	_, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, membersKey(channelID))
		p.Incr(ctx, membersEpochKey(channelID))
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: invalidate members: %w", err)
	}
	return nil
}
