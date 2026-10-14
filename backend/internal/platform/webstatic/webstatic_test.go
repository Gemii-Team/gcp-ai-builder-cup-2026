package webstatic

import (
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// shell is the body of the fixture's index.html: the app shell.
	shell = "<!doctype html><title>kiwi shell</title>"
	// secret is the content of a file beside the web directory, which no request may ever return.
	secret = "TOP-SECRET-OUTSIDE-THE-WEB-DIR"
)

var fixtureTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// siteFiles is the web directory of the fixture: what a Vite build holds, and a few files named
// to trip the policy (a file called "api", one with a backslash or a "%41" in its name).
var siteFiles = map[string]string{
	"index.html":                 shell,
	"sw.js":                      "self.skipWaiting();",
	"registerSW.js":              "navigator.serviceWorker.register('/sw.js');",
	"workbox-abc123.js":          "define(['./workbox'], () => {});",
	"manifest.webmanifest":       `{"name":"K.I.W.I."}`,
	"assets/app-abc123.js":       "console.log('app');",
	"assets/app-abc123.css":      "body{margin:0}",
	"assets/fonts/anuphan.woff2": "wOF2-bytes",
	"assets-old/app.js":          "console.log('old');",
	"icons/icon-192.png":         "\x89PNG\r\n\x1a\nicon",
	"brand/kiwi-mark.svg":        `<svg xmlns="http://www.w3.org/2000/svg"/>`,
	"api":                        "FILE-NAMED-API",
	"apiary":                     "FILE-NAMED-APIARY",
	"healthz":                    "FILE-NAMED-HEALTHZ",
	"readyz":                     "FILE-NAMED-READYZ",
	"ws/hello.txt":               "FILE-UNDER-WS",
	"wsx":                        "FILE-NAMED-WSX",
	"mockoidc/authorize":         "FILE-UNDER-MOCKOIDC",
	`a\b.txt`:                    "FILE-WITH-A-BACKSLASH",
	"%41.txt":                    "FILE-WITH-A-PERCENT",
}

// site is a temp tree built through os.Root, so building it cannot touch anything outside it:
//
//	<parent>/web/...            the web directory (siteFiles)
//	<parent>/outside/secret.txt what must stay out of reach
type site struct {
	parent  *os.Root
	web     *os.Root
	dir     string // absolute path of <parent>/web
	outside string // absolute path of <parent>/outside/secret.txt
}

func newSite(t *testing.T) *site {
	t.Helper()
	parentDir := t.TempDir()
	parent, err := os.OpenRoot(parentDir)
	noErr(t, err)
	t.Cleanup(func() { _ = parent.Close() })

	noErr(t, parent.Mkdir("web", 0o755))
	noErr(t, parent.MkdirAll("outside", 0o755))
	noErr(t, parent.WriteFile("outside/secret.txt", []byte(secret), 0o644))
	web, err := parent.OpenRoot("web")
	noErr(t, err)
	t.Cleanup(func() { _ = web.Close() })

	s := &site{
		parent:  parent,
		web:     web,
		dir:     filepath.Join(parentDir, "web"),
		outside: filepath.Join(parentDir, "outside", "secret.txt"),
	}
	for name, body := range siteFiles {
		s.put(t, name, body)
	}
	return s
}

// put writes a file of the web directory, with a fixed mtime so that Last-Modified and the ETag
// are the same on every run.
func (s *site) put(t *testing.T, name, body string) {
	t.Helper()
	if dir := path.Dir(name); dir != "." {
		noErr(t, s.web.MkdirAll(dir, 0o755))
	}
	noErr(t, s.web.WriteFile(name, []byte(body), 0o644))
	noErr(t, s.web.Chtimes(name, fixtureTime, fixtureTime))
}

// symlink makes name in the web directory a link to target, which may point anywhere.
func (s *site) symlink(t *testing.T, target, name string) {
	t.Helper()
	if dir := path.Dir(name); dir != "." {
		noErr(t, s.web.MkdirAll(dir, 0o755))
	}
	noErr(t, s.web.Symlink(target, name))
}

