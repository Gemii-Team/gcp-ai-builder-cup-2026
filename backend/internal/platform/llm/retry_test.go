package llm_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/fakellm"
)

// sleeps records the waits a WithRetry client asked for instead of taking them.
type sleeps struct{ got []time.Duration }

func (s *sleeps) sleep(d time.Duration) { s.got = append(s.got, d) }

func status(code int) error {
	return &llm.HTTPError{Status: code, Body: fmt.Sprintf("status %d", code)}
}

func TestWithRetry_Retries429And503WithBackoffAndJitter(t *testing.T) {
	inner := fakellm.New()
	inner.QueueGenerate(
		fakellm.GenerateReply{Err: status(503)},
		fakellm.GenerateReply{Err: status(429)},
		fakellm.GenerateReply{Result: llm.Result{Text: "done"}},
	)
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	res, err := c.Generate(t.Context(), llm.Request{})
	if err != nil || res.Text != "done" {
		t.Fatalf("Generate = %+v, %v, want the third answer", res, err)
	}
	if n := len(inner.GenerateCalls()); n != 3 {
		t.Fatalf("model calls = %d, want 3 (503, 429, 200)", n)
	}
	if len(s.got) != 2 {
		t.Fatalf("waits = %v, want 2", s.got)
	}
	// Retry n waits between half of and the whole of 1 s * 2^n: jitter is bounded
	// and the waits never shrink.
	for n, d := range s.got {
		full := time.Second << n
		if d < full/2 || d > full {
			t.Errorf("wait %d = %s, want between %s and %s", n, d, full/2, full)
		}
	}
	if s.got[1] < s.got[0] {
		t.Errorf("waits = %v, want them to grow", s.got)
	}
}

func TestWithRetry_JitterVaries(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 40 {
		inner := fakellm.New()
		inner.QueueGenerate(fakellm.GenerateReply{Err: status(503)}, fakellm.GenerateReply{})
		var s sleeps
		c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 1, Sleep: s.sleep})
		if _, err := c.Generate(t.Context(), llm.Request{}); err != nil {
			t.Fatal(err)
		}
		seen[s.got[0]] = true
	}
	if len(seen) < 2 {
		t.Fatalf("40 first waits were all %v: there is no jitter", seen)
	}
}

func TestWithRetry_NonRetryableErrorNotRetried(t *testing.T) {
	for name, err := range map[string]error{
		"HTTP 500":          status(500),
		"HTTP 400":          status(400),
		"HTTP 404":          status(404),
		"truncated":         llm.ErrTruncated,
		"unsupported":       llm.ErrUnsupportedByProvider,
		"budget":            llm.ErrBudgetExhausted,
		"a plain error":     errors.New("boom"),
		"wrapped HTTP 500":  fmt.Errorf("openai: %w", status(500)),
		"cancelled context": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			inner := fakellm.New()
			inner.QueueGenerate(fakellm.GenerateReply{Err: err}, fakellm.GenerateReply{Result: llm.Result{Text: "never"}})
			var s sleeps
			c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

			_, got := c.Generate(t.Context(), llm.Request{})
			if !errors.Is(got, err) {
				t.Fatalf("Generate error = %v, want %v", got, err)
			}
			if n := len(inner.GenerateCalls()); n != 1 || len(s.got) != 0 {
				t.Fatalf("model calls = %d, waits = %v, want 1 call and no wait", n, s.got)
			}
		})
	}
}

func TestWithRetry_WrappedHTTPErrorIsRetried(t *testing.T) {
	inner := fakellm.New()
	inner.QueueGenerate(fakellm.GenerateReply{Err: fmt.Errorf("openai: chat: %w", status(429))}, fakellm.GenerateReply{Result: llm.Result{Text: "ok"}})
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	if res, err := c.Generate(t.Context(), llm.Request{}); err != nil || res.Text != "ok" {
		t.Fatalf("Generate = %+v, %v, want the second answer", res, err)
	}
}

func TestWithRetry_GivesUpAfterMaxRetriesWithTheLastError(t *testing.T) {
	inner := fakellm.New()
	inner.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) { return llm.Result{}, status(429) })
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	_, err := c.Generate(t.Context(), llm.Request{})
	var he *llm.HTTPError
	if !errors.As(err, &he) || he.Status != 429 {
		t.Fatalf("Generate error = %v, want the HTTP 429", err)
	}
	if n := len(inner.GenerateCalls()); n != 4 || len(s.got) != 3 {
		t.Fatalf("model calls = %d, waits = %d, want 1 call + 3 retries and 3 waits", n, len(s.got))
	}
}

func TestWithRetry_ZeroMaxRetriesMeansNoRetry(t *testing.T) {
	inner := fakellm.New()
	inner.QueueGenerate(fakellm.GenerateReply{Err: status(503)}, fakellm.GenerateReply{})
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{Sleep: s.sleep})

	if _, err := c.Generate(t.Context(), llm.Request{}); err == nil {
		t.Fatal("Generate succeeded, want the 503")
	}
	if n := len(inner.GenerateCalls()); n != 1 {
		t.Fatalf("model calls = %d, want 1", n)
	}
}

