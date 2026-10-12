package main

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

const (
	// maxBody bounds a request: a document or an image goes to the model in one body.
	maxBody = 64 << 20

	// headRunes bounds what an unmatched request reveals of its last user message.
	headRunes = 120

	// Sizes, in runes, of the pieces a streamed answer is cut into.
	contentPiece = 16
	argsPiece    = 24
)

// replay answers requests from fixtures. It holds no state a request changes.
type replay struct {
	fx  *fixtureSet
	log *slog.Logger
}

// newHandler serves the OpenAI-compatible endpoints of the LLM adapter from fx, and
// /healthz. log gets one line per request: the path and the fixture that answered, or,
// for a request no fixture fits, its schema, its stage and the head of its last user
// message. It never gets a header or any other part of a body.
func newHandler(fx *fixtureSet, log *slog.Logger) http.Handler {
	rp := &replay{fx: fx, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /v1/chat/completions", rp.chat)
	mux.HandleFunc("POST /v1/embeddings", rp.embeddings)
	mux.HandleFunc("POST /v1/audio/transcriptions", rp.transcribe)
	return mux
}

// --- chat completions ---

type chatPayload struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Tools          []json.RawMessage `json:"tools"`
	ResponseFormat struct {
		JSONSchema struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	} `json:"response_format"`
	Stream        bool `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text is the text of a message: its content when that is a string, else its text
// parts joined by newlines (an image part has none).
func (m chatMessage) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// request is what a fixture is matched against.
type request struct {
	model        string
	schema       string // "" when the request names none
	stage        string
	text         string // the text of every message, joined by newlines
	lastUser     string // the text of the last user message
	stream       bool
	includeUsage bool
}

func newRequest(c chatPayload) request {
	in := request{
		model:        c.Model,
		schema:       c.ResponseFormat.JSONSchema.Name,
		stream:       c.Stream,
		includeUsage: c.StreamOptions.IncludeUsage,
		stage:        stageNone,
	}
	if len(c.Tools) > 0 {
		in.stage = stageAgentTool
	}
	texts := make([]string, 0, len(c.Messages))
	for _, m := range c.Messages {
		text := m.text()
		texts = append(texts, text)
		switch m.Role {
		case "tool":
			in.stage = stageAgentFinal
		case "user":
			in.lastUser = text
		}
	}
	in.text = strings.Join(texts, "\n")
	return in
}

// summary names the request the way an error does.
func (in request) summary() string {
	return "schema=" + cmp.Or(in.schema, "none") + " stage=" + in.stage
}

// head is the first headRunes runes of s with whitespace collapsed to single spaces.
func head(s string) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	return string(runes[:min(len(runes), headRunes)])
}

func (f *chatFixture) matches(in request) bool {
	if f.match.Schema != "" && f.match.Schema != in.schema {
		return false
	}
	if f.match.Stage != "" && f.match.Stage != in.stage {
		return false
	}
	for _, c := range f.match.Contains {
		if !strings.Contains(in.text, c) {
			return false
		}
	}
	return true
}

func (rp *replay) chat(w http.ResponseWriter, r *http.Request) {
	var req chatPayload
	if !rp.decode(w, r, &req) {
		return
	}
	in := newRequest(req)
	var fixture *chatFixture
	for i := range rp.fx.chat {
		if rp.fx.chat[i].matches(in) {
			fixture = &rp.fx.chat[i]
			break
		}
	}
	if fixture == nil {
		rp.chatMiss(w, r, in,
			"no fixture matches "+in.summary(),
			fmt.Sprintf("a chat fixture whose match fits %s and whose contains all occur in the request (%d chat fixtures loaded, none fits)", in.summary(), len(rp.fx.chat)))
		return
	}
	body, err := fixture.render(in)
	var miss *missError
	if errors.As(err, &miss) {
		rp.chatMiss(w, r, in,
			"a fixture matches "+in.summary()+" but cites something the request lacks",
			fmt.Sprintf("%s answers this request, but its response cites %s, which the request does not carry", fixture.name, miss.what))
		return
	}
	if err != nil {
		rp.fail(w, http.StatusInternalServerError, "render "+fixture.name+": "+err.Error())
		return
	}
	rp.log.Info("replayed", "path", r.URL.Path, "fixture", fixture.name, "schema", cmp.Or(in.schema, "none"), "stage", in.stage, "stream", in.stream)
	if !in.stream {
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeStream(w, streamFrames(body, in.includeUsage))
}

// unmatched is the body of an HTTP 599. llm.HTTPError.Error keeps the first 200 bytes
// of a body, so the message, which names the schema and the stage, comes first.
type unmatched struct {
	Message         string  `json:"message"`
	Type            string  `json:"type"`
	Code            int     `json:"code"`
	Schema          string  `json:"schema,omitempty"`
	Stage           string  `json:"stage,omitempty"`
	LookedFor       string  `json:"looked_for"`
	LastUserMessage *string `json:"last_user_message,omitempty"`
	AudioSHA256     string  `json:"audio_sha256,omitempty"`
}

// chatMiss answers a request no fixture can answer with HTTP 599, a status no real
// server sends, so that it cannot be taken for a model's answer.
func (rp *replay) chatMiss(w http.ResponseWriter, r *http.Request, in request, message, lookedFor string) {
	h := head(in.lastUser)
	rp.log.Warn("unmatched request", "path", r.URL.Path, "schema", cmp.Or(in.schema, "none"), "stage", in.stage, "looked_for", lookedFor, "last_user_message", h)
	writeJSON(w, 599, map[string]unmatched{"error": {
		Message: "replay: " + message, Type: "fixture_unmatched", Code: 599,
		Schema: cmp.Or(in.schema, "none"), Stage: in.stage, LookedFor: lookedFor, LastUserMessage: &h,
	}})
}

// missError says a response cites something the request does not carry.
type missError struct{ what string }

func (e *missError) Error() string { return "the request does not carry " + e.what }

// resolver fills the placeholders of a response from the request:
// {{chunk:TEXT}} is the id of the first <chunk id="…"> block whose text contains TEXT,
// and {{uuid:PREFIX}} is the UUID right after the first PREFIX that has one.
type resolver struct {
	text   string // the text of the request's messages
	chunks []requestChunk
	parsed bool
}

// requestChunk is one <chunk id="…"> block of a request.
type requestChunk struct{ id, text string }

var chunkBlock = regexp.MustCompile(`(?s)<chunk id="([^"]*)">\n(.*?)\n</chunk>`)

