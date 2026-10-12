package google_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"kiwi/internal/platform/google"
)

// googleText is the message every stubbed Google error carries. It stands for what a
// real answer holds (the project, the account, the feature) and must never reach a
// MeetError's text.
const googleText = "SECRET-GOOGLE-TEXT project 123456 account ana@example.org"

// featureUnavailableBody is what Meet answers a personal Google account that asks for
// auto-transcription.
const featureUnavailableBody = `{
  "error": {
    "code": 403,
    "message": "updateAutoTranscriptionGeneration is not available to the user.",
    "status": "PERMISSION_DENIED",
    "details": [
      {"@type": "type.googleapis.com/google.rpc.ErrorInfo", "domain": "meet.googleapis.com",
       "metadata": {"feature_name": "updateAutoTranscriptionGeneration"}, "reason": "FEATURE_UNAVAILABLE_TO_USER"},
      {"@type": "type.googleapis.com/google.rpc.LocalizedMessage", "locale": "en-US",
       "message": "updateAutoTranscriptionGeneration is not available to the user."}
    ]
  }
}`

// errorJSON is a Google API error body: code, status and message, and one ErrorInfo
// detail when reason is set.
func errorJSON(code int, status, reason, domain string, metadata map[string]string) string {
	e := map[string]any{"code": code, "message": googleText, "status": status}
	if reason != "" {
		e["details"] = []any{map[string]any{
			"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": reason, "domain": domain, "metadata": metadata,
		}}
	}
	b, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeErrorBody(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// carriesConfig reports whether a spaces.create body asks for any space config.
func carriesConfig(t *testing.T, body []byte) bool {
	t.Helper()
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body is not JSON: %v (%s)", err, body)
	}
	_, ok := req["config"]
	return ok
}

func okSpace(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeJSON(t, w, map[string]any{
		"name": "spaces/abc123", "meetingUri": "https://meet.google.com/abc-mnop-xyz", "meetingCode": "abc-mnop-xyz",
	})
}

func TestCreateMeetSpace_FeatureUnavailable_RetriesWithoutTranscriptionConfig(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		if carriesConfig(t, body.Bytes()) {
			writeErrorBody(w, http.StatusForbidden, featureUnavailableBody)
			return
		}
		okSpace(t, w)
	})

	got, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, true)
	if err != nil {
		t.Fatalf("CreateMeetSpace = %v, want the space made without the setting", err)
	}
	want := google.Space{Name: "spaces/abc123", MeetingURI: "https://meet.google.com/abc-mnop-xyz", Code: "abc-mnop-xyz", TranscriptionAutomatic: false}
	if got != want {
		t.Errorf("space = %+v, want %+v (automatic transcription off)", got, want)
	}
	reqs := m.requests("/v2/spaces")
	if len(reqs) != 2 {
		t.Fatalf("spaces.create requests = %d, want 2 (with the config, then without)", len(reqs))
	}
	if !carriesConfig(t, reqs[0].Body) {
		t.Errorf("first request = %s, want the transcription config", reqs[0].Body)
	}
	if carriesConfig(t, reqs[1].Body) {
		t.Errorf("second request = %s, want no config at all", reqs[1].Body)
	}
	for i, r := range reqs {
		if want := "Bearer tok-" + user.String(); r.Auth != want {
			t.Errorf("request %d Authorization = %q, want the invoking user's token", i, r.Auth)
		}
	}
}

func TestCreateMeetSpace_FeatureUnavailable_FallbackThatFailsReturnsItsOwnTypedError(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		if carriesConfig(t, body.Bytes()) {
			writeErrorBody(w, http.StatusForbidden, featureUnavailableBody)
			return
		}
		writeErrorBody(w, http.StatusServiceUnavailable, errorJSON(503, "UNAVAILABLE", "", "", nil))
	})

	_, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, true)

	var me *google.MeetError
	if !errors.As(err, &me) || me.Cause != google.CauseUnavailable {
		t.Fatalf("err = %v, want the retry's own failure, classified unavailable", err)
	}
	if n := len(m.requests("/v2/spaces")); n != 2 {
		t.Errorf("spaces.create requests = %d, want exactly one retry (2)", n)
	}
}

// fallbackLog returns the records CreateMeetSpace logs, one JSON object each, through
// the default logger, which is what the package logs with.
func fallbackLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	logs := new(bytes.Buffer)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

const fallbackLogMessage = "meet: auto-transcription unavailable for this account, space created without it"