func (s *site) handler(t *testing.T) http.Handler {
	t.Helper()
	h, err := New(s.dir)
	noErr(t, err)
	return h
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// reply is a response with its body read.
type reply struct {
	*http.Response
	body string
}

func do(t *testing.T, h http.Handler, method, target string, header ...string) reply {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return serveRequest(t, h, req)
}

func serveRequest(t *testing.T, h http.Handler, req *http.Request) reply {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	b, err := io.ReadAll(res.Body)
	noErr(t, err)
	noErr(t, res.Body.Close())
	return reply{Response: res, body: string(b)}
}

func get(t *testing.T, h http.Handler, target string, header ...string) reply {
	t.Helper()
	return do(t, h, http.MethodGet, target, header...)
}

// wantSecurityHeaders fails unless res carries the three headers every answer of the handler has.
func wantSecurityHeaders(t *testing.T, what string, res *http.Response) {
	t.Helper()
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := res.Header.Get(k); got != want {
			t.Errorf("%s: %s = %q, want %q", what, k, got, want)
		}
	}
}

// wantNotFound fails unless res is a 404 that nobody may store, with no validator and none of the files' content.
func wantNotFound(t *testing.T, what string, res reply, forbidden ...string) {
	t.Helper()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("%s: status %d, want 404 (body %q)", what, res.StatusCode, res.body)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
	}
	if res.Header.Get("ETag") != "" || res.Header.Get("Last-Modified") != "" {
		t.Errorf("%s: a 404 carries a validator: ETag %q, Last-Modified %q", what, res.Header.Get("ETag"), res.Header.Get("Last-Modified"))
	}
	for _, f := range append(forbidden, shell, secret) {
		if strings.Contains(res.body, f) {
			t.Errorf("%s: the 404 body holds %q", what, f)
		}
	}
	wantSecurityHeaders(t, what, res.Response)
}

func TestNew_RejectsADirectoryItCannotServe(t *testing.T) {
	s := newSite(t)
	noErr(t, s.parent.MkdirAll("empty", 0o755))
	noErr(t, s.parent.MkdirAll("indexdir/index.html", 0o755))
	noErr(t, s.parent.MkdirAll("linked", 0o755))
	noErr(t, s.parent.Symlink("../outside/secret.txt", "linked/index.html"))
	parentDir := filepath.Dir(s.dir)

	cwd, err := os.Getwd()
	noErr(t, err)
	// A valid directory, written as a path that is not absolute.
	rel, err := filepath.Rel(cwd, s.dir)
	noErr(t, err)

	tests := []struct {
		name string
		dir  string
	}{
		{"a relative path to a valid directory", rel},
		{"a relative directory name", "web"},
		{"an empty path", ""},
		{"a directory that does not exist", filepath.Join(parentDir, "missing")},
		{"a file", filepath.Join(s.dir, "index.html")},
		{"a directory without index.html", filepath.Join(parentDir, "empty")},
		{"a directory whose index.html is a directory", filepath.Join(parentDir, "indexdir")},
		{"a directory whose index.html links out of it", filepath.Join(parentDir, "linked")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(tc.dir)
			if err == nil {
				t.Fatalf("New(%q) = handler, nil; want an error", tc.dir)
			}
			if h != nil {
				t.Errorf("New(%q) returned a handler next to its error", tc.dir)
			}
			if !strings.HasPrefix(err.Error(), "webstatic: ") {
				t.Errorf("error %q does not say where it comes from", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q leaks file content", err)
			}
		})
	}
}

func TestNew_AcceptsADirectoryBehindASymlinkOrAnUncleanPath(t *testing.T) {
	// A "current -> releases/42" deployment, and a trailing slash, are ordinary.
	s := newSite(t)
	parentDir := filepath.Dir(s.dir)
	noErr(t, s.parent.Symlink("web", "current"))

	for _, dir := range []string{
		filepath.Join(parentDir, "current"),
		s.dir + "/",
		s.dir + "/../web",
	} {
		h, err := New(dir)
		if err != nil {
			t.Errorf("New(%q): %v", dir, err)
			continue
		}
		if res := get(t, h, "/"); res.StatusCode != http.StatusOK || res.body != shell {
			t.Errorf("New(%q) serves / as %d %q", dir, res.StatusCode, res.body)
		}
	}
}

