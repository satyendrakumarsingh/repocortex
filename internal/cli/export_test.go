package cli

import (
	"bytes"
	stdctx "context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/export"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestExportRejectsUnknownFormatAndNamesTheValidOnes(t *testing.T) {
	err := runExport(exportOptions{Format: "parquet"})
	if err == nil {
		t.Fatal("want an error for an unknown format")
	}
	if !strings.Contains(err.Error(), "dot") || !strings.Contains(err.Error(), "graphml") {
		t.Errorf("error should list the supported formats, got: %v", err)
	}
}

func TestExportWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/graph.dot"
	if err := writeAtomic(target, []byte("digraph {}\n")); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	// A second write must replace cleanly, leaving no temp file behind.
	if err := writeAtomic(target, []byte("digraph { a }\n")); err != nil {
		t.Fatalf("writeAtomic second: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d files, want 1 (a temp file leaked)", len(entries))
	}
}

// TestExportLossyNoteGoesToStderrNotStdout pins requirement 4 of the export
// contract. A lossy format's caveat is for a person, and the person is not the
// only reader: `cortex export --format csv | column -s, -t` puts a machine on
// stdout. A caveat printed there is not a caveat, it is a corrupt first row.
func TestExportLossyNoteGoesToStderrNotStdout(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{
			{Key: "a/x.go:One", Kind: model.KindFunction},
			{Key: "b/y.go:Two", Kind: model.KindFunction},
		},
		[]metrics.Edge{{
			From: "a/x.go:One", To: "b/y.go:Two",
			Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		}},
	)

	cases := []struct {
		name       string
		format     export.Format
		wantOnErr  bool // the exporter declares itself lossy
		mustBeOnIn string
	}{
		{name: "csv is lossy", format: export.FormatCSV, wantOnErr: true, mustBeOnIn: "a/x.go:One"},
		{name: "json is faithful", format: export.FormatJSON, wantOnErr: false, mustBeOnIn: "a/x.go:One"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex, err := export.ForFormat(tc.format)
			if err != nil {
				t.Fatalf("ForFormat(%q): %v", tc.format, err)
			}
			var stdout, stderr bytes.Buffer
			if err := emitExport(stdctx.Background(), ex, g, &stdout, &stderr, ""); err != nil {
				t.Fatalf("emitExport: %v", err)
			}

			note := ex.Lossy()
			if tc.wantOnErr {
				if note == "" {
					t.Fatalf("%s should declare itself lossy", tc.format)
				}
				if !strings.Contains(stderr.String(), note) {
					t.Errorf("lossy note missing from stderr; stderr = %q", stderr.String())
				}
				if strings.Contains(stdout.String(), note) {
					t.Errorf("lossy note leaked into stdout:\n%s", stdout.String())
				}
			} else {
				if note != "" {
					t.Fatalf("%s unexpectedly declares itself lossy: %s", tc.format, note)
				}
				if stderr.Len() != 0 {
					t.Errorf("faithful format wrote to stderr: %q", stderr.String())
				}
			}

			if !strings.Contains(stdout.String(), tc.mustBeOnIn) {
				t.Errorf("stdout does not carry the graph; got:\n%s", stdout.String())
			}
		})
	}
}

