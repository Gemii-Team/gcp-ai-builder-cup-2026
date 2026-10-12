package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	chatPath  = "/v1/chat/completions"
	embedPath = "/v1/embeddings"
	asrPath   = "/v1/audio/transcriptions"

	// testKey is what a client sends as its credential. It must never reach the logs.
	testKey = "Bearer probe-credential-do-not-log-0123"

	// A request names ids of its own: the replay may not know any in advance.
	chunkA   = "0194c1c0-0000-7000-8000-00000000000a"
	chunkB   = "0194c1c0-0000-7000-8000-00000000000b"
	chunkC   = "0194c1c0-0000-7000-8000-00000000000c"
	channel  = "0194c1c0-1111-7000-8000-0000000000c1"
	nokUser  = "0194c1c0-2222-7000-8000-0000000000e1"
	fixtures = "testdata/fixtures"
)

// syncBuffer is a log sink the handler goroutines and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startReplay serves the fixtures in dir; logs collects what the server logs.
func startReplay(t *testing.T, dir string) (*httptest.Server, *syncBuffer) {
	t.Helper()
	fx, err := loadFixtures(dir)
	if err != nil {
		t.Fatalf("loadFixtures(%s): %v", dir, err)
	}
	logs := &syncBuffer{}
	srv := httptest.NewServer(newHandler(fx, slog.New(slog.NewJSONHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return srv, logs
}

// writeFixtures writes files (name to content) into a fresh directory.
func writeFixtures(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(dir+"/"+name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type result struct {
	status int
	header http.Header
	body   []byte
}

func do(t *testing.T, req *http.Request) result {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: b}
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body any) result {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testKey)
	return do(t, req)
}

func asMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, b)
	}
	return m
}

// dig follows keys (string) and positions (int) down a decoded JSON value.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("dig %v: %v is not an object", path, v)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("dig %v: %v is not a list with position %d", path, v, k)
			}
			v = s[k]
		}
	}
	return v
}

// --- requests the way the adapter and ADK send them ---

type opts struct {
	schema string
	tools  bool
	stream bool
	usage  bool // stream_options.include_usage
}

func chatRequest(o opts, msgs ...map[string]any) map[string]any {
	req := map[string]any{"model": "qwen3.8-flash-next", "messages": msgs}
	if o.schema != "" {
		req["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": o.schema, "strict": true, "schema": map[string]any{"type": "object"}}}
	}
	if o.tools {
		req["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "search_knowledge", "parameters": map[string]any{"type": "object"}}}}
	}
	if o.stream {
		req["stream"] = true
	}
	if o.usage {
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	return req
}

func system(text string) map[string]any { return map[string]any{"role": "system", "content": text} }

// user is a user message of text parts, as the adapter sends it.
func user(texts ...string) map[string]any {
	parts := make([]any, len(texts))
	for i, t := range texts {
		parts[i] = map[string]any{"type": "text", "text": t}
	}
	return map[string]any{"role": "user", "content": parts}
}

func assistantCall(id, name, args string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}
}

func toolResult(id, text string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": text}
}

func content(t *testing.T, body []byte) string {
	t.Helper()
	s, ok := dig(t, asMap(t, body), "choices", 0, "message", "content").(string)
	if !ok {
		t.Fatalf("choices[0].message.content is not a string: %s", body)
	}
	return s
}

// --- TestReplay_* ---

func TestReplay_MatchesBySchemaName(t *testing.T) {
	srv, _ := startReplay(t, fixtures)

	t.Run("the schema name in response_format picks the fixture", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "meeting_summary"}, system("Summarise."), user("Meeting: Q4 launch", "[0] Malee: hello")))
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		var out struct {
			Summary   string
			Decisions []struct{ Text string }
		}
		if err := json.Unmarshal([]byte(content(t, got.body)), &out); err != nil {
			t.Fatalf("content is not the fixture's JSON: %v\n%s", err, got.body)
		}
		if !strings.Contains(out.Summary, "dark mode") || len(out.Decisions) != 1 {
			t.Errorf("answer = %+v, want the meeting_summary fixture", out)
		}
		if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	})

	t.Run("an object content is written as the JSON string the wire carries, and the defaults are filled", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "file_summary"}, system("Summarise."), user("warehouse.txt: pallets")))
		m := asMap(t, got.body)
		if m["object"] != "chat.completion" || m["model"] != "qwen3.8-flash-next" || m["id"] == "" || m["id"] == nil {
			t.Errorf("defaults: object=%v model=%v id=%v, want chat.completion, the request's model and an id", m["object"], m["model"], m["id"])
		}
		if dig(t, m, "choices", 0, "finish_reason") != "stop" || dig(t, m, "choices", 0, "message", "role") != "assistant" || dig(t, m, "choices", 0, "index") != float64(0) {
			t.Errorf("choice = %v, want finish_reason stop, role assistant, index 0", dig(t, m, "choices", 0))
		}
		var s struct{ Summary string }
		if err := json.Unmarshal([]byte(content(t, got.body)), &s); err != nil || !strings.Contains(s.Summary, "warehouse") {
			t.Errorf("content = %q (%v), want the warehouse fixture", content(t, got.body), err)
		}
		if dig(t, m, "usage", "total_tokens") != float64(160) {
			t.Errorf("usage = %v, want the fixture's", m["usage"])
		}
	})

	t.Run("the first fixture that fits wins; the next one answers what the first does not", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "file_summary"}, system("Summarise."), user("notes.txt: nothing about the other word")))
		if !strings.Contains(content(t, got.body), "A generic file summary.") {
			t.Errorf("content = %q, want the generic file_summary fixture", content(t, got.body))
		}
		if asMap(t, got.body)["id"] != "chatcmpl-recorded" {
			t.Errorf("id = %v, want the fixture's own id kept", asMap(t, got.body)["id"])
		}
	})

	t.Run("every contains must be in the request", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "meeting_summary"}, system("Summarise."), user("Meeting: Q4 launch")))
		if got.status != 599 {
			t.Fatalf("status %d for a request with one of two substrings, want 599: %s", got.status, got.body)
		}
	})

	t.Run("a schema no fixture names is unmatched", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "channel_summary"}, system("Summarise."), user("warehouse")))
		if got.status != 599 {
			t.Fatalf("status %d for an unknown schema, want 599", got.status)
		}
	})

	t.Run("a malformed request is a 400, never a fixture", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+chatPath, strings.NewReader(`{"messages": [`))
		if got := do(t, req); got.status != http.StatusBadRequest {
			t.Errorf("status %d, want 400", got.status)
		}
	})
}