// Rule 1.
func TestMethods_OnlyGetAndHeadAreServed(t *testing.T) {
	h := newSite(t).handler(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodTrace} {
		for _, target := range []string{"/", "/index.html", "/assets/app-abc123.js", "/signin", "/nope"} {
			what := method + " " + target
			res := do(t, h, method, target)
			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s: status %d, want 405", what, res.StatusCode)
			}
			if got := res.Header.Get("Allow"); got != "GET, HEAD" {
				t.Errorf("%s: Allow = %q, want %q", what, got, "GET, HEAD")
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
			}
			if strings.Contains(res.body, shell) || strings.Contains(res.body, "console.log") {
				t.Errorf("%s: the 405 carries file content: %q", what, res.body)
			}
			wantSecurityHeaders(t, what, res.Response)
		}
	}

	for _, target := range []string{"/", "/index.html", "/signin", "/assets/app-abc123.js"} {
		g := get(t, h, target)
		if g.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", target, g.StatusCode)
		}
		hd := do(t, h, http.MethodHead, target)
		if hd.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s: status %d, want 200", target, hd.StatusCode)
		}
		if hd.body != "" {
			t.Errorf("HEAD %s: body %q, want none", target, hd.body)
		}
		if got, want := hd.Header.Get("Content-Length"), strconv.Itoa(len(g.body)); got != want {
			t.Errorf("HEAD %s: Content-Length = %q, want %q", target, got, want)
		}
		for _, k := range []string{"Content-Type", "Cache-Control", "ETag"} {
			if hd.Header.Get(k) != g.Header.Get(k) {
				t.Errorf("HEAD %s: %s = %q, GET has %q", target, k, hd.Header.Get(k), g.Header.Get(k))
			}
		}
	}
}

// Rule 2: the shell answers the app's own navigations, and no other path that is not a file.
func TestSPAFallback_OnlyTheAppsNavigations(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{
		"/", "/signin", "/signin?error=access_denied", "/c/3f2a9c", "/c/abc%20def", "/c/3f2a9c?x=1",
		"/board", "/board?view=all", "/ask", "/ask?q=hello",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			what := method + " " + target
			res := do(t, h, method, target)
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d, want the shell", what, res.StatusCode)
				continue
			}
			if method == http.MethodGet && res.body != shell {
				t.Errorf("%s: body %q, want the shell", what, res.body)
			}
			if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("%s: Content-Type = %q", what, got)
			}
			if got := res.Header.Get("Cache-Control"); got != "no-cache" {
				t.Errorf("%s: Cache-Control = %q, want no-cache", what, got)
			}
			wantSecurityHeaders(t, what, res.Response)
		}
	}

	for _, target := range []string{
		"/c", "/c/", "/c/a/b", "/signin/", "/board/", "/ask/more", "/askme", "/Signin", "/signin.html",
		"/unknown", "/settings", "/favicon.ico", "/robots.txt", "/index.htm", "/sw.js.map", "/workbox-missing.js",
		"/assets", "/assets/missing-abc.js", "/assets/signin", "/icons", "/icons/missing.png",
		"/api/auth/login", "/mockoidc/authorize",
	} {
		wantNotFound(t, "GET "+target, get(t, h, target))
	}
}