// TestExportKeepsEverySymbolThatSharesAnFQN is the reproducer for the defect
// that a node key of FQN alone is not an identity. Three methods named String
// on three different types in one file share "internal/diffgraph/key.go:String"
// and used to arrive at metrics.NewGraph as one node — which not only lost two
// symbols but RE-PARENTED their edges onto the survivor, inventing
// relationships. Both halves are asserted: every symbol survives, and each
// keeps its own edge.
func TestExportKeepsEverySymbolThatSharesAnFQN(t *testing.T) {
	paths := map[int64]string{1: "internal/diffgraph/key.go", 2: "internal/z/z.go"}
	symbols := []model.Symbol{
		{ID: 1, Name: "String", FQN: "internal/diffgraph/key.go:String", FileID: 1,
			Kind: model.KindMethod, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 36}}},
		{ID: 2, Name: "String", FQN: "internal/diffgraph/key.go:String", FileID: 1,
			Kind: model.KindMethod, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 74}}},
		{ID: 3, Name: "String", FQN: "internal/diffgraph/key.go:String", FileID: 1,
			Kind: model.KindMethod, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 97}}},
		{ID: 4, Name: "Alone", FQN: "internal/z/z.go:Alone", FileID: 2,
			Kind: model.KindFunction, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 3}}},
	}
	edges := []model.Edge{
		{SrcID: 4, DstID: 1, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		{SrcID: 4, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		{SrcID: 4, DstID: 3, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
	}

	g, st, err := buildExportGraph(stdctx.Background(), paths, symbols, edges, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	if g.Order() != 4 {
		t.Fatalf("got %d nodes, want 4 — a symbol was deduplicated away; keys = %v", g.Order(), g.Keys())
	}
	if st.symbolsRead != 4 || st.nodesKept != 4 {
		t.Errorf("stats disagree with the graph: %+v", st)
	}
	// The unshared key must be untouched: disambiguation may not cost the
	// property that made FQN the right key, that it is diffable across runs.
	if !contains(g.Keys(), "internal/z/z.go:Alone") {
		t.Errorf("a non-colliding key was rewritten; keys = %v", g.Keys())
	}
	for _, want := range []string{
		"internal/diffgraph/key.go:String@internal/diffgraph/key.go#L36",
		"internal/diffgraph/key.go:String@internal/diffgraph/key.go#L74",
		"internal/diffgraph/key.go:String@internal/diffgraph/key.go#L97",
	} {
		if !contains(g.Keys(), want) {
			t.Errorf("missing disambiguated key %q; keys = %v", want, g.Keys())
		}
	}
	// Three distinct targets, not three edges onto one fabricated node.
	seen := map[string]bool{}
	for _, e := range g.Edges() {
		seen[e.To] = true
	}
	if len(seen) != 3 {
		t.Errorf("edges were re-parented onto %d distinct targets, want 3: %v", len(seen), seen)
	}
}

// TestExportAccountsForEveryEdgeItDoesNotWrite pins the second half of the
// contract: the counts printed must be the counts written, and an edge that
// does not reach the output must be named on stderr with a reason. A genuine
// self-loop (direct recursion) is the case that survives disambiguation and is
// still dropped, so it is the one worth pinning.
func TestExportAccountsForEveryEdgeItDoesNotWrite(t *testing.T) {
	paths := map[int64]string{1: "a/x.go"}
	symbols := []model.Symbol{
		{ID: 1, Name: "Walk", FQN: "a/x.go:Walk", FileID: 1, Kind: model.KindFunction,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 5}}},
		{ID: 2, Name: "Helper", FQN: "a/x.go:Helper", FileID: 1, Kind: model.KindFunction,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 20}}},
	}
	edges := []model.Edge{
		{SrcID: 1, DstID: 1, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}, // recursion
		{SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		{SrcID: 1, DstID: 99, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}, // dangling
	}

	g, st, err := buildExportGraph(stdctx.Background(), paths, symbols, edges, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	if st.edgesRead != 3 {
		t.Fatalf("edgesRead = %d, want 3", st.edgesRead)
	}
	if got := st.edgesRead - st.edgesKept; got != st.edgesDropped() {
		t.Errorf("dropped edges unaccounted: read-kept = %d, categories sum to %d (%+v)",
			got, st.edgesDropped(), st)
	}

	var stderr bytes.Buffer
	reportExportDeltas(&stderr, st, model.ConfidenceLow)
	if !strings.Contains(stderr.String(), "self-loop") {
		t.Errorf("the dropped recursive edge is not reported; stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "endpoint") {
		t.Errorf("the dropped dangling edge is not reported; stderr = %q", stderr.String())
	}

	// What was reported must be what was written.
	dir := t.TempDir()
	target := dir + "/graph.dot"
	ex, err := export.ForFormat(export.FormatDOT)
	if err != nil {
		t.Fatalf("ForFormat: %v", err)
	}
	var stdout, emitErr bytes.Buffer
	if err := emitExport(stdctx.Background(), ex, g, &stdout, &emitErr, target); err != nil {
		t.Fatalf("emitExport: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	written := strings.Count(string(body), "->")
	if written != len(g.Edges()) {
		t.Errorf("wrote %d arrows but the graph holds %d edges", written, len(g.Edges()))
	}
	if !strings.Contains(emitErr.String(), "wrote 2 nodes and 1 edges") {
		t.Errorf("reported counts do not match what was written; stderr = %q", emitErr.String())
	}
}

// TestExportEmptyGraphExplainsItself follows internal/cli/query.go's rule that
// an empty result is ambiguous. `--min-confidence exact` on an index with no
// compiler source produces a syntactically valid, entirely empty document; the
// document is correct and the silence is the bug.
func TestExportEmptyGraphExplainsItself(t *testing.T) {
	paths := map[int64]string{1: "a/x.go"}
	symbols := []model.Symbol{
		{ID: 1, Name: "Walk", FQN: "a/x.go:Walk", FileID: 1, Kind: model.KindFunction,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 5}}},
	}
	edges := []model.Edge{{SrcID: 1, DstID: 1, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}}

	g, st, err := buildExportGraph(stdctx.Background(), paths, symbols, edges, model.ConfidenceExact)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	if g.Order() != 0 {
		t.Fatalf("fixture is wrong: want an empty graph, got %d nodes", g.Order())
	}

	var stdout, stderr bytes.Buffer
	reportExportDeltas(&stderr, st, model.ConfidenceExact)
	ex, err := export.ForFormat(export.FormatDOT)
	if err != nil {
		t.Fatalf("ForFormat: %v", err)
	}
	if err := emitExport(stdctx.Background(), ex, g, &stdout, &stderr, ""); err != nil {
		t.Fatalf("emitExport: %v", err)
	}

	e := stderr.String()
	if !strings.Contains(e, "empty") {
		t.Errorf("empty export is not explained; stderr = %q", e)
	}
	if !strings.Contains(e, "exact") || !strings.Contains(e, "SCIP") {
		t.Errorf("the real reason (exact needs a compiler/SCIP source) is missing; stderr = %q", e)
	}
	if !strings.Contains(e, "1 symbol") {
		t.Errorf("stderr should say how much was dropped by the floor; stderr = %q", e)
	}
	// stdout must still be a valid, empty document.
	if !strings.Contains(stdout.String(), "digraph") {
		t.Errorf("stdout is not a valid empty document: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "empty") {
		t.Errorf("the explanation leaked into stdout: %q", stdout.String())
	}
}

// TestExportDisambiguatesCrossFileCollisionsByPath is the reproducer for
// FINDING A: two symbols with the same FQN in different files used to become
// "<fqn>#L1" and "<fqn>#L1#2" — an ordinal that names no file. Both keys must
// now carry their own file, and neither may be a bare "#<n>" ordinal.
func TestExportDisambiguatesCrossFileCollisionsByPath(t *testing.T) {
	paths := map[int64]string{
		1: ".github/workflows/ci.yml",
		2: ".github/workflows/release.yml",
	}
	symbols := []model.Symbol{
		{ID: 1, Name: "name", FQN: "config:name", FileID: 1, Kind: model.KindField,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}}},
		{ID: 2, Name: "name", FQN: "config:name", FileID: 2, Kind: model.KindField,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}}},
	}

	g, st, err := buildExportGraph(stdctx.Background(), paths, symbols, nil, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	if st.disambiguated != 2 {
		t.Fatalf("disambiguated = %d, want 2", st.disambiguated)
	}
	for _, want := range []string{
		"config:name@.github/workflows/ci.yml",
		"config:name@.github/workflows/release.yml",
	} {
		if !contains(g.Keys(), want) {
			t.Errorf("missing path-disambiguated key %q; keys = %v", want, g.Keys())
		}
	}
	for _, k := range g.Keys() {
		if strings.Contains(k, "#") {
			t.Errorf("cross-file collision produced a bare-ordinal-shaped key %q; it should name a file instead", k)
		}
	}
}

