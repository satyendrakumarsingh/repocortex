package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestTableSymbolIdentityDoesNotDependOnInsertionOrder is a determinism
// regression test with a measured failure behind it.
//
// `tableSymbols` resolves a table name to ONE canonical symbol so that a table
// created in one migration and altered in three later ones stays one node. It
// did that with "first declaration wins" over a SELECT with no ORDER BY — and an
// unordered SELECT has no first. Row order followed the query plan over ids that
// the PARALLEL indexing pass assigned, so the winner varied run to run.
//
// Measured on this repository before the fix: `table_accesses` is created in
// 0003_data_access.sql and named again by a CREATE INDEX in 0005_fk_indexes.sql.
// Two runs over byte-identical source produced different graphs — 40+ READS and
// WRITES edges pointing at a different table node, and two table symbols
// alternately orphaned — and the registry digest disagreed across repeats and
// across worker counts. That is a direct breach of the guarantee the product
// rests on: the same bytes must produce the same graph.
//
// The fix orders by path then line, so "first" means first in the migration
// sequence — which is also where the CREATE TABLE is, making the canonical node
// the correct one. This test seeds the two files in BOTH insertion orders, which
// is what varies under concurrency, and requires the same answer from each.
func TestTableSymbolIdentityDoesNotDependOnInsertionOrder(t *testing.T) {
	ctx := context.Background()

	// The later migration is inserted FIRST here, so it takes the lower symbol
	// id — the exact shape that let the wrong file win before the fix.
	created := fileSpec{
		path: "migrations/0003_create.sql", lang: "sql", hash: "h3",
		syms: []symSpec{{fqn: "table:orders", name: "orders", line: 1, kind: model.KindTable}},
	}
	indexed := fileSpec{
		path: "migrations/0005_index.sql", lang: "sql", hash: "h5",
		syms: []symSpec{{fqn: "table:orders", name: "orders", line: 1, kind: model.KindTable}},
	}

	pick := func(t *testing.T, files []fileSpec) (int64, map[int64]string) {
		t.Helper()
		r, _ := newTestRegistry(t)
		runID, err := r.BeginRun(ctx, "auto", "rev")
		if err != nil {
			t.Fatalf("begin run: %v", err)
		}
		writeContent(t, r, runID.ID, files, nil)

		got, err := r.tableSymbols(ctx, runID.ID)
		if err != nil {
			t.Fatalf("tableSymbols: %v", err)
		}
		id, ok := got["orders"]
		if !ok {
			t.Fatalf("table 'orders' did not resolve at all: %v", got)
		}
		paths := map[int64]string{}
		rows, err := r.db.QueryContext(ctx,
			`SELECT s.id, f.path FROM symbols s JOIN files f ON f.id = s.file_id
			  WHERE s.index_run_id = ? AND s.kind = ?`, runID.ID, string(model.KindTable))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var sid int64
			var p string
			if err := rows.Scan(&sid, &p); err != nil {
				t.Fatal(err)
			}
			paths[sid] = p
		}
		return id, paths
	}

	idA, pathsA := pick(t, []fileSpec{indexed, created})
	idB, pathsB := pick(t, []fileSpec{created, indexed})

	if pathsA[idA] != pathsB[idB] {
		t.Fatalf("insertion order changed which file owns the table: %q vs %q",
			pathsA[idA], pathsB[idB])
	}
	// And it must be the CREATE, not the later CREATE INDEX — the canonical node
	// should also be the correct one.
	if want := "migrations/0003_create.sql"; pathsA[idA] != want {
		t.Errorf("table bound to %q, want the earliest migration %q", pathsA[idA], want)
	}
}
