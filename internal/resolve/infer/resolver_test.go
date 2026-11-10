package infer

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func sym(id int64, name, fqn, file string, fileID int64, exported bool) registry.SymbolIndexEntry {
	return registry.SymbolIndexEntry{
		ID: id, Name: name, FQN: fqn, Kind: "method",
		FileID: fileID, FilePath: file, Exported: exported,
	}
}

func index(entries ...registry.SymbolIndexEntry) map[string][]registry.SymbolIndexEntry {
	m := map[string][]registry.SymbolIndexEntry{}
	for _, e := range entries {
		m[e.Name] = append(m[e.Name], e)
	}
	return m
}

// enclose gives a file one symbol spanning the whole file, so references have
// something to attribute to.
func enclose(id int64, name string) []registry.EnclosingSymbol {
	return []registry.EnclosingSymbol{{ID: id, Name: name, StartLine: 1, EndLine: 1000}}
}

// resolveOne runs one file and keys the outcomes by reference id so assertions
// can name the reference they are about.
//
// ResolveFile returns them as an ordered slice — see TestOutcomesFollowReference
// Order, which is the test that owns that property.
func resolveOne(t *testing.T, r *Resolver, fr registry.FileRefs) ([]model.Edge, map[int64]registry.RefOutcome, Stats) {
	t.Helper()
	var st Stats
	edges, outs, err := r.ResolveFile(context.Background(), fr, &st)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[int64]registry.RefOutcome, len(outs))
	for _, o := range outs {
		if _, dup := byID[o.RefID]; dup {
			t.Fatalf("reference %d reported twice", o.RefID)
		}
		byID[o.RefID] = o
	}
	return edges, byID, st
}

// TestOutcomesFollowReferenceOrder pins the ordering ResolveFile promises.
//
// registry.MarkResolutions sorts by reference id before it writes, and it can
// only do that cheaply because what it receives is already nearly sorted. This
// used to be a map, whose iteration order Go randomises, and the resulting
// shuffled writes were the measured cause of superlinear resolution.
func TestOutcomesFollowReferenceOrder(t *testing.T) {
	idx := index(
		sym(1, "alpha", "pkg.A.alpha", "a.go", 10, true),
		sym(2, "beta", "pkg.A.beta", "a.go", 20, true),
	)
	r := New(idx, Options{})

	refs := make([]registry.Reference, 0, 64)
	for i := 0; i < 64; i++ {
		// Rotate the three outcomes — unresolved, resolved, resolved elsewhere —
		// so the ordering assertion covers every branch that emits one.
		var name string
		switch i % 3 {
		case 0:
			name = "nothing-matches-this"
		case 1:
			name = "beta"
		default:
			name = "alpha"
		}
		refs = append(refs, registry.Reference{
			ID: int64(i + 1), Name: name, Kind: "reference.call", Line: i + 1,
		})
	}

	var st Stats
	_, outs, err := r.ResolveFile(context.Background(), registry.FileRefs{
		FileID: 10, Path: "a.go", Symbols: enclose(999, "Caller"), Refs: refs,
	}, &st)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != len(refs) {
		t.Fatalf("got %d outcomes for %d references", len(outs), len(refs))
	}
	for i, o := range outs {
		if o.RefID != refs[i].ID {
			t.Fatalf("outcome %d is for reference %d, want %d — outcomes must "+
				"follow reference order so the registry writes in key order",
				i, o.RefID, refs[i].ID)
		}
	}
}

func TestSameFileWinsWithHighConfidence(t *testing.T) {
	idx := index(
		sym(1, "save", "pkg.Repo.save", "a.go", 10, true),
		sym(2, "save", "other.Thing.save", "b.go", 20, true),
	)
	r := New(idx, Options{})

	edges, _, st := resolveOne(t, r, registry.FileRefs{
		FileID: 10, Path: "a.go",
		Symbols: enclose(99, "Caller"),
		Refs:    []registry.Reference{{ID: 1, Name: "save", Kind: "reference.call", Line: 5}},
	})

	if st.Resolved != 1 {
		t.Fatalf("expected 1 resolved, got %+v", st)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].DstID != 1 {
		t.Errorf("bound to symbol %d, want the same-file definition (1)", edges[0].DstID)
	}
	if edges[0].Confidence != model.ConfidenceHigh {
		t.Errorf("confidence = %s, want high", edges[0].Confidence)
	}
	if edges[0].SrcID != 99 {
		t.Errorf("SrcID = %d, want the enclosing symbol 99", edges[0].SrcID)
	}
}