// The operator sees that an account does not get auto-transcription: one INFO record
// when the fallback fires and works, with the user's id and nothing of the answer or the
// token; none when the call went through the first time or when the fallback failed too
// (that one is the API's error log).
func TestCreateMeetSpace_Fallback_LogsOneInfoRecordWithTheUserOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retry    func(w http.ResponseWriter)
		withFlag bool
		want     int
	}{
		{"the fallback works", func(w http.ResponseWriter) { okSpace(t, w) }, true, 1},
		{"the fallback fails too", func(w http.ResponseWriter) {
			writeErrorBody(w, http.StatusServiceUnavailable, errorJSON(503, "UNAVAILABLE", "", "", nil))
		}, true, 0},
		{"the account has the feature", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := fallbackLog(t)
			m := newMeetStub(t)
			user := uuid.New()
			m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, r *http.Request) {
				body := new(bytes.Buffer)
				_, _ = body.ReadFrom(r.Body)
				switch {
				case tc.withFlag && carriesConfig(t, body.Bytes()):
					writeErrorBody(w, http.StatusForbidden, featureUnavailableBody)
				case tc.retry != nil:
					tc.retry(w)
				default:
					okSpace(t, w)
				}
			})

			_, _ = newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, true)

			var records []map[string]any
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				var r map[string]any
				if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == fallbackLogMessage {
					records = append(records, r)
				}
			}
			if len(records) != tc.want {
				t.Fatalf("%d records of %q, want %d: %s", len(records), fallbackLogMessage, tc.want, logs)
			}
			if tc.want == 0 {
				return
			}
			if records[0]["level"] != "INFO" || records[0]["user_id"] != user.String() {
				t.Errorf("record = %v, want INFO with user_id %s", records[0], user)
			}
			for _, leak := range []string{"tok-", "Bearer", "SECRET", "updateAutoTranscriptionGeneration"} {
				if strings.Contains(logs.String(), leak) {
					t.Errorf("the log carries %q: only the user's id belongs in it: %s", leak, logs)
				}
			}
		})
	}
}

// Only Meet's refusal of the auto-transcription feature triggers the retry. Anything
// else, including a 403 for another reason or for another feature, fails the call
// after one request: creating a space without the setting would hide a real problem.
func TestCreateMeetSpace_Other403_DoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"403 without details", 403, `{"error":{"code":403,"message":"nope","status":"PERMISSION_DENIED"}}`},
		{"403 of another reason", 403, errorJSON(403, "PERMISSION_DENIED", "SERVICE_DISABLED", "googleapis.com", nil)},
		{"feature reason for another feature", 403, errorJSON(403, "PERMISSION_DENIED", "FEATURE_UNAVAILABLE_TO_USER", "meet.googleapis.com",
			map[string]string{"feature_name": "updateModerationConfig"})},
		{"feature name without the feature reason", 403, errorJSON(403, "PERMISSION_DENIED", "PERMISSION_DENIED", "meet.googleapis.com",
			map[string]string{"feature_name": "updateAutoTranscriptionGeneration"})},
		{"429", 429, errorJSON(429, "RESOURCE_EXHAUSTED", "", "", nil)},
		{"500", 500, errorJSON(500, "INTERNAL", "", "", nil)},
		{"401", 401, errorJSON(401, "UNAUTHENTICATED", "", "", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMeetStub(t)
			user := uuid.New()
			m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, _ *http.Request) {
				writeErrorBody(w, tc.status, tc.body)
			})

			_, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, true)

			if err == nil {
				t.Fatal("CreateMeetSpace succeeded against a failing Meet")
			}
			if n := len(m.requests("/v2/spaces")); n != 1 {
				t.Errorf("spaces.create requests = %d, want 1: only the feature refusal retries", n)
			}
		})
	}
}

func TestCreateMeetSpace_NoFeatureProblem_KeepsTranscriptionOn(t *testing.T) {
	m := newMeetStub(t)
	user := uuid.New()
	m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, _ *http.Request) { okSpace(t, w) })

	got, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, true)
	if err != nil {
		t.Fatalf("CreateMeetSpace: %v", err)
	}
	if !got.TranscriptionAutomatic {
		t.Errorf("space = %+v, want TranscriptionAutomatic when Meet took the setting", got)
	}
	reqs := m.requests("/v2/spaces")
	if len(reqs) != 1 || !carriesConfig(t, reqs[0].Body) {
		t.Errorf("requests = %+v, want one request carrying the config", reqs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingClient is a client whose every request fails before it gets an answer.
func failingClient(t *testing.T, user uuid.UUID, cause error) google.Client {
	t.Helper()
	c, err := google.New(tokenFor(t, user), google.Options{
		MeetBaseURL: "http://meet.invalid",
		HTTPClient:  &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })},
	})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	return c
}

