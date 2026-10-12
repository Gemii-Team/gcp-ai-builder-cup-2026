package pdftext

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSandbox is a sandbox like the default one with a shorter deadline for a child.
func newSandbox(timeout time.Duration) *sandbox {
	return &sandbox{timeout: timeout, slots: make(chan struct{}, maxConcurrent), lim: defaultLimits}
}

// tracker counts the children a sandbox has running.
type tracker struct {
	mu      sync.Mutex
	running int
	peak    int
	events  []int
}

func (tr *tracker) observe(delta int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.running += delta
	tr.peak = max(tr.peak, tr.running)
	tr.events = append(tr.events, delta)
}

func (tr *tracker) snapshot() (running, peak int, events []int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.running, tr.peak, slices.Clone(tr.events)
}

// waitUntil polls cond until it holds or the test's patience is out.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSandbox_ReadsLikeTheProcessItself(t *testing.T) {
	quietLogs(t)
	fixture, err := os.ReadFile(flateFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"three pages":   minimalPDF("First page.", "Second (page), with a \\ slash.", "Third"),
		"Thai":          thaiPDF("BT /F2 14 Tf 72 700 Td <000100020003000100040005> Tj 100 0 Td <0006000700030008> Tj ET"),
		"compressed":    fixture,
		"a scan":        imageOnlyPDF(),
		"not a PDF":     []byte("this is not a pdf"),
		"a hostile one": treePDF("<< /Type /Pages /Kids [2 0 R 3 0 R] /Count 1 >>", show("once")),
	} {
		t.Run(name, func(t *testing.T) {
			wantPages, wantErr := local(data)
			gotPages, gotErr := Extract(bytes.NewReader(data))
			if !same(wantPages, wantErr, gotPages, gotErr) {
				t.Fatalf("through the child: %q, %v; in this process: %q, %v", gotPages, gotErr, wantPages, wantErr)
			}
			wantN, wantNErr := localCount(data)
			gotN, gotNErr := PageCount(bytes.NewReader(data))
			if wantN != gotN || errors.Is(wantNErr, ErrUnreadable) != errors.Is(gotNErr, ErrUnreadable) {
				t.Fatalf("page count through the child: %d, %v; in this process: %d, %v", gotN, gotNErr, wantN, wantNErr)
			}
		})
	}
}

func TestSandbox_CraftedFilesAreContained(t *testing.T) {
	logs := quietLogs(t)
	s := newSandbox(3 * time.Second)
	normal := minimalPDF("still here")
	for _, c := range craftedFiles {
		t.Run(c.name, func(t *testing.T) {
			if c.does == "out of memory" && raceBuild && overcommitsEverything() {
				t.Skip("this host grants any allocation, and a race-built child has no memory limit to refuse it")
			}
			var pages []string
			var err error
			within(t, childBoxed, func() { pages, err = s.extract(t.Context(), bytes.NewReader(c.pdf())) })
			if !errors.Is(err, ErrUnreadable) || pages != nil {
				t.Fatalf("extract of a file that makes the library %s = %q, %v, want ErrUnreadable", c.does, pages, err)
			}
			// Counting pages does not load what the file breaks in the first two (the
			// resources of the page), so it may count; what it may not do is take the
			// process down.
			var n int
			within(t, childBoxed, func() { n, err = s.count(t.Context(), bytes.NewReader(c.pdf())) })
			if (err != nil || n != 1) && (!errors.Is(err, ErrUnreadable) || n != 0) {
				t.Fatalf("count of the same file = %d, %v, want its one page or ErrUnreadable", n, err)
			}
			// The server is still up and reads the next file.
			within(t, childBoxed, func() { pages, err = s.extract(t.Context(), bytes.NewReader(normal)) })
			if err != nil || !slices.Equal(pages, []string{"still here"}) {
				t.Fatalf("the next file = %q, %v, want its text", pages, err)
			}
		})
	}
	if !strings.Contains(logs.String(), "the reader process died on a file") {
		t.Fatalf("the deaths of the children were not logged:\n%s", logs.String())
	}
}

