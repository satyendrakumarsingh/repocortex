package scip

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// These tests hold the CALLS join to the two claims that make it legitimate:
// a compiler reference standing on a parsed call line becomes a call, and a
// compiler reference standing anywhere else never does. Both directions are
// checked, because the failure that matters is not "we produced too few calls"
// — it is a type annotation stamped CALLS at confidence=exact, which is
// indistinguishable from a real fact once it is in the database.

const (
	symCallerFn = "scip-go gomod example.com/repo v1.0.0 `internal/api`/Handle()."
	symTargetFn = "scip-go gomod example.com/repo v1.0.0 `internal/svc`/Service#updateStatus()."
	symTypeName = "scip-go gomod example.com/repo v1.0.0 `internal/svc`/Service#"
	symImported = "scip-go gomod fmt v1.0.0 fmt/Println()."
	callerPath  = "internal/api/handler.go"
)

// callFixtureDoc is one file whose four interesting positions are, in model
// (1-indexed) lines:
//
//	 2  an import of fmt.Println                    -> IMPORTS, never a call
//	 7  a call to updateStatus                      -> the promotion
//	 9  a `var s *Service` type annotation          -> stays REFERENCES
//	12  `Service{}.updateStatus()` — two targets    -> ambiguous, stays REFERENCES
//
// Lines 7 and 12 are BOTH parsed call sites in the fixture below. That is the
// point of line 12: it is not excluded for want of a parser fact, it is
// excluded because the compiler put two targets on it.
func callFixtureDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath:     callerPath,
		Language:         "go",
		PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		Symbols: []*scippb.SymbolInformation{
			{Symbol: symCallerFn, Kind: scippb.SymbolInformation_Function, DisplayName: "Handle"},
		},
		Occurrences: []*scippb.Occurrence{
			ref(symImported, rng(1, 1, 1, 12), scippb.SymbolRole_Import),
			def(symCallerFn, rng(3, 5, 3, 11), rng(3, 0, 20, 1)),
			ref(symTargetFn, rng(6, 8, 6, 20), scippb.SymbolRole_ReadAccess),
			ref(symTypeName, rng(8, 9, 8, 16), scippb.SymbolRole_ReadAccess),
			ref(symTypeName, rng(11, 4, 11, 11), scippb.SymbolRole_ReadAccess),
			ref(symTargetFn, rng(11, 14, 11, 26), scippb.SymbolRole_ReadAccess),
		},
	}
}

// callFixtureIDs is the repository's symbol table for the fixture. Ids are
// arbitrary but distinct: distinctness is what makes line 12 ambiguous.
var callFixtureIDs = map[string]int64{
	// The file-scope anchor. A real import gives it a rowid only where RepoCortex
	// indexed a file symbol; here it exists so the import occurrence at line 2,
	// which sits outside every definition body, still produces an edge to
	// assert about.
	callerPath:  50,
	symCallerFn: 10,
	symTargetFn: 20,
	symTypeName: 30,
	symImported: 40,
}

// callFixtureNames is RepoCortex's own name for each indexed symbol — the third
// observation the join checks, held against the parser's record of what was
// called on the line.
var callFixtureNames = map[int64]string{
	50: "handler.go",
	10: "Handle",
	20: "updateStatus",
	30: "Service",
	40: "Println",
}