// TestExportDisambiguatesSameFileCollisionsByLine pins the other half:
// path alone cannot discriminate two symbols that share both an FQN and a
// file, so line must still do the job there — three String() methods in one
// file is the real case this reproduces (see
// TestExportKeepsEverySymbolThatSharesAnFQN for the full fixture); this test
// isolates just the two-symbol case.
func TestExportDisambiguatesSameFileCollisionsByLine(t *testing.T) {
	paths := map[int64]string{1: "internal/diffgraph/key.go"}
	symbols := []model.Symbol{
		{ID: 1, Name: "String", FQN: "internal/diffgraph/key.go:String", FileID: 1,
			Kind: model.KindMethod, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 36}}},
		{ID: 2, Name: "String", FQN: "internal/diffgraph/key.go:String", FileID: 1,
			Kind: model.KindMethod, Confidence: model.ConfidenceHigh,
			Range: model.Range{Start: model.Position{Line: 74}}},
	}

	g, _, err := buildExportGraph(stdctx.Background(), paths, symbols, nil, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	for _, want := range []string{
		"internal/diffgraph/key.go:String@internal/diffgraph/key.go#L36",
		"internal/diffgraph/key.go:String@internal/diffgraph/key.go#L74",
	} {
		if !contains(g.Keys(), want) {
			t.Errorf("missing line-disambiguated key %q; keys = %v", want, g.Keys())
		}
	}
}