func TestWithRetry_CoversStreamAndEmbed(t *testing.T) {
	inner := fakellm.New()
	inner.QueueGenerate(fakellm.GenerateReply{Err: status(503)}, fakellm.GenerateReply{Result: llm.Result{Text: "streamed"}})
	inner.QueueEmbed(fakellm.EmbedReply{Err: status(429)}, fakellm.EmbedReply{Vectors: [][]float32{{1}}})
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	var got strings.Builder
	res, err := c.Stream(t.Context(), llm.Request{}, func(d string) { got.WriteString(d) })
	if err != nil || res.Text != "streamed" || got.String() != "streamed" {
		t.Fatalf("Stream = %+v, %v, deltas %q, want the second answer", res, err, got.String())
	}
	vecs, err := c.Embed(t.Context(), []string{"a"}, llm.EmbedDocument)
	if err != nil || len(vecs) != 1 {
		t.Fatalf("Embed = %v, %v, want the second answer", vecs, err)
	}
	if len(s.got) != 2 {
		t.Fatalf("waits = %v, want one per failed call", s.got)
	}
}

func TestWithRetry_StreamThatDeliveredNothingIsRetried(t *testing.T) {
	inner := fakellm.New()
	inner.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) { return llm.Result{}, status(503) })
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	_, err := c.Stream(t.Context(), llm.Request{}, func(string) {})
	var he *llm.HTTPError
	if !errors.As(err, &he) || len(inner.StreamCalls()) != 4 || len(s.got) != 3 {
		t.Fatalf("Stream = %v after %d calls and %d waits, want the 503 after 1 call + 3 retries", err, len(inner.StreamCalls()), len(s.got))
	}
}

// deliversThenFails is a Client whose Stream writes a delta and then fails with a 503.
type deliversThenFails struct {
	*fakellm.Client
	streams int
}

func (d *deliversThenFails) Stream(_ context.Context, _ llm.Request, onDelta func(string)) (llm.Result, error) {
	d.streams++
	onDelta("partial")
	return llm.Result{}, status(503)
}

func TestWithRetry_StreamThatDeliveredTextIsNotRetried(t *testing.T) {
	// A retry would write the text again.
	d := &deliversThenFails{Client: fakellm.New()}
	var s sleeps
	c := llm.WithRetry(d, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	if _, err := c.Stream(t.Context(), llm.Request{}, func(string) {}); err == nil {
		t.Fatal("Stream succeeded, want the 503")
	}
	if d.streams != 1 || len(s.got) != 0 {
		t.Fatalf("stream calls = %d, waits = %v, want one call and no wait", d.streams, s.got)
	}
}

func TestWithRetry_TranscribeIsNotRetried(t *testing.T) {
	// A transcription is many requests; running it again would send every chunk
	// again. The adapter retries the one chunk that failed and owns the policy.
	inner := fakellm.New()
	inner.QueueTranscribe(fakellm.TranscribeReply{Err: status(503)}, fakellm.TranscribeReply{})
	var s sleeps
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: s.sleep})

	if _, err := c.Transcribe(t.Context(), llm.Audio{}); err == nil {
		t.Fatal("Transcribe succeeded, want the 503")
	}
	if n := len(inner.TranscribeCalls()); n != 1 || len(s.got) != 0 {
		t.Fatalf("Transcribe calls = %d, waits = %v, want one call and no wait", n, s.got)
	}
}

func TestWithRetry_StopsWhenTheContextEndsDuringTheWait(t *testing.T) {
	inner := fakellm.New()
	inner.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) { return llm.Result{}, status(429) })
	ctx, cancel := context.WithCancel(t.Context())
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3, Sleep: func(time.Duration) { cancel() }})

	_, err := c.Generate(ctx, llm.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context.Canceled", err)
	}
	if n := len(inner.GenerateCalls()); n != 1 {
		t.Fatalf("model calls = %d, want 1: no call after the context ended", n)
	}
}

func TestWithRetry_DelegatesWhatIsNotRetried(t *testing.T) {
	inner := fakellm.New()
	inner.SetEmbeddingModel("embeddinggemma")
	c := llm.WithRetry(inner, llm.RetryOptions{MaxRetries: 3})

	if got := c.EmbeddingModel(); got != "embeddinggemma" {
		t.Fatalf("EmbeddingModel = %q, want the wrapped client's", got)
	}
	if _, err := c.ADKModel(llm.TierFlash); !errors.Is(err, fakellm.ErrUnscripted) {
		t.Fatalf("ADKModel error = %v, want the wrapped client's", err)
	}
}

func TestHTTPError_ErrorNamesStatusAndBoundsTheBody(t *testing.T) {
	short := (&llm.HTTPError{Status: 429, Body: "No deployments available"}).Error()
	for _, want := range []string{"429", "No deployments available"} {
		if !strings.Contains(short, want) {
			t.Errorf("Error() = %q, want it to contain %q", short, want)
		}
	}
	long := (&llm.HTTPError{Status: 502, Body: strings.Repeat("<html>", 1000)}).Error()
	if len(long) > 400 {
		t.Errorf("Error() is %d bytes: the body must be cut", len(long))
	}
}
