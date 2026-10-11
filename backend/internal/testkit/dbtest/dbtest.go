// Package dbtest gives tests a real Postgres (pgvector/pgvector:pg17 under
// testcontainers) with the four K.I.W.I. roles and the migrated schema.
//
// One container serves a whole test package: the first Start boots it, creates
// the roles and builds two template databases (one empty, one migrated through
// migrate.Up as kiwi_owner). Every Start then clones a template with CREATE
// DATABASE ... TEMPLATE, a file copy that takes milliseconds, so each test owns
// a database nobody else touches and is dropped when the test ends.
//
// The container is removed by testcontainers' reaper when the test process
// exits; running with TESTCONTAINERS_RYUK_DISABLED=true leaks it.
package dbtest

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"kiwi/internal/migrate"
)

const (
	image = "pgvector/pgvector:pg17"

	superuser     = "postgres"
	superPassword = "postgres"

	baseTemplate     = "kiwi_template_base"
	migratedTemplate = "kiwi_template_migrated"
)

// Env holds one connection URL per role, all pointing at the same private
// database.
type Env struct {
	OwnerURL  string // kiwi_owner: owns the tables, runs goose
	AppURL    string // kiwi_app: what the services connect as; bound by RLS
	SystemURL string // kiwi_system: BYPASSRLS, the db.System pool
	ROURL     string // kiwi_ro: SELECT on chat.channels and chat.channel_members
}

// roles are created by the environment bootstrap, never by migrations: they
// carry passwords. This is the test copy of it; the local postgres-init and
// Terraform carry the real ones.
var roles = []struct {
	name, password, attrs string
}{
	{"kiwi_owner", "owner-pw", ""},
	{"kiwi_app", "app-pw", ""},
	{"kiwi_system", "system-pw", "BYPASSRLS"},
	{"kiwi_ro", "ro-pw", ""},
}

var (
	once    sync.Once
	srv     *server
	srvErr  error
	counter atomic.Int64
)

type server struct{ host, port string }

// Start returns the roles and a fresh, fully migrated database.
func Start(t testing.TB) *Env { return newEnv(t, migratedTemplate) }

// StartEmpty returns the roles and a fresh database with the vector extension
// but no schema and no goose history, for tests of the migrations themselves.
func StartEmpty(t testing.TB) *Env { return newEnv(t, baseTemplate) }

