package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"kiwi/internal/chat"
	"kiwi/internal/identity"
	"kiwi/internal/knowledge"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/llm/llmfactory"
	"kiwi/internal/testkit/containers"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakellm"
)

func TestSeedIsRegistered(t *testing.T) {
	if commands["seed"] == nil {
		t.Fatal("seed is not registered")
	}
}

func TestSeed_TakesNoArgumentsAndKnowsItsFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"an argument":  {"seed", "everything"},
		"unknown flag": {"seed", "--all"},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 1 {
			t.Errorf("%s: exit code = %d, want 1", name, code)
		}
	}
	var stderr bytes.Buffer
	run(context.Background(), []string{"seed", "--all"}, &stderr)
	if want := "flag provided but not defined: -all"; !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "-members") {
		t.Errorf("stderr %q does not say %q and list the flags", stderr.String(), want)
	}
	// Asking for help is not a failure and needs no configuration; the usage goes to stdout.
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := run(context.Background(), []string{"seed", "-h"}, io.Discard)
	os.Stdout = stdout
	_ = w.Close()
	usage, _ := io.ReadAll(r)
	if code != 0 || !strings.Contains(string(usage), "-members") || !strings.Contains(string(usage), "alice@demo.example,bob@demo.example") {
		t.Errorf("seed -h: exit code = %d and usage %q, want 0 and the flags with their defaults", code, usage)
	}
}

func TestSeed_MissingConfigurationNamesTheVariable(t *testing.T) {
	t.Setenv("KIWI_DATABASE_URL", "")

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"seed"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "KIWI_DATABASE_URL"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not name %s", stderr.String(), want)
	}
}