// A file named like a navigation is a file, and the shell does not shadow it.
func TestSPAFallback_AFileWinsOverTheShell(t *testing.T) {
	s := newSite(t)
	s.put(t, "board", "A-FILE-NAMED-BOARD")
	h := s.handler(t)

	res := get(t, h, "/board")
	if res.body != "A-FILE-NAMED-BOARD" {
		t.Errorf("GET /board = %q, want the file", res.body)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

// A navigation whose name is a directory is not a file: the shell answers it, and nothing lists.
func TestSPAFallback_ADirectoryOfTheNameIsNotAFile(t *testing.T) {
	s := newSite(t)
	s.put(t, "ask/inner.txt", "INSIDE-ASK")
	h := s.handler(t)

	if res := get(t, h, "/ask"); res.StatusCode != http.StatusOK || res.body != shell {
		t.Errorf("GET /ask = %d %q, want the shell", res.StatusCode, res.body)
	}
	if res := get(t, h, "/ask/inner.txt"); res.body != "INSIDE-ASK" {
		t.Errorf("GET /ask/inner.txt = %q, want the file", res.body)
	}
}

// Rule 3.
func TestAssets_ImmutableWhenPresent_NotFoundAndUncachedWhenMissing(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{"/assets/app-abc123.js", "/assets/app-abc123.css", "/assets/fonts/anuphan.woff2", "/assets/app-abc123.js?v=2"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			what := method + " " + target
			res := do(t, h, method, target)
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d", what, res.StatusCode)
			}
			if got, want := res.Header.Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
				t.Errorf("%s: Cache-Control = %q, want %q", what, got, want)
			}
			wantSecurityHeaders(t, what, res.Response)
		}
	}

	for _, target := range []string{
		"/assets/missing-abc.js", "/assets/app-abc124.js", "/assets/fonts/missing.woff2", "/assets/x/y/z.css",
		"/assets/signin", "/assets/index.html",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			res := do(t, h, method, target)
			if method == http.MethodHead {
				res.body = "" // the recorder keeps what a server drops
			}
			wantNotFound(t, method+" "+target, res)
		}
	}

	// The prefix is a directory: its look-alikes are not under it.
	if got := get(t, h, "/assets-old/app.js").Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("/assets-old/app.js: Cache-Control = %q, want no-cache", got)
	}
}

// Rule 4.
func TestCacheControl_EverythingElseRevalidates(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{
		"/", "/index.html", "/sw.js", "/registerSW.js", "/workbox-abc123.js", "/manifest.webmanifest",
		"/icons/icon-192.png", "/brand/kiwi-mark.svg", "/apiary", "/assets-old/app.js",
		"/signin", "/c/abc", "/board", "/ask",
	} {
		res := get(t, h, target)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", target, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", target, got)
		}
	}
}

// Rule 5: the three headers are on every answer, whatever its status.
func TestSecurityHeaders_OnEveryResponse(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	tests := []struct {
		name   string
		method string
		target string
		header []string
		status int
	}{
		{"file", "GET", "/sw.js", nil, 200},
		{"asset", "GET", "/assets/app-abc123.js", nil, 200},
		{"shell", "GET", "/signin", nil, 200},
		{"HEAD", "HEAD", "/sw.js", nil, 200},
		{"range", "GET", "/assets/app-abc123.js", []string{"Range", "bytes=0-3"}, 206},
		{"not modified", "GET", "/sw.js", []string{"If-None-Match", "*"}, 304},
		{"missing file", "GET", "/missing.js", nil, 404},
		{"missing asset", "GET", "/assets/missing.js", nil, 404},
		{"reserved prefix", "GET", "/api/anything", nil, 404},
		{"directory", "GET", "/assets/", nil, 404},
		{"traversal", "GET", "/assets/../index.html", nil, 400},
		{"double encoding", "GET", "/%252e%252e/x", nil, 400},
		{"method", "POST", "/", nil, 405},
		{"range not satisfiable", "GET", "/sw.js", []string{"Range", "bytes=9999-"}, 416},
		{"precondition failed", "GET", "/sw.js", []string{"If-Match", `"nothing"`}, 412},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, h, tc.method, tc.target, tc.header...)
			if res.StatusCode != tc.status {
				t.Errorf("status %d, want %d", res.StatusCode, tc.status)
			}
			wantSecurityHeaders(t, tc.name, res.Response)
		})
	}

	t.Run("directory gone", func(t *testing.T) {
		noErr(t, s.parent.RemoveAll("web"))
		res := get(t, h, "/")
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", res.StatusCode)
		}
		wantSecurityHeaders(t, "directory gone", res.Response)
	})
}

