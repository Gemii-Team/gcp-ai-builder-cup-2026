package webstatic

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Each case of TestFileName is refused by exactly one of the guards of fileName, so that removing a guard
// fails the cases named after it, and no other guard hides the loss.
func TestFileName(t *testing.T) {
	tests := []struct {
		guard string
		path  string
		ok    bool
		name  string
	}{
		// Served as they are.
		{"", "/", true, ""},
		{"", "/index.html", true, "index.html"},
		{"", "/assets/app-abc123.js", true, "assets/app-abc123.js"},
		{"", "/c/3f2a9c", true, "c/3f2a9c"},
		{"", "/.well-known/x", true, ".well-known/x"},
		{"", "/a..b/c.", true, "a..b/c."},
		{"", "/a b", true, "a b"},
		{"", "/100%", true, "100%"},
		{"", "/a%zz", true, "a%zz"},
		{"", "/trailing/", true, "trailing/"},

		{"a path starts with a slash", "", false, ""},
		{"a path starts with a slash", "index.html", false, ""},
		{"a path starts with a slash", "assets/app.js", false, ""},

		{"no NUL", "/a\x00b", false, ""},
		{"no NUL", "/index.html\x00.png", false, ""},

		{"no backslash", `/a\b`, false, ""},
		{"no backslash", `/..\outside`, false, ""},

		{"no residual escape", "/%2e%2e/x", false, ""},
		{"no residual escape", "/%2E%2E%2Fx", false, ""},
		{"no residual escape", "/..%5cx", false, ""},
		{"no residual escape", "/assets/%41", false, ""},
		{"no residual escape", "/x%00", false, ""},

		{"no empty segment", "//index.html", false, ""},
		{"no empty segment", "/assets//app-abc123.js", false, ""},
		{"no empty segment", "/a///b", false, ""},

		{"no dot segment", "/..", false, ""},
		{"no dot segment", "/../outside/secret.txt", false, ""},
		{"no dot segment", "/assets/../index.html", false, ""},
		{"no dot segment", "/assets/..", false, ""},
		{"no dot segment", "/./index.html", false, ""},
		{"no dot segment", "/assets/.", false, ""},
	}
	for _, tc := range tests {
		name, ok := fileName(tc.path)
		if ok != tc.ok || name != tc.name {
			t.Errorf("fileName(%q) = %q, %v; want %q, %v (guard: %s)", tc.path, name, ok, tc.name, tc.ok, tc.guard)
		}
	}
}

// traversals are targets that must not give a file of the fixture. The handler is called directly, so
// that nothing but its own checks stands between the target and the files.
var traversals = []struct {
	name   string
	target string
}{
	// Out of the directory.
	{"dot-dot", "/../outside/secret.txt"},
	{"dot-dot below a directory", "/assets/../../outside/secret.txt"},
	{"encoded dot-dot", "/%2e%2e/outside/secret.txt"},
	{"encoded dot-dot, upper case", "/%2E%2E/outside/secret.txt"},
	{"encoded dot-dot and slash", "/%2E%2E%2Foutside/secret.txt"},
	{"dot-dot and encoded backslash", "/..%5coutside/secret.txt"},
	{"double-encoded dot-dot", "/%252e%252e/outside/secret.txt"},
	{"double-encoded dot-dot and slash", "/%252e%252e%252foutside/secret.txt"},
	{"dot-dot and double-encoded backslash", "/..%255coutside/secret.txt"},
	{"encoded backslash", "/%5c..%5coutside%5csecret.txt"},
	{"NUL", "/outside/secret.txt%00.png"},
	// Inside the directory, by a path that is not that of the file.
	{"dot-dot that stays inside", "/assets/../index.html"},
	{"encoded dot-dot that stays inside", "/assets/%2e%2e/index.html"},
	{"encoded dot-dot and slash that stay inside", "/assets/%2E%2E%2Findex.html"},
	{"dot segment", "/./index.html"},
	{"dot segment below a directory", "/assets/./app-abc123.js"},
	{"empty segment", "/assets//app-abc123.js"},
	{"empty first segment", "//index.html"},
	{"a file with a backslash in its name", "/a%5Cb.txt"},
	{"a file with a percent in its name, double-encoded", "/%2541.txt"},
	{"NUL in a navigation", "/c/a%00b"},
	{"backslash in a navigation", "/c/a%5Cb"},
	{"NUL before an extension", "/index.html%00.png"},
}

