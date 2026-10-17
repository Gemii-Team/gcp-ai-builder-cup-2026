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
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"kiwi/db/migrations"
	"kiwi/internal/migrate"
	"kiwi/internal/testkit/dbtest"
)

// insufficientPrivilege is SQLSTATE 42501, raised both for a missing GRANT and
// for a row-level-security violation; the message tells them apart.
const insufficientPrivilege = "42501"

// nullifGuard is how Postgres prints the fail-closed workspace expression.
const nullifGuard = `NULLIF(current_setting('kiwi.workspace_id'::text, true), ''::text)`

// kiwiSchemas are the schemas the grant catalog test inspects.
var kiwiSchemas = []string{"identity", "chat", "files", "meetings", "tasks", "knowledge", "agents", "calendar", "platform"}

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

// inTx runs fn in a transaction as the role behind url, with kiwi.workspace_id
// set the way db.InWorkspace will (a nil ws sets nothing), then commits or
// rolls back.
func inTx(t *testing.T, url string, ws *uuid.UUID, commit bool, fn func(tx pgx.Tx)) {
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
	if commit {
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

// inWorkspace runs fn in a rolled-back transaction bound to ws.
func inWorkspace(t *testing.T, url string, ws *uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()
	inTx(t, url, ws, false, fn)
}

// seedIn runs fn as kiwi_owner in a committed transaction bound to ws: FORCE
// ROW LEVEL SECURITY binds the owner, and WITH CHECK is satisfied. Tests seed
// through it so they never need a privilege kiwi_system lacks.
func seedIn(t *testing.T, env *dbtest.Env, ws uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()
	inTx(t, env.OwnerURL, &ws, true, fn)
}

// execErrIn runs sql in a throwaway transaction as the role behind url, bound
// to ws when non-nil, and returns the Postgres error (nil when it succeeds).
func execErrIn(t *testing.T, url string, ws *uuid.UUID, sql string, args ...any) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	inTx(t, url, ws, false, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), sql, args...)
		if err != nil && !errors.As(err, &pgErr) {
			t.Fatalf("%s: not a Postgres error: %v", sql, err)
		}
	})
	return pgErr
}

// execErrCode is execErrIn with no workspace, reduced to the SQLSTATE ("" on success).
func execErrCode(t *testing.T, url, sql string, args ...any) string {
	t.Helper()
	if pgErr := execErrIn(t, url, nil, sql, args...); pgErr != nil {
		return pgErr.Code
	}
	return ""
}

func queryStrings(t *testing.T, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args ...any) []string {
	t.Helper()
	rows, err := q.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// workspaceTables lists every table with a workspace_id column.
func workspaceTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	return queryStrings(t, conn, `
		SELECT n.nspname || '.' || c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'workspace_id' AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1`)
}

// tenantTables is every table RLS binds: the tables with a workspace_id and
// identity.workspaces, which has none and is bound by its own id.
func tenantTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	return append(workspaceTables(t, conn), "identity.workspaces")
}

func tenantColumn(table string) string {
	if table == "identity.workspaces" {
		return "id"
	}
	return "workspace_id"
}

