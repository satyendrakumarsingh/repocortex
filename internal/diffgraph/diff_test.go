package diffgraph

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestRunAgainstItselfIsEmpty is the property the feature is worthless without.
//
// Two forms, and the second is the load-bearing one. Diffing run 1 against run
// 1 could pass by accident if Compare short-circuited on equal ids, so the
// second case builds a SEPARATE run with identical content and completely
// different rowids — the situation two indexing passes over one commit actually
// produce — and requires that to compare empty too.
func TestRunAgainstItselfIsEmpty(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	baseline(src, 2, 5_000, 0) // identical content, disjoint id space

	t.Run("same run id", func(t *testing.T) {
		rep := mustDiff(t, src, 1, 1)
		requireEmpty(t, rep)
	})

	t.Run("identical content, different ids", func(t *testing.T) {
		rep := mustDiff(t, src, 1, 2)
		requireEmpty(t, rep)
		if len(rep.Caveats.Notes) != 0 {
			t.Errorf("clean comparison should have no caveats, got %v", rep.Caveats.Notes)
		}
		if !rep.Caveats.Comparable {
			t.Error("two ready runs must be comparable")
		}
	})

	t.Run("identical content, reversed row order", func(t *testing.T) {
		// The registry reads under a total ORDER BY, so this cannot happen in
		// production; asserting it anyway proves the diff derives its own
		// order from content rather than inheriting the store's.
		src.reverse(2)
		rep := mustDiff(t, src, 1, 2)
		requireEmpty(t, rep)
		src.reverse(2)
	})
}

func requireEmpty(t *testing.T, rep *Report) {
	t.Helper()
	if !rep.Empty() {
		var sb strings.Builder
		if err := Render(&sb, rep); err != nil {
			t.Fatalf("render: %v", err)
		}
		t.Fatalf("expected an empty report, got:\n%s", sb.String())
	}
}

// TestSeededChangesAreReportedWithEvidenceOnBothSides seeds one added edge,
// one removed edge and one confidence downgrade, and requires each to be
// classified correctly and to carry the facts from BOTH runs.
//
// The downgrade is the interesting one: nothing about the source text has to
// change for exact -> high to happen. A build that stopped producing a SCIP
// index looks identical to a line diff and is a real loss of certainty.
func TestSeededChangesAreReportedWithEvidenceOnBothSides(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)

	// Head: same three symbols, but
	//  - handleRefund -> Refund falls from exact/compiler to high/source_code
	//  - the READS edge to refunds is gone, replaced by a WRITES edge
	//  - a new REFERENCES edge appears from the endpoint to the service
	b := newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto")
	b.file("pkg/api.go", "ok").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:GET /refunds", "GET /refunds", model.KindEndpoint, 10).
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 20).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:GET /refunds", "pkg/api.go:handleRefund", model.EdgeExposes,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 10).
		edge("pkg/api.go:handleRefund", "pkg/service.go:Refund", model.EdgeCalls,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 22).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeWrites,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 9).
		endpoint("GET /refunds", "pkg/api.go", 10, "handleRefund", "pkg/api.go").
		table("refunds", true, 0, 1)

	rep := mustDiff(t, src, 1, 2)

	if len(rep.Symbols) != 0 {
		t.Errorf("no symbol changed, got %d symbol changes: %+v", len(rep.Symbols), rep.Symbols)
	}

	added := findEdge(t, rep, EdgeAdded, model.EdgeWrites)
	if added.To == nil || added.From != nil {
		t.Fatalf("an added edge must carry head evidence and no base evidence: %+v", added)
	}
	if added.Key.Dst.FQN != "db/schema.sql:refunds" || added.To.SiteCount != 1 {
		t.Errorf("added WRITES edge wrong: %+v", added)
	}
	if len(added.To.Sites) != 1 || added.To.Sites[0].Line != 9 {
		t.Errorf("added edge must carry its evidence site, got %+v", added.To.Sites)
	}

	removed := findEdge(t, rep, EdgeRemoved, model.EdgeReads)
	if removed.From == nil || removed.To != nil {
		t.Fatalf("a removed edge must carry base evidence and no head evidence: %+v", removed)
	}
	if len(removed.From.Sites) != 1 || removed.From.Sites[0].Line != 8 {
		t.Errorf("removed edge must carry its base evidence site, got %+v", removed.From.Sites)
	}

	changed := findEdge(t, rep, EdgeConfidenceChanged, model.EdgeCalls)
	if changed.Direction != Downgraded {
		t.Errorf("exact -> high is a downgrade, got %q", changed.Direction)
	}
	if changed.From == nil || changed.To == nil {
		t.Fatal("a confidence transition must carry evidence from BOTH runs")
	}
	if changed.From.Confidence != model.ConfidenceExact || changed.From.Source != model.SourceCompiler {
		t.Errorf("base side wrong: %+v", changed.From)
	}
	if changed.To.Confidence != model.ConfidenceHigh || changed.To.Source != model.SourceSourceCode {
		t.Errorf("head side wrong: %+v", changed.To)
	}
	if len(changed.From.Sites) == 0 || len(changed.To.Sites) == 0 {
		t.Error("both sides of a transition must carry evidence sites")
	}

	// The table view of the same facts: refunds lost a reader and gained a
	// writer. "This table gained a writer" is the sentence this exists for.
	if len(rep.Tables) != 1 {
		t.Fatalf("expected one table change, got %+v", rep.Tables)
	}
	tc := rep.Tables[0]
	if tc.Table != "refunds" || tc.WritersDelta != 1 || tc.ReadersDelta != -1 {
		t.Errorf("table change wrong: %+v", tc)
	}
}