// callFixtureEdges runs the fixture through the real path — ingest, then
// resolve relations to rowids — and returns edges ready for PromoteCalls.
func callFixtureEdges(t *testing.T) []model.Edge {
	t.Helper()
	data := indexBytes(t, metadata("file:///build/repo"), callFixtureDoc())
	facts, _, err := Collect(context.Background(), bytes.NewReader(data), Options{
		RepoRoot:   "/build/repo",
		KnownPaths: []string{callerPath},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("collected %d file(s), want 1", len(facts))
	}

	f := facts[0]
	// Replace the ingester's placeholder ids with the repository's, exactly as
	// the importer does before calling Edges.
	kept := f.Symbols[:0]
	for i := range f.Symbols {
		s := f.Symbols[i]
		id, ok := callFixtureIDs[s.FQN]
		if !ok {
			continue
		}
		s.ID = id
		kept = append(kept, s)
	}
	f.Symbols = kept

	edges, _, err := f.Edges(func(fqn string) (int64, bool) {
		id, ok := callFixtureIDs[fqn]
		return id, ok
	})
	if err != nil {
		t.Fatalf("resolve edges: %v", err)
	}
	return edges
}

// edgeAtLine finds the edge to dst on a model line.
func edgeAtLine(t *testing.T, edges []model.Edge, line int, dst int64) model.Edge {
	t.Helper()
	for _, e := range edges {
		if e.Evidence.Range.Start.Line == line && e.DstID == dst {
			return e
		}
	}
	var got []string
	for _, e := range edges {
		got = append(got, fmt.Sprintf("line %d -> %d (%s)", e.Evidence.Range.Start.Line, e.DstID, e.Kind))
	}
	t.Fatalf("no edge at line %d to %d; have %v", line, dst, got)
	return model.Edge{}
}

// TestPromoteCallsJoinsParserAndCompiler is the whole feature in one assertion:
// a position both sides observed becomes a CALLS edge at confidence=exact, and
// the confidence is honest because neither half of the claim was inferred.
func TestPromoteCallsJoinsParserAndCompiler(t *testing.T) {
	edges := callFixtureEdges(t)

	before := edgeAtLine(t, edges, 7, callFixtureIDs[symTargetFn])
	if before.Kind != model.EdgeReferences {
		t.Fatalf("setup: ingest already produced %s at line 7; the fixture no longer tests the join",
			before.Kind)
	}

	calls := NewCallSiteSet(1)
	calls.Add(callerPath, 7, "updateStatus", "")

	st, err := PromoteCalls(callerPath, edges, callFixtureNames, calls)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if st.Promoted != 1 {
		t.Errorf("promoted %d edge(s), want 1: %+v", st.Promoted, st)
	}

	got := edgeAtLine(t, edges, 7, callFixtureIDs[symTargetFn])
	if got.Kind != model.EdgeCalls {
		t.Errorf("line 7 is %s, want CALLS: the parser recorded a call there and the compiler "+
			"resolved it to exactly one target", got.Kind)
	}
	if got.Confidence != model.ConfidenceExact {
		t.Errorf("promoted call has confidence %q, want exact", got.Confidence)
	}
	if got.Evidence.Source != model.SourceCompiler {
		t.Errorf("promoted call has source %q, want compiler", got.Evidence.Source)
	}
	if got.SrcID != callFixtureIDs[symCallerFn] {
		t.Errorf("promoted call starts at %d, want the enclosing definition %d",
			got.SrcID, callFixtureIDs[symCallerFn])
	}
}

// TestPromoteCallsLeavesNonCallReferencesAlone is the test that keeps the
// feature honest.
//
// A type annotation and an import are compiler-proven references at positions
// the parser recorded no call at. If either turned into a CALLS edge, the
// exact stamp would be carrying an inference — the precise failure kinds.go
// refuses — and `repocortex impact` would report a change reaching code that
// never calls it.
func TestPromoteCallsLeavesNonCallReferencesAlone(t *testing.T) {
	edges := callFixtureEdges(t)

	// The parser saw calls at 7 and 12 only. Nothing was parsed at 2 or 9.
	calls := NewCallSiteSet(2)
	calls.Add(callerPath, 7, "updateStatus", "")
	calls.Add(callerPath, 12, "updateStatus", "")

	if _, err := PromoteCalls(callerPath, edges, callFixtureNames, calls); err != nil {
		t.Fatalf("promote: %v", err)
	}

	cases := []struct {
		name string
		line int
		dst  int64
		want model.EdgeKind
		why  string
	}{
		{"import", 2, callFixtureIDs[symImported], model.EdgeImports,
			"an import is not a call and was never a REFERENCES edge to begin with"},
		{"type annotation", 9, callFixtureIDs[symTypeName], model.EdgeReferences,
			"the parser recorded no call on this line, so nothing establishes call-ness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := edgeAtLine(t, edges, tc.line, tc.dst)
			if got.Kind != tc.want {
				t.Errorf("line %d is %s, want %s: %s", tc.line, got.Kind, tc.want, tc.why)
			}
		})
	}
}

// TestPromoteCallsDeclinesAmbiguousCallLines covers the restriction the whole
// construction rests on.
//
// Line 12 IS a parsed call site, but the compiler resolved two distinct symbols
// there — `Service` and `updateStatus`. Promoting both would stamp a type name
// as a call; promoting one would be a coin toss dressed up as exact. Neither is
// acceptable, so both stay REFERENCES and the decline is counted.
func TestPromoteCallsDeclinesAmbiguousCallLines(t *testing.T) {
	edges := callFixtureEdges(t)

	calls := NewCallSiteSet(1)
	calls.Add(callerPath, 12, "updateStatus", "")

	st, err := PromoteCalls(callerPath, edges, callFixtureNames, calls)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if st.Promoted != 0 {
		t.Errorf("promoted %d edge(s) on an ambiguous line, want 0", st.Promoted)
	}
	if st.CallLines != 1 || st.Ambiguous != 1 {
		t.Errorf("counted %d call line(s), %d ambiguous; want 1 and 1 — a decline that is not "+
			"reported is indistinguishable from a join that never ran: %+v", st.CallLines, st.Ambiguous, st)
	}
	for _, dst := range []int64{callFixtureIDs[symTypeName], callFixtureIDs[symTargetFn]} {
		if got := edgeAtLine(t, edges, 12, dst); got.Kind != model.EdgeReferences {
			t.Errorf("line 12 -> %d is %s, want REFERENCES", dst, got.Kind)
		}
	}
}

