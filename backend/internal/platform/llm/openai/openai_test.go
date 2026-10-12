package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm"
	"kiwi/internal/testkit/httpfixture"
)

// The model ids and hosts the fixtures were recorded with: a request's model name is
// part of what a replayed exchange is matched on.
const (
	flashModel = "qwen3.8-flash-next"
	proModel   = "qwen3.8-27b-uncensored"
	asrModel   = "qwen3-asr"
	embedModel = "embeddinggemma"
	testKey    = "test-key-not-a-secret"
	llmHost    = "llm.test"
	embedHost  = "embed.test"
	testdata   = "testdata"
)

func testConfig() config.Config {
	return config.Config{
		Env:           "local",
		LLMProvider:   "openai",
		LLMBaseURL:    "http://" + llmHost,
		LLMAPIKey:     testKey,
		LLMModelFlash: flashModel,
		LLMModelPro:   proModel,
		EmbedBaseURL:  "http://" + embedHost,
		EmbedModel:    embedModel,
		ASRModel:      asrModel,
	}
}

// sent is one request a client made.
type sent struct {
	Method string
	Host   string
	Path   string
	Header http.Header
	Body   []byte
}

// json decodes the body of a JSON request.
func (s sent) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.Body, &m); err != nil {
		t.Fatalf("request body to %s is not a JSON object: %v\n%s", s.Path, err, s.Body)
	}
	return m
}

// form decodes a multipart request into its fields and its one file.
func (s sent) form(t *testing.T) (fields map[string]string, filename string, file []byte) {
	t.Helper()
	mt, params, err := mime.ParseMediaType(s.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, want multipart/form-data (%v)", s.Header.Get("Content-Type"), err)
	}
	fields = map[string]string{}
	r := multipart.NewReader(bytes.NewReader(s.Body), params["boundary"])
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			return fields, filename, file
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		if p.FileName() != "" {
			if file != nil {
				t.Fatal("the request has more than one file")
			}
			filename, file = p.FileName(), data
			continue
		}
		fields[p.FormName()] = string(data)
	}
}

// spy is a RoundTripper that records every request before passing it on.
type spy struct {
	next http.RoundTripper
	mu   sync.Mutex
	reqs []sent
}

func (s *spy) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, sent{req.Method, req.URL.Host, req.URL.Path, req.Header.Clone(), body})
	s.mu.Unlock()
	return s.next.RoundTrip(req)
}

// to returns the requests sent to path, in order.
func (s *spy) to(path string) []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []sent
	for _, r := range s.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (s *spy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// only returns the one request sent to path.
func (s *spy) only(t *testing.T, path string) sent {
	t.Helper()
	got := s.to(path)
	if len(got) != 1 {
		t.Fatalf("%d requests to %s, want 1", len(got), path)
	}
	return got[0]
}

// reply is one scripted answer of a canned transport.
type reply struct {
	status      int
	contentType string
	body        string
	err         error
}

func jsonReply(body string) reply {
	return reply{status: 200, contentType: "application/json", body: body}
}

func statusReply(status int, body string) reply {
	return reply{status: status, contentType: "application/json", body: body}
}

// chatReply is a Chat Completions answer.
func chatReply(content, reasoning, finish string) reply {
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	})
	return jsonReply(string(b))
}

// canned answers requests from a script, in order, and fails the test when it runs
// out. It records what it was sent.
type canned struct {
	spy
	script   []reply
	idx      int
	inflight atomic.Int32
	peak     atomic.Int32
}

func newCanned(replies ...reply) *canned {
	c := &canned{script: replies}
	c.next = roundTripFunc(c.answer)
	return c
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (c *canned) answer(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	n := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	c.mu.Lock()
	i := c.idx
	c.idx++
	c.mu.Unlock()
	if i >= len(c.script) {
		return nil, fmt.Errorf("canned: request %d to %s has no scripted reply", i+1, req.URL.Path)
	}
	r := c.script[i]
	if r.err != nil {
		return nil, r.err
	}
	h := http.Header{}
	if r.contentType != "" {
		h.Set("Content-Type", r.contentType)
	}
	return &http.Response{
		StatusCode: r.status, Status: fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		Header: h, Body: io.NopCloser(strings.NewReader(r.body)), Request: req,
	}, nil
}

// newClient builds the client over rt with the test configuration.
func newClient(t *testing.T, rt http.RoundTripper, edit ...func(*config.Config)) *client {
	t.Helper()
	cfg := testConfig()
	for _, f := range edit {
		f(&cfg)
	}
	c, err := New(cfg, &http.Client{Transport: rt})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.(*client)
}

// replayed is a client that answers from the recorded fixtures.
func replayed(t *testing.T) (*client, *spy) {
	t.Helper()
	s := &spy{next: httpfixture.Replay(t, testdata)}
	return newClient(t, s), s
}

// --- the requests the fixtures were recorded with; TestRecord sends the same ones ---

const summarySchemaJSON = `{
	"type": "object",
	"properties": {
		"summary": {"type": "string"},
		"decisions": {"type": "array", "items": {
			"type": "object",
			"properties": {"text": {"type": "string"}, "entry_refs": {"type": "array", "items": {"type": "integer"}}},
			"required": ["text", "entry_refs"],
			"additionalProperties": false
		}},
		"action_items": {"type": "array", "items": {
			"type": "object",
			"properties": {
				"title": {"type": "string"},
				"owner_hint": {"type": "string"},
				"due_hint": {"type": "string", "pattern": "^(\\d{4}-\\d{2}-\\d{2}|\\+\\d{1,3}d|next:(mon|tue|wed|thu|fri|sat|sun)|eow|none)$"},
				"entry_refs": {"type": "array", "items": {"type": "integer"}}
			},
			"required": ["title", "owner_hint", "due_hint", "entry_refs"],
			"additionalProperties": false
		}}
	},
	"required": ["summary", "decisions", "action_items"],
	"additionalProperties": false
}`

const summarySystem = `You read the transcript of one meeting and write down what its people decided and promised to do.
Write in the language of the meeting; keep names and Latin terms as they were said.
Return JSON with these fields.
- summary: two or three sentences.
- decisions: what was agreed; text is one sentence, entry_refs are the entry numbers where it was said.
- action_items: every promise; title is a short imperative sentence, owner_hint the name of the person, due_hint one of YYYY-MM-DD, +Nd, next:fri, eow or none.`

const summaryTranscript = `<transcript>
[0] Malee: สวัสดีครับทุกคน วันนี้เราจะคุยเรื่อง dark mode กับ release ครับ
[1] John: I think we should ship dark mode in v1.1, not v1.0.
[2] Malee: ตกลงครับ เราเลื่อน dark mode ไป v1.1
[3] Nok: I will update the release notes by next Friday.
[4] John: And I'll tell the QA team tomorrow.
</transcript>`

func summaryRequest(tier llm.Tier) llm.Request {
	return llm.Request{
		Tier:            tier,
		System:          summarySystem,
		Parts:           []llm.Part{{Text: "Meeting date: Friday 2026-10-09 (Asia/Bangkok)"}, {Text: summaryTranscript}},
		Schema:          &llm.Schema{Name: "meeting_summary", JSON: json.RawMessage(summarySchemaJSON)},
		MaxOutputTokens: 1024,
	}
}

// oneSentenceRequest asks for a short answer: a reasoning model thinks first, so the
// recorded answer carries reasoning_content next to a content that starts with blank
// lines.
func oneSentenceRequest() llm.Request {
	return llm.Request{
		Tier:            llm.TierFlash,
		Parts:           []llm.Part{{Text: "In one sentence, what is a kanban board?"}},
		MaxOutputTokens: 256,
	}
}

// truncatedRequest asks for something that takes more reasoning than the headroom
// allows, with an output budget too small to matter.
func truncatedRequest() llm.Request {
	return llm.Request{
		Tier: llm.TierFlash,
		Parts: []llm.Part{{Text: "Find all the ways to place 8 queens on a chessboard so that none attacks another. " +
			"List the first 12 solutions in lexicographic order of their column indexes, and verify each one square by square before you answer."}},
		MaxOutputTokens: 1,
	}
}

var searchTool = llm.Tool{
	Name:        "search_knowledge",
	Description: "Search the workspace's meetings and files for a passage that answers the query.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"what to look for"}},"required":["query"]}`),
}

func toolRequest() llm.Request {
	return llm.Request{
		Tier:            llm.TierFlash,
		System:          "Answer from the workspace's knowledge. Always call search_knowledge before you answer.",
		Parts:           []llm.Part{{Text: "What did we decide about dark mode?"}},
		Tools:           []llm.Tool{searchTool},
		MaxOutputTokens: 512,
	}
}

func streamRequest() llm.Request {
	return llm.Request{Tier: llm.TierFlash, Parts: []llm.Part{{Text: "In one sentence, what is a kanban board?"}}, MaxOutputTokens: 256}
}

var (
	embedDocuments = []string{"Dark mode ships in v1.1.", "ตกลงเลื่อน dark mode ไป v1.1"}
	embedQuery     = []string{"When does dark mode ship?"}
)

// speech is 10.9 s of synthetic speech mixing Thai and English, as 16 kHz mono PCM16.
func speech(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, "speech.wav"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func speechAudio(t testing.TB) llm.Audio {
	return llm.Audio{Data: speech(t), MIME: "audio/wav", Name: "recording.wav"}
}

// --- fixtures ---

// recorded is what a fixture file holds that a test needs to read back.
type recorded struct {
	Body json.RawMessage `json:"body"`
	Text string          `json:"body_text"`
}

func readFixture(t *testing.T, name string) recorded {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, name))
	if err != nil {
		t.Fatalf("fixture %s: %v (record it: KIWI_LLM_RECORD=1 go test ./internal/platform/llm/openai -run TestRecord)", name, err)
	}
	var r recorded
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// message is the first choice of a recorded Chat Completions answer.
func (r recorded) message(t *testing.T) (content, reasoning, finish string) {
	t.Helper()
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(r.Body, &resp); err != nil || len(resp.Choices) == 0 {
		t.Fatalf("recorded answer has no choice: %v", err)
	}
	c := resp.Choices[0]
	return c.Message.Content, c.Message.ReasoningContent, c.FinishReason
}

// --- Generate ---

type meetingSummary struct {
	Summary   string `json:"summary"`
	Decisions []struct {
		Text      string `json:"text"`
		EntryRefs []int  `json:"entry_refs"`
	} `json:"decisions"`
	ActionItems []struct {
		Title     string `json:"title"`
		OwnerHint string `json:"owner_hint"`
		DueHint   string `json:"due_hint"`
		EntryRefs []int  `json:"entry_refs"`
	} `json:"action_items"`
}

var dueHint = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}|\+\d{1,3}d|next:(mon|tue|wed|thu|fri|sat|sun)|eow|none)$`)

func TestGenerate_JSONSchemaOutputParsesIntoDomainType(t *testing.T) {
	c, s := replayed(t)

	res, err := c.Generate(t.Context(), summaryRequest(llm.TierFlash))
	if err != nil {
		t.Fatal(err)
	}
	var got meetingSummary
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil {
		t.Fatalf("Text is not the requested JSON: %v\n%s", err, res.Text)
	}
	if strings.TrimSpace(got.Summary) == "" || len(got.Decisions) == 0 || len(got.ActionItems) == 0 {
		t.Fatalf("summary = %+v, want a summary, decisions and action items", got)
	}
	for _, a := range got.ActionItems {
		if a.Title == "" || !dueHint.MatchString(a.DueHint) {
			t.Errorf("action item %+v: want a title and a due_hint of the agreed grammar", a)
		}
	}
	if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Errorf("Usage = %+v, want the provider's counts", res.Usage)
	}

	body := s.only(t, "/v1/chat/completions").json(t)
	if body["model"] != flashModel {
		t.Errorf("model = %v, want the flash model", body["model"])
	}
	rf, _ := body["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "meeting_summary" || js["strict"] != true {
		t.Fatalf("response_format = %v, want a strict json_schema named meeting_summary", rf)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(summarySchemaJSON), &want)
	if fmt.Sprint(js["schema"]) != fmt.Sprint(want) {
		t.Errorf("json_schema.schema = %v, want the request's schema", js["schema"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %v, want a system and a user message", msgs)
	}
	parts, _ := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "text" {
		t.Errorf("user content = %v, want the two text parts", parts)
	}
}

func TestGenerate_NeverSurfacesReasoningContent(t *testing.T) {
	c, _ := replayed(t)

	content, reasoning, finish := readFixture(t, "generate_one_sentence-01.json").message(t)
	// The premise: the model did think, and did start its content with blank lines.
	if len(reasoning) < 40 || finish != "stop" || content == strings.TrimSpace(content) {
		t.Fatalf("the recorded answer has reasoning %d bytes, finish %q, content %q: not the shape this test needs", len(reasoning), finish, content)
	}

	res, err := c.Generate(t.Context(), oneSentenceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimLeft(content, " \t\r\n"); res.Text != want {
		t.Errorf("Text = %q, want the content with its leading blank lines trimmed: %q", res.Text, want)
	}
	if strings.Contains(res.Text, reasoning[:40]) {
		t.Errorf("Text contains the reasoning: %q", res.Text)
	}
}

func TestGenerate_ReasoningOnlyTruncation_ReturnsErrTruncated(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		c, _ := replayed(t)
		content, reasoning, finish := readFixture(t, "generate_truncated-01.json").message(t)
		if strings.TrimSpace(content) != "" || reasoning == "" || finish != "length" {
			t.Fatalf("the recorded answer has content %q, reasoning %d bytes, finish %q: not reasoning-only truncation", content, len(reasoning), finish)
		}

		res, err := c.Generate(t.Context(), truncatedRequest())
		if !errors.Is(err, llm.ErrTruncated) {
			t.Fatalf("Generate = %+v, %v, want llm.ErrTruncated", res, err)
		}
		if res.Text != "" {
			t.Errorf("Text = %q, want none", res.Text)
		}
	})
	t.Run("empty content, whatever the reasoning", func(t *testing.T) {
		c := newClient(t, newCanned(chatReply("\n\n", "thinking…", "length")))
		if _, err := c.Generate(t.Context(), oneSentenceRequest()); !errors.Is(err, llm.ErrTruncated) {
			t.Fatalf("Generate error = %v, want llm.ErrTruncated", err)
		}
	})
	t.Run("JSON cut off at the limit", func(t *testing.T) {
		c := newClient(t, newCanned(chatReply(`{"summary":"The team met and`, "", "length")))
		if _, err := c.Generate(t.Context(), summaryRequest(llm.TierFlash)); !errors.Is(err, llm.ErrTruncated) {
			t.Fatalf("Generate error = %v, want llm.ErrTruncated: half a JSON document is not an answer", err)
		}
	})
	t.Run("text the caller capped is returned", func(t *testing.T) {
		c := newClient(t, newCanned(chatReply("\n\nA kanban board is a", "", "length")))
		res, err := c.Generate(t.Context(), oneSentenceRequest())
		if err != nil || res.Text != "A kanban board is a" {
			t.Fatalf("Generate = %+v, %v, want the partial text: the caller asked for a limit", res, err)
		}
	})
	t.Run("an empty answer that stopped is not truncated", func(t *testing.T) {
		c := newClient(t, newCanned(chatReply("", "", "stop")))
		res, err := c.Generate(t.Context(), oneSentenceRequest())
		if err != nil || res.Text != "" {
			t.Fatalf("Generate = %+v, %v, want an empty answer and no error", res, err)
		}
	})
}