func (r *resolver) expand(s string) (string, error) {
	var first error
	out := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholder.FindStringSubmatch(m)
		v, err := r.lookup(sub[1], sub[2])
		if err != nil && first == nil {
			first = err
		}
		return v
	})
	return out, first
}

func (r *resolver) lookup(kind, arg string) (string, error) {
	switch kind {
	case "chunk":
		if !r.parsed {
			for _, m := range chunkBlock.FindAllStringSubmatch(r.text, -1) {
				r.chunks = append(r.chunks, requestChunk{id: m[1], text: m[2]})
			}
			r.parsed = true
		}
		for _, c := range r.chunks {
			if strings.Contains(c.text, arg) {
				return c.id, nil
			}
		}
		return "", &missError{what: fmt.Sprintf("a chunk containing %q", arg)}
	case "uuid":
		for from := 0; ; {
			i := strings.Index(r.text[from:], arg)
			if i < 0 {
				break
			}
			if id := uuidAtStart.FindString(r.text[from+i+len(arg):]); id != "" {
				return id, nil
			}
			from += i + 1
		}
		return "", &missError{what: fmt.Sprintf("a UUID right after %q", arg)}
	}
	return "", fmt.Errorf("unknown placeholder kind %q", kind)
}

// render builds the chat.completion body that answers in: the fixture's response with
// its placeholders filled from the request, JSON values written as the strings the
// wire carries, and what a server always sends (id, object, model, index, role, tool
// call id and type, finish_reason) when the fixture leaves it out.
func (f *chatFixture) render(in request) (map[string]any, error) {
	tree, err := decodeTree(f.response)
	if err != nil {
		return nil, err
	}
	r := resolver{text: in.text}
	if tree, err = mapStrings(tree, r.expand); err != nil {
		return nil, err
	}
	// checkResponse saw an object with one choice that has a message.
	body := tree.(map[string]any)
	setDefault(body, "id", "chatcmpl-replay")
	setDefault(body, "object", "chat.completion")
	setDefault(body, "model", in.model)
	choice := body["choices"].([]any)[0].(map[string]any)
	setDefault(choice, "index", 0)
	msg := choice["message"].(map[string]any)
	setDefault(msg, "role", "assistant")
	if msg["content"], err = wireString(msg["content"]); err != nil {
		return nil, err
	}
	calls, _ := msg["tool_calls"].([]any)
	for i, c := range calls {
		call := c.(map[string]any)
		setDefault(call, "id", fmt.Sprintf("call_replay_%d", i+1))
		setDefault(call, "type", "function")
		fn := call["function"].(map[string]any)
		if fn["arguments"] == nil {
			fn["arguments"] = "{}"
		}
		if fn["arguments"], err = wireString(fn["arguments"]); err != nil {
			return nil, err
		}
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	setDefault(choice, "finish_reason", finish)
	return body, nil
}

// setDefault sets m[key] to v unless the fixture gave it a value.
func setDefault(m map[string]any, key string, v any) {
	if cur, ok := m[key]; !ok || cur == nil || cur == "" {
		m[key] = v
	}
}

// wireString is v as a chat completion carries it: null and strings as they are, a
// JSON object or list as its compact text.
func wireString(v any) (any, error) {
	switch v.(type) {
	case map[string]any, []any:
		return marshal(v)
	}
	return v, nil
}

func marshal(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// --- the stream ---

// streamFrames cuts a completion into the chunks a server streams it as, the way a
// real model's stream was recorded: a first chunk with the role and empty content,
// the reasoning and then the content in pieces, the tool calls (a first piece with
// the id and the name, then the arguments in pieces), a chunk with the finish reason
// and an empty delta and, when the client asked for it, a chunk with the usage.
func streamFrames(body map[string]any, includeUsage bool) []map[string]any {
	choice := body["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	frame := func(delta map[string]any, finish any) map[string]any {
		c := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			c["finish_reason"] = finish
		}
		f := map[string]any{"id": body["id"], "object": "chat.completion.chunk", "choices": []any{c}}
		for _, k := range []string{"created", "model", "system_fingerprint"} {
			if v, ok := body[k]; ok {
				f[k] = v
			}
		}
		return f
	}
	frames := []map[string]any{frame(map[string]any{"role": "assistant", "content": ""}, nil)}
	if s, _ := msg["reasoning_content"].(string); s != "" {
		for _, p := range pieces(s, contentPiece) {
			frames = append(frames, frame(map[string]any{"reasoning_content": p}, nil))
		}
	}
	if s, _ := msg["content"].(string); s != "" {
		for _, p := range pieces(s, contentPiece) {
			frames = append(frames, frame(map[string]any{"content": p}, nil))
		}
	}
	calls, _ := msg["tool_calls"].([]any)
	for i, c := range calls {
		call := c.(map[string]any)
		fn := call["function"].(map[string]any)
		frames = append(frames, frame(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": call["id"], "type": "function",
			"function": map[string]any{"name": fn["name"], "arguments": ""},
		}}}, nil))
		for _, p := range pieces(fn["arguments"].(string), argsPiece) {
			frames = append(frames, frame(map[string]any{"tool_calls": []any{map[string]any{
				"index": i, "function": map[string]any{"arguments": p},
			}}}, nil))
		}
	}
	frames = append(frames, frame(map[string]any{}, choice["finish_reason"]))
	if usage, ok := body["usage"]; ok && includeUsage {
		u := frame(map[string]any{}, nil)
		u["usage"] = usage
		frames = append(frames, u)
	}
	return frames
}

