package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"kiwi/internal/platform/errs"
)

func sessionKey(id string) string   { return "sess:" + id }
func oauthStateKey(s string) string { return "oauth:" + s }
func (c *Valkey) sessionTTL(createdAt time.Time) time.Duration {
	return min(c.opt.SessionSliding, c.opt.SessionAbsolute-c.now().Sub(createdAt))
}

// PutSession stores the session under id for the sliding lifetime, shortened to
// what is left of the absolute one.
func (c *Valkey) PutSession(ctx context.Context, id string, s SessionRecord) error {
	if id == "" {
		return fmt.Errorf("cache: empty session id: %w", errs.ErrInvalid)
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = c.now()
	}
	s.CreatedAt = s.CreatedAt.UTC()
	ttl := c.sessionTTL(s.CreatedAt)
	if ttl < time.Millisecond {
		return fmt.Errorf("cache: session created at %s is past its absolute lifetime: %w", s.CreatedAt.Format(time.RFC3339), errs.ErrInvalid)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("cache: encode session: %w", err)
	}
	if err := c.rdb.Set(ctx, sessionKey(id), raw, ttl).Err(); err != nil {
		return fmt.Errorf("cache: put session: %w", err)
	}
	return nil
}

// GetSession returns the session and slides its lifetime forward, never past
// the absolute cap. A missing, expired or capped session is ErrNotFound.
func (c *Valkey) GetSession(ctx context.Context, id string) (SessionRecord, error) {
	key := sessionKey(id)
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return SessionRecord{}, fmt.Errorf("cache: session: %w", errs.ErrNotFound)
	}
	if err != nil {
		return SessionRecord{}, fmt.Errorf("cache: get session: %w", err)
	}
	var s SessionRecord
	if err := json.Unmarshal(raw, &s); err != nil {
		return SessionRecord{}, fmt.Errorf("cache: decode session: %w", err)
	}

	ttl := c.sessionTTL(s.CreatedAt)
	if ttl < time.Millisecond {
		// The cap is enforced here as well as by the TTL, so a read right at
		// the boundary cannot resurrect the session. A failed DEL changes
		// nothing: the next read finds it capped again and the TTL ends it.
		_ = c.rdb.Del(ctx, key).Err()
		return SessionRecord{}, fmt.Errorf("cache: session: %w", errs.ErrNotFound)
	}
	extended, err := c.rdb.PExpire(ctx, key, ttl).Result()
	if err != nil {
		return SessionRecord{}, fmt.Errorf("cache: extend session: %w", err)
	}
	if !extended { // expired or deleted between the GET and the PEXPIRE
		return SessionRecord{}, fmt.Errorf("cache: session: %w", errs.ErrNotFound)
	}
	return s, nil
}

// DeleteSession removes the session; removing an absent one is not an error.
func (c *Valkey) DeleteSession(ctx context.Context, id string) error {
	if err := c.rdb.Del(ctx, sessionKey(id)).Err(); err != nil {
		return fmt.Errorf("cache: delete session: %w", err)
	}
	return nil
}

// PutOAuthState stores the login state for ten minutes.
func (c *Valkey) PutOAuthState(ctx context.Context, state string, v OAuthState) error {
	if state == "" {
		return fmt.Errorf("cache: empty oauth state: %w", errs.ErrInvalid)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache: encode oauth state: %w", err)
	}
	if err := c.rdb.Set(ctx, oauthStateKey(state), raw, oauthStateTTL).Err(); err != nil {
		return fmt.Errorf("cache: put oauth state: %w", err)
	}
	return nil
}

// TakeOAuthState returns the state and deletes it in one step (GETDEL), so a
// replayed callback finds nothing: ErrNotFound.
func (c *Valkey) TakeOAuthState(ctx context.Context, state string) (OAuthState, error) {
	raw, err := c.rdb.GetDel(ctx, oauthStateKey(state)).Bytes()
	if errors.Is(err, redis.Nil) {
		return OAuthState{}, fmt.Errorf("cache: oauth state: %w", errs.ErrNotFound)
	}
	if err != nil {
		return OAuthState{}, fmt.Errorf("cache: take oauth state: %w", err)
	}
	var v OAuthState
	if err := json.Unmarshal(raw, &v); err != nil {
		return OAuthState{}, fmt.Errorf("cache: decode oauth state: %w", err)
	}
	return v, nil
}