func TestGenerate_AddsReasoningHeadroom(t *testing.T) {
	c, s := replayed(t)
	if _, err := c.Generate(t.Context(), oneSentenceRequest()); err != nil {
		t.Fatal(err)
	}
	// Reasoning tokens count against max_tokens: the requested 256 plus 2048.
	if got := s.only(t, "/v1/chat/completions").json(t)["max_tokens"]; got != float64(256+2048) {
		t.Errorf("max_tokens = %v, want %d", got, 256+2048)
	}

	t.Run("no limit asked, none sent", func(t *testing.T) {
		cn := newCanned(chatReply("ok", "", "stop"))
		if _, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{Text: "hi"}}}); err != nil {
			t.Fatal(err)
		}
		body := cn.only(t, "/v1/chat/completions").json(t)
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if _, ok := body[k]; ok {
				t.Errorf("body has %s without a requested limit: %v", k, body)
			}
		}
	})
}

func TestGenerate_ToolCallsMapToADKFunctionCalls(t *testing.T) {
	t.Run("the client returns them as tool calls", func(t *testing.T) {
		c, s := replayed(t)
		res, err := c.Generate(t.Context(), toolRequest())
		if err != nil {
			t.Fatal(err)
		}
		if len(res.ToolCalls) != 1 {
			t.Fatalf("ToolCalls = %+v, want one call of search_knowledge", res.ToolCalls)
		}
		call := res.ToolCalls[0]
		var args struct {
			Query string `json:"query"`
		}
		if call.Name != "search_knowledge" || call.ID == "" || json.Unmarshal(call.Args, &args) != nil || !strings.Contains(strings.ToLower(args.Query), "dark mode") {
			t.Fatalf("tool call = %+v (args %s), want search_knowledge with a query about dark mode and an id", call, call.Args)
		}
		if strings.TrimSpace(res.Text) != "" && strings.Contains(res.Text, "search_knowledge") {
			t.Errorf("Text = %q: tool calls are not text", res.Text)
		}

		body := s.only(t, "/v1/chat/completions").json(t)
		tools, _ := body["tools"].([]any)
		fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" || fn["name"] != "search_knowledge" || fn["parameters"] == nil {
			t.Errorf("tools = %v, want the declaration as a function tool with its parameters", tools)
		}
	})

	t.Run("the ADK model returns FunctionCall parts and takes the response back", func(t *testing.T) {
		c, s := replayed(t)
		m, err := c.ADKModel(llm.TierFlash)
		if err != nil {
			t.Fatal(err)
		}
		if m.Name() != flashModel {
			t.Errorf("Name = %q, want the flash model id", m.Name())
		}
		first, second := adkRequests()

		call := lastPart(t, m, first, func(p *genai.Part) bool { return p.FunctionCall != nil })
		var args struct{ Query string }
		b, _ := json.Marshal(call.FunctionCall.Args)
		if call.FunctionCall.Name != "search_knowledge" || json.Unmarshal(b, &args) != nil || !strings.Contains(strings.ToLower(args.Query), "dark mode") {
			t.Fatalf("function call = %+v, want search_knowledge about dark mode", call.FunctionCall)
		}

		answer := lastPart(t, m, second, func(p *genai.Part) bool { return p.Text != "" && !p.Thought })
		if !strings.Contains(answer.Text, "MTG-1001") {
			t.Errorf("final answer = %q, want it to use the tool result", answer.Text)
		}

		// Chat Completions, with the key given explicitly, and not the Responses API
		// that is the ADK default.
		if got := s.to("/v1/responses"); len(got) != 0 {
			t.Fatalf("%d requests to /v1/responses, want none", len(got))
		}
		reqs := s.to("/v1/chat/completions")
		if len(reqs) != 2 {
			t.Fatalf("%d requests to /v1/chat/completions, want 2 (call, then answer)", len(reqs))
		}
		for _, r := range reqs {
			if r.Host != llmHost || r.Header.Get("Authorization") != "Bearer "+testKey {
				t.Errorf("ADK request to host %q with an unexpected credential", r.Host)
			}
		}
	})
}

// adkRequests are the two turns of a tool round trip as an agent runtime sends them:
// the question with a declared tool, then the same plus the call and its result.
func adkRequests() (first, second *model.LLMRequest) {
	decl := &genai.FunctionDeclaration{
		Name:        "search_knowledge",
		Description: "Search the workspace's meetings and files for a passage that answers the query.",
		ParametersJsonSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []any{"query"},
		},
	}
	// MaxOutputTokens: thinking is on and counts against it; below about 1100 the
	// ADK model reports an answer with no text.
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens:   2048,
		SystemInstruction: genai.NewContentFromText("Answer from the workspace's knowledge. Call search_knowledge first and cite the REF you get.", genai.RoleUser),
		Tools:             []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{decl}}},
	}
	question := genai.NewContentFromText("What did we decide about dark mode?", genai.RoleUser)
	first = &model.LLMRequest{Contents: []*genai.Content{question}, Config: cfg}
	call := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "call_1", Name: "search_knowledge", Args: map[string]any{"query": "dark mode decision"},
	}}}}
	result := genai.NewContentFromParts([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{
		ID: "call_1", Name: "search_knowledge",
		Response: map[string]any{"result": "MTG-1001#12: dark mode moves from v1.0 to v1.1."},
	}}}, genai.RoleUser)
	second = &model.LLMRequest{Contents: []*genai.Content{question, call, result}, Config: cfg}
	return first, second
}

