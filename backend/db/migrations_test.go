package db_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kiwi/db/migrations"
	"kiwi/internal/migrate"
	"kiwi/internal/testkit/dbtest"
)

// insufficientPrivilege is SQLSTATE 42501, raised both for a missing GRANT and
// for a row-level-security violation.
const insufficientPrivilege = "42501"

// nullifGuard is how Postgres prints the fail-closed workspace expression.
const nullifGuard = `NULLIF(current_setting('kiwi.workspace_id'::text, true), ''::text)`

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func scalar[T any](t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := q.QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// inWorkspace runs fn in a transaction with kiwi.workspace_id set the way
// db.InWorkspace will, and rolls it back afterwards. A nil ws sets nothing.
func inWorkspace(t *testing.T, url string, ws *uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()
	conn := connect(t, url)
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if ws != nil {
		if _, err := tx.Exec(t.Context(), `SELECT set_config('kiwi.workspace_id', $1, true)`, ws.String()); err != nil {
			t.Fatalf("set workspace: %v", err)
		}
	}
	fn(tx)
}

// execErrCode runs sql in a throwaway transaction as the role behind url and
// returns the SQLSTATE of the failure ("" when it succeeds).
func execErrCode(t *testing.T, url, sql string, args ...any) string {
	t.Helper()
	conn := connect(t, url)
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(t.Context(), sql, args...)
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: not a Postgres error: %v", sql, err)
	}
	return pgErr.Code
}

// workspaceTables lists every table with a workspace_id column.
func workspaceTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `
		SELECT n.nspname || '.' || c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'workspace_id' AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("list workspace tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list workspace tables: %v", err)
	}
	return tables
}

func TestEveryWorkspaceTableHasForcedRLS(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)

	tables := workspaceTables(t, conn)
	for _, want := range []string{
		"chat.channels", "chat.channel_members", "chat.messages", "files.files",
		"meetings.meetings", "meetings.transcript_entries", "meetings.summaries", "meetings.space_subscriptions",
		"tasks.tasks", "knowledge.chunks", "agents.runs", "agents.sessions", "agents.session_events",
		"identity.users", "identity.oauth_tokens", "platform.ref_counters",
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("%s has no workspace_id column (catalog query found %v)", want, tables)
		}
	}

	// The tenant table itself has no workspace_id, so it is bound by its own id.
	policied := append(slices.Clone(tables), "identity.workspaces")
	for _, table := range policied {
		var enabled, forced bool
		var policies int
		err := conn.QueryRow(t.Context(), `
			SELECT c.relrowsecurity, c.relforcerowsecurity,
			       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
			FROM pg_class c WHERE c.oid = $1::regclass`, table).Scan(&enabled, &forced, &policies)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if !enabled || !forced || policies < 1 {
			t.Errorf("%s: relrowsecurity=%v relforcerowsecurity=%v policies=%d, want enabled, forced and >= 1 policy",
				table, enabled, forced, policies)
		}
	}

	// Every policy must carry the NULLIF guard in both USING and WITH CHECK.
	rows, err := conn.Query(t.Context(), `
		SELECT schemaname || '.' || tablename, policyname, cmd, coalesce(qual, ''), coalesce(with_check, '')
		FROM pg_policies WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, cmd, qual, check string
		if err := rows.Scan(&table, &name, &cmd, &qual, &check); err != nil {
			t.Fatal(err)
		}
		if cmd != "ALL" {
			t.Errorf("%s policy %s applies to %s, want ALL", table, name, cmd)
		}
		for what, expr := range map[string]string{"USING": qual, "WITH CHECK": check} {
			if !strings.Contains(expr, nullifGuard) {
				t.Errorf("%s policy %s %s = %s, want it to contain %s", table, name, what, expr, nullifGuard)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// The platform schema is infrastructure: no tenant column, no RLS.
	for _, table := range []string{"platform.outbox", "platform.processed_events"} {
		if slices.Contains(tables, table) {
			t.Errorf("%s has a workspace_id column; the platform queue tables are not tenant data", table)
		}
		if rls := scalar[bool](t, conn, `SELECT relrowsecurity FROM pg_class WHERE oid = $1::regclass`, table); rls {
			t.Errorf("%s has RLS enabled, want none", table)
		}
	}
}

var isolatedTables = []string{
	"chat.channels", "chat.messages", "tasks.tasks", "files.files",
	"meetings.meetings", "knowledge.chunks", "agents.runs",
}

func TestIsolation_AcrossDomainTables(t *testing.T) {
	env := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, env)

	for _, table := range isolatedTables {
		t.Run(table, func(t *testing.T) {
			inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
				if got := scalar[int](t, tx, `SELECT count(*) FROM `+table); got != 1 {
					t.Errorf("as workspace A: %d rows, want 1", got)
				}
				if got := scalar[int](t, tx, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, b); got != 0 {
					t.Errorf("as workspace A: %d of B's rows visible, want 0", got)
				}
			})
			inWorkspace(t, env.AppURL, &b, func(tx pgx.Tx) {
				if got := scalar[int](t, tx, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, a); got != 0 {
					t.Errorf("as workspace B: %d of A's rows visible, want 0", got)
				}
				if got := scalar[int](t, tx, `SELECT count(*) FROM `+table); got != 1 {
					t.Errorf("as workspace B: %d rows, want 1", got)
				}
			})
		})
	}

	// Every tenant table, not only the seven above: the app role sees exactly
	// what kiwi_system counts for its own workspace.
	system := connect(t, env.SystemURL)
	for _, table := range workspaceTables(t, system) {
		want := scalar[int](t, system, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, a)
		inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
			if got := scalar[int](t, tx, `SELECT count(*) FROM `+table); got != want {
				t.Errorf("%s as workspace A: %d rows, want %d", table, got, want)
			}
		})
	}

	inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
		if got := scalar[int](t, tx, `SELECT count(*) FROM identity.workspaces`); got != 1 {
			t.Errorf("identity.workspaces as workspace A: %d rows, want 1", got)
		}
	})
}

