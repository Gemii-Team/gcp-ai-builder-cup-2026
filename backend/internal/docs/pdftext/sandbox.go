package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The PDF library cannot be trusted with a hostile file inside the server: on a few
// hundred crafted bytes it recurses until the Go runtime throws "stack overflow", asks
// for a hundred gigabytes, or loops for ever, inside pdf.NewReader or an object lookup,
// before any check of ours can run, and none of those can be recovered from. So the
// file is read by a child process, the binary run again with childEnv in its
// environment (see child.go), and the parent keeps what the child prints. What the
// child dies of costs the file, as ErrUnreadable, and nothing else.
//
// A child gets the file on its standard input and an empty environment (the server's
// secrets stay with the server), runs in a scratch directory under CPU and memory
// limits, and is killed at a deadline or when the caller gives up. At most
// maxConcurrent children run at once, so uploads cannot fork the host to death.

const (
	// childEnv marks a process as the child of the sandbox; its value is the mode.
	childEnv = "KIWI_PDFTEXT_CHILD"

	modeExtract = "extract"
	modeCount   = "count"

	// What a child prints: frameStarted as soon as it is confined, then one result
	// frame, then it exits.
	frameStarted = 'S'
	framePages   = 'P' // a uvarint count, then each page as a uvarint length and its bytes
	frameCount   = 'N' // a uvarint count
	frameError   = 'E' // the reason, as text

	// maxConcurrent is how many children run at once.
	maxConcurrent = 2
	// maxStderr is how much of a child's standard error is kept for the log.
	maxStderr = 2 << 10
	// outputSlack is what a result may take beyond its text: the lengths of the pages.
	outputSlack = 256 << 10
	// killGrace is how long a killed child's pipes are waited for.
	killGrace = 2 * time.Second
)

// sandbox runs extractions in child processes.
type sandbox struct {
	// exe is the program to run as the child; empty means this one.
	exe string
	// timeout is how long a child may run: a little more than the extraction's own
	// budget, which ends it cleanly first unless the PDF library is stuck.
	timeout time.Duration
	// slots holds one token per running child.
	slots chan struct{}
	// observe, when set, is told +1 as a child starts and -1 as it is done with.
	observe func(delta int)
	lim     limits
}

var defaultSandbox = &sandbox{
	timeout: defaultLimits.duration + 5*time.Second,
	slots:   make(chan struct{}, maxConcurrent),
	lim:     defaultLimits,
}

// Extract returns the text of every page, in page order: page n is pages[n-1]. Lines
// are separated by "\n" and words by one space; a page without a text layer is "".
// The number of pages is the number actually found in the page tree, whatever the
// document declares. Text inside form XObjects is not read. A file that cannot be read,
// or that crashed or outlasted the process that read it, is ErrUnreadable.
func Extract(r io.Reader) (pages []string, err error) {
	return ExtractContext(context.Background(), r)
}

// ExtractContext is Extract that stops when ctx is done and returns its error. An
// extraction that outlasts its own time budget is ErrUnreadable instead.
func ExtractContext(ctx context.Context, r io.Reader) ([]string, error) {
	return defaultSandbox.extract(ctx, r)
}

// PageCount returns the number of pages in the page tree.
func PageCount(r io.Reader) (int, error) {
	return PageCountContext(context.Background(), r)
}

// PageCountContext is PageCount that stops when ctx is done.
func PageCountContext(ctx context.Context, r io.Reader) (int, error) {
	return defaultSandbox.count(ctx, r)
}

func (s *sandbox) extract(ctx context.Context, r io.Reader) ([]string, error) {
	res, err := s.run(ctx, modeExtract, r)
	if err != nil {
		return nil, err
	}
	return s.pages(res)
}

func (s *sandbox) count(ctx context.Context, r io.Reader) (int, error) {
	res, err := s.run(ctx, modeCount, r)
	if err != nil {
		return 0, err
	}
	return s.pageCount(res)
}