// TestCommonNameIsRefusedNotGuessed is the most important test in this file.
//
// A name like `Get` matching hundreds of symbols must produce NO edge. Binding
// to an arbitrary one is a coin flip presented as an answer; binding to all of
// them floods the graph and makes every traversal downstream useless.
func TestCommonNameIsRefusedNotGuessed(t *testing.T) {
	var entries []registry.SymbolIndexEntry
	for i := int64(1); i <= 50; i++ {
		entries = append(entries, sym(i, "Get", "pkg.T.Get", "f.go", i, true))
	}
	r := New(index(entries...), Options{})

	edges, outs, st := resolveOne(t, r, registry.FileRefs{
		FileID: 999, Path: "caller.go",
		Symbols: enclose(1000, "Caller"),
		Refs:    []registry.Reference{{ID: 7, Name: "Get", Kind: "reference.call", Line: 3}},
	})

	if len(edges) != 0 {
		t.Fatalf("emitted %d edges for a name with 50 candidates; the graph must not be flooded", len(edges))
	}
	if st.Ambiguous != 1 {
		t.Errorf("expected the reference recorded as ambiguous, got %+v", st)
	}
	if outs[7].Resolution != registry.ResolutionAmbiguous {
		t.Errorf("resolution = %q, want ambiguous", outs[7].Resolution)
	}
	if outs[7].SymbolID != 0 {
		t.Error("an ambiguous reference must not be bound to a symbol")
	}
}

// TestUnknownNameIsUnresolvedNotDropped: a call to a third-party library is a
// real call site. Recording it as unresolved keeps the gap measurable.
func TestUnknownNameIsUnresolvedNotDropped(t *testing.T) {
	r := New(index(sym(1, "somethingElse", "x", "a.go", 1, true)), Options{})

	_, outs, st := resolveOne(t, r, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "Caller"),
		Refs:    []registry.Reference{{ID: 3, Name: "println", Kind: "reference.call", Line: 2}},
	})
	if st.Unresolved != 1 {
		t.Errorf("expected 1 unresolved, got %+v", st)
	}
	if outs[3].Resolution != registry.ResolutionUnresolved {
		t.Errorf("resolution = %q, want unresolved", outs[3].Resolution)
	}
}

func TestReceiverDisambiguates(t *testing.T) {
	idx := index(
		sym(1, "updateStatus", "com.pay.TransactionService.updateStatus", "svc/Transaction.java", 10, true),
		sym(2, "updateStatus", "com.pay.OrderService.updateStatus", "svc/Order.java", 11, true),
	)
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "api/Controller.java",
		Symbols: enclose(60, "handleCallback"),
		Refs: []registry.Reference{
			{ID: 1, Name: "updateStatus", Kind: "reference.call", Receiver: "transactionService", Line: 5},
		},
	})
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	if edges[0].DstID != 1 {
		t.Errorf("bound to %d, want TransactionService.updateStatus (1)", edges[0].DstID)
	}
	if edges[0].Confidence != model.ConfidenceHigh {
		t.Errorf("confidence = %s, want high — the receiver constrained both name and type", edges[0].Confidence)
	}
}

// TestNeverClaimsExact guards the system's central promise. Syntactic inference
// may never produce compiler-grade certainty.
func TestNeverClaimsExact(t *testing.T) {
	idx := index(sym(1, "save", "pkg.save", "a.go", 1, true))
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "C"),
		Refs:    []registry.Reference{{ID: 1, Name: "save", Kind: "reference.call", Line: 2}},
	})
	for _, e := range edges {
		if e.Confidence == model.ConfidenceExact {
			t.Fatal("inference claimed exact confidence")
		}
		if e.Evidence.Source == model.SourceCompiler {
			t.Fatal("inference claimed compiler provenance")
		}
		if err := e.Validate(); err != nil {
			t.Errorf("emitted an invalid edge: %v", err)
		}
	}
}

// TestEveryEdgeCarriesEvidence: an edge without a source location is an
// assertion an agent cannot verify.
func TestEveryEdgeCarriesEvidence(t *testing.T) {
	idx := index(sym(1, "save", "pkg.save", "a.go", 1, true))
	r := New(idx, Options{})
	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "C"),
		Refs:    []registry.Reference{{ID: 1, Name: "save", Kind: "reference.call", Line: 42, Col: 7}},
	})
	if len(edges) == 0 {
		t.Fatal("no edges")
	}
	e := edges[0]
	if e.Evidence.FilePath == "" || e.Evidence.Range.Start.Line != 42 {
		t.Errorf("evidence missing or wrong: %+v", e.Evidence)
	}
	if e.Evidence.Detail == "" {
		t.Error("edge does not record WHY it was bound; explainability is a product requirement")
	}
}

