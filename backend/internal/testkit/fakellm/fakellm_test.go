package fakellm_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/fakellm"
)

func TestGenerate_QueueThenHandlerThenUnscripted(t *testing.T) {
	c := fakellm.New()
	errBoom := errors.New("boom")
	c.QueueGenerate(
		fakellm.GenerateReply{Result: llm.Result{Text: "first"}},
		fakellm.GenerateReply{Err: errBoom},
	)
	c.HandleGenerate(func(_ context.Context, req llm.Request) (llm.Result, error) {
		return llm.Result{Text: "handled " + req.System}, nil
	})
	ctx := context.Background()

	if res, err := c.Generate(ctx, llm.Request{}); err != nil || res.Text != "first" {
		t.Fatalf("first call = (%q, %v), want (first, nil)", res.Text, err)
	}
	if _, err := c.Generate(ctx, llm.Request{}); !errors.Is(err, errBoom) {
		t.Fatalf("second call error = %v, want boom", err)
	}
	if res, err := c.Generate(ctx, llm.Request{System: "sys"}); err != nil || res.Text != "handled sys" {
		t.Fatalf("third call = (%q, %v), want the handler's answer", res.Text, err)
	}
	if got := len(c.GenerateCalls()); got != 3 {
		t.Fatalf("recorded Generate calls = %d, want 3", got)
	}

	if _, err := fakellm.New().Generate(ctx, llm.Request{}); !errors.Is(err, fakellm.ErrUnscripted) {
		t.Fatalf("unscripted Generate error = %v, want ErrUnscripted", err)
	}
}

func TestStream_DrawsFromTheGenerateScriptAndSendsOneDelta(t *testing.T) {
	c := fakellm.New()
	c.QueueGenerate(fakellm.GenerateReply{Result: llm.Result{Text: "hello"}})

	var deltas []string
	res, err := c.Stream(context.Background(), llm.Request{System: "s"}, func(d string) { deltas = append(deltas, d) })
	if err != nil || res.Text != "hello" {
		t.Fatalf("Stream = (%q, %v), want (hello, nil)", res.Text, err)
	}
	if !slices.Equal(deltas, []string{"hello"}) {
		t.Fatalf("deltas = %q, want [hello]", deltas)
	}
	if len(c.StreamCalls()) != 1 || len(c.GenerateCalls()) != 0 {
		t.Fatalf("stream calls %d, generate calls %d; want 1 and 0", len(c.StreamCalls()), len(c.GenerateCalls()))
	}
}

func TestEmbed_DefaultsToDeterministicUnitVectors(t *testing.T) {
	c := fakellm.New()
	got, err := c.Embed(context.Background(), []string{"alpha", "beta", "alpha"}, llm.EmbedQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != 3 || len(got[0]) != fakellm.EmbedDim {
		t.Fatalf("got %d vectors of %d dims, want 3 of %d", len(got), len(got[0]), fakellm.EmbedDim)
	}
	if !slices.Equal(got[0], got[2]) {
		t.Fatal("the same text must embed to the same vector")
	}
	if slices.Equal(got[0], got[1]) {
		t.Fatal("different texts must embed to different vectors")
	}
	var norm float64
	for _, x := range got[0] {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-4 {
		t.Fatalf("squared norm = %v, want 1", norm)
	}
	calls := c.EmbedCalls()
	if len(calls) != 1 || calls[0].Task != llm.EmbedQuery || !slices.Equal(calls[0].Texts, []string{"alpha", "beta", "alpha"}) {
		t.Fatalf("recorded calls = %+v", calls)
	}
}

func TestEmbed_QueueOverridesHandlerOverridesDefault(t *testing.T) {
	c := fakellm.New()
	c.QueueEmbed(fakellm.EmbedReply{Vectors: [][]float32{{1, 0}}})
	c.HandleEmbed(func(context.Context, []string, llm.EmbedTask) ([][]float32, error) {
		return [][]float32{{0, 1}}, nil
	})
	ctx := context.Background()

	if got, _ := c.Embed(ctx, []string{"x"}, llm.EmbedDocument); !slices.Equal(got[0], []float32{1, 0}) {
		t.Fatalf("queued reply not used first: %v", got)
	}
	if got, _ := c.Embed(ctx, []string{"x"}, llm.EmbedDocument); !slices.Equal(got[0], []float32{0, 1}) {
		t.Fatalf("handler not used after the queue: %v", got)
	}
}

func TestTranscribe_QueueAndRecording(t *testing.T) {
	c := fakellm.New()
	want := []llm.TranscriptEntry{{Speaker: "A", Text: "hi"}}
	c.QueueTranscribe(fakellm.TranscribeReply{Entries: want})

	got, err := c.Transcribe(context.Background(), llm.Audio{Name: "a.wav", MIME: "audio/wav"})
	if err != nil || len(got) != 1 || got[0].Text != "hi" {
		t.Fatalf("Transcribe = (%v, %v)", got, err)
	}
	if _, err := c.Transcribe(context.Background(), llm.Audio{}); !errors.Is(err, fakellm.ErrUnscripted) {
		t.Fatalf("second Transcribe error = %v, want ErrUnscripted", err)
	}
	if calls := c.TranscribeCalls(); len(calls) != 2 || calls[0].Name != "a.wav" {
		t.Fatalf("recorded calls = %+v", calls)
	}
}

func TestCancelledContext_FailsWithItsError(t *testing.T) {
	c := fakellm.New()
	c.QueueGenerate(fakellm.GenerateReply{Result: llm.Result{Text: "never"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Generate(ctx, llm.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context.Canceled", err)
	}
}

func TestConcurrentCalls_AreRecordedWithoutRaces(t *testing.T) {
	c := fakellm.New()
	c.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) { return llm.Result{Text: "ok"}, nil })

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = c.Generate(context.Background(), llm.Request{})
			_, _ = c.Embed(context.Background(), []string{"x"}, llm.EmbedDocument)
			_ = c.GenerateCalls()
		})
	}
	wg.Wait()

	if got := len(c.GenerateCalls()); got != 20 {
		t.Fatalf("recorded Generate calls = %d, want 20", got)
	}
	if got := len(c.EmbedCalls()); got != 20 {
		t.Fatalf("recorded Embed calls = %d, want 20", got)
	}
}

func TestEmbeddingModelAndADKModel(t *testing.T) {
	c := fakellm.New()
	if got := c.EmbeddingModel(); got != "fake-embedding" {
		t.Fatalf("EmbeddingModel() = %q, want fake-embedding", got)
	}
	c.SetEmbeddingModel("other")
	if got := c.EmbeddingModel(); got != "other" {
		t.Fatalf("EmbeddingModel() = %q, want other", got)
	}
	if _, err := c.ADKModel(llm.TierFlash); !errors.Is(err, fakellm.ErrUnscripted) {
		t.Fatalf("unscripted ADKModel error = %v, want ErrUnscripted", err)
	}
}
