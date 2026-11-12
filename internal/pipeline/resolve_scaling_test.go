package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Resolution must stay close to linear in reference count, and must not hold
// the run's outcomes in memory while it runs.
//
// It did neither. `repocortex resolve` over a synthetic Java tree took 3.5s at
// 1,000 files and 203s at 25,000 — 58x the time for 25x the input — and an
// instrumented pass attributed 164s of that 203s to two write calls:
// registry.MarkResolutions and registry.AddResolvedEdges. MarkResolutions took
// map[int64]ResolvedRef and iterated it, so Go's randomised map order sent
// 2.6 million single-row UPDATEs into the reference table's B-trees in shuffled
// primary-key order, and the cost of each one grew as those B-trees outgrew the
// page cache.
//
// The tests below guard the two structural properties that fix depends on,
// deterministically where possible and by measurement only where it is not.

// resolveFixture builds a committed run with `files` files, `perFile` symbols in
// each, and `refsPerFile` references per file, and returns the registry and run.
//
// The reference names are chosen so a fixed fraction resolves: two thirds name a
// symbol that exists, one third names nothing. That mix matters — a fixture
// where everything resolves would never exercise the outcome buffer, which is
// the buffer that used to grow without bound.
func resolveFixture(tb testing.TB, dir string, files, perFile, refsPerFile int) (*registry.Registry, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "index.db")})
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		tb.Fatalf("migrate: %v", err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "scaling")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < files; i++ {
		path := fmt.Sprintf("src/pkg%03d/File%05d.java", i/20, i)
		fileID, err := w.AddFile(ctx, &model.File{
			Path: path, Language: "java",
			ContentHash: fmt.Sprintf("%064x", i), SizeBytes: 4096, ParseStatus: "ok",
		})
		if err != nil {
			tb.Fatalf("add file: %v", err)
		}
		// One enclosing type plus the methods, so every reference has a source
		// symbol to hang an edge on.
		if _, err := w.AddSymbol(ctx, &model.Symbol{
			Kind: model.KindClass, Name: fmt.Sprintf("File%05d", i),
			FQN: path + "#File", FileID: fileID,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 10000}},
			Exported:   true,
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}); err != nil {
			tb.Fatalf("add class: %v", err)
		}
		for j := 0; j < perFile; j++ {
			if _, err := w.AddSymbol(ctx, &model.Symbol{
				Kind: model.KindMethod, Name: fmt.Sprintf("op%05d_%d", i, j),
				FQN:    fmt.Sprintf("%s#op%05d_%d", path, i, j),
				FileID: fileID,
				Range: model.Range{
					Start: model.Position{Line: j*8 + 2}, End: model.Position{Line: j*8 + 7}},
				Exported:   true,
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}); err != nil {
				tb.Fatalf("add symbol: %v", err)
			}
		}
		// Import a neighbouring package so the import tier has something to see.
		if err := w.AddImport(ctx, fileID, registry.Import{
			Path: fmt.Sprintf("src.pkg%03d", ((i/20)+1)%(files/20+1)), Line: 2,
		}); err != nil {
			tb.Fatalf("add import: %v", err)
		}
		for k := 0; k < refsPerFile; k++ {
			// Unique to this call site, so nothing in the symbol table can match
			// it. With perFile == 0 every reference takes this branch, which is
			// the "resolves nothing" fixture.
			name := fmt.Sprintf("nothing%05d_%d", i, k)
			line := k%64 + 2
			if perFile > 0 && k%3 != 0 {
				// Names a real method in another file, spread by a stride
				// coprime with the file count so targets do not cluster.
				t := (i*7919 + k) % files
				name = fmt.Sprintf("op%05d_%d", t, k%perFile)
				line = k%(perFile*8) + 2
			}
			if err := w.AddReference(ctx, fileID, registry.Reference{
				Name: name, Kind: "reference.call", Line: line,
			}); err != nil {
				tb.Fatalf("add reference: %v", err)
			}
		}
	}
	if err := w.Flush(ctx); err != nil {
		tb.Fatalf("flush: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	return reg, run.ID
}

// TestResolveBoundsItsBuffers is the deterministic half of the guard.
//
// Resolve used to flush only when the EDGE buffer filled, so a repository whose
// references mostly point outside it — the common case for a service that
// mainly calls its framework — produced almost no edges, never tripped the
// flush, and held every one of the run's outcomes in memory until the end. This
// fixture resolves nothing at all, which is that case in its extreme form.
//
// Asserted on the batch sizes reported by OnFlush rather than on heap. Heap was
// the first attempt and it is not a usable signal here: Go's collector lets the
// heap reach roughly twice the live set before it runs, so the reading depends
// on what the rest of the package allocated before this test started, and the
// same code passed alone and failed in a full `go test ./...`. The buffer sizes
// are the property; measure the property.
func TestResolveBoundsItsBuffers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 60,000-reference fixture; skipped under -short")
	}
	ctx := context.Background()

	// 600 files x 100 references, none of which can resolve: every name is
	// unique to its own call site, so the symbol table has no candidate for it.
	reg, runID := resolveFixture(t, t.TempDir(), 600, 0, 100)

	const bound = DefaultResolveBatch * outcomeBatchFactor
	var flushes, maxEdges, maxOutcomes int
	stats, err := Resolve(ctx, reg, runID, ResolveOptions{
		OnFlush: func(edges, outcomes int) {
			flushes++
			maxEdges = max(maxEdges, edges)
			maxOutcomes = max(maxOutcomes, outcomes)
		},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if stats.References != 60_000 {
		t.Fatalf("fixture drifted: resolved %d references, want 60000", stats.References)
	}
	if stats.Edges != 0 {
		t.Fatalf("fixture drifted: %d edges from a corpus that resolves nothing", stats.Edges)
	}
	t.Logf("references=%d flushes=%d maxEdgeBatch=%d maxOutcomeBatch=%d (bound %d)",
		stats.References, flushes, maxEdges, maxOutcomes, bound)

	if maxOutcomes > bound {
		t.Errorf("a flush carried %d outcomes against a bound of %d: resolution is "+
			"accumulating per-reference state instead of flushing it. The flush must "+
			"trip on EITHER buffer — a corpus that resolves nothing produces no edges, "+
			"so keying it on edges alone holds the whole run in memory.",
			maxOutcomes, bound)
	}
	// A run this size cannot be done in one flush without having accumulated,
	// so the count is a second, independent statement of the same property.
	if want := stats.References / bound; flushes < int(want) {
		t.Errorf("%d flush(es) for %d references; a %d-outcome bound requires at least %d",
			flushes, stats.References, bound, want)
	}
}
