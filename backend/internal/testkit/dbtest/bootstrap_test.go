package dbtest

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// localPostgresInit is the bootstrap the k3d stack runs on its first start.
// dbtest carries its own copy of it (the roles table in dbtest.go); the two are
// compared here so a role added, renamed or widened on one side only is a red
// build. scripts/local/render.test.sh pins the same facts from the manifest side.
const localPostgresInit = "../../../../deploy/k8s/overlays/local/deps/postgres-init.yaml"

// createRole matches `CREATE ROLE name ATTR... PASSWORD`; the attributes sit
// between the name and PASSWORD in the manifest.
var createRole = regexp.MustCompile(`(?m)^\s*CREATE ROLE (\w+)((?: [A-Z]+)*) PASSWORD `)

func TestRoles_MatchLocalPostgresInit(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(localPostgresInit))
	if err != nil {
		t.Fatalf("read the local bootstrap: %v", err)
	}
	init := string(raw)

	got := map[string][]string{}
	for _, m := range createRole.FindAllStringSubmatch(init, -1) {
		attrs := strings.Fields(m[2])
		slices.Sort(attrs)
		got[m[1]] = attrs
	}
	if n := strings.Count(init, "CREATE ROLE"); n != len(got) {
		t.Fatalf("postgres-init has %d CREATE ROLE statements, %d of them in the shape this test reads", n, len(got))
	}

	want := map[string][]string{}
	for _, r := range roles {
		attrs := append([]string{"LOGIN"}, strings.Fields(r.attrs)...)
		slices.Sort(attrs)
		want[r.name] = attrs
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("role attributes differ\n postgres-init: %v\n dbtest:        %v", got, want)
	}

	// The database is owned by the role that runs goose, and the extension that
	// only a superuser may create is installed inside it.
	const owner = "kiwi_owner"
	if _, ok := want[owner]; !ok {
		t.Fatalf("dbtest creates no %s role", owner)
	}
	if !strings.Contains(init, "CREATE DATABASE kiwi OWNER "+owner+";") {
		t.Errorf("postgres-init does not create database kiwi owned by %s", owner)
	}
	if !strings.Contains(init, "CREATE EXTENSION vector;") {
		t.Error("postgres-init does not create the vector extension")
	}
}