func TestEveryWorkspaceTableHasForcedRLS(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)

	tables := workspaceTables(t, conn)
	for _, want := range []string{
		"chat.channels", "chat.channel_members", "chat.messages", "files.files",
		"meetings.meetings", "meetings.transcript_entries", "meetings.summaries", "meetings.space_subscriptions",
		"tasks.tasks", "knowledge.chunks", "agents.runs", "agents.sessions", "agents.session_events",
		"calendar.events", "identity.users", "identity.oauth_tokens", "platform.ref_counters",
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("%s has no workspace_id column (catalog query found %v)", want, tables)
		}
	}

	// A table without a workspace_id is a decision, not an accident: only the
	// platform queue, the tenant table itself and goose's bookkeeping may lack one.
	exempt := []string{"platform.outbox", "platform.processed_events", "identity.workspaces", "public.goose_db_version"}
	for _, table := range queryStrings(t, conn, `
		SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1`) {
		if !slices.Contains(tables, table) && !slices.Contains(exempt, table) {
			t.Errorf("%s has no workspace_id column and is not an exempt table %v", table, exempt)
		}
	}

	tenant := tenantTables(t, conn)
	for _, table := range tenant {
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

	// Every policy is exactly the fail-closed expression on the tenant column,
	// in both USING and WITH CHECK: `true OR ...`, a missing NULLIF or the wrong
	// column all fail.
	rows, err := conn.Query(t.Context(), `
		SELECT schemaname || '.' || tablename, policyname, permissive, roles, cmd, coalesce(qual, ''), coalesce(with_check, '')
		FROM pg_policies WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, permissive, cmd, qual, check string
		var roles []string
		if err := rows.Scan(&table, &name, &permissive, &roles, &cmd, &qual, &check); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(tenant, table) {
			t.Errorf("%s has policy %s but is not a tenant table", table, name)
			continue
		}
		want := fmt.Sprintf("(%s = (%s)::uuid)", tenantColumn(table), nullifGuard)
		if qual != want {
			t.Errorf("%s policy %s USING = %s, want %s", table, name, qual, want)
		}
		if check != want {
			t.Errorf("%s policy %s WITH CHECK = %s, want %s", table, name, check, want)
		}
		if permissive != "PERMISSIVE" || cmd != "ALL" || !slices.Equal(roles, []string{"public"}) {
			t.Errorf("%s policy %s is %s for %s on %v, want PERMISSIVE for ALL on public", table, name, permissive, cmd, roles)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// The platform queue is infrastructure: no tenant column, no RLS.
	for _, table := range []string{"platform.outbox", "platform.processed_events"} {
		if slices.Contains(tables, table) {
			t.Errorf("%s has a workspace_id column; the platform queue tables are not tenant data", table)
		}
		if rls := scalar[bool](t, conn, `SELECT relrowsecurity FROM pg_class WHERE oid = $1::regclass`, table); rls {
			t.Errorf("%s has RLS enabled, want none", table)
		}
	}
}

func TestIsolation_AcrossDomainTables(t *testing.T) {
	env := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, env)

	// The seven tables of the brief, and every other table RLS binds: the seed
	// puts exactly one row of each in each workspace.
	tenant := tenantTables(t, connect(t, env.OwnerURL))
	for _, named := range []string{"chat.channels", "chat.messages", "tasks.tasks", "files.files", "meetings.meetings", "knowledge.chunks", "agents.runs"} {
		if !slices.Contains(tenant, named) {
			t.Fatalf("%s is not a tenant table", named)
		}
	}

	for _, table := range tenant {
		col := tenantColumn(table)
		t.Run(table, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				own, other uuid.UUID
			}{{"A", a, b}, {"B", b, a}} {
				inWorkspace(t, env.AppURL, &tc.own, func(tx pgx.Tx) {
					if got := scalar[int](t, tx, `SELECT count(*) FROM `+table); got != 1 {
						t.Errorf("as workspace %s: %d rows, want 1 (does dbtest.SeedTwoWorkspaces seed this table?)", tc.name, got)
					}
					if got := scalar[int](t, tx, `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, tc.other); got != 0 {
						t.Errorf("as workspace %s: %d of the other workspace's rows visible, want 0", tc.name, got)
					}
				})
			}
		})
	}
}

func TestIsolation_WithCheckRejectsRowsOfAnotherWorkspace(t *testing.T) {
	env := dbtest.Start(t)
	a, b := dbtest.SeedTwoWorkspaces(t, env)

	pgErr := execErrIn(t, env.AppURL, &a,
		`INSERT INTO chat.channels (id, workspace_id, name, kind) VALUES ($1, $2, 'smuggled', 'public')`, uuid.New(), b)
	if pgErr == nil || pgErr.Code != insufficientPrivilege || !strings.Contains(pgErr.Message, "row-level security") {
		t.Errorf("inserting a row for workspace B as A: err = %v, want a row-level security violation", pgErr)
	}

	// Moving A's row to B must fail on the policy, not on a missing privilege:
	// every table through kiwi_app, except identity.workspaces, which kiwi_app
	// cannot update at all, so the owner (bound by FORCE) proves its policy.
	for _, table := range tenantTables(t, connect(t, env.OwnerURL)) {
		t.Run(table, func(t *testing.T) {
			url := env.AppURL
			if table == "identity.workspaces" {
				url = env.OwnerURL
			}
			pgErr := execErrIn(t, url, &a, fmt.Sprintf(`UPDATE %s SET %s = $1`, table, tenantColumn(table)), b)
			if pgErr == nil || pgErr.Code != insufficientPrivilege || !strings.Contains(pgErr.Message, "row-level security") {
				t.Errorf("moving A's row to B: err = %v, want a row-level security violation", pgErr)
			}
		})
	}
}

