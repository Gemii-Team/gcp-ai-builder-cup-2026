// Package httpfixture replays recorded HTTP exchanges to an http.Client, and records
// them from a live server.
//
// A fixture is one JSON file: the method, the path and a hash of the normalised
// request body it answers, and the response's status, content type and body. It
// holds model output only: no request body (the hash is what matches), no header
// but the content type, and no host, so a fixture is safe to commit.
//
// Replay answers a request from the fixture with the same method, path and body
// hash that has not been used yet, in file name order, and fails the request when
// there is none. NewRecorder forwards requests to a live server and writes what it
// sees. The recorder is switched on by the test, which asks Recording.
package httpfixture

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// recordEnv is the variable that switches recording on.
const recordEnv = "KIWI_LLM_RECORD"

// FixtureHost replaces the host of the live server wherever a response repeats it.
const FixtureHost = "llm.test"

// redacted replaces a credential wherever a response repeats it.
const redacted = "REDACTED"

// maxBody bounds a recorded response.
const maxBody = 64 << 20

// Recording reports whether KIWI_LLM_RECORD=1: the test should record from the live
// server instead of replaying.
func Recording() bool { return os.Getenv(recordEnv) == "1" }

// fixture is the file format.
type fixture struct {
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	RequestSHA256 string          `json:"request_sha256"`
	Status        int             `json:"status"`
	ContentType   string          `json:"content_type,omitempty"`
	Body          json.RawMessage `json:"body,omitempty"`
	BodyText      string          `json:"body_text,omitempty"`
}

func (f fixture) key() string { return f.Method + " " + f.Path + " " + f.RequestSHA256 }

// body is the recorded response body as bytes.
func (f fixture) body() []byte {
	if len(f.Body) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, f.Body); err == nil {
			return buf.Bytes()
		}
		return f.Body
	}
	return []byte(f.BodyText)
}

// requestHash is a digest of a request body that does not change with how the client
// laid it out: a JSON body is hashed as a value with sorted keys, a multipart body as
// its parts (name, file name, type, content digest) and not its boundary.
func requestHash(contentType string, body []byte) string {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	var normal []byte
	switch {
	case mediaType == "multipart/form-data" && params["boundary"] != "":
		normal = normaliseMultipart(body, params["boundary"])
	case len(bytes.TrimSpace(body)) > 0 && (mediaType == "application/json" || mediaType == ""):
		normal = normaliseJSON(body)
	}
	if normal == nil {
		normal = body
	}
	sum := sha256.Sum256(normal)
	return hex.EncodeToString(sum[:])
}

// normaliseJSON re-encodes body with sorted keys and no whitespace; nil when it is not JSON.
func normaliseJSON(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

// normaliseMultipart describes the parts of body, sorted; nil when it does not parse.
func normaliseMultipart(body []byte, boundary string) []byte {
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	var lines []string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf("%s\x00%s\x00%s\x00%x", p.FormName(), p.FileName(), p.Header.Get("Content-Type"), sum))
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n"))
}

// readBody returns the request body and puts it back so the request can still be sent.
func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return body, nil
}

// --- replay ---

type entry struct {
	f    fixture
	used bool
}

type replayer struct {
	dir   string
	count int
	mu    sync.Mutex
	byKey map[string][]*entry
}

// Replay returns a RoundTripper that answers from the fixtures in dir. It fails the
// test when dir has none or one cannot be read.
func Replay(t testing.TB, dir string) http.RoundTripper {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("httpfixture: no fixtures in %s (%v)", dir, err)
	}
	slices.Sort(files)
	r := &replayer{dir: dir, byKey: map[string][]*entry{}}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("httpfixture: %v", err)
		}
		var f fixture
		if err := json.Unmarshal(b, &f); err != nil || f.Method == "" || f.Path == "" || f.RequestSHA256 == "" || f.Status == 0 {
			t.Fatalf("httpfixture: %s is not a fixture (%v)", file, err)
		}
		r.byKey[f.key()] = append(r.byKey[f.key()], &entry{f: f})
		r.count++
	}
	return r
}

