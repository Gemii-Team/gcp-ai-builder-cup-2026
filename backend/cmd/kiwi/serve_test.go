package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