// TestPromoteCallsWithoutParserFactsPromotesNothing pins the degradation. An
// import into a repository whose references were never resolved has only one
// of the two facts, and one fact is not a call.
func TestPromoteCallsWithoutParserFactsPromotesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls CallSites
	}{
		{"nil", nil},
		{"empty", NewCallSiteSet(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := callFixtureEdges(t)
			st, err := PromoteCalls(callerPath, edges, callFixtureNames, tc.calls)
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			if st.Promoted != 0 || st.CallLines != 0 {
				t.Errorf("promoted %d with %d call line(s) and no parser facts; want 0 and 0",
					st.Promoted, st.CallLines)
			}
			for _, e := range edges {
				if e.Kind == model.EdgeCalls {
					t.Fatalf("a CALLS edge appeared with no parser fact behind it: %+v", e)
				}
			}
		})
	}
}

// TestPromoteCallsIsDeterministic. Three determinism bugs have shipped in this
// codebase from unordered reads feeding first-wins rules, so the verdict is
// checked to be a function of the inputs and nothing else — including Go's map
// iteration order, which is randomised per run and per range statement.
func TestPromoteCallsIsDeterministic(t *testing.T) {
	calls := NewCallSiteSet(2)
	calls.Add(callerPath, 7, "updateStatus", "")
	calls.Add(callerPath, 12, "updateStatus", "")

	var want []string
	var wantStats PromoteStats
	for i := range 25 {
		edges := callFixtureEdges(t)
		st, err := PromoteCalls(callerPath, edges, callFixtureNames, calls)
		if err != nil {
			t.Fatalf("promote: %v", err)
		}
		got := make([]string, 0, len(edges))
		for _, e := range edges {
			got = append(got, fmt.Sprintf("%d->%d %s %s @%s:%d",
				e.SrcID, e.DstID, e.Kind, e.Confidence,
				e.Evidence.FilePath, e.Evidence.Range.Start.Line))
		}
		if i == 0 {
			want, wantStats = got, st
			continue
		}
		if st != wantStats {
			t.Fatalf("run %d reported %+v, run 0 reported %+v", i, st, wantStats)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d produced %d edge(s), run 0 produced %d", i, len(got), len(want))
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("run %d edge %d is %q, run 0 had %q", i, j, got[j], want[j])
			}
		}
	}
}

// TestPromoteCallsDeclinesWhenTheNameDisagrees is the constraint added after
// measuring 11,459 bad edges on a real repository without it.
//
// Line 9 holds `var s *Service` in this fixture; suppose instead the parser had
// recorded a call there — to something else entirely, a library function whose
// definition RepoCortex never indexed. The compiler's only in-repository target
// on the line is then the TYPE Service, and every other check passes: one
// target, and a parsed call on the line. Promoting would stamp a type
// annotation CALLS at exact. The names disagree, so it does not.
func TestPromoteCallsDeclinesWhenTheNameDisagrees(t *testing.T) {
	edges := callFixtureEdges(t)

	calls := NewCallSiteSet(1)
	calls.Add(callerPath, 9, "relationship", "") // a library call, not `Service`

	st, err := PromoteCalls(callerPath, edges, callFixtureNames, calls)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if st.Promoted != 0 {
		t.Errorf("promoted %d edge(s) whose target is merely NEAR a call, want 0", st.Promoted)
	}
	if st.CallLines != 1 || st.NameMismatch != 1 {
		t.Errorf("counted %d call line(s), %d name mismatch(es); want 1 and 1: %+v",
			st.CallLines, st.NameMismatch, st)
	}
	if got := edgeAtLine(t, edges, 9, callFixtureIDs[symTypeName]); got.Kind != model.EdgeReferences {
		t.Errorf("line 9 is %s, want REFERENCES: `Service` is not what the parser saw called there",
			got.Kind)
	}
}

// TestPromoteCallsDeclinesAReceiver is the 13.6% correction.
//
// `updateStatus` is written as `svc.updateStatus(...)`, so the parser emits a
// call `updateStatus` WITH receiver `svc` and, at the same column, a bare call
// `svc`. Here the compiler's single target on line 7 is `updateStatus`, and the
// parser also records `updateStatus` as the receiver of something on that line
// — the shape `updateStatus.result()` produces. Whatever the compiler resolved,
// it did not prove that the receiver is called, so the edge is not stamped
// exact.
func TestPromoteCallsDeclinesAReceiver(t *testing.T) {
	edges := callFixtureEdges(t)

	calls := NewCallSiteSet(1)
	calls.Add(callerPath, 7, "updateStatus", "")       // the bare row the parser also writes
	calls.Add(callerPath, 7, "result", "updateStatus") // the specific row that contradicts it

	st, err := PromoteCalls(callerPath, edges, callFixtureNames, calls)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if st.Promoted != 0 {
		t.Errorf("promoted %d edge(s) for a receiver, want 0: %+v", st.Promoted, st)
	}
	if st.Receivers != 1 {
		t.Errorf("the decline was not reported as a receiver: %+v", st)
	}
	if got := edgeAtLine(t, edges, 7, callFixtureIDs[symTargetFn]); got.Kind != model.EdgeReferences {
		t.Errorf("line 7 is %s, want REFERENCES: a receiver is not a callee", got.Kind)
	}
}
