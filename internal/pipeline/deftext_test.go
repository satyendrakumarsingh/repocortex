package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// longServiceModule is a module whose one function about auditing sits under
// thousands of lines of others, as task_service.py's does on a real service.
func longServiceModule() string {
	var b strings.Builder
	b.WriteString("import logging\n\n")
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "def helper_%d(value):\n    return value + %d\n\n", i, i)
	}
	b.WriteString(`def _record_transition(session, task, previous, current):
    entry = AuditLogEntry(task_id=task.id, old_status=previous, new_status=current)
    session.add(entry)
    logging.info("audit log entry written for status change")

def advance_task(session, task, current):
    previous = task.status
    task.status = current
    _record_transition(session, task, previous, current)
`)
	return b.String()
}

func definitionID(t *testing.T, reg *registry.Registry, runID int64, name string) int64 {
	t.Helper()
	cands, _, err := reg.SymbolCandidates(context.Background(), runID, name, 5)
	if err != nil || len(cands) != 1 {
		t.Fatalf("symbol %s: %d candidates, err %v", name, len(cands), err)
	}
	return cands[0].ID
}

func searchesTo(t *testing.T, reg *registry.Registry, query string, id int64) bool {
	t.Helper()
	got, err := reg.SearchDefinitions(context.Background(), query, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.SymbolID == id {
			return true
		}
	}
	return false
}

// Each function's own text is indexed when the file is, removed with it, and
// rebuilt from the stored file text for an index made before it existed.
func TestDefinitionTextFollowsTheIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"app/services/task_service.py": longServiceModule()})
	dbPath := filepath.Join(t.TempDir(), "index.db")
	reg, runID, _ := indexTree(t, root, dbPath)

	deep := definitionID(t, reg, runID, "_record_transition")
	if !searchesTo(t, reg, `"audit" AND "status"`, deep) {
		t.Fatal("the function at line 4,500 is not found by its own text")
	}
	if searchesTo(t, reg, `"audit"`, definitionID(t, reg, runID, "helper_7")) {
		t.Fatal("a helper is found by text that is only in another function")
	}
	matched, err := reg.DefinitionsMatching(ctx, `"entry"`, []int64{deep, definitionID(t, reg, runID, "advance_task")})
	if err != nil || len(matched) != 1 || matched[0] != deep {
		t.Fatalf("DefinitionsMatching = %v, %v; want only the function that says entry", matched, err)
	}

	// An index built before definition text existed: the rows are rebuilt
	// from the stored file text, once.
	raw, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM defs_fts`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if searchesTo(t, reg, `"audit"`, deep) {
		t.Fatal("rows survived the wipe")
	}
	n, err := reg.BackfillDefinitionText(ctx, runID)
	if err != nil || n != 1502 {
		t.Fatalf("backfill wrote %d definitions, err %v; want 1502", n, err)
	}
	if !searchesTo(t, reg, `"audit" AND "status"`, deep) {
		t.Fatal("the backfill did not restore the deep function's text")
	}
	if n, err := reg.BackfillDefinitionText(ctx, runID); err != nil || n != 0 {
		t.Fatalf("a second backfill wrote %d, err %v; want 0", n, err)
	}

	// Deleting the file deletes its definitions' rows: FTS5 does not cascade.
	if err := reg.DeleteFile(ctx, runID, "app/services/task_service.py", registry.AllowCommittedRun()); err != nil {
		t.Fatal(err)
	}
	// Counted in the FTS table itself: a search joins symbols, so an orphaned
	// row would be invisible to it and still cost space and rank.
	raw, err = store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var left int
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM defs_fts WHERE defs_fts MATCH '"value"'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("after delete: %d definition rows left; want none", left)
	}
}
