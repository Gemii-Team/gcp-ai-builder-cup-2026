package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/testkit/dbtest"
	"kiwi/internal/testkit/fakeoidc"
)

// The Postgres session store is what keeps people signed in across a redeploy of the
// minimal profile, and the one place a session credential is stored outside Valkey.
// These tests run it on a real database as the roles of production: kiwi_system through
// db.System, and the owner to look at what was written.

type sessionClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *sessionClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sessionClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sessionFixture struct {
	t     *testing.T
	env   *dbtest.Env
	db    *db.DB
	store *Sessions
	clock *sessionClock
	ws    uuid.UUID
	user  uuid.UUID
}

// newSessionFixture opens a migrated database with one workspace and one user, and a
// store whose clock the test moves. A zero opt takes the real defaults.
func newSessionFixture(t *testing.T, opt cache.Options) *sessionFixture {
	t.Helper()
	env := dbtest.Start(t)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)

	f := &sessionFixture{t: t, env: env, db: d, ws: uuid.Must(uuid.NewV7()), user: uuid.Must(uuid.NewV7())}
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, 'ws', 'ws.example')`, f.ws); err != nil {
			t.Fatalf("seed workspace: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity.users (id, workspace_id, google_sub, email, name) VALUES ($1, $2, 'sub', 'u@ws.example', 'U')`, f.user, f.ws); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	})
	f.store = NewSessions(d, opt)
	f.clock = &sessionClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	f.store.now = f.clock.now
	return f
}

// asOwner runs fn as kiwi_owner in a committed transaction bound to the fixture's workspace.
func (f *sessionFixture) asOwner(fn func(ctx context.Context, tx pgx.Tx)) {
	f.t.Helper()
	ctx := f.t.Context()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, f.ws.String()); err != nil {
		f.t.Fatal(err)
	}
	fn(ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sessionFixture) record() cache.SessionRecord {
	return cache.SessionRecord{UserID: f.user, WorkspaceID: f.ws}
}

// expiry is expires_at of the session stored under id.
func (f *sessionFixture) expiry(id string) time.Time {
	f.t.Helper()
	var at time.Time
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		err := tx.QueryRow(ctx, `SELECT expires_at FROM identity.sessions WHERE id_hash = $1`, sessionHash(id)).Scan(&at)
		if err != nil {
			f.t.Fatalf("expires_at of %q: %v", id, err)
		}
	})
	return at
}

