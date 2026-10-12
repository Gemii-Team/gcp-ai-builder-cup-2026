package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/testkit/fakegoogle"
	"kiwi/internal/testkit/fakeoidc"
)

// contractDir holds one JSON file per answer StartMeeting can fail with: the status,
// the content type and the exact body the composed server wrote. The web reads the
// same files (apps/web/src/features/channel/startMeetingContract.test.tsx) and feeds
// each through its own client, so the two sides agree on the bytes without either
// holding a hand-written copy. UPDATE_CONTRACT=1 rewrites the files from the server.
const contractDir = "../../../packages/api-client/testdata/connect-errors"

// contractAnswer is one fixture file.
type contractAnswer struct {
	HTTPStatus  int             `json:"httpStatus"`
	ContentType string          `json:"contentType"`
	Body        json.RawMessage `json:"body"`
}

// answerOf reads what the server wrote. The body must be JSON: every failure of a
// Connect unary call is.
func answerOf(t *testing.T, resp *http.Response) contractAnswer {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		t.Fatalf("the answer is not JSON (%d %q): %v", resp.StatusCode, body, err)
	}
	return contractAnswer{HTTPStatus: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: compact.Bytes()}
}

// startMeeting posts StartMeeting the way the web's transport does (JSON, the
// protocol header) through s, whose cookie and Origin the client adds.
func (s *session) startMeeting(channelID string) contractAnswer {
	s.r.t.Helper()
	return postStartMeeting(s.r, s.http, channelID)
}

func postStartMeeting(r *rig, client *http.Client, channelID string) contractAnswer {
	r.t.Helper()
	return postJSON(r, client, "kiwi.meetings.v1.MeetingService/StartMeeting", fmt.Sprintf(`{"channelId":%q}`, channelID))
}

// postJSON calls the unary RPC procedure the way the web's transport does (JSON, the
// protocol header) through client, and returns what the server wrote.
func postJSON(r *rig, client *http.Client, procedure, body string) contractAnswer {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.URL+"/api/"+procedure, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := client.Do(req)
	if err != nil {
		r.t.Fatalf("%s: %v", procedure, err)
	}
	return answerOf(r.t, resp)
}

