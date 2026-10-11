package blob

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"

	"kiwi/internal/platform/config"
)

// Open returns the object store that cfg.BlobURL selects. Only file:///<absolute
// path> is built in the local MVP; it returns a *Local, which also serves the signed
// URLs as an http.Handler. Any other scheme is refused without echoing the URL, since
// it may carry credentials.
func Open(_ context.Context, cfg config.Config) (Store, error) {
	u, err := url.Parse(cfg.BlobURL)
	if err != nil {
		return nil, errors.New("blob: the blob URL is not a valid URL")
	}
	if u.Scheme != "file" {
		return nil, fmt.Errorf("blob: scheme %q is not built in the local MVP (only file:///<absolute path>)", u.Scheme)
	}
	if u.Host != "" || !filepath.IsAbs(u.Path) {
		return nil, errors.New("blob: the file blob URL must be file:///<absolute path>")
	}
	return newLocal(u.Path, cfg.BlobSigningKey, cfg.PublicURL)
}