func TestIsolation_WithCheckRejectsRowsOfAnotherWorkspace(t *testing.T) {
	env := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, env)

	inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(),
			`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, 'smuggled', 'public')`,
			uuid.New(), b)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege || !strings.Contains(pgErr.Message, "row-level security") {
			t.Fatalf("inserting a row for workspace B as A: err = %v, want a row-level security violation", err)
		}
	})
	inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
		tag, err := tx.Exec(t.Context(), `UPDATE chat.channels SET workspace_id = $1`, b)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
			t.Fatalf("moving A's channel to B: tag = %v, err = %v, want a row-level security violation", tag, err)
		}
	})
}

func TestFailClosed_NoGUC_ReturnsZeroRows(t *testing.T) {
	env := dbtest.Start(t)
	dbtest.SeedTwoWorkspaces(t, env)

	inWorkspace(t, env.AppURL, nil, func(tx pgx.Tx) {
		for _, table := range append(slices.Clone(isolatedTables), "identity.users", "identity.workspaces", "platform.ref_counters") {
			if got := scalar[int](t, tx, `SELECT count(*) FROM `+table); got != 0 {
				t.Errorf("%s without kiwi.workspace_id: %d rows, want 0", table, got)
			}
		}
	})
}

func TestFailClosed_PooledConnectionAfterSetLocalReverts(t *testing.T) {
	env := dbtest.Start(t)
	a, _ := dbtest.SeedTwoWorkspaces(t, env)
	conn := connect(t, env.AppURL)

	// What a pooled connection looks like after db.InWorkspace commits: SET LOCAL
	// reverted the custom setting to '' (not NULL), which a bare ::uuid cast
	// would turn into an error instead of zero rows.
	for _, stmt := range []string{
		"BEGIN",
		fmt.Sprintf("SET LOCAL kiwi.workspace_id = '%s'", a),
		"COMMIT",
	} {
		if _, err := conn.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	var setting *string
	if err := conn.QueryRow(t.Context(), `SELECT current_setting('kiwi.workspace_id', true)`).Scan(&setting); err != nil {
		t.Fatal(err)
	}
	if setting == nil || *setting != "" {
		t.Fatalf("after SET LOCAL reverts, current_setting = %v, want the empty string", setting)
	}

	var count int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM chat.channels`).Scan(&count); err != nil {
		t.Fatalf("select on the reused connection: %v", err)
	}
	if count != 0 {
		t.Fatalf("reused connection sees %d channels, want 0", count)
	}
}

func TestRoles_PlatformGrants(t *testing.T) {
	env := dbtest.Start(t)
	a, _ := dbtest.SeedTwoWorkspaces(t, env)

	t.Run("role attributes", func(t *testing.T) {
		conn := connect(t, env.OwnerURL)
		for role, want := range map[string]struct{ super, bypass bool }{
			"kiwi_owner":  {false, false},
			"kiwi_app":    {false, false},
			"kiwi_system": {false, true},
			"kiwi_ro":     {false, false},
		} {
			var super, bypass bool
			err := conn.QueryRow(t.Context(), `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass)
			if err != nil {
				t.Fatalf("%s: %v", role, err)
			}
			if super != want.super || bypass != want.bypass {
				t.Errorf("%s: rolsuper=%v rolbypassrls=%v, want %v and %v", role, super, bypass, want.super, want.bypass)
			}
		}
	})

	enqueue := `INSERT INTO platform.outbox (topic, key, payload) VALUES ('chat.message.created', 'k', '{"workspace_id":"x"}')`
	tests := []struct {
		name string
		url  string
		sql  string
		want string // SQLSTATE, "" for success
	}{
		{"app cannot read the outbox", env.AppURL, `SELECT count(*) FROM platform.outbox`, insufficientPrivilege},
		{"app can enqueue", env.AppURL, enqueue, ""},
		{"app cannot update the outbox", env.AppURL, `UPDATE platform.outbox SET published_at = now()`, insufficientPrivilege},
		{"app can record a processed event", env.AppURL,
			`INSERT INTO platform.processed_events (consumer, event_id) VALUES ('c', 'e')`, ""},
		{"app can read processed events", env.AppURL, `SELECT count(*) FROM platform.processed_events`, ""},
		{"app cannot delete processed events", env.AppURL, `DELETE FROM platform.processed_events`, insufficientPrivilege},
		{"system can read the outbox", env.SystemURL, `SELECT count(*) FROM platform.outbox`, ""},
		{"system can update the outbox", env.SystemURL, `UPDATE platform.outbox SET published_at = now()`, ""},
		{"system cannot enqueue", env.SystemURL, enqueue, insufficientPrivilege},
		{"system cannot delete from the outbox", env.SystemURL, `DELETE FROM platform.outbox`, insufficientPrivilege},
		{"system cannot touch processed events", env.SystemURL, `SELECT count(*) FROM platform.processed_events`, insufficientPrivilege},
		{"ro cannot read the outbox", env.ROURL, `SELECT count(*) FROM platform.outbox`, insufficientPrivilege},
		{"ro cannot read messages", env.ROURL, `SELECT count(*) FROM chat.messages`, insufficientPrivilege},
		{"ro cannot read tasks", env.ROURL, `SELECT count(*) FROM tasks.tasks`, insufficientPrivilege},
		{"ro can read channels", env.ROURL, `SELECT count(*) FROM chat.channels`, ""},
		{"ro can read channel members", env.ROURL, `SELECT count(*) FROM chat.channel_members`, ""},
		{"ro cannot insert", env.ROURL,
			`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES (gen_random_uuid(), gen_random_uuid(), 'x', 'public')`,
			insufficientPrivilege},
		{"ro cannot update", env.ROURL, `UPDATE chat.channels SET name = 'x'`, insufficientPrivilege},
		{"ro cannot enqueue", env.ROURL, enqueue, insufficientPrivilege},
		{"app cannot create objects", env.AppURL, `CREATE TABLE chat.rogue (id int)`, insufficientPrivilege},
		{"app cannot truncate", env.AppURL, `TRUNCATE chat.messages`, insufficientPrivilege},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := execErrCode(t, tc.url, tc.sql); got != tc.want {
				t.Errorf("SQLSTATE = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("ro is bound by row-level security", func(t *testing.T) {
		inWorkspace(t, env.ROURL, &a, func(tx pgx.Tx) {
			if got := scalar[int](t, tx, `SELECT count(*) FROM chat.channels`); got != 1 {
				t.Errorf("kiwi_ro in workspace A sees %d channels, want 1", got)
			}
		})
		inWorkspace(t, env.ROURL, nil, func(tx pgx.Tx) {
			if got := scalar[int](t, tx, `SELECT count(*) FROM chat.channels`); got != 0 {
				t.Errorf("kiwi_ro without a workspace sees %d channels, want 0", got)
			}
		})
	})
}

