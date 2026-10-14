package cache

import (
	"context"
	"fmt"

	"kiwi/internal/platform/config"
)

// Open returns the cache cfg.CacheProvider selects: the Valkey at cfg.ValkeyURL (which
// it dials, so it fails at once when Valkey cannot be reached) or an in-process one.
// An empty provider, a Config that did not come from config.Load, is Valkey, which is
// what every process used before the provider was configurable. The caller closes it.
//
// Sessions are not part of Cache. When the cache is a Sessions as well (Valkey), it
// keeps them; otherwise the caller keeps them elsewhere: see app.New.
func Open(ctx context.Context, cfg config.Config) (Cache, error) {
	switch cfg.CacheProvider {
	case config.CacheMemory:
		return NewMemory(Options{}), nil
	case config.CacheValkey, "":
		v, err := OpenValkey(ctx, cfg.ValkeyURL, Options{})
		if err != nil {
			return nil, fmt.Errorf("KIWI_VALKEY_URL: %w", err)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("KIWI_CACHE_PROVIDER=%s is not built (valkey or memory)", cfg.CacheProvider)
	}
}