func TestMeetError_ClassifiesGoogleFailures(t *testing.T) {
	scope := errorJSON(403, "PERMISSION_DENIED", "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "googleapis.com", map[string]string{"service": "meet.googleapis.com"})
	disabled := errorJSON(403, "PERMISSION_DENIED", "SERVICE_DISABLED", "googleapis.com", map[string]string{"service": "meet.googleapis.com", "consumer": "projects/123456"})
	legacyDisabled := `{"error":{"code":403,"message":"` + googleText + `","errors":[{"domain":"usageLimits","reason":"accessNotConfigured","message":"` + googleText + `"}]}}`

	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantReason string
		wantCause  string
	}{
		{"401", 401, errorJSON(401, "UNAUTHENTICATED", "", "", nil), google.ReasonMeetAccess, google.CauseAccessDenied},
		{"403 insufficient scope", 403, scope, google.ReasonMeetAccess, google.CauseAccessDenied},
		{"403 SERVICE_DISABLED", 403, disabled, google.ReasonMeetUnavailable, google.CauseAPIDisabled},
		{"403 accessNotConfigured, legacy errors list", 403, legacyDisabled, google.ReasonMeetUnavailable, google.CauseAPIDisabled},
		{"403 for any other reason", 403, errorJSON(403, "PERMISSION_DENIED", "SOMETHING_ELSE", "meet.googleapis.com", nil), google.ReasonMeetUnavailable, google.CauseRefused},
		{"403 without details", 403, `{"error":{"code":403,"message":"` + googleText + `"}}`, google.ReasonMeetUnavailable, google.CauseRefused},
		{"429", 429, errorJSON(429, "RESOURCE_EXHAUSTED", "", "", nil), google.ReasonMeetUnavailable, google.CauseRateLimited},
		{"500", 500, errorJSON(500, "INTERNAL", "", "", nil), google.ReasonMeetUnavailable, google.CauseUnavailable},
		{"503", 503, errorJSON(503, "UNAVAILABLE", "", "", nil), google.ReasonMeetUnavailable, google.CauseUnavailable},
		{"400", 400, errorJSON(400, "INVALID_ARGUMENT", "", "", nil), google.ReasonMeetUnavailable, google.CauseRefused},
		{"404", 404, errorJSON(404, "NOT_FOUND", "", "", nil), google.ReasonMeetUnavailable, google.CauseRefused},
		{"an error that is not JSON", 502, "<html>" + googleText + "</html>", google.ReasonMeetUnavailable, google.CauseUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMeetStub(t)
			user := uuid.New()
			m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, _ *http.Request) { writeErrorBody(w, tc.status, tc.body) })

			_, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, false)

			expectMeetError(t, err, tc.wantReason, tc.wantCause, tc.status)
			var api *googleapi.Error
			if !errors.As(err, &api) || api.Code != tc.status {
				t.Errorf("err = %v, want the original *googleapi.Error %d kept in the chain for the operator's log", err, tc.status)
			}
		})
	}
}

func TestMeetError_NetworkAndTimeoutErrorsAreUnavailable(t *testing.T) {
	user := uuid.New()
	for name, cause := range map[string]error{
		"connection refused": &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused " + googleText)},
		"deadline exceeded":  context.DeadlineExceeded,
		"unexpected EOF":     fmt.Errorf("read: %w", errors.New("unexpected EOF "+googleText)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := failingClient(t, user, cause).CreateMeetSpace(t.Context(), user, false)

			expectMeetError(t, err, google.ReasonMeetUnavailable, google.CauseUnavailable, 0)
		})
	}
}

// A token endpoint that refuses the OAuth client (invalid_client, unauthorized_client:
// the operator's credentials are wrong or disabled) fails the request before it reaches
// Meet. The http.Client wraps that error in a *url.Error, as it does a network failure,
// but Google did answer: it is a refusal, not "Meet isn't answering".
func TestMeetError_TokenEndpointRefusal_IsRefusedNotUnavailable(t *testing.T) {
	user := uuid.New()
	for _, code := range []string{"invalid_client", "unauthorized_client", "invalid_request"} {
		t.Run(code, func(t *testing.T) {
			m := newMeetStub(t)
			refusal := &oauth2.RetrieveError{Response: &http.Response{StatusCode: 401}, ErrorCode: code, ErrorDescription: googleText}
			tokens := func(context.Context, uuid.UUID, string) (oauth2.TokenSource, error) { return revoked{refusal}, nil }

			_, err := newClient(t, m, tokens).CreateMeetSpace(t.Context(), user, false)

			expectMeetError(t, err, google.ReasonMeetUnavailable, google.CauseRefused, 0)
			var got *oauth2.RetrieveError
			if !errors.As(err, &got) || got.ErrorCode != code || !strings.Contains(err.Error(), googleText) {
				t.Errorf("err = %v, want the token endpoint's own error kept whole in the chain for the operator's log", err)
			}
			if n := m.total(); n != 0 {
				t.Errorf("Meet saw %d requests, want none: the token was never issued", n)
			}
		})
	}
}

