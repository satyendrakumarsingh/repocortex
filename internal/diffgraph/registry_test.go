package diffgraph

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// One test against the REAL adapter, because the fakes above prove the
// classification logic and nothing else. This proves the port is satisfiable by
// internal/registry in practice — that the ids, file joins and evidence columns
// line up the way the reduction assumes.
//
// Three separate index databases, because that is what production looks like:
// registry.CommitRun retires the previously committed run and DELETES its rows,
// so one index file holds exactly one committed run and the two sides of a real
// diff cannot live in the same store. (An in-memory store would not do here
// either: store.OpenMemory uses "file::memory:?cache=shared", so two of them
// open at once are the same database.)
func TestAgainstTheRealRegistry(t *testing.T) {
	// Two passes over the same content, in two stores. Different rowids,
	// identical facts.
	regA, runA := indexedStore(t, "rev1", model.ConfidenceExact, model.SourceCompiler, 0)
	regB, runB := indexedStore(t, "rev1", model.ConfidenceExact, model.SourceCompiler, 0)

	sideA := Side{Source: regA, RunID: runA}
	sideB := Side{Source: regB, RunID: runB}

	rep := mustDiffAcross(t, sideA, sideB)
	if !rep.Empty() {
		t.Fatalf("two indexing passes over the same content must compare empty:\n%s", render(t, rep))
	}
	if !rep.Caveats.DigestsEqual {
		t.Errorf("the registry digest itself should call these runs identical; got base=%q head=%q",
			rep.Base.Digest, rep.Head.Digest)
	}

	// A third index: the file gained an import (every definition shifts down by
	// two lines) and the compiler stopped proving the call.
	regC, runC := indexedStore(t, "rev2", model.ConfidenceHigh, model.SourceSourceCode, 2)

	rep = mustDiffAcross(t, sideA, Side{Source: regC, RunID: runC})

	if len(rep.Edges) != 1 || rep.Edges[0].Change != EdgeConfidenceChanged {
		t.Fatalf("expected exactly one confidence transition, got %+v", rep.Edges)
	}
	if rep.Edges[0].Direction != Downgraded {
		t.Errorf("exact -> high must read as a downgrade, got %q", rep.Edges[0].Direction)
	}
	if rep.Edges[0].From.Confidence != model.ConfidenceExact || rep.Edges[0].To.Confidence != model.ConfidenceHigh {
		t.Errorf("transition must carry both sides: %+v -> %+v", rep.Edges[0].From, rep.Edges[0].To)
	}
	for _, c := range rep.Symbols {
		if c.Change != SymbolMoved {
			t.Errorf("a two-line shift must report as a move, got %s for %s", c.Change, c.Key)
		}
		if c.LineDelta != 2 {
			t.Errorf("%s: line delta %d, want 2", c.Key, c.LineDelta)
		}
	}
	if len(rep.Symbols) != 2 {
		t.Errorf("expected both symbols moved, got %+v", rep.Symbols)
	}
}

// indexedStore opens a fresh on-disk index, indexes one two-symbol, one-edge
// repository into it and commits.
func indexedStore(t *testing.T, rev string,
	conf model.Confidence, source model.SourceType, shift int) (*registry.Registry, int64) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reg := registry.New(db)

	run, err := reg.BeginRun(ctx, "auto", rev)
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	file := &model.File{
		Path: "pkg/api.go", Language: "go", ContentHash: "hash-api",
		SizeBytes: 120, ParseStatus: "ok",
	}
	fileID, err := w.AddFile(ctx, file)
	if err != nil {
		t.Fatalf("add file: %v", err)
	}

	mkSym := func(fqn, name string, line int) int64 {
		s := &model.Symbol{
			Kind: model.KindFunction, Name: name, FQN: fqn, FileID: fileID,
			Range: model.Range{
				Start: model.Position{Line: line + shift, Col: 1},
				End:   model.Position{Line: line + shift + 4, Col: 1},
			},
			Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, err := w.AddSymbol(ctx, s)
		if err != nil {
			t.Fatalf("add symbol %s: %v", fqn, err)
		}
		return id
	}
	src := mkSym("pkg/api.go:handleRefund", "handleRefund", 10)
	dst := mkSym("pkg/api.go:refund", "refund", 40)

	if _, err := w.AddEdge(ctx, &model.Edge{
		SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: conf,
		Evidence: model.Evidence{
			FilePath: "pkg/api.go",
			Range:    model.Range{Start: model.Position{Line: 12 + shift, Col: 3}},
			Source:   source,
		},
	}); err != nil {
		t.Fatalf("add edge: %v", err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit run: %v", err)
	}
	return reg, run.ID
}
