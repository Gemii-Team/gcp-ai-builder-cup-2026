package db

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/errs"
	"kiwi/internal/testkit/dbtest"
)

type fixture struct {
	env  *dbtest.Env
	db   *DB
	a, b uuid.UUID
}

func start(t *testing.T) fixture {
	t.Helper()
	env := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, env)
	d, err := Open(t.Context(), config.Config{DatabaseURL: env.AppURL, DatabaseSystemURL: env.SystemURL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(d.Close)
	return fixture{env: env, db: d, a: a, b: b}
}

// channels counts the chat.channels rows a transaction bound to ws can see.
func channels(t *testing.T, d *DB, ws uuid.UUID) int {
	t.Helper()
	var n int
	err := d.InWorkspace(t.Context(), ws, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM chat.channels`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count channels in %s: %v", ws, err)
	}
	return n
}

func insertChannel(ctx context.Context, tx pgx.Tx, ws uuid.UUID, name string) error {
	_, err := tx.Exec(ctx, `INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, $3, 'public')`,
		uuid.Must(uuid.NewV7()), ws, name)
	return err
}

// poolSettings opens the pools for cfg (they connect lazily, so no database has to
// answer) and returns each one's configuration.
func poolSettings(t *testing.T, cfg config.Config) (app, system *pgxpool.Config) {
	t.Helper()
	cfg.DatabaseURL = "postgres://kiwi_app:pw@127.0.0.1:1/kiwi"
	cfg.DatabaseSystemURL = "postgres://kiwi_system:pw@127.0.0.1:1/kiwi"
	d, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(d.Close)
	return d.app.Config(), d.system.Config()
}

// A Config that did not come from config.Load has zero pool settings: they take the
// defaults, which are what the pools were before they became configurable.
func TestOpen_PoolDefaults(t *testing.T) {
	app, system := poolSettings(t, config.Config{})
	if app.MaxConns != 20 || system.MaxConns != 16 {
		t.Errorf("MaxConns = %d app + %d system, want 20 + 16", app.MaxConns, system.MaxConns)
	}
	for name, pc := range map[string]*pgxpool.Config{"app": app, "system": system} {
		if pc.MaxConnIdleTime != 30*time.Minute {
			t.Errorf("%s MaxConnIdleTime = %v, want 30m", name, pc.MaxConnIdleTime)
		}
		if pc.MinConns != 0 {
			t.Errorf("%s MinConns = %d, want 0: idle connections must be able to go away", name, pc.MinConns)
		}
		// No server-side prepared statements: safe behind a transaction-pooling proxy.
		if pc.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
			t.Errorf("%s DefaultQueryExecMode = %v, want QueryExecModeExec", name, pc.ConnConfig.DefaultQueryExecMode)
		}
	}
}

func TestOpen_PoolOverrides(t *testing.T) {
	app, system := poolSettings(t, config.Config{DBAppMaxConns: 6, DBSystemMaxConns: 7, DBMaxConnIdle: 2 * time.Minute})
	if app.MaxConns != 6 || system.MaxConns != 7 {
		t.Errorf("MaxConns = %d app + %d system, want 6 + 7", app.MaxConns, system.MaxConns)
	}
	if app.MaxConnIdleTime != 2*time.Minute || system.MaxConnIdleTime != 2*time.Minute {
		t.Errorf("MaxConnIdleTime = %v app, %v system, want 2m for both", app.MaxConnIdleTime, system.MaxConnIdleTime)
	}
}

func TestOpen_RefusesAPoolSizePgxCannotUse(t *testing.T) {
	for _, n := range []int{-1, 1 << 40} {
		_, err := Open(t.Context(), config.Config{DatabaseURL: "postgres://kiwi_app:pw@127.0.0.1:1/kiwi", DBAppMaxConns: n})
		if err == nil || !strings.Contains(err.Error(), "pool size") {
			t.Errorf("DBAppMaxConns %d: err = %v, want a pool size error", n, err)
		}
	}
}

func TestOpen_RejectsBadConfigWithoutEchoingSecrets(t *testing.T) {
	if _, err := Open(t.Context(), config.Config{}); err == nil {
		t.Fatal("Open with no database URL succeeded")
	}
	const secret = "s3cr3t-pw"
	for _, cfg := range []config.Config{
		{DatabaseURL: "postgres://kiwi_app:" + secret + "@localhost:notaport/kiwi"},
		{DatabaseURL: "postgres://kiwi_app@localhost/kiwi", DatabaseSystemURL: "postgres://kiwi_system:" + secret + "@localhost:notaport/kiwi"},
	} {
		_, err := Open(t.Context(), cfg)
		if err == nil {
			t.Fatalf("Open(%+v) succeeded", cfg)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Open error leaks the password: %v", err)
		}
	}
}

func TestInWorkspace_FailClosedOnReusedConnection(t *testing.T) {
	fx := start(t)
	// One connection, so the raw query below provably runs on the connection
	// InWorkspace just used.
	pool, err := newPool(t.Context(), fx.env.AppURL, 1, time.Minute)
	if err != nil {
		t.Fatalf("newPool: %v", err)
	}
	t.Cleanup(pool.Close)
	d := &DB{app: pool}

	var inside int
	err = d.InWorkspace(t.Context(), fx.a, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM chat.channels`).Scan(&inside)
	})
	if err != nil {
		t.Fatalf("InWorkspace: %v", err)
	}
	if inside != 1 {
		t.Fatalf("inside workspace A: %d channels, want 1", inside)
	}

	var outside int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM chat.channels`).Scan(&outside); err != nil {
		t.Fatalf("raw query on the reused connection: %v", err)
	}
	if outside != 0 {
		t.Fatalf("raw query outside any workspace saw %d channels, want 0 (the workspace leaked past the transaction)", outside)
	}
	if got := pool.Stat().TotalConns(); got != 1 {
		t.Fatalf("pool opened %d connections, want 1: the test did not reuse a connection", got)
	}
}

func TestInWorkspace_RollsBackOnError(t *testing.T) {
	fx := start(t)
	boom := errors.New("boom")

	err := fx.db.InWorkspace(t.Context(), fx.a, func(ctx context.Context, tx pgx.Tx) error {
		if err := insertChannel(ctx, tx, fx.a, "doomed"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the function's error", err)
	}
	if got := channels(t, fx.db, fx.a); got != 1 {
		t.Fatalf("after a failed InWorkspace: %d channels, want 1 (the seeded one)", got)
	}

	err = fx.db.InWorkspace(t.Context(), fx.a, func(ctx context.Context, tx pgx.Tx) error {
		return insertChannel(ctx, tx, fx.a, "kept")
	})
	if err != nil {
		t.Fatalf("InWorkspace: %v", err)
	}
	if got := channels(t, fx.db, fx.a); got != 2 {
		t.Fatalf("after a successful InWorkspace: %d channels, want 2", got)
	}
}

func TestInWorkspace_RollsBackOnPanic(t *testing.T) {
	fx := start(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		_ = fx.db.InWorkspace(t.Context(), fx.a, func(ctx context.Context, tx pgx.Tx) error {
			if err := insertChannel(ctx, tx, fx.a, "doomed"); err != nil {
				t.Fatalf("insert: %v", err)
			}
			panic("handler bug")
		})
	}()
	if got := channels(t, fx.db, fx.a); got != 1 {
		t.Fatalf("after a panicking InWorkspace: %d channels, want 1", got)
	}
}

func TestInWorkspace_RejectsNilWorkspace(t *testing.T) {
	fx := start(t)
	called := false
	err := fx.db.InWorkspace(t.Context(), uuid.Nil, func(context.Context, pgx.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("err = %v, want errs.ErrInvalid", err)
	}
	if called {
		t.Fatal("the function ran without a workspace")
	}
}

func TestInWorkspace_BindsTheRequestedWorkspace(t *testing.T) {
	fx := start(t)
	for _, ws := range []uuid.UUID{fx.a, fx.b} {
		var visible []uuid.UUID
		err := fx.db.InWorkspace(t.Context(), ws, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT workspace_id FROM chat.channels`)
			if err != nil {
				return err
			}
			visible, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		})
		if err != nil {
			t.Fatalf("InWorkspace(%s): %v", ws, err)
		}
		if !slices.Equal(visible, []uuid.UUID{ws}) {
			t.Fatalf("InWorkspace(%s) saw channels of %v", ws, visible)
		}
	}
}

