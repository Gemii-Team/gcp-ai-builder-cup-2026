package google_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"kiwi/internal/platform/google"
)

const meetScope = "https://www.googleapis.com/auth/meetings.space.created"

// seen is one request the Meet stand-in received.
type seen struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
	Body   []byte
}

// meetStub is an httptest server standing in for the Meet REST API; tests register
// the routes they need on mux and read back what the client sent.
type meetStub struct {
	*httptest.Server
	mux  *http.ServeMux
	mu   sync.Mutex
	reqs []seen
}

func newMeetStub(t *testing.T) *meetStub {
	t.Helper()
	m := &meetStub{mux: http.NewServeMux()}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("stub: read request body: %v", err)
		}
		m.mu.Lock()
		m.reqs = append(m.reqs, seen{r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Authorization"), body})
		m.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		m.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(m.Close)
	return m
}

// requests returns what the stub saw for path, in arrival order.
func (m *meetStub) requests(path string) []seen {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []seen
	for _, r := range m.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (m *meetStub) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("stub: encode response: %v", err)
	}
}

// tokenFor serves a static token for want and fails the test when the client asks
// for another user or another scope.
func tokenFor(t *testing.T, want uuid.UUID) google.TokenFunc {
	t.Helper()
	return func(_ context.Context, userID uuid.UUID, scope string) (oauth2.TokenSource, error) {
		if userID != want {
			t.Errorf("token requested for user %s, want %s", userID, want)
		}
		if scope != meetScope {
			t.Errorf("token requested for scope %q, want %q", scope, meetScope)
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok-" + userID.String()}), nil
	}
}

func newClient(t *testing.T, m *meetStub, tokens google.TokenFunc) google.Client {
	t.Helper()
	c, err := google.New(tokens, google.Options{MeetBaseURL: m.URL, HTTPClient: m.Client()})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	return c
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func expectTime(t *testing.T, what string, got time.Time, want string) {
	t.Helper()
	if !got.Equal(mustTime(t, want)) {
		t.Errorf("%s = %s, want %s", what, got.Format(time.RFC3339Nano), want)
	}
}

func expectNoTime(t *testing.T, what string, got *time.Time) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %s, want nil", what, got.Format(time.RFC3339Nano))
	}
}

func expectTimePtr(t *testing.T, what string, got *time.Time, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %s", what, want)
		return
	}
	expectTime(t, what, *got, want)
}

func expectMS(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %d", what, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", what, *got, want)
	}
}

const (
	transcriptName = "conferenceRecords/cr1/transcripts/t1"
	somchai        = "conferenceRecords/cr1/participants/p1"
	guest          = "conferenceRecords/cr1/participants/p2"
)

func entryJSON(idx int, participant, start, end, text string) map[string]any {
	e := map[string]any{
		"name": fmt.Sprintf("%s/entries/e%d", transcriptName, idx),
		"text": text,
	}
	if participant != "" {
		e["participant"] = participant
	}
	if start != "" {
		e["startTime"] = start
	}
	if end != "" {
		e["endTime"] = end
	}
	return e
}

