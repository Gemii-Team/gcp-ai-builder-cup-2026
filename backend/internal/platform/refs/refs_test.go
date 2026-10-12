package refs_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/refs"
	"kiwi/internal/testkit/dbtest"
)

type env struct {
	db   *db.DB
	url  string
	a, b uuid.UUID
}

func start(t *testing.T) env {
	t.Helper()
	e := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, e)
	d, err := db.Open(t.Context(), config.Config{DatabaseURL: e.AppURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(d.Close)
	return env{db: d, url: e.AppURL, a: a, b: b}
}

func next(t *testing.T, d *db.DB, ws uuid.UUID, kind string) (string, error) {
	t.Helper()
	var ref string
	err := d.InWorkspace(t.Context(), ws, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ref, err = refs.Next(ctx, tx, kind)
		return err
	})
	return ref, err
}

// allocate takes n references concurrently, each in its own transaction.
func allocate(t *testing.T, d *db.DB, ws uuid.UUID, kind string, n int) []string {
	t.Helper()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		got  []string
		errc = make(chan error, n)
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := next(t, d, ws, kind)
			if err != nil {
				errc <- err
				return
			}
			mu.Lock()
			got = append(got, ref)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("allocate %s: %v", kind, err)
	}
	slices.Sort(got)
	return got
}

func series(kind string, from, to int) []string {
	var out []string
	for n := from; n <= to; n++ {
		out = append(out, fmt.Sprintf("%s-%d", kind, n))
	}
	return out
}

func TestRefsNext_GaplessPerWorkspaceAndKind(t *testing.T) {
	e := start(t)

	// MTG has a counter row seeded at 1000; DOC has none, so its first
	// allocation inserts: both paths must hand out 1001.. without a duplicate.
	for _, kind := range []string{"MTG", "DOC"} {
		got := allocate(t, e.db, e.a, kind, 20)
		if want := series(kind, 1001, 1020); !slices.Equal(got, want) {
			t.Fatalf("workspace A %s = %v, want %v", kind, got, want)
		}
	}

	for _, tc := range []struct{ kind, want string }{
		{"MTG", "MTG-1001"}, // workspace B has its own counter
		{"ANS", "ANS-1001"},
		{"TSK", "TSK-1001"},
	} {
		if got, err := next(t, e.db, e.b, tc.kind); err != nil || got != tc.want {
			t.Fatalf("workspace B %s = %q, %v; want %q", tc.kind, got, err, tc.want)
		}
	}
	if got, err := next(t, e.db, e.a, "MTG"); err != nil || got != "MTG-1021" {
		t.Fatalf("workspace A MTG after B's allocations = %q, %v; want MTG-1021", got, err)
	}

	for _, kind := range []string{"XYZ", "", "mtg", "MTG-"} {
		if _, err := next(t, e.db, e.a, kind); !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("kind %q: err = %v, want errs.ErrInvalid", kind, err)
		}
	}
}

// TestRefsNext_RollbackConsumesNothing pins "gapless": a failed attempt issues no REF.
func TestRefsNext_RollbackConsumesNothing(t *testing.T) {
	e := start(t)
	boom := errors.New("boom")
	err := e.db.InWorkspace(t.Context(), e.a, func(ctx context.Context, tx pgx.Tx) error {
		if ref, err := refs.Next(ctx, tx, "MTG"); err != nil || ref != "MTG-1001" {
			t.Fatalf("Next = %q, %v; want MTG-1001", ref, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got, err := next(t, e.db, e.a, "MTG"); err != nil || got != "MTG-1001" {
		t.Fatalf("after rollback Next = %q, %v; want MTG-1001 again", got, err)
	}
}

// TestRefsNext_OutsideWorkspaceFails: with no kiwi.workspace_id the counter is
// not allocated for anybody.
func TestRefsNext_OutsideWorkspaceFails(t *testing.T) {
	e := start(t)
	conn, err := pgx.Connect(t.Context(), e.url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if ref, err := refs.Next(t.Context(), tx, "MTG"); err == nil {
		t.Fatalf("Next without a workspace = %q, want an error", ref)
	}
}
