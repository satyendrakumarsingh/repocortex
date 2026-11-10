package registry

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Benchmark shape: 500 files, 50,000 symbols, 100,000 edges — roughly a
// mid-sized service repository. CONTRIBUTING forbids publishing numbers that no
// run produced, so these exist to produce them.
const (
	benchFiles   = 500
	benchSymbols = 50_000
	benchEdges   = 100_000
)

// phases records where the time went in one full indexing pass.
type phases struct {
	symbols time.Duration // files + symbols
	edges   time.Duration
	commit  time.Duration // validate + digest + swap
	total   time.Duration
}

// writeBenchRun performs one complete indexing pass, ending with CommitRun.
func writeBenchRun(tb testing.TB, r *Registry) phases {
	tb.Helper()
	ctx := context.Background()

	start := time.Now()
	run, err := r.BeginRun(ctx, "auto", "bench")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	symIDs := make([]int64, 0, benchSymbols)
	perFile := benchSymbols / benchFiles
	for i := 0; i < benchFiles; i++ {
		f := &model.File{
			Path:        fmt.Sprintf("internal/pkg%03d/file%03d.go", i/25, i),
			Language:    "go",
			ContentHash: fmt.Sprintf("%064x", i),
			SizeBytes:   4096,
			ParseStatus: "ok",
		}
		fileID, err := w.AddFile(ctx, f)
		if err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if err := w.IndexFileText(ctx, fileID, f.Path,
			"package pkg\n\nfunc handleCallbackRequest(ctx context.Context) error { return updateStatusRecord(ctx) }\n"); err != nil {
			tb.Fatalf("index text: %v", err)
		}
		for j := 0; j < perFile; j++ {
			id, err := w.AddSymbol(ctx, &model.Symbol{
				Kind:       model.KindFunction,
				Name:       fmt.Sprintf("handleThing%d", j),
				FQN:        fmt.Sprintf("%s#handleThing%d", f.Path, j),
				FileID:     fileID,
				Range:      model.Range{Start: model.Position{Line: j * 4}, End: model.Position{Line: j*4 + 3}},
				Exported:   j%2 == 0,
				Confidence: model.ConfidenceHigh,
				Source:     model.SourceSourceCode,
			})
			if err != nil {
				tb.Fatalf("add symbol: %v", err)
			}
			symIDs = append(symIDs, id)
		}
	}
	symDone := time.Now()

	n := int64(len(symIDs))
	for i := 0; i < benchEdges; i++ {
		// 7919 is coprime with the symbol count, so destinations spread evenly;
		// evidence_line is unique per edge so the UNIQUE constraint never trips.
		if _, err := w.AddEdge(ctx, &model.Edge{
			SrcID:      symIDs[int64(i)%n],
			DstID:      symIDs[(int64(i)*7919)%n],
			Kind:       model.EdgeCalls,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: "internal/pkg000/file000.go",
				Range:    model.Range{Start: model.Position{Line: i}},
				Source:   model.SourceSourceCode,
				Detail:   "import + receiver type",
			},
		}); err != nil {
			tb.Fatalf("add edge %d: %v", i, err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		tb.Fatalf("flush: %v", err)
	}
	edgeDone := time.Now()

	if err := r.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	done := time.Now()

	return phases{
		symbols: symDone.Sub(start),
		edges:   edgeDone.Sub(symDone),
		commit:  done.Sub(edgeDone),
		total:   done.Sub(start),
	}
}

// BenchmarkIndexRunMemory measures insert throughput with no disk I/O: the
// upper bound the writer can reach.
func BenchmarkIndexRunMemory(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// One store at a time: store.OpenMemory's shared-cache DSN means two
		// simultaneously-open in-memory stores are the same database.
		db, err := store.OpenMemory(ctx)
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		r := New(db)
		b.StartTimer()

		writeBenchRun(b, r)

		b.StopTimer()
		_ = db.Close()
		b.StartTimer()
	}
	reportRowRate(b)
}

// BenchmarkIndexRunDisk is the number that matters: an on-disk WAL database,
// which is what a user actually indexes into.
func BenchmarkIndexRunDisk(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db := openDiskStore(b, filepath.Join(b.TempDir(), "index.db"))
		r := New(db)
		b.StartTimer()

		writeBenchRun(b, r)

		b.StopTimer()
		_ = db.Close()
		b.StartTimer()
	}
	reportRowRate(b)
}

// BenchmarkRegistryDigest measures the determinism gate on its own, since it
// runs inside every commit and must not come to dominate it.
func BenchmarkRegistryDigest(b *testing.B) {
	ctx := context.Background()
	db := openDiskStore(b, filepath.Join(b.TempDir(), "index.db"))
	defer func() { _ = db.Close() }()
	r := New(db)
	writeBenchRun(b, r)

	run, err := r.CurrentRun(ctx)
	if err != nil {
		b.Fatalf("current run: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RegistryDigest(ctx, run.ID); err != nil {
			b.Fatalf("digest: %v", err)
		}
	}
}

func reportRowRate(b *testing.B) {
	b.Helper()
	rows := float64(benchFiles+benchSymbols+benchEdges) * float64(b.N)
	b.ReportMetric(rows/b.Elapsed().Seconds(), "rows/s")
}

func openDiskStore(tb testing.TB, path string) *store.DB {
	tb.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		_ = db.Close()
		tb.Fatalf("migrate: %v", err)
	}
	return db
}