func TestListTranscriptEntries_PaginatesAll(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	const start = "2026-10-11T10:00:00Z"
	m.mux.HandleFunc("GET /v2/"+transcriptName, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": transcriptName, "startTime": start, "state": "FILE_GENERATED"})
	})
	m.mux.HandleFunc("GET /v2/"+somchai, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": somchai, "signedinUser": map[string]any{"displayName": "Somchai"}})
	})
	type page struct {
		entries []map[string]any
		next    string
	}
	pages := map[string]page{
		"": {[]map[string]any{
			entryJSON(0, somchai, "2026-10-11T10:00:01Z", "2026-10-11T10:00:02Z", "สวัสดีครับ"),
			entryJSON(1, somchai, "2026-10-11T10:00:03Z", "2026-10-11T10:00:04Z", "second"),
		}, "page-2"},
		"page-2": {[]map[string]any{
			entryJSON(2, somchai, "2026-10-11T10:00:05Z", "2026-10-11T10:00:06Z", "third"),
			entryJSON(3, somchai, "2026-10-11T10:00:07Z", "2026-10-11T10:00:08Z", "fourth"),
		}, "page-3"},
		"page-3": {[]map[string]any{
			entryJSON(4, somchai, "2026-10-11T10:00:09Z", "2026-10-11T10:00:10Z", "fifth"),
		}, ""},
	}
	m.mux.HandleFunc("GET /v2/"+transcriptName+"/entries", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("pageSize"); got != "100" {
			t.Errorf("entries pageSize = %q, want 100", got)
		}
		p, ok := pages[r.URL.Query().Get("pageToken")]
		if !ok {
			http.Error(w, "unknown page token", http.StatusBadRequest)
			return
		}
		resp := map[string]any{"transcriptEntries": p.entries}
		if p.next != "" {
			resp["nextPageToken"] = p.next
		}
		writeJSON(t, w, resp)
	})

	got, err := newClient(t, m, tokenFor(t, user)).ListTranscriptEntries(t.Context(), user, transcriptName)
	if err != nil {
		t.Fatalf("ListTranscriptEntries: %v", err)
	}
	wantText := []string{"สวัสดีครับ", "second", "third", "fourth", "fifth"}
	if len(got) != len(wantText) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(wantText), got)
	}
	for i, e := range got {
		if e.Idx != i {
			t.Errorf("entry %d: Idx = %d, want %d", i, e.Idx, i)
		}
		if e.Text != wantText[i] {
			t.Errorf("entry %d: Text = %q, want %q", i, e.Text, wantText[i])
		}
		if e.Speaker != "Somchai" {
			t.Errorf("entry %d: Speaker = %q, want Somchai", i, e.Speaker)
		}
	}
	reqs := m.requests("/v2/" + transcriptName + "/entries")
	if len(reqs) != 3 {
		t.Fatalf("entries requests = %d, want 3", len(reqs))
	}
	for i, want := range []string{"", "page-2", "page-3"} {
		if got := reqs[i].Query.Get("pageToken"); got != want {
			t.Errorf("entries request %d: pageToken = %q, want %q", i, got, want)
		}
		if want := "Bearer tok-" + user.String(); reqs[i].Auth != want {
			t.Errorf("entries request %d: Authorization = %q, want the invoking user's token", i, reqs[i].Auth)
		}
	}
}

func TestListTranscriptEntries_ResolvesParticipantNamesOncePerParticipant(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("GET /v2/"+transcriptName, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": transcriptName, "startTime": "2026-10-11T10:00:00Z"})
	})
	m.mux.HandleFunc("GET /v2/"+somchai, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": somchai, "signedinUser": map[string]any{"displayName": "Somchai", "user": "users/1"}})
	})
	m.mux.HandleFunc("GET /v2/"+guest, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": guest, "anonymousUser": map[string]any{"displayName": "Guest 7"}})
	})
	m.mux.HandleFunc("GET /v2/"+transcriptName+"/entries", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			writeJSON(t, w, map[string]any{"transcriptEntries": []map[string]any{
				entryJSON(0, somchai, "", "", "a"),
				entryJSON(1, guest, "", "", "b"),
				entryJSON(2, somchai, "", "", "c"),
			}, "nextPageToken": "next"})
			return
		}
		writeJSON(t, w, map[string]any{"transcriptEntries": []map[string]any{
			entryJSON(3, guest, "", "", "d"),
			entryJSON(4, somchai, "", "", "e"),
		}})
	})
	c := newClient(t, m, tokenFor(t, user))

	got, err := c.ListTranscriptEntries(t.Context(), user, transcriptName)
	if err != nil {
		t.Fatalf("ListTranscriptEntries: %v", err)
	}
	wantSpeaker := []string{"Somchai", "Guest 7", "Somchai", "Guest 7", "Somchai"}
	if len(got) != len(wantSpeaker) {
		t.Fatalf("got %d entries, want %d", len(got), len(wantSpeaker))
	}
	for i, e := range got {
		if e.Speaker != wantSpeaker[i] {
			t.Errorf("entry %d: Speaker = %q, want %q", i, e.Speaker, wantSpeaker[i])
		}
	}
	for _, p := range []string{somchai, guest} {
		if n := len(m.requests("/v2/" + p)); n != 1 {
			t.Errorf("lookups of %s = %d, want 1", p, n)
		}
	}

	// The cache lives for one call: names can change between polls.
	if _, err := c.ListTranscriptEntries(t.Context(), user, transcriptName); err != nil {
		t.Fatalf("second ListTranscriptEntries: %v", err)
	}
	for _, p := range []string{somchai, guest} {
		if n := len(m.requests("/v2/" + p)); n != 2 {
			t.Errorf("lookups of %s after two calls = %d, want 2", p, n)
		}
	}
}