// RoundTrip implements http.RoundTripper.
func (r *replayer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readBody(req)
	if err != nil {
		return nil, fmt.Errorf("httpfixture: read request: %w", err)
	}
	hash := requestHash(req.Header.Get("Content-Type"), body)
	key := req.Method + " " + req.URL.Path + " " + hash

	r.mu.Lock()
	var hit *entry
	for _, e := range r.byKey[key] {
		if !e.used {
			e.used = true
			hit = e
			break
		}
	}
	r.mu.Unlock()
	if hit == nil {
		used := len(r.byKey[key])
		return nil, fmt.Errorf("httpfixture: no recorded exchange for %s %s with request body %.12s (%d of the %d fixtures in %s match it and are already used)", req.Method, req.URL.Path, hash, used, r.count, r.dir)
	}
	data := hit.f.body()
	h := http.Header{}
	if hit.f.ContentType != "" {
		h.Set("Content-Type", hit.f.ContentType)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", hit.f.Status, http.StatusText(hit.f.Status)),
		StatusCode:    hit.f.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: int64(len(data)),
		Request:       req,
	}, nil
}

// --- record ---

// keyShape matches a LiteLLM virtual key; the recorder removes one wherever it shows
// up, as the secret scan would.
var keyShape = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)

// Recorder is a RoundTripper that forwards requests to a live server and writes each
// answer to a fixture file. Name says what the next exchanges are called.
type Recorder struct {
	t    testing.TB
	dir  string
	next http.RoundTripper

	mu    sync.Mutex
	name  string
	seq   map[string]int
	scrub []replacement // hosts and credentials seen in requests, to remove from answers
}

// replacement removes from and writes to in its place.
type replacement struct{ from, to string }

// NewRecorder returns a Recorder that forwards to next and writes fixtures into dir.
// Delete the fixtures it replaces first: it overwrites by name and never removes.
func NewRecorder(t testing.TB, dir string, next http.RoundTripper) *Recorder {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("httpfixture: %v", err)
	}
	return &Recorder{t: t, dir: dir, next: next, seq: map[string]int{}}
}

// Name sets the name of the exchanges that follow: the first is written to
// <name>-01.json, the next to <name>-02.json, and so on. Replay serves same-named
// requests in that order.
func (r *Recorder) Name(name string) {
	r.mu.Lock()
	r.name = name
	r.mu.Unlock()
}

// learn adds to what must not reach a fixture: the host the request went to and any
// credential it carried.
func (r *Recorder) learn(req *http.Request) {
	add := func(from, to string) {
		if len(from) >= 4 && !slices.ContainsFunc(r.scrub, func(x replacement) bool { return x.from == from }) {
			r.scrub = append(r.scrub, replacement{from, to})
		}
	}
	add(req.URL.Host, FixtureHost)
	add(req.URL.Hostname(), FixtureHost)
	for _, v := range req.Header.Values("Authorization") {
		add(v, redacted)
		add(strings.TrimSpace(strings.TrimPrefix(v, "Bearer")), redacted)
	}
	for _, v := range req.Header.Values("X-Api-Key") {
		add(v, redacted)
	}
	// Longest first, so "host:port" goes before "host".
	slices.SortFunc(r.scrub, func(a, b replacement) int { return cmp.Compare(len(b.from), len(a.from)) })
}

// sanitise removes the learned hosts and credentials from an answer.
func (r *Recorder) sanitise(s string) string {
	for _, x := range r.scrub {
		s = strings.ReplaceAll(s, x.from, x.to)
	}
	return keyShape.ReplaceAllString(s, redacted)
}

// RoundTrip implements http.RoundTripper.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	name := r.name
	r.mu.Unlock()
	if name == "" {
		return nil, fmt.Errorf("httpfixture: Recorder.Name was not called before the request to %s", req.URL.Path)
	}
	body, err := readBody(req)
	if err != nil {
		return nil, fmt.Errorf("httpfixture: read request: %w", err)
	}
	hash := requestHash(req.Header.Get("Content-Type"), body)
	r.mu.Lock()
	r.learn(req)
	r.mu.Unlock()

	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("httpfixture: read response: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		r.t.Errorf("httpfixture: %s %s answered %d: fixtures hold successful answers only, nothing was written", req.Method, req.URL.Path, resp.StatusCode)
		return resp, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	f := fixture{
		Method:        req.Method,
		Path:          req.URL.Path,
		RequestSHA256: hash,
		Status:        resp.StatusCode,
		ContentType:   resp.Header.Get("Content-Type"),
	}
	text := r.sanitise(string(data))
	if json.Valid([]byte(text)) {
		f.Body = json.RawMessage(text)
	} else {
		f.BodyText = text
	}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("httpfixture: %w", err)
	}
	r.seq[name]++
	file := filepath.Join(r.dir, fmt.Sprintf("%s-%02d.json", name, r.seq[name]))
	if err := os.WriteFile(file, append(out, '\n'), 0o644); err != nil {
		return nil, fmt.Errorf("httpfixture: %w", err)
	}
	return resp, nil
}
