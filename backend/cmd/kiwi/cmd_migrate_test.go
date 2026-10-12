package main

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"kiwi/db/migrations"
	"kiwi/internal/testkit/dbtest"
)

func TestMigrateIsRegistered(t *testing.T) {
	if commands["migrate"] == nil {
		t.Fatal("migrate is not registered")
	}
}

func TestMigrate_MissingDatabaseURLNamesTheVariable(t *testing.T) {
	t.Setenv("KIWI_MIGRATE_DATABASE_URL", "")

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"migrate"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "KIWI_MIGRATE_DATABASE_URL"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not name %s", stderr.String(), want)
	}
}

func TestMigrate_TakesNoArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"migrate", "--down"}, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if want := "takes no arguments"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr %q does not contain %q", stderr.String(), want)
	}
}

func TestMigrate_AppliesTheSchemaAndSucceedsWhenCurrent(t *testing.T) {
	env := dbtest.StartEmpty(t)
	t.Setenv("KIWI_MIGRATE_DATABASE_URL", env.OwnerURL)

	for attempt := 1; attempt <= 2; attempt++ {
		var stderr bytes.Buffer
		if code := run(context.Background(), []string{"migrate"}, &stderr); code != 0 {
			t.Fatalf("run %d: exit code = %d, stderr: %s", attempt, code, stderr.String())
		}
	}

	conn, err := pgx.Connect(t.Context(), env.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var version int
	if err := conn.QueryRow(t.Context(), `SELECT max(version_id) FROM goose_db_version`).Scan(&version); err != nil {
		t.Fatalf("goose_db_version: %v", err)
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if version != len(files) {
		t.Errorf("schema version = %d, want %d (the embedded migrations)", version, len(files))
	}
}
