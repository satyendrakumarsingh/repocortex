package graph

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Fixtures write rows with direct SQL rather than going through
// registry.Writer. Two reasons: this package's tests should fail when graph
// traversal breaks, not when an unrelated write path changes, and the
// benchmarks need to load 600k rows in seconds, which a validating,
// batch-rotating Writer is not built for.

type tedge struct {
	src, dst int64
	kind     model.EdgeKind
	conf     model.Confidence
}

// e is the common case: a CALLS edge with high confidence.
func e(src, dst int64) tedge {
	return tedge{src: src, dst: dst, kind: model.EdgeCalls, conf: model.ConfidenceHigh}
}

func (t tedge) withConf(c model.Confidence) tedge { t.conf = c; return t }
func (t tedge) withKind(k model.EdgeKind) tedge   { t.kind = k; return t }

// build creates a committed run holding a symbol for every id mentioned in
// edges (plus any isolated extras) and returns a Graph over it.
func build(tb testing.TB, edges []tedge, isolated ...int64) *Graph {
	tb.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	ids := make(map[int64]struct{})
	for _, ed := range edges {
		ids[ed.src] = struct{}{}
		ids[ed.dst] = struct{}{}
	}
	for _, id := range isolated {
		ids[id] = struct{}{}
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}

	runID := seedRun(tb, db, true, list)
	insertEdges(tb, db, runID, edges)
	return New(db)
}

// seedRun inserts a run with one file and a symbol per id.
//
// committed is a parameter because idx_index_runs_current permits only one
// committed row: a test that wants a second, invisible run must ask for an
// uncommitted one.
func seedRun(tb testing.TB, db *store.DB, committed bool, symbolIDs []int64) int64 {
	tb.Helper()
	ctx := context.Background()

	flag := 0
	if committed {
		flag = 1
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO index_runs (status, committed, started_unix) VALUES ('ready', ?, 0)`, flag)
	if err != nil {
		tb.Fatalf("insert run: %v", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		tb.Fatalf("run id: %v", err)
	}

	res, err = db.ExecContext(ctx,
		`INSERT INTO files (index_run_id, path, language, content_hash) VALUES (?, 'a.go', 'go', 'hash')`, runID)
	if err != nil {
		tb.Fatalf("insert file: %v", err)
	}
	fileID, err := res.LastInsertId()
	if err != nil {
		tb.Fatalf("file id: %v", err)
	}

	inTx(tb, db, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `
			INSERT INTO symbols (id, index_run_id, file_id, kind, name, fqn,
			                     start_line, confidence, source)
			VALUES (?,?,?,'function',?,?,?,'high','source_code')`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, id := range symbolIDs {
			name := fmt.Sprintf("sym%d", id)
			if _, err := st.ExecContext(ctx, id, runID, fileID, name, "a.go#"+name, id); err != nil {
				return err
			}
		}
		return nil
	})
	return runID
}

func insertEdges(tb testing.TB, db *store.DB, runID int64, edges []tedge) {
	tb.Helper()
	ctx := context.Background()

	inTx(tb, db, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `
			INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, evidence_col, source)
			VALUES (?,?,?,?,?,'a.go',?,0,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i, ed := range edges {
			// Invariant 2 is enforced by a SQL CHECK, so an exact fixture edge
			// must claim a compiler source or the insert is rejected.
			source := model.SourceSourceCode
			if ed.conf == model.ConfidenceExact {
				source = model.SourceCompiler
			}
			if _, err := st.ExecContext(ctx, runID, ed.src, ed.dst,
				string(ed.kind), string(ed.conf), i+1, string(source)); err != nil {
				return fmt.Errorf("edge %d->%d: %w", ed.src, ed.dst, err)
			}
		}
		return nil
	})
}

func inTx(tb testing.TB, db *store.DB, fn func(*sql.Tx) error) {
	tb.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		tb.Fatalf("tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// ids extracts symbol ids from a result in their returned order.
func ids(nodes []Node) []int64 {
	out := make([]int64, len(nodes))
	for i, n := range nodes {
		out[i] = n.SymbolID
	}
	return out
}

func nodeByID(tb testing.TB, res Result, id int64) Node {
	tb.Helper()
	for _, n := range res.Nodes {
		if n.SymbolID == id {
			return n
		}
	}
	tb.Fatalf("symbol %d not in result %v", id, ids(res.Nodes))
	return Node{}
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