func (f *sessionFixture) rows() int {
	f.t.Helper()
	var n int
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity.sessions`).Scan(&n); err != nil {
			f.t.Fatal(err)
		}
	})
	return n
}

// newID is a session id of the shape newSessionID makes: 256 random bits as base64url.
func newID() string {
	id, err := newSessionID()
	if err != nil {
		panic(err)
	}
	return id
}

// A session slides forward on every read, but never past the absolute lifetime from its
// creation: the behaviour of the Valkey store, run on a clock the test moves.
func TestSessions_SlidingExtendsAndAbsoluteCaps(t *testing.T) {
	f := newSessionFixture(t, cache.Options{SessionSliding: time.Hour, SessionAbsolute: 3 * time.Hour})
	ctx := t.Context()
	start := f.clock.now()
	id := newID()

	if err := f.store.PutSession(ctx, id, f.record()); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	if got := f.expiry(id); !got.Equal(start.Add(time.Hour)) {
		t.Fatalf("a new session expires at %v, want the sliding lifetime from now (%v)", got, start.Add(time.Hour))
	}

	f.clock.advance(30 * time.Minute)
	got, err := f.store.GetSession(ctx, id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != f.user || got.WorkspaceID != f.ws || !got.CreatedAt.Equal(start) {
		t.Fatalf("GetSession = %+v, want the user and workspace it was put with, created at %v", got, start)
	}
	if e := f.expiry(id); !e.Equal(start.Add(90 * time.Minute)) {
		t.Errorf("after a read at +30m the session expires at %v, want +90m: a read slides it", e)
	}

	// Left alone for the whole sliding lifetime, it is gone.
	f.clock.advance(time.Hour) // +90m, exactly the expiry
	if _, err := f.store.GetSession(ctx, id); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetSession at the sliding expiry: err = %v, want ErrNotFound", err)
	}

	// A fresh session, read often enough to outlive its sliding lifetime several times.
	id = newID()
	if err := f.store.PutSession(ctx, id, f.record()); err != nil {
		t.Fatal(err)
	}
	created := f.clock.now()
	for range 5 {
		f.clock.advance(35 * time.Minute)
		if _, err := f.store.GetSession(ctx, id); err != nil {
			t.Fatalf("GetSession at +%v: %v", f.clock.now().Sub(created), err)
		}
	}
	// +175m: 5 minutes of the 3 hours are left, so the slide is capped to them instead of an hour.
	if e := f.expiry(id); !e.Equal(created.Add(3 * time.Hour)) {
		t.Errorf("near the cap the session expires at %v, want exactly the absolute lifetime (%v)", e, created.Add(3*time.Hour))
	}
	// Past the cap the session is gone, however recently it was read.
	f.clock.advance(5*time.Minute + time.Millisecond)
	if _, err := f.store.GetSession(ctx, id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetSession past the absolute cap: err = %v, want ErrNotFound", err)
	}
}

func TestSessions_PutValidationReplaceAndDelete(t *testing.T) {
	f := newSessionFixture(t, cache.Options{SessionSliding: time.Hour, SessionAbsolute: 3 * time.Hour})
	ctx := t.Context()

	if err := f.store.PutSession(ctx, "", f.record()); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("empty id: err = %v, want ErrInvalid", err)
	}
	old := f.record()
	old.CreatedAt = f.clock.now().Add(-4 * time.Hour)
	if err := f.store.PutSession(ctx, newID(), old); !errors.Is(err, errs.ErrInvalid) {
		t.Errorf("record older than the absolute lifetime: err = %v, want ErrInvalid", err)
	}
	if n := f.rows(); n != 0 {
		t.Errorf("%d rows after two refused puts, want none", n)
	}

	// A record created earlier keeps its own absolute clock.
	aged := f.record()
	aged.CreatedAt = f.clock.now().Add(-2*time.Hour - 50*time.Minute)
	id := newID()
	if err := f.store.PutSession(ctx, id, aged); err != nil {
		t.Fatalf("PutSession of an aged record: %v", err)
	}
	if e := f.expiry(id); !e.Equal(aged.CreatedAt.Add(3 * time.Hour)) {
		t.Errorf("an aged record expires at %v, want its creation + the absolute lifetime (%v): the slide is shortened to what is left", e, aged.CreatedAt.Add(3*time.Hour))
	}

	// Putting the same id again replaces the record.
	if err := f.store.PutSession(ctx, id, f.record()); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.GetSession(ctx, id); err != nil || !got.CreatedAt.Equal(f.clock.now()) {
		t.Errorf("GetSession after a second put = %+v, %v; want the new record", got, err)
	}
	if n := f.rows(); n != 1 {
		t.Errorf("%d rows after putting one id twice, want 1", n)
	}

	if err := f.store.DeleteSession(ctx, id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := f.store.GetSession(ctx, id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetSession after delete: err = %v, want ErrNotFound", err)
	}
	if err := f.store.DeleteSession(ctx, id); err != nil {
		t.Errorf("deleting an absent session: %v", err)
	}
	if err := f.store.DeleteSession(ctx, ""); err != nil {
		t.Errorf("deleting the empty id: %v", err)
	}
}

// The id is a bearer credential: what reaches the table is its hash, and nothing in the
// row can be used to sign in.
func TestSessions_RowHoldsTheHashOfTheIDAndNotTheID(t *testing.T) {
	f := newSessionFixture(t, cache.Options{})
	id := newID()
	if err := f.store.PutSession(t.Context(), id, f.record()); err != nil {
		t.Fatal(err)
	}

	var key []byte
	var asText string
	f.asOwner(func(ctx context.Context, tx pgx.Tx) {
		// Every column of the row, as the server would hand it to anyone who copies the table.
		err := tx.QueryRow(ctx, `SELECT id_hash, row_to_json(s)::text FROM identity.sessions s`).Scan(&key, &asText)
		if err != nil {
			t.Fatalf("read the row: %v", err)
		}
	})
	sum := sha256.Sum256([]byte(id))
	if string(key) != string(sum[:]) {
		t.Errorf("id_hash = %x, want the SHA-256 of the id (%x)", key, sum)
	}
	for name, secret := range map[string]string{
		"the id":                  id,
		"the id as std base64":    base64.StdEncoding.EncodeToString([]byte(id)),
		"the id as hex":           hex.EncodeToString([]byte(id)),
		"the id as bytea escapes": `\x` + hex.EncodeToString([]byte(id)),
	} {
		if strings.Contains(asText, secret) {
			t.Errorf("the stored row contains %s: %s", name, asText)
		}
	}
}

// A session can only be found by the exact id: nearby ids, the empty id and the stored
// hash itself match nothing.
func TestSessions_OnlyTheExactIDFindsASession(t *testing.T) {
	f := newSessionFixture(t, cache.Options{})
	ctx := t.Context()
	id := newID()
	if err := f.store.PutSession(ctx, id, f.record()); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(id))
	for name, probe := range map[string]string{
		"the empty id":     "",
		"the id, extended": id + "x",
		"the id, cut":      id[:len(id)-1],
		"the stored hash":  string(sum[:]),
		"the hash as hex":  hex.EncodeToString(sum[:]),
	} {
		if _, err := f.store.GetSession(ctx, probe); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := f.store.GetSession(ctx, id); err != nil {
		t.Errorf("the exact id: %v", err)
	}
}

// Rows that expired are deleted by the next sign-in, and a read ignores them before that.
func TestSessions_ExpiredRowsAreDeletedByTheNextSignIn(t *testing.T) {
	f := newSessionFixture(t, cache.Options{SessionSliding: time.Hour, SessionAbsolute: 3 * time.Hour})
	ctx := t.Context()
	stale, kept := newID(), newID()
	for _, id := range []string{stale, kept} {
		if err := f.store.PutSession(ctx, id, f.record()); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.advance(50 * time.Minute)
	if _, err := f.store.GetSession(ctx, kept); err != nil { // slides: lives to +110m
		t.Fatal(err)
	}
	f.clock.advance(20 * time.Minute) // +70m: stale expired at +60m, kept has 40 minutes left

	if _, err := f.store.GetSession(ctx, stale); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("an expired session before any clean-up: err = %v, want ErrNotFound", err)
	}
	if n := f.rows(); n != 2 {
		t.Fatalf("%d rows before a sign-in, want both: a read deletes nothing", n)
	}

	fresh := newID()
	if err := f.store.PutSession(ctx, fresh, f.record()); err != nil {
		t.Fatal(err)
	}
	if n := f.rows(); n != 2 {
		t.Errorf("%d rows after the sign-in, want the live one and the new one", n)
	}
	for id, want := range map[string]bool{stale: false, kept: true, fresh: true} {
		_, err := f.store.GetSession(ctx, id)
		if (err == nil) != want {
			t.Errorf("GetSession(%q...) err = %v, want found = %v", id[:6], err, want)
		}
	}
}

func TestSessions_ConcurrentReadsAllSucceedAndAgree(t *testing.T) {
	f := newSessionFixture(t, cache.Options{SessionSliding: time.Hour, SessionAbsolute: 3 * time.Hour})
	ctx := t.Context()
	id := newID()
	if err := f.store.PutSession(ctx, id, f.record()); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(10 * time.Minute)

	// A browser fires several requests at once; each reads (and slides) the same row.
	var wg sync.WaitGroup
	errc := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			if _, err := f.store.GetSession(ctx, id); err != nil {
				errc <- err
			}
		})
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Errorf("a concurrent GetSession failed: %v", err)
	}
	if e := f.expiry(id); !e.Equal(f.clock.now().Add(time.Hour)) {
		t.Errorf("expires at %v, want %v: concurrent reads must agree", e, f.clock.now().Add(time.Hour))
	}
}

// The store reaches the table only as kiwi_system. A DB opened without the system pool
// (KIWI_DATABASE_SYSTEM_URL unset) must fail every call, not fall back to another role.
func TestSessions_NeedTheSystemPool(t *testing.T) {
	f := newSessionFixture(t, cache.Options{})
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: f.env.AppURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	store := NewSessions(d, cache.Options{})

	if err := store.PutSession(t.Context(), newID(), f.record()); err == nil || !strings.Contains(err.Error(), "system pool") {
		t.Errorf("PutSession without a system pool: err = %v, want it to name the pool", err)
	}
	if _, err := store.GetSession(t.Context(), newID()); err == nil || errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetSession without a system pool: err = %v, want a failure that is not 'not found'", err)
	}
	// A cookie that cannot be one of our ids never reaches the pool: with none, it is
	// still just "not found", which is how the cheap kind of junk costs no transaction.
	for _, junk := range []string{"", "x", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("a", 42) + "=", strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "\x00"} {
		if _, err := store.GetSession(t.Context(), junk); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("GetSession(%q) = %v, want not found without a transaction", junk, err)
		}
	}
}

// The point of keeping sessions in Postgres: a process that starts later, with nothing
// but the database in common with the first, still knows the session.
func TestSessions_SurviveARestartOfTheProcess(t *testing.T) {
	f := newFixture(t, withMemoryProfile())
	f.seedWorkspace(fakeoidc.DefaultDomain)
	sid := f.mustSignIn(fakeoidc.AliceEmail)

	// A second process: its own memory cache and session store over the same database.
	d, err := db.Open(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	mem := cache.NewMemory(cache.Options{})
	t.Cleanup(func() { _ = mem.Close() })
	next, err := New(t.Context(), Deps{Config: f.cfg, DB: d, Cache: mem, Sessions: NewSessions(d, cache.Options{})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := next.Session(t.Context(), sid.Value)
	if err != nil {
		t.Fatalf("the session after a restart: %v", err)
	}
	first, err := f.mod.Session(t.Context(), sid.Value)
	if err != nil || got.UserID != first.UserID || got.WorkspaceID != first.WorkspaceID {
		t.Errorf("the restarted process resolves the session to %+v, the first to %+v (%v)", got, first, err)
	}

	// What lived in the first process's memory did not follow: a login in flight.
	authURL, _ := f.login("/")
	if _, err := mem.TakeOAuthState(t.Context(), authURL.Query().Get("state")); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the login state of the first process reached the second: err = %v", err)
	}

	// Logging out in one process ends the session for the other.
	if err := next.Logout(t.Context(), sid.Value); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mod.Session(t.Context(), sid.Value); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the first process after the second logged out: err = %v, want ErrNotFound", err)
	}
}
