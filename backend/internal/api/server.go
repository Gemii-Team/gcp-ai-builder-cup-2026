// Package api is the HTTP shell of the API role. It mounts the Connect handlers
// of every domain, the sign-in routes and the signed blob URLs behind one chain of
// guards (panic recovery, origin check, session check) and gives the domains the
// two things they need from it: ActorFrom and HandlerOptions, which maps their
// errors to Connect codes. The domains know nothing else of it.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/httpx"
)

// Handler is one domain's Connect handler as its Handler constructor returns it:
// Path is the service's route ("/kiwi.chat.v1.ChatService/") and H serves it.
type Handler struct {
	Path string
	H    http.Handler
}

// SessionStore resolves a session id to the user and workspace it was issued to; a
// missing, expired or ended session is errs.ErrNotFound. *cache.Cache implements it.
type SessionStore interface {
	GetSession(ctx context.Context, id string) (cache.SessionRecord, error)
}

// Deps are what New builds the shell from.
type Deps struct {
	// PublicURL is the origin the browser sees; state-changing requests must name
	// it in their Origin header. It must be an absolute http(s) URL.
	PublicURL string
	// Sessions resolves the session cookie of Connect requests.
	Sessions SessionStore
	// Auth serves the /api/auth/ routes (identity's AuthHandler). It is mounted
	// outside the session check, with the full request path.
	Auth http.Handler
	// Blob serves the signed URLs under /api/blob/ (the local store). Its token is
	// the only credential, so it is mounted outside the session check, with the
	// full request path.
	Blob http.Handler
	// Handlers are the domains' Connect handlers, each built with ActorFrom as its
	// actor accessor and HandlerOptions among its options.
	Handlers []Handler
	// Logger receives recovered panics and failed session lookups; nil selects
	// slog.Default().
	Logger *slog.Logger
}

// New returns the handler to mount at "/api/" with the request path unchanged. In
// front of everything sit panic recovery and the origin check; Connect handlers
// (reachable at "/api" + Handler.Path) also need a live session, while /api/auth/
// and /api/blob/ do not. Any other path is a 404. Liveness and readiness are not
// served here: they belong to the process's listener (httpx.Server), outside /api/.
//
// New panics on incomplete Deps, a bad PublicURL or two handlers on one route:
// they are wiring errors to find at startup.
func New(d Deps) http.Handler {
	switch {
	case d.Sessions == nil:
		panic("api: Deps.Sessions is required")
	case d.Auth == nil:
		panic("api: Deps.Auth is required")
	case d.Blob == nil:
		panic("api: Deps.Blob is required")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	sameOrigin := httpx.RequireSameOrigin(d.PublicURL)
	session := requireSession(d.Sessions, log)

	mux := http.NewServeMux()
	mux.Handle("/api/auth/", d.Auth)
	mux.Handle(blob.Prefix, d.Blob)
	for _, h := range d.Handlers {
		if h.H == nil || !strings.HasPrefix(h.Path, "/") || !strings.HasSuffix(h.Path, "/") {
			panic("api: a Handler needs a Path like \"/kiwi.chat.v1.ChatService/\" and an H")
		}
		// Connect handlers match on their own path, which does not include /api.
		mux.Handle("/api"+h.Path, session(http.StripPrefix("/api", h.H)))
	}
	return recoverPanics(log)(sameOrigin(mux))
}