func TestEntryOffsetsAreRelativeToTranscriptStart(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("GET /v2/"+transcriptName, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": transcriptName, "startTime": "2026-10-11T10:00:00Z"})
	})
	m.mux.HandleFunc("GET /v2/"+somchai, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"name": somchai, "signedinUser": map[string]any{"displayName": "Somchai"}})
	})
	m.mux.HandleFunc("GET /v2/"+transcriptName+"/entries", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"transcriptEntries": []map[string]any{
			entryJSON(0, somchai, "2026-10-11T10:00:00Z", "2026-10-11T10:00:04.500Z", "at the very start"),
			entryJSON(1, somchai, "2026-10-11T10:01:05.250Z", "2026-10-11T10:01:09Z", "later, with a fraction"),
			entryJSON(2, somchai, "2026-10-11T11:30:00Z", "2026-10-11T11:30:01Z", "past the first hour"),
			entryJSON(3, somchai, "", "", "no timing in the source"),
		}})
	})

	got, err := newClient(t, m, tokenFor(t, user)).ListTranscriptEntries(t.Context(), user, transcriptName)
	if err != nil {
		t.Fatalf("ListTranscriptEntries: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4", len(got))
	}
	expectMS(t, "entry 0 StartMS", got[0].StartMS, 0)
	expectMS(t, "entry 0 EndMS", got[0].EndMS, 4500)
	expectMS(t, "entry 1 StartMS", got[1].StartMS, 65250)
	expectMS(t, "entry 1 EndMS", got[1].EndMS, 69000)
	expectMS(t, "entry 2 StartMS", got[2].StartMS, 90*60*1000)
	expectMS(t, "entry 2 EndMS", got[2].EndMS, 90*60*1000+1000)
	if got[3].StartMS != nil || got[3].EndMS != nil {
		t.Errorf("entry without timing: StartMS=%v EndMS=%v, want nil nil", got[3].StartMS, got[3].EndMS)
	}
}

func TestCreateMeetSpace_TranscriptionConfigOnlyWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enable  bool
		wantCfg bool
	}{
		{"disabled sends no config", false, false},
		{"enabled sends auto transcription", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMeetStub(t)
			user := uuid.New()
			m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{
					"name":        "spaces/abc123",
					"meetingUri":  "https://meet.google.com/abc-mnop-xyz",
					"meetingCode": "abc-mnop-xyz",
				})
			})

			got, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, tc.enable)
			if err != nil {
				t.Fatalf("CreateMeetSpace: %v", err)
			}
			want := google.Space{Name: "spaces/abc123", MeetingURI: "https://meet.google.com/abc-mnop-xyz", Code: "abc-mnop-xyz", TranscriptionAutomatic: tc.enable}
			if got != want {
				t.Errorf("space = %+v, want %+v", got, want)
			}

			reqs := m.requests("/v2/spaces")
			if len(reqs) != 1 {
				t.Fatalf("spaces.create requests = %d, want 1", len(reqs))
			}
			if want := "Bearer tok-" + user.String(); reqs[0].Auth != want {
				t.Errorf("Authorization = %q, want the invoking user's token", reqs[0].Auth)
			}
			var body map[string]any
			if len(bytes.TrimSpace(reqs[0].Body)) > 0 {
				if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
					t.Fatalf("request body is not JSON: %v (%s)", err, reqs[0].Body)
				}
			}
			if !tc.wantCfg {
				if len(body) != 0 {
					t.Errorf("request body = %s, want no space config", reqs[0].Body)
				}
				return
			}
			var wantBody map[string]any
			const wantJSON = `{"config":{"artifactConfig":{"transcriptionConfig":{"autoTranscriptionGeneration":"ON"}}}}`
			if err := json.Unmarshal([]byte(wantJSON), &wantBody); err != nil {
				t.Fatal(err)
			}
			gotCanon, _ := json.Marshal(body)
			wantCanon, _ := json.Marshal(wantBody)
			if !bytes.Equal(gotCanon, wantCanon) {
				t.Errorf("request body = %s, want %s", gotCanon, wantCanon)
			}
		})
	}
}

