package httpx

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// start serves s on a free loopback port. The returned stop cancels the Run
// context and returns Run's error; it is safe to call more than once.
func start(t *testing.T, s *Server) (base string, stop func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, ln) }()

	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			result = <-done
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return "http://" + ln.Addr().String(), stop
}

func client() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

func status(t *testing.T, url string) int {
	t.Helper()
	resp, err := client().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestReadyz_503UntilReady(t *testing.T) {
	s := New("", nil)
	s.DrainTimeout = 0 // these tests do not exercise the drain; cleanup should not wait for it
	base, _ := start(t, s)

	if got := status(t, base+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz before ready = %d, want 200", got)
	}
	if got := status(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz before ready = %d, want 503", got)
	}

	s.SetReady(true)
	if got := status(t, base+"/readyz"); got != http.StatusOK {
		t.Errorf("/readyz after SetReady(true) = %d, want 200", got)
	}

	s.SetReady(false)
	if got := status(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz after SetReady(false) = %d, want 503", got)
	}
	if got := status(t, base+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz while not ready = %d, want 200", got)
	}
}

func TestRun_ServesTheWrappedHandlerBesideTheProbes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wrapped "+r.URL.Path, http.StatusTeapot)
	})
	s := New("", next)
	s.DrainTimeout = 0
	base, _ := start(t, s)

	if got := status(t, base+"/api/ping"); got != http.StatusTeapot {
		t.Errorf("/api/ping = %d, want the wrapped handler's 418 even before ready", got)
	}
	if got := status(t, base+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: probes must not reach the wrapped handler", got)
	}
	if got := status(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503: probes must not reach the wrapped handler", got)
	}
}

func TestRun_ReturnsTheListenError(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()

	if err := New(taken.Addr().String(), nil).Run(context.Background()); err == nil {
		t.Fatal("Run succeeded on an address that is already in use")
	}
}

func TestShutdown_FlipsReadinessBeforeClosing(t *testing.T) {
	const drain = 300 * time.Millisecond

	entered := make(chan struct{})
	release := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	s := New("", next)
	s.DrainTimeout = drain
	s.SetReady(true)
	base, stop := start(t, s)

	type reply struct {
		code int
		body string
		err  error
	}
	inflight := make(chan reply, 1)
	go func() {
		resp, err := client().Get(base + "/slow")
		if err != nil {
			inflight <- reply{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		inflight <- reply{code: resp.StatusCode, body: string(b), err: err}
	}()
	<-entered
	if got := status(t, base+"/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d, want 200", got)
	}

	cancelledAt := time.Now()
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()

	// Poll until the listener closes. Until then every answer must be 503 once
	// readiness has flipped, and it must never flip back.
	sawNotReady := false
	for {
		resp, err := client().Get(base + "/readyz")
		if err != nil {
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			sawNotReady = true
		case http.StatusOK:
			if sawNotReady {
				t.Fatal("/readyz went back to 200 during shutdown")
			}
		default:
			t.Fatalf("/readyz = %d during shutdown", resp.StatusCode)
		}
		if time.Since(cancelledAt) > 10*time.Second {
			t.Fatal("listener still open 10s after cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closedAfter := time.Since(cancelledAt)

	if !sawNotReady {
		t.Error("the listener closed without /readyz ever answering 503")
	}
	if closedAfter < drain {
		t.Errorf("listener closed %v after cancel, before DrainTimeout %v", closedAfter, drain)
	}

	select {
	case err := <-stopped:
		t.Fatalf("Run returned (%v) while a request was still in flight", err)
	case r := <-inflight:
		t.Fatalf("in-flight request ended before it was released: %+v", r)
	default:
	}

	close(release)
	r := <-inflight
	if r.err != nil || r.code != http.StatusOK || r.body != "finished" {
		t.Fatalf("in-flight request = %+v, want 200 finished", r)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Run = %v, want nil after a clean shutdown", err)
	}
}
