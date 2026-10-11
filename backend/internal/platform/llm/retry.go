package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// HTTPError is a provider answer with a failing HTTP status. Adapters return it,
// wrapped or not, for every status of 300 and over, so WithRetry and callers can
// tell a busy provider from a refusal with errors.As.
type HTTPError struct {
	Status int
	// Body is what the provider said, as far as the adapter read it.
	Body string
}

// maxErrorBody bounds how much of Body Error repeats: a gateway in front of the
// provider may answer with a page of HTML.
const maxErrorBody = 200

// Error implements error.
func (e *HTTPError) Error() string {
	body := e.Body
	if len(body) > maxErrorBody {
		body = strings.ToValidUTF8(body[:maxErrorBody], "") + "…"
	}
	return fmt.Sprintf("llm: provider answered HTTP %d: %s", e.Status, body)
}

// RetryOptions configures WithRetry.
type RetryOptions struct {
	// MaxRetries is how many times a failed call is repeated after the first
	// attempt; zero means never.
	MaxRetries int
	// Sleep waits between attempts; nil waits on the clock and stops when the
	// context ends. Tests replace it.
	Sleep func(time.Duration)
}

const (
	baseBackoff = time.Second
	maxBackoff  = 30 * time.Second
)

// WithRetry wraps c so that Generate, Stream and Embed repeat a call the provider
// refused as busy (HTTP 429 or 503), up to o.MaxRetries times, waiting between
// attempts 1 s, 2 s, 4 s and so on, each wait cut by a random half so that callers
// that failed together do not return together. Every other error is returned at
// once. Transcribe is passed through: a transcription is many requests, and
// repeating it would send every chunk again; the adapter retries the one chunk that
// failed.
func WithRetry(c Client, o RetryOptions) Client {
	return &retrying{Client: c, opts: o}
}

type retrying struct {
	Client
	opts RetryOptions
}

// retryable reports an HTTPError of status 429 or 503, however deeply wrapped.
func retryable(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == http.StatusTooManyRequests || he.Status == http.StatusServiceUnavailable)
}

// backoff is the wait before retry n (from 0): between half of and the whole of
// baseBackoff * 2^n, at most maxBackoff.
func backoff(n int) time.Duration {
	d := min(baseBackoff<<min(n, 5), maxBackoff)
	return d/2 + rand.N(d/2+1)
}

// wait sleeps for d, or until ctx ends.
func (r *retrying) wait(ctx context.Context, d time.Duration) error {
	if r.opts.Sleep != nil {
		r.opts.Sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// do runs call, repeating it while it fails with a retryable error and retries
// remain. again reports whether a failed call may be repeated at all.
func do[T any](ctx context.Context, r *retrying, again func() bool, call func() (T, error)) (T, error) {
	for n := 0; ; n++ {
		res, err := call()
		if err == nil || n >= r.opts.MaxRetries || !retryable(err) || !again() {
			return res, err
		}
		if err := r.wait(ctx, backoff(n)); err != nil {
			return res, err
		}
	}
}

func always() bool { return true }

// Generate implements Client.
func (r *retrying) Generate(ctx context.Context, req Request) (Result, error) {
	return do(ctx, r, always, func() (Result, error) { return r.Client.Generate(ctx, req) })
}

// Stream implements Client. A stream that has already delivered text is not
// repeated: the caller would see that text twice.
func (r *retrying) Stream(ctx context.Context, req Request, onDelta func(delta string)) (Result, error) {
	delivered := false
	wrapped := func(d string) {
		delivered = true
		if onDelta != nil {
			onDelta(d)
		}
	}
	return do(ctx, r, func() bool { return !delivered }, func() (Result, error) { return r.Client.Stream(ctx, req, wrapped) })
}

// Embed implements Client.
func (r *retrying) Embed(ctx context.Context, texts []string, task EmbedTask) ([][]float32, error) {
	return do(ctx, r, always, func() ([][]float32, error) { return r.Client.Embed(ctx, texts, task) })
}