func TestSystem_SeesAllWorkspaces(t *testing.T) {
	fx := start(t)

	var got []uuid.UUID
	err := fx.db.System(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM identity.workspaces ORDER BY id`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		t.Fatalf("System: %v", err)
	}
	if want := []uuid.UUID{fx.a, fx.b}; !slices.Equal(got, want) {
		t.Fatalf("System saw workspaces %v, want %v", got, want)
	}

	// The same query through InWorkspace is bound to one tenant.
	var scoped int
	err = fx.db.InWorkspace(t.Context(), fx.a, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM identity.workspaces`).Scan(&scoped)
	})
	if err != nil || scoped != 1 {
		t.Fatalf("InWorkspace saw %d workspaces (err %v), want 1", scoped, err)
	}
}

func TestSystem_RollsBackOnError(t *testing.T) {
	fx := start(t)
	boom := errors.New("boom")
	err := fx.db.System(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE identity.users SET name = 'renamed'`)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the function's error", err)
	}
	var renamed int
	err = fx.db.System(t.Context(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM identity.users WHERE name = 'renamed'`).Scan(&renamed)
	})
	if err != nil || renamed != 0 {
		t.Fatalf("%d users renamed by a failed System call (err %v), want 0", renamed, err)
	}
}

func TestSystem_WithoutSystemURLFails(t *testing.T) {
	fx := start(t)
	d, err := Open(t.Context(), config.Config{DatabaseURL: fx.env.AppURL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(d.Close)
	called := false
	err = d.System(t.Context(), func(context.Context, pgx.Tx) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("System without a system pool: err = %v, called = %v; want an error and no call", err, called)
	}
}

func TestPing_ChecksEveryPoolItWasOpenedWith(t *testing.T) {
	fx := start(t)
	if err := fx.db.Ping(t.Context()); err != nil {
		t.Fatalf("Ping of a reachable database: %v", err)
	}

	appOnly, err := Open(t.Context(), config.Config{DatabaseURL: fx.env.AppURL})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(appOnly.Close)
	if err := appOnly.Ping(t.Context()); err != nil {
		t.Fatalf("Ping without a system pool: %v", err)
	}
}

func TestPing_NamesThePoolThatCannotConnectWithoutEchoingTheURL(t *testing.T) {
	fx := start(t)
	const secret = "s3cr3t-pw"
	// Port 1 refuses at once; the password is what must not come back in the error.
	dead := "postgres://kiwi_app:" + secret + "@127.0.0.1:1/kiwi?sslmode=disable"
	deadSystem := "postgres://kiwi_system:" + secret + "@127.0.0.1:1/kiwi?sslmode=disable"

	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"application pool", config.Config{DatabaseURL: dead}, "application"},
		{"system pool", config.Config{DatabaseURL: fx.env.AppURL, DatabaseSystemURL: deadSystem}, "system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open(t.Context(), tc.cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(d.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err = d.Ping(ctx)
			if err == nil {
				t.Fatal("Ping of an unreachable database succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Ping error %q does not name the %s", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Ping error leaks the password: %v", err)
			}
		})
	}
}
