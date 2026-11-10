package infer

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeAncestry is the Ancestry port backed by a fixed hierarchy.
//
// Deliberately tiny — one method, a map literal per test. A port a fake cannot
// cheaply implement has stopped being a test aid (CLAUDE.md §1), and this is
// the check on whether Ancestry stayed small enough.
type fakeAncestry struct {
	// edges maps a type to its DIRECT supertypes, lowercased keys so the fake
	// answers the way a real index would: type names are matched
	// case-insensitively everywhere else in this package.
	edges map[string][]string

	// calls counts requests, so a test can prove the walk terminated by
	// bounded work rather than by the test binary's timeout.
	calls int
}

func (f *fakeAncestry) Supertypes(_ context.Context, types []string) (map[string][]string, error) {
	f.calls++
	out := make(map[string][]string, len(types))
	for _, t := range types {
		if parents := f.edges[strings.ToLower(t)]; len(parents) > 0 {
			out[t] = parents
		}
	}
	return out, nil
}

// javaMethod builds a candidate the way a Java symbol arrives from the index:
// a package-qualified owner in the FQN, and a file named for the class.
func javaMethod(id int64, owner, name string) registry.SymbolIndexEntry {
	return registry.SymbolIndexEntry{
		ID: id, Name: name,
		FQN:      "com.pay." + owner + "." + name,
		Kind:     "method",
		FileID:   id * 100,
		FilePath: "src/main/java/com/pay/" + owner + ".java",
		Exported: true,
	}
}

// callOn builds the one file every test in here resolves: a Java file calling
// `<receiver>.<name>()`, where the parser has already rewritten the receiver to
// its declared type (see varTypeQueries in internal/parse/queries.go).
func callOn(receiver, name string) registry.FileRefs {
	return registry.FileRefs{
		FileID: 1, Path: "src/main/java/com/pay/Caller.java", Language: "java",
		Symbols: enclose(7, "process"),
		Refs: []registry.Reference{{
			ID: 42, Name: name, Kind: "call", Receiver: receiver, Line: 10, Col: 4,
		}},
	}
}

// runTwoPasses drives the shape a caller must use: every file through
// ResolveFile, then one ResolveDeferred once the loop is over.
func runTwoPasses(t *testing.T, r *Resolver, files ...registry.FileRefs) ([]model.Edge, []registry.RefOutcome, Stats) {
	t.Helper()
	ctx := context.Background()
	var st Stats
	for _, fr := range files {
		if _, _, err := r.ResolveFile(ctx, fr, &st); err != nil {
			t.Fatal(err)
		}
	}
	edges, outs, err := r.ResolveDeferred(ctx, &st)
	if err != nil {
		t.Fatal(err)
	}
	return edges, outs, st
}