func TestReplay_AgentStagesToolThenFinal(t *testing.T) {
	srv, _ := startReplay(t, fixtures)
	question := "What did we decide about dark mode?"

	t.Run("agent_tool: tools and no tool message answers with the tool call", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{tools: true}, system("You are @assistant."), user(question)))
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		m := asMap(t, got.body)
		if dig(t, m, "choices", 0, "finish_reason") != "tool_calls" {
			t.Errorf("finish_reason = %v, want tool_calls", dig(t, m, "choices", 0, "finish_reason"))
		}
		call := dig(t, m, "choices", 0, "message", "tool_calls", 0)
		if dig(t, call, "type") != "function" || dig(t, call, "id") == "" || dig(t, call, "id") == nil {
			t.Errorf("tool call = %v, want type function and an id", call)
		}
		if dig(t, call, "function", "name") != "search_knowledge" {
			t.Errorf("tool name = %v", dig(t, call, "function", "name"))
		}
		args, ok := dig(t, call, "function", "arguments").(string)
		if !ok {
			t.Fatalf("arguments = %v, want the JSON string the wire carries", dig(t, call, "function", "arguments"))
		}
		var a struct{ Question string }
		if err := json.Unmarshal([]byte(args), &a); err != nil || a.Question != question {
			t.Errorf("arguments = %s (%v), want the question", args, err)
		}
		if dig(t, m, "choices", 0, "message", "content") != nil {
			t.Errorf("content = %v, want null beside a tool call", dig(t, m, "choices", 0, "message", "content"))
		}
	})

	t.Run("agent_final: a tool message answers with the text, not with another call", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{tools: true},
			system("You are @assistant."), user(question),
			assistantCall("call_1", "search_knowledge", `{"question":"dark mode"}`),
			toolResult("call_1", `{"found":true,"answer":"Moved to v1.1 [1]"}`)))
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		m := asMap(t, got.body)
		if !strings.Contains(content(t, got.body), "v1.1") || dig(t, m, "choices", 0, "finish_reason") != "stop" || dig(t, m, "choices", 0, "message", "tool_calls") != nil {
			t.Errorf("answer = %s, want the agent_final text and no tool call", got.body)
		}
	})

	t.Run("a tool message without declared tools is still the final stage", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{}, user(question), assistantCall("call_1", "search_knowledge", `{}`), toolResult("call_1", "dark mode result")))
		if got.status != 200 || !strings.Contains(content(t, got.body), "v1.1") {
			t.Errorf("status %d: %s", got.status, got.body)
		}
	})

	t.Run("neither stage: a request with no tools and no tool message is not an agent turn", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{}, system("You are @assistant."), user(question)))
		if got.status != 599 {
			t.Fatalf("status %d, want 599: the fixtures name a stage, and this request has none", got.status)
		}
		if dig(t, asMap(t, got.body), "error", "stage") != "none" {
			t.Errorf("stage = %v, want none", dig(t, asMap(t, got.body), "error", "stage"))
		}
	})

	t.Run("a fixture for one stage never answers the other", func(t *testing.T) {
		// "draft a task" has an agent_tool fixture and no agent_final one.
		got := postJSON(t, srv, chatPath, chatRequest(opts{tools: true},
			user("@assistant draft a task"), assistantCall("call_d", "create_task_draft", `{}`), toolResult("call_d", `{"ref":"TSK-1001"}`)))
		if got.status != 599 {
			t.Fatalf("status %d, want 599 for the final stage of a call with no agent_final fixture", got.status)
		}
		if dig(t, asMap(t, got.body), "error", "stage") != "agent_final" {
			t.Errorf("stage = %v, want agent_final", dig(t, asMap(t, got.body), "error", "stage"))
		}
	})

	t.Run("tool arguments echo the ids the request carries", func(t *testing.T) {
		input := fmt.Sprintf("[Context for your tools: this message was posted in the channel with id %s by Alice. People it mentions, with their ids: Nok = %s.]\n\n@assistant draft a task to update the release notes", channel, nokUser)
		got := postJSON(t, srv, chatPath, chatRequest(opts{tools: true}, system("You are @assistant."), user(input)))
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		call := dig(t, asMap(t, got.body), "choices", 0, "message", "tool_calls", 0)
		if dig(t, call, "id") != "call_draft" {
			t.Errorf("id = %v, want the fixture's call_draft", dig(t, call, "id"))
		}
		var a struct {
			ChannelID   string `json:"channel_id"`
			Title       string
			OwnerUserID string `json:"owner_user_id"`
			DueHint     string `json:"due_hint"`
		}
		if err := json.Unmarshal([]byte(dig(t, call, "function", "arguments").(string)), &a); err != nil {
			t.Fatal(err)
		}
		if a.ChannelID != channel || a.OwnerUserID != nokUser || a.Title != "Update the release notes" || a.DueHint != "next:fri" {
			t.Errorf("arguments = %+v, want the channel and Nok's id from the request", a)
		}
	})

	t.Run("an id the request does not carry is an unmatched request", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{tools: true}, system("You are @assistant."), user("@assistant draft a task, no ids here")))
		if got.status != 599 {
			t.Fatalf("status %d, want 599 when the fixture needs a channel id the request lacks: %s", got.status, got.body)
		}
		if looked := dig(t, asMap(t, got.body), "error", "looked_for").(string); !strings.Contains(looked, "the channel with id ") || !strings.Contains(looked, "40-agent.json") {
			t.Errorf("looked_for = %q, want the fixture and the text it searched for", looked)
		}
	})
}

