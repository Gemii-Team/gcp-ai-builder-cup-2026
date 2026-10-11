package fakellm_test

import (
	"context"
	"encoding/json"
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

func TestCancelledContext_FailsWithItsErrorAndKeepsQueuedReplies(t *testing.T) {
	c := fakellm.New()
	c.QueueGenerate(fakellm.GenerateReply{Result: llm.Result{Text: "queued generate"}})
	c.QueueEmbed(fakellm.EmbedReply{Vectors: [][]float32{{1}}})
	c.QueueTranscribe(fakellm.TranscribeReply{Entries: []llm.TranscriptEntry{{Text: "queued transcribe"}}})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Generate(cancelled, llm.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context.Canceled", err)
	}
	if _, err := c.Stream(cancelled, llm.Request{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream error = %v, want context.Canceled", err)
	}
	if _, err := c.Embed(cancelled, []string{"x"}, llm.EmbedDocument); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed error = %v, want context.Canceled", err)
	}
	if _, err := c.Transcribe(cancelled, llm.Audio{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Transcribe error = %v, want context.Canceled", err)
	}

	// The cancelled calls were recorded but consumed nothing.
	if got := len(c.GenerateCalls()) + len(c.StreamCalls()) + len(c.EmbedCalls()) + len(c.TranscribeCalls()); got != 4 {
		t.Fatalf("recorded %d cancelled calls, want 4", got)
	}
	live := context.Background()
	if res, err := c.Generate(live, llm.Request{}); err != nil || res.Text != "queued generate" {
		t.Fatalf("Generate after cancelled calls = (%q, %v), want the queued reply", res.Text, err)
	}
	if got, err := c.Embed(live, []string{"x"}, llm.EmbedDocument); err != nil || len(got) != 1 || !slices.Equal(got[0], []float32{1}) {
		t.Fatalf("Embed after cancelled calls = (%v, %v), want the queued reply", got, err)
	}
	if got, err := c.Transcribe(live, llm.Audio{}); err != nil || len(got) != 1 || got[0].Text != "queued transcribe" {
		t.Fatalf("Transcribe after cancelled calls = (%v, %v), want the queued reply", got, err)
	}
}

func TestRecordedCalls_AreDeepCopies(t *testing.T) {
	c := fakellm.New()
	c.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) { return llm.Result{}, nil })
	c.HandleTranscribe(func(context.Context, llm.Audio) ([]llm.TranscriptEntry, error) { return nil, nil })

	data := []byte("image-bytes")
	params := json.RawMessage(`{"type":"object"}`)
	schema := &llm.Schema{Name: "s", JSON: json.RawMessage(`{"type":"string"}`)}
	audio := []byte("riff")
	req := llm.Request{
		Parts:  []llm.Part{{Data: data, MIME: "image/png"}},
		Tools:  []llm.Tool{{Name: "t", Parameters: params}},
		Schema: schema,
	}
	_, _ = c.Generate(context.Background(), req)
	_, _ = c.Stream(context.Background(), req, nil)
	_, _ = c.Transcribe(context.Background(), llm.Audio{Data: audio})

	// The caller reuses its buffers and mutates the request after the call.
	data[0], params[2], schema.JSON[2], audio[0] = 'X', 'X', 'X', 'X'
	schema.Name = "changed"
	req.Parts[0].MIME = "changed"

	check := func(name string, got llm.Request) {
		t.Helper()
		if string(got.Parts[0].Data) != "image-bytes" || got.Parts[0].MIME != "image/png" {
			t.Errorf("%s: recorded part = %q %q, want it unchanged", name, got.Parts[0].Data, got.Parts[0].MIME)
		}
		if string(got.Tools[0].Parameters) != `{"type":"object"}` {
			t.Errorf("%s: recorded tool parameters = %s, want them unchanged", name, got.Tools[0].Parameters)
		}
		if got.Schema.Name != "s" || string(got.Schema.JSON) != `{"type":"string"}` {
			t.Errorf("%s: recorded schema = %+v, want it unchanged", name, got.Schema)
		}
	}
	check("Generate", c.GenerateCalls()[0])
	check("Stream", c.StreamCalls()[0])
	if got := c.TranscribeCalls()[0].Data; string(got) != "riff" {
		t.Errorf("recorded audio = %q, want it unchanged", got)
	}

	// And mutating what a getter returned does not change the record either.
	c.GenerateCalls()[0].Parts[0].Data[0] = 'Y'
	check("Generate after mutating a returned copy", c.GenerateCalls()[0])
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