func TestSandbox_AtMostTwoChildrenRun(t *testing.T) {
	quietLogs(t)
	s := newSandbox(10 * time.Second)
	tr := &tracker{}
	s.observe = tr.observe

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pages, err := s.extract(t.Context(), bytes.NewReader(minimalPDF("x")))
			if err != nil || !slices.Equal(pages, []string{"x"}) {
				t.Errorf("extract = %q, %v", pages, err)
			}
		}()
	}
	wg.Wait()
	running, peak, events := tr.snapshot()
	if running != 0 || peak < 1 || peak > maxConcurrent || len(events) != 16 {
		t.Fatalf("running %d, peak %d, %d events; want none running, a peak of at most %d and 8 children", running, peak, len(events), maxConcurrent)
	}
}

func TestSandbox_AChildWaitsForASlot(t *testing.T) {
	quietLogs(t)
	s := newSandbox(2 * time.Second)
	s.slots = make(chan struct{}, 1)
	tr := &tracker{}
	s.observe = tr.observe

	// The first child hangs, on a file that makes the library loop, until its deadline.
	stuck := make(chan error, 1)
	go func() {
		_, err := s.extract(t.Context(), bytes.NewReader(objectStreamExtendsLoopPDF()))
		stuck <- err
	}()
	waitUntil(t, "the first child", func() bool { running, _, _ := tr.snapshot(); return running == 1 })

	next := make(chan []string, 1)
	go func() {
		pages, err := s.extract(t.Context(), bytes.NewReader(minimalPDF("later")))
		if err != nil {
			t.Errorf("second extract: %v", err)
		}
		next <- pages
	}()
	time.Sleep(500 * time.Millisecond)
	if _, _, events := tr.snapshot(); !slices.Equal(events, []int{1}) {
		t.Fatalf("events %v after the second file arrived, want only the first child started", events)
	}

	if err := <-stuck; !errors.Is(err, ErrUnreadable) {
		t.Fatalf("the stuck file = %v, want ErrUnreadable", err)
	}
	if pages := <-next; !slices.Equal(pages, []string{"later"}) {
		t.Fatalf("the waiting file = %q", pages)
	}
	if _, _, events := tr.snapshot(); !slices.Equal(events, []int{1, -1, 1, -1}) {
		t.Fatalf("events %v, want the two children one after the other", events)
	}

	// A caller that gives up while it waits for a slot is told so, and never starts a
	// child: this test holds the only slot.
	s.slots <- struct{}{}
	defer func() { <-s.slots }()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := s.extract(ctx, bytes.NewReader(minimalPDF("never")))
	if _, _, events := tr.snapshot(); len(events) != 4 {
		t.Fatalf("events %v, a child was started for a caller that gave up", events)
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("extract with no slot free = %v, want the context's error", err)
	}
}

func TestSandbox_CallerGivingUpKillsTheChild(t *testing.T) {
	quietLogs(t)
	s := newSandbox(time.Minute)
	tr := &tracker{}
	s.observe = tr.observe
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	result := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := s.extract(ctx, bytes.NewReader(objectStreamExtendsLoopPDF()))
		result <- err
	}()
	waitUntil(t, "the child", func() bool { running, _, _ := tr.snapshot(); return running == 1 })
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnreadable) {
			t.Fatalf("extract = %v, want the context's error and not ErrUnreadable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child was not killed when the caller gave up")
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("it took the deadline of the child, not the caller's cancellation")
	}
	if running, _, _ := tr.snapshot(); running != 0 {
		t.Fatalf("%d children still counted as running", running)
	}
}