// twoLevelHierarchy is Payment -> BasePayment -> LedgerEntity, with settle
// declared only on the grandparent, plus an unrelated Invoice.settle so the
// name is not unique and the first pass cannot bind it by elimination.
func twoLevelHierarchy() (*fakeAncestry, map[string][]registry.SymbolIndexEntry) {
	anc := &fakeAncestry{edges: map[string][]string{
		"payment":     {"BasePayment"},
		"basepayment": {"LedgerEntity"},
	}}
	return anc, index(
		javaMethod(10, "LedgerEntity", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)
}

// TestGrandparentMethodResolvesFromACallOnTheChild is the case A21 exists for.
//
// `payment.settle()` where Payment declares no settle, BasePayment declares no
// settle, and LedgerEntity two levels up does. No tier in pick() can reach it:
// tier 2 asks whether a candidate is OWNED by Payment and the answer is no for
// every one of them. Before the deferred pass this reference was ambiguous.
func TestGrandparentMethodResolvesFromACallOnTheChild(t *testing.T) {
	anc, symbols := twoLevelHierarchy()
	r := New(symbols, Options{Ancestry: anc})

	edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if len(outs) != 1 || outs[0].RefID != 42 {
		t.Fatalf("second pass produced %+v, want one outcome for reference 42", outs)
	}
	if outs[0].Resolution != registry.ResolutionResolved || outs[0].SymbolID != 10 {
		t.Fatalf("bound to %+v, want symbol 10 (LedgerEntity.settle) resolved", outs[0])
	}
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	if edges[0].SrcID != 7 || edges[0].DstID != 10 || edges[0].Kind != model.EdgeCalls {
		t.Errorf("edge %+v, want 7 -calls-> 10", edges[0])
	}

	// Two levels of inheritance is weak evidence: two unverified EXTENDS edges
	// stacked, either of which could hide an override the index never linked.
	if edges[0].Confidence != model.ConfidenceLow {
		t.Errorf("confidence %q at depth 2, want low", edges[0].Confidence)
	}
	if !strings.Contains(edges[0].Evidence.Detail, "LedgerEntity") ||
		!strings.Contains(edges[0].Evidence.Detail, "2 level") {
		t.Errorf("evidence %q does not name the ancestor and the distance",
			edges[0].Evidence.Detail)
	}

	// The totals must stay consistent: pass one counted this ambiguous, and
	// resolution precision is published from these numbers.
	if st.References != 1 || st.Resolved != 1 || st.Ambiguous != 0 {
		t.Errorf("stats %+v, want 1 reference, 1 resolved, 0 ambiguous", st)
	}
	if st.Deferred != 1 || st.Inherited != 1 || st.InheritedAmbiguous != 0 {
		t.Errorf("stats %+v, want 1 deferred, 1 inherited, 0 inherited-ambiguous", st)
	}
	if st.ByConfidence[model.ConfidenceLow] != 1 {
		t.Errorf("by-confidence %v, want one low binding", st.ByConfidence)
	}
}

// TestDirectParentEarnsMediumAndNeverHigh pins the confidence one inheritance
// step earns, and the ceiling on all of them.
//
// High in this package means the evidence constrained the TYPE as well as the
// name — same file, an owner carried in the qualified name, a DECLARED import.
// An inherited binding has none of those; it rests on the receiver token naming
// the type it appears to AND on an EXTENDS edge this same heuristic resolver
// produced. Publishing that as High would claim more than the evidence
// supports.
func TestDirectParentEarnsMediumAndNeverHigh(t *testing.T) {
	anc := &fakeAncestry{edges: map[string][]string{"payment": {"BasePayment"}}}
	symbols := index(
		javaMethod(10, "BasePayment", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)
	r := New(symbols, Options{Ancestry: anc})

	edges, _, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if len(edges) != 1 || edges[0].DstID != 10 {
		t.Fatalf("edges %+v, want one edge to symbol 10", edges)
	}
	if edges[0].Confidence != model.ConfidenceMedium {
		t.Errorf("confidence %q at depth 1, want medium", edges[0].Confidence)
	}
	if st.ByConfidence[model.ConfidenceExact] != 0 && st.ByConfidence[model.ConfidenceHigh] != 0 {
		t.Errorf("inherited dispatch reported %v; it must never claim high or exact",
			st.ByConfidence)
	}
}

// TestAmbiguousChainRecordsAmbiguityRatherThanGuessing is the lesson the two
// reverted guards paid for, applied one layer up.
//
// Payment extends BasePayment and implements Auditable, and BOTH declare
// settle. Nothing available here can say which one the call reaches, and the
// languages offer no nearest-declaration rule to appeal to at equal depth.
// Picking either would be a coin flip wearing a resolution's confidence — the
// exact conversion of honest ambiguity into confident wrong answers that cost
// A19 2.4pp of high-tier precision.
func TestAmbiguousChainRecordsAmbiguityRatherThanGuessing(t *testing.T) {
	anc := &fakeAncestry{edges: map[string][]string{
		"payment": {"BasePayment", "Auditable"},
	}}
	symbols := index(
		javaMethod(10, "BasePayment", "settle"),
		javaMethod(11, "Auditable", "settle"),
	)
	r := New(symbols, Options{Ancestry: anc})

	edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if len(edges) != 0 {
		t.Fatalf("emitted %d edges from an ambiguous hierarchy: %+v", len(edges), edges)
	}
	if len(outs) != 1 || outs[0].Resolution != registry.ResolutionAmbiguous {
		t.Fatalf("outcomes %+v, want reference 42 recorded ambiguous", outs)
	}
	if st.InheritedAmbiguous != 1 || st.Inherited != 0 {
		t.Errorf("stats %+v, want the ambiguity recorded and nothing bound", st)
	}
	if st.Ambiguous != 1 || st.Resolved != 0 {
		t.Errorf("stats %+v, want the reference to stay ambiguous", st)
	}
}

// TestNearestAncestorWinsWithoutAmbiguity is the case that is NOT ambiguous,
// and the boundary matters.
//
// Payment extends BasePayment, which extends LedgerEntity, and both declare
// settle. A subclass override shadows its parent — that is what the language
// specifies, not something this package infers — so the nearest declaration is
// the one the call reaches. Reporting this as ambiguous would refuse an answer
// the language actually gives.
func TestNearestAncestorWinsWithoutAmbiguity(t *testing.T) {
	anc := &fakeAncestry{edges: map[string][]string{
		"payment":     {"BasePayment"},
		"basepayment": {"LedgerEntity"},
	}}
	symbols := index(
		javaMethod(10, "BasePayment", "settle"),
		javaMethod(11, "LedgerEntity", "settle"),
	)
	r := New(symbols, Options{Ancestry: anc})

	edges, _, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if len(edges) != 1 || edges[0].DstID != 10 {
		t.Fatalf("edges %+v, want the nearest declaration (symbol 10)", edges)
	}
	if st.InheritedAmbiguous != 0 {
		t.Errorf("recorded ambiguity for a hierarchy the language disambiguates: %+v", st)
	}
}

// TestCycleInTheHierarchyTerminates covers the input a heuristic index can
// absolutely produce and a compiler never would.
//
// Two sub-cases, because the two failure modes are different: a cycle whose
// types DO declare the method must still bind at the nearest level, and a cycle
// whose types declare nothing must return empty-handed. Both must do bounded
// work — asserted on the request count rather than left to the test binary's
// timeout, so a regression fails loudly instead of hanging.
func TestCycleInTheHierarchyTerminates(t *testing.T) {
	cycle := map[string][]string{
		"payment":      {"BasePayment"},
		"basepayment":  {"LedgerEntity"},
		"ledgerentity": {"Payment"}, // back to the start
	}

	t.Run("defined inside the cycle", func(t *testing.T) {
		anc := &fakeAncestry{edges: cycle}
		symbols := index(
			javaMethod(10, "BasePayment", "settle"),
			javaMethod(11, "Invoice", "settle"),
		)
		r := New(symbols, Options{Ancestry: anc})

		edges, _, _ := runTwoPasses(t, r, callOn("Payment", "settle"))
		if len(edges) != 1 || edges[0].DstID != 10 {
			t.Fatalf("edges %+v, want one edge to symbol 10", edges)
		}
		if anc.calls > MaxAncestryDepth {
			t.Errorf("%d supertype requests for one chunk, capped at %d",
				anc.calls, MaxAncestryDepth)
		}
	})

	t.Run("defined nowhere in the cycle", func(t *testing.T) {
		anc := &fakeAncestry{edges: cycle}
		symbols := index(
			javaMethod(10, "Invoice", "settle"),
			javaMethod(11, "Receipt", "settle"),
		)
		r := New(symbols, Options{Ancestry: anc})

		edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))
		if len(edges) != 0 || len(outs) != 0 {
			t.Fatalf("a cycle that defines nothing produced %d edges and %d outcomes",
				len(edges), len(outs))
		}
		if st.Ambiguous != 1 {
			t.Errorf("stats %+v, want the first pass's outcome left standing", st)
		}
		if anc.calls > MaxAncestryDepth {
			t.Errorf("%d supertype requests for one chunk, capped at %d",
				anc.calls, MaxAncestryDepth)
		}
	})

	t.Run("a type that is its own supertype", func(t *testing.T) {
		anc := &fakeAncestry{edges: map[string][]string{"payment": {"Payment"}}}
		r := New(index(javaMethod(10, "Invoice", "settle"),
			javaMethod(11, "Receipt", "settle")), Options{Ancestry: anc})

		if edges, outs, _ := runTwoPasses(t, r, callOn("Payment", "settle")); len(edges) != 0 || len(outs) != 0 {
			t.Fatalf("self-cycle produced %d edges and %d outcomes", len(edges), len(outs))
		}
	})
}

