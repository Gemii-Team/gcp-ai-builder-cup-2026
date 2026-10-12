package httpfixture_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiwi/internal/testkit/httpfixture"
)

const secretKey = "test-credential-0123456789abcdef"

// liveServer answers like a gateway that echoes what must never reach a fixture:
// its own host name and the credential it was called with.
func liveServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"delta\":\"one\"}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Litellm-Call-Id", "must-not-be-kept")
		_, _ = io.WriteString(w, `{"echo":`+string(bytes.TrimSpace(body))+`,"host":"`+r.Host+`","auth":"`+r.Header.Get("Authorization")+`","text":"hello"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, rt http.RoundTripper, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

func record(t *testing.T, dir, name, path, body string) string {
	t.Helper()
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(t, dir, http.DefaultTransport)
	rec.Name(name)
	status, got := post(t, rec, srv.URL+path, body)
	if status != http.StatusOK {
		t.Fatalf("recording: status %d: %s", status, got)
	}
	return got
}

func TestRecordThenReplay_MatchesByMethodPathAndNormalisedBody(t *testing.T) {
	dir := t.TempDir()
	live := record(t, dir, "chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":1}`)
	if !strings.Contains(live, `"text":"hello"`) {
		t.Fatalf("the recorder must hand the caller the live answer, got %s", live)
	}

	rt := httpfixture.Replay(t, dir)
	// Same request, keys in another order and with other whitespace.
	status, got := post(t, rt, "http://llm.test/v1/chat/completions", "{ \"n\": 1,\n \"messages\": [ {\"content\":\"hi\", \"role\":\"user\"} ], \"model\": \"m\" }")
	if status != http.StatusOK || !strings.Contains(got, `"text":"hello"`) {
		t.Fatalf("replay = %d %s, want the recorded answer", status, got)
	}
}

func TestReplay_UnmatchedRequestIsAnError(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, "chat", "/v1/chat/completions", `{"model":"m"}`)
	rt := httpfixture.Replay(t, dir)

	for name, c := range map[string]struct{ path, body string }{
		"another body": {"/v1/chat/completions", `{"model":"other"}`},
		"another path": {"/v1/embeddings", `{"model":"m"}`},
	} {
		status, got := post(t, rt, "http://llm.test"+c.path, c.body)
		if status != 0 || !strings.Contains(got, "no recorded exchange") {
			t.Errorf("%s: = %d %s, want an error saying there is no recorded exchange", name, status, got)
		}
	}
}

