package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"kiwi/internal/migrate"
	"kiwi/internal/platform/config"
)

func init() { register("migrate", runMigrate) }

// runMigrate applies the embedded migrations as kiwi_owner, through
// KIWI_MIGRATE_DATABASE_URL, and succeeds when the schema is already current.
func runMigrate(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("takes no arguments, got %q", args)
	}
	cfg, err := config.Load([]config.Role{config.RoleMigrate}, os.Getenv)
	if err != nil {
		return err
	}
	if err := migrate.Up(ctx, cfg.MigrateURL); err != nil {
		return err
	}
	slog.Info("database schema is up to date")
	return nil
}