// Rule 6.
func TestContentTypes(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	pdf := mime.TypeByExtension(".pdf")
	if pdf == "" {
		t.Skip("this Go has no builtin type for .pdf, so the fallback to the mime package cannot be shown")
	}

	tests := []struct{ file, want string }{
		{"x.html", "text/html; charset=utf-8"},
		{"x.js", "text/javascript; charset=utf-8"},
		{"x.mjs", "text/javascript; charset=utf-8"},
		{"x.css", "text/css; charset=utf-8"},
		{"x.txt", "text/plain; charset=utf-8"},
		{"x.json", "application/json"},
		{"x.map", "application/json"},
		{"x.webmanifest", "application/manifest+json"},
		{"x.wasm", "application/wasm"},
		{"x.svg", "image/svg+xml"},
		{"x.png", "image/png"},
		{"x.jpg", "image/jpeg"},
		{"x.jpeg", "image/jpeg"},
		{"x.webp", "image/webp"},
		{"x.avif", "image/avif"},
		{"x.gif", "image/gif"},
		{"x.ico", "image/vnd.microsoft.icon"},
		{"x.woff2", "font/woff2"},
		{"x.woff", "font/woff"},
		{"x.ttf", "font/ttf"},
		// The extension is compared in lower case. The first two are not saved by the mime package, which
		// does not know them.
		{"UPPER.WOFF2", "font/woff2"},
		{"Mixed.WebManifest", "application/manifest+json"},
		{"UPPER.JS", "text/javascript; charset=utf-8"},
		{"dotted.name.woff2", "font/woff2"},
		// Not in the table: what the mime package knows, then the generic type. HTML content never makes a
		// type of its own (the type is set, so net/http does not sniff).
		{"x.pdf", pdf},
		{"x.noSuchExt", "application/octet-stream"},
		{"noext", "application/octet-stream"},
	}
	for _, tc := range tests {
		s.put(t, "types/"+tc.file, "<!doctype html><html><script>alert(1)</script></html>")
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			res := get(t, h, "/types/"+tc.file)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d", res.StatusCode)
			}
			if got := res.Header.Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
		})
	}

	if got := get(t, h, "/missing.html").Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("a 404 is %q, want plain text", got)
	}
}

// A host whose mime database says otherwise (a mime.types of another distribution) does not change the type of
// what the build emits. The registration is process-wide and stays for the rest of the run, which is harmless:
// the answers of these extensions never come from the mime package.
func TestContentTypes_TableBeatsTheHostMimeDatabase(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	for ext, want := range contentTypes {
		noErr(t, mime.AddExtensionType(ext, "application/x-host-says-otherwise"))
		file := "host/file" + strings.ToUpper(ext)
		s.put(t, file, "x")
		if got := get(t, h, "/"+file).Header.Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", ext, got, want)
		}
	}
}

// Rule 7.
func TestReservedPrefixes_AreNeverServed(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{
		"/api", "/api/", "/api/auth/login", "/api/x.js",
		"/ws", "/ws/", "/ws/hello.txt",
		"/healthz", "/healthz/", "/readyz", "/readyz/x",
		"/mockoidc", "/mockoidc/authorize", "/mockoidc/authorize?client_id=x",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			res := do(t, h, method, target)
			if method == http.MethodHead {
				res.body = ""
			}
			wantNotFound(t, method+" "+target, res, "FILE-")
		}
	}

	// A prefix is a path segment, not a string prefix.
	for target, want := range map[string]string{
		"/apiary": "FILE-NAMED-APIARY",
		"/wsx":    "FILE-NAMED-WSX",
	} {
		if res := get(t, h, target); res.StatusCode != http.StatusOK || res.body != want {
			t.Errorf("GET %s = %d %q, want the file %q", target, res.StatusCode, res.body, want)
		}
	}
}

