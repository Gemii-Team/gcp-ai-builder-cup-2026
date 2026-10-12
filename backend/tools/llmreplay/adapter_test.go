package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/llm/openai"
)

// These tests put the real client of the stack, the openai adapter and the ADK model
// it hands the agent runtime, in front of the replay: whatever the replay answers,
// kiwi must be able to read, and kiwi's own requests must be the ones it matches.

func adapterClient(t *testing.T, base string) llm.Client {
	t.Helper()
	c, err := openai.New(config.Config{
		Env: "local", LLMProvider: "openai",
		LLMBaseURL: base, LLMAPIKey: "placeholder-not-a-secret",
		LLMModelFlash: "flash-model", LLMModelPro: "pro-model",
		EmbedBaseURL: base, EmbedModel: "embeddinggemma", ASRModel: "asr-model",
	}, http.DefaultClient)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	return c
}

var summarySchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"key_points":{"type":"array","items":{"type":"string"}}},"required":["summary","key_points"],"additionalProperties":false}`)

func TestAdapter_GenerateAndStreamReadTheReplay(t *testing.T) {
	srv, _ := startReplay(t, fixtures)
	c := adapterClient(t, srv.URL)
	req := llm.Request{
		Tier: llm.TierFlash, System: "Summarise the file.",
		Parts:  []llm.Part{{Text: "warehouse.txt\n\nPallets arrive on Friday."}},
		Schema: &llm.Schema{Name: "file_summary", JSON: summarySchema}, MaxOutputTokens: 800,
	}

	res, err := c.Generate(t.Context(), req)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var out struct {
		Summary   string   `json:"summary"`
		KeyPoints []string `json:"key_points"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || !strings.Contains(out.Summary, "warehouse") || len(out.KeyPoints) != 1 {
		t.Errorf("Generate text = %q (%v), want the warehouse fixture", res.Text, err)
	}
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 40 {
		t.Errorf("usage = %+v, want the fixture's 120 in, 40 out", res.Usage)
	}

	// The same fixture, streamed: the adapter reads deltas, never the reasoning.
	generic := req
	generic.Parts = []llm.Part{{Text: "notes.txt: nothing special"}}
	var deltas []string
	streamed, err := c.Stream(t.Context(), generic, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(deltas) < 2 {
		t.Errorf("%d deltas, want the answer in several pieces", len(deltas))
	}
	plain, err := c.Generate(t.Context(), generic)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if streamed.Text != plain.Text || !strings.Contains(streamed.Text, "A generic file summary.") || strings.Contains(streamed.Text, "one sentence") {
		t.Errorf("streamed %q, plain %q: want the same answer without its reasoning", streamed.Text, plain.Text)
	}
	if streamed.Usage != plain.Usage || streamed.Usage.InputTokens != 87 {
		t.Errorf("usage streamed %+v, plain %+v, want the fixture's", streamed.Usage, plain.Usage)
	}
}