// Rule 8, the handler alone.
func TestTraversal_Handler(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	for _, tc := range traversals {
		t.Run(tc.name, func(t *testing.T) {
			res := get(t, h, tc.target)
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("GET %s: status %d, want 400", tc.target, res.StatusCode)
			}
			for _, leak := range []string{secret, shell, "FILE-WITH-A", "console.log"} {
				if strings.Contains(res.body, leak) {
					t.Errorf("GET %s: the body holds %q", tc.target, leak)
				}
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s: Cache-Control = %q, want no-store", tc.target, got)
			}
			wantSecurityHeaders(t, tc.target, res.Response)
		})
	}

	t.Run("a path that does not start with a slash", func(t *testing.T) {
		for _, p := range []string{"", "index.html", "assets/app-abc123.js", "../outside/secret.txt"} {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.URL.Path = p
			res := serveRequest(t, h, req)
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("path %q: status %d, want 400", p, res.StatusCode)
			}
			wantSecurityHeaders(t, "path "+p, res.Response)
		}
	})
}

// A symlink that leaves the web directory is no file of it, whether it names a file or a directory,
// by an absolute or a relative target. A link that stays inside is as good as the file.
func TestTraversal_SymlinksOutOfTheRoot(t *testing.T) {
	s := newSite(t)
	s.symlink(t, s.outside, "abs.txt")
	s.symlink(t, "../outside/secret.txt", "rel.txt")
	s.symlink(t, "../outside", "linkdir")
	s.symlink(t, "../../outside/secret.txt", "assets/leak.js")
	s.symlink(t, "../../outside", "assets/leakdir")
	s.symlink(t, "../outside/secret.txt", "board") // an SPA route, shadowed by a link out
	s.symlink(t, "index.html", "inside.txt")
	h := s.handler(t)

	for _, target := range []string{
		"/abs.txt", "/rel.txt", "/linkdir/secret.txt", "/linkdir", "/assets/leak.js", "/assets/leakdir/secret.txt",
	} {
		wantNotFound(t, "GET "+target, get(t, h, target))
	}

	// The shell answers /board because there is no file of that name to serve: never the secret.
	if res := get(t, h, "/board"); res.StatusCode != http.StatusOK || res.body != shell {
		t.Errorf("GET /board = %d %q, want the shell and not the file the link points at", res.StatusCode, res.body)
	}

	if res := get(t, h, "/inside.txt"); res.StatusCode != http.StatusOK || res.body != shell {
		t.Errorf("GET /inside.txt = %d %q, want the file the link points at, inside the directory", res.StatusCode, res.body)
	}
}