// lastPart runs req on m (not streaming) and returns the last part that matches.
func lastPart(t *testing.T, m model.LLM, req *model.LLMRequest, match func(*genai.Part) bool) *genai.Part {
	t.Helper()
	var found *genai.Part
	for resp, err := range m.GenerateContent(t.Context(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if match(p) {
				found = p
			}
		}
	}
	if found == nil {
		t.Fatal("the ADK model answered with no matching part")
	}
	return found
}

func TestGenerate_ToolCallArgumentsThatAreNotJSONAreAnError(t *testing.T) {
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"search_knowledge","arguments":"{\"query\": "}}]}}]}`
	c := newClient(t, newCanned(jsonReply(body)))
	if _, err := c.Generate(t.Context(), toolRequest()); err == nil || !strings.Contains(err.Error(), "search_knowledge") {
		t.Fatalf("Generate error = %v, want one naming the tool", err)
	}
}

func TestGenerate_ToolCallWithNoArgumentsGetsAnEmptyObject(t *testing.T) {
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"list_channels","arguments":""}}]}}]}`
	c := newClient(t, newCanned(jsonReply(body)))
	res, err := c.Generate(t.Context(), toolRequest())
	if err != nil || len(res.ToolCalls) != 1 || string(res.ToolCalls[0].Args) != "{}" {
		t.Fatalf("Generate = %+v, %v, want one call with the arguments {}", res, err)
	}
}

// --- structured output on the pro tier ---

func TestGenerate_ProUsesJSONObjectWithTheSchemaInThePrompt(t *testing.T) {
	c, s := replayed(t)

	res, err := c.Generate(t.Context(), summaryRequest(llm.TierPro))
	if err != nil {
		t.Fatal(err)
	}
	var got meetingSummary
	if err := json.Unmarshal([]byte(res.Text), &got); err != nil || got.Summary == "" || len(got.ActionItems) == 0 {
		t.Fatalf("Text = %q (%v), want the requested JSON", res.Text, err)
	}

	body := s.only(t, "/v1/chat/completions").json(t)
	if body["model"] != proModel {
		t.Errorf("model = %v, want the pro model", body["model"])
	}
	// json_schema is unreliable on pro (runaway output, scrambled fields): json_object
	// and a prompt that describes every field.
	if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_object" || rf["json_schema"] != nil {
		t.Fatalf("response_format = %v, want {type: json_object}", rf)
	}
	system := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(system, summarySystem) {
		t.Errorf("the system prompt does not start with the caller's:\n%s", system)
	}
	described := system[len(summarySystem):]
	prev := -1
	for _, field := range []string{`"summary"`, `"decisions"`, `"action_items"`} {
		i := strings.Index(described, field)
		if i <= prev {
			t.Fatalf("%s is described at %d, not after the field before it at %d (or not at all):\n%s", field, i, prev, system)
		}
		prev = i
	}
	for _, field := range []string{`"text"`, `"entry_refs"`, `"title"`, `"owner_hint"`, `"due_hint"`} {
		if !strings.Contains(described, field) {
			t.Fatalf("the system prompt does not describe %s:\n%s", field, system)
		}
	}
	if !strings.Contains(system, "^(") || !strings.Contains(system, "required") {
		t.Errorf("the prompt must carry the due_hint pattern and say which fields are required:\n%s", system)
	}
}

func TestGenerate_ProRetriesTruncatedAndInvalidAnswers(t *testing.T) {
	good := `{"summary":"s","decisions":[],"action_items":[{"title":"t","owner_hint":"","due_hint":"eow","entry_refs":[1]}]}`
	cn := newCanned(
		chatReply(strings.Repeat(" ", 50), "", "length"),        // ran away to the limit
		chatReply(`{"summary":"s","decisions":[]}`, "", "stop"), // valid JSON, but action_items is missing
		chatReply("\n\n"+good, "", "stop"),
	)
	c := newClient(t, cn)

	res, err := c.Generate(t.Context(), summaryRequest(llm.TierPro))
	if err != nil || res.Text != good {
		t.Fatalf("Generate = %q, %v, want the third answer", res.Text, err)
	}
	if n := len(cn.to("/v1/chat/completions")); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	// Each attempt is the same request: the model is asked again, not differently.
	reqs := cn.to("/v1/chat/completions")
	if !bytes.Equal(reqs[0].Body, reqs[1].Body) || !bytes.Equal(reqs[1].Body, reqs[2].Body) {
		t.Error("the attempts are not identical requests")
	}
	if res.Usage.InputTokens != 3*11 || res.Usage.OutputTokens != 3*7 {
		t.Errorf("Usage = %+v, want the tokens of all three attempts", res.Usage)
	}
}

func TestGenerate_ProValidatesAgainstTheSchema(t *testing.T) {
	for name, answer := range map[string]string{
		"not JSON":                  "Sure! Here is the summary.",
		"a JSON array":              `[]`,
		"a missing field":           `{"summary":"s","decisions":[]}`,
		"an extra field":            `{"summary":"s","decisions":[],"action_items":[],"mood":"good"}`,
		"a field of the wrong type": `{"summary":3,"decisions":[],"action_items":[]}`,
		"a due_hint off grammar":    `{"summary":"s","decisions":[],"action_items":[{"title":"t","owner_hint":"","due_hint":"tomorrow-ish","entry_refs":[]}]}`,
		"text after the document":   `{"summary":"s","decisions":[],"action_items":[]} Hope that helps.`,
	} {
		t.Run(name, func(t *testing.T) {
			cn := newCanned(chatReply(answer, "", "stop"), chatReply(answer, "", "stop"), chatReply(answer, "", "stop"), chatReply(answer, "", "stop"))
			_, err := newClient(t, cn).Generate(t.Context(), summaryRequest(llm.TierPro))
			if err == nil {
				t.Fatal("Generate accepted an answer that does not fit the schema")
			}
			if errors.Is(err, llm.ErrTruncated) {
				t.Errorf("error = %v: an invalid answer is not a truncated one", err)
			}
			if n := len(cn.to("/v1/chat/completions")); n != 3 {
				t.Errorf("requests = %d, want the 3 attempts a call is bounded to", n)
			}
		})
	}
}

func TestGenerate_ProGivesUpAfterBoundedAttempts(t *testing.T) {
	t.Run("the last attempt ran out of tokens", func(t *testing.T) {
		cn := newCanned(chatReply("nope", "", "stop"), chatReply("nope", "", "stop"), chatReply("", "thinking", "length"), chatReply("{}", "", "stop"))
		_, err := newClient(t, cn).Generate(t.Context(), summaryRequest(llm.TierPro))
		if !errors.Is(err, llm.ErrTruncated) {
			t.Fatalf("Generate error = %v, want llm.ErrTruncated", err)
		}
		if n := len(cn.to("/v1/chat/completions")); n != 3 {
			t.Fatalf("requests = %d, want 3", n)
		}
	})
	t.Run("HTTP failures are not retried here", func(t *testing.T) {
		cn := newCanned(statusReply(503, `{"error":"busy"}`), chatReply("{}", "", "stop"))
		_, err := newClient(t, cn).Generate(t.Context(), summaryRequest(llm.TierPro))
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 503 || len(cn.to("/v1/chat/completions")) != 1 {
			t.Fatalf("Generate error = %v after %d requests, want the HTTP 503 after one: llm.WithRetry owns that", err, len(cn.to("/v1/chat/completions")))
		}
	})
}

func TestGenerate_FlashSchemaIsNotValidatedAndNotRetried(t *testing.T) {
	// json_schema is enforced by the server on flash; the caller parses what it gets.
	cn := newCanned(chatReply(`{"summary":3}`, "", "stop"))
	res, err := newClient(t, cn).Generate(t.Context(), summaryRequest(llm.TierFlash))
	if err != nil || res.Text != `{"summary":3}` || len(cn.to("/v1/chat/completions")) != 1 {
		t.Fatalf("Generate = %q, %v after %d requests, want the answer as it came, once", res.Text, err, len(cn.to("/v1/chat/completions")))
	}
}

func TestGenerate_UnusableRequests(t *testing.T) {
	c := newClient(t, newCanned())
	for name, req := range map[string]llm.Request{
		"an unknown tier":           {Tier: "huge", Parts: []llm.Part{{Text: "hi"}}},
		"nothing to say":            {Tier: llm.TierFlash},
		"an empty part":             {Parts: []llm.Part{{}}},
		"a schema without a name":   {Parts: []llm.Part{{Text: "hi"}}, Schema: &llm.Schema{JSON: json.RawMessage(`{}`)}},
		"a schema that is not JSON": {Parts: []llm.Part{{Text: "hi"}}, Schema: &llm.Schema{Name: "s", JSON: json.RawMessage(`{`)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Generate(t.Context(), req); err == nil {
				t.Fatal("Generate accepted the request")
			}
		})
	}
	if n := c.hc.Transport.(*canned).count(); n != 0 {
		t.Fatalf("%d requests sent for requests that cannot be built, want 0", n)
	}
}

func TestTierDefaultsToFlash(t *testing.T) {
	cn := newCanned(chatReply("ok", "", "stop"))
	if _, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if got := cn.only(t, "/v1/chat/completions").json(t)["model"]; got != flashModel {
		t.Fatalf("model = %v, want the flash model for a request without a tier", got)
	}
}

// --- failures ---