func TestChunks_VectorAndHNSW(t *testing.T) {
	env := dbtest.Start(t)
	a, _ := dbtest.SeedTwoWorkspaces(t, env)
	system := connect(t, env.SystemURL)

	version := scalar[string](t, system, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`)
	major, minor, _ := strings.Cut(version, ".")
	minor, _, _ = strings.Cut(minor, ".")
	maj, _ := strconv.Atoi(major)
	mnr, _ := strconv.Atoi(minor)
	if maj == 0 && mnr < 8 {
		t.Fatalf("pgvector %s, the spec needs >= 0.8 (iterative index scans)", version)
	}
	t.Logf("pgvector %s", version)

	def := scalar[string](t, system, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'knowledge' AND tablename = 'chunks' AND indexdef ILIKE '%hnsw%'`)
	if !strings.Contains(def, "USING hnsw (embedding vector_cosine_ops)") {
		t.Fatalf("HNSW index definition = %s, want USING hnsw (embedding vector_cosine_ops)", def)
	}

	channel := scalar[uuid.UUID](t, system, `SELECT id FROM chat.channels WHERE workspace_id = $1`, a)
	// 1000 random 768-dim vectors; the correlated "n > 0" makes Postgres draw a
	// fresh vector for every row.
	if _, err := system.Exec(t.Context(), `
		INSERT INTO knowledge.chunks
		  (id, workspace_id, channel_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
		SELECT gen_random_uuid(), $1, $2, 'doc', gen_random_uuid(), 'page:' || n, 'en', 'chunk ' || n, md5(n::text),
		       to_tsvector('simple', 'chunk ' || n), 'embeddinggemma',
		       (SELECT array_agg((random() * 2 - 1)::real) FROM generate_series(1, 768) WHERE n > 0)::vector
		FROM generate_series(1, 1000) AS n`, a, channel); err != nil {
		t.Fatalf("seed chunks: %v", err)
	}
	analyze(t, env, `knowledge.chunks`)

	t.Run("wrong dimension is rejected", func(t *testing.T) {
		code := execErrCode(t, env.SystemURL, `
			INSERT INTO knowledge.chunks
			  (id, workspace_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
			VALUES (gen_random_uuid(), $1, 'doc', gen_random_uuid(), 'page:1', 'en', 'x', 'x', to_tsvector('simple', 'x'), 'm', '[1,2,3]')`, a)
		if code != "22000" { // data_exception: expected 768 dimensions, not 3
			t.Errorf("SQLSTATE = %q, want 22000 (expected 768 dimensions)", code)
		}
	})

	t.Run("nearest neighbour uses the HNSW index", func(t *testing.T) {
		query := scalar[string](t, system, `SELECT embedding::text FROM knowledge.chunks WHERE source_anchor = 'page:500'`)
		// The retriever's shape: model filter, the caller's channels or workspace-wide, RLS on top.
		const nearest = `SELECT source_anchor FROM knowledge.chunks
			WHERE embedding_model = 'embeddinggemma' AND (channel_id = ANY($2) OR channel_id IS NULL)
			ORDER BY embedding <=> $1::vector LIMIT 5`
		set := func(tx pgx.Tx, stmts ...string) {
			t.Helper()
			for _, stmt := range stmts {
				if _, err := tx.Exec(t.Context(), stmt); err != nil {
					t.Fatalf("%s (pgvector %s): %v", stmt, version, err)
				}
			}
		}

		inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
			set(tx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`)
			if plan := explain(t, tx, nearest, query, []uuid.UUID{channel}); !strings.Contains(plan, "Index Scan using chunks_embedding_hnsw_idx") {
				t.Errorf("plan does not use the HNSW index:\n%s", plan)
			}
		})
		inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
			// Force the index and widen the search to the whole graph, so the
			// answer is the exact nearest neighbour and the check cannot flake.
			set(tx, `SET LOCAL hnsw.ef_search = 1000`, `SET LOCAL enable_seqscan = off`,
				`SET LOCAL enable_sort = off`, `SET LOCAL enable_bitmapscan = off`)
			if anchor := scalar[string](t, tx, nearest, query, []uuid.UUID{channel}); anchor != "page:500" {
				t.Errorf("nearest chunk to vector 500 is %s, want page:500", anchor)
			}
		})
	})
}

// analyze refreshes planner statistics as the table owner; kiwi_system may not
// ANALYZE and silently skips it, which leaves the planner guessing.
func analyze(t *testing.T, env *dbtest.Env, table string) {
	t.Helper()
	conn := connect(t, env.OwnerURL)
	if _, err := conn.Exec(t.Context(), `ANALYZE `+table); err != nil {
		t.Fatalf("analyze %s: %v", table, err)
	}
}

// explain returns the plan of sql, with the long vector literals cut short.
func explain(t *testing.T, tx pgx.Tx, sql string, args ...any) string {
	t.Helper()
	rows, err := tx.Query(t.Context(), `EXPLAIN (COSTS OFF) `+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for i, l := range lines {
		if len(l) > 200 {
			lines[i] = l[:200] + "…"
		}
	}
	return strings.Join(lines, "\n")
}

func TestMessages_SeqIndexServesDescendingPaging(t *testing.T) {
	env := dbtest.Start(t)
	a, _ := dbtest.SeedTwoWorkspaces(t, env)
	system := connect(t, env.SystemURL)

	channel := scalar[uuid.UUID](t, system, `SELECT id FROM chat.channels WHERE workspace_id = $1`, a)
	if _, err := system.Exec(t.Context(), `
		INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id)
		SELECT gen_random_uuid(), $1, $2, n, 'user', gen_random_uuid(), 'message ' || n, 'c' || n
		FROM generate_series(2, 5000) AS n`, a, channel); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	analyze(t, env, `chat.messages`)

	inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
		plan := explain(t, tx, `SELECT * FROM chat.messages WHERE channel_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT 50`, channel, 4000)
		if !strings.Contains(plan, "Index Scan Backward using messages_channel_id_seq_key") || strings.Contains(plan, "Sort") {
			t.Errorf("descending page query is not served by the (channel_id, seq) index:\n%s", plan)
		}
	})
}

func TestMigrateUp_IsIdempotent(t *testing.T) {
	env := dbtest.StartEmpty(t)
	conn := connect(t, env.OwnerURL)

	state := func() (applied, latest int, tables int) {
		t.Helper()
		err := conn.QueryRow(t.Context(),
			`SELECT count(*), coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied AND version_id > 0`).Scan(&applied, &latest)
		if err != nil {
			t.Fatalf("goose_db_version: %v", err)
		}
		tables = scalar[int](t, conn, `
			SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')`)
		return applied, latest, tables
	}

	if got := scalar[bool](t, conn, `SELECT to_regclass('goose_db_version') IS NULL`); !got {
		t.Fatal("StartEmpty returned a database that already ran goose")
	}
	if err := migrate.Up(t.Context(), env.OwnerURL); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("embedded migrations: %v (%d files)", err, len(files))
	}
	applied, latest, tables := state()
	if applied != len(files) || latest != len(files) {
		t.Fatalf("after the first Up: %d migrations applied, latest %d, want %d and %d", applied, latest, len(files), len(files))
	}
	if err := migrate.Up(t.Context(), env.OwnerURL); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	applied2, latest2, tables2 := state()
	if applied2 != applied || latest2 != latest || tables2 != tables {
		t.Errorf("second Up changed the database: applied %d→%d, latest %d→%d, tables %d→%d",
			applied, applied2, latest, latest2, tables, tables2)
	}
}