func findEdge(t *testing.T, rep *Report, change ChangeKind, kind model.EdgeKind) EdgeChange {
	t.Helper()
	var found []EdgeChange
	for _, c := range rep.Edges {
		if c.Change == change && c.Key.Kind == kind {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s %s, got %d in %+v", change, kind, len(found), rep.Edges)
	}
	return found[0]
}

// TestShiftedSymbolIsMovedNotRecreated is the noise test.
//
// Adding an import pushes every definition in the file down. If the diff called
// that a thousand removals and a thousand additions, nobody would run it twice.
func TestShiftedSymbolIsMovedNotRecreated(t *testing.T) {
	const shift = 10

	src := newFakeSource()
	baseline(src, 1, 0, 0)
	baseline(src, 2, 5_000, shift)

	rep := mustDiff(t, src, 1, 2)

	if len(rep.Edges) != 0 {
		t.Errorf("edge identity excludes evidence position, so a shift must change no edge; got %+v", rep.Edges)
	}
	if len(rep.Tables) != 0 {
		t.Errorf("a shift must change no table access; got %+v", rep.Tables)
	}

	moved := 0
	for _, c := range rep.Symbols {
		switch c.Change {
		case SymbolMoved:
			moved++
			if c.LineDelta != shift {
				t.Errorf("%s: line delta %d, want %d", c.Key, c.LineDelta, shift)
			}
			if c.From == nil || c.To == nil {
				t.Errorf("%s: a move must show both positions", c.Key)
			}
		default:
			t.Errorf("a shifted symbol must not be reported as %s: %s", c.Change, c.Key)
		}
	}
	// Three of the four symbols live in the shifted files; the table symbol in
	// db/schema.sql does not move.
	if moved != 3 {
		t.Errorf("expected 3 moved symbols, got %d: %+v", moved, rep.Symbols)
	}
}

// TestCrossFileMoveIsNotRemoveAndAdd covers the harder move: the definition
// changed file, so its path-qualified FQN changed and the content key cannot
// match. Pairing is by (kind, name, signature) and only when unambiguous.
func TestCrossFileMoveIsNotRemoveAndAdd(t *testing.T) {
	src := newFakeSource()
	newRun(src, 1, 0, model.StatusReady, "rev1", "auto").
		file("pkg/old.go", "ok").
		sym("pkg/old.go", "pkg/old.go:Refund", "Refund", model.KindMethod, 12)
	newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto").
		file("pkg/new.go", "ok").
		sym("pkg/new.go", "pkg/new.go:Refund", "Refund", model.KindMethod, 30)

	rep := mustDiff(t, src, 1, 2)
	if len(rep.Symbols) != 1 {
		t.Fatalf("expected one change, got %+v", rep.Symbols)
	}
	c := rep.Symbols[0]
	if c.Change != SymbolMoved {
		t.Fatalf("expected a move, got %s", c.Change)
	}
	if c.FromKey == nil || c.FromKey.Path != "pkg/old.go" || c.Key.Path != "pkg/new.go" {
		t.Errorf("a cross-file move must name both sides: %+v", c)
	}
}

// TestAmbiguousMoveStaysRemoveAndAdd guards the conservatism of the pairing:
// two identical-looking candidates on one side must NOT be guessed at.
func TestAmbiguousMoveStaysRemoveAndAdd(t *testing.T) {
	src := newFakeSource()
	newRun(src, 1, 0, model.StatusReady, "rev1", "auto").
		file("pkg/old.go", "ok").
		sym("pkg/old.go", "pkg/old.go:Refund", "Refund", model.KindMethod, 12)
	newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto").
		file("pkg/a.go", "ok").
		file("pkg/b.go", "ok").
		sym("pkg/a.go", "pkg/a.go:Refund", "Refund", model.KindMethod, 30).
		sym("pkg/b.go", "pkg/b.go:Refund", "Refund", model.KindMethod, 40)

	rep := mustDiff(t, src, 1, 2)
	var kinds []string
	for _, c := range rep.Symbols {
		kinds = append(kinds, string(c.Change))
	}
	want := []string{string(SymbolAdded), string(SymbolAdded), string(SymbolRemoved)}
	if strings.Join(sorted(kinds), ",") != strings.Join(want, ",") {
		t.Errorf("ambiguous move must not be guessed; got %v", kinds)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestSymbolLosesItsLastCaller covers the fact the package doc leads with.
func TestSymbolLosesItsLastCaller(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)

	// Head keeps every symbol but drops the CALLS edge into Refund.
	b := newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto")
	b.file("pkg/api.go", "ok").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:GET /refunds", "GET /refunds", model.KindEndpoint, 10).
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 20).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:GET /refunds", "pkg/api.go:handleRefund", model.EdgeExposes,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 10).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeReads,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 8).
		endpoint("GET /refunds", "pkg/api.go", 10, "handleRefund", "pkg/api.go").
		table("refunds", true, 1, 0)

	rep := mustDiff(t, src, 1, 2)

	var orphan *SymbolChange
	for i := range rep.Symbols {
		if rep.Symbols[i].Change == SymbolOrphaned {
			orphan = &rep.Symbols[i]
		}
	}
	if orphan == nil {
		t.Fatalf("expected an orphaned symbol, got %+v", rep.Symbols)
	}
	if orphan.Key.FQN != "pkg/service.go:Refund" {
		t.Errorf("wrong symbol orphaned: %s", orphan.Key)
	}
	if orphan.From.Inbound != 1 || orphan.To.Inbound != 0 {
		t.Errorf("orphaning must show the inbound count on both sides: %+v -> %+v", orphan.From, orphan.To)
	}
}

// TestEndpointHandlerChange covers "the route still exists, but it now runs
// something else" — an add plus a remove would bury it.
func TestEndpointHandlerChange(t *testing.T) {
	src := newFakeSource()
	newRun(src, 1, 0, model.StatusReady, "rev1", "auto").
		file("pkg/api.go", "ok").
		endpoint("GET /refunds", "pkg/api.go", 10, "handleRefund", "pkg/api.go").
		endpoint("GET /old", "pkg/api.go", 20, "handleOld", "pkg/api.go")
	newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto").
		file("pkg/api.go", "ok").
		endpoint("GET /refunds", "pkg/api.go", 10, "handleRefundV2", "pkg/api.go").
		endpoint("POST /refunds", "pkg/api.go", 30, "createRefund", "pkg/api.go")

	rep := mustDiff(t, src, 1, 2)
	if len(rep.Endpoints) != 3 {
		t.Fatalf("expected 3 endpoint changes, got %+v", rep.Endpoints)
	}
	// Sorted by route then path: "GET /old", "GET /refunds", "POST /refunds".
	if rep.Endpoints[0].Change != EndpointRemoved || rep.Endpoints[0].Key.Route != "GET /old" {
		t.Errorf("first change wrong: %+v", rep.Endpoints[0])
	}
	if rep.Endpoints[1].Change != EndpointHandlerChanged {
		t.Errorf("re-pointed route must report as a handler change: %+v", rep.Endpoints[1])
	}
	if got := rep.Endpoints[1].To.Handlers; len(got) != 1 || got[0] != "pkg/api.go:handleRefundV2" {
		t.Errorf("handler change must name the new handler: %v", got)
	}
	if rep.Endpoints[2].Change != EndpointAdded || rep.Endpoints[2].Key.Route != "POST /refunds" {
		t.Errorf("third change wrong: %+v", rep.Endpoints[2])
	}
}

// TestCaveatsStateWhatCouldNotBeCompared. A missing edge in a degraded run is
// not evidence that a call was deleted, and the report has to say so.
func TestCaveatsStateWhatCouldNotBeCompared(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)

	b := newRun(src, 2, 5_000, model.StatusDegraded, "rev2", "exact")
	b.src.runs[2].ParseFailures = 3
	b.file("pkg/api.go", "failed").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeReads,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 8).
		table("refunds", true, 1, 0)

	rep := mustDiff(t, src, 1, 2)

	joined := strings.Join(rep.Caveats.Notes, "\n")
	for _, want := range []string{"degraded", "accuracy mode differs", "did not parse cleanly"} {
		if !strings.Contains(joined, want) {
			t.Errorf("caveats must mention %q; got:\n%s", want, joined)
		}
	}
	if rep.Caveats.UnreliableFiles != 1 || rep.Caveats.UnreliablePaths[0] != "pkg/api.go" {
		t.Errorf("unreliable paths wrong: %+v", rep.Caveats)
	}

	// Every change involving the unparsed file must be flagged rather than
	// asserted.
	sawUncertain := false
	for _, c := range rep.Symbols {
		if strings.HasPrefix(c.Key.Path, "pkg/api.go") {
			if !c.Uncertain {
				t.Errorf("change in an unparsed file must be uncertain: %+v", c)
			}
			sawUncertain = true
		}
	}
	if !sawUncertain {
		t.Error("expected changes inside the unparsed file")
	}
	for _, c := range rep.Edges {
		if c.Key.Src.Path == "pkg/api.go" && !c.Uncertain {
			t.Errorf("edge out of an unparsed file must be uncertain: %+v", c)
		}
	}
}

