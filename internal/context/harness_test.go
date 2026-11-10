package context

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// indexFixture builds a real, committed, resolved index over a repository tree.
//
// The golden test must exercise the same data an agent would see, so this goes
// through the actual pipeline (parse -> registry -> resolve) rather than
// hand-writing rows. A hand-built fixture would let the context engine pass
// against symbols and edges the indexer never actually produces.
func indexFixture(tb testing.TB, root string) (*registry.Registry, *graph.Graph, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.RepoRoot = root

	walker, err := fs.NewWalker(root, cfg)
	if err != nil {
		tb.Fatalf("walker: %v", err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("writer: %v", err)
	}
	sink := pipeline.NewRegistrySink(w)
	if _, err := pipeline.NewRunner(walker, pipeline.Options{Workers: 2}).Run(ctx, sink); err != nil {
		tb.Fatalf("index: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		tb.Fatalf("flush: %v", err)
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	if _, err := pipeline.Resolve(ctx, reg, run.ID, pipeline.ResolveOptions{}); err != nil {
		tb.Fatalf("resolve: %v", err)
	}
	return reg, graph.New(db), run.ID
}

func paymentFixture(tb testing.TB) (*registry.Registry, *graph.Graph, int64) {
	tb.Helper()
	return indexFixture(tb, filepath.Join("..", "..", "fixtures", "payment-platform"))
}
