package llmfactory_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/llm/llmfactory"
)

// exitWithin is how long `kiwi serve` may take to refuse a provider: the guard
// runs before anything is dialled, so it answers at once.
const exitWithin = 10 * time.Second

// buildKiwi compiles the kiwi binary, so that the timeout below measures the
// process and not the compiler.
func buildKiwi(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kiwi")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "kiwi/cmd/kiwi").CombinedOutput(); err != nil {
		t.Fatalf("go build kiwi/cmd/kiwi: %v\n%s", err, out)
	}
	return bin
}

// prodEnv is a complete production environment for every role, pointing at ports
// where nothing listens, with the given LLM provider. Nothing but the provider can
// be wrong with it: a connection error in the output would mean the process got as
// far as dialling.
func prodEnv(provider string) []string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	return []string{
		"KIWI_ENV=prod",
		"KIWI_PUBLIC_URL=https://kiwi.example",
		"KIWI_DATABASE_URL=postgres://kiwi_app:dummy@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_DATABASE_SYSTEM_URL=postgres://kiwi_system:dummy@127.0.0.1:1/kiwi?sslmode=disable",
		"KIWI_VALKEY_URL=redis://127.0.0.1:1",
		"KIWI_BUS_PROVIDER=memory",
		"KIWI_BLOB_URL=file:///nonexistent/blobs",
		"KIWI_BLOB_SIGNING_KEY=" + key,
		"KIWI_TOKEN_ENC_KEY=" + key,
		"KIWI_LLM_PROVIDER=" + provider,
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
}

func TestNew_ProdRequiresVertex_ExitsAtStartup(t *testing.T) {
	bin := buildKiwi(t)

	for _, tc := range []struct {
		name, provider, want string
	}{
		{"the openai provider", "openai", "KIWI_LLM_PROVIDER=vertex"},
		{"an unknown provider", "ollama", "KIWI_LLM_PROVIDER=vertex"},
		// vertex is what prod asks for, and this binary does not have it: it must not
		// serve either.
		{"vertex, which this binary does not build", "vertex", "not built in the local MVP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), exitWithin)
			defer cancel()
			var stderr bytes.Buffer
			cmd := exec.CommandContext(ctx, bin, "serve")
			cmd.Env = prodEnv(tc.provider) // nothing of the calling shell leaks in
			cmd.Stderr = &stderr
			err := cmd.Run()

			out := stderr.String()
			if ctx.Err() != nil {
				t.Fatalf("kiwi serve was still running after %s: the prod guard did not stop it\nstderr: %s", exitWithin, out)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() == 0 {
				t.Fatalf("kiwi serve = %v, want a non-zero exit\nstderr: %s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, out)
			}
			// The guard runs first: no dial error precedes it. This is what the order of
			// config.Load and llmfactory.MustNew in serve means, and what L27 must keep
			// when serve starts opening Postgres and Valkey.
			for _, early := range []string{"connection refused", "dial tcp", "failed to connect", "valkey", "postgres"} {
				if strings.Contains(strings.ToLower(out), early) {
					t.Errorf("stderr mentions %q: the process went past the guard before it was refused:\n%s", early, out)
				}
			}
		})
	}
}

func TestNew_ProdWithoutVertexIsRefusedInProcess(t *testing.T) {
	for _, provider := range []string{"openai", "", "ollama"} {
		c, err := llmfactory.New(localConfig(t, "http://127.0.0.1:1", func(c *config.Config) {
			c.Env, c.LLMProvider = "prod", provider
		}))
		if err == nil || !strings.Contains(err.Error(), "KIWI_LLM_PROVIDER=vertex") {
			t.Errorf("New(prod, %q) = %v, %v, want an error naming KIWI_LLM_PROVIDER=vertex", provider, c, err)
		}
	}
}

// localConfig is the configuration of a local stack whose LiteLLM is at base.
func localConfig(t *testing.T, base string, edit ...func(*config.Config)) config.Config {
	t.Helper()
	cfg := config.Config{
		Env:           "local",
		LLMProvider:   "openai",
		LLMBaseURL:    base,
		LLMAPIKey:     "test-key",
		LLMModelFlash: "flash-model",
		LLMModelPro:   "pro-model",
		EmbedBaseURL:  base,
		EmbedModel:    "embeddinggemma",
		ASRModel:      "asr-model",
	}
	for _, f := range edit {
		f(&cfg)
	}
	return cfg
}

func TestNew_LocalAcceptsOpenAI(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// The wrapper retries a 503: proof the client is the retrying one.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"error":"busy"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"\n\nhello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)

	c, err := llmfactory.New(localConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("New(local, openai) = %v, want a client", err)
	}
	if got := c.EmbeddingModel(); got != "embeddinggemma" {
		t.Errorf("EmbeddingModel = %q, want the configured one", got)
	}
	res, err := c.Generate(t.Context(), llm.Request{Tier: llm.TierFlash, Parts: []llm.Part{{Text: "hi"}}})
	if err != nil || res.Text != "hello" {
		t.Fatalf("Generate = %+v, %v, want hello after one retry", res, err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("server calls = %d, want 2 (the 503, then the retry)", n)
	}
}

func TestNew_VertexAndUnknownProviderError(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"vertex", "not built in the local MVP"},
		{"anthropic", `unknown KIWI_LLM_PROVIDER "anthropic"`},
		{"", `unknown KIWI_LLM_PROVIDER ""`},
	} {
		c, err := llmfactory.New(localConfig(t, "http://127.0.0.1:1", func(c *config.Config) { c.LLMProvider = tc.provider }))
		if err == nil || !strings.Contains(err.Error(), tc.want) || c != nil {
			t.Errorf("New(local, %q) = %v, %v, want an error containing %q", tc.provider, c, err, tc.want)
		}
	}
}

func TestNew_OpenAIWithAnIncompleteConfigNamesTheVariable(t *testing.T) {
	_, err := llmfactory.New(localConfig(t, "", func(c *config.Config) { c.LLMBaseURL = "" }))
	if err == nil || !strings.Contains(err.Error(), "KIWI_LLM_BASE_URL") {
		t.Fatalf("New without a base URL = %v, want an error naming KIWI_LLM_BASE_URL", err)
	}
}