func TestHTTPFailuresReturnHTTPError(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := newClient(t, newCanned(statusReply(status, `{"error":{"message":"No deployments available"}}`)))
			_, err := c.Generate(t.Context(), oneSentenceRequest())
			var he *llm.HTTPError
			if !errors.As(err, &he) || he.Status != status || !strings.Contains(he.Body, "No deployments available") {
				t.Fatalf("Generate error = %v, want *llm.HTTPError with status %d and the body", err, status)
			}
		})
	}
}

func TestErrorsNameNeitherHostNorKey(t *testing.T) {
	boom := errors.New("connection refused")
	// A network error names the address it dialled, and a gateway's error page the
	// upstream it could not reach.
	dialFails := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp %s: %w", r.URL.Host, boom)
	})
	gatewayFails := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return httpResponse(statusReply(502, `{"error":"upstream `+r.URL.Host+` is down"}`), r), nil
	})
	for name, call := range map[string]func(*client) error{
		"Generate": func(c *client) error { _, err := c.Generate(t.Context(), oneSentenceRequest()); return err },
		"Stream":   func(c *client) error { _, err := c.Stream(t.Context(), oneSentenceRequest(), nil); return err },
		"Embed":    func(c *client) error { _, err := c.Embed(t.Context(), []string{"a"}, llm.EmbedQuery); return err },
		"Transcribe": func(c *client) error {
			_, err := c.Transcribe(t.Context(), speechAudio(t))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			for what, rt := range map[string]http.RoundTripper{"a failed dial": dialFails, "a gateway error": gatewayFails} {
				err := call(newClient(t, rt))
				if err == nil {
					t.Fatalf("%s: no error", what)
				}
				if what == "a failed dial" && !errors.Is(err, boom) {
					t.Errorf("%s: error = %v, want the transport's error to be reachable", what, err)
				}
				var he *llm.HTTPError
				if what == "a gateway error" && (!errors.As(err, &he) || he.Status != 502 || !strings.Contains(he.Body, "is down")) {
					t.Errorf("%s: error = %v, want the HTTP 502 with the rest of its body", what, err)
				}
				text := err.Error()
				if he != nil {
					text += he.Body
				}
				for _, secret := range []string{llmHost, embedHost, testKey, "Bearer"} {
					if strings.Contains(text, secret) {
						t.Errorf("%s: error %q contains %q", what, text, secret)
					}
				}
			}
		})
	}
}