// A directory is not listed and does not redirect to a slash.
func TestDirectories_AreNotFound(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{"/assets/", "/assets", "/assets/fonts", "/assets/fonts/", "/icons", "/icons/", "/brand", "/brand/", "/index.html/", "/sw.js/"} {
		res := get(t, h, target)
		wantNotFound(t, "GET "+target, res, "app-abc123", "icon-192", "kiwi-mark")
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("GET %s redirects to %q", target, loc)
		}
	}
}

var etagForm = regexp.MustCompile(`^W/"[0-9a-f]+-[0-9a-f]+"$`)

// Rule 9.
func TestConditionalRequests(t *testing.T) {
	h := newSite(t).handler(t)

	for _, target := range []string{"/index.html", "/sw.js", "/manifest.webmanifest", "/signin", "/c/abc", "/assets/app-abc123.js"} {
		t.Run(target, func(t *testing.T) {
			first := get(t, h, target)
			etag := first.Header.Get("ETag")
			if !etagForm.MatchString(etag) {
				t.Fatalf("ETag = %q, want a weak validator", etag)
			}
			lastModified := first.Header.Get("Last-Modified")
			if lastModified == "" {
				t.Fatal("no Last-Modified")
			}

			for _, cond := range [][]string{
				{"If-None-Match", etag},
				{"If-None-Match", `"other", ` + etag},
				{"If-None-Match", "*"},
				{"If-Modified-Since", lastModified},
			} {
				res := get(t, h, target, cond...)
				if res.StatusCode != http.StatusNotModified {
					t.Errorf("%s: status %d, want 304", cond[0], res.StatusCode)
				}
				if res.body != "" {
					t.Errorf("%s: a 304 carries a body %q", cond[0], res.body)
				}
				if got := res.Header.Get("Cache-Control"); got != first.Header.Get("Cache-Control") {
					t.Errorf("%s: 304 Cache-Control = %q, want %q", cond[0], got, first.Header.Get("Cache-Control"))
				}
				if cond[0] == "If-None-Match" && res.Header.Get("ETag") != etag {
					t.Errorf("%s: 304 ETag = %q, want %q", cond[0], res.Header.Get("ETag"), etag)
				}
				wantSecurityHeaders(t, "304 "+cond[0], res.Response)
			}

			for _, cond := range [][]string{
				{"If-None-Match", `W/"0-0"`},
				{"If-Modified-Since", fixtureTime.Add(-time.Hour).UTC().Format(http.TimeFormat)},
			} {
				res := get(t, h, target, cond...)
				if res.StatusCode != http.StatusOK || res.body != first.body {
					t.Errorf("%s that does not match: %d %q, want the full answer", cond[0], res.StatusCode, res.body)
				}
			}
		})
	}
}

func TestRangeRequests(t *testing.T) {
	h := newSite(t).handler(t)
	full := siteFiles["assets/app-abc123.js"]

	res := get(t, h, "/assets/app-abc123.js", "Range", "bytes=2-5")
	if res.StatusCode != http.StatusPartialContent || res.body != full[2:6] {
		t.Errorf("Range 2-5: %d %q, want 206 %q", res.StatusCode, res.body, full[2:6])
	}
	if got, want := res.Header.Get("Content-Range"), "bytes 2-5/"+strconv.Itoa(len(full)); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if got := res.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q on a range", got)
	}
}

