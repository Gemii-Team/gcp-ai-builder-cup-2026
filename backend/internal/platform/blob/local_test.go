package blob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/errs"
)

const publicURL = "https://kiwi.test"

// clock is a settable time source, so expiry tests never sleep.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a Local on a temp dir behind a real HTTP server.
type fixture struct {
	*Local
	dir string
	srv *httptest.Server
	clk *clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(t.Context(), localConfig(t, dir))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	l, ok := st.(*Local)
	if !ok {
		t.Fatalf("Open returned %T, want *Local", st)
	}
	t.Cleanup(func() { _ = l.Close() })
	clk := &clock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	l.now = clk.Now
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	return &fixture{Local: l, dir: dir, srv: srv, clk: clk}
}

// localConfig returns a file-store config with a fresh random signing key.
func localConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return config.Config{
		BlobURL:        (&url.URL{Scheme: "file", Path: dir}).String(),
		BlobSigningKey: key,
		PublicURL:      publicURL,
	}
}

// do sends a request to signedURL, which was built on publicURL, as a browser would
// against the same origin. A nil header map and nil body mean none.
func (f *fixture) do(t *testing.T, method, signedURL string, headers map[string]string, body io.Reader) *http.Response {
	t.Helper()
	target, ok := strings.CutPrefix(signedURL, publicURL)
	if !ok {
		t.Fatalf("url %q is not under %s", signedURL, publicURL)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+target, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (f *fixture) signedPut(t *testing.T, key, contentType string, maxBytes int64, ttl time.Duration) (string, map[string]string) {
	t.Helper()
	u, h, err := f.SignedPut(t.Context(), key, contentType, maxBytes, ttl)
	if err != nil {
		t.Fatalf("SignedPut: %v", err)
	}
	return u, h
}

func (f *fixture) signedGet(t *testing.T, key string, ttl time.Duration) string {
	t.Helper()
	u, err := f.SignedGet(t.Context(), key, ttl)
	if err != nil {
		t.Fatalf("SignedGet: %v", err)
	}
	return u
}

// upload stores data under key through a signed PUT, as a client does.
func (f *fixture) upload(t *testing.T, key, contentType string, data []byte) {
	t.Helper()
	u, h := f.signedPut(t, key, contentType, int64(len(data))+1, time.Minute)
	wantStatus(t, f.do(t, http.MethodPut, u, h, bytes.NewReader(data)), http.StatusOK)
}

func (f *fixture) wantNotExist(t *testing.T, key string) {
	t.Helper()
	if _, err := f.Head(t.Context(), key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Head(%q) error = %v, want fs.ErrNotExist", key, err)
	}
}

// wantEmptyDir fails if the store left any file behind, for example a temp file of an
// aborted upload.
func (f *fixture) wantEmptyDir(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Errorf("unexpected file left in the store: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, want, body)
	}
}

func TestLocal_SignedPutThenHeadReadDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const key = "uploads/ws-1/file-1"
	payload := []byte("%PDF-1.7 hello")

	u, headers := f.signedPut(t, key, "application/pdf", 1<<20, time.Minute)
	if !strings.HasPrefix(u, publicURL+"/api/blob/") {
		t.Fatalf("SignedPut url = %q, want prefix %s/api/blob/", u, publicURL)
	}
	if len(headers) != 1 || headers["Content-Type"] != "application/pdf" {
		t.Fatalf("SignedPut headers = %v, want only Content-Type: application/pdf", headers)
	}
	f.wantNotExist(t, key)
	if _, err := f.Read(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read before upload error = %v, want fs.ErrNotExist", err)
	}

	wantStatus(t, f.do(t, http.MethodPut, u, headers, bytes.NewReader(payload)), http.StatusOK)

	attrs, err := f.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if attrs.Size != int64(len(payload)) || attrs.ContentType != "application/pdf" {
		t.Fatalf("Head = %+v, want size %d type application/pdf", attrs, len(payload))
	}
	rc, err := f.Read(ctx, key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("Read = %q, %v; want %q", got, err, payload)
	}

	get := f.do(t, http.MethodGet, f.signedGet(t, key, time.Minute), nil, nil)
	wantStatus(t, get, http.StatusOK)
	body, _ := io.ReadAll(get.Body)
	if !bytes.Equal(body, payload) {
		t.Fatalf("GET body = %q, want %q", body, payload)
	}
	if ct := get.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("GET Content-Type = %q, want application/pdf", ct)
	}
	if got := get.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("GET X-Content-Type-Options = %q, want nosniff", got)
	}

	if err := f.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.wantNotExist(t, key)
	if err := f.Delete(ctx, key); err != nil {
		t.Fatalf("Delete of a missing key = %v, want nil", err)
	}
	if got := f.URI(key); got != "" {
		t.Fatalf("URI = %q, want empty", got)
	}
}

func TestLocal_GetOfMissingObject_Is404(t *testing.T) {
	f := newFixture(t)
	wantStatus(t, f.do(t, http.MethodGet, f.signedGet(t, "uploads/ws-1/none", time.Minute), nil, nil), http.StatusNotFound)
}