// TestExportCrossFileKeysAreStableAgainstNewFiles is the stability test that
// motivates the whole fix. The old ordinal-in-path-order scheme assigned "#2",
// "#3", ... by walking symbols in path order, so a new file whose path sorted
// ahead of the existing ones renumbered every symbol that already had a key —
// which defeats the cross-run diffability that made FQN the key in the first
// place. Each symbol's key must depend only on its own base key and its own
// path, never on how many other files happen to share the collision.
func TestExportCrossFileKeysAreStableAgainstNewFiles(t *testing.T) {
	mkFixture := func(paths map[int64]string, symbols []model.Symbol) map[int64]string {
		keyed := make(map[int64]string, len(symbols))
		g, _, err := buildExportGraph(stdctx.Background(), paths, symbols, nil, model.ConfidenceLow)
		if err != nil {
			t.Fatalf("buildExportGraph: %v", err)
		}
		keys := g.Keys()
		if len(keys) != len(symbols) {
			t.Fatalf("got %d keys for %d symbols", len(keys), len(symbols))
		}
		// Every symbol here has its own file, so "<base>@<path>" identifies
		// which key belongs to which original symbol without depending on
		// node emission order.
		want := make(map[string]bool, len(keys))
		for _, k := range keys {
			want[k] = true
		}
		for _, sym := range symbols {
			k := "config:name@" + paths[sym.FileID]
			if !want[k] {
				t.Fatalf("expected key %q not present among %v", k, keys)
			}
			keyed[sym.ID] = k
		}
		return keyed
	}

	origPaths := map[int64]string{
		1: ".github/workflows/ci.yml",
		2: ".github/workflows/deploy.yml",
		3: ".github/workflows/release.yml",
	}
	origSymbols := []model.Symbol{
		{ID: 1, Name: "name", FQN: "config:name", FileID: 1, Kind: model.KindField,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}}},
		{ID: 2, Name: "name", FQN: "config:name", FileID: 2, Kind: model.KindField,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}}},
		{ID: 3, Name: "name", FQN: "config:name", FileID: 3, Kind: model.KindField,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}}},
	}
	before := mkFixture(origPaths, origSymbols)

	// Add one more file whose path sorts FIRST ("aaa" < ".github" is false
	// lexically since '.' < 'a', so pick a path that genuinely sorts ahead of
	// the existing ones under ORDER BY f.path).
	newPaths := map[int64]string{
		1: ".github/workflows/ci.yml",
		2: ".github/workflows/deploy.yml",
		3: ".github/workflows/release.yml",
		4: ".github/workflows/aaa.yml",
	}
	newSymbols := append(append([]model.Symbol{}, origSymbols...), model.Symbol{
		ID: 4, Name: "name", FQN: "config:name", FileID: 4, Kind: model.KindField,
		Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 1}},
	})
	after := mkFixture(newPaths, newSymbols)

	for _, sym := range origSymbols {
		if before[sym.ID] != after[sym.ID] {
			t.Errorf("symbol %d's key changed after an unrelated file was added: %q -> %q",
				sym.ID, before[sym.ID], after[sym.ID])
		}
	}
}