func TestListConferences_FiltersBySpaceAndMapsTranscriptStates(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	const space = "spaces/abc123"
	m.mux.HandleFunc("GET /v2/conferenceRecords", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got, want := q.Get("filter"), `space.name = "spaces/abc123"`; got != want {
			t.Errorf("conferenceRecords filter = %q, want %q", got, want)
		}
		if got := q.Get("pageSize"); got != "100" {
			t.Errorf("conferenceRecords pageSize = %q, want 100", got)
		}
		switch q.Get("pageToken") {
		case "":
			writeJSON(t, w, map[string]any{
				"conferenceRecords": []map[string]any{{
					"name": "conferenceRecords/cr-done", "space": space,
					"startTime": "2026-10-11T10:00:00Z", "endTime": "2026-10-11T10:45:00Z",
				}},
				"nextPageToken": "records-2",
			})
		case "records-2":
			writeJSON(t, w, map[string]any{"conferenceRecords": []map[string]any{{
				"name": "conferenceRecords/cr-live", "space": space, "startTime": "2026-10-11T12:00:00Z",
			}}})
		default:
			http.Error(w, "unknown page token", http.StatusBadRequest)
		}
	})
	m.mux.HandleFunc("GET /v2/conferenceRecords/cr-done/transcripts", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("pageSize"); got != "100" {
			t.Errorf("transcripts pageSize = %q, want 100", got)
		}
		if r.URL.Query().Get("pageToken") == "" {
			writeJSON(t, w, map[string]any{"transcripts": []map[string]any{
				{"name": "conferenceRecords/cr-done/transcripts/started", "state": "STARTED", "startTime": "2026-10-11T10:01:00Z"},
				{"name": "conferenceRecords/cr-done/transcripts/ended", "state": "ENDED", "startTime": "2026-10-11T10:02:00Z", "endTime": "2026-10-11T10:44:00Z"},
			}, "nextPageToken": "transcripts-2"})
			return
		}
		writeJSON(t, w, map[string]any{"transcripts": []map[string]any{
			{"name": "conferenceRecords/cr-done/transcripts/file", "state": "FILE_GENERATED", "startTime": "2026-10-11T10:03:00Z", "endTime": "2026-10-11T10:45:00Z"},
		}})
	})
	m.mux.HandleFunc("GET /v2/conferenceRecords/cr-live/transcripts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{})
	})

	got, err := newClient(t, m, tokenFor(t, user)).ListConferences(t.Context(), user, space)
	if err != nil {
		t.Fatalf("ListConferences: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d conferences, want 2: %+v", len(got), got)
	}

	done := got[0]
	if done.Name != "conferenceRecords/cr-done" {
		t.Errorf("conference 0 Name = %q", done.Name)
	}
	expectTime(t, "conference 0 StartTime", done.StartTime, "2026-10-11T10:00:00Z")
	expectTimePtr(t, "conference 0 EndTime", done.EndTime, "2026-10-11T10:45:00Z")
	if len(done.Transcripts) != 3 {
		t.Fatalf("conference 0 has %d transcripts, want 3: %+v", len(done.Transcripts), done.Transcripts)
	}
	wantRefs := []struct {
		name  string
		state google.TranscriptState
		end   string
	}{
		{"conferenceRecords/cr-done/transcripts/started", google.StateStarted, ""},
		{"conferenceRecords/cr-done/transcripts/ended", google.StateEnded, "2026-10-11T10:44:00Z"},
		{"conferenceRecords/cr-done/transcripts/file", google.StateFileGenerated, "2026-10-11T10:45:00Z"},
	}
	for i, w := range wantRefs {
		ref := done.Transcripts[i]
		if ref.Name != w.name || ref.State != w.state {
			t.Errorf("transcript %d = {%s %s}, want {%s %s}", i, ref.Name, ref.State, w.name, w.state)
		}
		if w.end == "" {
			expectNoTime(t, fmt.Sprintf("transcript %d EndTime", i), ref.EndTime)
		} else {
			expectTimePtr(t, fmt.Sprintf("transcript %d EndTime", i), ref.EndTime, w.end)
		}
	}

	live := got[1]
	if live.Name != "conferenceRecords/cr-live" {
		t.Errorf("conference 1 Name = %q", live.Name)
	}
	expectTime(t, "conference 1 StartTime", live.StartTime, "2026-10-11T12:00:00Z")
	expectNoTime(t, "active conference EndTime", live.EndTime)
	if len(live.Transcripts) != 0 {
		t.Errorf("active conference has %d transcripts, want 0", len(live.Transcripts))
	}

	reqs := m.requests("/v2/conferenceRecords")
	if len(reqs) != 2 {
		t.Fatalf("conferenceRecords requests = %d, want 2 (two pages)", len(reqs))
	}
	for i, r := range reqs {
		if r.Query.Get("filter") != `space.name = "spaces/abc123"` {
			t.Errorf("page %d dropped the space filter: %q", i, r.Query.Get("filter"))
		}
	}
}

