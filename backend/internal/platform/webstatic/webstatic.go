// Package webstatic serves the built web app (the Vite output: index.html, the service
// worker, the manifest, hashed assets, icons) from one directory, so a single process can
// answer the API, the realtime socket and the app itself.
//
// It mirrors apps/web/Caddyfile, with three deliberate differences. A path that is not a file
// falls back to index.html only for the navigations the app itself owns (spaNavigations); every
// other missing path is a 404, where Caddy answers the shell for any path. The paths that belong
// to the server's other roles are never served from the directory. And there is no compression:
// Caddy's zstd/gzip is not mirrored.
package webstatic

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	indexName    = "index.html"
	assetsPrefix = "/assets/"

	// Hashed build output never changes under its name.
	cacheImmutable = "public, max-age=31536000, immutable"
	// Everything else is stored but revalidated on every use (the ETag makes that a 304).
	cacheRevalidate = "no-cache"
	// An error is never stored: a missing asset that appears with the next deploy must not stay missing.
	cacheNever = "no-store"

	htmlType = "text/html; charset=utf-8"
)

// spaNavigations is the one list of paths that answer with the app shell when no file of that
// name exists: the browser routes of apps/web/src/router.tsx. It is an allowlist for the reason
// SPA_NAVIGATION_ALLOWLIST in apps/web/src/pwaRoutes.ts is one: any other path (a mistyped
// asset, an API route whose role is off, a probe) must stay a 404 instead of becoming a 200 page
// that hides the fault.
//
// The patterns are those of pwaRoutes.ts without their optional query suffix, because here they
// are tested against the URL path alone. webstatic_drift_test.go reads pwaRoutes.ts and fails
// when the two lists disagree: change them together.
var spaNavigations = []*regexp.Regexp{
	regexp.MustCompile(`^/$`),
	regexp.MustCompile(`^/signin$`),
	regexp.MustCompile(`^/c/[^/?#]+$`),
	regexp.MustCompile(`^/board$`),
	regexp.MustCompile(`^/ask$`),
}

// reservedPrefixes are the paths of the server's other roles: the API, the realtime socket, the
// probes and the local mock sign-in. The process mounts this handler as the fallback of its mux,
// so a request for one of them reaches it only when that role is off. It must be a 404 then:
// neither a file of that name nor the app shell, which would answer an API client with HTML.
var reservedPrefixes = [...]string{"/api", "/ws", "/healthz", "/readyz", "/mockoidc"}

// contentTypes is the type of every extension a Vite build emits, so the answer does not depend
// on a mime database of the host (a distroless image has none). Text types carry their charset.
// An extension outside it falls back to the mime package, then to application/octet-stream.
var contentTypes = map[string]string{
	".html":        htmlType,
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".wasm":        "application/wasm",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".gif":         "image/gif",
	".ico":         "image/vnd.microsoft.icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
}

type handler struct {
	dir string
}