// TestExportNonCollidingKeyStaysBare pins the property that keeps most keys
// byte-identical across runs: a symbol with no colliding FQN gets no suffix
// at all, path or line.
func TestExportNonCollidingKeyStaysBare(t *testing.T) {
	paths := map[int64]string{1: "a/x.go"}
	symbols := []model.Symbol{
		{ID: 1, Name: "Walk", FQN: "a/x.go:Walk", FileID: 1, Kind: model.KindFunction,
			Confidence: model.ConfidenceHigh, Range: model.Range{Start: model.Position{Line: 5}}},
	}
	g, st, err := buildExportGraph(stdctx.Background(), paths, symbols, nil, model.ConfidenceLow)
	if err != nil {
		t.Fatalf("buildExportGraph: %v", err)
	}
	if st.disambiguated != 0 {
		t.Errorf("disambiguated = %d, want 0 for a non-colliding symbol", st.disambiguated)
	}
	if !contains(g.Keys(), "a/x.go:Walk") {
		t.Errorf("non-colliding key was rewritten; keys = %v", g.Keys())
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestExportSuccessLineCountsWhatTheArtifactHolds is I5: the success line and
// the lossy note printed three lines above it must not contradict each other.
//
// The CSV exporter writes edge rows only, so a node with no edges has nowhere
// to live and is absent from the file — which its Lossy() says plainly. The
// success line reported the GRAPH's order anyway, so one real run printed
// "wrote 9059 nodes and 16433 edges" over a file holding 4651 distinct node
// ids. Two claims about the same artifact, both on stderr, one of them false.
func TestExportSuccessLineCountsWhatTheArtifactHolds(t *testing.T) {
	// Four nodes, one edge: two endpoints reach the file, two isolated nodes
	// cannot.
	g := metrics.NewGraph(
		[]metrics.Node{
			{Key: "a/x.go:One", Kind: model.KindFunction},
			{Key: "b/y.go:Two", Kind: model.KindFunction},
			{Key: "c/z.go:Lonely", Kind: model.KindFunction},
			{Key: "d/w.go:AlsoLonely", Kind: model.KindFunction},
		},
		[]metrics.Edge{{
			From: "a/x.go:One", To: "b/y.go:Two",
			Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		}},
	)

	dir := t.TempDir()
	path := dir + "/edges.csv"
	ex, err := export.ForFormat(export.FormatCSV)
	if err != nil {
		t.Fatalf("ForFormat: %v", err)
	}
	var errOut bytes.Buffer
	if err := emitExport(stdctx.Background(), ex, g, io.Discard, &errOut, path); err != nil {
		t.Fatalf("emitExport: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse artifact: %v", err)
	}
	distinct := map[string]struct{}{}
	for _, r := range rows[1:] { // skip the header
		distinct[r[0]] = struct{}{}
		distinct[r[1]] = struct{}{}
	}

	want := fmt.Sprintf("wrote %d nodes and %d edges", len(distinct), len(rows)-1)
	if got := errOut.String(); !strings.Contains(got, want) {
		t.Fatalf("success line must report the artifact's own counts (%q); stderr was:\n%s", want, got)
	}
	if strings.Contains(errOut.String(), "wrote 4 nodes") {
		t.Error("success line reported the graph's node count, which the CSV cannot hold — " +
			"it contradicts the lossy note printed immediately above it")
	}
}

// TestExportStatesItsProvenanceOnStderr is I4: `cortex export` was the only
// new surface that shipped an artifact with no indication of what it describes.
//
// Every MCP response carries index_revision and a provenance caveat, and
// `cortex impact` refuses outright when the index was built at a commit
// outside the analysed range. An export is the artifact most likely to outlive
// the session that produced it — it gets committed, attached to a ticket,
// joined against something in a notebook — so it is the one that can least
// afford to be silent about which index it came from.
//
// stdout stays a clean machine-readable artifact, so this goes to stderr.
func TestExportStatesItsProvenanceOnStderr(t *testing.T) {
	var errOut bytes.Buffer
	reportExportProvenance(&errOut,
		exportProvenance{runID: 7, revision: "0123456789abcdef0123456789abcdef01234567"})

	got := errOut.String()
	if !strings.Contains(got, "index_revision: 7") {
		t.Errorf("provenance must name the index revision in the vocabulary the other surfaces use; got:\n%s", got)
	}
	if !strings.Contains(got, "0123456") {
		t.Errorf("provenance must name the commit the index was built at; got:\n%s", got)
	}

	// A stale index says so, in the sentence openQuery already logs, rather
	// than in a third phrasing invented here.
	errOut.Reset()
	reportExportProvenance(&errOut, exportProvenance{
		runID: 7, revision: "abc", stale: "This index was written by cortex 0.1 and you are running 0.2.",
	})
	if !strings.Contains(errOut.String(), "This index was written by cortex 0.1") {
		t.Errorf("a stale index must be disclosed on the export too; got:\n%s", errOut.String())
	}
}
