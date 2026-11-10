package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// TestRegistrySinkSurvivesRunnerReturn is a regression test for a real
// integration defect.
//
// Runner.Run used to derive a cancellable context and pass it to the Sink.
// database/sql rolls a transaction back the moment the context it was created
// with is cancelled, so Run's `defer cancel()` silently destroyed every row
// just written, and the caller's Flush failed with "transaction has already
// been committed or rolled back".
//
// The failure only appears when a real transactional sink is wired to a real
// runner — no unit test of either half catches it. That is precisely why it is
// pinned here.
func TestRegistrySinkSurvivesRunnerReturn(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"svc/service.go": "package svc\n\ntype Service struct{}\n\nfunc (s *Service) Do() error { return nil }\n",
		"api/handler.go": "package api\n\nfunc Handle() {}\n",
	})

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	sink := NewRegistrySink(writer)
	stats, err := runnerFor(t, root, Options{Workers: 2}).Run(ctx, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The operation that used to fail.
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush after Run returned: %v — the runner's internal context cancelled the sink's transaction", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("CommitRun: %v", err)
	}

	// Rows must have actually survived, not merely failed to error.
	counts, err := reg.Counts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files == 0 {
		t.Fatal("committed run contains no files; writes were rolled back")
	}
	if counts.Symbols == 0 {
		t.Fatal("committed run contains no symbols")
	}
	t.Logf("committed: %+v (stats %+v)", counts, stats)

	current, err := reg.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != run.ID {
		t.Errorf("current run = %d, want %d", current.ID, run.ID)
	}
}

// TestIndexIsDeterministic is the M1 exit gate: indexing the same tree twice
// must produce byte-identical registry content.
//
// This is the cheapest way to catch map-iteration nondeterminism, goroutine
// races and ordering bugs, which unit tests routinely miss.
func TestIndexIsDeterministic(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	files := map[string]string{}
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		files["pkg/"+name+"/svc.go"] = "package " + name + "\n\n" +
			"import \"context\"\n\n" +
			"type Service struct{}\n\n" +
			"func (s *Service) Handle(ctx context.Context) error { return s.repo.Save() }\n"
	}
	files["api/Controller.java"] = "package api;\nimport com.pay.Service;\npublic class Controller { void go() { svc.handle(); } }\n"
	files["app/main.py"] = "from app.service import Service\n\nclass Runner:\n    def run(self):\n        return self.svc.handle()\n"
	writeTree(t, root, files)

	digestOf := func(t *testing.T, workers int) string {
		t.Helper()
		db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "i.db")})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
			t.Fatal(err)
		}
		reg := registry.New(db)
		run, err := reg.BeginRun(ctx, "fast", "")
		if err != nil {
			t.Fatal(err)
		}
		w, err := reg.NewWriter(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		sink := NewRegistrySink(w)
		if _, err := runnerFor(t, root, Options{Workers: workers}).Run(ctx, sink); err != nil {
			t.Fatal(err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := reg.CommitRun(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		d, err := reg.RegistryDigest(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Vary worker counts: if the digest depended on completion order rather
	// than content, differing concurrency is what would expose it.
	base := digestOf(t, 1)
	for _, workers := range []int{1, 2, 4, 8} {
		got := digestOf(t, workers)
		if got != base {
			t.Fatalf("digest differs at %d workers:\n base=%s\n got =%s", workers, base, got)
		}
	}
	t.Logf("stable digest across 1/2/4/8 workers: %s", base)
}

// TestDigestChangesWhenSourceChanges proves the gate is sensitive, not merely
// stable. A digest that never changes would pass the determinism test while
// detecting nothing.
func TestDigestChangesWhenSourceChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.go": "package p\n\nfunc Alpha() {}\n"})

	digest := func() string {
		db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "i.db")})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
			t.Fatal(err)
		}
		reg := registry.New(db)
		run, _ := reg.BeginRun(ctx, "fast", "")
		w, err := reg.NewWriter(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		sink := NewRegistrySink(w)
		if _, err := runnerFor(t, root, Options{}).Run(ctx, sink); err != nil {
			t.Fatal(err)
		}
		_ = sink.Flush(ctx)
		_ = w.Close()
		if err := reg.CommitRun(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		d, err := reg.RegistryDigest(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	before := digest()
	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package p\n\nfunc Alpha() {}\n\nfunc Beta() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := digest()

	if before == after {
		t.Fatal("digest unchanged after adding a symbol; the determinism gate detects nothing")
	}
	t.Logf("digest correctly changed:\n before=%s\n after =%s", before, after)
}
