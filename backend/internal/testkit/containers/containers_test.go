package containers

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// bindRace is the start error observed on this host (RootlessKit port manager,
// the endpoint id shortened) when the host port Docker picked was already held.
const bindRace = "generic container: start container: container start: Error response from daemon: " +
	"failed to set up container networking: driver failed programming external connectivity on endpoint " +
	"romantic_satoshi (1c94ad177ddf): error while calling RootlessKit PortManager.AddPort(): " +
	"listen tcp4 0.0.0.0:33354: bind: address already in use"

// fakeContainer stands in for a created-but-failed container. Only Terminate
// is implemented; any other method on the nil embedded interface panics, which
// is what we want: the helper must not touch anything else.
type fakeContainer struct {
	testcontainers.Container
	terminateErr error
	terminated   int
	// what the cleanup context looked like inside Terminate, before the helper
	// cancels it on return
	termCtxErr     error
	termHasTimeout bool
}

func (f *fakeContainer) Terminate(ctx context.Context, _ ...testcontainers.TerminateOption) error {
	f.terminated++
	f.termCtxErr = ctx.Err()
	_, f.termHasTimeout = ctx.Deadline()
	return f.terminateErr
}

// logRecords collects slog records.
type logRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecords) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logRecords) WithGroup(string) slog.Handler            { return h }
func (h *logRecords) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// withMessage returns the records whose message contains substr.
func (h *logRecords) withMessage(substr string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, substr) {
			out = append(out, r)
		}
	}
	return out
}

func attr(r slog.Record, key string) (slog.Value, bool) {
	var (
		v     slog.Value
		found bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value, true
			return false
		}
		return true
	})
	return v, found
}

// rig runs start with the production policy except that sleeping is recorded
// instead of done and logs are captured.
type rig struct {
	r     retrier
	logs  *logRecords
	slept []time.Duration
	calls int
}

func newRig() *rig {
	g := &rig{logs: &logRecords{}}
	g.r = defaultRetrier
	g.r.log = slog.New(g.logs)
	g.r.sleep = func(ctx context.Context, d time.Duration) error {
		g.slept = append(g.slept, d)
		return ctx.Err()
	}
	return g
}

// run drives start with a starter that is told the 1-based call number.
func (g *rig) run(ctx context.Context, starter func(call int) (testcontainers.Container, error)) (testcontainers.Container, error) {
	return start(ctx, g.r, func(context.Context) (testcontainers.Container, error) {
		g.calls++
		return starter(g.calls)
	})
}