// startValkey runs a Valkey for the test and returns its URL.
func startValkey(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := containers.Start(ctx, "valkey/valkey:8",
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("valkey-server", "--save", "", "--appendonly", "no"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		t.Fatalf("start valkey (is the Docker daemon reachable?): %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return "redis://" + host + ":" + port.Port()
}

// fakeModel serves what the seed asks the model for, in the shape of the OpenAI API the
// configured adapter speaks: a summary of the document and embeddings.
func fakeModel(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		content := `{"summary":"A synthetic note on the Bang Na warehouse.","key_points":["85% utilisation"]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content},
		}}})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data := make([]any, len(in.Input))
		for i, text := range in.Input {
			data[i] = map[string]any{"index": i, "embedding": fakellm.Vector(text)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seedPod sets the environment of the pod `task seed` runs in: the real Postgres and
// Valkey, a blob directory, and the fake model.
func seedPod(t *testing.T) *dbtest.Env {
	t.Helper()
	quiet := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // the command logs what it did
	t.Cleanup(func() { slog.SetDefault(quiet) })
	env := dbtest.Start(t)
	valkey := startValkey(t)
	model := fakeModel(t)

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	for k, v := range map[string]string{
		"KIWI_ENV":                 "local",
		"KIWI_ROLES":               "",
		"KIWI_PUBLIC_URL":          "http://localhost:8080",
		"KIWI_DATABASE_URL":        env.AppURL,
		"KIWI_DATABASE_SYSTEM_URL": env.SystemURL,
		"KIWI_VALKEY_URL":          valkey,
		"KIWI_BUS_PROVIDER":        "memory",
		"KIWI_BLOB_URL":            "file://" + t.TempDir(),
		"KIWI_BLOB_SIGNING_KEY":    key,
		"KIWI_TOKEN_ENC_KEY":       key,
		"KIWI_LLM_PROVIDER":        "openai",
		"KIWI_LLM_BASE_URL":        model.URL,
		"KIWI_LLM_API_KEY":         "not-used",
		"KIWI_LLM_MODEL_FLASH":     "flash",
		"KIWI_LLM_MODEL_PRO":       "pro",
		"KIWI_EMBED_BASE_URL":      model.URL,
		"KIWI_EMBED_MODEL":         "test-embed",
		"KIWI_ASR_MODEL":           "asr",
		"KIWI_OAUTH_CLIENT_ID":     "id",
		"KIWI_OAUTH_CLIENT_SECRET": "secret",
	} {
		t.Setenv(k, v)
	}
	return env
}

// noDomain is how seededUsers lists the workspace without a domain.
const noDomain = "(no domain)"

// seededUsers lists the domain of every workspace and the email of every user, through
// the system pool; the workspace without a domain is listed as noDomain.
func seededUsers(t *testing.T, env *dbtest.Env) (domains, emails []string) {
	t.Helper()
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, env.SystemURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for query, into := range map[string]*[]string{
		`SELECT coalesce(google_domain, '` + noDomain + `') FROM identity.workspaces ORDER BY 1`: &domains,
		`SELECT email FROM identity.users ORDER BY 1`:                                            &emails,
	} {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if *into, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
	}
	return domains, emails
}

// TestSeed_DefaultsSeedTheMockUsersAndKeepHRPrivate runs `kiwi seed` with no flags, as
// `task seed` does under the mock sign-in: the workspace is demo.example with exactly
// alice and bob, alice being the first, and bob cannot retrieve what is said in
// #hr-private, which alice can.
func TestSeed_DefaultsSeedTheMockUsersAndKeepHRPrivate(t *testing.T) {
	env := seedPod(t)

	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"seed"}, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}

	domains, emails := seededUsers(t, env)
	if !slices.Equal(domains, []string{"demo.example"}) || !slices.Equal(emails, []string{"alice@demo.example", "bob@demo.example"}) {
		t.Fatalf("seeded %v with users %v, want demo.example with exactly alice and bob", domains, emails)
	}

	cfg, err := config.Load(nil, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	model, err := llmfactory.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	c, err := cache.Open(t.Context(), cfg.ValkeyURL, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ws, ids, err := identity.EnsureSeedWorkspace(t.Context(), d, "demo.example", []string{"alice@demo.example", "bob@demo.example"})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(chat.NewMembership(d, c))
	retr := knowledge.NewRetriever(knowledge.Deps{DB: d, LLM: model, Authz: az, Chat: chat.New(chat.Deps{DB: d, Authz: az, Cache: c})})
	search := func(who uuid.UUID, text string) []knowledge.Hit {
		t.Helper()
		hits, err := retr.Search(t.Context(), authz.Actor{Kind: authz.ActorUser, WorkspaceID: ws, UserID: who}, knowledge.Query{Text: text, Lang: "en", Limit: 100})
		if err != nil {
			t.Fatalf("Search(%q): %v", text, err)
		}
		return hits
	}
	mentions := func(hits []knowledge.Hit, phrase string) bool {
		return slices.ContainsFunc(hits, func(h knowledge.Hit) bool { return strings.Contains(h.Content, phrase) })
	}

	const private = "merit increase pool"
	if hits := search(ids[0], private); !mentions(hits, private) {
		t.Fatalf("alice, the first member, did not find #hr-private: %v", hits)
	}
	if hits := search(ids[1], private); mentions(hits, private) || mentions(hits, "performance review calibration") {
		t.Errorf("bob retrieved what is said in #hr-private: %v", hits)
	}
	if hits := search(ids[1], "Bang Na warehouse capacity"); !mentions(hits, "9,600 pallet positions") {
		t.Errorf("bob did not find the warehouse document in #q4-launch: %v", hits)
	}

	// Seeding again, as someone who ran `task seed` twice does, changes nothing.
	if code := run(t.Context(), []string{"seed"}, &stderr); code != 0 {
		t.Fatalf("second run: exit code = %d, stderr: %s", code, stderr.String())
	}
	if _, again := seededUsers(t, env); !slices.Equal(again, emails) {
		t.Errorf("users after the second run = %v, want %v", again, emails)
	}
}

// TestSeed_FlagsOverrideTheDefaults seeds the workspace and members that were asked for,
// and not the defaults.
func TestSeed_FlagsOverrideTheDefaults(t *testing.T) {
	env := seedPod(t)

	var stderr bytes.Buffer
	args := []string{"seed", "--domain", "Acme.Example", "--members", "you@acme.example, Ann@acme.example"}
	if code := run(t.Context(), args, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}

	domains, emails := seededUsers(t, env)
	if !slices.Equal(domains, []string{"acme.example"}) || !slices.Equal(emails, []string{"ann@acme.example", "you@acme.example"}) {
		t.Errorf("seeded %v with users %v, want acme.example with you and ann", domains, emails)
	}
}

// TestSeed_MembersWithoutDomainSeedTheWorkspaceWithoutOne is how the real Google sign-in
// is prepared when the team has no Workspace domain: `task seed -- --members <address>`.
// The one workspace has no domain, the member is a placeholder with the lowercased
// address, and running it again changes nothing. An empty --domain says the same thing.
func TestSeed_MembersWithoutDomainSeedTheWorkspaceWithoutOne(t *testing.T) {
	env := seedPod(t)

	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"seed", "--members", " Pat@Gmail.example "}, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}
	domains, emails := seededUsers(t, env)
	if !slices.Equal(domains, []string{noDomain}) || !slices.Equal(emails, []string{"pat@gmail.example"}) {
		t.Fatalf("seeded %v with users %v, want the workspace without a domain with pat only", domains, emails)
	}

	if code := run(t.Context(), []string{"seed", "--domain", "", "--members", "pat@gmail.example"}, &stderr); code != 0 {
		t.Fatalf("with --domain \"\": exit code = %d, stderr: %s", code, stderr.String())
	}
	if domains2, emails2 := seededUsers(t, env); !slices.Equal(domains2, domains) || !slices.Equal(emails2, emails) {
		t.Errorf("after a second run: %v with users %v, want %v with %v", domains2, emails2, domains, emails)
	}
}

func TestSeed_DomainFlagRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		domain string
		given  map[string]bool
		want   string
		err    string
	}{
		{"neither flag: the mock's workspace", defaultDomain, nil, defaultDomain, ""},
		{"only --members: the workspace without a domain", defaultDomain, map[string]bool{"members": true}, "", ""},
		{"--domain: that domain", "acme.example", map[string]bool{"domain": true}, "acme.example", ""},
		{"--domain and --members", "acme.example", map[string]bool{"domain": true, "members": true}, "acme.example", ""},
		{"an empty --domain with --members", "", map[string]bool{"domain": true, "members": true}, "", ""},
		{"a blank --domain with --members", "  ", map[string]bool{"domain": true, "members": true}, "", ""},
		{"an empty --domain alone", "", map[string]bool{"domain": true}, "", `--domain "" seeds the workspace without a domain and needs --members`},
	} {
		got, err := seedDomain(tc.domain, tc.given)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || strings.TrimSpace(got) != tc.want {
			t.Errorf("%s: domain %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// The flag rules are checked before anything is opened, so a wrong command line names its
// mistake without needing a database.
func TestSeed_FlagMistakesAreNamedBeforeAnythingOpens(t *testing.T) {
	t.Setenv("KIWI_DATABASE_URL", "")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"an empty --domain without --members": {[]string{"seed", "--domain", ""}, `--domain "" seeds the workspace without a domain and needs --members`},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), tc.args, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 1 and %q", name, code, stderr.String(), tc.want)
		}
	}
}

// A malformed member is refused by the seed with the entry named, and nothing is made.
func TestSeed_MalformedMembersAreRefusedWithTheEntryNamed(t *testing.T) {
	env := seedPod(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"not an address, with a domain":    {[]string{"seed", "--domain", "acme.example", "--members", "you@acme.example,not-an-address"}, `"not-an-address" is not an email address`},
		"not an address, without a domain": {[]string{"seed", "--members", "not-an-address"}, `"not-an-address" is not an email address`},
		"a display name":                   {[]string{"seed", "--members", "Pat <pat@gmail.example>"}, "pat <pat@gmail.example>"},
		"listed twice":                     {[]string{"seed", "--members", "pat@gmail.example,Pat@gmail.example"}, "member pat@gmail.example is listed twice"},
	} {
		var stderr bytes.Buffer
		if code := run(t.Context(), tc.args, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: exit code %d, stderr %q, want 1 and %q", name, code, stderr.String(), tc.want)
		}
	}
	if domains, emails := seededUsers(t, env); len(domains) != 0 || len(emails) != 0 {
		t.Errorf("refused commands made %v with users %v, want nothing", domains, emails)
	}
}