// A model call of the agents runs through the ADK, whose SDK puts the URL it called into
// the errors it returns and a gateway's own words into the ones it reads: those reach the
// log of a failed run, so they carry neither the host nor the key either.
func TestADKModel_ErrorsNameNeitherHostNorKey(t *testing.T) {
	boom := errors.New("connection refused")
	dialFails := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp %s: %w", r.URL.Host, boom)
	})
	// A gateway that says which upstream it could not reach, and repeats the key it was given.
	gatewayFails := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"error":{"message":"Incorrect API key ` + testKey + ` for ` + r.URL.Host + `"}}`
		return httpResponse(statusReply(401, body), r), nil
	})
	for what, rt := range map[string]http.RoundTripper{"a failed dial": dialFails, "a gateway error": gatewayFails} {
		t.Run(what, func(t *testing.T) {
			m, err := newClient(t, rt).ADKModel(llm.TierFlash)
			if err != nil {
				t.Fatal(err)
			}
			first, _ := adkRequests()
			var got error
			for _, err := range m.GenerateContent(t.Context(), first, false) {
				got = errors.Join(got, err)
			}
			if got == nil {
				t.Fatal("GenerateContent reported no error")
			}
			for _, secret := range []string{llmHost, testKey, "Bearer"} {
				if strings.Contains(got.Error(), secret) {
					t.Errorf("error %q contains %q", got, secret)
				}
			}
		})
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := newClient(t, newCanned(chatReply("ok", "", "stop")))
	if _, err := c.Generate(ctx, oneSentenceRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context.Canceled", err)
	}
}

// --- Stream ---

func TestStream_YieldsContentDeltasOnly(t *testing.T) {
	c, s := replayed(t)

	// The premise: the recorded stream opens with reasoning_content deltas.
	var reasoning strings.Builder
	var frames int
	for line := range strings.SplitSeq(readFixture(t, "stream_one_sentence-01.json").Text, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var f struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			t.Fatalf("recorded frame %q: %v", payload, err)
		}
		frames++
		for _, ch := range f.Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
		}
	}
	if reasoning.Len() < 40 || frames < 10 {
		t.Fatalf("the recorded stream has %d frames and %d bytes of reasoning: not the shape this test needs", frames, reasoning.Len())
	}

	var deltas []string
	res, err := c.Stream(t.Context(), streamRequest(), func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(deltas, "")
	if len(deltas) < 2 || joined == "" || res.Text != joined {
		t.Fatalf("deltas = %q, Text = %q, want several deltas that add up to the text", deltas, res.Text)
	}
	if strings.TrimLeft(deltas[0], " \t\r\n") != deltas[0] {
		t.Errorf("first delta = %q: the model's leading blank lines are not text", deltas[0])
	}
	if strings.Contains(joined, reasoning.String()[:30]) {
		t.Errorf("the deltas contain the reasoning: %q", joined)
	}
	if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Errorf("Usage = %+v, want the counts of the final usage frame", res.Usage)
	}

	body := s.only(t, "/v1/chat/completions").json(t)
	opts, _ := body["stream_options"].(map[string]any)
	if body["stream"] != true || opts["include_usage"] != true || body["max_tokens"] != float64(256+2048) {
		t.Errorf("body = %v, want stream with include_usage and the headroom", body)
	}
}

func sse(frames ...string) reply {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return reply{status: 200, contentType: "text/event-stream", body: b.String()}
}

func TestStream_Edges(t *testing.T) {
	t.Run("blank lines before the text are not yielded", func(t *testing.T) {
		cn := newCanned(sse(
			`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
			`{"choices":[{"delta":{"content":"\n"}}]}`,
			`{"choices":[{"delta":{"content":"\n"}}]}`,
			`{"choices":[{"delta":{"content":"  Hello"}}]}`,
			`{"choices":[{"delta":{"content":", world"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		))
		var deltas []string
		res, err := newClient(t, cn).Stream(t.Context(), oneSentenceRequest(), func(d string) { deltas = append(deltas, d) })
		if err != nil || res.Text != "Hello, world" || strings.Join(deltas, "") != "Hello, world" || deltas[0] != "Hello" {
			t.Fatalf("Stream = %q, %v, deltas %q, want Hello, world with no leading blanks", res.Text, err, deltas)
		}
		if res.Usage != (llm.Usage{InputTokens: 5, OutputTokens: 3}) {
			t.Errorf("Usage = %+v", res.Usage)
		}
	})
	t.Run("comments, blank lines and a nil callback", func(t *testing.T) {
		cn := newCanned(reply{status: 200, contentType: "text/event-stream", body: ": keep-alive\n\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"})
		res, err := newClient(t, cn).Stream(t.Context(), oneSentenceRequest(), nil)
		if err != nil || res.Text != "hi" {
			t.Fatalf("Stream = %+v, %v", res, err)
		}
	})
	t.Run("only reasoning, then the limit", func(t *testing.T) {
		cn := newCanned(sse(
			`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		))
		var n int
		_, err := newClient(t, cn).Stream(t.Context(), oneSentenceRequest(), func(string) { n++ })
		if !errors.Is(err, llm.ErrTruncated) || n != 0 {
			t.Fatalf("Stream error = %v after %d deltas, want llm.ErrTruncated and no delta", err, n)
		}
	})
	t.Run("tool calls arrive in pieces", func(t *testing.T) {
		cn := newCanned(sse(
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"search_knowledge","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"dark mode\"}"}}]},"finish_reason":"tool_calls"}]}`,
		))
		res, err := newClient(t, cn).Stream(t.Context(), toolRequest(), func(string) {})
		if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "c1" || res.ToolCalls[0].Name != "search_knowledge" || string(res.ToolCalls[0].Args) != `{"query":"dark mode"}` {
			t.Fatalf("Stream = %+v, %v, want the call put together", res, err)
		}
	})
	t.Run("an HTTP failure before the stream", func(t *testing.T) {
		_, err := newClient(t, newCanned(statusReply(429, "slow down"))).Stream(t.Context(), oneSentenceRequest(), nil)
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 429 || he.Body != "slow down" {
			t.Fatalf("Stream error = %v, want *llm.HTTPError 429", err)
		}
	})
	t.Run("a stream that ends without [DONE]", func(t *testing.T) {
		cn := newCanned(reply{status: 200, contentType: "text/event-stream", body: "data: {\"choices\":[{\"delta\":{\"content\":\"cut\"}}]}\n\n"})
		if _, err := newClient(t, cn).Stream(t.Context(), oneSentenceRequest(), nil); err == nil {
			t.Fatal("Stream accepted a stream that ended before [DONE]")
		}
	})
	t.Run("a frame that is not JSON", func(t *testing.T) {
		cn := newCanned(reply{status: 200, contentType: "text/event-stream", body: "data: {nope\n\n"})
		if _, err := newClient(t, cn).Stream(t.Context(), oneSentenceRequest(), nil); err == nil {
			t.Fatal("Stream accepted a frame that is not JSON")
		}
	})
}

// --- Embed ---

func TestEmbed_UsesPrefixesAndRejectsWrongDimension(t *testing.T) {
	c, s := replayed(t)

	docs, err := c.Embed(t.Context(), embedDocuments, llm.EmbedDocument)
	if err != nil {
		t.Fatal(err)
	}
	query, err := c.Embed(t.Context(), embedQuery, llm.EmbedQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || len(query) != 1 {
		t.Fatalf("vectors = %d and %d, want 2 and 1, in order", len(docs), len(query))
	}
	for _, v := range append(docs, query...) {
		if len(v) != 768 {
			t.Fatalf("vector has %d dimensions, want 768", len(v))
		}
	}

	reqs := s.to("/v1/embeddings")
	if len(reqs) != 2 {
		t.Fatalf("%d requests to /v1/embeddings, want 2", len(reqs))
	}
	for i, want := range [][]string{
		{"title: none | text: " + embedDocuments[0], "title: none | text: " + embedDocuments[1]},
		{"task: search result | query: " + embedQuery[0]},
	} {
		body := reqs[i].json(t)
		if body["model"] != embedModel || fmt.Sprint(body["input"]) != fmt.Sprint(toAny(want)) {
			t.Errorf("request %d body = %v, want model %s and input %q", i+1, body, embedModel, want)
		}
		if reqs[i].Host != embedHost {
			t.Errorf("request %d went to %q, want the embedding host", i+1, reqs[i].Host)
		}
		// Ollama gets no credential: the LiteLLM key stays with LiteLLM.
		if got := reqs[i].Header.Get("Authorization"); got != "" {
			t.Errorf("request %d carries an Authorization header", i+1)
		}
	}
	if got := c.EmbeddingModel(); got != embedModel {
		t.Errorf("EmbeddingModel = %q, want the configured model", got)
	}

	t.Run("a vector of the wrong dimension is an error", func(t *testing.T) {
		cn := newCanned(jsonReply(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
		_, err := newClient(t, cn).Embed(t.Context(), []string{"a"}, llm.EmbedDocument)
		if err == nil || !strings.Contains(err.Error(), "768") {
			t.Fatalf("Embed error = %v, want one naming the 768 dimensions", err)
		}
	})
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func TestEmbed_Edges(t *testing.T) {
	vec := func(first float32) string {
		v := make([]float32, 768)
		v[0] = first
		b, _ := json.Marshal(v)
		return string(b)
	}
	t.Run("no texts, no request", func(t *testing.T) {
		cn := newCanned()
		got, err := newClient(t, cn).Embed(t.Context(), nil, llm.EmbedDocument)
		if err != nil || len(got) != 0 || cn.count() != 0 {
			t.Fatalf("Embed(nil) = %v, %v with %d requests, want nothing", got, err, cn.count())
		}
	})
	t.Run("vectors come back in the order of the texts", func(t *testing.T) {
		cn := newCanned(jsonReply(`{"data":[{"index":1,"embedding":` + vec(2) + `},{"index":0,"embedding":` + vec(1) + `}]}`))
		got, err := newClient(t, cn).Embed(t.Context(), []string{"a", "b"}, llm.EmbedDocument)
		if err != nil || len(got) != 2 || got[0][0] != 1 || got[1][0] != 2 {
			t.Fatalf("Embed = %v, %v, want the vectors sorted by index", got, err)
		}
	})
	t.Run("a wrong number of vectors is an error", func(t *testing.T) {
		cn := newCanned(jsonReply(`{"data":[{"index":0,"embedding":` + vec(1) + `}]}`))
		if _, err := newClient(t, cn).Embed(t.Context(), []string{"a", "b"}, llm.EmbedDocument); err == nil {
			t.Fatal("Embed accepted one vector for two texts")
		}
	})
	t.Run("an index that is out of range or repeated is an error", func(t *testing.T) {
		cn := newCanned(jsonReply(`{"data":[{"index":0,"embedding":`+vec(1)+`},{"index":0,"embedding":`+vec(2)+`}]}`), jsonReply(`{"data":[{"index":5,"embedding":`+vec(1)+`}]}`))
		c := newClient(t, cn)
		if _, err := c.Embed(t.Context(), []string{"a", "b"}, llm.EmbedDocument); err == nil {
			t.Error("Embed accepted a repeated index")
		}
		if _, err := c.Embed(t.Context(), []string{"a"}, llm.EmbedDocument); err == nil {
			t.Error("Embed accepted an index out of range")
		}
	})
	t.Run("an HTTP failure", func(t *testing.T) {
		_, err := newClient(t, newCanned(statusReply(503, "loading"))).Embed(t.Context(), []string{"a"}, llm.EmbedQuery)
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 503 {
			t.Fatalf("Embed error = %v, want *llm.HTTPError 503", err)
		}
	})
}

// --- Transcribe ---

// wav builds a WAV file: format 1 (PCM) unless fmtTag says otherwise, with before
// chunks in front of the data chunk and after chunks behind it.
func wav(rate uint32, channels, bits uint16, data []byte, before, after [][]byte) []byte {
	return wavTagged(1, rate, channels, bits, data, before, after, uint32(len(data)))
}

func wavTagged(tag uint16, rate uint32, channels, bits uint16, data []byte, before, after [][]byte, dataSize uint32) []byte {
	le := binary.LittleEndian
	chunk := func(id string, body []byte) []byte {
		b := append([]byte(id), le.AppendUint32(nil, uint32(len(body)))...)
		b = append(b, body...)
		if len(body)%2 == 1 {
			b = append(b, 0)
		}
		return b
	}
	fmtBody := le.AppendUint16(nil, tag)
	fmtBody = le.AppendUint16(fmtBody, channels)
	fmtBody = le.AppendUint32(fmtBody, rate)
	fmtBody = le.AppendUint32(fmtBody, rate*uint32(channels)*uint32(bits/8))
	fmtBody = le.AppendUint16(fmtBody, channels*bits/8)
	fmtBody = le.AppendUint16(fmtBody, bits)

	body := []byte("WAVE")
	body = append(body, chunk("fmt ", fmtBody)...)
	for _, c := range before {
		body = append(body, c...)
	}
	body = append(body, append([]byte("data"), le.AppendUint32(nil, dataSize)...)...)
	body = append(body, data...)
	if len(data)%2 == 1 {
		body = append(body, 0)
	}
	for _, c := range after {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), le.AppendUint32(nil, uint32(len(body)))...), body...)
}

// tone is n samples of a ramp, so that every sample is distinguishable and chunks can be
// compared with the source byte for byte.
func tone(n int) []byte {
	b := make([]byte, 2*n)
	for i := range n {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(i%30000+1))
	}
	return b
}

func transcribeReply(text string) reply {
	b, _ := json.Marshal(map[string]any{"text": text, "language": "English"})
	return jsonReply(string(b))
}

func TestTranscribe_SplitsThaiAndEnglishSentences_NullSpeakerTiming(t *testing.T) {
	c, s := replayed(t)

	entries, err := c.Transcribe(t.Context(), speechAudio(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("entries = %+v, want the transcript split into sentences", entries)
	}
	var thai, latin bool
	var joined []string
	for _, e := range entries {
		if e.Speaker != "" || e.StartMS != nil || e.EndMS != nil {
			t.Errorf("entry %+v: the provider tells no speaker and no timing, want them unset", e)
		}
		if strings.TrimSpace(e.Text) != e.Text || e.Text == "" || utf8.RuneCountInString(e.Text) > 160 {
			t.Errorf("entry text %q: want it trimmed, non-empty and at most 160 runes", e.Text)
		}
		thai = thai || regexp.MustCompile(`\p{Thai}`).MatchString(e.Text)
		latin = latin || regexp.MustCompile(`[A-Za-z]{3}`).MatchString(e.Text)
		joined = append(joined, e.Text)
	}
	if !thai || !latin {
		t.Errorf("entries = %q: want both Thai and English in them (language=en keeps the English)", joined)
	}

	// What went over the wire: one chunk, as a WAV, with the model and the language.
	fields, name, file := s.only(t, "/v1/audio/transcriptions").form(t)
	if fields["model"] != asrModel || fields["language"] != "en" {
		t.Errorf("form fields = %v, want model %s and language en", fields, asrModel)
	}
	if name == "" || !bytes.HasPrefix(file, []byte("RIFF")) || !bytes.Equal(file, speech(t)) {
		t.Errorf("file %q of %d bytes: want the 10.9 s recording, as it is, in a WAV", name, len(file))
	}
	if got := s.to("/v1/audio/transcriptions")[0].Host; got != llmHost {
		t.Errorf("sent to %q, want the LLM host", got)
	}
}

func TestTranscribe_ChunksLongAudioInto20SecondWindows(t *testing.T) {
	const k = 1000
	for _, tc := range []struct {
		name     string
		rate     uint32
		channels uint16
		chunks   []int // frames per chunk
	}{
		{"16 kHz mono, 65 s", 16000, 1, []int{20 * 16 * k, 20 * 16 * k, 20 * 16 * k, 5 * 16 * k}},
		{"44.1 kHz stereo, 41 s: a tail of a second is a chunk of its own", 44100, 2, []int{20 * 44100, 20 * 44100, 44100}},
		{"exactly one window", 16000, 1, []int{20 * 16 * k}},
		{"a tail under a second joins the last window: 20.5 s is one request", 16000, 1, []int{20*16*k + 8*k}},
		{"a tail under a second joins the last window: 40.5 s is two", 16000, 1, []int{20 * 16 * k, 20*16*k + 8*k}},
		{"a quarter of a second, the least a recording has", 16000, 1, []int{4 * k}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, total := int(tc.channels)*2, 0
			for _, n := range tc.chunks {
				total += n
			}
			data := tone(total * int(tc.channels))
			audio := wav(tc.rate, tc.channels, 16, data, nil, nil)

			replies := make([]reply, len(tc.chunks))
			for i := range replies {
				replies[i] = transcribeReply(fmt.Sprintf("Chunk %d is here.", i+1))
			}
			cn := newCanned(replies...)
			entries, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: audio, MIME: "audio/wav", Name: "m.wav"})
			if err != nil {
				t.Fatal(err)
			}

			reqs := cn.to("/v1/audio/transcriptions")
			if len(reqs) != len(tc.chunks) {
				t.Fatalf("%d requests, want %d", len(reqs), len(tc.chunks))
			}
			var rejoined []byte
			for i, r := range reqs {
				fields, _, file := r.form(t)
				if fields["model"] != asrModel || fields["language"] != "en" {
					t.Errorf("chunk %d fields = %v", i+1, fields)
				}
				pcm := pcmOf(t, file, tc.rate, tc.channels)
				rejoined = append(rejoined, pcm...)
				// 20 s windows; the server cuts a long input silently, so the hard
				// maximum is 30 s.
				if got := len(pcm) / frame; got != tc.chunks[i] || got > 30*int(tc.rate) {
					t.Errorf("chunk %d holds %d frames (%.2f s), want %d", i+1, got, float64(got)/float64(tc.rate), tc.chunks[i])
				}
			}
			if !bytes.Equal(rejoined, data) {
				t.Errorf("the chunks put together are %d bytes and not the %d bytes of the recording", len(rejoined), len(data))
			}
			if len(entries) != len(tc.chunks) {
				t.Fatalf("entries = %+v, want one sentence per chunk, in order", entries)
			}
			for i, e := range entries {
				if e.Text != fmt.Sprintf("Chunk %d is here.", i+1) {
					t.Errorf("entry %d = %q, want chunk %d's text", i, e.Text, i+1)
				}
			}
			if cn.peak.Load() != 1 {
				t.Errorf("up to %d requests at once, want one at a time", cn.peak.Load())
			}
		})
	}
}

// pcmOf checks that file is a canonical PCM16 WAV of the given format and returns its audio.
func pcmOf(t *testing.T, file []byte, rate uint32, channels uint16) []byte {
	t.Helper()
	le := binary.LittleEndian
	if len(file) < 44 || string(file[0:4]) != "RIFF" || string(file[8:16]) != "WAVEfmt " || string(file[36:40]) != "data" {
		t.Fatalf("chunk is not a canonical WAV: % x", file[:min(len(file), 44)])
	}
	if le.Uint16(file[20:]) != 1 || le.Uint16(file[22:]) != channels || le.Uint32(file[24:]) != rate || le.Uint16(file[34:]) != 16 {
		t.Fatalf("chunk format = tag %d, %d channels, %d Hz, %d bits", le.Uint16(file[20:]), le.Uint16(file[22:]), le.Uint32(file[24:]), le.Uint16(file[34:]))
	}
	if size := int(le.Uint32(file[40:])); size != len(file)-44 || int(le.Uint32(file[4:])) != len(file)-8 {
		t.Fatalf("chunk sizes say %d data bytes and %d in the RIFF, the file has %d bytes", size, le.Uint32(file[4:]), len(file))
	}
	return file[44:]
}

func TestTranscribe_ReadsTheWAVsItSupports(t *testing.T) {
	data := tone(16000 * 2) // 2 s mono at 16 kHz
	for name, audio := range map[string][]byte{
		"a plain WAV":                          wav(16000, 1, 16, data, nil, nil),
		"a LIST chunk before the data":         wav(16000, 1, 16, data, [][]byte{append([]byte("LIST"), append([]byte{5, 0, 0, 0}, []byte("abcde\x00")...)...)}, nil),
		"a trailing chunk after the data":      wav(16000, 1, 16, data, nil, [][]byte{append([]byte("LIST"), append([]byte{4, 0, 0, 0}, []byte("tail")...)...)}),
		"a streaming writer's 0xFFFFFFFF size": wavTagged(1, 16000, 1, 16, data, nil, nil, 0xFFFFFFFF),
		"a streaming writer's zero size":       wavTagged(1, 16000, 1, 16, data, nil, nil, 0),
	} {
		t.Run(name, func(t *testing.T) {
			cn := newCanned(transcribeReply("Hello there."))
			entries, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: audio, MIME: "audio/wav", Name: "m.wav"})
			if err != nil || len(entries) != 1 {
				t.Fatalf("Transcribe = %+v, %v, want one entry", entries, err)
			}
			_, _, file := cn.only(t, "/v1/audio/transcriptions").form(t)
			if got := pcmOf(t, file, 16000, 1); !bytes.Equal(got, data) {
				t.Errorf("chunk audio is %d bytes, want the %d of the source", len(got), len(data))
			}
		})
	}
}

func TestTranscribe_RefusesWhatIsNotPCM16WAV_WithoutARequest(t *testing.T) {
	data := tone(16000)
	mp4 := append([]byte{0, 0, 0, 0x20}, []byte("ftypisom\x00\x00\x02\x00isomiso2avc1mp41")...)
	for name, audio := range map[string][]byte{
		"nothing":                            nil,
		"a header only":                      []byte("RIFF"),
		"an MP4":                             mp4,
		"WebM":                               append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 100)...),
		"an MP3 with an ID3 tag":             append([]byte("ID3\x04\x00\x00"), make([]byte, 100)...),
		"RIFF but not WAVE":                  append([]byte("RIFF\x24\x00\x00\x00AVI "), make([]byte, 100)...),
		"a WAV of 32-bit float":              wavTagged(3, 16000, 1, 32, data, nil, nil, uint32(len(data))),
		"a WAV of 8-bit samples":             wav(16000, 1, 8, data, nil, nil),
		"a WAV of 24-bit samples":            wav(16000, 1, 24, data, nil, nil),
		"a µ-law WAV":                        wavTagged(7, 8000, 1, 8, data, nil, nil, uint32(len(data))),
		"a WAV with six channels":            wav(16000, 6, 16, data, nil, nil),
		"a WAV at 0 Hz":                      wav(0, 1, 16, data, nil, nil),
		"a WAV without a data chunk":         wav(16000, 1, 16, nil, [][]byte{}, nil)[:36],
		"a WAV with an empty data":           wav(16000, 1, 16, nil, nil, nil),
		"a WAV of one byte of audio":         wavTagged(1, 16000, 1, 16, []byte{1}, nil, nil, 1),
		"a data size that overruns the file": wavTagged(1, 16000, 1, 16, data, nil, nil, uint32(len(data))+1000),
		"a WAV at 1 Hz":                      wav(1, 1, 16, data, nil, nil),
		"a WAV at 7999 Hz":                   wav(7999, 1, 16, data, nil, nil),
		"a WAV at 96001 Hz":                  wav(96001, 1, 16, data, nil, nil),
		"a tenth of a second of audio":       wav(16000, 1, 16, tone(1600), nil, nil),
		"data before fmt": append([]byte("RIFF\x30\x00\x00\x00WAVEdata\x04\x00\x00\x00\x01\x02\x03\x04fmt \x10\x00\x00\x00"),
			1, 0, 1, 0, 0x80, 0x3e, 0, 0, 0, 0x7d, 0, 0, 2, 0, 16, 0),
	} {
		t.Run(name, func(t *testing.T) {
			cn := newCanned(transcribeReply("never"))
			_, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: audio, MIME: "audio/wav", Name: "m.wav"})
			// An undecodable body sent to the ASR is an HTTP 500 that puts the shared
			// deployment into a 60 s cooldown for everyone: no request may be made.
			if !errors.Is(err, llm.ErrUnsupportedByProvider) {
				t.Fatalf("Transcribe error = %v, want llm.ErrUnsupportedByProvider", err)
			}
			if cn.count() != 0 {
				t.Fatalf("%d requests sent, want none", cn.count())
			}
		})
	}
}

func TestTranscribe_WaitsOutTheASRCooldown(t *testing.T) {
	audio := speechAudio(t)
	cooldown := statusReply(429, `{"error":{"message":"No deployments available for selected model, Try again in 60 seconds","code":"429"}}`)

	t.Run("429 is retried after 65 s, up to three times", func(t *testing.T) {
		cn := newCanned(cooldown, cooldown, transcribeReply("Back again."))
		c := newClient(t, cn)
		var waits []time.Duration
		c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

		entries, err := c.Transcribe(t.Context(), audio)
		if err != nil || len(entries) != 1 || entries[0].Text != "Back again." {
			t.Fatalf("Transcribe = %+v, %v, want the third answer", entries, err)
		}
		if len(waits) != 2 || waits[0] != 65*time.Second || waits[1] != 65*time.Second {
			t.Fatalf("waits = %v, want two of 65 s", waits)
		}
	})
	t.Run("the same chunk is sent again, not the next", func(t *testing.T) {
		long := wav(16000, 1, 16, tone(16000*25), nil, nil) // two chunks
		cn := newCanned(transcribeReply("One."), cooldown, transcribeReply("Two."))
		c := newClient(t, cn)
		c.sleep = func(context.Context, time.Duration) error { return nil }

		entries, err := c.Transcribe(t.Context(), llm.Audio{Data: long, MIME: "audio/wav", Name: "m.wav"})
		if err != nil || len(entries) != 2 {
			t.Fatalf("Transcribe = %+v, %v", entries, err)
		}
		reqs := cn.to("/v1/audio/transcriptions")
		if len(reqs) != 3 || !bytes.Equal(fileOf(t, reqs[1]), fileOf(t, reqs[2])) || bytes.Equal(fileOf(t, reqs[0]), fileOf(t, reqs[1])) {
			t.Fatalf("%d requests: want chunk 1, chunk 2 (refused), chunk 2 again", len(reqs))
		}
	})
	t.Run("a cooldown that lasts gives up after three waits", func(t *testing.T) {
		cn := newCanned(cooldown, cooldown, cooldown, cooldown, transcribeReply("too late"))
		c := newClient(t, cn)
		var waits int
		c.sleep = func(context.Context, time.Duration) error { waits++; return nil }

		_, err := c.Transcribe(t.Context(), audio)
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 429 {
			t.Fatalf("Transcribe error = %v, want the HTTP 429", err)
		}
		if n := len(cn.to("/v1/audio/transcriptions")); n != 4 || waits != 3 {
			t.Fatalf("requests = %d, waits = %d, want 1 + 3 retries and 3 waits", n, waits)
		}
	})
	t.Run("a 500 is not retried: it is deterministic and starts a cooldown", func(t *testing.T) {
		cn := newCanned(statusReply(500, `{"error":{"message":"litellm.InternalServerError"}}`), transcribeReply("never"))
		c := newClient(t, cn)
		c.sleep = func(context.Context, time.Duration) error { t.Error("waited after a 500"); return nil }

		_, err := c.Transcribe(t.Context(), audio)
		var he *llm.HTTPError
		if !errors.As(err, &he) || he.Status != 500 || cn.count() != 1 {
			t.Fatalf("Transcribe error = %v after %d requests, want the HTTP 500 after one", err, cn.count())
		}
	})
	t.Run("a cancelled wait stops the transcription", func(t *testing.T) {
		cn := newCanned(cooldown, transcribeReply("never"))
		c := newClient(t, cn)
		c.sleep = func(context.Context, time.Duration) error { return context.Canceled }

		if _, err := c.Transcribe(t.Context(), audio); !errors.Is(err, context.Canceled) || cn.count() != 1 {
			t.Fatalf("Transcribe error = %v after %d requests, want context.Canceled after one", err, cn.count())
		}
	})
}

func fileOf(t *testing.T, r sent) []byte {
	t.Helper()
	_, _, file := r.form(t)
	return file
}

func TestTranscribe_NeedsAnASRModel(t *testing.T) {
	cn := newCanned()
	c := newClient(t, cn, func(c *config.Config) { c.ASRModel = "" })
	if _, err := c.Transcribe(t.Context(), speechAudio(t)); err == nil || !strings.Contains(err.Error(), "KIWI_ASR_MODEL") || cn.count() != 0 {
		t.Fatalf("Transcribe error = %v after %d requests, want one naming KIWI_ASR_MODEL and no request", err, cn.count())
	}
}

func TestTranscribe_SkipsSilentChunksAndSplitsTheRest(t *testing.T) {
	long := wav(16000, 1, 16, tone(16000*45), nil, nil) // three chunks
	cn := newCanned(transcribeReply("First chunk. Second sentence!"), transcribeReply(""), transcribeReply("Last words"))
	entries, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: long, MIME: "audio/wav", Name: "m.wav"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Text)
	}
	if want := []string{"First chunk.", "Second sentence!", "Last words"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
}

// --- SplitSentences ---

func TestSplitSentences(t *testing.T) {
	thai := func(n int) string { return strings.Repeat("สวัสดี", n) } // 6 runes each
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"empty", "", nil},
		{"blank", " \n\t ", nil},
		{"one sentence without a stop", "Good morning everyone", []string{"Good morning everyone"}},
		{"stops, question marks and exclamation marks", "Hello there. How are you? Fine! Thanks.", []string{"Hello there.", "How are you?", "Fine!", "Thanks."}},
		{"a stop at the end of the text", "It ends here.", []string{"It ends here."}},
		{"a stop inside a number or a version is not an end", "Ship v1.1 on 3.5 percent. Done.", []string{"Ship v1.1 on 3.5 percent.", "Done."}},
		{"runs of stops end one sentence", "Wait... what?! Really?", []string{"Wait...", "what?!", "Really?"}},
		{"a quote after the stop stays with its sentence", `He said "go." Then left.`, []string{`He said "go."`, "Then left."}},
		{"newlines end a sentence", "first line\nsecond line\r\n\r\nthird", []string{"first line", "second line", "third"}},
		{"Thai has no stop: a short text stays whole", "สวัสดีครับทุกคน วันนี้เราจะคุยเรื่อง release ครับ", []string{"สวัสดีครับทุกคน วันนี้เราจะคุยเรื่อง release ครับ"}},
		{"a space inside Thai ends a sentence once it holds 60 runes", thai(10) + " " + thai(2), []string{thai(10), thai(2)}},
		{"a space inside Thai before 60 runes does not", thai(9) + " " + thai(2), []string{thai(9) + " " + thai(2)}},
		{"a space beside Latin is not a Thai pause", thai(11) + " QA " + thai(2), []string{thai(11) + " QA " + thai(2)}},
		{"Thai then English with no space between", "ขอบคุณครับGood morning everyone. The launch is Friday.", []string{"ขอบคุณครับGood morning everyone.", "The launch is Friday."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SplitSentences(tc.in); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", tc.want) {
				t.Fatalf("SplitSentences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// thaiDependent reports a rune that cannot start a syllable cluster: a vowel or mark
// that attaches to the consonant before it.
func thaiDependent(r rune) bool {
	return r == 0x0E30 || r == 0x0E31 || r == 0x0E32 || r == 0x0E33 || (r >= 0x0E34 && r <= 0x0E3A) || r == 0x0E45 || r == 0x0E46 || (r >= 0x0E47 && r <= 0x0E4E)
}

// thaiLeading reports a rune that is written before its consonant.
func thaiLeading(r rune) bool { return r >= 0x0E40 && r <= 0x0E44 }

func TestSplitSentences_ThaiRunOnIsCutAtTheCap(t *testing.T) {
	// A 30 s Thai chunk comes back as one run-on: no stops, and spaces only around
	// the Latin words.
	runOn := strings.Repeat("ครับตอนนี้ QA เหลือบัคสองตัวค่ะฉันจะฟิกให้เสร็จก่อนวันศุกร์นะคะเดี๋ยวส่งให้ดูอีกที", 5)
	checkPieces(t, runOn, SplitSentences(runOn))

	t.Run("pure Thai with no space at all", func(t *testing.T) {
		pure := strings.Repeat("เราจะเปิดตัวแอปมือถือในวันศุกร์หน้าและต้องแก้บัคให้เสร็จก่อนนั้นให้ได้", 12)
		got := SplitSentences(pure)
		checkPieces(t, pure, got)
		if len(got) < 4 {
			t.Errorf("%d pieces for %d runes, want it cut", len(got), utf8.RuneCountInString(pure))
		}
	})
	t.Run("a boundary between the scripts is preferred", func(t *testing.T) {
		in := strings.Repeat("ก", 100) + "Good morning everyone" + strings.Repeat("ข", 100)
		got := SplitSentences(in)
		checkPieces(t, in, got)
		if len(got) != 2 || !strings.HasSuffix(got[0], "everyone") || !strings.HasPrefix(got[1], "ข") {
			t.Errorf("pieces = %q, want the cut between 'everyone' and the Thai that follows it", got)
		}
	})
	t.Run("mixed boundaries with no space, as the ASR writes them", func(t *testing.T) {
		in := "วันนี้เราจะคุยเรื่องแผนการเปิดตัวและทบทวนบัคที่ค้างอยู่ทั้งหมดก่อนส่งให้ลูกค้าครับGood morning everyone and welcome to the weekly release sync where we go through the open bugs " +
			"the mobile appตอนนี้ QA เหลือบัคสองตัวค่ะฉันจะฟิกให้เสร็จก่อนวันศุกร์นะคะ"
		checkPieces(t, in, SplitSentences(in))
	})
	t.Run("Latin text with no stops is cut at a space", func(t *testing.T) {
		in := strings.TrimSpace(strings.Repeat("we keep talking without ever ending the sentence ", 12))
		got := SplitSentences(in)
		checkPieces(t, in, got)
		words := map[string]bool{"we": true, "keep": true, "talking": true, "without": true, "ever": true, "ending": true, "the": true, "sentence": true}
		for _, p := range got {
			w := strings.Fields(p)
			if !words[w[0]] || !words[w[len(w)-1]] {
				t.Errorf("piece %q starts or ends inside a word", p)
			}
		}
		if len(got) < 3 {
			t.Errorf("%d pieces, want the %d-rune text cut", len(got), utf8.RuneCountInString(in))
		}
	})
}

// checkPieces asserts the properties every split must keep: each piece is trimmed and
// at most 160 runes; no cut separates a Thai vowel or tone mark from its consonant or
// leaves a leading vowel behind; and nothing but whitespace is lost.
func checkPieces(t *testing.T, in string, pieces []string) {
	t.Helper()
	strip := func(s string) string { return strings.Join(strings.Fields(s), "") }
	if got, want := strip(strings.Join(pieces, "")), strip(in); got != want {
		t.Fatalf("the pieces do not add up to the text:\n got %q\nwant %q", got, want)
	}
	for i, p := range pieces {
		if p == "" || p != strings.TrimSpace(p) {
			t.Errorf("piece %d = %q: want it trimmed and not empty", i, p)
		}
		if n := utf8.RuneCountInString(p); n > 160 {
			t.Errorf("piece %d has %d runes, want at most 160: %q", i, n, p)
		}
		first, _ := utf8.DecodeRuneInString(p)
		last, _ := utf8.DecodeLastRuneInString(p)
		if thaiDependent(first) {
			t.Errorf("piece %d starts with %U, a vowel or mark that belongs to the letter before it: %q", i, first, p)
		}
		if thaiLeading(last) {
			t.Errorf("piece %d ends with %U, a vowel that belongs to the letter after it: %q", i, last, p)
		}
	}
}

// --- parts ---

func TestParts_ScannedPDFUnsupportedByProvider(t *testing.T) {
	pdf := []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")

	t.Run("a PDF part with no text layer", func(t *testing.T) {
		cn := newCanned(chatReply("never", "", "stop"))
		_, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{Text: "Summarise:"}, {Data: pdf, MIME: "application/pdf"}}})
		if !errors.Is(err, llm.ErrUnsupportedByProvider) || cn.count() != 0 {
			t.Fatalf("Generate error = %v after %d requests, want llm.ErrUnsupportedByProvider and no request", err, cn.count())
		}
	})
	t.Run("the same through Stream", func(t *testing.T) {
		cn := newCanned(chatReply("never", "", "stop"))
		_, err := newClient(t, cn).Stream(t.Context(), llm.Request{Parts: []llm.Part{{Data: pdf, MIME: "application/pdf; charset=binary"}}}, nil)
		if !errors.Is(err, llm.ErrUnsupportedByProvider) || cn.count() != 0 {
			t.Fatalf("Stream error = %v after %d requests, want llm.ErrUnsupportedByProvider", err, cn.count())
		}
	})
	t.Run("a PDF given as a URI", func(t *testing.T) {
		cn := newCanned(chatReply("never", "", "stop"))
		_, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{URI: "gs://bucket/scan.pdf", MIME: "application/pdf"}}})
		if !errors.Is(err, llm.ErrUnsupportedByProvider) || cn.count() != 0 {
			t.Fatalf("Generate error = %v, want llm.ErrUnsupportedByProvider", err)
		}
	})
	t.Run("its text layer, supplied as text, is fine", func(t *testing.T) {
		cn := newCanned(chatReply("A summary.", "", "stop"))
		res, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{Text: "<document>page one</document>"}}})
		if err != nil || res.Text != "A summary." {
			t.Fatalf("Generate = %+v, %v", res, err)
		}
	})
	t.Run("what the provider cannot read at all", func(t *testing.T) {
		for _, mime := range []string{"audio/wav", "video/mp4", "application/zip", "text/plain", ""} {
			cn := newCanned(chatReply("never", "", "stop"))
			_, err := newClient(t, cn).Generate(t.Context(), llm.Request{Parts: []llm.Part{{Data: []byte("x"), MIME: mime}}})
			if !errors.Is(err, llm.ErrUnsupportedByProvider) || cn.count() != 0 {
				t.Errorf("a %q part: error = %v after %d requests, want llm.ErrUnsupportedByProvider", mime, err, cn.count())
			}
		}
	})
}

func TestParts_ImagesGoAsDataURLs(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot really an image")
	cn := newCanned(chatReply("Text in the image.", "", "stop"))
	_, err := newClient(t, cn).Generate(t.Context(), llm.Request{
		System: "Transcribe the text in the image.",
		Parts:  []llm.Part{{Data: png, MIME: "image/png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := cn.only(t, "/v1/chat/completions").json(t)["messages"].([]any)
	parts := msgs[1].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	got, _ := img["image_url"].(map[string]any)["url"].(string)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if len(parts) != 1 || img["type"] != "image_url" || got != want {
		t.Fatalf("user content = %v, want one image_url part with the data URL %s", parts, want)
	}
}

// --- what the requests never carry ---

func TestRequestsCarryNoGeminiOnlySettings(t *testing.T) {
	cn := newCanned(
		chatReply("ok", "", "stop"), // Generate, flash, plain
		chatReply(`{"summary":"s","decisions":[],"action_items":[]}`, "", "stop"), // flash, schema
		chatReply(`{"summary":"s","decisions":[],"action_items":[]}`, "", "stop"), // pro, schema
		chatReply("ok", "", "stop"), // tools
		sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`),
		jsonReply(`{"data":[{"index":0,"embedding":`+zeros(768)+`}]}`),
		transcribeReply("ok"),
	)
	c := newClient(t, cn)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	plain := llm.Request{Parts: []llm.Part{{Text: "hi"}}, System: "be brief", MaxOutputTokens: 100}
	_, err := c.Generate(ctx, plain)
	must(err)
	_, err = c.Generate(ctx, summaryRequest(llm.TierFlash))
	must(err)
	_, err = c.Generate(ctx, summaryRequest(llm.TierPro))
	must(err)
	_, err = c.Generate(ctx, toolRequest())
	must(err)
	_, err = c.Stream(ctx, plain, nil)
	must(err)
	_, err = c.Embed(ctx, []string{"a"}, llm.EmbedDocument)
	must(err)
	_, err = c.Transcribe(ctx, speechAudio(t))
	must(err)

	if cn.count() != 7 {
		t.Fatalf("%d requests, want 7", cn.count())
	}
	for _, r := range cn.reqs {
		// The multipart body holds audio: look at its field names, not its bytes.
		text := string(r.Body)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			fields, _, _ := r.form(t)
			text = fmt.Sprint(fields)
		}
		for _, banned := range []string{"safetySettings", "safety_settings", "top_k", "topK", "top_k_", "candidateCount", "generationConfig", "thinkingConfig", "responseMimeType", "response_mime_type", "systemInstruction"} {
			if strings.Contains(text, banned) {
				t.Errorf("a request to %s carries %q: %.200s", r.Path, banned, text)
			}
		}
	}
}

