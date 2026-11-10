package scip

import (
	"bytes"
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestFactsLandInTheRegistry closes the loop the rest of the suite leaves open:
// the values this package returns are shaped for registry.ReplaceFile, and the
// only way to know that is to write them.
//
// It also puts the exact-implies-compiler invariant in front of the SQL CHECK
// in migration 0001, which is the enforcement the Go-level assertions elsewhere
// in this package cannot stand in for — the two checks exist precisely because
// neither is trusted alone.
func TestFactsLandInTheRegistry(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "exact", "deadbeef")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc(), handlerDoc())
	facts, _, err := Collect(ctx, bytes.NewReader(data),
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Two passes, which is what a SCIP ingest fundamentally requires: a
	// reference in one document points at a definition in another, so no edge
	// can be written until every symbol has a rowid. This is why the package
	// returns Relations keyed by FQN instead of pre-built edges.
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	table := map[string]int64{}
	for i := range facts {
		f := &facts[i]
		file := &model.File{Path: f.Path, Language: f.Language, ContentHash: "x"}
		fileID, err := w.AddFile(ctx, file)
		if err != nil {
			t.Fatalf("AddFile %s: %v", f.Path, err)
		}
		for j := range f.Symbols {
			// Passing the element by pointer matters: AddSymbol writes the real
			// rowid back into Symbol.ID, which is what turns the placeholders
			// FileFacts.Edges reads through LocalIDs into storable ids.
			f.Symbols[j].FileID = fileID
			id, err := w.AddSymbol(ctx, &f.Symbols[j])
			if err != nil {
				t.Fatalf("AddSymbol %s: %v", f.Symbols[j].FQN, err)
			}
			table[f.Symbols[j].FQN] = id
		}
	}

	lookup := func(fqn string) (int64, bool) { id, ok := table[fqn]; return id, ok }
	var written int
	for i := range facts {
		f := &facts[i]
		edges, unresolved, err := f.Edges(lookup)
		if err != nil {
			t.Fatalf("Edges %s: %v", f.Path, err)
		}
		for j := range edges {
			if _, err := w.AddEdge(ctx, &edges[j]); err != nil {
				t.Fatalf("AddEdge %s: %v", f.Path, err)
			}
			written++
		}
		// References into fmt and net/http have no symbol in this repository.
		// They are reported, not counted as edges.
		t.Logf("%s: %d edge(s), %d unresolved", f.Path, len(edges), len(unresolved))
	}
	if written == 0 {
		t.Fatal("no edges written; the round trip proved nothing")
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	// CommitRun re-checks the invariants across the whole run before publishing.
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	stored, err := reg.Edges(ctx, run.ID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("edges did not survive the write")
	}
	for _, e := range stored {
		if e.Confidence != model.ConfidenceExact || e.Evidence.Source != model.SourceCompiler {
			t.Fatalf("stored edge lost its provenance: %+v", e)
		}
		if e.Evidence.Detail == "" {
			t.Fatalf("stored edge does not name the indexer: %+v", e)
		}
	}
}
