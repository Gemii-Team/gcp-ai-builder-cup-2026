package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kiwi/internal/testkit/fakeoidc/fakeoidctest"
)

// localEnv is a complete local environment for every role whose Valkey is a port
// where nothing listens, with a distinctive value for every secret, so that a test can
// look for them in what the process prints.
func localEnv() []string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	return []string{
		"KIWI_ENV=local",
		"KIWI_PUBLIC_URL=http://localhost:8080",
		"KIWI_DATABASE_URL=postgres://kiwi_app:never-print-app-pw@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_DATABASE_SYSTEM_URL=postgres://kiwi_system:never-print-system-pw@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_VALKEY_URL=redis://:never-print-valkey-pw@127.0.0.1:1",
		"KIWI_BUS_PROVIDER=memory",
		"KIWI_BLOB_URL=file:///nonexistent/never-print-blob-dir",
		"KIWI_BLOB_SIGNING_KEY=" + key,
		"KIWI_TOKEN_ENC_KEY=" + key,
		"KIWI_LLM_PROVIDER=openai",
		"KIWI_LLM_BASE_URL=http://never-print-llm-host.invalid",
		"KIWI_LLM_API_KEY=never-print-the-llm-credential",
		"KIWI_LLM_MODEL_FLASH=flash",
		"KIWI_LLM_MODEL_PRO=pro",
		"KIWI_EMBED_BASE_URL=http://127.0.0.1:1",
		"KIWI_EMBED_MODEL=embed",
		"KIWI_ASR_MODEL=asr",
		"KIWI_OAUTH_CLIENT_ID=client-id",
		"KIWI_OAUTH_CLIENT_SECRET=never-print-oauth-secret",
	}
}

// secrets are the values of localEnv that no output of the process may carry.
var secrets = []string{
	"never-print-app-pw", "never-print-system-pw", "never-print-valkey-pw", "never-print-blob-dir",
	"never-print-llm-host", "never-print-the-llm-credential", "never-print-oauth-secret",
	base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
}

// TestServe_UnreachableValkey_FailsNamingItAndPrintsNoSecret is the control of the
// prod-guard test: it shows what `serve` prints when it does dial, so that the absence
// of those words from the refusals of that test means the process stopped before it
// dialled. It also shows that the configuration logged on the way is redacted.
func TestServe_UnreachableValkey_FailsNamingItAndPrintsNoSecret(t *testing.T) {
	bin := buildKiwi(t)
	ctx, cancel := context.WithTimeout(context.Background(), exitWithin)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = localEnv() // nothing of the calling shell leaks in
	cmd.Stderr = &stderr
	err := cmd.Run()

	if ctx.Err() != nil {
		t.Fatalf("kiwi serve was still running after %s with Valkey unreachable\nstderr: %s", exitWithin, stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("kiwi serve = %v, want exit status 1\nstderr: %s", err, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"KIWI_VALKEY_URL", "connection refused", `"[REDACTED]"`} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr does not contain %q:\n%s", want, out)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("stderr shows the secret %q:\n%s", secret, out)
		}
	}
}

