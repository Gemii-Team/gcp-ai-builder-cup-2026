package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"kiwi/internal/platform/google"
)

const calendarScope = "https://www.googleapis.com/auth/calendar.events"

// calTokenFor serves a static token for want and fails the test when the client asks
// for another user or a scope other than the calendar one.
func calTokenFor(t *testing.T, want uuid.UUID) google.TokenFunc {
	t.Helper()
	return func(_ context.Context, userID uuid.UUID, scope string) (oauth2.TokenSource, error) {
		if userID != want {
			t.Errorf("token requested for user %s, want %s", userID, want)
		}
		if scope != calendarScope {
			t.Errorf("token requested for scope %q, want %q", scope, calendarScope)
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok"}), nil
	}
}

func newCalendarClient(t *testing.T, base string) google.Client {
	t.Helper()
	c, err := google.New(calTokenFor(t, calUser), google.Options{CalendarBaseURL: base, HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	return c
}

var calUser = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func TestScheduleCalendarEvent_AttachesMeetAndReturnsTheLink(t *testing.T) {
	var gotPath, gotConfVersion string
	var gotBody struct {
		Summary        string                    `json:"summary"`
		Start          struct{ DateTime string } `json:"start"`
		End            struct{ DateTime string } `json:"end"`
		ConferenceData struct {
			CreateRequest struct {
				RequestId             string
				ConferenceSolutionKey struct{ Type string }
			}
		} `json:"conferenceData"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotConfVersion = r.URL.Query().Get("conferenceDataVersion")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "ev123",
			"htmlLink":    "https://calendar.google.com/event?eid=ev123",
			"hangoutLink": "https://meet.google.com/abc-defg-hij",
		})
	}))
	t.Cleanup(srv.Close)

	ev, err := newCalendarClient(t, srv.URL).ScheduleCalendarEvent(t.Context(), calUser, google.CalendarEventInput{
		Title:   "Weekly sync",
		StartAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		EndAt:   time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ScheduleCalendarEvent: %v", err)
	}

	if ev.ID != "ev123" || ev.MeetURI != "https://meet.google.com/abc-defg-hij" || ev.HTMLLink == "" {
		t.Errorf("event = %+v, want id ev123 with the Meet and html links", ev)
	}
	if gotPath != "/calendars/primary/events" {
		t.Errorf("insert path = %q, want /calendars/primary/events", gotPath)
	}
	if gotConfVersion != "1" {
		t.Errorf("conferenceDataVersion = %q, want 1 (so Google mints the Meet link)", gotConfVersion)
	}
	if gotBody.Summary != "Weekly sync" || gotBody.Start.DateTime != "2026-10-20T09:00:00Z" || gotBody.End.DateTime != "2026-10-20T09:30:00Z" {
		t.Errorf("request body = %+v, want the title and the UTC RFC3339 span", gotBody)
	}
	if gotBody.ConferenceData.CreateRequest.ConferenceSolutionKey.Type != "hangoutsMeet" || gotBody.ConferenceData.CreateRequest.RequestId == "" {
		t.Errorf("conference request = %+v, want a hangoutsMeet create request with an id", gotBody.ConferenceData.CreateRequest)
	}
}

func TestCancelCalendarEvent_TreatsAGoneEventAsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound, http.StatusGone} {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(status)
		}))

		err := newCalendarClient(t, srv.URL).CancelCalendarEvent(t.Context(), calUser, "ev123")
		srv.Close()
		if err != nil {
			t.Errorf("CancelCalendarEvent on HTTP %d: %v, want nil", status, err)
		}
		if gotMethod != http.MethodDelete || gotPath != "/calendars/primary/events/ev123" {
			t.Errorf("on HTTP %d: %s %s, want DELETE /calendars/primary/events/ev123", status, gotMethod, gotPath)
		}
	}
}