// TestRunStillIndexingIsNotComparable: absence proves nothing at all here.
func TestRunStillIndexingIsNotComparable(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	newRun(src, 2, 5_000, model.StatusIndexing, "rev2", "auto").file("pkg/api.go", "ok")

	rep := mustDiff(t, src, 1, 2)
	if rep.Caveats.Comparable {
		t.Error("a run that is still indexing must not be reported as comparable")
	}
	if !strings.Contains(strings.Join(rep.Caveats.Notes, "\n"), "proves nothing") {
		t.Errorf("notes must say so: %v", rep.Caveats.Notes)
	}
}

// TestOutputIsBounded: a diff between distant commits must not be unbounded,
// and a bounded report must say exactly what it dropped.
func TestOutputIsBounded(t *testing.T) {
	src := newFakeSource()
	newRun(src, 1, 0, model.StatusReady, "rev1", "auto").file("pkg/a.go", "ok")
	b := newRun(src, 2, 5_000, model.StatusReady, "rev2", "auto").file("pkg/a.go", "ok")
	for i := 0; i < 25; i++ {
		fqn := "pkg/a.go:fn" + string(rune('a'+i%25))
		b.sym("pkg/a.go", fqn, "fn", model.KindFunction, 10+i)
	}

	rep := mustDiff(t, src, 1, 2, WithMaxPerSection(5))
	if len(rep.Symbols) != 5 {
		t.Fatalf("expected the section capped at 5, got %d", len(rep.Symbols))
	}
	if len(rep.Truncations) != 1 {
		t.Fatalf("a capped section must record a truncation, got %+v", rep.Truncations)
	}
	tr := rep.Truncations[0]
	if tr.Section != "symbols" || tr.Total != 25 || tr.Omitted != 20 || tr.Shown != 5 {
		t.Errorf("truncation wrong: %+v", tr)
	}
	if tr.OrderedBy == "" {
		t.Error("a truncation must say what order it kept the first N under")
	}
}