func TestFailClosed_NoGUC_ReturnsZeroRows(t *testing.T) {
	env := dbtest.Start(t)
	dbtest.SeedTwoWorkspaces(t, env)
	tenant := tenantTables(t, connect(t, env.OwnerURL))

	inWorkspace(t, env.AppURL, nil, func(tx pgx.Tx) {
		for _, table := range tenant {
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
	newWorkspace := `INSERT INTO identity.workspaces (id, name, google_domain) VALUES (gen_random_uuid(), 'new.example', 'new.example')`
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
		{"app can read workspaces", env.AppURL, `SELECT count(*) FROM identity.workspaces`, ""},
		{"app cannot create a workspace", env.AppURL, newWorkspace, insufficientPrivilege},
		{"app cannot update a workspace", env.AppURL, `UPDATE identity.workspaces SET name = 'x'`, insufficientPrivilege},
		{"app cannot delete a workspace", env.AppURL, `DELETE FROM identity.workspaces`, insufficientPrivilege},
		{"system can read the outbox", env.SystemURL, `SELECT count(*) FROM platform.outbox`, ""},
		{"system can update the outbox", env.SystemURL, `UPDATE platform.outbox SET published_at = now()`, ""},
		{"system cannot enqueue", env.SystemURL, enqueue, insufficientPrivilege},
		{"system cannot delete from the outbox", env.SystemURL, `DELETE FROM platform.outbox`, insufficientPrivilege},
		{"system cannot touch processed events", env.SystemURL, `SELECT count(*) FROM platform.processed_events`, insufficientPrivilege},
		{"system sees every workspace", env.SystemURL, `SELECT count(*) FROM identity.workspaces`, ""},
		{"system can create a workspace", env.SystemURL, newWorkspace, ""},
		{"system cannot update a workspace", env.SystemURL, `UPDATE identity.workspaces SET name = 'x'`, insufficientPrivilege},
		{"system cannot delete a workspace", env.SystemURL, `DELETE FROM identity.workspaces`, insufficientPrivilege},
		{"system can read users", env.SystemURL, `SELECT count(*) FROM identity.users`, ""},
		{"system can update users", env.SystemURL, `UPDATE identity.users SET name = 'x'`, ""},
		{"system cannot delete users", env.SystemURL, `DELETE FROM identity.users`, insufficientPrivilege},
		{"system cannot read oauth tokens", env.SystemURL, `SELECT count(*) FROM identity.oauth_tokens`, insufficientPrivilege},
		{"system can read meetings", env.SystemURL, `SELECT count(*) FROM meetings.meetings`, ""},
		{"system cannot update meetings", env.SystemURL, `UPDATE meetings.meetings SET title = 'x'`, insufficientPrivilege},
		{"system cannot delete meetings", env.SystemURL, `DELETE FROM meetings.meetings`, insufficientPrivilege},
		{"system cannot read messages", env.SystemURL, `SELECT count(*) FROM chat.messages`, insufficientPrivilege},
		{"system cannot write channels", env.SystemURL, `UPDATE chat.channels SET name = 'x'`, insufficientPrivilege},
		{"system cannot read chunks", env.SystemURL, `SELECT count(*) FROM knowledge.chunks`, insufficientPrivilege},
		{"system cannot touch ref counters", env.SystemURL, `SELECT count(*) FROM platform.ref_counters`, insufficientPrivilege},
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

// TestRoles_GrantsAreExact pins every privilege kiwi_app, kiwi_system and
// kiwi_ro hold on the kiwi schemas: relation, sequence and schema privileges,
// column privileges (none are used) and default privileges. Any widening is a
// red build. A task that genuinely needs one more privilege adds a new goose
// migration for it and extends the lists below in the same change, so the
// review sees the new privilege.
//
// The ACLs come from pg_class/pg_namespace/pg_default_acl rather than
// information_schema.role_table_grants: that view omits sequences and PUBLIC
// grants and only shows roles the connection has enabled.
func TestRoles_GrantsAreExact(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)

	var want []string
	add := func(role, object string, privs ...string) {
		for _, p := range privs {
			want = append(want, role+" "+object+" "+p)
		}
	}
	dml := []string{"DELETE", "INSERT", "SELECT", "UPDATE"}
	for _, table := range []string{
		"identity.users", "identity.oauth_tokens", "chat.channels", "chat.channel_members", "chat.messages",
		"files.files", "meetings.meetings", "meetings.transcript_entries", "meetings.summaries",
		"meetings.space_subscriptions", "tasks.tasks", "knowledge.chunks", "agents.runs", "agents.sessions",
		"agents.session_events", "calendar.events",
	} {
		add("kiwi_app", table, dml...)
	}
	add("kiwi_app", "identity.workspaces", "SELECT")
	add("kiwi_app", "platform.ref_counters", "INSERT", "SELECT", "UPDATE")
	add("kiwi_app", "platform.outbox", "INSERT")
	add("kiwi_app", "platform.processed_events", "INSERT", "SELECT")
	add("kiwi_app", "agents.session_events_id_seq", "USAGE")
	add("kiwi_app", "platform.outbox_id_seq", "USAGE")
	// kiwi_system: the three db.System callers and nothing else. No DELETE.
	add("kiwi_system", "identity.workspaces", "INSERT", "SELECT")
	add("kiwi_system", "identity.users", "INSERT", "SELECT", "UPDATE")
	add("kiwi_system", "meetings.meetings", "SELECT")
	add("kiwi_system", "platform.outbox", "SELECT", "UPDATE")
	add("kiwi_ro", "chat.channels", "SELECT")
	add("kiwi_ro", "chat.channel_members", "SELECT")

	got := queryStrings(t, conn, `
		SELECT pg_get_userbyid(a.grantee) || ' ' || n.nspname || '.' || c.relname || ' ' || a.privilege_type
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace,
		LATERAL aclexplode(c.relacl) a
		WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S') AND a.grantee <> c.relowner`, kiwiSchemas)
	assertSameSet(t, "relation and sequence privileges", got, want)

	want = nil
	for _, schema := range []string{"identity", "chat", "files", "meetings", "tasks", "knowledge", "agents", "calendar", "platform"} {
		add("kiwi_app", schema, "USAGE")
	}
	for _, schema := range []string{"identity", "meetings", "platform"} {
		add("kiwi_system", schema, "USAGE")
	}
	add("kiwi_ro", "chat", "USAGE")
	got = queryStrings(t, conn, `
		SELECT pg_get_userbyid(a.grantee) || ' ' || n.nspname || ' ' || a.privilege_type
		FROM pg_namespace n, LATERAL aclexplode(n.nspacl) a
		WHERE n.nspname = ANY($1) AND a.grantee <> n.nspowner`, kiwiSchemas)
	assertSameSet(t, "schema privileges", got, want)

	// Default privileges: only kiwi_app, only the domain schemas.
	want = nil
	for _, schema := range []string{"identity", "chat", "files", "meetings", "tasks", "knowledge", "agents", "calendar"} {
		for _, p := range dml {
			want = append(want, "kiwi_app "+schema+" r "+p)
		}
		want = append(want, "kiwi_app "+schema+" S USAGE")
	}
	got = queryStrings(t, conn, `
		SELECT pg_get_userbyid(a.grantee) || ' ' || coalesce(n.nspname, '*') || ' ' || d.defaclobjtype::text || ' ' || a.privilege_type
		FROM pg_default_acl d
		LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace,
		LATERAL aclexplode(d.defaclacl) a
		WHERE a.grantee <> d.defaclrole`)
	assertSameSet(t, "default privileges", got, want)

	got = queryStrings(t, conn, `
		SELECT n.nspname || '.' || c.relname || '.' || a.attname
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ANY($1) AND a.attacl IS NOT NULL`, kiwiSchemas)
	assertSameSet(t, "column privileges", got, nil)
}

func assertSameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	got, want = slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("%s: unexpected grant %q", what, g)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s: missing grant %q", what, w)
		}
	}
}

