// Package api is the HTTP shell of the API role. It mounts the Connect handlers
// of every domain, the sign-in routes and the signed blob URLs behind one chain of
// guards (panic recovery, origin check, session check) and gives the domains the
// two things they need from it: ActorFrom, and the error mapping to Connect codes
// that New builds into every handler it mounts. The domains know nothing else of it.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/httpx"
)

// maxRequestBytes bounds what any RPC reads of a request. connect-go decodes a body of any
// size unless told a limit, and each domain's own limits apply only after the decode. File
// bytes never travel through an RPC (they go to /api/blob/, whose signed URL carries its
// own cap), so 1 MiB is far above the largest request a client has a reason to send.
const maxRequestBytes = 1 << 20

// Handler is how a domain's Connect handler is registered: not as a built
// http.Handler, which no interceptor can be added to, but as the function that
// builds it.
type Handler struct {
	// Build constructs the handler with the options New gives it and returns its
	// route ("/kiwi.chat.v1.ChatService/") and the handler. The options carry the
	// error mapping that keeps domain errors from reaching clients as anything but
	// Connect codes and keeps infrastructure errors from reaching them at all, so
	// Build MUST pass every one of them on to the handler it builds, which the
	// domains' Handler constructors do (their opts parameter):
	//
	//	api.Handler{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
	//		return chat.Handler(chatSvc, api.ActorFrom, opts...)
	//	}}
	//
	// New refuses at startup a Build that did not: it counts the RPCs the mapping
	// was applied to, and none is a panic. What it cannot see is a Build that
	// applied the options to some handler other than the one it returns, or to only
	// some of its RPCs; the generated Connect constructors do neither.
	Build func(opts ...connect.HandlerOption) (path string, h http.Handler)
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
	// Handlers are the domains' Connect handlers. Each is built by New with the
	// error mapping and mounted behind the session check.
	Handlers []Handler
	// Logger receives recovered panics, failed session lookups and the errors that
	// are hidden from clients; nil selects slog.Default().
	Logger *slog.Logger
}

// New returns the handler to mount at "/api/" with the request path unchanged. In
// front of everything sit panic recovery and the origin check; Connect handlers
// (reachable at "/api" + the path their Build returns) also need a live session,
// while /api/auth/ and /api/blob/ do not. Any other path is a 404. Liveness and
// readiness are not served here: they belong to the process's listener
// (httpx.Server), outside /api/.
//
// New panics on incomplete Deps, a bad PublicURL, a Build that returns no usable
// handler or ignores its options, or two handlers on one route: they are wiring
// errors to find at startup.
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
	mapper := &errorMapper{log: log}
	opts := []connect.HandlerOption{connect.WithInterceptors(mapper), connect.WithReadMaxBytes(maxRequestBytes)}

	mux := http.NewServeMux()
	mux.Handle("/api/auth/", d.Auth)
	mux.Handle(blob.Prefix, d.Blob)
	for _, h := range d.Handlers {
		if h.Build == nil {
			panic("api: a Handler needs a Build")
		}
		before := mapper.wrapped.Load()
		path, handler := h.Build(opts...)
		if handler == nil || !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") {
			panic(fmt.Sprintf("api: a Handler's Build must return a path like \"/kiwi.chat.v1.ChatService/\" and a handler, got %q", path))
		}
		if mapper.wrapped.Load() == before {
			panic(fmt.Sprintf("api: the handler built for %q ignored the options New gave it, so its errors would reach clients unmapped", path))
		}
		// Connect handlers match on their own path, which does not include /api.
		mux.Handle("/api"+path, session(http.StripPrefix("/api", handler)))
	}
	return recoverPanics(log)(sameOrigin(mux))
}