// TestPassIsInertWithoutInheritanceEdges is the guarantee that lets this land
// on a repository whose language emits no EXTENDS or IMPLEMENTS at all.
//
// An Ancestry that reports nothing must produce no edges, no outcomes and no
// movement in the counters — the run is exactly what it was before A21.
func TestPassIsInertWithoutInheritanceEdges(t *testing.T) {
	empty := &fakeAncestry{edges: map[string][]string{}}
	symbols := index(
		javaMethod(10, "LedgerEntity", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)
	r := New(symbols, Options{Ancestry: empty})

	edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if len(edges) != 0 || len(outs) != 0 {
		t.Fatalf("no inheritance edges, yet the pass produced %d edges and %d outcomes",
			len(edges), len(outs))
	}
	if st.Resolved != 0 || st.Ambiguous != 1 || st.Inherited != 0 || st.InheritedAmbiguous != 0 {
		t.Errorf("stats %+v, want the first pass's outcome untouched", st)
	}
}

// TestNoAncestryPortParksNothing is the stronger half of inertness: a caller
// that has not been wired up must not even pay the queue.
func TestNoAncestryPortParksNothing(t *testing.T) {
	symbols := index(
		javaMethod(10, "LedgerEntity", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)
	r := New(symbols, Options{})

	edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))

	if r.Deferred() != 0 || st.Deferred != 0 {
		t.Errorf("parked %d references with no Ancestry supplied", r.Deferred())
	}
	if edges != nil || outs != nil {
		t.Errorf("ResolveDeferred returned %v / %v with no Ancestry supplied", edges, outs)
	}
}

