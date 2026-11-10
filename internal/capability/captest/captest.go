// Package captest builds in-memory fixtures for the capability packages'
// tests.
//
// It exists so impact and tests can both assert against the REAL registry and
// the REAL graph rather than against a fake that re-implements traversal.
//
// That choice matters more than it looks. The central claim of
// capability/impact is "confidence compounds along the path" — and that
// arithmetic lives in internal/graph. A fake Graph that returned pre-computed
// path minima would prove only that the fake was written to agree with the
// assertion. Driving the same SQLite schema production uses means the test
// fails if internal/graph ever stops compounding, which is the failure that
// actually matters.
//
// Rows are written with direct SQL rather than through registry.Writer for the
// same reason internal/graph's fixtures are: a capability test should fail when
// a capability breaks, not when an unrelated write path changes, and the
// benchmarks need to load 600k rows in seconds.
//
// The capability.Deps returned by Build satisfies the same interfaces the MCP
// server wires up, so nothing here is a parallel implementation of the
// production path.
package captest

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Sym is one symbol to seed.
type Sym struct {
	ID       int64
	Name     string
	FQN      string
	Kind     string
	Path     string
	Exported bool

	// Partial marks this symbol's FILE as one the parser could not read
	// completely. It hangs off the symbol because that is where a test names a
	// file, but it is a property of the file: every symbol sharing the path
	// sees the same status.
	Partial bool
}

// S is the common case: a function, FQN derived from path and name.
func S(id int64, name, filePath string) Sym {
	return Sym{ID: id, Name: name, FQN: filePath + "#" + name, Kind: "function", Path: filePath}
}

// Exported marks a symbol as part of the public surface.
func (s Sym) Export() Sym { s.Exported = true; return s }

// WithKind overrides the entity kind.
func (s Sym) WithKind(k string) Sym { s.Kind = k; return s }

// WithFQN overrides the fully-qualified name.
func (s Sym) WithFQN(fqn string) Sym { s.FQN = fqn; return s }

// PartialParse marks this symbol's file as partially parsed, so a test can
// exercise the path where a count is a floor rather than a total.
//
// Real repositories make this awkward to reach: a file whose parse failed
// usually yields no symbols at all, so it never appears in a view that draws
// symbol-bearing files — which is exactly why the true branch needs a fixture
// to be testable at all.
func (s Sym) PartialParse() Sym { s.Partial = true; return s }

// Edge is one relationship to seed.
type Edge struct {
	Src, Dst int64
	Kind     model.EdgeKind
	Conf     model.Confidence
}

// E is the common case: a high-confidence CALLS edge.
func E(src, dst int64) Edge {
	return Edge{Src: src, Dst: dst, Kind: model.EdgeCalls, Conf: model.ConfidenceHigh}
}

// Conf overrides the edge confidence.
func (e Edge) WithConf(c model.Confidence) Edge { e.Conf = c; return e }

// WithKind overrides the edge kind.
func (e Edge) WithKind(k model.EdgeKind) Edge { e.Kind = k; return e }

// Refs are reference-resolution outcomes to seed, so a report's completeness
// block has real numbers behind it instead of zeros.
type Refs struct {
	Resolved   int
	Ambiguous  int
	Unresolved int
	Pending    int
	Ignored    int
}

// Build creates a committed run holding syms and edges and returns Deps over
// the real registry and graph, plus the run id.
func Build(tb testing.TB, syms []Sym, edges []Edge, refs Refs) (capability.Deps, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	runID := seedRun(tb, db, syms, edges, refs)
	return capability.NewDeps(registry.New(db), graph.New(db)), runID
}

