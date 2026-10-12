// Package blob is the object-storage port. Clients upload and download straight
// through short-lived signed URLs; the backend reads and deletes objects itself.
package blob

import (
	"context"
	"io"
	"time"
)

// Attrs describes a stored object.
type Attrs struct {
	Size        int64
	ContentType string
}

// Store keeps uploaded files and recordings under opaque keys. Head and Read of a key
// that does not exist return an error wrapping fs.ErrNotExist, so callers test it with
// errors.Is(err, fs.ErrNotExist); Delete of such a key returns nil.
type Store interface {
	// SignedPut returns the URL and the headers a client must send to upload one
	// object under key; the upload is bound to contentType and refused above
	// maxBytes, and the URL expires after ttl.
	SignedPut(ctx context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (url string, headers map[string]string, err error)
	// SignedGet returns a URL that downloads the object under key until ttl passes.
	SignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Head returns the size and content type of the object under key.
	Head(ctx context.Context, key string) (Attrs, error)
	// Read opens the object under key; the caller closes it.
	Read(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the object under key.
	Delete(ctx context.Context, key string) error
	// URI returns the provider's own address of key (gs://bucket/key on GCS) for APIs
	// that read the object themselves, or "" on stores that have none (S3, the local
	// file store).
	URI(key string) string
}