func TestLocal_PutWithDifferentContentTypeRejected(t *testing.T) {
	f := newFixture(t)
	const key = "uploads/ws-1/file-2"
	u, _ := f.signedPut(t, key, "application/pdf", 1<<20, time.Minute)

	for name, headers := range map[string]map[string]string{
		"other type": {"Content-Type": "text/html"},
		"no type":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, f.do(t, http.MethodPut, u, headers, strings.NewReader("<script>")), http.StatusUnsupportedMediaType)
			f.wantNotExist(t, key)
			f.wantEmptyDir(t)
		})
	}
}

func TestLocal_PutOverMaxBytesRejected_NoObjectPersisted(t *testing.T) {
	const (
		key   = "uploads/ws-1/file-3"
		limit = 1 << 10
	)
	big := bytes.Repeat([]byte{'x'}, 30<<10)

	t.Run("declared length", func(t *testing.T) {
		f := newFixture(t)
		u, h := f.signedPut(t, key, "application/pdf", limit, time.Minute)
		wantStatus(t, f.do(t, http.MethodPut, u, h, bytes.NewReader(big)), http.StatusRequestEntityTooLarge)
		f.wantNotExist(t, key)
		f.wantEmptyDir(t)
	})

	t.Run("chunked body of unknown length", func(t *testing.T) {
		f := newFixture(t)
		u, h := f.signedPut(t, key, "application/pdf", limit, time.Minute)
		// Hiding the length forces the server to count bytes as it reads them.
		wantStatus(t, f.do(t, http.MethodPut, u, h, io.NopCloser(bytes.NewReader(big))), http.StatusRequestEntityTooLarge)
		f.wantNotExist(t, key)
		f.wantEmptyDir(t)
	})

	t.Run("exactly the limit is accepted", func(t *testing.T) {
		f := newFixture(t)
		u, h := f.signedPut(t, key, "application/pdf", limit, time.Minute)
		wantStatus(t, f.do(t, http.MethodPut, u, h, bytes.NewReader(big[:limit])), http.StatusOK)
		attrs, err := f.Head(t.Context(), key)
		if err != nil || attrs.Size != limit {
			t.Fatalf("Head = %+v, %v; want size %d", attrs, err, limit)
		}
	})
}

func TestLocal_ExpiredTokenRejected(t *testing.T) {
	f := newFixture(t)
	const key = "uploads/ws-1/file-4"

	put, h := f.signedPut(t, key, "application/pdf", 1<<20, time.Second)
	get := f.signedGet(t, key, time.Second)
	f.clk.Advance(time.Second + time.Millisecond)

	wantStatus(t, f.do(t, http.MethodPut, put, h, strings.NewReader("late")), http.StatusForbidden)
	f.wantNotExist(t, key)

	// A GET token for an object that exists is refused just the same.
	live, lh := f.signedPut(t, key, "application/pdf", 1<<20, time.Minute)
	wantStatus(t, f.do(t, http.MethodPut, live, lh, strings.NewReader("on time")), http.StatusOK)
	wantStatus(t, f.do(t, http.MethodGet, get, nil, nil), http.StatusForbidden)
}

func TestLocal_TokenIsValidUpToItsTTL(t *testing.T) {
	f := newFixture(t)
	const key = "uploads/ws-1/file-5"
	u, h := f.signedPut(t, key, "application/pdf", 1<<20, time.Second)
	f.clk.Advance(500 * time.Millisecond)
	wantStatus(t, f.do(t, http.MethodPut, u, h, strings.NewReader("in time")), http.StatusOK)
}

// forge re-encodes the claims of token with edit applied and keeps the original
// signature: an attacker who edits the claims but cannot sign them.
func forge(t *testing.T, token string, edit func(*claims)) string {
	t.Helper()
	payload, sig, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	edit(&c)
	raw, err = json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + sig
}

func tokenOf(signedURL string) string { return strings.TrimPrefix(signedURL, publicURL+"/api/blob/") }

func TestLocal_TamperedTokenRejected(t *testing.T) {
	f := newFixture(t)
	const (
		key   = "uploads/ws-1/file-6"
		other = "uploads/ws-2/secret"
	)
	f.upload(t, other, "application/pdf", []byte("another workspace"))
	flip := func(s string, i int) string {
		c := byte('A')
		if s[i] == 'A' {
			c = 'B'
		}
		return s[:i] + string(c) + s[i+1:]
	}

	put, h := f.signedPut(t, key, "application/pdf", 1<<10, time.Minute)
	putToken := tokenOf(put)
	payload, sig, _ := strings.Cut(putToken, ".")
	foreign, _ := newFixture(t).signedPut(t, key, "application/pdf", 1<<10, time.Minute) // another random signing key

	cases := map[string]string{
		"flipped payload character":   flip(payload, len(payload)/2) + "." + sig,
		"flipped signature character": payload + "." + flip(sig, len(sig)/2),
		"raised size limit":           forge(t, putToken, func(c *claims) { c.MaxBytes = 1 << 30 }),
		"signed by another key":       tokenOf(foreign),
		"no signature":                payload,
		"empty signature":             payload + ".",
		"no token":                    "",
		"garbage":                     "not-a-token",
		"too many parts":              putToken + "." + sig,
	}
	for name, tok := range cases {
		t.Run("put/"+name, func(t *testing.T) {
			wantStatus(t, f.do(t, http.MethodPut, publicURL+"/api/blob/"+tok, h, strings.NewReader("data")), http.StatusForbidden)
			f.wantNotExist(t, key)
		})
	}

	t.Run("get/forged key", func(t *testing.T) {
		tok := forge(t, tokenOf(f.signedGet(t, key, time.Minute)), func(c *claims) { c.Key = other })
		wantStatus(t, f.do(t, http.MethodGet, publicURL+"/api/blob/"+tok, nil, nil), http.StatusForbidden)
	})
}

