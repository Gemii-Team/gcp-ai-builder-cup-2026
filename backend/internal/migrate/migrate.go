// Package migrate applies the embedded goose migrations.
package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"kiwi/db/migrations"
)

// Up applies every pending migration in migrations.FS to the database at url,
// which must connect as kiwi_owner. It returns nil when the schema is already
// up to date.
func Up(ctx context.Context, url string) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return fmt.Errorf("migrate: parse database url: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