func TestStart_RetriesPortBindRaceThenSucceeds(t *testing.T) {
	g := newRig()
	failed := []*fakeContainer{{}, {}}
	ok := &fakeContainer{}

	got, err := g.run(context.Background(), func(call int) (testcontainers.Container, error) {
		if call <= 2 {
			return failed[call-1], errors.New(bindRace)
		}
		return ok, nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got != testcontainers.Container(ok) {
		t.Fatalf("Start returned %v, want the container of the third attempt", got)
	}
	if g.calls != 3 {
		t.Errorf("starter called %d times, want exactly 3", g.calls)
	}
	for i, f := range failed {
		if f.terminated != 1 {
			t.Errorf("failed attempt %d: leftover container terminated %d times, want 1", i+1, f.terminated)
		}
	}
	if ok.terminated != 0 {
		t.Errorf("the container that started was terminated")
	}

	retries := g.logs.withMessage("retrying")
	if len(retries) != 2 {
		t.Fatalf("logged %d retries, want 2", len(retries))
	}
	for i, r := range retries {
		if r.Level != slog.LevelWarn {
			t.Errorf("retry %d logged at %v, want WARN", i+1, r.Level)
		}
		if n, _ := attr(r, "attempt"); n.Int64() != int64(i+1) {
			t.Errorf("retry %d logged attempt=%v, want %d", i+1, n, i+1)
		}
		if e, _ := attr(r, "error"); !strings.Contains(e.String(), "address already in use") {
			t.Errorf("retry %d did not log the start error, got %v", i+1, e)
		}
	}
	if len(g.slept) != 2 {
		t.Errorf("slept %d times, want 2 (before each retry)", len(g.slept))
	}
}

func TestStart_GivesUpAfterThreeAttempts(t *testing.T) {
	g := newRig()
	var errs []error
	var leftovers []*fakeContainer

	got, err := g.run(context.Background(), func(call int) (testcontainers.Container, error) {
		e := fmt.Errorf("attempt %d: %s", call, bindRace)
		f := &fakeContainer{}
		errs, leftovers = append(errs, e), append(leftovers, f)
		return f, e
	})
	if err == nil {
		t.Fatal("Start succeeded, want an error")
	}
	if got != nil {
		t.Errorf("Start returned a container %v alongside the error", got)
	}
	if g.calls != 3 {
		t.Errorf("starter called %d times, want exactly 3", g.calls)
	}
	if !errors.Is(err, errs[2]) {
		t.Errorf("error does not wrap the last attempt's error: %v", err)
	}
	if errors.Is(err, errs[1]) || errors.Is(err, errs[0]) {
		t.Errorf("error wraps an earlier attempt's error: %v", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("error does not mention 3 attempts: %v", err)
	}
	for i, f := range leftovers {
		if f.terminated != 1 {
			t.Errorf("attempt %d: leftover container terminated %d times, want 1", i+1, f.terminated)
		}
	}
	if n := len(g.logs.withMessage("retrying")); n != 2 {
		t.Errorf("logged %d retries, want 2 (none after the last attempt)", n)
	}
	if len(g.slept) != 2 {
		t.Errorf("slept %d times, want 2 (no sleep after the last attempt)", len(g.slept))
	}
}

func TestStart_DoesNotRetryOtherErrors(t *testing.T) {
	for name, startErr := range map[string]error{
		"plain error":      errors.New("boom"),
		"context deadline": fmt.Errorf("generic container: create container: %w", context.DeadlineExceeded),
		"docker not found": errors.New("rootless Docker not found: Cannot connect to the Docker daemon at unix:///run/user/1000/docker.sock. Is the docker daemon running?"),
		"image pull":       errors.New("generic container: create container: pull access denied for valkey/valkey, repository does not exist or may require 'docker login'"),
		"fixed port taken": errors.New("Error response from daemon: driver failed programming external connectivity on endpoint x: Bind for 0.0.0.0:6379 failed: port is already allocated"),
		"name conflict":    errors.New(`Error response from daemon: Conflict. The container name "/kiwi-pg" is already in use by container "3f2a"`),
		"near miss":        errors.New("listen tcp4 0.0.0.0:33124: bind: address in use"),
		"readiness":        errors.New("wait until ready: log message \"Ready to accept connections\" not seen in time"),
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig()
			got, err := g.run(context.Background(), func(int) (testcontainers.Container, error) {
				return nil, startErr
			})
			if g.calls != 1 {
				t.Errorf("starter called %d times, want exactly 1", g.calls)
			}
			if !errors.Is(err, startErr) {
				t.Errorf("Start returned %v, want the start error", err)
			}
			if got != nil {
				t.Errorf("Start returned a container %v alongside the error", got)
			}
			if n := len(g.logs.withMessage("retrying")); n != 0 {
				t.Errorf("logged %d retries, want none", n)
			}
			if len(g.slept) != 0 {
				t.Errorf("slept %d times, want none", len(g.slept))
			}
		})
	}
}

func TestStart_StopsRetryingWhenContextCancelled(t *testing.T) {
	t.Run("during the attempt", func(t *testing.T) {
		g := newRig()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		leftover := &fakeContainer{}

		_, err := g.run(ctx, func(int) (testcontainers.Container, error) {
			cancel()
			return leftover, errors.New(bindRace)
		})
		if g.calls != 1 {
			t.Errorf("starter called %d times, want exactly 1", g.calls)
		}
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "address already in use") {
			t.Errorf("error should carry both the cancellation and the start error, got %v", err)
		}
		if len(g.slept) != 0 {
			t.Errorf("slept %d times, want none", len(g.slept))
		}
		if leftover.terminated != 1 {
			t.Fatalf("leftover container terminated %d times, want 1", leftover.terminated)
		}
		if leftover.termCtxErr != nil {
			t.Errorf("cleanup ran on a cancelled context: %v", leftover.termCtxErr)
		}
		if !leftover.termHasTimeout {
			t.Error("cleanup context has no deadline, want a bounded one")
		}
	})

	t.Run("during the backoff", func(t *testing.T) {
		g := newRig()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.r.sleep = func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		}

		_, err := g.run(ctx, func(int) (testcontainers.Container, error) {
			return nil, errors.New(bindRace)
		})
		if g.calls != 1 {
			t.Errorf("starter called %d times, want exactly 1", g.calls)
		}
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "address already in use") {
			t.Errorf("error should carry both the cancellation and the start error, got %v", err)
		}
	})
}