func TestLocal_GetTokenCannotPut(t *testing.T) {
	f := newFixture(t)
	const key = "uploads/ws-1/file-7"
	get := f.signedGet(t, key, time.Minute)
	wantStatus(t, f.do(t, http.MethodPut, get, map[string]string{"Content-Type": "application/pdf"}, strings.NewReader("data")), http.StatusForbidden)
	f.wantNotExist(t, key)
	f.wantEmptyDir(t)
}

func TestLocal_PutTokenCannotGet(t *testing.T) {
	f := newFixture(t)
	const key = "uploads/ws-1/file-8"
	f.upload(t, key, "application/pdf", []byte("stored"))
	put, _ := f.signedPut(t, key, "application/pdf", 1<<10, time.Minute)
	wantStatus(t, f.do(t, http.MethodGet, put, nil, nil), http.StatusForbidden)
}

func TestLocal_KeyTraversalRejected(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for _, key := range []string{"../escape", "uploads/../../escape", "/etc/passwd", "uploads/ws\x00/file", ""} {
		t.Run(strings.ReplaceAll(key, "\x00", "NUL"), func(t *testing.T) {
			if _, _, err := f.SignedPut(ctx, key, "application/pdf", 1<<10, time.Minute); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("SignedPut error = %v, want errs.ErrInvalid", err)
			}
			if _, err := f.SignedGet(ctx, key, time.Minute); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("SignedGet error = %v, want errs.ErrInvalid", err)
			}
			if _, err := f.Head(ctx, key); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("Head error = %v, want errs.ErrInvalid", err)
			}
			if _, err := f.Read(ctx, key); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("Read error = %v, want errs.ErrInvalid", err)
			}
			if err := f.Delete(ctx, key); !errors.Is(err, errs.ErrInvalid) {
				t.Errorf("Delete error = %v, want errs.ErrInvalid", err)
			}
		})
	}
}

func TestLocal_SignedURLRejectsInvalidArguments(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"put without type": func() error {
			_, _, err := f.SignedPut(ctx, "uploads/ws-1/f", "", 1, time.Minute)
			return err
		},
		"put without bytes": func() error {
			_, _, err := f.SignedPut(ctx, "uploads/ws-1/f", "application/pdf", 0, time.Minute)
			return err
		},
		"put without ttl": func() error {
			_, _, err := f.SignedPut(ctx, "uploads/ws-1/f", "application/pdf", 1, 0)
			return err
		},
		"get without ttl": func() error {
			_, err := f.SignedGet(ctx, "uploads/ws-1/f", -time.Second)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, errs.ErrInvalid) {
				t.Fatalf("error = %v, want errs.ErrInvalid", err)
			}
		})
	}
}

func TestLocal_NotUnderPrefixIs404(t *testing.T) {
	f := newFixture(t)
	resp, err := f.srv.Client().Get(f.srv.URL + "/elsewhere/token")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	wantStatus(t, resp, http.StatusNotFound)
}

func TestOpen_RejectsGCSAndS3Schemes(t *testing.T) {
	for _, raw := range []string{"gs://bucket/prefix", "s3://bucket", "mem://"} {
		t.Run(raw, func(t *testing.T) {
			cfg := localConfig(t, t.TempDir())
			cfg.BlobURL = raw
			st, err := Open(t.Context(), cfg)
			if err == nil {
				t.Fatalf("Open(%q) = %T, want an error", raw, st)
			}
			if !strings.Contains(err.Error(), "not built in the local MVP") {
				t.Fatalf("error = %q, want it to say %q", err, "not built in the local MVP")
			}
			if strings.Contains(err.Error(), "bucket") {
				t.Fatalf("error %q echoes the configured URL", err)
			}
		})
	}
}

func TestOpen_FailsClosedOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	for name, edit := range map[string]func(*config.Config){
		"empty signing key": func(c *config.Config) { c.BlobSigningKey = nil },
		"short signing key": func(c *config.Config) { c.BlobSigningKey = c.BlobSigningKey[:16] },
		"no path":           func(c *config.Config) { c.BlobURL = "file://" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := localConfig(t, dir)
			edit(&cfg)
			if st, err := Open(t.Context(), cfg); err == nil {
				t.Fatalf("Open = %T, want an error", st)
			}
		})
	}
}