// Rule 10: the directory is read on every request.
func TestReadsTheDirectoryPerRequest(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	t.Run("a rewritten file is served at once, with a new validator", func(t *testing.T) {
		before := get(t, h, "/sw.js")
		s.put(t, "sw.js", "self.skipWaiting(); /* v2 */")
		after := get(t, h, "/sw.js")
		if after.body != "self.skipWaiting(); /* v2 */" {
			t.Errorf("body %q after the rewrite", after.body)
		}
		if after.Header.Get("ETag") == before.Header.Get("ETag") {
			t.Errorf("ETag %q did not change with the file", after.Header.Get("ETag"))
		}
		if res := get(t, h, "/sw.js", "If-None-Match", before.Header.Get("ETag")); res.StatusCode != http.StatusOK {
			t.Errorf("the old validator got %d, want a full 200", res.StatusCode)
		}
	})

	t.Run("a new file appears, a removed one is gone", func(t *testing.T) {
		wantNotFound(t, "before", get(t, h, "/assets/late-abc.js"))
		s.put(t, "assets/late-abc.js", "late")
		if res := get(t, h, "/assets/late-abc.js"); res.body != "late" {
			t.Errorf("a new file: %d %q", res.StatusCode, res.body)
		}
		noErr(t, s.web.Remove("assets/late-abc.js"))
		wantNotFound(t, "after", get(t, h, "/assets/late-abc.js"))
	})

	t.Run("the whole directory is replaced", func(t *testing.T) {
		noErr(t, s.parent.Rename("web", "web-old"))
		noErr(t, s.parent.Mkdir("web", 0o755))
		noErr(t, s.parent.WriteFile("web/index.html", []byte("<title>second deploy</title>"), 0o644))
		noErr(t, s.parent.WriteFile("web/only-new.js", []byte("new"), 0o644))

		if res := get(t, h, "/signin"); res.body != "<title>second deploy</title>" {
			t.Errorf("shell after the replacement: %q", res.body)
		}
		if res := get(t, h, "/only-new.js"); res.body != "new" {
			t.Errorf("a file of the new directory: %q", res.body)
		}
		wantNotFound(t, "a file of the old directory", get(t, h, "/sw.js"))
	})
}

// The handler is built on a directory that goes away: it answers 503, not a 404 that a cache may keep and not
// the shell, until the directory is back.
func TestUnavailableDirectory(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	t.Run("the shell is gone", func(t *testing.T) {
		noErr(t, s.web.Remove("index.html"))
		res := get(t, h, "/signin")
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET /signin: %d, want 503", res.StatusCode)
		}
		if got := res.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", got)
		}
		// What is not a navigation stays a 404, and what is there is still served.
		wantNotFound(t, "GET /nope", get(t, h, "/nope"))
		if res := get(t, h, "/sw.js"); res.StatusCode != http.StatusOK {
			t.Errorf("GET /sw.js: %d, want 200", res.StatusCode)
		}
	})

	t.Run("the directory is gone", func(t *testing.T) {
		noErr(t, s.parent.RemoveAll("web"))
		for _, target := range []string{"/", "/sw.js", "/nope", "/assets/app-abc123.js"} {
			res := get(t, h, target)
			if res.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("GET %s: %d, want 503", target, res.StatusCode)
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("GET %s: Cache-Control = %q, want no-store", target, got)
			}
		}
		// The policy stays ahead of the file system: these never needed the directory.
		if res := do(t, h, http.MethodPost, "/"); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /: %d, want 405", res.StatusCode)
		}
		if res := get(t, h, "/api/x"); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /api/x: %d, want 404", res.StatusCode)
		}
	})
}

// Rule 10, for -race: many readers of one handler.
func TestConcurrentRequests(t *testing.T) {
	s := newSite(t)
	h := s.handler(t)

	targets := []struct {
		target string
		status int
		body   string
	}{
		{"/", 200, shell},
		{"/signin", 200, shell},
		{"/sw.js", 200, siteFiles["sw.js"]},
		{"/assets/app-abc123.js", 200, siteFiles["assets/app-abc123.js"]},
		{"/assets/missing.js", 404, ""},
		{"/api/x", 404, ""},
		{"/assets/../index.html", 400, ""},
	}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 100 {
				tc := targets[(g+i)%len(targets)]
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
				if rec.Code != tc.status {
					t.Errorf("GET %s: status %d, want %d", tc.target, rec.Code, tc.status)
					return
				}
				if tc.body != "" && rec.Body.String() != tc.body {
					t.Errorf("GET %s: body %q, want %q", tc.target, rec.Body.String(), tc.body)
					return
				}
			}
		})
	}
	wg.Wait()
}