func TestStart_CleanupFailureDoesNotMaskTheStartError(t *testing.T) {
	cleanupErr := errors.New("remove: no such container")

	t.Run("on an error that is not retried", func(t *testing.T) {
		g := newRig()
		startErr := errors.New("pull access denied")
		leftover := &fakeContainer{terminateErr: cleanupErr}

		_, err := g.run(context.Background(), func(int) (testcontainers.Container, error) {
			return leftover, startErr
		})
		if !errors.Is(err, startErr) {
			t.Errorf("Start returned %v, want the start error", err)
		}
		if errors.Is(err, cleanupErr) || strings.Contains(err.Error(), cleanupErr.Error()) {
			t.Errorf("the cleanup failure leaked into the returned error: %v", err)
		}
		logged := g.logs.withMessage("remove")
		if len(logged) != 1 {
			t.Fatalf("logged %d cleanup failures, want 1", len(logged))
		}
		if e, _ := attr(logged[0], "error"); !strings.Contains(e.String(), cleanupErr.Error()) {
			t.Errorf("cleanup failure log lacks the cleanup error, got %v", e)
		}
	})

	t.Run("on an attempt that is retried", func(t *testing.T) {
		g := newRig()
		failed := &fakeContainer{terminateErr: cleanupErr}
		ok := &fakeContainer{}

		got, err := g.run(context.Background(), func(call int) (testcontainers.Container, error) {
			if call == 1 {
				return failed, errors.New(bindRace)
			}
			return ok, nil
		})
		if err != nil || got != testcontainers.Container(ok) {
			t.Fatalf("Start = %v, %v; want the second attempt's container", got, err)
		}
		if g.calls != 2 {
			t.Errorf("starter called %d times, want 2", g.calls)
		}
	})
}

func TestBackoff_IsJitteredWithinBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 1000 {
		d := backoff()
		if d < 200*time.Millisecond || d >= 600*time.Millisecond {
			t.Fatalf("backoff() = %v, want in [200ms, 600ms)", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("backoff() always returned the same value, want jitter")
	}
}

func TestStart_SleepsBetweenAttemptsWithinTheBackoffBounds(t *testing.T) {
	g := newRig()
	_, _ = g.run(context.Background(), func(int) (testcontainers.Container, error) {
		return nil, errors.New(bindRace)
	})
	if len(g.slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(g.slept))
	}
	for _, d := range g.slept {
		if d < 200*time.Millisecond || d >= 600*time.Millisecond {
			t.Errorf("slept %v, want in [200ms, 600ms)", d)
		}
	}
}

// bypasses returns "file:line: expr" for every way f starts a container other
// than through this package: a reference (call or stored value) to
// testcontainers.GenericContainer, ParallelContainers, Run or RunContainer, to
// Run or RunContainer of any testcontainers module, or a dot import of either,
// which would hide the calls.
func bypasses(fset *token.FileSet, f *ast.File) []string {
	const root = "github.com/testcontainers/testcontainers-go"
	forbidden := func(pkgPath, name string) bool {
		switch {
		case pkgPath == root:
			return slices.Contains([]string{"GenericContainer", "ParallelContainers", "Run", "RunContainer"}, name)
		case strings.HasPrefix(pkgPath, root+"/modules/"):
			return name == "Run" || name == "RunContainer"
		}
		return false
	}

	where := func(p token.Pos) string {
		pos := fset.Position(p)
		return pos.Filename + ":" + strconv.Itoa(pos.Line)
	}
	var found []string
	byName := map[string]string{} // local package name -> import path
	for _, imp := range f.Imports {
		pkgPath, _ := strconv.Unquote(imp.Path.Value)
		if pkgPath != root && !strings.HasPrefix(pkgPath, root+"/modules/") {
			continue
		}
		switch {
		case imp.Name == nil && pkgPath == root: // package testcontainers, directory testcontainers-go
			byName["testcontainers"] = pkgPath
		case imp.Name == nil:
			byName[path.Base(pkgPath)] = pkgPath
		case imp.Name.Name == ".":
			found = append(found, fmt.Sprintf("%s: dot import of %s", where(imp.Pos()), pkgPath))
		case imp.Name.Name != "_":
			byName[imp.Name.Name] = pkgPath
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && forbidden(byName[id.Name], sel.Sel.Name) {
			found = append(found, fmt.Sprintf("%s: %s.%s", where(sel.Pos()), id.Name, sel.Sel.Name))
		}
		return true
	})
	return found
}