func zeros(n int) string {
	b, _ := json.Marshal(make([]float32, n))
	return string(b)
}

// --- construction ---

func TestNew_RejectsAnIncompleteConfigNamingTheVariableNeverItsValue(t *testing.T) {
	secretURL := "http://user:hunter2@litellm.internal.example"
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"no base URL", func(c *config.Config) { c.LLMBaseURL = "" }, "KIWI_LLM_BASE_URL"},
		{"a base URL that is not a URL", func(c *config.Config) { c.LLMBaseURL = "litellm.internal.example" }, "KIWI_LLM_BASE_URL"},
		{"a base URL with credentials", func(c *config.Config) { c.LLMBaseURL = secretURL }, "KIWI_LLM_BASE_URL"},
		{"a base URL with a scheme other than http(s)", func(c *config.Config) { c.LLMBaseURL = "ftp://litellm.internal.example" }, "KIWI_LLM_BASE_URL"},
		{"no key", func(c *config.Config) { c.LLMAPIKey = "" }, "KIWI_LLM_API_KEY"},
		{"no flash model", func(c *config.Config) { c.LLMModelFlash = "" }, "KIWI_LLM_MODEL_FLASH"},
		{"no pro model", func(c *config.Config) { c.LLMModelPro = "" }, "KIWI_LLM_MODEL_PRO"},
		{"no embedding host", func(c *config.Config) { c.EmbedBaseURL = "" }, "KIWI_EMBED_BASE_URL"},
		{"an embedding host with credentials", func(c *config.Config) { c.EmbedBaseURL = secretURL }, "KIWI_EMBED_BASE_URL"},
		{"no embedding model", func(c *config.Config) { c.EmbedModel = "" }, "KIWI_EMBED_MODEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.edit(&cfg)
			c, err := New(cfg, nil)
			if err == nil || c != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, %v, want an error naming %s", c, err, tc.want)
			}
			for _, secret := range []string{"hunter2", "litellm.internal"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q contains %q", err, secret)
				}
			}
		})
	}
}

