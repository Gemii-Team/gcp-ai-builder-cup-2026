package blob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	cdk "gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/gcerrors"

	"kiwi/internal/platform/errs"
)

// Prefix is the URL path that precedes the token of a signed URL. Mount a *Local on
// it with the full path preserved: it reads the token from the request path.
const Prefix = "/api/blob/"

const (
	signingKeyBytes = 32
	dirMode         = 0o700
)

var b64 = base64.RawURLEncoding

// claims is what a token binds, signed as the JSON below.
type claims struct {
	Key         string `json:"k"`
	Method      string `json:"m"`
	ContentType string `json:"ct,omitempty"`  // PUT only
	MaxBytes    int64  `json:"max,omitempty"` // PUT only
	Exp         int64  `json:"exp"`           // Unix milliseconds
}

// Local is the object store of the local MVP: files under one directory, with
// application-signed URLs instead of a storage service's. The signed URLs point at
// the API itself, which serves them through ServeHTTP, so browsers upload
// same-origin and neither CORS nor storage credentials are involved. All methods
// are safe for concurrent use.
type Local struct {
	bucket    *cdk.Bucket
	key       []byte
	publicURL string
	now       func() time.Time
}

var (
	_ Store        = (*Local)(nil)
	_ http.Handler = (*Local)(nil)
)

func newLocal(dir string, signingKey []byte, publicURL string) (*Local, error) {
	if len(signingKey) != signingKeyBytes {
		return nil, fmt.Errorf("blob: the signing key must be %d bytes, got %d", signingKeyBytes, len(signingKey))
	}
	// NoTempDir keeps temp files on the same mount as their target: the directory is
	// a volume, and renaming across mounts fails.
	bucket, err := fileblob.OpenBucket(dir, &fileblob.Options{CreateDir: true, DirFileMode: dirMode, NoTempDir: true})
	if err != nil {
		return nil, fmt.Errorf("blob: open %s: %w", dir, err)
	}
	return &Local{bucket: bucket, key: slices.Clone(signingKey), publicURL: publicURL, now: time.Now}, nil
}

// Close releases the store.
func (l *Local) Close() error { return l.bucket.Close() }

// SignedPut implements Store. The URL belongs to ServeHTTP: it accepts one PUT of
// at most maxBytes with exactly this Content-Type until ttl passes.
func (l *Local) SignedPut(_ context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (string, map[string]string, error) {
	if err := validKey(key); err != nil {
		return "", nil, err
	}
	if contentType == "" || maxBytes <= 0 || ttl <= 0 {
		return "", nil, fmt.Errorf("blob: a signed upload needs a content type, a positive size limit and a positive ttl: %w", errs.ErrInvalid)
	}
	u, err := l.sign(claims{Key: key, Method: http.MethodPut, ContentType: contentType, MaxBytes: maxBytes}, ttl)
	if err != nil {
		return "", nil, err
	}
	return u, map[string]string{"Content-Type": contentType}, nil
}

// SignedGet implements Store. The URL belongs to ServeHTTP: it accepts GET until ttl
// passes.
func (l *Local) SignedGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if err := validKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		return "", fmt.Errorf("blob: a signed download needs a positive ttl: %w", errs.ErrInvalid)
	}
	return l.sign(claims{Key: key, Method: http.MethodGet}, ttl)
}

// Head implements Store; a missing key wraps fs.ErrNotExist.
func (l *Local) Head(ctx context.Context, key string) (Attrs, error) {
	if err := validKey(key); err != nil {
		return Attrs{}, err
	}
	a, err := l.bucket.Attributes(ctx, key)
	if err != nil {
		return Attrs{}, wrapErr("head", err)
	}
	return Attrs{Size: a.Size, ContentType: a.ContentType}, nil
}

// Read implements Store; a missing key wraps fs.ErrNotExist.
func (l *Local) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	r, err := l.bucket.NewReader(ctx, key, nil)
	if err != nil {
		return nil, wrapErr("read", err)
	}
	return r, nil
}

// Delete implements Store; a missing key is not an error.
func (l *Local) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if err := l.bucket.Delete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return wrapErr("delete", err)
	}
	return nil
}

// URI implements Store; the local file store has no address of its own.
func (l *Local) URI(string) string { return "" }