// flagged parses src as file x.go and returns what the guard flags in it.
func flagged(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return bypasses(fset, f)
}

func TestBypasses_FlagsEveryWayAroundTheHelper(t *testing.T) {
	const tc = `"github.com/testcontainers/testcontainers-go"`
	const pg = `"github.com/testcontainers/testcontainers-go/modules/postgres"`
	for name, tt := range map[string]struct {
		src  string
		want []string
	}{
		"testcontainers.Run": {
			src:  "package x\nimport " + tc + "\nvar _, _ = testcontainers.Run(nil, \"i\")\n",
			want: []string{"x.go:3: testcontainers.Run"},
		},
		"testcontainers.GenericContainer": {
			src:  "package x\nimport " + tc + "\nvar _, _ = testcontainers.GenericContainer(nil, testcontainers.GenericContainerRequest{})\n",
			want: []string{"x.go:3: testcontainers.GenericContainer"},
		},
		"aliased module Run": {
			src:  "package x\nimport tcpg " + pg + "\nfunc f() { tcpg.Run(nil, \"i\") }\n",
			want: []string{"x.go:3: tcpg.Run"},
		},
		"module Run kept as a value": {
			src:  "package x\nimport " + pg + "\nvar run = postgres.Run\n",
			want: []string{"x.go:3: postgres.Run"},
		},
		"valkey module": {
			src:  "package x\nimport \"github.com/testcontainers/testcontainers-go/modules/valkey\"\nvar _, _ = valkey.Run(nil, \"i\")\n",
			want: []string{"x.go:3: valkey.Run"},
		},
		"dot import": {
			src:  "package x\nimport . " + tc + "\nvar _, _ = Run(nil, \"i\")\n",
			want: []string{"x.go:2: dot import of github.com/testcontainers/testcontainers-go"},
		},
		"options and types are fine": {
			src: "package x\nimport " + tc + "\nvar _ = testcontainers.WithCmd(\"a\")\nvar _ testcontainers.Container\nvar _ = testcontainers.GenericContainerRequest{}\n",
		},
		"module options are fine": {
			src: "package x\nimport " + pg + "\nvar _ = postgres.WithUsername(\"u\")\n",
		},
		"unrelated Run is fine": {
			src: "package x\nimport \"os/exec\"\nvar _ = exec.Command\nfunc Run() {}\nfunc g() { Run() }\n",
		},
		"the helper is fine": {
			src: "package x\nimport \"kiwi/internal/testkit/containers\"\nvar _, _ = containers.Start(nil, \"i\")\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := flagged(t, tt.src)
			if !slices.Equal(got, tt.want) {
				t.Errorf("flagged %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNoPackageStartsContainersDirectly keeps new tests from bypassing the
// retry: every container start under backend/ goes through this package.
func TestNoPackageStartsContainersDirectly(t *testing.T) {
	root := filepath.Join("..", "..", "..") // backend/
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("backend root not found from %s: %v", t.Name(), err)
	}
	self := filepath.Join(root, "internal", "testkit", "containers")

	var files int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, v := range bypasses(fset, f) {
			t.Errorf("%s: starts a container directly; use containers.Start or containers.StartPostgres", v)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files < 50 {
		t.Fatalf("walked only %d Go files under %s, the guard is not looking at the module", files, root)
	}
}