// TestBoundReferencesAreNeverParked is what keeps this pass strictly additive.
//
// The two guards recorded above ResolveFile were reverted because they changed
// which references resolved. This one may only look at references the first
// pass REFUSED, so there is no existing binding for it to overturn and no
// ambiguity for it to destroy.
func TestBoundReferencesAreNeverParked(t *testing.T) {
	anc := &fakeAncestry{edges: map[string][]string{"payment": {"BasePayment"}}}
	symbols := index(
		// Owned by the receiver's own type: tier 2 binds this outright.
		javaMethod(10, "Payment", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)
	r := New(symbols, Options{Ancestry: anc})

	var st Stats
	edges, _, err := r.ResolveFile(context.Background(), callOn("Payment", "settle"), &st)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].DstID != 10 || edges[0].Confidence != model.ConfidenceHigh {
		t.Fatalf("first pass produced %+v, want a high-confidence edge to symbol 10", edges)
	}
	if r.Deferred() != 0 {
		t.Errorf("parked a reference the first pass already bound")
	}
}

// TestReceiverThatIsNotATypeIsNotWalked keeps the walk's premise honest.
//
// The whole pass rests on the receiver NAMING a type. Where the parser leaves
// it as a variable identifier — every language without a var-type query, and
// `this`/`self` everywhere — asking what it extends is not a type question, and
// answering one anyway is how a mis-inference gets laundered into a fact (A16).
func TestReceiverThatIsNotATypeIsNotWalked(t *testing.T) {
	anc := &fakeAncestry{edges: map[string][]string{
		"payment": {"BasePayment"}, "this": {"BasePayment"}, "p": {"BasePayment"},
	}}
	symbols := index(
		javaMethod(10, "BasePayment", "settle"),
		javaMethod(11, "Invoice", "settle"),
	)

	for _, tc := range []struct{ name, language, receiver string }{
		{"a variable name in a language with no var-type query", "go", "payment"},
		{"this", "java", "this"},
		{"self", "python", "self"},
		{"below the length floor", "java", "p"},
		{"no receiver at all", "java", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(symbols, Options{Ancestry: anc})
			fr := callOn(tc.receiver, "settle")
			fr.Language = tc.language

			if _, outs, _ := runTwoPasses(t, r, fr); len(outs) != 0 {
				t.Errorf("walked a receiver that names no type: %+v", outs)
			}
		})
	}
}

// TestDeferredPassIsDeterministic pins the property the registry digest depends
// on. Two ancestors, two candidates, several files: the same input must give
// the same edges in the same order, every run.
func TestDeferredPassIsDeterministic(t *testing.T) {
	build := func() ([]model.Edge, []registry.RefOutcome) {
		anc := &fakeAncestry{edges: map[string][]string{
			"payment":     {"BasePayment"},
			"basepayment": {"LedgerEntity"},
			"refund":      {"LedgerEntity"},
		}}
		symbols := index(
			javaMethod(10, "LedgerEntity", "settle"),
			javaMethod(11, "Invoice", "settle"),
			javaMethod(12, "LedgerEntity", "audit"),
			javaMethod(13, "Invoice", "audit"),
		)
		r := New(symbols, Options{Ancestry: anc})

		var files []registry.FileRefs
		for i, tc := range []struct{ receiver, name string }{
			{"Payment", "settle"}, {"Refund", "audit"},
			{"Payment", "audit"}, {"Refund", "settle"},
		} {
			fr := callOn(tc.receiver, tc.name)
			fr.FileID = int64(i + 1)
			fr.Refs[0].ID = int64(100 + i)
			files = append(files, fr)
		}
		edges, outs, _ := runTwoPasses(t, r, files...)
		return edges, outs
	}

	edges, outs := build()
	if len(edges) != 4 {
		t.Fatalf("got %d edges, want 4", len(edges))
	}
	// Outcomes must arrive in reference order — registry.MarkResolutions sorts
	// by reference id and can only do that cheaply because it is handed
	// something already nearly sorted.
	if !sort.SliceIsSorted(outs, func(i, j int) bool { return outs[i].RefID < outs[j].RefID }) {
		t.Errorf("outcomes are not in reference order: %+v", outs)
	}
	for run := 0; run < 5; run++ {
		gotEdges, gotOuts := build()
		if len(gotEdges) != len(edges) || len(gotOuts) != len(outs) {
			t.Fatalf("run %d produced %d edges / %d outcomes, want %d / %d",
				run, len(gotEdges), len(gotOuts), len(edges), len(outs))
		}
		for i := range edges {
			if gotEdges[i] != edges[i] {
				t.Fatalf("run %d edge %d = %+v, want %+v", run, i, gotEdges[i], edges[i])
			}
		}
		for i := range outs {
			if gotOuts[i] != outs[i] {
				t.Fatalf("run %d outcome %d = %+v, want %+v", run, i, gotOuts[i], outs[i])
			}
		}
	}
}