func TestNew_AcceptsTrailingSlashesAndWorksWithoutAnHTTPClient(t *testing.T) {
	var path atomic.Value
	cn := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path.Store(r.URL.String())
		return httpResponse(chatReply("ok", "", "stop"), r), nil
	})
	cfg := testConfig()
	cfg.LLMBaseURL += "/"
	c, err := New(cfg, &http.Client{Transport: cn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(t.Context(), oneSentenceRequest()); err != nil {
		t.Fatal(err)
	}
	if got := path.Load(); got != "http://llm.test/v1/chat/completions" {
		t.Errorf("URL = %v, want http://llm.test/v1/chat/completions", got)
	}
	if c, err := New(testConfig(), nil); err != nil || c == nil {
		t.Fatalf("New without an HTTP client = %v, %v", c, err)
	}
}

func httpResponse(r reply, req *http.Request) *http.Response {
	return &http.Response{StatusCode: r.status, Header: http.Header{"Content-Type": {r.contentType}}, Body: io.NopCloser(strings.NewReader(r.body)), Request: req}
}

func TestADKModel_Errors(t *testing.T) {
	c := newClient(t, newCanned())
	if _, err := c.ADKModel("huge"); err == nil {
		t.Error("ADKModel accepted an unknown tier")
	}
	for tier, want := range map[llm.Tier]string{llm.TierFlash: flashModel, llm.TierPro: proModel, "": flashModel} {
		m, err := c.ADKModel(tier)
		if err != nil || m.Name() != want {
			t.Errorf("ADKModel(%q) = %v, %v, want a model named %s", tier, m, err, want)
		}
	}
}

// --- recording ---

// TestRecord records the fixtures the other tests replay, from the live LiteLLM and
// Ollama. It is opt-in: with KIWI_LLM_RECORD=1 and the LiteLLM variables of .env
// loaded, plus KIWI_EMBED_BASE_URL pointing at Ollama,
//
//	KIWI_LLM_RECORD=1 go test ./internal/platform/llm/openai -run TestRecord -v
//
// writes testdata/*.json with no header, host or credential in them. The requests are
// the ones the replay tests send; keep them in step.
func TestRecord(t *testing.T) {
	if !httpfixture.Recording() {
		t.Skip("set KIWI_LLM_RECORD=1 to record fixtures from the live LiteLLM and Ollama")
	}
	cfg := testConfig()
	for env, dst := range map[string]*string{"KIWI_LLM_BASE_URL": &cfg.LLMBaseURL, "KIWI_LLM_API_KEY": &cfg.LLMAPIKey, "KIWI_EMBED_BASE_URL": &cfg.EmbedBaseURL} {
		if *dst = os.Getenv(env); *dst == "" {
			t.Fatalf("%s is not set", env)
		}
	}
	rec := httpfixture.NewRecorder(t, testdata, http.DefaultTransport)
	c, err := New(cfg, &http.Client{Transport: rec})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	t.Run("generate", func(t *testing.T) {
		rec.Name("generate_meeting_summary")
		if _, err := c.Generate(ctx, summaryRequest(llm.TierFlash)); err != nil {
			t.Fatal(err)
		}
		rec.Name("generate_pro_meeting_summary")
		if _, err := c.Generate(ctx, summaryRequest(llm.TierPro)); err != nil {
			t.Fatal(err)
		}
		rec.Name("generate_one_sentence")
		if _, err := c.Generate(ctx, oneSentenceRequest()); err != nil {
			t.Fatal(err)
		}
		rec.Name("generate_truncated")
		if _, err := c.Generate(ctx, truncatedRequest()); !errors.Is(err, llm.ErrTruncated) {
			t.Errorf("the truncation request returned %v, want llm.ErrTruncated: delete generate_truncated-01.json and choose a harder prompt", err)
		}
		rec.Name("generate_tool_call")
		if _, err := c.Generate(ctx, toolRequest()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("stream", func(t *testing.T) {
		rec.Name("stream_one_sentence")
		if _, err := c.Stream(ctx, streamRequest(), nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("adk", func(t *testing.T) {
		m, err := c.ADKModel(llm.TierFlash)
		if err != nil {
			t.Fatal(err)
		}
		first, second := adkRequests()
		rec.Name("adk_function_call")
		lastPart(t, m, first, func(p *genai.Part) bool { return p.FunctionCall != nil })
		rec.Name("adk_function_response")
		lastPart(t, m, second, func(p *genai.Part) bool { return p.Text != "" && !p.Thought })
	})
	t.Run("embed", func(t *testing.T) {
		rec.Name("embed_documents")
		if _, err := c.Embed(ctx, embedDocuments, llm.EmbedDocument); err != nil {
			t.Fatal(err)
		}
		rec.Name("embed_query")
		if _, err := c.Embed(ctx, embedQuery, llm.EmbedQuery); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("transcribe", func(t *testing.T) {
		// One short chunk only: a long request is never recorded, and a request the ASR
		// cannot decode would put it into a cooldown for the whole team.
		if n := len(speech(t)); n > 20*16000*2+44 {
			t.Fatalf("the sample is %d bytes: record one chunk of at most 20 s", n)
		}
		rec.Name("transcribe_speech")
		if _, err := c.Transcribe(ctx, speechAudio(t)); err != nil {
			t.Fatal(err)
		}
	})
}