func TestChunks_VectorAndHNSW(t *testing.T) {
	env := dbtest.Start(t)
	a, _ := dbtest.SeedTwoWorkspaces(t, env)

	version := scalar[string](t, connect(t, env.OwnerURL), `SELECT extversion FROM pg_extension WHERE extname = 'vector'`)
	major, minor, _ := strings.Cut(version, ".")
	minor, _, _ = strings.Cut(minor, ".")
	maj, _ := strconv.Atoi(major)
	mnr, _ := strconv.Atoi(minor)
	if maj == 0 && mnr < 8 {
		t.Fatalf("pgvector %s, the spec needs >= 0.8 (iterative index scans)", version)
	}
	t.Logf("pgvector %s", version)

	def := scalar[string](t, connect(t, env.OwnerURL),
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'knowledge' AND tablename = 'chunks' AND indexdef ILIKE '%hnsw%'`)
	if !strings.Contains(def, "USING hnsw (embedding vector_cosine_ops)") {
		t.Fatalf("HNSW index definition = %s, want USING hnsw (embedding vector_cosine_ops)", def)
	}

	var channel uuid.UUID
	seedIn(t, env, a, func(tx pgx.Tx) {
		channel = scalar[uuid.UUID](t, tx, `SELECT id FROM chat.channels`)
		// 1000 random 768-dim vectors; the correlated "n > 0" makes Postgres
		// draw a fresh vector for every row.
		if _, err := tx.Exec(t.Context(), `
			INSERT INTO knowledge.chunks
			  (id, workspace_id, channel_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
			SELECT gen_random_uuid(), $1, $2, 'doc', gen_random_uuid(), 'page:' || n, 'en', 'chunk ' || n, md5(n::text),
			       to_tsvector('simple', 'chunk ' || n), 'embeddinggemma',
			       (SELECT array_agg((random() * 2 - 1)::real) FROM generate_series(1, 768) WHERE n > 0)::vector
			FROM generate_series(1, 1000) AS n`, a, channel); err != nil {
			t.Fatalf("seed chunks: %v", err)
		}
	})
	analyze(t, env, `knowledge.chunks`)

	t.Run("wrong dimension is rejected", func(t *testing.T) {
		pgErr := execErrIn(t, env.OwnerURL, &a, `
			INSERT INTO knowledge.chunks
			  (id, workspace_id, source_kind, source_id, source_anchor, lang, content, content_sha256, tsv, embedding_model, embedding)
			VALUES (gen_random_uuid(), $1, 'doc', gen_random_uuid(), 'page:1', 'en', 'x', 'x', to_tsvector('simple', 'x'), 'm', '[1,2,3]')`, a)
		if pgErr == nil || pgErr.Code != "22000" { // data_exception: expected 768 dimensions, not 3
			t.Errorf("err = %v, want SQLSTATE 22000 (expected 768 dimensions)", pgErr)
		}
	})

	t.Run("nearest neighbour uses the HNSW index", func(t *testing.T) {
		var query string
		inWorkspace(t, env.OwnerURL, &a, func(tx pgx.Tx) {
			query = scalar[string](t, tx, `SELECT embedding::text FROM knowledge.chunks WHERE source_anchor = 'page:500'`)
		})
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

// analyze refreshes planner statistics as the table owner; kiwi_system and
// kiwi_app may not ANALYZE and silently skip it, which leaves the planner guessing.
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
	lines := queryStrings(t, tx, `EXPLAIN (COSTS OFF) `+sql, args...)
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

	var channel uuid.UUID
	seedIn(t, env, a, func(tx pgx.Tx) {
		channel = scalar[uuid.UUID](t, tx, `SELECT id FROM chat.channels`)
		if _, err := tx.Exec(t.Context(), `
			INSERT INTO chat.messages (id, workspace_id, channel_id, seq, author_kind, author_id, body, client_msg_id)
			SELECT gen_random_uuid(), $1, $2, n, 'user', gen_random_uuid(), 'message ' || n, 'c' || n
			FROM generate_series(2, 5000) AS n`, a, channel); err != nil {
			t.Fatalf("seed messages: %v", err)
		}
	})
	analyze(t, env, `chat.messages`)

	inWorkspace(t, env.AppURL, &a, func(tx pgx.Tx) {
		plan := explain(t, tx, `SELECT * FROM chat.messages WHERE channel_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT 50`, channel, 4000)
		if !strings.Contains(plan, "Index Scan Backward using messages_channel_id_seq_key") || strings.Contains(plan, "Sort") {
			t.Errorf("descending page query is not served by the (channel_id, seq) index:\n%s", plan)
		}
	})
}

func TestMeetings_RecordingFileIDIsANullableTypedReference(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)

	var dataType, nullable string
	err := conn.QueryRow(t.Context(), `
		SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema = 'meetings' AND table_name = 'meetings' AND column_name = 'recording_file_id'`).Scan(&dataType, &nullable)
	if err != nil {
		t.Fatalf("meetings.meetings.recording_file_id: %v", err)
	}
	if dataType != "uuid" || nullable != "YES" {
		t.Errorf("recording_file_id is %s, nullable=%s, want uuid, nullable=YES", dataType, nullable)
	}

	// A typed reference, not a constraint: meetings never points into the files schema.
	if n := scalar[int](t, conn, `
		SELECT count(*) FROM pg_constraint k
		JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = ANY (k.conkey)
		WHERE k.contype = 'f' AND k.conrelid = 'meetings.meetings'::regclass AND a.attname = 'recording_file_id'`); n != 0 {
		t.Errorf("recording_file_id is part of %d foreign keys, want none", n)
	}
}

// TestCrossSchemaForeignKeysOnlyReferenceIdentity pins spec 3.1: foreign keys
// leave a schema only towards identity.workspaces and identity.users; every
// other cross-domain link is a typed reference.
func TestCrossSchemaForeignKeysOnlyReferenceIdentity(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)

	for _, fk := range queryStrings(t, conn, `
		SELECT cn.nspname || '.' || c.relname || ' -> ' || pn.nspname || '.' || p.relname
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace cn ON cn.oid = c.relnamespace
		JOIN pg_class p ON p.oid = k.confrelid JOIN pg_namespace pn ON pn.oid = p.relnamespace
		WHERE k.contype = 'f' AND cn.nspname <> pn.nspname
		  AND pn.nspname || '.' || p.relname NOT IN ('identity.workspaces', 'identity.users')`) {
		t.Errorf("foreign key %s crosses a domain schema; use a typed reference", fk)
	}
}

// TestMigration_WorkspaceWithoutDomain_AllowedAndDomainsUniqueCaseInsensitively pins
// 0011: a workspace may have no Google domain (the sign-in rule for accounts without
// hd lives in such a workspace), any number of them may coexist at the database level,
// and two workspaces never share a domain whatever its letter case.
func TestMigration_WorkspaceWithoutDomain_AllowedAndDomainsUniqueCaseInsensitively(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)
	const insert = `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, $2, $3)`

	if nullable := scalar[string](t, conn, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'identity' AND table_name = 'workspaces' AND column_name = 'google_domain'`); nullable != "YES" {
		t.Errorf("identity.workspaces.google_domain is_nullable = %s, want YES", nullable)
	}

	for _, name := range []string{"no domain one", "no domain two"} {
		ws := uuid.New()
		seedIn(t, env, ws, func(tx pgx.Tx) {
			if _, err := tx.Exec(t.Context(), insert, ws, name, nil); err != nil {
				t.Fatalf("workspace %q without a domain: %v", name, err)
			}
		})
	}

	first := uuid.New()
	seedIn(t, env, first, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), insert, first, "demo", "demo.example"); err != nil {
			t.Fatalf("demo.example: %v", err)
		}
	})
	for _, domain := range []string{"demo.example", "Demo.Example", "DEMO.EXAMPLE"} {
		ws := uuid.New()
		pgErr := execErrIn(t, env.OwnerURL, &ws, insert, ws, "again", domain)
		if pgErr == nil || pgErr.Code != "23505" || pgErr.ConstraintName != "workspaces_google_domain_lower_key" {
			t.Errorf("a second workspace for %q: %v, want unique_violation on workspaces_google_domain_lower_key", domain, pgErr)
		}
	}
	other := uuid.New()
	if pgErr := execErrIn(t, env.OwnerURL, &other, insert, other, "other", "other.example"); pgErr != nil {
		t.Errorf("a workspace for another domain: %v", pgErr)
	}
}