func TestEnclosingPicksInnermostSymbol(t *testing.T) {
	// A class spanning the file, a method inside it. A call at line 20 should
	// be attributed to the method, not the class.
	symbols := []registry.EnclosingSymbol{
		{ID: 1, Name: "TheClass", StartLine: 1, EndLine: 100},
		{ID: 2, Name: "theMethod", StartLine: 10, EndLine: 30},
	}
	got, ok := enclosing(symbols, 20)
	if !ok || got != 2 {
		t.Errorf("enclosing(20) = %d,%v; want the innermost symbol 2", got, ok)
	}
	if got, ok := enclosing(symbols, 5); !ok || got != 1 {
		t.Errorf("enclosing(5) = %d,%v; want the class 1", got, ok)
	}
	if _, ok := enclosing(symbols, 200); ok {
		t.Error("a line beyond every symbol must not be attributed")
	}
}

// TestZeroEndLineDoesNotSwallowFile: a symbol without an end span must cover
// only its own line, or every later reference would be misattributed to it.
func TestZeroEndLineDoesNotSwallowFile(t *testing.T) {
	symbols := []registry.EnclosingSymbol{{ID: 1, Name: "X", StartLine: 5, EndLine: 0}}
	if _, ok := enclosing(symbols, 500); ok {
		t.Fatal("a symbol with no end line swallowed the rest of the file")
	}
	if got, ok := enclosing(symbols, 5); !ok || got != 1 {
		t.Errorf("enclosing(5) = %d,%v; want 1", got, ok)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	idx := index(
		sym(3, "handle", "c.handle", "c.go", 3, true),
		sym(1, "handle", "a.handle", "a.go", 1, true),
		sym(2, "handle", "b.handle", "b.go", 2, true),
	)
	fr := registry.FileRefs{
		FileID: 9, Path: "x/main.go",
		Imports: []registry.Import{{Path: "a"}, {Path: "b"}, {Path: "c"}},
		Symbols: enclose(50, "main"),
		Refs:    []registry.Reference{{ID: 1, Name: "handle", Kind: "reference.call", Line: 3}},
	}

	var first int64 = -1
	for i := 0; i < 20; i++ {
		r := New(idx, Options{})
		edges, _, _ := resolveOne(t, r, fr)
		var got int64
		if len(edges) > 0 {
			got = edges[0].DstID
		}
		if first == -1 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d bound to %d, previously %d; resolution is not deterministic", i, got, first)
		}
	}
}

func TestMinConfidenceFiltersWeakBindings(t *testing.T) {
	// A unique but unimported name yields only low confidence.
	idx := index(sym(1, "obscure", "far.away.obscure", "far/away.go", 77, true))

	strict := New(idx, Options{MinConfidence: model.ConfidenceHigh})
	edges, _, _ := resolveOne(t, strict, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "C"),
		Refs:    []registry.Reference{{ID: 1, Name: "obscure", Kind: "reference.call", Line: 2}},
	})
	if len(edges) != 0 {
		t.Errorf("min-confidence=high admitted a low-confidence binding: %+v", edges)
	}

	loose := New(idx, Options{MinConfidence: model.ConfidenceLow})
	edges, _, _ = resolveOne(t, loose, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "C"),
		Refs:    []registry.Reference{{ID: 1, Name: "obscure", Kind: "reference.call", Line: 2}},
	})
	if len(edges) != 1 || edges[0].Confidence != model.ConfidenceLow {
		t.Errorf("min-confidence=low should admit it as low: %+v", edges)
	}
}

func TestUnexportedCrossFileIsRefused(t *testing.T) {
	// An unexported symbol in another file is more likely a coincidence than a
	// target; the reference probably points outside the repository.
	idx := index(sym(1, "helper", "other.helper", "other/x.go", 77, false))
	r := New(idx, Options{})
	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 1, Path: "a.go",
		Symbols: enclose(9, "C"),
		Refs:    []registry.Reference{{ID: 1, Name: "helper", Kind: "reference.call", Line: 2}},
	})
	if len(edges) != 0 {
		t.Errorf("bound to an unexported symbol in an unrelated file: %+v", edges)
	}
}

func BenchmarkResolveFile(b *testing.B) {
	var entries []registry.SymbolIndexEntry
	for i := int64(1); i <= 5000; i++ {
		entries = append(entries, sym(i, "sym"+string(rune('a'+i%26))+string(rune('a'+i/26%26)),
			"pkg.T.m", "f.go", i, true))
	}
	idx := index(entries...)
	r := New(idx, Options{})

	refs := make([]registry.Reference, 500)
	for i := range refs {
		refs[i] = registry.Reference{
			ID: int64(i), Name: "sym" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)),
			Kind: "reference.call", Line: i%900 + 1,
		}
	}
	fr := registry.FileRefs{FileID: 1, Path: "a.go", Symbols: enclose(1, "C"), Refs: refs}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var st Stats
		if _, _, err := r.ResolveFile(context.Background(), fr, &st); err != nil {
			b.Fatal(err)
		}
	}
}