// rawGet sends one GET with the target byte for byte, which http.Client would normalise, and
// returns the response with its body read.
func rawGet(t *testing.T, addr, target string) (*http.Response, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	noErr(t, err)
	defer func() { _ = conn.Close() }()
	noErr(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: kiwi.test\r\nConnection: close\r\n\r\n", target)
	noErr(t, err)
	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	noErr(t, err)
	body, err := io.ReadAll(res.Body)
	noErr(t, err)
	noErr(t, res.Body.Close())
	return res, string(body)
}

// follow requests target and follows redirects by hand, as a browser would, and returns every response.
func follow(t *testing.T, addr, target string) (statuses []int, last *http.Response, bodies []string) {
	t.Helper()
	for range 4 {
		res, body := rawGet(t, addr, target)
		statuses = append(statuses, res.StatusCode)
		bodies = append(bodies, body)
		last = res
		loc := res.Header.Get("Location")
		if res.StatusCode < 300 || res.StatusCode > 399 || loc == "" {
			return statuses, last, bodies
		}
		u, err := url.Parse(loc)
		noErr(t, err)
		target = u.RequestURI()
	}
	t.Fatalf("GET %s: more than 4 redirects: %v", target, statuses)
	return nil, nil, nil
}

// Rule 8, mounted the way the process mounts it: the handler is the "/" fallback of a ServeMux. The mux cleans
// and redirects the paths it can see are not canonical, but it judges the escaped path, so an encoded dot segment
// (%2e%2e) reaches the handler as a decoded ".." and only the handler stands between it and the file system.
// Whatever happens, no request ends on a file outside the directory.
func TestTraversal_ThroughServeMux(t *testing.T) {
	s := newSite(t)
	s.symlink(t, s.outside, "abs.txt")
	h := s.handler(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "API") })
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	tests := []struct {
		target string
		final  int
		body   string // "" = anything but a file of the fixture
	}{
		// The mux cleans these into paths of the directory: a redirect to the clean path, which has no such file.
		{"/../outside/secret.txt", http.StatusNotFound, ""},
		{"/assets/../../outside/secret.txt", http.StatusNotFound, ""},
		{"//outside/secret.txt", http.StatusNotFound, ""},
		// The mux cleans these into a path of a file that is there: the file, by its own path.
		{"/assets/../index.html", http.StatusOK, shell},
		{"/./index.html", http.StatusOK, shell},
		{"//index.html", http.StatusOK, shell},
		{"/assets//app-abc123.js", http.StatusOK, siteFiles["assets/app-abc123.js"]},
		// The mux sees one odd segment and passes the path on, decoded; the handler refuses it.
		{"/%2e%2e/outside/secret.txt", http.StatusBadRequest, ""},
		{"/%2E%2E%2Foutside/secret.txt", http.StatusBadRequest, ""},
		{"/assets/%2e%2e/%2e%2e/outside/secret.txt", http.StatusBadRequest, ""},
		{"/assets/%2e%2e/index.html", http.StatusBadRequest, ""},
		{"/assets/%2E%2E%2Findex.html", http.StatusBadRequest, ""},
		{"/assets/%2e/app-abc123.js", http.StatusBadRequest, ""},
		{"/assets%2F%2Fapp-abc123.js", http.StatusBadRequest, ""},
		{"/assets/%2F/app-abc123.js", http.StatusBadRequest, ""},
		// The mux leaves these alone, and the handler refuses them.
		{"/..%5coutside/secret.txt", http.StatusBadRequest, ""},
		{"/%5c..%5coutside%5csecret.txt", http.StatusBadRequest, ""},
		{"/%252e%252e/outside/secret.txt", http.StatusBadRequest, ""},
		{"/%252e%252e%252foutside/secret.txt", http.StatusBadRequest, ""},
		{"/..%255coutside/secret.txt", http.StatusBadRequest, ""},
		{"/a%5Cb.txt", http.StatusBadRequest, ""},
		{"/%2541.txt", http.StatusBadRequest, ""},
		{"/c/a%00b", http.StatusBadRequest, ""},
		{"/outside/secret.txt%00.png", http.StatusBadRequest, ""},
		// A link that leaves the directory.
		{"/abs.txt", http.StatusNotFound, ""},
		// The routes of the mux keep winning: the handler is the fallback.
		{"/api/ping", http.StatusOK, "API"},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			statuses, last, bodies := follow(t, addr, tc.target)
			if got := statuses[len(statuses)-1]; got != tc.final {
				t.Errorf("GET %s: statuses %v, want it to end on %d", tc.target, statuses, tc.final)
			}
			for i, body := range bodies {
				if strings.Contains(body, secret) {
					t.Errorf("GET %s: response %d holds the secret", tc.target, i)
				}
			}
			if tc.body != "" && bodies[len(bodies)-1] != tc.body {
				t.Errorf("GET %s: body %q, want %q", tc.target, bodies[len(bodies)-1], tc.body)
			}
			if tc.final == http.StatusBadRequest || tc.final == http.StatusNotFound {
				wantSecurityHeaders(t, "GET "+tc.target, last)
			}
		})
	}

	// The method policy of the handler holds behind the mux, with its headers.
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader("x"))
	noErr(t, err)
	res, err := http.DefaultClient.Do(req)
	noErr(t, err)
	noErr(t, res.Body.Close())
	if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /: %d, Allow %q", res.StatusCode, res.Header.Get("Allow"))
	}
	wantSecurityHeaders(t, "POST /", res)
}