// TestDrainingTwiceIsEmpty: the queue is consumed, so a caller that drains
// after each page rather than once at the end does not write an edge twice.
func TestDrainingTwiceIsEmpty(t *testing.T) {
	anc, symbols := twoLevelHierarchy()
	r := New(symbols, Options{Ancestry: anc})

	var st Stats
	runTwoPasses(t, r, callOn("Payment", "settle"))
	edges, outs, err := r.ResolveDeferred(context.Background(), &st)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 || len(outs) != 0 {
		t.Errorf("a second drain re-emitted %d edges and %d outcomes", len(edges), len(outs))
	}
}

// TestMinConfidenceDropsWeakInheritedBindings: a caller that asked for medium
// and above must not be handed a Low binding from three levels up.
func TestMinConfidenceDropsWeakInheritedBindings(t *testing.T) {
	anc, symbols := twoLevelHierarchy() // settle is two levels up -> low
	r := New(symbols, Options{Ancestry: anc, MinConfidence: model.ConfidenceMedium})

	edges, outs, st := runTwoPasses(t, r, callOn("Payment", "settle"))
	if len(edges) != 0 || len(outs) != 0 {
		t.Fatalf("a low binding survived MinConfidence=medium: %+v / %+v", edges, outs)
	}
	if st.Inherited != 0 || st.Resolved != 0 {
		t.Errorf("stats %+v, want nothing bound", st)
	}
}

// TestDepthCapStopsAVeryLongChain: the acyclic case the cycle guard cannot
// catch. A chain of distinct types longer than the cap must stop, not walk.
//
// The chain is genuinely connected — asserted by the sub-test that finds a
// definition INSIDE the cap — so the miss past it is the cap doing its job and
// not a broken fixture.
func TestDepthCapStopsAVeryLongChain(t *testing.T) {
	chain := map[string][]string{"payment": {chainName(1)}}
	for i := 1; i < 40; i++ {
		chain[strings.ToLower(chainName(i))] = []string{chainName(i + 1)}
	}

	t.Run("inside the cap", func(t *testing.T) {
		anc := &fakeAncestry{edges: chain}
		r := New(index(javaMethod(10, chainName(3), "settle"),
			javaMethod(11, "Invoice", "settle")), Options{Ancestry: anc})

		got, _, _ := runTwoPasses(t, r, callOn("Payment", "settle"))
		if len(got) != 1 || got[0].DstID != 10 {
			t.Fatalf("edges %+v, want the definition four levels up", got)
		}
	})

	t.Run("past the cap", func(t *testing.T) {
		anc := &fakeAncestry{edges: chain}
		r := New(index(javaMethod(10, chainName(30), "settle"),
			javaMethod(11, "Invoice", "settle")), Options{Ancestry: anc})

		got, outs, _ := runTwoPasses(t, r, callOn("Payment", "settle"))
		if len(got) != 0 || len(outs) != 0 {
			t.Fatalf("walked past the depth cap of %d: %+v / %+v", MaxAncestryDepth, got, outs)
		}
		if anc.calls > MaxAncestryDepth {
			t.Errorf("%d supertype requests, capped at %d", anc.calls, MaxAncestryDepth)
		}
	})
}

func chainName(i int) string {
	return "T" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}
