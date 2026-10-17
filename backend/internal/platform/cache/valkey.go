package cache

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey is the Valkey adapter: a connection pool plus the lifetimes it applies. It
// keeps the sessions as well, which the memory adapter does not. It is safe for
// concurrent use.
type Valkey struct {
	rdb *redis.Client
	opt Options
	now func() time.Time // replaced by tests to move the absolute session cap
}

var (
	_ Cache    = (*Valkey)(nil)
	_ Sessions = (*Valkey)(nil)
)

// OpenValkey connects to the Valkey at url (redis:// or rediss://, as
// config.Config.ValkeyURL) and checks that it answers.
func OpenValkey(ctx context.Context, rawURL string, opt Options) (*Valkey, error) {
	ro, err := redis.ParseURL(rawURL)
	if err != nil {
		// url.Error quotes the whole URL, password included: keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("cache: parse valkey url: %w", err)
	}
	opt = opt.WithDefaults()

	c := &Valkey{rdb: redis.NewClient(ro), opt: opt, now: time.Now}
	if err := c.Ping(ctx); err != nil {
		_ = c.rdb.Close()
		return nil, err
	}
	return c, nil
}

// Ping checks that Valkey answers, for readiness probes.
func (c *Valkey) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: ping valkey: %w", err)
	}
	return nil
}

// Close releases the connection pool. Close every Subscription first.
func (c *Valkey) Close() error { return c.rdb.Close() }