// New returns the handler that serves the web app found in dir. It fails when dir is not an
// absolute path, is not a directory, or has no index.html (a regular file inside dir).
//
// The handler reads the directory on every request and keeps no file content, so a redeploy that
// replaces the files, or the whole directory, is served at once. Safe for concurrent use.
//
// Method and path policy: only GET and HEAD are served (405 otherwise). Mount it for every
// method, as the "/" pattern of the mux, so that its 405 carries the headers below; a pattern
// restricted to GET would answer 405 itself, without them.
func New(dir string) (http.Handler, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("webstatic: web directory %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("webstatic: open web directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	f, _, ok := openRegular(root, indexName)
	if !ok {
		return nil, fmt.Errorf("webstatic: web directory %q has no readable %s", dir, indexName)
	}
	_ = f.Close()

	return &handler{dir: dir}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// First, so that it is on every answer below, errors included.
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "same-origin")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed)
		return
	}

	p := r.URL.Path
	name, ok := fileName(p)
	if !ok {
		fail(w, http.StatusBadRequest)
		return
	}
	if isReserved(p) {
		fail(w, http.StatusNotFound)
		return
	}

	// Opened per request, not once: a root holds the directory it was opened on, which a
	// redeploy that replaces the directory (rather than its files) leaves behind.
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = root.Close() }()

	// A file wins over the shell. Whatever is not a readable regular file here (missing, a
	// directory, a symlink out of the root) is no file, and falls through to the allowlist.
	if name != "" {
		if f, info, ok := openRegular(root, name); ok {
			defer func() { _ = f.Close() }()
			cache := cacheRevalidate
			if strings.HasPrefix(p, assetsPrefix) {
				cache = cacheImmutable
			}
			serve(w, r, f, info, contentType(name), cache)
			return
		}
	}

	if !isSPANavigation(p) {
		fail(w, http.StatusNotFound)
		return
	}
	f, info, ok := openRegular(root, indexName)
	if !ok {
		// New found it: the deploy that replaces the directory is half done.
		fail(w, http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = f.Close() }()
	serve(w, r, f, info, htmlType, cacheRevalidate)
}

// fileName maps a URL path (already percent-decoded once, as net/http hands it over) to a name
// relative to the web directory, "" for "/". It refuses what no build output is named and no
// browser sends, so that "what the path says" and "what the file system resolves" cannot differ:
//
//   - a path that does not start with "/",
//   - a NUL byte, which the file system would read as the end of the name,
//   - a backslash, a separator on some systems,
//   - a "." or ".." segment, and an empty one ("//"): os.Root keeps ".." inside the directory,
//     but "/assets/../index.html" is still not the path of what it would serve,
//   - a percent escape that is still there after the decoding (%252e%252e is ".." for a proxy
//     in front of this process that decodes twice).
func fileName(p string) (string, bool) {
	if p == "" || p[0] != '/' {
		return "", false
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", false
	}
	if strings.IndexByte(p, '\\') >= 0 {
		return "", false
	}
	if hasPercentEscape(p) {
		return "", false
	}
	if strings.Contains(p, "//") {
		return "", false
	}
	for rest := p[1:]; rest != ""; {
		var seg string
		seg, rest, _ = strings.Cut(rest, "/")
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	return p[1:], true
}

// hasPercentEscape reports whether s holds a "%" followed by two hex digits.
func hasPercentEscape(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2]) {
			return true
		}
	}
	return false
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// isReserved reports whether p is one of reservedPrefixes or below it. "/apiary" is not.
func isReserved(p string) bool {
	for _, prefix := range reservedPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

func isSPANavigation(p string) bool {
	for _, re := range spaNavigations {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// openRegular opens name below root and returns the file and its info, or false when there is no
// regular file of that name that root can reach: missing, a directory, or a symlink that leaves it.
func openRegular(root *os.Root, name string) (*os.File, os.FileInfo, bool) {
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, false
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, false
	}
	return f, info, true
}

// serve answers with f. ServeContent brings Range, If-Modified-Since, If-None-Match and HEAD; the
// type is set here, so it never sniffs the content.
func serve(w http.ResponseWriter, r *http.Request, f *os.File, info os.FileInfo, ctype, cache string) {
	hdr := w.Header()
	hdr.Set("Content-Type", ctype)
	hdr.Set("Cache-Control", cache)
	hdr.Set("ETag", weakETag(info))
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// weakETag derives a validator from what stat tells, as Caddy does. Weak, because two different
// contents of one size within one mtime tick would share it.
func weakETag(info os.FileInfo) string {
	return `W/"` + strconv.FormatInt(info.Size(), 16) + "-" + strconv.FormatInt(info.ModTime().UnixNano(), 16) + `"`
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// fail answers with a plain-text status and a body that tells nothing about the directory.
func fail(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", cacheNever)
	http.Error(w, http.StatusText(status), status)
}