func TestSandbox_AChildThatDoesNotStartIsNotTheFilesFault(t *testing.T) {
	quietLogs(t)
	truth, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) to stand in for a program that does not know what a child is")
	}
	for name, exe := range map[string]string{
		"no such program":                         filepath.Join(t.TempDir(), "missing"),
		"a program that is not the reader":        truth,
		"a file that is not a program at all":     filepath.Join("testdata", "flate-truetype.pdf"),
		"a directory where the program should be": t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(5 * time.Second)
			s.exe = exe
			pages, err := s.extract(t.Context(), bytes.NewReader(minimalPDF("x")))
			if err == nil || errors.Is(err, ErrUnreadable) || pages != nil {
				t.Fatalf("extract = %q, %v, want an error that is not ErrUnreadable", pages, err)
			}
		})
	}
}

func TestSandbox_CommandIsConfined(t *testing.T) {
	t.Setenv("KIWI_LLM_API_KEY", "a secret the child must not see")
	t.Setenv("PATH", "/somewhere")
	s := newSandbox(time.Second)
	data := []byte("%PDF-1.4")
	cmd := s.command(t.Context(), "/proc/self/exe", modeExtract, data)

	for _, kv := range cmd.Env {
		if strings.Contains(kv, "secret") || strings.HasPrefix(kv, "KIWI_LLM") || strings.HasPrefix(kv, "PATH=") {
			t.Fatalf("the child's environment carries %q", kv)
		}
	}
	if !slices.Contains(cmd.Env, childEnv+"=extract") {
		t.Fatalf("environment %v, want the marker", cmd.Env)
	}
	for _, kv := range cmd.Env {
		if k, _, _ := strings.Cut(kv, "="); k != childEnv && k != "GOMAXPROCS" && k != "GORACE" {
			t.Fatalf("environment %v has %q, which is not on the list of what a child gets", cmd.Env, k)
		}
	}
	if st, err := os.Stat(cmd.Dir); err != nil || !st.IsDir() {
		t.Fatalf("the child's directory %q is not one: %v", cmd.Dir, err)
	}
	if cmd.Stdin == nil || cmd.WaitDelay <= 0 {
		t.Fatalf("stdin %v, wait delay %v: the file goes in on stdin and a killed child's pipes are not waited for for ever", cmd.Stdin, cmd.WaitDelay)
	}
}

func TestSandbox_ResultOverTheCapIsUnreadable(t *testing.T) {
	quietLogs(t)
	s := newSandbox(10 * time.Second)
	s.lim.totalText = 0 // all that is left to a result is the slack for its framing
	long := helveticaPage("BT /F1 12 Tf 72 720 Td (" + strings.Repeat("a", 300<<10) + ") Tj ET")
	pages, err := s.extract(t.Context(), bytes.NewReader(long))
	if !errors.Is(err, ErrUnreadable) || pages != nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Fatalf("extract = %d pages, %v, want ErrUnreadable for a result over the cap", len(pages), err)
	}
	// And the file that is over the size limit is refused before a child is started.
	s = newSandbox(10 * time.Second)
	s.lim.input = 10
	tr := &tracker{}
	s.observe = tr.observe
	if _, err := s.extract(t.Context(), bytes.NewReader(minimalPDF("x"))); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("extract of a file over the limit = %v, want ErrUnreadable", err)
	}
	if _, _, events := tr.snapshot(); len(events) != 0 {
		t.Fatalf("a child was started for a file over the size limit: %v", events)
	}
}

