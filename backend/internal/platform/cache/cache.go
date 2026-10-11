// Package cache is the Valkey client for ephemeral state: sessions, one-time
// realtime tickets, the channel-membership cache and sharded pub/sub fan-out.
// Nothing durable lives here; losing Valkey costs sessions only (spec §3.3).
//
// Keys follow spec §3.3, where {x} stands for the value of x:
// sess:{id}, rtt:{ticket}, members:{channel}. OAuth login state, which §3.3
// does not list, is oauth:{state}.
package cache

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"
)

// Defaults for the zero fields of Options.
const (
	DefaultSessionSliding  = 7 * 24 * time.Hour
	DefaultSessionAbsolute = 30 * 24 * time.Hour
	DefaultTicketTTL       = 30 * time.Second
)

// oauthStateTTL bounds how long a user may take at the identity provider.
const oauthStateTTL = 10 * time.Minute

// Options tunes lifetimes; a zero field takes its default.
type Options struct {
	// SessionSliding is how long a session lives after its last read.
	SessionSliding time.Duration
	// SessionAbsolute is how long a session lives at most, however often read.
	SessionAbsolute time.Duration
	// TicketTTL is how long a realtime ticket can be redeemed.
	TicketTTL time.Duration
}

// Cache is a Valkey connection pool plus the lifetimes it applies. It is safe
// for concurrent use.
type Cache struct {
	rdb *redis.Client
	opt Options
	now func() time.Time // replaced by tests to move the absolute session cap
}

// Open connects to the Valkey at url (redis:// or rediss://, as
// config.Config.ValkeyURL) and checks that it answers.
func Open(ctx context.Context, rawURL string, opt Options) (*Cache, error) {
	ro, err := redis.ParseURL(rawURL)
	if err != nil {
		// url.Error quotes the whole URL, password included: keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("cache: parse valkey url: %w", err)
	}
	if opt.SessionSliding <= 0 {
		opt.SessionSliding = DefaultSessionSliding
	}
	if opt.SessionAbsolute <= 0 {
		opt.SessionAbsolute = DefaultSessionAbsolute
	}
	if opt.TicketTTL <= 0 {
		opt.TicketTTL = DefaultTicketTTL
	}

	c := &Cache{rdb: redis.NewClient(ro), opt: opt, now: time.Now}
	if err := c.Ping(ctx); err != nil {
		_ = c.rdb.Close()
		return nil, err
	}
	return c, nil
}

// Ping checks that Valkey answers, for readiness probes.
func (c *Cache) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: ping valkey: %w", err)
	}
	return nil
}

// Close releases the connection pool. Close every Subscription first.
func (c *Cache) Close() error { return c.rdb.Close() }