func TestListConferences_UnknownTranscriptStateIsAnError(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("GET /v2/conferenceRecords", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"conferenceRecords": []map[string]any{{
			"name": "conferenceRecords/cr1", "startTime": "2026-10-11T10:00:00Z",
		}}})
	})
	m.mux.HandleFunc("GET /v2/conferenceRecords/cr1/transcripts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"transcripts": []map[string]any{
			{"name": "conferenceRecords/cr1/transcripts/t1", "state": "STATE_UNSPECIFIED"},
		}})
	})

	_, err := newClient(t, m, tokenFor(t, user)).ListConferences(t.Context(), user, "spaces/abc123")
	if err == nil {
		t.Fatal("ListConferences accepted a transcript state outside STARTED|ENDED|FILE_GENERATED")
	}
	const want = `transcript conferenceRecords/cr1/transcripts/t1 has unexpected state "STATE_UNSPECIFIED"`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to name the transcript and the state: %q", err, want)
	}
}

func TestListConferences_QuotesTheSpaceNameInTheFilter(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("GET /v2/conferenceRecords", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{})
	})

	if _, err := newClient(t, m, tokenFor(t, user)).ListConferences(t.Context(), user, `spaces/a"b\c`); err != nil {
		t.Fatalf("ListConferences: %v", err)
	}

	reqs := m.requests("/v2/conferenceRecords")
	if len(reqs) != 1 {
		t.Fatalf("conferenceRecords requests = %d, want 1", len(reqs))
	}
	// The quote and the backslash are escaped, so the name cannot end the string and
	// widen the filter.
	if got, want := reqs[0].Query.Get("filter"), `space.name = "spaces/a\"b\\c"`; got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
}

func TestListConferences_ZeroEndTimeMapsToNil(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	const zero = "0001-01-01T00:00:00Z"
	m.mux.HandleFunc("GET /v2/conferenceRecords", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"conferenceRecords": []map[string]any{{
			"name": "conferenceRecords/cr1", "startTime": "2026-10-11T10:00:00Z", "endTime": zero,
		}}})
	})
	m.mux.HandleFunc("GET /v2/conferenceRecords/cr1/transcripts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"transcripts": []map[string]any{
			{"name": "conferenceRecords/cr1/transcripts/t1", "state": "STARTED", "endTime": zero},
		}})
	})

	got, err := newClient(t, m, tokenFor(t, user)).ListConferences(t.Context(), user, "spaces/abc123")
	if err != nil {
		t.Fatalf("ListConferences: %v", err)
	}
	if len(got) != 1 || len(got[0].Transcripts) != 1 {
		t.Fatalf("got %+v, want one conference with one transcript", got)
	}
	// A conference that Meet reports with the zero time is still running: reading it
	// as ended in year 1 would schedule the no-transcript check in the past.
	expectNoTime(t, "conference EndTime", got[0].EndTime)
	expectNoTime(t, "transcript EndTime", got[0].Transcripts[0].EndTime)
}