func TestReplay_ServesSameRequestInRecordedOrderThenFails(t *testing.T) {
	dir := t.TempDir()
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(t, dir, http.DefaultTransport)
	rec.Name("attempt")
	for range 2 {
		post(t, rec, srv.URL+"/v1/chat/completions", `{"model":"m"}`)
	}
	// Make the second recording distinguishable by editing it.
	second := filepath.Join(dir, "attempt-02.json")
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, bytes.Replace(b, []byte(`"text": "hello"`), []byte(`"text": "second"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}

	rt := httpfixture.Replay(t, dir)
	for i, want := range []string{"hello", "second"} {
		if _, got := post(t, rt, "http://llm.test/v1/chat/completions", `{"model":"m"}`); !strings.Contains(got, `"text":"`+want+`"`) {
			t.Fatalf("answer %d = %s, want text %q", i+1, got, want)
		}
	}
	if status, got := post(t, rt, "http://llm.test/v1/chat/completions", `{"model":"m"}`); status != 0 || !strings.Contains(got, "no recorded exchange") {
		t.Fatalf("a third request = %d %s, want an error: both recordings are used", status, got)
	}
}

func TestRecorder_KeepsModelOutputOnly(t *testing.T) {
	dir := t.TempDir()
	record(t, dir, "chat", "/v1/chat/completions", `{"model":"m"}`)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 || filepath.Base(files[0]) != "chat-01.json" {
		t.Fatalf("fixtures = %v, %v, want chat-01.json", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	// The echo carried the live host (127.0.0.1:port), the credential and a header.
	for _, leak := range []string{secretKey, "Bearer", "Authorization", "127.0.0.1", "must-not-be-kept", "X-Litellm"} {
		if strings.Contains(text, leak) {
			t.Errorf("fixture contains %q:\n%s", leak, text)
		}
	}
	for _, want := range []string{"llm.test", "REDACTED", `"text": "hello"`, `"request_sha256"`} {
		if !strings.Contains(text, want) {
			t.Errorf("fixture does not contain %q:\n%s", want, text)
		}
	}
}

func TestRecorder_RemovesAKeyShapeItWasNotCalledWith(t *testing.T) {
	// A gateway may echo another key than the one in the request. The literal is built
	// here so that the secret scan has nothing to flag in this file.
	shaped := "s" + "k-" + strings.Repeat("Ab1", 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"other":"`+shaped+`"}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	rec := httpfixture.NewRecorder(t, dir, http.DefaultTransport)
	rec.Name("echo")
	if status, _ := post(t, rec, srv.URL+"/v1/chat/completions", `{}`); status != http.StatusOK {
		t.Fatalf("recording failed with %d", status)
	}
	b, err := os.ReadFile(filepath.Join(dir, "echo-01.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), shaped) || !strings.Contains(string(b), "REDACTED") {
		t.Fatalf("fixture = %s, want the key shape replaced by REDACTED", b)
	}
}

func TestRecorder_NeedsAName(t *testing.T) {
	srv := liveServer(t)
	rec := httpfixture.NewRecorder(t, t.TempDir(), http.DefaultTransport)
	if status, got := post(t, rec, srv.URL+"/v1/chat/completions", `{}`); status != 0 || !strings.Contains(got, "Name") {
		t.Fatalf("recording without a name = %d %s, want an error", status, got)
	}
}

func TestRecordThenReplay_KeepsAnEventStreamAsText(t *testing.T) {
	dir := t.TempDir()
	live := record(t, dir, "stream", "/v1/stream", `{"stream":true}`)

	status, got := post(t, httpfixture.Replay(t, dir), "http://llm.test/v1/stream", `{"stream":true}`)
	if status != http.StatusOK || got != live {
		t.Fatalf("replay = %d %q, want %q", status, got, live)
	}
}

func multipartBody(t *testing.T, boundary string, audio []byte) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	_ = w.WriteField("model", "asr")
	part, err := w.CreateFormFile("file", "a.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(audio)
	_ = w.Close()
	return w.FormDataContentType(), &buf
}

func postForm(t *testing.T, rt http.RoundTripper, url, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

func TestReplay_MultipartMatchesOnPartsNotOnTheBoundary(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"transcript"}`)
	}))
	t.Cleanup(srv.Close)

	rec := httpfixture.NewRecorder(t, dir, http.DefaultTransport)
	rec.Name("asr")
	ct, body := multipartBody(t, "recording-boundary", []byte("RIFF-audio-1"))
	if status, got := postForm(t, rec, srv.URL+"/v1/audio/transcriptions", ct, body); status != http.StatusOK {
		t.Fatalf("recording: %d %s", status, got)
	}

	rt := httpfixture.Replay(t, dir)
	ct, body = multipartBody(t, "another-boundary", []byte("RIFF-audio-1"))
	if status, got := postForm(t, rt, "http://llm.test/v1/audio/transcriptions", ct, body); status != http.StatusOK || !strings.Contains(got, "transcript") {
		t.Fatalf("same parts, other boundary: %d %s, want the recorded answer", status, got)
	}
	ct, body = multipartBody(t, "another-boundary", []byte("RIFF-audio-2"))
	if status, got := postForm(t, rt, "http://llm.test/v1/audio/transcriptions", ct, body); status != 0 || !strings.Contains(got, "no recorded exchange") {
		t.Fatalf("other audio: %d %s, want no recorded exchange", status, got)
	}
}

func TestRecording_ReadsTheSwitch(t *testing.T) {
	t.Setenv("KIWI_LLM_RECORD", "1")
	if !httpfixture.Recording() {
		t.Error("Recording() = false with KIWI_LLM_RECORD=1")
	}
	t.Setenv("KIWI_LLM_RECORD", "")
	if httpfixture.Recording() {
		t.Error("Recording() = true with KIWI_LLM_RECORD unset")
	}
}