// The bytes the composed server writes for every way StartMeeting fails equal the
// committed fixtures the web is tested against. A change on either side that the
// other does not know about fails here (or in the web's test) instead of showing as
// a silent button in the browser.
func TestContract_StartMeetingFailures_MatchTheFixturesTheWebReads(t *testing.T) {
	// Meet's failures come from a fake Google client; the consent answer comes from the
	// real Google client over the real identity token source, which finds no Meet scope
	// in the grant that sign-in stored.
	fake := newRig(t)
	fake.start()
	alice, bob := fake.signIn(fakeoidc.AliceEmail), fake.signIn(fakeoidc.BobEmail)
	channel := alice.newChannel("contract")

	viaIdentity := newRig(t, withOption(WithGoogle(nil)))
	viaIdentity.start()
	realAlice := viaIdentity.signIn(fakeoidc.AliceEmail)
	realChannel := realAlice.newChannel("contract")

	googleText := errors.New("google: spaces.create: googleapi: Error 403: not allowed for ana@example.org in projects/123456")
	meetFails := func(reason, cause string, status int) func() contractAnswer {
		return func() contractAnswer {
			fake.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: errors.Join(googleText, &google.MeetError{Reason: reason, Cause: cause, Status: status})})
			return alice.startMeeting(channel)
		}
	}

	cases := []struct {
		name   string
		answer func() contractAnswer
	}{
		{"consent_required", func() contractAnswer { return realAlice.startMeeting(realChannel) }},
		{"meet_access_access_denied", meetFails(google.ReasonMeetAccess, google.CauseAccessDenied, 401)},
		{"meet_unavailable_api_disabled", meetFails(google.ReasonMeetUnavailable, google.CauseAPIDisabled, 403)},
		{"meet_unavailable_rate_limited", meetFails(google.ReasonMeetUnavailable, google.CauseRateLimited, 429)},
		{"meet_unavailable_unavailable", meetFails(google.ReasonMeetUnavailable, google.CauseUnavailable, 503)},
		{"meet_unavailable_refused", meetFails(google.ReasonMeetUnavailable, google.CauseRefused, 400)},
		{"internal", func() contractAnswer {
			fake.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: googleText}) // an error nothing typed
			return alice.startMeeting(channel)
		}},
		// Membership is checked before anything else, so a channel nobody has is answered like one the person
		// is not in (a private channel's existence is not told): the same bytes as permission_denied.
		{"unknown_channel", func() contractAnswer { return alice.startMeeting(uuid.NewString()) }},
		// not_found is what an errs.ErrNotFound from below the membership check becomes (a channel deleted
		// while the call was running); the fake Google client is the seam that can raise one here.
		{"not_found", func() contractAnswer {
			fake.google.QueueCreateMeetSpace(fakegoogle.SpaceReply{Err: fmt.Errorf("meetings: the channel is gone: %w", errs.ErrNotFound)})
			return alice.startMeeting(channel)
		}},
		{"permission_denied", func() contractAnswer { return bob.startMeeting(channel) }}, // not a member of alice's private channel
		{"unauthenticated", func() contractAnswer {
			return postStartMeeting(fake, &http.Client{Transport: headerTransport{base: http.DefaultTransport, header: http.Header{"Origin": {fake.URL}}}}, channel)
		}},
	}

	names := make([]string, len(cases))
	for i, tc := range cases {
		names[i] = tc.name
		t.Run(tc.name, func(t *testing.T) {
			got := tc.answer()
			if strings.Contains(string(got.Body), "ana@example.org") || strings.Contains(string(got.Body), "projects/123456") {
				t.Errorf("the body %s carries Google's own message", got.Body)
			}
			checkFixture(t, tc.name, got)
		})
	}

	t.Run("every fixture has a case here", func(t *testing.T) { noStrayFixtures(t, contractDir, names) })
}

// noStrayFixtures fails for a fixture of dir that has no case among names; with
// UPDATE_CONTRACT=1 it removes it instead.
func noStrayFixtures(t *testing.T, dir string, names []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		if slices.Contains(names, name) {
			continue
		}
		if os.Getenv("UPDATE_CONTRACT") == "1" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			continue
		}
		t.Errorf("fixture %s has no case in this test: delete it, or add the case (UPDATE_CONTRACT=1 removes it)", file)
	}
}

// checkFixture compares what the server wrote with the committed fixture of name in
// contractDir; with UPDATE_CONTRACT=1 it writes the fixture instead.
func checkFixture(t *testing.T, name string, got contractAnswer) {
	t.Helper()
	checkFixtureIn(t, contractDir, name, got)
}

func checkFixtureIn(t *testing.T, dir, name string, got contractAnswer) {
	t.Helper()
	path := filepath.Join(dir, name+".json")
	if os.Getenv("UPDATE_CONTRACT") == "1" {
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no fixture %s for %q: %v\nrun UPDATE_CONTRACT=1 go test ./internal/app -run TestContract to write it, then review the diff: the web is tested against this file", path, name, err)
	}
	var want contractAnswer
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("fixture %s is not a contract answer: %v", path, err)
	}
	var wantBody bytes.Buffer
	if err := json.Compact(&wantBody, want.Body); err != nil {
		t.Fatalf("fixture %s: body is not JSON: %v", path, err)
	}
	if got.HTTPStatus != want.HTTPStatus || got.ContentType != want.ContentType || !bytes.Equal(got.Body, wantBody.Bytes()) {
		t.Errorf("the server's answer for %q differs from the fixture %s the web is tested against.\n  server:  %d %q %s\n  fixture: %d %q %s\nif the server's answer is meant to change, update the web to match, then run UPDATE_CONTRACT=1 go test ./internal/app -run TestContract",
			name, path, got.HTTPStatus, got.ContentType, got.Body, want.HTTPStatus, want.ContentType, wantBody.Bytes())
	}
}
