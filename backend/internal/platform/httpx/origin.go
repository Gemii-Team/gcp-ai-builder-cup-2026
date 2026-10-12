package httpx

import (
	"net/http"
	"net/url"
	"strings"
)

// RequireSameOrigin returns middleware that rejects, with 403, every request that
// changes state (any method but GET, HEAD and OPTIONS) unless its Origin header is
// the origin of publicURL. A state-changing request without an Origin is rejected
// too: browsers send one on every cross-origin POST and on same-origin ones, so an
// absent header is a client that is not a browser page. Safe methods pass whatever
// they carry. It panics when publicURL is not an absolute http(s) URL, which is a
// configuration error to find at startup.
func RequireSameOrigin(publicURL string) func(http.Handler) http.Handler {
	u, err := url.Parse(publicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		panic("httpx: RequireSameOrigin needs an absolute http(s) public URL")
	}
	want := strings.ToLower(u.Scheme + "://" + u.Host)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if !sameOrigin(r.Header.Get("Origin"), want) {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sameOrigin reports whether the Origin header value is exactly want, a
// lower-case scheme://host[:port]. An opaque ("null") or malformed origin never is.
func sameOrigin(origin, want string) bool {
	if origin == "" {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" || o.Path != "" || o.RawQuery != "" || o.User != nil {
		return false
	}
	return strings.ToLower(o.Scheme+"://"+o.Host) == want
}
