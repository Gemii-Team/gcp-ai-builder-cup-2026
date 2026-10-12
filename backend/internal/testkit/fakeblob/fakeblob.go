// Package fakeblob is an in-memory blob.Store for tests. There is no HTTP behind the
// signed URLs: tests put objects straight into the store with Put, as a client's
// upload would, and inspect the URLs that were asked for with Grants. All methods are
// safe for concurrent use.
package fakeblob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"slices"
	"sync"
	"time"

	"kiwi/internal/platform/blob"
)

// Grant records one signed URL that was requested.
type Grant struct {
	// Method is "PUT" for SignedPut and "GET" for SignedGet.
	Method      string
	Key         string
	ContentType string // SignedPut only
	MaxBytes    int64  // SignedPut only
	TTL         time.Duration
}

type object struct {
	data        []byte
	contentType string
}

// Store implements blob.Store.
type Store struct {
	mu      sync.Mutex
	objects map[string]object
	grants  []Grant
}

var _ blob.Store = (*Store)(nil)

// New returns an empty Store.
func New() *Store {
	return &Store{objects: map[string]object{}}
}

// Put stores data under key as an upload of contentType would.
func (s *Store) Put(key, contentType string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = object{data: slices.Clone(data), contentType: contentType}
}

// Grants returns every signed URL requested so far, in order.
func (s *Store) Grants() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants)
}

// SignedPut implements blob.Store. The URL is a stub that nothing serves.
func (s *Store) SignedPut(_ context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (string, map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, Grant{Method: "PUT", Key: key, ContentType: contentType, MaxBytes: maxBytes, TTL: ttl})
	return stubURL("put", key), map[string]string{"Content-Type": contentType}, nil
}

// SignedGet implements blob.Store. The URL is a stub that nothing serves.
func (s *Store) SignedGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = append(s.grants, Grant{Method: "GET", Key: key, TTL: ttl})
	return stubURL("get", key), nil
}

// Head implements blob.Store; a missing key wraps fs.ErrNotExist.
func (s *Store) Head(_ context.Context, key string) (blob.Attrs, error) {
	o, err := s.get(key)
	if err != nil {
		return blob.Attrs{}, err
	}
	return blob.Attrs{Size: int64(len(o.data)), ContentType: o.contentType}, nil
}

// Read implements blob.Store; a missing key wraps fs.ErrNotExist.
func (s *Store) Read(_ context.Context, key string) (io.ReadCloser, error) {
	o, err := s.get(key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

// Delete implements blob.Store; a missing key is not an error.
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

// URI implements blob.Store; like the local file store it has no provider address.
func (s *Store) URI(string) string { return "" }

// get returns the stored object. Its data is never modified in place, so callers may
// read it after the lock is released.
func (s *Store) get(key string) (object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return object{}, fmt.Errorf("fakeblob: %q: %w", key, fs.ErrNotExist)
	}
	return o, nil
}

func stubURL(op, key string) string {
	return "https://fakeblob.invalid/" + op + "/" + url.PathEscape(key)
}
