package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// systemCallers is every non-test file allowed to call DB.System, which
// connects as the BYPASSRLS role. Adding a caller is a security decision: it
// is made here, in review, and nowhere else.
var systemCallers = []string{
	"internal/identity/callback.go",
	"internal/platform/outbox/relay.go",
	"internal/meetings/poll.go",
	"internal/knowledge/reindex.go",
}

// callsSystem reports whether src contains a call of a method or function
// named System. It parses rather than greps, so the declaration of DB.System,
// comments and string literals do not count.
func callsSystem(filename string, src any) (bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return false, err
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "System" {
				found = true
			}
		}
		return !found
	})
	return found, nil
}

func TestSystemCallersAreAllowlisted(t *testing.T) {
	backend := filepath.Join("..", "..", "..") // this package is backend/internal/platform/db
	if _, err := os.Stat(filepath.Join(backend, "go.mod")); err != nil {
		t.Fatalf("backend root not found from the test directory: %v", err)
	}

	var parsed, callers []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(backend, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(backend, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			found, err := callsSystem(path, nil)
			if err != nil {
				return err
			}
			parsed = append(parsed, rel)
			if found {
				callers = append(callers, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	// A wrong root would find nothing and pass; require the declaring file.
	if !slices.Contains(parsed, "internal/platform/db/db.go") {
		t.Fatalf("the walk did not reach internal/platform/db/db.go (parsed %d files)", len(parsed))
	}
	for _, rel := range callers {
		if !slices.Contains(systemCallers, rel) {
			t.Errorf("%s calls db.System but is not on the allowlist %v: the system pool bypasses row-level security, so adding a caller needs a deliberate edit of this test", rel, systemCallers)
		}
	}
}

func TestSystemCallDetector(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"method call", "package p\nfunc f(d *DB) { _ = d.System(ctx, nil) }", true},
		{"call through a field", "package p\nfunc f(s S) { s.db.System(ctx, nil) }", true},
		{"call inside a closure", "package p\nfunc f(d *DB) { go func() { d.System(ctx, nil) }() }", true},
		{"declaration", "package p\nfunc (d *DB) System(ctx context.Context) error { return nil }", false},
		{"comment", "package p\n// d.System(ctx, fn) is the only door\nfunc f() {}", false},
		{"string", "package p\nvar s = \"d.System(ctx, fn)\"", false},
		{"other selector", "package p\nfunc f(d *DB) { d.InWorkspace(ctx, ws, nil) }", false},
		{"unqualified call", "package p\nfunc f() { System() }", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := callsSystem("x.go", tc.src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("callsSystem = %v, want %v", got, tc.want)
			}
		})
	}
}