func TestSandbox_FramesFromAChild(t *testing.T) {
	s := newSandbox(time.Second)
	frame := func(kind byte, parts ...uint64) []byte {
		b := []byte{kind}
		for _, p := range parts {
			b = binary.AppendUvarint(b, p)
		}
		return b
	}
	good := append(frame(framePages, 2, 3), "abc"...)
	good = append(binary.AppendUvarint(good, 2), "de"...)
	if pages, err := s.pages(good); err != nil || !slices.Equal(pages, []string{"abc", "de"}) {
		t.Fatalf("pages = %q, %v", pages, err)
	}
	if pages, err := s.pages(frame(framePages, 0)); err != nil || len(pages) != 0 {
		t.Fatalf("pages of an empty result = %q, %v", pages, err)
	}
	if _, err := s.pages(append([]byte{frameError}, "the page tree is too deep"...)); !errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("the child's verdict = %v, want ErrUnreadable with its reason", err)
	}
	if n, err := s.pageCount(frame(frameCount, 7)); err != nil || n != 7 {
		t.Fatalf("count = %d, %v", n, err)
	}

	// Anything else is not something this program's child prints: a bug, not a verdict
	// on the file, so it is not ErrUnreadable.
	for name, bad := range map[string][]byte{
		"nothing":                 nil,
		"a frame of another kind": frame('X', 1),
		"the count of the other":  frame(frameCount, 1),
		"too many pages":          frame(framePages, uint64(s.lim.pages)+1),
		"a page cut short":        append(frame(framePages, 1, 5), "abc"...),
		"a missing page":          frame(framePages, 2, 0),
		"bytes after the pages":   append(frame(framePages, 1, 1), "ab"...),
		"an endless varint":       append([]byte{framePages}, bytes.Repeat([]byte{0xff}, 12)...),
	} {
		if pages, err := s.pages(bad); err == nil || errors.Is(err, ErrUnreadable) || pages != nil {
			t.Errorf("%s: pages = %q, %v, want an error that is not ErrUnreadable", name, pages, err)
		}
	}
	for name, bad := range map[string][]byte{
		"nothing":               nil,
		"pages for a count":     frame(framePages, 0),
		"bytes after the count": append(frame(frameCount, 1), 0),
		"too many pages":        frame(frameCount, uint64(s.lim.pages)+1),
	} {
		if n, err := s.pageCount(bad); err == nil || errors.Is(err, ErrUnreadable) || n != 0 {
			t.Errorf("%s: count = %d, %v, want an error that is not ErrUnreadable", name, n, err)
		}
	}
}

// TestSandbox_ProductionBuildRunsUnderItsLimits runs the same checks with a child that
// is built the way the server is, without the race detector: the memory limit of a child
// applies only there, and the child must still start and work under it. The program is
// testdata/childmain, which is nothing but the init hook of this package.
func TestSandbox_ProductionBuildRunsUnderItsLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a program")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to build the child with")
	}
	logs := quietLogs(t)
	exe := filepath.Join(t.TempDir(), "childmain")
	build := exec.Command(goTool, "build", "-o", exe, "./testdata/childmain")
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./testdata/childmain: %v\n%s", err, out)
	}

	s := newSandbox(3 * time.Second)
	s.exe = exe
	fixture, err := os.ReadFile(flateFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"three pages": minimalPDF("First page.", "Second page.", "Third"),
		"Thai":        thaiPDF("BT /F2 14 Tf 72 700 Td <000100020003000100040005> Tj ET"),
		"compressed":  fixture,
	} {
		want, wantErr := local(data)
		got, gotErr := s.extract(t.Context(), bytes.NewReader(data))
		if !same(want, wantErr, got, gotErr) || wantErr != nil {
			t.Fatalf("%s: through the production build: %q, %v; in this process: %q, %v", name, got, gotErr, want, wantErr)
		}
	}
	for _, c := range craftedFiles {
		var pages []string
		var err error
		within(t, childBoxed, func() { pages, err = s.extract(t.Context(), bytes.NewReader(c.pdf())) })
		if !errors.Is(err, ErrUnreadable) || pages != nil {
			t.Fatalf("%s: extract = %q, %v, want ErrUnreadable", c.name, pages, err)
		}
	}
	if !strings.Contains(logs.String(), "stack overflow") {
		t.Fatalf("the log of the dead children does not say why the first one died:\n%s", logs.String())
	}
}