// The same classification covers every method of the client, not only the one
// StartMeeting calls, and the poll's access check still sees what it always saw.
func TestMeetError_EveryClientMethodClassifies(t *testing.T) {
	user := uuid.New()
	calls := []struct {
		name string
		path string
		call func(google.Client) error
	}{
		{"ListConferences", "GET /v2/conferenceRecords", func(c google.Client) error {
			_, err := c.ListConferences(t.Context(), user, "spaces/abc123")
			return err
		}},
		{"ListTranscriptEntries", "GET /v2/" + transcriptName, func(c google.Client) error {
			_, err := c.ListTranscriptEntries(t.Context(), user, transcriptName)
			return err
		}},
	}
	for _, c := range calls {
		for _, tc := range []struct {
			status     int
			wantCause  string
			wantDenied bool
		}{
			{401, google.CauseAccessDenied, true},
			{403, google.CauseRefused, true},
			{429, google.CauseRateLimited, false},
			{500, google.CauseUnavailable, false},
		} {
			t.Run(fmt.Sprintf("%s/%d", c.name, tc.status), func(t *testing.T) {
				m := newMeetStub(t)
				m.mux.HandleFunc(c.path, func(w http.ResponseWriter, _ *http.Request) {
					writeErrorBody(w, tc.status, errorJSON(tc.status, "ERR", "", "", nil))
				})

				err := c.call(newClient(t, m, tokenFor(t, user)))

				var me *google.MeetError
				if !errors.As(err, &me) || me.Cause != tc.wantCause {
					t.Fatalf("err = %v, want a *MeetError with cause %s", err, tc.wantCause)
				}
				if got := google.IsAccessDenied(err); got != tc.wantDenied {
					t.Errorf("IsAccessDenied = %v for HTTP %d, want %v (the poll's decision must not change)", got, tc.status, tc.wantDenied)
				}
			})
		}
	}
}

// Failures that are not Meet's stay plain errors, which the API answers as internal:
// a cancelled request, a response nobody can read, a consent error (its own type).
func TestMeetError_OtherFailuresStayPlain(t *testing.T) {
	user := uuid.New()
	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := failingClient(t, user, context.Canceled).CreateMeetSpace(ctx, user, false)
		var me *google.MeetError
		if err == nil || errors.As(err, &me) {
			t.Errorf("err = %v, want a plain error: a cancelled request is not Meet's failure", err)
		}
	})
	t.Run("a success body that is not a space", func(t *testing.T) {
		m := newMeetStub(t)
		m.mux.HandleFunc("POST /v2/spaces", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name": [`))
		})
		_, err := newClient(t, m, tokenFor(t, user)).CreateMeetSpace(t.Context(), user, false)
		var me *google.MeetError
		if err == nil || errors.As(err, &me) {
			t.Errorf("err = %v, want a plain error", err)
		}
	})
	t.Run("a token source that fails for a reason of its own", func(t *testing.T) {
		boom := errors.New("vault: cannot open the grant")
		tokens := func(context.Context, uuid.UUID, string) (oauth2.TokenSource, error) { return nil, boom }
		c, err := google.New(tokens, google.Options{MeetBaseURL: "http://meet.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CreateMeetSpace(t.Context(), user, false)
		var me *google.MeetError
		if !errors.Is(err, boom) || errors.As(err, &me) {
			t.Errorf("err = %v, want a plain error wrapping the vault's", err)
		}
	})
}

// expectMeetError checks that err holds a *MeetError of the given reason, cause and
// status whose own text carries nothing of Google's answer.
func expectMeetError(t *testing.T, err error, reason, cause string, status int) {
	t.Helper()
	var me *google.MeetError
	if !errors.As(err, &me) {
		t.Fatalf("err = %v (%T), want a *google.MeetError in the chain", err, err)
	}
	if me.Reason != reason || me.Cause != cause || me.Status != status {
		t.Errorf("MeetError = {%s %s %d}, want {%s %s %d}", me.Reason, me.Cause, me.Status, reason, cause, status)
	}
	for _, leak := range []string{"SECRET", "123456", "ana@example.org", "projects/", "meet.invalid", "127.0.0.1", "http://", "googleapi"} {
		if strings.Contains(me.Error(), leak) {
			t.Errorf("MeetError text %q contains %q: Google's answer must not leave the log", me.Error(), leak)
		}
	}
	if !strings.Contains(err.Error(), "google:") {
		t.Errorf("err text = %q, want the package prefix", err.Error())
	}
}