// kbRequest is the request knowledge sends: the chunks in <chunk id> tags, then the question.
func kbRequest(question string, chunks ...[2]string) map[string]any {
	tags := make([]string, len(chunks))
	for i, c := range chunks {
		tags[i] = fmt.Sprintf("<chunk id=%q>\n%s\n</chunk>", c[0], c[1])
	}
	return chatRequest(opts{schema: "kb_answer"}, system("You answer one question from passages."), user(strings.Join(tags, "\n\n"), "<question>\n"+question+"\n</question>"))
}

func TestReplay_KBAnswerCitesTheChunksTheRequestCarries(t *testing.T) {
	srv, _ := startReplay(t, fixtures)
	type answer struct {
		AnswerMarkdown string `json:"answer_markdown"`
		Citations      []struct {
			ChunkID string `json:"chunk_id"`
			Quote   string
		}
	}
	ask := func(t *testing.T, req map[string]any) (answer, result) {
		t.Helper()
		got := postJSON(t, srv, chatPath, req)
		var a answer
		if got.status == 200 {
			if err := json.Unmarshal([]byte(content(t, got.body)), &a); err != nil {
				t.Fatalf("content is not the kb_answer JSON: %v\n%s", err, got.body)
			}
		}
		return a, got
	}

	t.Run("the citations carry the request's chunk ids, whatever they are, in the fixture's order", func(t *testing.T) {
		for _, ids := range [][3]string{{chunkA, chunkB, chunkC}, {chunkC, chunkA, chunkB}} {
			a, got := ask(t, kbRequest("How many days of annual leave do I get?",
				[2]string{ids[0], "The office closes at six."},
				[2]string{ids[1], "Every employee gets 15 days of annual leave a year."},
				[2]string{ids[2], "Unused days: you may carry over five days into the next year."}))
			if got.status != 200 {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
			if len(a.Citations) != 2 || a.Citations[0].ChunkID != ids[1] || a.Citations[1].ChunkID != ids[2] {
				t.Errorf("citations = %+v, want chunks %s then %s", a.Citations, ids[1], ids[2])
			}
			if !strings.Contains(a.AnswerMarkdown, "[1]") || !strings.Contains(a.AnswerMarkdown, "[2]") {
				t.Errorf("answer = %q, want the markers [1] and [2] of its two citations", a.AnswerMarkdown)
			}
			if a.Citations[0].Quote != "15 days of annual leave" {
				t.Errorf("quote = %q", a.Citations[0].Quote)
			}
		}
	})

	t.Run("a Thai question cites a Thai chunk", func(t *testing.T) {
		a, got := ask(t, kbRequest("พนักงานลาพักร้อนกี่วัน", [2]string{chunkA, "พนักงานทุกคนมีสิทธิ์ลาพักร้อนประจำปี 15 วัน ต่อปี"}))
		if got.status != 200 || len(a.Citations) != 1 || a.Citations[0].ChunkID != chunkA || !strings.Contains(a.AnswerMarkdown, "[1]") {
			t.Errorf("status %d, answer %+v: %s", got.status, a, got.body)
		}
	})

	t.Run("a not-found answer has no citations", func(t *testing.T) {
		a, got := ask(t, kbRequest("an unanswerable question", [2]string{chunkA, "Nothing relevant."}))
		if got.status != 200 || a.AnswerMarkdown != "" || len(a.Citations) != 0 {
			t.Errorf("status %d, answer %+v, want the empty answer and no citation", got.status, a)
		}
		if !strings.Contains(content(t, got.body), `"citations":[]`) {
			t.Errorf("citations must be an empty list, not null: %s", got.body)
		}
	})

	t.Run("a passage the request does not carry is an unmatched request naming it", func(t *testing.T) {
		_, got := ask(t, kbRequest("a drifted question", [2]string{chunkA, "Some other passage."}))
		if got.status != 599 {
			t.Fatalf("status %d, want 599: the cited chunk is not in the request", got.status)
		}
		e := asMap(t, got.body)["error"].(map[string]any)
		if e["schema"] != "kb_answer" || !strings.Contains(e["looked_for"].(string), "text that no retrieved chunk holds") || !strings.Contains(e["looked_for"].(string), "30-kb-answer.json[3]") {
			t.Errorf("error = %v, want the schema, the fixture and the passage it looked for", e)
		}
	})

	t.Run("a quote the cited chunk does not hold is an unmatched request naming it", func(t *testing.T) {
		// The product shows the chunk's own words when a quote is absent from it, so
		// a misquoting fixture would pass unnoticed: the replay refuses to serve it.
		_, got := ask(t, kbRequest("a misquoted question", [2]string{chunkA, "Every employee gets 15 days of annual leave a year."}))
		if got.status != 599 {
			t.Fatalf("status %d, want 599: the quote is not in the chunk it cites: %s", got.status, got.body)
		}
		e := asMap(t, got.body)["error"].(map[string]any)
		looked, _ := e["looked_for"].(string)
		if e["schema"] != "kb_answer" || !strings.Contains(looked, "20 days of annual leave") || !strings.Contains(looked, "30-kb-answer.json[4]") || !strings.Contains(e["message"].(string), "quote") {
			t.Errorf("error = %v, want the schema, the fixture, its message naming the quote and the quote in looked_for", e)
		}
	})
}

// sseFrame is one data line of an event stream.
type sseFrame struct {
	raw  string
	done bool
	obj  map[string]any
}

func parseSSE(t *testing.T, body []byte) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, block := range strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n") {
		payload, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("a frame does not start with %q: %q", "data: ", block)
		}
		f := sseFrame{raw: payload}
		if payload == "[DONE]" {
			f.done = true
		} else if err := json.Unmarshal([]byte(payload), &f.obj); err != nil {
			t.Fatalf("a frame is not JSON: %v: %q", err, payload)
		}
		frames = append(frames, f)
	}
	return frames
}

