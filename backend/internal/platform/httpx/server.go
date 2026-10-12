// Package httpx runs the process's HTTP listener: liveness, readiness and a
// shutdown that stops advertising readiness before it stops listening.
package httpx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	defaultDrainTimeout = 5 * time.Second
	// drain plus shutdown stays inside Kubernetes' default 30 s termination grace.
	shutdownTimeout   = 20 * time.Second
	readHeaderTimeout = 10 * time.Second
	// readyCheckTimeout bounds one run of ReadyCheck: a hung dependency answers the
	// probe with 503 instead of hanging it.
	readyCheckTimeout = 2 * time.Second
)

// Server serves /healthz and /readyz itself and hands every other request to
// the wrapped handler.
type Server struct {
	// DrainTimeout is how long Run keeps serving, with /readyz answering 503,
	// after its context is cancelled and before it stops accepting connections.
	DrainTimeout time.Duration
	// ReadyCheck, when set, is asked on every /readyz probe once SetReady(true) has
	// been called, and a non-nil answer makes the probe 503: the process is ready
	// only while what it depends on answers. It runs under a short deadline. The
	// cause is logged, never put in the response. Set it before Run or Serve.
	ReadyCheck func(ctx context.Context) error

	srv   *http.Server
	next  http.Handler
	ready atomic.Bool
}

// New returns a Server for addr that is not ready until SetReady(true). A nil
// h makes every path except the probes answer 404.
func New(addr string, h http.Handler) *Server {
	if h == nil {
		h = http.NotFoundHandler()
	}
	s := &Server{DrainTimeout: defaultDrainTimeout, next: h}
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           http.HandlerFunc(s.route),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return s
}

// SetReady sets what /readyz reports. /healthz is unaffected.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		reply(w, http.StatusOK, "ok")
	case "/readyz":
		if s.isReady(r.Context()) {
			reply(w, http.StatusOK, "ready")
			return
		}
		reply(w, http.StatusServiceUnavailable, "not ready")
	default:
		s.next.ServeHTTP(w, r)
	}
}

// isReady is the readiness flag, and the dependency check when there is one.
func (s *Server) isReady(ctx context.Context) bool {
	if !s.ready.Load() {
		return false
	}
	if s.ReadyCheck == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, readyCheckTimeout)
	defer cancel()
	if err := s.ReadyCheck(ctx); err != nil {
		slog.WarnContext(ctx, "httpx: not ready", "error", err)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body+"\n")
}

// Run listens on the address given to New and serves until ctx is cancelled.
// It then flips /readyz to 503, keeps serving for DrainTimeout so load
// balancers stop routing here, and shuts the server down, waiting for in-flight
// requests. A clean shutdown returns nil.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("httpx: %w", err)
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on a listener the caller opened, so that it can fail on a taken port
// before it starts anything else, or serve on a port it does not choose. Serve owns
// ln from here on.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- s.srv.Serve(ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("httpx: serve: %w", err)
	case <-ctx.Done():
	}

	s.SetReady(false)
	drain := time.NewTimer(s.DrainTimeout)
	defer drain.Stop()
	select {
	case err := <-served:
		return fmt.Errorf("httpx: serve: %w", err)
	case <-drain.C:
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		_ = s.srv.Close()
		return fmt.Errorf("httpx: shutdown: %w", err)
	}
	<-served // Serve returns http.ErrServerClosed once Shutdown has begun.
	return nil
}