func newEnv(t testing.TB, template string) *Env {
	t.Helper()
	once.Do(func() { srv, srvErr = boot() })
	if srvErr != nil {
		t.Fatalf("dbtest: %v", srvErr)
	}

	name := fmt.Sprintf("kiwi_test_%d", counter.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.admin(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s OWNER kiwi_owner`, name, template)); err != nil {
		t.Fatalf("dbtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.admin(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
			t.Errorf("dbtest: drop database %s: %v", name, err)
		}
	})

	return &Env{
		OwnerURL:  srv.url("kiwi_owner", name),
		AppURL:    srv.url("kiwi_app", name),
		SystemURL: srv.url("kiwi_system", name),
		ROURL:     srv.url("kiwi_ro", name),
	}
}

// boot starts the container, creates the roles and builds both templates.
func boot() (*server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// fsync and friends off: the data is thrown away with the container.
	ctr, err := tcpostgres.Run(ctx, image,
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		tcpostgres.WithUsername(superuser),
		tcpostgres.WithPassword(superPassword),
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.BasicWaitStrategies(),
		testcontainers.WithCmd("postgres", "-c", "fsync=off", "-c", "synchronous_commit=off",
			"-c", "full_page_writes=off", "-c", "max_connections=300"),
	)
	if err != nil {
		return nil, fmt.Errorf("start %s (is the Docker daemon reachable?): %w", image, err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	s := &server{host: host, port: port.Port()}

	for _, r := range roles {
		stmt := fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' %s`, r.name, r.password, r.attrs)
		if err := s.admin(ctx, stmt); err != nil {
			return nil, fmt.Errorf("create role %s: %w", r.name, err)
		}
	}

	if err := s.admin(ctx, `CREATE DATABASE `+baseTemplate+` OWNER kiwi_owner`); err != nil {
		return nil, err
	}
	// pgvector is not a trusted extension, so a superuser installs it.
	if err := s.adminIn(ctx, baseTemplate, `CREATE EXTENSION vector`); err != nil {
		return nil, fmt.Errorf("create extension vector: %w", err)
	}
	if err := s.admin(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s OWNER kiwi_owner`, migratedTemplate, baseTemplate)); err != nil {
		return nil, err
	}
	if err := migrate.Up(ctx, s.url("kiwi_owner", migratedTemplate)); err != nil {
		return nil, err
	}
	for _, tpl := range []string{baseTemplate, migratedTemplate} {
		if err := s.admin(ctx, fmt.Sprintf(`ALTER DATABASE %s WITH IS_TEMPLATE true ALLOW_CONNECTIONS false`, tpl)); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *server) url(role, database string) string {
	var password string
	for _, r := range roles {
		if r.name == role {
			password = r.password
		}
	}
	return s.dsn(role, password, database)
}

func (s *server) dsn(user, password, database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     s.host + ":" + s.port,
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func (s *server) admin(ctx context.Context, sql string) error { return s.adminIn(ctx, "postgres", sql) }

func (s *server) adminIn(ctx context.Context, database, sql string) error {
	conn, err := pgx.Connect(ctx, s.dsn(superuser, superPassword, database))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

// SeedTwoWorkspaces writes two tenants through kiwi_system and returns their
// ids. Each gets one user (the channel's owner), channel, message, task, file,
// meeting, chunk and agent run, so isolation tests have something to see
// and something to hide. REFs are zero-numbered (MTG-0001) and never collide
// with refs.Next, which starts at 1001.
func SeedTwoWorkspaces(t testing.TB, env *Env) (a, b uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, env.SystemURL)
	if err != nil {
		t.Fatalf("dbtest: connect as kiwi_system: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	a, b = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, ws := range []struct {
		id     uuid.UUID
		domain string
	}{{a, "a.example"}, {b, "b.example"}} {
		if err := seedWorkspace(ctx, conn, ws.id, ws.domain); err != nil {
			t.Fatalf("dbtest: seed workspace %s: %v", ws.domain, err)
		}
	}
	return a, b
}

func seedWorkspace(ctx context.Context, conn *pgx.Conn, ws uuid.UUID, domain string) error {
	v7 := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	user, channel, meeting := v7(), v7(), v7()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	steps := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $3)`,
			[]any{ws, "Workspace " + domain, domain}},
		{`INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, $3, $4, $5)`,
			[]any{user, ws, "sub-" + domain, "owner@" + domain, "Owner of " + domain}},
		{`INSERT INTO chat.channels (id, workspace_id, name, kind, last_seq) VALUES ($1, $2, 'general', 'public', 1)`,
			[]any{channel, ws}},
		{`INSERT INTO chat.channel_members (channel_id, user_id, workspace_id, role) VALUES ($1, $2, $3, 'owner')`,
			[]any{channel, user, ws}},
		{`INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id)
		  VALUES ($1, $2, $3, 1, 'user', $4, 'hello', 'seed-1')`,
			[]any{v7(), ws, channel, user}},
		{`INSERT INTO tasks.tasks (id, workspace_id, channel_id, ref, title, state, status, position, created_by_kind, created_by_id)
		  VALUES ($1, $2, $3, 'TSK-0001', 'Seeded task', 'confirmed', 'todo', 'a0', 'user', $4)`,
			[]any{v7(), ws, channel, user}},
		{`INSERT INTO files.files (id, workspace_id, channel_id, uploaded_by, ref, name, blob_key, mime, size, sha256, status)
		  VALUES ($1, $2, $3, $4, 'DOC-0001', 'seed.txt', 'uploads/seed', 'text/plain', 4, 'sha', 'ready')`,
			[]any{v7(), ws, channel, user}},
		{`INSERT INTO meetings.meetings (id, workspace_id, channel_id, owner_user_id, ref, title, status)
		  VALUES ($1, $2, $3, $4, 'MTG-0001', 'Seeded meeting', 'ready')`,
			[]any{meeting, ws, channel, user}},
		{`INSERT INTO knowledge.chunks
		    (id, workspace_id, channel_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
		  VALUES ($1, $2, $3, 'meeting', $4, 'entries:0-0', 'en', 'seeded chunk', 'sha', to_tsvector('simple', 'seeded chunk'),
		          'embeddinggemma', array_fill(0.1::real, ARRAY[768])::vector)`,
			[]any{v7(), ws, channel, meeting}},
		{`INSERT INTO agents.runs (id, workspace_id, agent, on_behalf_of, status, ref)
		  VALUES ($1, $2, 'assistant', $3, 'done', 'ANS-0001')`,
			[]any{v7(), ws, user}},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("%s: %w", s.sql, err)
		}
	}
	return tx.Commit(ctx)
}