// ServeHTTP serves the URLs SignedPut and SignedGet returned: PUT stores the request
// body, GET returns the object. The token in the path is the only authorisation, so
// a bad, expired or mismatched one, or one issued for the other method, is answered
// 403 with no detail.
func (l *Local) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, Prefix)
	if !ok {
		reject(w, http.StatusNotFound)
		return
	}
	c, ok := l.verify(token)
	if !ok || c.Method != r.Method {
		reject(w, http.StatusForbidden)
		return
	}
	switch c.Method {
	case http.MethodPut:
		l.servePut(w, r, c)
	case http.MethodGet:
		l.serveGet(w, r, c)
	}
}

func (l *Local) servePut(w http.ResponseWriter, r *http.Request, c claims) {
	if r.Header.Get("Content-Type") != c.ContentType {
		reject(w, http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > c.MaxBytes {
		reject(w, http.StatusRequestEntityTooLarge)
		return
	}

	// Cancelling the context before Close makes the store discard the partial file
	// instead of publishing it.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bw, err := l.bucket.NewWriter(ctx, c.Key, &cdk.WriterOptions{ContentType: c.ContentType})
	if err != nil {
		slog.ErrorContext(r.Context(), "blob: start upload", "err", err)
		reject(w, http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(bw, http.MaxBytesReader(w, r.Body, c.MaxBytes)); err != nil {
		cancel()
		_ = bw.Close()
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			reject(w, http.StatusRequestEntityTooLarge)
		case errors.Is(err, io.ErrUnexpectedEOF):
			reject(w, http.StatusBadRequest)
		case r.Context().Err() != nil: // the client went away; nobody reads the answer
		default:
			slog.ErrorContext(r.Context(), "blob: store upload", "err", err)
			reject(w, http.StatusInternalServerError)
		}
		return
	}
	if err := bw.Close(); err != nil {
		slog.ErrorContext(r.Context(), "blob: finish upload", "err", err)
		reject(w, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (l *Local) serveGet(w http.ResponseWriter, r *http.Request, c claims) {
	br, err := l.bucket.NewReader(r.Context(), c.Key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			reject(w, http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "blob: open download", "err", err)
		reject(w, http.StatusInternalServerError)
		return
	}
	defer func() { _ = br.Close() }()
	contentType := br.ContentType()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.FormatInt(br.Size(), 10))
	// The object is served from the application's own origin with the type its
	// uploader declared; never let a browser second-guess it.
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, br) // a failure here is the client going away
}

// sign returns the URL for c, valid for ttl.
func (l *Local) sign(c claims, ttl time.Duration) (string, error) {
	c.Exp = l.now().Add(ttl).UnixMilli()
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("blob: encode token: %w", err)
	}
	payload := b64.EncodeToString(raw)
	return l.publicURL + Prefix + payload + "." + l.mac(payload), nil
}

// verify returns the claims of token if it carries our signature, has not expired
// and names a key and method we sign.
func (l *Local) verify(token string) (claims, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return claims{}, false
	}
	// Compare the encoded forms, so a token with a non-canonical encoding of a
	// correct signature is no more valid than any other.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(l.mac(payload))) != 1 {
		return claims{}, false
	}
	raw, err := b64.DecodeString(payload)
	if err != nil {
		return claims{}, false
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return claims{}, false
	}
	if !l.now().Before(time.UnixMilli(c.Exp)) || validKey(c.Key) != nil {
		return claims{}, false
	}
	if c.Method != http.MethodPut && c.Method != http.MethodGet {
		return claims{}, false
	}
	return c, true
}

// mac is the signature of an encoded payload.
func (l *Local) mac(payload string) string {
	h := hmac.New(sha256.New, l.key)
	h.Write([]byte(payload))
	return b64.EncodeToString(h.Sum(nil))
}

// validKey refuses keys that could climb out of the store's directory or that the
// store cannot name. The error never echoes the key.
func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.ContainsRune(key, 0) {
		return fmt.Errorf("blob: invalid key: %w", errs.ErrInvalid)
	}
	return nil
}

// wrapErr maps the store's not-found code to fs.ErrNotExist, the port's contract.
func wrapErr(op string, err error) error {
	if gcerrors.Code(err) == gcerrors.NotFound {
		return fmt.Errorf("blob: %s: %w", op, fs.ErrNotExist)
	}
	return fmt.Errorf("blob: %s: %w", op, err)
}

// reject answers with the status text only.
func reject(w http.ResponseWriter, status int) {
	http.Error(w, http.StatusText(status), status)
}