func seedRun(tb testing.TB, db *store.DB, syms []Sym, edges []Edge, refs Refs) int64 {
	tb.Helper()
	ctx := context.Background()

	res, err := db.ExecContext(ctx,
		`INSERT INTO index_runs (status, committed, started_unix) VALUES ('ready', 1, 0)`)
	if err != nil {
		tb.Fatalf("insert run: %v", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		tb.Fatalf("run id: %v", err)
	}

	// Files are inserted BEFORE any transaction is opened. An in-memory store
	// runs with MaxOpenConns(1) — a second connection to ":memory:" would be a
	// second, empty database — so issuing a db.Exec while a Tx holds that one
	// connection deadlocks the pool. Doing all the file inserts up front keeps
	// every later statement inside a single transaction.
	files := insertFiles(tb, db, runID, filePaths(syms), partialPaths(syms))
	fileID := func(p string) int64 {
		if p == "" {
			p = "unknown.go"
		}
		id, ok := files[p]
		if !ok {
			tb.Fatalf("captest: no file row for %q (fixture bug)", p)
		}
		return id
	}

	inTx(tb, db, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `
			INSERT INTO symbols (id, index_run_id, file_id, kind, name, fqn,
			                     start_line, exported, confidence, source)
			VALUES (?,?,?,?,?,?,?,?,'high','source_code')`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i, s := range syms {
			exported := 0
			if s.Exported {
				exported = 1
			}
			kind := s.Kind
			if kind == "" {
				kind = "function"
			}
			fqn := s.FQN
			if fqn == "" {
				fqn = s.Path + "#" + s.Name
			}
			if _, err := st.ExecContext(ctx, s.ID, runID, fileID(s.Path), kind,
				s.Name, fqn, i+1, exported); err != nil {
				return fmt.Errorf("symbol %d (%s): %w", s.ID, s.Name, err)
			}
		}
		return nil
	})

	inTx(tb, db, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `
			INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
			                   evidence_file, evidence_line, evidence_col, source)
			VALUES (?,?,?,?,?,?,?,0,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for i, e := range edges {
			// Invariant 2 is enforced by a SQL CHECK, so an exact fixture edge
			// must claim a compiler source or the insert is rejected.
			source := model.SourceSourceCode
			if e.Conf == model.ConfidenceExact {
				source = model.SourceCompiler
			}
			kind := e.Kind
			if kind == "" {
				kind = model.EdgeCalls
			}
			conf := e.Conf
			if conf == "" {
				conf = model.ConfidenceHigh
			}
			if _, err := st.ExecContext(ctx, runID, e.Src, e.Dst, string(kind), string(conf),
				"edge.go", i+1, string(source)); err != nil {
				return fmt.Errorf("edge %d->%d: %w", e.Src, e.Dst, err)
			}
		}
		return nil
	})

	seedRefs(tb, db, runID, fileID("refs.go"), refs)
	return runID
}

// filePaths collects the distinct file paths a fixture needs, including the
// synthetic file the reference rows hang off.
func filePaths(syms []Sym) []string {
	seen := map[string]struct{}{"refs.go": {}, "unknown.go": {}}
	out := []string{"refs.go", "unknown.go"}
	for _, s := range syms {
		p := s.Path
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// partialPaths is the set of files any seeded symbol marked as partially
// parsed.
func partialPaths(syms []Sym) map[string]bool {
	out := map[string]bool{}
	for _, s := range syms {
		if s.Partial {
			out[s.Path] = true
		}
	}
	return out
}

func insertFiles(tb testing.TB, db *store.DB, runID int64, paths []string, partial map[string]bool) map[string]int64 {
	tb.Helper()
	ctx := context.Background()
	out := make(map[string]int64, len(paths))
	for _, p := range paths {
		status := "ok"
		if partial[p] {
			status = "partial"
		}
		r, err := db.ExecContext(ctx,
			`INSERT INTO files (index_run_id, path, language, content_hash, parse_status) VALUES (?,?,?,?,?)`,
			runID, p, langOf(p), "hash-"+p, status)
		if err != nil {
			tb.Fatalf("insert file %s: %v", p, err)
		}
		id, err := r.LastInsertId()
		if err != nil {
			tb.Fatalf("file id: %v", err)
		}
		out[p] = id
	}
	return out
}

func seedRefs(tb testing.TB, db *store.DB, runID, fileID int64, refs Refs) {
	tb.Helper()
	ctx := context.Background()
	counts := []struct {
		res registry.Resolution
		n   int
	}{
		{registry.ResolutionResolved, refs.Resolved},
		{registry.ResolutionAmbiguous, refs.Ambiguous},
		{registry.ResolutionUnresolved, refs.Unresolved},
		{registry.ResolutionPending, refs.Pending},
		{registry.ResolutionIgnored, refs.Ignored},
	}
	inTx(tb, db, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `
			INSERT INTO symbol_references (index_run_id, file_id, name, kind, resolution)
			VALUES (?,?,?, 'reference.call', ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, c := range counts {
			for i := 0; i < c.n; i++ {
				if _, err := st.ExecContext(ctx, runID, fileID,
					fmt.Sprintf("ref%s%d", c.res, i), string(c.res)); err != nil {
					return err
				}
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

func langOf(p string) string {
	switch path.Ext(p) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".java":
		return "java"
	default:
		return "unknown"
	}
}