// pieces cuts s into pieces of at most size runes that join back into s.
func pieces(s string, size int) []string {
	runes := []rune(s)
	var out []string
	for len(runes) > 0 {
		n := min(size, len(runes))
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}
	return out
}

func writeStream(w http.ResponseWriter, frames []map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	for _, f := range frames {
		payload, err := marshal(f)
		if err != nil {
			return // the stream ends with no [DONE], which a client reports as an error
		}
		send(payload)
	}
	send("[DONE]")
}

// --- transcriptions ---

func (rp *replay) transcribe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	mr, err := r.MultipartReader()
	if err != nil {
		rp.fail(w, http.StatusBadRequest, "the request is not multipart/form-data")
		return
	}
	var sum string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			rp.fail(w, http.StatusBadRequest, "read the multipart body: "+err.Error())
			return
		}
		if part.FormName() != "file" || sum != "" {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, part); err != nil {
			rp.fail(w, http.StatusBadRequest, "read the file part: "+err.Error())
			return
		}
		sum = hex.EncodeToString(h.Sum(nil))
	}
	if sum == "" {
		rp.fail(w, http.StatusBadRequest, `the request has no "file" part`)
		return
	}
	fixture, ok := rp.fx.audio[sum]
	if !ok {
		prefix := sum[:8]
		rp.log.Warn("unmatched request", "path", r.URL.Path, "audio_sha256_prefix", prefix)
		writeJSON(w, 599, map[string]unmatched{"error": {
			Message: "replay: no transcription fixture for audio sha256 " + prefix, Type: "fixture_unmatched", Code: 599,
			LookedFor:   fmt.Sprintf(`a fixture {"audio_sha256": "<the hash of the uploaded file>", "text": "…"} (%d transcription fixtures loaded, none with this hash)`, len(rp.fx.audio)),
			AudioSHA256: sum,
		}})
		return
	}
	rp.log.Info("replayed", "path", r.URL.Path, "fixture", fixture.name)
	writeJSON(w, http.StatusOK, map[string]string{"text": fixture.text})
}

// --- shared ---

// decode reads the JSON body of r into v, or answers the client with the problem.
func (rp *replay) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			rp.fail(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		} else {
			rp.fail(w, http.StatusBadRequest, "the request body is not the expected JSON")
		}
		return false
	}
	return true
}

// fail answers with an error object shaped as an OpenAI server writes it.
func (rp *replay) fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": "replay: " + message, "type": "invalid_request_error"}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := marshal(v)
	if err != nil {
		http.Error(w, "replay: encode the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, b+"\n")
}