// run reads the PDF from r, has a child process work on it and returns its result
// frame. An error that is ErrUnreadable is the file's: the child said so, or died of
// it, or ran out of time. Any other error is ours or the host's (the program could not
// be started, the caller gave up) and says nothing about the file.
func (s *sandbox) run(ctx context.Context, mode string, r io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pdftext: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(r, s.lim.input+1))
	if err != nil {
		return nil, fmt.Errorf("pdftext: read: %w", err)
	}
	if int64(len(data)) > s.lim.input {
		return nil, limitError("the file is over the size limit")
	}
	exe, err := s.program()
	if err != nil {
		return nil, err
	}

	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()

	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	stdout := &capped{max: s.lim.totalText + outputSlack, cancel: cancel}
	stderr := &capped{max: maxStderr, quiet: true}
	cmd := s.command(runCtx, exe, mode, data)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()

	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("pdftext: %w", ctx.Err())
	case stdout.over:
		return nil, limitError("its text is larger than the limit")
	case !bytes.HasPrefix(stdout.buf.Bytes(), []byte{frameStarted}):
		// It died before it began (the program would not start, or could not confine
		// itself): that is not the file's doing.
		return nil, fmt.Errorf("pdftext: the reader process did not start: %v: %s", runErr, strings.TrimSpace(stderr.buf.String()))
	case runCtx.Err() != nil:
		return nil, limitError("reading it took too long")
	case runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay):
		slog.WarnContext(ctx, "pdftext: the reader process died on a file", "error", runErr, "stderr", strings.TrimSpace(stderr.buf.String()))
		return nil, limitError("the process that read it stopped: " + runErr.Error())
	}
	return stdout.buf.Bytes()[1:], nil
}

// program is the executable to run as the child.
func (s *sandbox) program() (string, error) {
	if s.exe != "" {
		return s.exe, nil
	}
	// This process's own image, even if its file has been replaced since it started,
	// which is what a rebuilt container does.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/proc/self/exe"); err == nil {
			return "/proc/self/exe", nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("pdftext: locate the executable to run the reader in: %w", err)
	}
	return exe, nil
}

// command builds the child: the file on its standard input, nothing in its
// environment but the marker (and a bound on its threads), and a scratch directory to
// stand in.
func (s *sandbox) command(ctx context.Context, exe, mode string, data []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = []string{childEnv + "=" + mode, "GOMAXPROCS=2"}
	if raceBuild {
		// The race detector sleeps a second before a process exits; a test binary that
		// runs a child per file would spend its time asleep.
		cmd.Env = append(cmd.Env, "GORACE=atexit_sleep_ms=0")
	}
	cmd.Dir = scratchDir()
	cmd.Stdin = bytes.NewReader(data)
	cmd.WaitDelay = killGrace
	return cmd
}

// scratchDir is where a child runs: the temporary directory if there is one, else the
// root. It writes nothing there.
func scratchDir() string {
	if dir := os.TempDir(); dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return string(os.PathSeparator)
}

func (s *sandbox) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		if s.observe != nil {
			s.observe(1)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pdftext: %w", ctx.Err())
	}
}

func (s *sandbox) release() {
	if s.observe != nil {
		s.observe(-1)
	}
	<-s.slots
}

// capped is a Writer that keeps at most max bytes. Over the limit it either fails the
// write and cancels the child (the default), or, when quiet, drops the rest.
type capped struct {
	buf    bytes.Buffer
	max    int
	quiet  bool
	over   bool
	cancel func()
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	if c.quiet {
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	c.over = true
	if c.cancel != nil {
		c.cancel()
	}
	return 0, errors.New("pdftext: the reader process printed more than it may")
}

// pages decodes the result frame of an extraction.
func (s *sandbox) pages(res []byte) ([]string, error) {
	kind, body, err := frameOf(res)
	if err != nil {
		return nil, err
	}
	switch kind {
	case frameError:
		return nil, limitError(string(body))
	case framePages:
		n, rest, err := uvarint(body)
		if err != nil || n > uint64(s.lim.pages) {
			return nil, errBadFrame
		}
		pages := make([]string, 0, n)
		for range n {
			var size uint64
			if size, rest, err = uvarint(rest); err != nil || size > uint64(len(rest)) {
				return nil, errBadFrame
			}
			pages = append(pages, string(rest[:size]))
			rest = rest[size:]
		}
		if len(rest) != 0 {
			return nil, errBadFrame
		}
		return pages, nil
	}
	return nil, errBadFrame
}

// pageCount decodes the result frame of a page count.
func (s *sandbox) pageCount(res []byte) (int, error) {
	kind, body, err := frameOf(res)
	if err != nil {
		return 0, err
	}
	switch kind {
	case frameError:
		return 0, limitError(string(body))
	case frameCount:
		n, rest, err := uvarint(body)
		if err != nil || len(rest) != 0 || n > uint64(s.lim.pages) {
			return 0, errBadFrame
		}
		return int(n), nil
	}
	return 0, errBadFrame
}

// errBadFrame is a result that is not one a child of this program prints: a bug, not a
// verdict on the file.
var errBadFrame = errors.New("pdftext: the reader process printed something that is not a result")

func frameOf(res []byte) (kind byte, body []byte, err error) {
	if len(res) == 0 {
		return 0, nil, errBadFrame
	}
	return res[0], res[1:], nil
}

func uvarint(b []byte) (uint64, []byte, error) {
	var n uint64
	for i, c := range b {
		if i == 10 {
			break
		}
		n |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return n, b[i+1:], nil
		}
	}
	return 0, nil, errBadFrame
}