func TestAdapter_KBAnswerCitesTheChunksOfTheRequest(t *testing.T) {
	srv, _ := startReplay(t, fixtures)
	c := adapterClient(t, srv.URL)
	chunk := func(id, text string) string { return fmt.Sprintf("<chunk id=%q>\n%s\n</chunk>", id, text) }
	res, err := c.Generate(t.Context(), llm.Request{
		Tier: llm.TierFlash, System: "You answer one question from passages.",
		Parts: []llm.Part{
			{Text: chunk(chunkA, "Every employee gets 15 days of annual leave a year.") + "\n\n" + chunk(chunkB, "Unused days: you may carry over five days.")},
			{Text: "<question>\nHow many days of annual leave do I get?\n</question>"},
		},
		Schema: &llm.Schema{Name: "kb_answer", JSON: json.RawMessage(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var a struct {
		AnswerMarkdown string `json:"answer_markdown"`
		Citations      []struct {
			ChunkID string `json:"chunk_id"`
		}
	}
	if err := json.Unmarshal([]byte(res.Text), &a); err != nil || len(a.Citations) != 2 || a.Citations[0].ChunkID != chunkA || a.Citations[1].ChunkID != chunkB {
		t.Errorf("answer = %q (%v), want citations of %s then %s", res.Text, err, chunkA, chunkB)
	}
}

func TestAdapter_EmbedAndTranscribeReadTheReplay(t *testing.T) {
	speech := pcm16WAV(32000, 7)
	srv, _ := startReplay(t, writeFixtures(t, map[string]string{
		"10-asr.json": fmt.Sprintf(`{"audio_sha256": %q, "text": "Good morning everyone. Dark mode moves to version one point one."}`, sha(speech)),
	}))
	c := adapterClient(t, srv.URL)

	t.Run("embeddings", func(t *testing.T) {
		vecs, err := c.Embed(t.Context(), []string{"Every employee gets 15 days of annual leave.", "พนักงานลาพักร้อนได้ 15 วัน"}, llm.EmbedDocument)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(vecs) != 2 || len(vecs[0]) != 768 || len(vecs[1]) != 768 {
			t.Fatalf("got %d vectors, want 2 of 768 dimensions", len(vecs))
		}
		query, err := c.Embed(t.Context(), []string{"How many days of annual leave"}, llm.EmbedQuery)
		if err != nil {
			t.Fatalf("Embed query: %v", err)
		}
		if cosine(query[0], vecs[0]) <= cosine(query[0], vecs[1]) {
			t.Errorf("the English query is closer to the Thai text (%.3f) than to the English one (%.3f)", cosine(query[0], vecs[1]), cosine(query[0], vecs[0]))
		}
	})

	t.Run("transcription", func(t *testing.T) {
		entries, err := c.Transcribe(t.Context(), llm.Audio{Data: speech, MIME: "audio/wav", Name: "recording.wav"})
		if err != nil {
			t.Fatalf("Transcribe: %v", err)
		}
		var text []string
		for _, e := range entries {
			text = append(text, e.Text)
			if e.StartMS != nil || e.EndMS != nil {
				t.Errorf("entry %+v has timing, which the replay does not invent", e)
			}
		}
		if got := strings.Join(text, " "); !strings.Contains(got, "Good morning everyone.") || !strings.Contains(got, "version one point one") {
			t.Errorf("transcript = %q", got)
		}
	})

	t.Run("audio no fixture names fails as an HTTP 599 the adapter reports", func(t *testing.T) {
		_, err := c.Transcribe(t.Context(), llm.Audio{Data: pcm16WAV(32000, 8), MIME: "audio/wav", Name: "other.wav"})
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 599 || !strings.Contains(err.Error(), "no transcription fixture") {
			t.Errorf("err = %v, want an HTTPError 599 whose text says no transcription fixture matched", err)
		}
	})
}

func TestAdapter_AFixtureMissIsAnHTTP599WhoseTextNamesTheSchema(t *testing.T) {
	srv, _ := startReplay(t, t.TempDir())
	c := adapterClient(t, srv.URL)
	_, err := c.Generate(t.Context(), llm.Request{
		Tier: llm.TierFlash, System: "Summarise.", Parts: []llm.Part{{Text: "hello"}},
		Schema: &llm.Schema{Name: "meeting_summary", JSON: summarySchema},
	})
	var he *llm.HTTPError
	if !errors.As(err, &he) || he.Status != 599 {
		t.Fatalf("err = %v, want an HTTPError 599 (never retried: llm.WithRetry repeats only 429 and 503)", err)
	}
	// What the service logs is err.Error(), which keeps 200 bytes of the body.
	if !strings.Contains(err.Error(), "schema=meeting_summary") {
		t.Errorf("err = %q, want the first unmatched schema named in what a service logs", err)
	}
}

// adkTurns are the two requests of a tool round trip as the agent runtime sends them.
func adkTurns(question string) (first, second *model.LLMRequest) {
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens:   2048,
		SystemInstruction: genai.NewContentFromText("You are @assistant.", genai.RoleUser),
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:                 "search_knowledge",
			Description:          "Search the workspace.",
			ParametersJsonSchema: map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}},
		}}}},
	}
	q := genai.NewContentFromText(question, genai.RoleUser)
	call := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call_1", Name: "search_knowledge", Args: map[string]any{"question": question}}}}}
	result := genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "search_knowledge", Response: map[string]any{"result": "dark mode moves to v1.1"}}}}, genai.RoleUser)
	return &model.LLMRequest{Contents: []*genai.Content{q}, Config: cfg}, &model.LLMRequest{Contents: []*genai.Content{q, call, result}, Config: cfg}
}

