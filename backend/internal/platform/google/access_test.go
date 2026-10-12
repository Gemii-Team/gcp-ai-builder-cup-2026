package google_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/api/googleapi"

	"kiwi/internal/platform/google"
)

func TestIsAccessDenied(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"consent required", &google.ConsentRequiredError{URL: "/api/auth/meet-consent"}, true},
		{"consent required in a chain", fmt.Errorf("poll: %w", &google.ConsentRequiredError{}), true},
		{"HTTP 401", &googleapi.Error{Code: http.StatusUnauthorized}, true},
		{"HTTP 403", &googleapi.Error{Code: http.StatusForbidden}, true},
		{"HTTP 403 in a chain", fmt.Errorf("google: spaces.create: %w", &googleapi.Error{Code: http.StatusForbidden}), true},
		{"HTTP 404", &googleapi.Error{Code: http.StatusNotFound}, false},
		{"HTTP 429", &googleapi.Error{Code: http.StatusTooManyRequests}, false},
		{"HTTP 500", &googleapi.Error{Code: http.StatusInternalServerError}, false},
		{"plain error", errors.New("connection reset"), false},
		{"cancelled context", context.Canceled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := google.IsAccessDenied(tc.err); got != tc.want {
				t.Errorf("IsAccessDenied(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A Meet 401/403 that came through the real client is still recognised, and is not
// mistaken for a missing grant: the two stay distinct errors.
func TestIsAccessDenied_MeetStatusFromTheClient(t *testing.T) {
	for _, tc := range []struct {
		status       int
		wantDenied   bool
		wantAPICode  int
		wantCategory string
	}{
		{http.StatusUnauthorized, true, 401, "unauthorized"},
		{http.StatusForbidden, true, 403, "forbidden"},
		{http.StatusInternalServerError, false, 500, "server error"},
		{http.StatusNotFound, false, 404, "not found"},
	} {
		t.Run(tc.wantCategory, func(t *testing.T) {
			m := newMeetStub(t)
			user := uuid.New()
			m.mux.HandleFunc("GET /v2/conferenceRecords", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":{"code":`+fmt.Sprint(tc.status)+`,"message":"stub"}}`, tc.status)
			})

			_, err := newClient(t, m, tokenFor(t, user)).ListConferences(t.Context(), user, "spaces/abc123")
			if err == nil {
				t.Fatal("ListConferences succeeded against a failing Meet")
			}

			var api *googleapi.Error
			if !errors.As(err, &api) || api.Code != tc.wantAPICode {
				t.Fatalf("err = %v (%T), want a *googleapi.Error %d kept in the chain", err, err, tc.wantAPICode)
			}
			var consent *google.ConsentRequiredError
			if errors.As(err, &consent) {
				t.Errorf("an HTTP %d from Meet was reported as a missing grant", tc.status)
			}
			if got := google.IsAccessDenied(err); got != tc.wantDenied {
				t.Errorf("IsAccessDenied = %v for HTTP %d, want %v", got, tc.status, tc.wantDenied)
			}
		})
	}
}
