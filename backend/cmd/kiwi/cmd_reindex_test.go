package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/testkit/dbtest"
)

func TestReindexIsRegistered(t *testing.T) {
	if commands["reindex"] == nil {
		t.Fatal("reindex is not registered")
	}
}

func TestReindex_TakesNoArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"reindex", "--all"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "takes no arguments"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not contain %q", stderr.String(), want)
	}
}

func TestReindex_MissingConfigurationNamesTheVariable(t *testing.T) {
	t.Setenv("KIWI_EMBED_MODEL", "")

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"reindex"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "KIWI_EMBED_MODEL"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not name %s", stderr.String(), want)
	}
}

// TestReindex_ReembedsChunksOfAnotherModelThroughTheConfiguredAdapter runs the command
// as `task reindex` does, on the environment of the pod: the real openai adapter against
// a fake embeddings endpoint, the real database and the system pool.
func TestReindex_ReembedsChunksOfAnotherModelThroughTheConfiguredAdapter(t *testing.T) {
	quiet := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // the command logs what it did
	t.Cleanup(func() { slog.SetDefault(quiet) })
	env := dbtest.Start(t)
	ws, src := uuid.New(), uuid.New()
	asOwner(t, env.OwnerURL, ws, func(ctx context.Context, tx pgx.Tx) {
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, 'acme', 'acme.example')`, []any{ws}},
			{`INSERT INTO knowledge.chunks (id, workspace_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
				VALUES ($1, $2, 'doc', $3, 'page:1', 'en', 'the budget was approved', 'sha', to_tsvector('simple', 'the budget was approved'),
				        'old-model', array_fill(1::real, ARRAY[768])::vector)`, []any{uuid.New(), ws, src}},
		} {
			if _, err := tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
				t.Fatal(err)
			}
		}
	})

	var requests atomic.Int32
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if r.URL.Path != "/v1/embeddings" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Model != "new-model" || len(in.Input) != 1 ||
			!strings.HasSuffix(in.Input[0], "the budget was approved") {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		vec := make([]float32, 768)
		for i := range vec {
			vec[i] = 0.25
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vec}}})
	}))
	t.Cleanup(embeddings.Close)

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for k, v := range map[string]string{
		"KIWI_ENV":                 "local",
		"KIWI_ROLES":               "",
		"KIWI_PUBLIC_URL":          "http://localhost:8080",
		"KIWI_DATABASE_URL":        env.AppURL,
		"KIWI_DATABASE_SYSTEM_URL": env.SystemURL,
		"KIWI_VALKEY_URL":          "redis://127.0.0.1:1",
		"KIWI_BUS_PROVIDER":        "memory",
		"KIWI_BLOB_URL":            "file:///tmp/kiwi-reindex-test",
		"KIWI_BLOB_SIGNING_KEY":    key,
		"KIWI_TOKEN_ENC_KEY":       key,
		"KIWI_LLM_PROVIDER":        "openai",
		"KIWI_LLM_BASE_URL":        embeddings.URL,
		"KIWI_LLM_API_KEY":         "not-used",
		"KIWI_LLM_MODEL_FLASH":     "flash",
		"KIWI_LLM_MODEL_PRO":       "pro",
		"KIWI_EMBED_BASE_URL":      embeddings.URL,
		"KIWI_EMBED_MODEL":         "new-model",
		"KIWI_ASR_MODEL":           "asr",
		"KIWI_OAUTH_CLIENT_ID":     "id",
		"KIWI_OAUTH_CLIENT_SECRET": "secret",
	} {
		t.Setenv(k, v)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		var stderr bytes.Buffer
		if code := run(context.Background(), []string{"reindex"}, &stderr); code != 0 {
			t.Fatalf("run %d: exit code = %d, stderr: %s", attempt, code, stderr.String())
		}
	}

	var model, vector string
	asOwner(t, env.OwnerURL, ws, func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT embedding_model, embedding::text FROM knowledge.chunks WHERE source_id = $1`, src).Scan(&model, &vector); err != nil {
			t.Fatal(err)
		}
	})
	if model != "new-model" || !strings.HasPrefix(vector, "[0.25,0.25,") {
		t.Errorf("chunk is on model %q with vector %.20s..., want new-model and the endpoint's vector", model, vector)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("%d embedding requests over two runs, want 1: the second run finds nothing on the old model", got)
	}
}

// asOwner runs fn as kiwi_owner in a transaction bound to ws, which is how a test
// writes tenant rows without the application's own code.
func asOwner(t *testing.T, ownerURL string, ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
		t.Fatal(err)
	}
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