// TestServe_BuiltBinaryAnswersTheChildOfThePDFSandbox: package pdftext reads a PDF in a
// child process, which is this same binary started with KIWI_PDFTEXT_CHILD in its
// environment (an init hook of the package answers it before main runs). The hook only
// exists in a binary that links docs, which is the one this task makes; the child of the
// sandbox is /proc/self/exe, so a binary without it would fail every PDF upload.
func TestServe_BuiltBinaryAnswersTheChildOfThePDFSandbox(t *testing.T) {
	bin := buildKiwi(t)
	pdf, err := os.ReadFile(filepath.Join("..", "..", "internal", "docs", "pdftext", "testdata", "flate-truetype.pdf"))
	if err != nil {
		t.Fatalf("read the fixture PDF: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), exitWithin)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = []string{"KIWI_PDFTEXT_CHILD=count", "GOMAXPROCS=2"} // what the sandbox gives its child
	cmd.Stdin = bytes.NewReader(pdf)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the built binary as a PDF child: %v\nstderr: %s", err, stderr.String())
	}

	// The child prints 'S' once it is confined and then one frame: 'N' and a uvarint count.
	out := stdout.Bytes()
	if len(out) < 3 || out[0] != 'S' || out[1] != 'N' {
		t.Fatalf("child output = %q, want the frames S then N<count>", out)
	}
	if pages, n := binary.Uvarint(out[2:]); n <= 0 || pages < 1 {
		t.Fatalf("child counted %d pages (decoded %d bytes) in %q, want at least one", pages, n, out)
	}
}

// TestManifestsNeverSetThePDFChildVariable: a pod with KIWI_PDFTEXT_CHILD in its
// environment would not start the server at all, it would wait for a PDF on its standard
// input. Nothing under deploy/ may mention it.
func TestManifestsNeverSetThePDFChildVariable(t *testing.T) {
	root := filepath.Join("..", "..", "..", "deploy")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("KIWI_PDFTEXT_CHILD")) {
			t.Errorf("%s sets KIWI_PDFTEXT_CHILD: the server would run as a PDF child", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// TestServe_MemoryCache_ListensOnPORTAndServesTheWebAppWithoutValkey runs the built binary
// the way the all-in-one image does on Cloud Run: PORT names the port, KIWI_WEB_DIR the
// web app, the cache is the process's own and there is no Valkey address at all. The
// database is a port where nothing listens, so the process is alive and serves the app
// but is not ready, which is what the platform's probes should see.
func TestServe_MemoryCache_ListensOnPORTAndServesTheWebAppWithoutValkey(t *testing.T) {
	bin := buildKiwi(t)
	idp := fakeoidctest.New(t)
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<title>shell of the all-in-one image</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()

	env := []string{}
	for _, kv := range localEnv() {
		if !strings.HasPrefix(kv, "KIWI_VALKEY_URL=") && !strings.HasPrefix(kv, "KIWI_BLOB_URL=") &&
			!strings.HasPrefix(kv, "KIWI_OAUTH_CLIENT_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"KIWI_CACHE_PROVIDER=memory",
		"KIWI_WEB_DIR="+web,
		"KIWI_BLOB_URL=file://"+t.TempDir(),
		"KIWI_OIDC_ISSUER="+idp.Issuer(),
		"KIWI_OAUTH_CLIENT_ID="+idp.ClientID(),
		"KIWI_OAUTH_CLIENT_SECRET="+idp.ClientSecret(),
		"PORT="+port,
	)

	var stderr lockedBuffer // the process writes while the test reads
	cmd := exec.Command(bin, "serve")
	cmd.Env = env
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	get := func(path string) (int, string) {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://127.0.0.1:" + port + path)
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-exited:
			t.Fatalf("kiwi serve exited early (%v)\nstderr: %s", err, stderr.String())
		default:
		}
		if code, _ := get("/healthz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing answered /healthz on PORT=%s within 60 s\nstderr: %s", port, stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	if code, body := get("/"); code != http.StatusOK || !strings.Contains(body, "shell of the all-in-one image") {
		t.Errorf("GET / = %d %q, want the web app from KIWI_WEB_DIR", code, body)
	}
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d, want 503: the database is unreachable", code)
	}
	if code, _ := get("/api/auth/config"); code != http.StatusOK {
		t.Errorf("GET /api/auth/config = %d, want 200: the API is on the same port", code)
	}
	for _, secret := range secrets {
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("stderr shows the secret %q", secret)
		}
	}
	if strings.Contains(stderr.String(), "KIWI_VALKEY_URL") {
		t.Errorf("the process mentioned KIWI_VALKEY_URL with the memory cache:\n%s", stderr.String())
	}
}

// lockedBuffer is a bytes.Buffer that may be written by the process under test while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
