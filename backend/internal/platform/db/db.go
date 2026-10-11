// Package db is the only way into Postgres. InWorkspace runs a transaction
// bound to one tenant through row-level security as kiwi_app; System runs one
// as the BYPASSRLS role kiwi_system and may be called only by the files listed
// in system_allowlist_test.go.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/errs"
)

const (
	appMaxConns    = 20
	systemMaxConns = 16
)

// DB holds the two pools. Both use pgx.QueryExecModeExec, so no server-side
// prepared statement exists and a transaction-pooling proxy can sit in front.
type DB struct {
	app    *pgxpool.Pool
	system *pgxpool.Pool // nil when KIWI_DATABASE_SYSTEM_URL is unset
}

// Open creates the application pool from cfg.DatabaseURL and, when
// cfg.DatabaseSystemURL is set, the system pool. Connections are made lazily,
// so Open does not need the database to be reachable.
func Open(ctx context.Context, cfg config.Config) (*DB, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("db: KIWI_DATABASE_URL is empty")
	}
	app, err := newPool(ctx, cfg.DatabaseURL, appMaxConns)
	if err != nil {
		return nil, fmt.Errorf("db: application pool: %w", err)
	}
	d := &DB{app: app}
	if cfg.DatabaseSystemURL != "" {
		if d.system, err = newPool(ctx, cfg.DatabaseSystemURL, systemMaxConns); err != nil {
			app.Close()
			return nil, fmt.Errorf("db: system pool: %w", err)
		}
	}
	return d, nil
}

func newPool(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err // pgx redacts the password in its parse errors
	}
	pc.MaxConns = maxConns
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	return pgxpool.NewWithConfig(ctx, pc)
}

// Close releases both pools, waiting for connections in use to be returned.
func (d *DB) Close() {
	d.app.Close()
	if d.system != nil {
		d.system.Close()
	}
}

// InWorkspace runs fn in a transaction whose kiwi.workspace_id is ws, so row-level
// security shows fn that workspace's rows and nothing else. The setting is
// transaction-local: a connection returned to the pool carries no workspace and
// the policies then match no rows. fn's error rolls the transaction back and is
// returned as is. A nil ws is errs.ErrInvalid.
func (d *DB) InWorkspace(ctx context.Context, ws uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if ws == uuid.Nil {
		return fmt.Errorf("db: in workspace: nil workspace id: %w", errs.ErrInvalid)
	}
	return inTx(ctx, d.app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
			return fmt.Errorf("db: bind workspace: %w", err)
		}
		return fn(ctx, tx)
	})
}

// System runs fn in a transaction as kiwi_system, which bypasses row-level
// security. Only the files on the allowlist may call it; see
// TestSystemCallersAreAllowlisted.
func (d *DB) System(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if d.system == nil {
		return errors.New("db: system pool is not configured (KIWI_DATABASE_SYSTEM_URL)")
	}
	return inTx(ctx, d.system, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

// inTx begins a transaction on pool, runs fn and commits, rolling back when fn
// fails or panics.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	// After a successful Commit this returns pgx.ErrTxClosed, which is ignored. When
	// ctx is already done the rollback fails and pgx discards the connection: the
	// server ends the transaction when the connection closes.
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