// TestDeterminism: identical inputs, byte-identical report — twice over, and
// again with the second run's rows reversed.
func TestDeterminism(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	b := newRun(src, 2, 5_000, model.StatusDegraded, "rev2", "exact")
	b.src.runs[2].ParseFailures = 2
	b.file("pkg/api.go", "partial").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 25).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("pkg/service.go", "pkg/service.go:Audit", "Audit", model.KindMethod, 60).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:handleRefund", "pkg/service.go:Refund", model.EdgeCalls,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 27).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeWrites,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 9).
		edge("pkg/service.go:Audit", "db/schema.sql:refunds", model.EdgeReads,
			model.ConfidenceMedium, model.SourceInferred, "pkg/service.go", 62).
		endpoint("GET /refunds", "pkg/api.go", 10, "", "").
		table("refunds", true, 1, 1).
		table("ledger", false, 2, 0)

	first := render(t, mustDiff(t, src, 1, 2))
	if !strings.Contains(first, "SYMBOLS") || !strings.Contains(first, "EDGES") {
		t.Fatalf("fixture produced nothing to compare:\n%s", first)
	}

	for i := 0; i < 5; i++ {
		if got := render(t, mustDiff(t, src, 1, 2)); got != first {
			t.Fatalf("run %d differed:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}

	src.reverse(1)
	src.reverse(2)
	if got := render(t, mustDiff(t, src, 1, 2)); got != first {
		t.Fatalf("report depends on row arrival order:\n--- sorted ---\n%s\n--- reversed ---\n%s", first, got)
	}
}

func render(t *testing.T, rep *Report) string {
	t.Helper()
	var sb strings.Builder
	if err := Render(&sb, rep); err != nil {
		t.Fatalf("render: %v", err)
	}
	return sb.String()
}

// TestEqualDigestsWithDifferencesIsFlagged: the digest is the ground truth for
// "the content is the same". If it says equal and this package reports
// differences, the bug is here, and the report must not be believed.
func TestEqualDigestsWithDifferencesIsFlagged(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	baseline(src, 2, 5_000, 7) // shifted, so the diff is non-empty
	src.runs[1].RegistryDigest = "abc123"
	src.runs[2].RegistryDigest = "abc123"

	rep := mustDiff(t, src, 1, 2)
	if !rep.Caveats.DigestsEqual {
		t.Fatal("equal digests must be recorded")
	}
	if !strings.Contains(strings.Join(rep.Caveats.Notes, "\n"), "INCONSISTENT") {
		t.Errorf("a non-empty diff under equal digests must be flagged: %v", rep.Caveats.Notes)
	}
}

// TestCompareHonoursCancellation — CLAUDE.md §4.1.
func TestCompareHonoursCancellation(t *testing.T) {
	src := newFakeSource()
	baseline(src, 1, 0, 0)
	baseline(src, 2, 5_000, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d, err := New()
	if err != nil {
		t.Fatalf("new differ: %v", err)
	}
	if _, err := d.Compare(ctx, Side{Source: src, RunID: 1}, Side{Source: src, RunID: 2}); err == nil {
		t.Fatal("a cancelled context must abort the comparison")
	}
}

// TestOptionsAndSidesReject nonsense rather than silently clamping it.
func TestOptionsAndSidesReject(t *testing.T) {
	if _, err := New(WithMaxPerSection(0)); err == nil {
		t.Error("a non-positive section cap must be rejected")
	}
	d, err := New()
	if err != nil {
		t.Fatalf("new differ: %v", err)
	}
	if _, err := d.Compare(context.Background(), Side{}, Side{Source: newFakeSource()}); err == nil {
		t.Error("a side with no source must be rejected")
	}
}