func TestCalls_PreCancelledContextFailsWithoutReachingMeet(t *testing.T) {
	user := uuid.New()
	calls := []struct {
		name string
		call func(context.Context, google.Client) error
	}{
		{"CreateMeetSpace", func(ctx context.Context, c google.Client) error {
			_, err := c.CreateMeetSpace(ctx, user, true)
			return err
		}},
		{"ListConferences", func(ctx context.Context, c google.Client) error {
			_, err := c.ListConferences(ctx, user, "spaces/abc123")
			return err
		}},
		{"ListTranscriptEntries", func(ctx context.Context, c google.Client) error {
			_, err := c.ListTranscriptEntries(ctx, user, transcriptName)
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			m := newMeetStub(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := c.call(ctx, newClient(t, m, tokenFor(t, user)))

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want it to wrap context.Canceled", err)
			}
			if google.IsAccessDenied(err) {
				t.Errorf("a cancelled context was reported as lost access: %v", err)
			}
			if m.total() != 0 {
				t.Errorf("Meet saw %d requests from a cancelled context", m.total())
			}
		})
	}
}

// revoked is a token source whose grant Google has revoked, the way
// identity.UserTokenSource reports it once it asks Google for an access token.
type revoked struct{ err error }

func (r revoked) Token() (*oauth2.Token, error) { return nil, r.err }

func TestConsentRequiredPropagates(t *testing.T) {
	user := uuid.New()
	calls := []struct {
		name string
		call func(google.Client) error
	}{
		{"CreateMeetSpace", func(c google.Client) error {
			_, err := c.CreateMeetSpace(t.Context(), user, true)
			return err
		}},
		{"ListConferences", func(c google.Client) error {
			_, err := c.ListConferences(t.Context(), user, "spaces/abc123")
			return err
		}},
		{"ListTranscriptEntries", func(c google.Client) error {
			_, err := c.ListTranscriptEntries(t.Context(), user, transcriptName)
			return err
		}},
	}
	sources := []struct {
		name     string
		tokens   func(want *google.ConsentRequiredError) google.TokenFunc
		noTokens bool // the TokenFunc itself refuses, so no request may reach Meet
	}{
		{"no grant for the scope", func(want *google.ConsentRequiredError) google.TokenFunc {
			return func(context.Context, uuid.UUID, string) (oauth2.TokenSource, error) { return nil, want }
		}, true},
		{"grant revoked at refresh", func(want *google.ConsentRequiredError) google.TokenFunc {
			return func(context.Context, uuid.UUID, string) (oauth2.TokenSource, error) { return revoked{want}, nil }
		}, false},
	}
	for _, src := range sources {
		for _, c := range calls {
			t.Run(src.name+"/"+c.name, func(t *testing.T) {
				m := newMeetStub(t)
				want := &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}

				err := c.call(newClient(t, m, src.tokens(want)))

				var got *google.ConsentRequiredError
				if !errors.As(err, &got) {
					t.Fatalf("err = %v (%T), want *google.ConsentRequiredError", err, err)
				}
				if got != want {
					t.Errorf("consent error was replaced: got %+v, want the original %+v", got, want)
				}
				if direct, ok := err.(*google.ConsentRequiredError); !ok || direct != want {
					t.Errorf("err = %v (%T), want the *google.ConsentRequiredError itself, not a wrapper", err, err)
				}
				if src.noTokens && m.total() != 0 {
					t.Errorf("Meet saw %d requests without a token", m.total())
				}
			})
		}
	}
}
