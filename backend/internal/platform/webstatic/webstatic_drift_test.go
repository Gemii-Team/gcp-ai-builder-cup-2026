package webstatic

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// pwaRoutesTS is the allowlist the service worker is built with, from this package's directory.
const pwaRoutesTS = "../../../../apps/web/src/pwaRoutes.ts"

var (
	// The array of SPA_NAVIGATION_ALLOWLIST, up to the "];" that closes it.
	allowlistBlock = regexp.MustCompile(`(?s)SPA_NAVIGATION_ALLOWLIST\s*:\s*RegExp\[\]\s*=\s*\[\n(.*?)\n\];`)
	// One entry: a regex literal on a line of its own, with the comma after it.
	allowlistEntry = regexp.MustCompile(`^\s*/(.+)/([a-z]*),\s*$`)
)

// tsAllowlist returns the regex literals of SPA_NAVIGATION_ALLOWLIST, compiled by RE2. The syntax
// of the two engines agrees on these patterns ("\/" is a legal escape in RE2). It fails on an
// entry it cannot read as a literal, so that the test cannot pass by reading fewer entries.
func tsAllowlist(t *testing.T) []*regexp.Regexp {
	t.Helper()
	src, err := os.ReadFile(pwaRoutesTS)
	if err != nil {
		t.Fatalf("read the allowlist the service worker is built with: %v", err)
	}
	m := allowlistBlock.FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s: no SPA_NAVIGATION_ALLOWLIST array found; update this test with the file", pwaRoutesTS)
	}

	var res []*regexp.Regexp
	for line := range strings.SplitSeq(string(m[1]), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		e := allowlistEntry.FindStringSubmatch(line)
		if e == nil {
			t.Fatalf("%s: cannot read %q as one regex literal per line", pwaRoutesTS, line)
		}
		if e[2] != "" {
			t.Fatalf("%s: %q has flags %q, which the Go list does not model", pwaRoutesTS, line, e[2])
		}
		re, err := regexp.Compile(e[1])
		if err != nil {
			t.Fatalf("%s: %q does not compile as RE2: %v", pwaRoutesTS, e[1], err)
		}
		res = append(res, re)
	}
	if len(res) == 0 {
		t.Fatalf("%s: the allowlist is empty", pwaRoutesTS)
	}
	return res
}

// The Go list of spaNavigations and SPA_NAVIGATION_ALLOWLIST in apps/web/src/pwaRoutes.ts name the
// same navigations. They answer the same question for the same browser, "does the shell serve this
// route?", one in the service worker and one on the server; a route in only one of them is a reload
// that works offline and 404s online, or the reverse.
func TestSPANavigations_AgreeWithPwaRoutes(t *testing.T) {
	ts := tsAllowlist(t)
	h := newSite(t).handler(t)

	if len(ts) != len(spaNavigations) {
		t.Fatalf("pwaRoutes.ts lists %d navigations, spaNavigations %d: change them together", len(ts), len(spaNavigations))
	}

	// Paths and a query for each, as the service worker sees them (pathname plus search). None
	// is a file of the fixture, so a 200 is the shell.
	paths := []string{
		"/", "/welcome", "/welcome/", "/welcomes", "/signin", "/signin/", "/Signin", "/signin.html", "/signin2",
		"/c", "/c/", "/c/abc", "/c/3f2a9c-uuid", "/c/abc/def", "/c/abc/", "/cx/abc",
		"/board", "/board/", "/boards", "/ask", "/ask/", "/asked", "/ask/more",
		"/calendar", "/calendar/", "/calendars", "/meetings", "/meetings/", "/meetings/x",
		"/meeting", "/meeting/", "/meeting/abc", "/meeting/abc/def",
		"/settings", "/unknown", "/favicon.ico", "/robots.txt",
		"/assets/x.js", "/assets/signin", "/icons/x.png",
		"/api", "/api/auth/login", "/api/blob/abc", "/apiary", "/ws", "/ws/x", "/healthz", "/readyz", "/mockoidc", "/mockoidc/authorize",
	}
	queries := []string{"", "?x=1", "?error=access_denied", "?next=/board", "?"}

	matched := make([]bool, len(ts))
	for _, p := range paths {
		for _, q := range queries {
			target := p + q
			tsServes := false
			for i, re := range ts {
				if re.MatchString(target) {
					tsServes = true
					matched[i] = true
				}
			}
			res := get(t, h, target)
			goServes := res.StatusCode == http.StatusOK && res.body == shell
			if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNotFound {
				t.Errorf("GET %s: status %d", target, res.StatusCode)
			}
			if tsServes != goServes {
				t.Errorf("GET %s: the service worker answers the shell = %v, the server = %v", target, tsServes, goServes)
			}
		}
	}

	// A table that no entry of the TypeScript list matches into would pass whatever the entries said.
	for i, ok := range matched {
		if !ok {
			t.Errorf("no path of the table matches pwaRoutes.ts entry %d (%s): add one", i, ts[i])
		}
	}
}

// The patterns of the Go list that never answer for what belongs to the other roles, whatever pwaRoutes.ts says:
// a navigation list that drifts into /api would answer an API client with HTML if the role were off.
func TestSPANavigations_NeverCoverTheOtherRoles(t *testing.T) {
	for _, p := range []string{"/api", "/api/auth/login", "/ws", "/healthz", "/readyz", "/mockoidc", "/mockoidc/authorize", "/assets/app.js"} {
		if isSPANavigation(p) {
			t.Errorf("spaNavigations answers %q with the shell", p)
		}
	}
	for _, p := range []string{"/api", "/api/x", "/ws", "/ws/x", "/healthz", "/healthz/x", "/readyz", "/mockoidc", "/mockoidc/x"} {
		if !isReserved(p) {
			t.Errorf("isReserved(%q) = false", p)
		}
	}
	for _, p := range []string{"/", "/apiary", "/api.json", "/wsx", "/healthzz", "/mockoidcx", "/c/api", "/signin"} {
		if isReserved(p) {
			t.Errorf("isReserved(%q) = true", p)
		}
	}
}
