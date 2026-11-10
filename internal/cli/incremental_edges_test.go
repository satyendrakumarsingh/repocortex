package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callsEdges(t *testing.T, root string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, ".cortex", "index.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM edges
		 WHERE kind = 'CALLS'
		   AND index_run_id = (SELECT id FROM index_runs WHERE committed = 1)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// writeCallerCallee lays out a caller in one file and its callee in another, so
// that editing the callee exercises the cross-file path. A single-file fixture
// would not: the reference and the definition would be replaced together.
func writeCallerCallee(t *testing.T, root string) (callee string) {
	t.Helper()
	for _, d := range []string{"src/api", "src/db"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module demo\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callee = filepath.Join(root, "src", "api", "handler.go")
	if err := os.WriteFile(callee, []byte("package api\n\nfunc Handle() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "db", "store.go"),
		[]byte("package db\n\nimport \"demo/src/api\"\n\nfunc Reload() { api.Handle() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return callee
}

// TestEditingADefinitionKeepsItsCallerEdges pins a silent graph regression.
//
// symbol_references.resolved_symbol_id is ON DELETE SET NULL. Replacing a file
// deletes its symbols, so the cascade cleared the pointer on every reference
// bound to them — but left resolution = 'resolved'. Resolve only visits
// references marked 'pending', so those rows were never revisited: each
// claimed to be resolved while pointing at nothing, and the call edge stayed
// missing until a full re-index.
//
// Editing a definition therefore deleted its callers' edges, permanently and
// without a symptom. `cortex callers` returned nothing, and `cortex arch`
// reported "No violations" on a repo whose forbidden dependency was still
// there — a false clean bill of health, which is the one governance failure
// nobody investigates.
func TestEditingADefinitionKeepsItsCallerEdges(t *testing.T) {
	root := isolate(t)
	callee := writeCallerCallee(t, root)

	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	before := callsEdges(t, root)
	if before != 1 {
		t.Fatalf("setup failed — expected 1 CALLS edge, got %d", before)
	}

	// Edit the DEFINITION, repeatedly. The bug bit the file being pointed at,
	// not the one doing the pointing, and it recurred on every edit.
	for i := 0; i < 3; i++ {
		f, err := os.OpenFile(callee, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("\n// edit\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		for _, args := range [][]string{{"index"}, {"resolve"}} {
			if out, err := run(t, root, args...); err != nil {
				t.Fatalf("edit %d, %v: %v\n%s", i, args, err, out)
			}
		}
		if got := callsEdges(t, root); got != 1 {
			t.Fatalf("edit %d: CALLS edges %d, want 1 — editing a definition dropped its caller's edge", i, got)
		}
	}

	// The user-visible symptom, not just the row count.
	out, err := run(t, root, "callers", "Handle")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Reload") {
		t.Errorf("callers no longer reports the caller after edits:\n%s", out)
	}
}

// TestResolveRepairsOrphanedReferences pins the self-heal.
//
// Indexes written before the fix above still contain references marked
// resolved that point at nothing. Those users must not need a full re-index to
// recover a correct graph, so resolve treats the broken invariant as pending
// work. This injects the exact damaged state rather than trusting that the
// repair path is reachable.
func TestResolveRepairsOrphanedReferences(t *testing.T) {
	root := isolate(t)
	writeCallerCallee(t, root)

	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if callsEdges(t, root) != 1 {
		t.Fatal("setup failed — no CALLS edge to lose")
	}

	dbPath := filepath.Join(root, ".cortex", "index.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM edges WHERE kind = 'CALLS'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		UPDATE symbol_references
		   SET resolution = 'resolved', resolved_symbol_id = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if callsEdges(t, root) != 0 {
		t.Fatal("failed to inject the damaged state")
	}

	// resolve alone — no re-index — must recover the edge.
	if out, err := run(t, root, "resolve"); err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	if got := callsEdges(t, root); got != 1 {
		t.Errorf("resolve did not repair the orphaned reference: CALLS edges %d, want 1", got)
	}
}