// recordedFrameKeys are the keys of the chunks of a real streamed answer, as L21 recorded it.
func recordedFrameKeys(t *testing.T) (top, delta map[string]bool) {
	t.Helper()
	b, err := os.ReadFile("../../internal/platform/llm/openai/testdata/stream_one_sentence-01.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		BodyText string `json:"body_text"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	top, delta = map[string]bool{}, map[string]bool{}
	for _, f := range parseSSE(t, []byte(rec.BodyText)) {
		if f.done {
			continue
		}
		for k := range f.obj {
			top[k] = true
		}
		for _, c := range f.obj["choices"].([]any) {
			for k := range c.(map[string]any)["delta"].(map[string]any) {
				delta[k] = true
			}
		}
	}
	return top, delta
}

func TestReplay_StreamingMirrorsNonStreamingFixture(t *testing.T) {
	srv, _ := startReplay(t, fixtures)
	recordedTop, recordedDelta := recordedFrameKeys(t)

	cases := []struct {
		name string
		o    opts
		msgs []map[string]any
	}{
		{"text with reasoning", opts{schema: "file_summary"}, []map[string]any{system("Summarise."), user("notes.txt: nothing special")}},
		{"a tool call", opts{tools: true}, []map[string]any{system("You are @assistant."), user("What did we decide about dark mode?")}},
		{"a final answer", opts{tools: true}, []map[string]any{user("dark mode?"), assistantCall("c1", "search_knowledge", `{}`), toolResult("c1", "result")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := asMap(t, postJSON(t, srv, chatPath, chatRequest(tc.o, tc.msgs...)).body)
			so := tc.o
			so.stream, so.usage = true, true
			got := postJSON(t, srv, chatPath, chatRequest(so, tc.msgs...))
			if got.status != 200 {
				t.Fatalf("status %d: %s", got.status, got.body)
			}
			if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Fatalf("Content-Type = %q, want text/event-stream", ct)
			}
			frames := parseSSE(t, got.body)
			if last := frames[len(frames)-1]; !last.done {
				t.Fatalf("the stream ends with %q, want data: [DONE]", last.raw)
			}
			frames = frames[:len(frames)-1]

			var text, reasoning strings.Builder
			type call struct{ id, name, args string }
			calls := map[int]*call{}
			var finishes []string
			var usage any
			for i, f := range frames {
				if f.obj["object"] != "chat.completion.chunk" || f.obj["id"] != frames[0].obj["id"] {
					t.Errorf("frame %d: object %v id %v, want chat.completion.chunk of one id", i, f.obj["object"], f.obj["id"])
				}
				for k := range f.obj {
					if !recordedTop[k] {
						t.Errorf("frame %d has the key %q a recorded stream never has", i, k)
					}
				}
				if u, ok := f.obj["usage"]; ok {
					usage = u
					continue
				}
				choice := dig(t, f.obj, "choices", 0).(map[string]any)
				delta := choice["delta"].(map[string]any)
				for k := range delta {
					if !recordedDelta[k] && k != "tool_calls" {
						t.Errorf("frame %d: delta key %q is not one a recorded stream has", i, k)
					}
				}
				if i == 0 && (delta["role"] != "assistant" || delta["content"] != "") {
					t.Errorf("first frame delta = %v, want role assistant and empty content as recorded", delta)
				}
				if s, ok := delta["content"].(string); ok {
					text.WriteString(s)
				}
				if s, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(s)
				}
				for _, raw := range asSlice(delta["tool_calls"]) {
					tc := raw.(map[string]any)
					idx := int(tc["index"].(float64))
					c := calls[idx]
					if c == nil {
						c = &call{}
						calls[idx] = c
					}
					if id, ok := tc["id"].(string); ok {
						c.id = id
					}
					fn := tc["function"].(map[string]any)
					if n, ok := fn["name"].(string); ok {
						c.name = n
					}
					c.args += fn["arguments"].(string)
				}
				if fr, ok := choice["finish_reason"].(string); ok {
					finishes = append(finishes, fr)
					if len(delta) != 0 {
						t.Errorf("the finish frame has a delta %v, want {}", delta)
					}
				}
			}

			msg := dig(t, plain, "choices", 0, "message").(map[string]any)
			wantText, _ := msg["content"].(string)
			if text.String() != wantText {
				t.Errorf("streamed content = %q, want the JSON answer's %q", text.String(), wantText)
			}
			wantReasoning, _ := msg["reasoning_content"].(string)
			if reasoning.String() != wantReasoning {
				t.Errorf("streamed reasoning = %q, want %q", reasoning.String(), wantReasoning)
			}
			wantCalls := asSlice(msg["tool_calls"])
			if len(calls) != len(wantCalls) {
				t.Fatalf("%d streamed tool calls, want %d", len(calls), len(wantCalls))
			}
			for i, raw := range wantCalls {
				fn := dig(t, raw, "function")
				want := call{id: dig(t, raw, "id").(string), name: dig(t, fn, "name").(string), args: dig(t, fn, "arguments").(string)}
				if *calls[i] != want {
					t.Errorf("streamed tool call %d = %+v, want %+v", i, *calls[i], want)
				}
			}
			wantFinish := dig(t, plain, "choices", 0, "finish_reason")
			if len(finishes) != 1 || finishes[0] != wantFinish {
				t.Errorf("finish reasons = %v, want [%v]", finishes, wantFinish)
			}
			if !reflect.DeepEqual(usage, plain["usage"]) {
				t.Errorf("streamed usage = %v, want %v", usage, plain["usage"])
			}

			// Usage is sent only to a client that asked for it.
			quiet := postJSON(t, srv, chatPath, chatRequest(withStream(tc.o), tc.msgs...))
			for _, f := range parseSSE(t, quiet.body) {
				if _, ok := f.obj["usage"]; ok {
					t.Errorf("a stream without stream_options.include_usage carries usage: %s", f.raw)
				}
			}
		})
	}
}

