package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// consumesTree is a two-sided repository: a Go service that declares routes and
// a TypeScript module that calls them. Both sides are needed, and they must be
// in different files — a CONSUMES edge that could be produced from one file
// would not be testing the staging pass at all.
func consumesTree() map[string]string {
	return map[string]string{
		"server/orders.go": `package server

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/orders", ListOrders)
	mux.HandleFunc("/api/payments", CreatePayment)
}

func ListOrders(w http.ResponseWriter, r *http.Request) {}

func CreatePayment(w http.ResponseWriter, r *http.Request) {}
`,
		"web/orders.ts": `export async function loadOrders() {
	const res = await fetch("/api/orders");
	return res.json();
}

export async function pay(amount: number) {
	const res = await fetch("/api/payments", { method: "POST" });
	return res.json();
}
`,
	}
}

// indexTree performs one complete pass — discover, parse, stage, publish, bind
// — and returns the run and the digest recorded for it.
//
// It is deliberately the whole path rather than a hand-built fixture: A14 is an
// integration feature, and the defect it replaces was two halves that each
// worked and were never connected.
func indexTree(t *testing.T, root, dbPath string) (*registry.Registry, int64, string) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	sink := NewRegistrySink(writer)

	if _, err := runnerFor(t, root, Options{Workers: 2}).Run(ctx, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("CommitRun: %v", err)
	}

	// The digest is taken at publish time, before the binding passes add edges,
	// which is the state two runs of the same tree must agree on.
	dg, err := reg.CommittedDigest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(ctx, reg, run.ID, ResolveOptions{}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return reg, run.ID, dg
}

// TestConsumesEdgesAreProducedEndToEnd is the acceptance test for A14: index a
// real tree, and the CONSUMES edges model.EdgeConsumes has always declared and
// nothing has ever produced now exist.
func TestConsumesEdgesAreProducedEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, consumesTree())

	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var consumes []model.Edge
	for _, e := range edges {
		if e.Kind == model.EdgeConsumes {
			consumes = append(consumes, e)
		}
	}
	if len(consumes) != 2 {
		report, _ := reg.ClientCallReport(ctx, runID)
		t.Fatalf("got %d CONSUMES edges, want 2 (one per fetch); staged calls: %+v",
			len(consumes), report)
	}

	// Every edge must point from the calling declaration in the client file to
	// an endpoint node in the server file. An edge that landed the other way
	// round, or that started at a file, would still count above.
	syms, err := reg.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]model.Symbol{}
	for _, s := range syms {
		byID[s.ID] = s
	}
	for _, e := range consumes {
		src, dst := byID[e.SrcID], byID[e.DstID]
		if dst.Kind != model.KindEndpoint {
			t.Errorf("CONSUMES points at a %s (%q); the target must be the endpoint node",
				dst.Kind, dst.Name)
		}
		if src.Kind == model.KindEndpoint {
			t.Errorf("CONSUMES starts at an endpoint (%q); the source must be the caller", src.Name)
		}
		if e.Evidence.FilePath == "" || e.Evidence.Source != model.SourceSourceCode {
			t.Errorf("CONSUMES edge %d->%d carries no usable provenance: %+v",
				e.SrcID, e.DstID, e.Evidence)
		}
	}

	// The staged rows survive binding, so "which endpoints do we call" stays
	// answerable from the calls rather than only from the edges.
	report, err := reg.ClientCallReport(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 2 {
		t.Fatalf("staged %d client calls, want 2: %+v", len(report), report)
	}
	for _, c := range report {
		if c.Binding != "bound" || c.Endpoint == "" {
			t.Errorf("call %s %s recorded %q -> %q, want bound to an endpoint",
				c.Method, c.Path, c.Binding, c.Endpoint)
		}
	}
}

// TestTwoIndexesOfOneTreeAgree is the determinism gate. Two full passes over
// one tree must produce byte-identical digests, and the same completeness
// verdict — if either varies, something in the pipeline depends on goroutine
// scheduling, map order or walk order.
func TestTwoIndexesOfOneTreeAgree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, consumesTree())

	regA, runA, digestA := indexTree(t, root, filepath.Join(t.TempDir(), "a.db"))
	regB, runB, digestB := indexTree(t, root, filepath.Join(t.TempDir(), "b.db"))

	if digestA == "" {
		t.Fatal("no digest recorded for the first run")
	}
	if digestA != digestB {
		t.Fatalf("digests differ across two indexes of one tree:\n  %s\n  %s", digestA, digestB)
	}

	// The CONSUMES pass must be as reproducible as the rest, including which
	// calls refused to bind.
	callsA, err := regA.ClientCallReport(ctx, runA)
	if err != nil {
		t.Fatal(err)
	}
	callsB, err := regB.ClientCallReport(ctx, runB)
	if err != nil {
		t.Fatal(err)
	}
	if len(callsA) != len(callsB) {
		t.Fatalf("staged %d calls then %d; staging is not deterministic", len(callsA), len(callsB))
	}
	for i := range callsA {
		a, b := callsA[i], callsB[i]
		if a.FilePath != b.FilePath || a.Line != b.Line || a.Path != b.Path ||
			a.Binding != b.Binding || a.Endpoint != b.Endpoint {
			t.Fatalf("call %d differs between runs:\n  %+v\n  %+v", i, a, b)
		}
	}

	// Completeness travels with the run, not with the process that wrote it.
	for _, tc := range []struct {
		name string
		reg  *registry.Registry
	}{{"first", regA}, {"second", regB}} {
		run, err := tc.reg.CurrentRun(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !run.Complete() {
			t.Errorf("%s run reads as %q on a tree that indexed cleanly", tc.name, run.IndexState)
		}
		if run.FilesDiscovered != int64(len(consumesTree())) {
			t.Errorf("%s run: files_discovered = %d, want %d",
				tc.name, run.FilesDiscovered, len(consumesTree()))
		}
		if run.FilesUnaccounted != 0 {
			t.Errorf("%s run: files_unaccounted = %d, want 0", tc.name, run.FilesUnaccounted)
		}
	}
}