// gooseProvider opens a goose provider over the embedded migrations as the role behind
// url (kiwi_owner), for the tests that go down a version, which migrate.Up cannot.
func gooseProvider(t *testing.T, url string) *goose.Provider {
	t.Helper()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMigration0011_Down_FailsLoudlyWhileAWorkspaceHasNoDomain pins the comment of
// 0011's Down: with a workspace without a domain it stops and leaves the schema as it
// was (goose runs it in a transaction); once none is left it restores the old shape,
// and Up brings 0011 back.
func TestMigration0011_Down_FailsLoudlyWhileAWorkspaceHasNoDomain(t *testing.T) {
	env := dbtest.Start(t)
	conn := connect(t, env.OwnerURL)
	p := gooseProvider(t, env.OwnerURL)
	const before = 10 // the version before 0011

	nullable := func() string {
		return scalar[string](t, conn, `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'identity' AND table_name = 'workspaces' AND column_name = 'google_domain'`)
	}
	version := func() int {
		return scalar[int](t, conn, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`)
	}

	ws := uuid.New()
	seedIn(t, env, ws, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, 'no domain', NULL)`, ws); err != nil {
			t.Fatalf("workspace without a domain: %v", err)
		}
	})
	if _, err := p.DownTo(t.Context(), before); err == nil || !strings.Contains(err.Error(), "google_domain") {
		t.Fatalf("Down with a workspace without a domain = %v, want an error naming google_domain", err)
	}
	if got := version(); got != before+1 || nullable() != "YES" {
		t.Fatalf("after the failed Down: version %d, google_domain nullable %s; want 11 and YES, the schema untouched", got, nullable())
	}

	seedIn(t, env, ws, func(tx pgx.Tx) {
		if _, err := tx.Exec(t.Context(), `DELETE FROM identity.workspaces WHERE id = $1`, ws); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := p.DownTo(t.Context(), before); err != nil {
		t.Fatalf("Down without one: %v", err)
	}
	if got := version(); got != before || nullable() != "NO" {
		t.Fatalf("after Down: version %d, google_domain nullable %s; want 10 and NO", got, nullable())
	}
	if n := scalar[int](t, conn, `SELECT count(*) FROM pg_constraint WHERE conname = 'workspaces_google_domain_key' AND contype = 'u'`); n != 1 {
		t.Errorf("the case-sensitive unique constraint is back %d times, want once", n)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
	// Up reapplies 0011 (google_domain nullable again) and every migration after it,
	// so the version returns to the latest, not to 0011.
	latest := latestMigrationVersion(t)
	if got := version(); got != latest || nullable() != "YES" {
		t.Errorf("after Up again: version %d, google_domain nullable %s; want %d and YES", got, nullable(), latest)
	}
}

// latestMigrationVersion is the number of the last embedded migration (they are
// numbered 1..N without gaps), so a test that goes down and back up knows where up
// lands without hard-coding it.
func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("embedded migrations: %v (%d files)", err, len(files))
	}
	return len(files)
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
