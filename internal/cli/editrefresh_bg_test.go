package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// A branch switch changes thousands of files; one tool call must not wait for
// all of them. The call re-indexes what its cap allows, says how many remain,
// and a background pass holding the same reindex.lock finishes the rest while
// later calls report progress.
func TestALargeEditIsFinishedInTheBackground(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod": "module example.com/edit\n\ngo 1.22\n",
		"a.go":   "package edit\n\nfunc Alpha() int { return 1 }\n",
	})
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	g := &Global{Project: root, Quiet: true}
	cmd := NewRoot()
	cmd.SetArgs([]string{"--project", root, "--quiet", "version"})
	cmd.SetOut(io.Discard)
	if err := g.setup(cmd); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, IndexDir, IndexFile)
	r := newEditRefresher(g, dbPath)
	r.maxFiles, r.chunk, r.syncBudget = 2, 2, 0
	gate := make(chan struct{})
	r.beforeChunk = func() { <-gate }
	defer r.Close()

	more := map[string]string{}
	for _, n := range []string{"b", "c", "d", "e", "f", "g"} {
		more[n+".go"] = "package edit\n\nfunc " + strings.ToUpper(n) + "() int { return 1 }\n"
	}
	writeFiles(t, root, more)

	ctx := context.Background()
	first := r.Refresh(ctx)
	if first.Err != nil {
		t.Fatal(first.Err)
	}
	if len(first.Refreshed) != 2 || first.Pending != 4 {
		t.Fatalf("first call: refreshed %v, pending %d; want 2 files and 4 pending", first.Refreshed, first.Pending)
	}
	// The background pass holds the lock, so no other writer gets in.
	if lock, err := fs.TryLock(filepath.Join(root, IndexDir, "reindex.lock")); err == nil {
		_ = lock.Unlock()
		t.Fatal("the background pass does not hold reindex.lock")
	}
	if mid := r.Refresh(ctx); mid.Pending != 4 || len(mid.Refreshed) != 0 {
		t.Errorf("a call during the pass: pending %d refreshed %v; want 4 and none", mid.Pending, mid.Refreshed)
	}

	close(gate) // let the pass run
	deadline := time.Now().Add(30 * time.Second)
	var last devmcp.RefreshOutcome
	for time.Now().Before(deadline) {
		last = r.Refresh(ctx)
		if last.Pending == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.Pending != 0 || len(last.Refreshed) != 4 {
		t.Fatalf("after the pass: pending %d refreshed %v; want 0 and the 4 remaining files", last.Pending, last.Refreshed)
	}
	if lock, err := fs.TryLock(filepath.Join(root, IndexDir, "reindex.lock")); err != nil {
		t.Errorf("the lock was not released: %v", err)
	} else {
		_ = lock.Unlock()
	}
	db, err := store.Open(ctx, store.Options{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE path LIKE '_.go'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("%d single-letter .go files in the index, want 7", n)
	}
}
