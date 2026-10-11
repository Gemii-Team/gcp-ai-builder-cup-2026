package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireSameOrigin_RejectsCrossOriginPOST_AllowsSameOrigin(t *testing.T) {
	const public = "https://kiwi.example.com/some/path?x=1"

	tests := []struct {
		name    string
		method  string
		origin  string // "" = header absent
		allowed bool
	}{
		{"same-origin POST", http.MethodPost, "https://kiwi.example.com", true},
		{"same-origin POST, host in upper case", http.MethodPost, "https://KIWI.example.com", true},
		{"cross-origin POST", http.MethodPost, "https://evil.example", false},
		{"cross-origin PUT", http.MethodPut, "https://evil.example", false},
		{"cross-origin PATCH", http.MethodPatch, "https://evil.example", false},
		{"cross-origin DELETE", http.MethodDelete, "https://evil.example", false},
		{"POST from a sibling subdomain", http.MethodPost, "https://app.kiwi.example.com", false},
		{"POST from the same host over http", http.MethodPost, "http://kiwi.example.com", false},
		{"POST from the same host on another port", http.MethodPost, "https://kiwi.example.com:8443", false},
		{"POST with the opaque origin", http.MethodPost, "null", false},
		{"POST with an unparsable origin", http.MethodPost, "https://kiwi.example.com\n@evil.example", false},
		{"POST without an origin", http.MethodPost, "", false},
		{"GET without an origin", http.MethodGet, "", true},
		{"cross-origin GET", http.MethodGet, "https://evil.example", true},
		{"cross-origin HEAD", http.MethodHead, "https://evil.example", true},
		{"cross-origin OPTIONS", http.MethodOptions, "https://evil.example", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := RequireSameOrigin(public)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(tc.method, "/api/anything", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tc.allowed {
				if !called || rec.Code != http.StatusNoContent {
					t.Fatalf("status %d, handler called = %v; want the request to pass through", rec.Code, called)
				}
				return
			}
			if called {
				t.Fatal("the wrapped handler ran for a request that must be rejected")
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
		})
	}
}

func TestRequireSameOrigin_RejectionDoesNotEchoTheOrigin(t *testing.T) {
	h := RequireSameOrigin("https://kiwi.example.com")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Origin", "https://<script>alert(1)</script>.evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "script") {
		t.Fatalf("response body reflects the Origin header: %q", rec.Body.String())
	}
}

func TestRequireSameOrigin_PanicsOnAPublicURLWithoutAnOrigin(t *testing.T) {
	for _, public := range []string{"", "kiwi.example.com", "/relative", "ftp://kiwi.example.com", "https://"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RequireSameOrigin(%q) did not panic", public)
				}
			}()
			RequireSameOrigin(public)
		}()
	}
}