func withStream(o opts) opts { o.stream = true; return o }

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func TestReplay_UnmatchedReturns599WithSummary(t *testing.T) {
	srv, logs := startReplay(t, fixtures)

	// More than 120 runes of mixed Thai and English over several lines, then a marker
	// that must reach neither the answer nor the log.
	body := "สวัสดีครับทุกคน\n\n   วันนี้เราจะคุยเรื่อง " + strings.Repeat("แผนการรีลีส ", 12) + "and a long English tail " + strings.Repeat("x", 80) + " TAILMARKER-9f3a"
	wantHead := string([]rune(strings.Join(strings.Fields(body), " "))[:120])
	if utf8.RuneCountInString(wantHead) != 120 {
		t.Fatalf("the test message is too short: %d runes", utf8.RuneCountInString(wantHead))
	}
	req := chatRequest(opts{schema: "channel_summary"}, system("Summarise the channel."), user("an earlier message"), map[string]any{"role": "assistant", "content": "ok"}, user(body))
	got := postJSON(t, srv, chatPath, req)

	if got.status != 599 {
		t.Fatalf("status %d, want 599", got.status)
	}
	if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	e, _ := asMap(t, got.body)["error"].(map[string]any)
	if e["schema"] != "channel_summary" || e["stage"] != "none" || e["type"] != "fixture_unmatched" || e["code"] != float64(599) {
		t.Errorf("error = %v, want schema channel_summary, stage none, type fixture_unmatched, code 599", e)
	}
	if looked, _ := e["looked_for"].(string); !strings.Contains(looked, "channel_summary") {
		t.Errorf("looked_for = %q, want what it looked for to name the schema", looked)
	}
	if head, _ := e["last_user_message"].(string); head != wantHead {
		t.Errorf("last_user_message = %q, want the first 120 runes of the last user message, whitespace collapsed:\n%q", head, wantHead)
	}
	if strings.Contains(string(got.body), "TAILMARKER-9f3a") || strings.Contains(string(got.body), "an earlier message") {
		t.Errorf("the answer carries more than the head of the last user message: %s", got.body)
	}
	// llm.HTTPError.Error repeats only the first 200 bytes of the body: the schema must be in them.
	if first := string(got.body[:min(200, len(got.body))]); !strings.Contains(first, "schema=channel_summary") || !strings.Contains(first, "stage=none") {
		t.Errorf("the first 200 bytes are %q, want them to name the schema and the stage", first)
	}
	// The adapter replaces the host name of its endpoint, which in the cluster is
	// llmreplay, by <host> in what it reports; a body that said it would read "<host>: …".
	if strings.Contains(string(got.body), "llmreplay") {
		t.Errorf("the answer names the server, which the adapter would scrub out of kiwi's log: %s", got.body)
	}

	t.Run("the log has the summary and nothing of the request but its head", func(t *testing.T) {
		log := logs.String()
		if !strings.Contains(log, "unmatched") || !strings.Contains(log, `"schema":"channel_summary"`) || !strings.Contains(log, `"stage":"none"`) || !strings.Contains(log, wantHead) {
			t.Errorf("log = %s, want an unmatched line with the schema, the stage and the head", log)
		}
		for _, leak := range []string{"Bearer", "probe-credential", "Authorization", "TAILMARKER-9f3a", "an earlier message", "Summarise the channel."} {
			if strings.Contains(log, leak) {
				t.Errorf("the log contains %q: %s", leak, log)
			}
		}
	})

	t.Run("a streaming request is answered with the same 599, not with a stream", func(t *testing.T) {
		so := opts{schema: "channel_summary", stream: true, usage: true}
		got := postJSON(t, srv, chatPath, chatRequest(so, system("Summarise."), user("hello")))
		if got.status != 599 || !strings.HasPrefix(got.header.Get("Content-Type"), "application/json") {
			t.Errorf("status %d, Content-Type %q, want 599 as JSON", got.status, got.header.Get("Content-Type"))
		}
	})

	t.Run("a request with no user message names an empty head", func(t *testing.T) {
		got := postJSON(t, srv, chatPath, chatRequest(opts{schema: "channel_summary"}, system("Summarise.")))
		e, _ := asMap(t, got.body)["error"].(map[string]any)
		if got.status != 599 || e["last_user_message"] != "" {
			t.Errorf("status %d, error %v, want 599 and an empty last_user_message", got.status, e)
		}
	})
}

