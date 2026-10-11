package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exitWithin is how long `kiwi serve` may take to refuse a configuration: the
// guard runs before anything is dialled, so it answers at once.
const exitWithin = 10 * time.Second

// buildKiwi compiles this package's binary, so that the timeout below measures
// the process and not the compiler.
func buildKiwi(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kiwi")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/kiwi: %v\n%s", err, out)
	}
	return bin
}

// prodEnv is a complete production environment for every role, pointing at ports
// where nothing listens, plus extra. Nothing but the OIDC variables in extra is
// wrong with it: a connection error in the output would mean the process got as
// far as dialling.
func prodEnv(extra ...string) []string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	env := []string{
		"KIWI_ENV=prod",
		"KIWI_PUBLIC_URL=https://kiwi.example",
		"KIWI_DATABASE_URL=postgres://kiwi_app:dummy@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_DATABASE_SYSTEM_URL=postgres://kiwi_system:dummy@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_VALKEY_URL=redis://127.0.0.1:1",
		"KIWI_BUS_PROVIDER=memory",
		"KIWI_BLOB_URL=file:///nonexistent/blobs",
		"KIWI_BLOB_SIGNING_KEY=" + key,
		"KIWI_TOKEN_ENC_KEY=" + key,
		"KIWI_LLM_PROVIDER=vertex",
		"KIWI_LLM_BASE_URL=http://127.0.0.1:1",
		"KIWI_LLM_API_KEY=dummy",
		"KIWI_LLM_MODEL_FLASH=dummy",
		"KIWI_LLM_MODEL_PRO=dummy",
		"KIWI_EMBED_BASE_URL=http://127.0.0.1:1",
		"KIWI_EMBED_MODEL=dummy",
		"KIWI_ASR_MODEL=dummy",
		"KIWI_OAUTH_CLIENT_ID=dummy",
		"KIWI_OAUTH_CLIENT_SECRET=dummy",
	}
	return append(env, extra...)
}

func TestServe_ProdRejectsMockIssuer_ExitsAtStartup(t *testing.T) {
	bin := buildKiwi(t)

	for _, tc := range []struct {
		name  string
		extra []string
		// names is every variable the refusal must name.
		names []string
	}{
		{"the mock issuer", []string{"KIWI_OIDC_ISSUER=http://localhost:8080/mockoidc"}, []string{"KIWI_OIDC_ISSUER"}},
		{"only an internal discovery URL", []string{"KIWI_OIDC_INTERNAL_URL=http://mockoidc:8090/mockoidc"}, []string{"KIWI_OIDC_INTERNAL_URL"}},
		{"both", []string{"KIWI_OIDC_ISSUER=http://localhost:8080/mockoidc", "KIWI_OIDC_INTERNAL_URL=http://mockoidc:8090/mockoidc"}, []string{"KIWI_OIDC_ISSUER", "KIWI_OIDC_INTERNAL_URL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), exitWithin)
			defer cancel()
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, "serve")
			cmd.Env = prodEnv(tc.extra...) // nothing of the calling shell leaks in
			cmd.Stderr = &stderr
			err := cmd.Run()

			if ctx.Err() != nil {
				t.Fatalf("kiwi serve was still running after %s: the prod guard did not stop it\nstderr: %s", exitWithin, stderr.String())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() == 0 {
				t.Fatalf("kiwi serve = %v, want a non-zero exit\nstderr: %s", err, stderr.String())
			}
			out := stderr.String()
			for _, name := range tc.names {
				if !strings.Contains(out, name) {
					t.Errorf("stderr does not name %s:\n%s", name, out)
				}
			}
			if !strings.Contains(out, "KIWI_ENV=prod") {
				t.Errorf("stderr does not say that KIWI_ENV=prod is the reason:\n%s", out)
			}
			// The guard comes before any connection: no dial, no database, no cache.
			for _, early := range []string{"connection refused", "dial tcp", "failed to connect", "missing required environment variables", "valkey", "postgres"} {
				if strings.Contains(strings.ToLower(out), early) {
					t.Errorf("stderr mentions %q: the process went past configuration before it was refused:\n%s", early, out)
				}
			}
		})
	}
}