func TestAdapter_ADKToolRoundTripThroughTheReplay(t *testing.T) {
	srv, logs := startReplay(t, fixtures)
	m, err := adapterClient(t, srv.URL).ADKModel(llm.TierFlash)
	if err != nil {
		t.Fatalf("ADKModel: %v", err)
	}
	first, second := adkTurns("What did we decide about dark mode?")

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var call *genai.FunctionCall
			for resp, err := range m.GenerateContent(t.Context(), first, stream) {
				if err != nil {
					t.Fatalf("first turn: %v", err)
				}
				if resp.Content == nil {
					continue
				}
				for _, p := range resp.Content.Parts {
					if p.FunctionCall != nil {
						call = p.FunctionCall
					}
				}
			}
			if call == nil || call.Name != "search_knowledge" || call.Args["question"] != "What did we decide about dark mode?" {
				t.Fatalf("function call = %+v, want search_knowledge with the fixture's question", call)
			}

			var text string
			for resp, err := range m.GenerateContent(t.Context(), second, stream) {
				if err != nil {
					t.Fatalf("second turn: %v", err)
				}
				if resp.Content == nil || resp.Partial {
					continue
				}
				for _, p := range resp.Content.Parts {
					if p.Text != "" && !p.Thought {
						text = p.Text
					}
				}
			}
			if !strings.Contains(text, "v1.1") {
				t.Errorf("final text = %q, want the agent_final fixture", text)
			}
		})
	}
	if log := logs.String(); !strings.Contains(log, `"fixture":"40-agent.json[0]"`) || !strings.Contains(log, `"fixture":"40-agent.json[1]"`) || strings.Contains(log, "placeholder-not-a-secret") {
		t.Errorf("log = %s, want a line per matched request naming its fixture and no credential", log)
	}
}

// A model call the replay cannot answer must stop the agent run with an error, never
// hang it and never come back as an empty answer the run could post as a slip. openai-go
// retries a status of 500 and over twice, so the replay sees an agent's miss up to three
// times and logs a warning for each: the test counts none of them.
func TestAdapter_AnAgentMissSurfacesAsAnError(t *testing.T) {
	srv, logs := startReplay(t, fixtures)
	m, err := adapterClient(t, srv.URL).ADKModel(llm.TierFlash)
	if err != nil {
		t.Fatalf("ADKModel: %v", err)
	}
	first, _ := adkTurns("a question no agent fixture answers")

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var failure error
			for resp, err := range m.GenerateContent(ctx, first, stream) {
				if err != nil {
					failure = err
					break
				}
				if resp != nil && resp.Content != nil {
					for _, p := range resp.Content.Parts {
						if p.Text != "" || p.FunctionCall != nil {
							t.Fatalf("an unmatched request produced %+v, want an error and nothing else", p)
						}
					}
				}
			}
			if ctx.Err() != nil {
				t.Fatal("the model call hung until the deadline")
			}
			if failure == nil || !strings.Contains(failure.Error(), "599") || !strings.Contains(failure.Error(), "stage=agent_tool") {
				t.Fatalf("error = %v, want one that carries the 599 and names the stage", failure)
			}
		})
	}
	if !strings.Contains(logs.String(), `"msg":"unmatched request"`) {
		t.Errorf("log = %s, want the unmatched request warned about", logs.String())
	}
}