// pcm16WAV is a canonical 44-byte-header mono PCM 16-bit WAV of n samples at 16 kHz;
// seed changes the audio, so each seed has a hash of its own.
func pcm16WAV(n, seed int) []byte {
	data := make([]byte, 2*n)
	for i := range n {
		s := int16(((i*37+seed*1001)%2000 - 1000) * 8)
		data[2*i], data[2*i+1] = byte(s), byte(s>>8)
	}
	h := make([]byte, 0, 44)
	le32 := func(v uint32) { h = append(h, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	le16 := func(v uint16) { h = append(h, byte(v), byte(v>>8)) }
	h = append(h, "RIFF"...)
	le32(uint32(36 + len(data)))
	h = append(h, "WAVEfmt "...)
	le32(16)
	le16(1)
	le16(1)
	le32(16000)
	le32(32000)
	le16(2)
	le16(16)
	h = append(h, "data"...)
	le32(uint32(len(data)))
	return append(h, data...)
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// upload posts audio the way the adapter does: model and language fields, then the file.
func upload(t *testing.T, srv *httptest.Server, audio []byte) result {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", "qwen3-asr")
	_ = w.WriteField("language", "en")
	part, err := w.CreateFormFile("file", "chunk-001.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(audio)
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+asrPath, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", testKey)
	return do(t, req)
}

func TestReplay_TranscriptionKeyedByAudioSHA(t *testing.T) {
	speech, silence, unknown := pcm16WAV(16000, 1), pcm16WAV(8000, 2), pcm16WAV(16000, 3)
	dir := writeFixtures(t, map[string]string{
		"10-asr.json": fmt.Sprintf(`[
			{"audio_sha256": %q, "text": "สวัสดีครับทุกคน Good morning everyone, dark mode moves to v1.1."},
			{"audio_sha256": %q, "text": ""}
		]`, sha(speech), strings.ToUpper(sha(silence))),
	})
	srv, logs := startReplay(t, dir)

	t.Run("the text of the fixture whose hash is the audio's", func(t *testing.T) {
		got := upload(t, srv, speech)
		if got.status != 200 || !strings.HasPrefix(got.header.Get("Content-Type"), "application/json") {
			t.Fatalf("status %d, Content-Type %q: %s", got.status, got.header.Get("Content-Type"), got.body)
		}
		if text := asMap(t, got.body)["text"]; text != "สวัสดีครับทุกคน Good morning everyone, dark mode moves to v1.1." {
			t.Errorf("text = %v", text)
		}
	})

	t.Run("an empty transcript is a 200 with empty text, and a hash in capitals is still its key", func(t *testing.T) {
		got := upload(t, srv, silence)
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		if text, ok := asMap(t, got.body)["text"]; !ok || text != "" {
			t.Errorf("text = %v (present %v), want the empty string", text, ok)
		}
	})

	t.Run("unknown audio is a 599 naming the first 8 hex digits of its hash", func(t *testing.T) {
		got := upload(t, srv, unknown)
		if got.status != 599 {
			t.Fatalf("status %d, want 599: %s", got.status, got.body)
		}
		e := asMap(t, got.body)["error"].(map[string]any)
		prefix := sha(unknown)[:8]
		if !strings.Contains(e["message"].(string), prefix) || strings.Contains(e["message"].(string), sha(unknown)[:9]) {
			t.Errorf("message = %q, want it to name %s and no more of the hash", e["message"], prefix)
		}
		if e["audio_sha256"] != sha(unknown) {
			t.Errorf("audio_sha256 = %v, want the whole hash so a fixture can be written from it", e["audio_sha256"])
		}
		if first := string(got.body[:min(200, len(got.body))]); !strings.Contains(first, prefix) {
			t.Errorf("the first 200 bytes are %q, want the hash prefix in them", first)
		}
		if log := logs.String(); !strings.Contains(log, prefix) || strings.Contains(log, sha(unknown)[:9]) || strings.Contains(log, "Bearer") {
			t.Errorf("log = %s, want the 8-digit prefix and no credential", log)
		}
	})

	t.Run("a request with no file part is a 400", func(t *testing.T) {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		_ = w.WriteField("model", "qwen3-asr")
		_ = w.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+asrPath, &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		if got := do(t, req); got.status != http.StatusBadRequest {
			t.Errorf("status %d, want 400", got.status)
		}
	})
}

// cosine of two L2-normalised vectors.
func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

func l2norm(v []float32) float64 { return math.Sqrt(cosine(v, v)) }

func TestHashedEmbedding_OverlappingThaiTextIsCloserThanUnrelated(t *testing.T) {
	query := "วันลาพักร้อนประจำปีมีกี่วัน"
	overlapping := "พนักงานมีสิทธิ์ลาพักร้อนประจำปี 15 วัน และสะสมวันลาได้"
	unrelated := "เซิร์ฟเวอร์สำรองข้อมูลทุกคืนเวลาตีสอง แล้วส่งรายงานให้ทีมโครงสร้างพื้นฐาน"

	q, o, u := hashedEmbedding(query), hashedEmbedding(overlapping), hashedEmbedding(unrelated)
	if got, want := cosine(q, o), cosine(q, u); got < want+0.15 {
		t.Errorf("cosine(query, overlapping Thai text) = %.3f, cosine(query, unrelated) = %.3f: the overlapping text must be clearly closer", got, want)
	}

	// The same holds in English, and across the case of Latin letters.
	eq, eo, eu := hashedEmbedding("How many days of annual leave do I get?"), hashedEmbedding("Every employee gets 15 days of ANNUAL LEAVE a year."), hashedEmbedding("The backup runs nightly at two and mails the infrastructure team.")
	if got, want := cosine(eq, eo), cosine(eq, eu); got < want+0.15 {
		t.Errorf("English: cosine(overlapping) = %.3f, cosine(unrelated) = %.3f", got, want)
	}

	// An identical text is its own nearest neighbour, and the vector is deterministic.
	if got := cosine(q, hashedEmbedding(query)); math.Abs(got-1) > 1e-5 {
		t.Errorf("cosine(text, itself) = %v, want 1", got)
	}
}

func TestHashedEmbedding_AnyInputIsAUnitVector(t *testing.T) {
	for name, text := range map[string]string{
		"empty": "", "spaces": "   \n\t ", "one rune": "ก", "emoji": "🚀🔥", "thai only": "สวัสดี", "NUL": "\x00",
		"invalid UTF-8": "bad \xff\xfe bytes", "long": strings.Repeat("annual leave ", 5000),
	} {
		v := hashedEmbedding(text)
		if len(v) != embedDim {
			t.Errorf("%s: %d dimensions, want %d", name, len(v), embedDim)
		}
		if n := l2norm(v); math.Abs(n-1) > 1e-4 || math.IsNaN(n) {
			t.Errorf("%s: norm %v, want 1", name, n)
		}
	}
	// Normalisation: a decomposed and a composed spelling, and a SARA AM, are the same text.
	if got := cosine(hashedEmbedding("caf\u00e9 ล\u0e33ดับ"), hashedEmbedding("cafe\u0301 ล\u0e4d\u0e32ดับ")); got < 0.999 {
		t.Errorf("composed and decomposed spellings differ: cosine %v", got)
	}
}

func TestEmbeddings_ShapeTheAdapterReads(t *testing.T) {
	srv, _ := startReplay(t, fixtures)

	t.Run("a batch comes back in order, 768 floats each, whatever the language", func(t *testing.T) {
		inputs := []string{"title: none | text: Every employee gets 15 days of annual leave.", "task: search result | query: ลาพักร้อนกี่วัน", ""}
		got := postJSON(t, srv, embedPath, map[string]any{"model": "embeddinggemma", "input": inputs})
		if got.status != 200 {
			t.Fatalf("status %d: %s", got.status, got.body)
		}
		var out struct {
			Object string
			Model  string
			Data   []struct {
				Object    string
				Index     int
				Embedding []float32
			}
		}
		if err := json.Unmarshal(got.body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Object != "list" || out.Model != "embeddinggemma" || len(out.Data) != len(inputs) {
			t.Fatalf("answer = %+v, want a list of %d for the model asked", out, len(inputs))
		}
		for i, d := range out.Data {
			if d.Index != i || d.Object != "embedding" || len(d.Embedding) != embedDim || math.Abs(l2norm(d.Embedding)-1) > 1e-4 {
				t.Errorf("data[%d]: index %d, %d floats, norm %v", i, d.Index, len(d.Embedding), l2norm(d.Embedding))
			}
			if want := hashedEmbedding(stripTaskPrefix(inputs[i])); !reflect.DeepEqual(d.Embedding, want) {
				t.Errorf("data[%d] is not the hashed embedding of its text", i)
			}
		}
	})

	t.Run("the task prefix of the model does not count as text", func(t *testing.T) {
		doc := hashedEmbedding(stripTaskPrefix("title: none | text: annual leave"))
		query := hashedEmbedding(stripTaskPrefix("task: search result | query: annual leave"))
		if got := cosine(doc, query); got < 0.999 {
			t.Errorf("cosine between the same words as a document and as a query = %v, want 1", got)
		}
	})

	t.Run("a single string is a batch of one", func(t *testing.T) {
		got := postJSON(t, srv, embedPath, map[string]any{"model": "m", "input": "annual leave"})
		if got.status != 200 || len(asMap(t, got.body)["data"].([]any)) != 1 {
			t.Errorf("status %d: %s", got.status, got.body)
		}
	})

	t.Run("no input is a 400, and nothing is a 599", func(t *testing.T) {
		if got := postJSON(t, srv, embedPath, map[string]any{"model": "m", "input": []string{}}); got.status != http.StatusBadRequest {
			t.Errorf("status %d for an empty batch, want 400", got.status)
		}
	})
}

// A model takes seconds, and the screens of K.I.W.I. show that wait (a pending slip); a
// replay that answers in microseconds would let a pending state come and go between two
// frames of a browser. A fixture can hold its answer back for delay_ms.
func TestReplay_DelayMsHoldsTheAnswerBack(t *testing.T) {
	const delay = 300 * time.Millisecond
	speech := pcm16WAV(16000, 5)
	dir := writeFixtures(t, map[string]string{
		"10-slow.json": `[
			{"match": {"schema": "file_summary", "contains": "slow"}, "delay_ms": 300, "response": {"choices": [{"message": {"content": "slow answer"}}]}},
			{"match": {"schema": "file_summary"}, "response": {"choices": [{"message": {"content": "quick answer"}}]}}
		]`,
		"20-asr.json": fmt.Sprintf(`{"audio_sha256": %q, "text": "slow speech", "delay_ms": 300}`, sha(speech)),
	})
	srv, _ := startReplay(t, dir)

	timed := func(f func() result) (result, time.Duration) {
		start := time.Now()
		got := f()
		return got, time.Since(start)
	}
	chat := func(text string, o opts) func() result {
		return func() result { return postJSON(t, srv, chatPath, chatRequest(o, user(text))) }
	}

	t.Run("a chat answer", func(t *testing.T) {
		got, took := timed(chat("a slow file", opts{schema: "file_summary"}))
		if got.status != 200 || content(t, got.body) != "slow answer" || took < delay {
			t.Errorf("status %d, content %q after %v, want the slow answer after at least %v", got.status, got.body, took, delay)
		}
	})

	t.Run("a streamed answer is held back before its first frame", func(t *testing.T) {
		got, took := timed(chat("a slow file", opts{schema: "file_summary", stream: true}))
		if got.status != 200 || took < delay || !strings.Contains(string(got.body), "slow answer"[:4]) {
			t.Errorf("status %d after %v: %s", got.status, took, got.body)
		}
	})

	t.Run("a transcription", func(t *testing.T) {
		got, took := timed(func() result { return upload(t, srv, speech) })
		if got.status != 200 || asMap(t, got.body)["text"] != "slow speech" || took < delay {
			t.Errorf("status %d, body %s after %v, want the transcript after at least %v", got.status, got.body, took, delay)
		}
	})

	t.Run("a fixture with no delay answers at once", func(t *testing.T) {
		got, took := timed(chat("a file", opts{schema: "file_summary"}))
		if got.status != 200 || content(t, got.body) != "quick answer" || took >= delay {
			t.Errorf("status %d, content %q after %v, want the quick answer in under %v", got.status, got.body, took, delay)
		}
	})

	t.Run("an unmatched request is never held back", func(t *testing.T) {
		got, took := timed(chat("anything", opts{schema: "meeting_summary"}))
		if got.status != 599 || took >= delay {
			t.Errorf("status %d after %v, want an immediate 599", got.status, took)
		}
	})
}
