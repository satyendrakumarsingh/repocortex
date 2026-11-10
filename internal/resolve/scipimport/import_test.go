package scipimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// These tests drive the whole path: build a repository index the way
// `cortex index` would, write a .scip the way an indexer would, import it,
// and read the graph back.
//
// Nothing is stubbed, because everything that can go wrong here lives in the
// seams — a path that does not reconcile, a symbol that does not join, an edge
// that collides with a heuristic one and loses. A test with a fake registry
// would exercise none of them.

const (
	svcPath = "internal/svc/service.go"
	apiPath = "internal/api/handler.go"

	symService = "scip-go gomod example.com/repo v1.0.0 `internal/svc`/Service#"
	symUpdate  = "scip-go gomod example.com/repo v1.0.0 `internal/svc`/Service#updateStatus()."
	symHandle  = "scip-go gomod example.com/repo v1.0.0 `internal/api`/Handle()."
	symExtern  = "scip-go gomod github.com/other/lib v2.0.0 `lib`/Helper()."
)

// ---------------------------------------------------------------- fixtures

func writeMessage(t testing.TB, w *os.File, field protowire.Number, m proto.Message) {
	t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var hdr []byte
	hdr = protowire.AppendTag(hdr, field, protowire.BytesType)
	hdr = protowire.AppendVarint(hdr, uint64(len(body)))
	if _, err := w.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
}

func rng(sl, sc, el, ec int32) scippb.Range {
	return scippb.Range{
		Start: scippb.Position{Line: sl, Character: sc},
		End:   scippb.Position{Line: el, Character: ec},
	}
}

func def(symbol string, name, body scippb.Range) *scippb.Occurrence {
	o := &scippb.Occurrence{Symbol: symbol, SymbolRoles: int32(scippb.SymbolRole_Definition)}
	o.SetSourceRange(name)
	o.SetEnclosingSourceRange(body)
	return o
}

func ref(symbol string, at scippb.Range) *scippb.Occurrence {
	o := &scippb.Occurrence{Symbol: symbol, SymbolRoles: int32(scippb.SymbolRole_ReadAccess)}
	o.SetSourceRange(at)
	return o
}

// scipFile writes a .scip index to a temp file and returns its path.
//
// SCIP ranges are 0-based; RepoCortex's model is 1-based. Every line below is
// written one lower than the line the repository index uses, which is the
// conversion toModelRange performs and the thing this fixture must agree with
// or the positional join silently matches nothing.
func scipFile(t *testing.T, docs ...*scippb.Document) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.scip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	writeMessage(t, f, 1, &scippb.Metadata{
		Version:     scippb.ProtocolVersion_UnspecifiedProtocolVersion,
		ProjectRoot: "file:///build/repo",
		ToolInfo:    &scippb.ToolInfo{Name: "scip-go", Version: "v0.9.9"},
	})
	for _, d := range docs {
		writeMessage(t, f, 2, d)
	}
	return path
}

// serviceDoc defines Service (line 6) and updateStatus (line 9), and calls
// updateStatus from Handle's document.
func serviceDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath:     svcPath,
		Language:         "go",
		PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		Symbols: []*scippb.SymbolInformation{
			{Symbol: symService, Kind: scippb.SymbolInformation_Struct, DisplayName: "Service"},
			{Symbol: symUpdate, Kind: scippb.SymbolInformation_Method, DisplayName: "updateStatus"},
		},
		Occurrences: []*scippb.Occurrence{
			def(symService, rng(5, 5, 5, 12), rng(5, 0, 20, 1)),
			def(symUpdate, rng(8, 18, 8, 30), rng(8, 0, 11, 1)),
		},
	}
}

// handlerDoc defines Handle (line 4) and, from inside it, references
// updateStatus (line 7) and an external package symbol.
func handlerDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath:     apiPath,
		Language:         "go",
		PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		Symbols: []*scippb.SymbolInformation{
			{Symbol: symHandle, Kind: scippb.SymbolInformation_Function, DisplayName: "Handle"},
		},
		Occurrences: []*scippb.Occurrence{
			def(symHandle, rng(3, 5, 3, 11), rng(3, 0, 10, 1)),
			ref(symUpdate, rng(6, 8, 6, 20)),
			ref(symExtern, rng(7, 8, 7, 20)),
		},
	}
}

// repoIndex builds a committed run whose symbols sit at the lines the SCIP
// fixture expects: Service at 6, updateStatus at 9, Handle at 4.
func repoIndex(t *testing.T) (*registry.Registry, int64, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "cafe")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]int64{}
	add := func(path string, syms ...model.Symbol) {
		fileID, err := w.AddFile(ctx, &model.File{Path: path, Language: "go", ContentHash: "h" + path})
		if err != nil {
			t.Fatal(err)
		}
		for i := range syms {
			syms[i].FileID = fileID
			syms[i].FQN = path + ":" + syms[i].Name // RepoCortex's scheme, not SCIP's
			syms[i].Confidence = model.ConfidenceHigh
			syms[i].Source = model.SourceSourceCode
			id, err := w.AddSymbol(ctx, &syms[i])
			if err != nil {
				t.Fatal(err)
			}
			ids[syms[i].Name] = id
		}
	}
	line := func(n int) model.Range {
		return model.Range{Start: model.Position{Line: n}, End: model.Position{Line: n + 3}}
	}

	add(svcPath,
		model.Symbol{Kind: model.KindClass, Name: "Service", Range: line(6)},
		model.Symbol{Kind: model.KindMethod, Name: "updateStatus", Range: line(9)},
	)
	add(apiPath,
		model.Symbol{Kind: model.KindFunction, Name: "Handle", Range: line(4)},
	)

	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return reg, run.ID, ids
}

func importFixture(t *testing.T) (*registry.Registry, int64, map[string]int64, ImportResult) {
	t.Helper()
	reg, runID, ids := repoIndex(t)
	path := scipFile(t, serviceDoc(), handlerDoc())

	imp := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"})
	res, err := imp.Import(context.Background(), path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return reg, runID, ids, res
}

// ---------------------------------------------------------------- tests

func TestImportWritesCompilerProvenEdges(t *testing.T) {
	reg, runID, ids, res := importFixture(t)

	if res.SymbolsMatched != 3 {
		t.Errorf("matched %d compiler definitions, want 3 (Service, updateStatus, Handle); unmatched: %v",
			res.SymbolsMatched, res.UnmatchedSymbols)
	}
	if res.Edges.Inserted == 0 {
		t.Fatalf("no edges written; the import proved nothing.\n%s", res.Summary())
	}

	edges, err := reg.Edges(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	// Every edge the import wrote must carry the provenance that justifies its
	// confidence. This is the invariant the whole product rests on, checked
	// here on rows that actually went through SQLite rather than on values in
	// memory.
	var found bool
	for _, e := range edges {
		if e.Confidence != model.ConfidenceExact {
			continue
		}
		found = true
		if e.Evidence.Source != model.SourceCompiler {
			t.Fatalf("exact edge without compiler provenance: %+v", e)
		}
		if e.Evidence.Detail == "" {
			t.Errorf("exact edge does not name the indexer that produced it: %+v", e)
		}
		if e.Evidence.FilePath == "" || e.Evidence.Range.Start.Line == 0 {
			t.Errorf("exact edge carries no evidence location: %+v", e)
		}
	}
	if !found {
		t.Fatal("no exact edges in the registry after import")
	}

	// Handle references updateStatus, so the graph must connect them.
	var connected bool
	for _, e := range edges {
		if e.SrcID == ids["Handle"] && e.DstID == ids["updateStatus"] {
			connected = true
		}
	}
	if !connected {
		t.Errorf("Handle -> updateStatus is absent; the cross-document join failed.\n%s", res.Summary())
	}
}

// TestImportUpgradesHeuristicEdgeInPlace is the test that justifies this
// feature existing at all.
//
// The edges table's UNIQUE key spans (run, src, dst, kind, evidence_file,
// evidence_line) and NOT confidence, so the compiler's version of a call the
// heuristic resolver already found COLLIDES with it. An INSERT OR IGNORE keeps
// the weaker row, and the user is told "high" about something a typechecker
// proved — the product's central claim quietly broken.
func TestImportUpgradesHeuristicEdgeInPlace(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)

	// The heuristic resolver got there first, at the same evidence location the
	// compiler will report.
	heuristic := model.Edge{
		SrcID: ids["Handle"], DstID: ids["updateStatus"],
		Kind: model.EdgeReferences, Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: apiPath,
			Range:    model.Range{Start: model.Position{Line: 7, Col: 8}},
			Source:   model.SourceSourceCode,
			Detail:   "import + receiver type",
		},
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{heuristic}); err != nil {
		t.Fatal(err)
	}

	before := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if before.Confidence != model.ConfidenceHigh {
		t.Fatalf("setup failed: heuristic edge is %q", before.Confidence)
	}

	path := scipFile(t, serviceDoc(), handlerDoc())
	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).Import(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	after := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if after.Confidence != model.ConfidenceExact {
		t.Fatalf("the heuristic edge was not upgraded: still %q.\n%s", after.Confidence, res.Summary())
	}
	if after.Evidence.Source != model.SourceCompiler {
		t.Fatalf("upgraded edge kept its old provenance: %+v", after)
	}
	if res.Edges.Upgraded == 0 {
		t.Error("the report does not mention an upgrade; a user cannot tell confirmation from discovery")
	}

	// One row, not two. A duplicate would make `cortex callers` report the
	// same call twice, once as a guess.
	if n := countEdges(t, reg, runID, ids["Handle"], ids["updateStatus"]); n != 1 {
		t.Errorf("%d rows for one relationship; the upgrade duplicated instead of replacing", n)
	}
}

// TestImportNeverDowngrades guards the other direction. A stale or weaker index
// must not be able to weaken a graph, or importing becomes a risk rather than
// an improvement.
func TestImportNeverDowngrades(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)

	exact := model.Edge{
		SrcID: ids["Handle"], DstID: ids["updateStatus"],
		Kind: model.EdgeReferences, Confidence: model.ConfidenceExact,
		Evidence: model.Evidence{
			FilePath: apiPath,
			Range:    model.Range{Start: model.Position{Line: 7}},
			Source:   model.SourceCompiler,
			Detail:   "scip: scip-go v0.9.9",
		},
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{exact}); err != nil {
		t.Fatal(err)
	}

	weaker := exact
	weaker.Confidence = model.ConfidenceLow
	weaker.Evidence.Source = model.SourceInferred
	st, err := reg.UpgradeEdges(ctx, runID, []model.Edge{weaker})
	if err != nil {
		t.Fatal(err)
	}
	if st.Upgraded != 0 {
		t.Errorf("a weaker edge reported an upgrade: %+v", st)
	}
	if st.Unchanged != 1 {
		t.Errorf("expected the weaker edge to be ignored, got %+v", st)
	}

	after := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if after.Confidence != model.ConfidenceExact {
		t.Fatalf("DOWNGRADED: exact became %q", after.Confidence)
	}
}

// TestReimportIsANoOp: re-running an import must be safe. CI re-uploads the
// same index constantly, and a second run that churned rows or double-counted
// would make the report meaningless.
func TestReimportIsANoOp(t *testing.T) {
	ctx := context.Background()
	reg, runID, _ := repoIndex(t)
	path := scipFile(t, serviceDoc(), handlerDoc())

	first, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).Import(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).Import(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	if second.Edges.Inserted != 0 || second.Edges.Upgraded != 0 {
		t.Errorf("re-import changed the graph: %+v", second.Edges)
	}
	if second.Edges.Unchanged != first.Edges.Inserted {
		t.Errorf("re-import saw %d unchanged, first import wrote %d",
			second.Edges.Unchanged, first.Edges.Inserted)
	}
}

// TestImportRefusesAnIndexThatMatchesNothing is the anti-silence test.
//
// An index built from a different commit, or with paths this repository does
// not have, produces zero edges and no error — indistinguishable from a
// repository with no relationships. That must be a loud failure.
func TestImportRefusesAnIndexThatMatchesNothing(t *testing.T) {
	ctx := context.Background()
	reg, runID, _ := repoIndex(t)

	// Same shape, entirely different paths.
	doc := serviceDoc()
	doc.RelativePath = "somewhere/else/entirely.go"
	path := scipFile(t, doc)

	_, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).Import(ctx, path)
	if err == nil {
		t.Fatal("an index matching nothing was accepted silently")
	}
	for _, want := range []string{"different commit", "matched"} {
		if !contains(err.Error(), want) {
			t.Errorf("error should explain the likely cause; got %q", err)
		}
	}
}

// TestImportReportsUnmatchedDefinitions covers the partial case: the paths line
// up, but a definition sits at a line RepoCortex did not index. The user must be
// able to see which, or a half-attached graph looks like a complete one.
func TestImportReportsUnmatchedDefinitions(t *testing.T) {
	ctx := context.Background()
	reg, runID, _ := repoIndex(t)

	doc := serviceDoc()
	doc.Symbols = append(doc.Symbols, &scippb.SymbolInformation{
		Symbol:      "scip-go gomod example.com/repo v1.0.0 `internal/svc`/Ghost#",
		Kind:        scippb.SymbolInformation_Struct,
		DisplayName: "Ghost",
	})
	doc.Occurrences = append(doc.Occurrences,
		def("scip-go gomod example.com/repo v1.0.0 `internal/svc`/Ghost#", rng(40, 5, 40, 10), rng(40, 0, 45, 1)))

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, doc, handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}

	if res.SymbolsUnmatched == 0 {
		t.Fatal("a definition with no indexed counterpart was not reported")
	}
	var named bool
	for _, s := range res.UnmatchedSymbols {
		if contains(s, "Ghost") {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmatched sample does not name Ghost: %v", res.UnmatchedSymbols)
	}
	if !contains(res.Summary(), "no counterpart") {
		t.Errorf("the summary hides unmatched definitions:\n%s", res.Summary())
	}
}

// TestImportReportsReferencesOutsideTheRepository: a call into a third-party
// package is unresolvable and normal. It must be counted, not silently dropped,
// or "SCIP produced fewer edges than expected" is undiagnosable.
func TestImportReportsReferencesOutsideTheRepository(t *testing.T) {
	_, _, _, res := importFixture(t)
	if res.RelationsUnresolved == 0 {
		t.Errorf("the reference into github.com/other/lib was not reported as unresolved:\n%s",
			res.Summary())
	}
}

func TestImportRequiresAnIndexedRepository(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewImporter(reg, run.ID, scip.Options{}).
		Import(ctx, scipFile(t, serviceDoc()))
	if err == nil {
		t.Fatal("importing into an empty run succeeded")
	}
	if !contains(err.Error(), "cortex index") {
		t.Errorf("error should point at the fix; got %q", err)
	}
}

// ---------------------------------------------------- the CALLS join

// recordCall stages a parser-recorded call at path:line on a committed run,
// which is what `cortex index` writes and `cortex resolve` reads. It is the
// half of the CALLS join that does NOT come from the compiler, and every test
// below turns on whether it is present.
func recordCall(t *testing.T, reg *registry.Registry, runID int64, path, name, receiver string, line int) {
	t.Helper()
	ctx := context.Background()
	files, err := reg.Files(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path != path {
			continue
		}
		if err := reg.ReplaceFileReferences(ctx, runID, f.ID, []registry.Reference{{
			Name: name, Kind: "reference.call", Receiver: receiver, Line: line,
			ArgCount: model.ArityUnknown,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no indexed file %q", path)
}

// TestImportProducesExactCallsWhereBothSourcesAgree is the feature, end to end.
//
// handlerDoc references updateStatus at model line 7. On its own that is a
// REFERENCES edge and nothing more — SCIP does not say it is a call. With the
// parser's own record of a call expression at that same line, the two
// independently sourced halves join into a CALLS edge at confidence=exact,
// which is what `cortex callers`, impact and compile actually traverse.
func TestImportProducesExactCallsWhereBothSourcesAgree(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)
	recordCall(t, reg, runID, apiPath, "updateStatus", "", 7)

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Promote.NameMismatch != 0 {
		t.Errorf("the name check rejected the join: %+v", res.Promote)
	}
	if res.ParsedCallSites != 1 {
		t.Fatalf("the importer read %d parsed call site(s), want 1", res.ParsedCallSites)
	}
	if res.Promote.Promoted != 1 {
		t.Fatalf("promoted %d edge(s), want 1: %+v\n%s", res.Promote.Promoted, res.Promote, res.Summary())
	}

	got := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if got.Kind != model.EdgeCalls {
		t.Errorf("Handle -> updateStatus is %s, want CALLS", got.Kind)
	}
	if got.Confidence != model.ConfidenceExact || got.Evidence.Source != model.SourceCompiler {
		t.Errorf("call edge is %q/%q, want exact/compiler: %+v",
			got.Confidence, got.Evidence.Source, got)
	}
	if !contains(res.Summary(), "CALLS at 'exact'") {
		t.Errorf("the summary does not report the join:\n%s", res.Summary())
	}
}

// TestImportLeavesNonCallReferencesAsReferences is the same import with the
// parser fact moved one line away. Nothing else changes, so any CALLS edge that
// appears would have been derived from the index alone — the inference the
// confidence system exists to prevent.
func TestImportLeavesNonCallReferencesAsReferences(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)
	recordCall(t, reg, runID, apiPath, "somethingElse", "", 99)

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Promote.Promoted != 0 {
		t.Fatalf("promoted %d edge(s) with no call parsed at the reference's line: %+v",
			res.Promote.Promoted, res.Promote)
	}
	got := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if got.Kind == model.EdgeCalls {
		t.Fatal("a CALLS edge was produced from the compiler index alone")
	}
	if got.Kind != model.EdgeReferences {
		t.Errorf("Handle -> updateStatus is %s, want REFERENCES", got.Kind)
	}
}

// TestImportUpgradesHeuristicCallInPlace is requirement three: an existing
// high-confidence CALLS edge at the same (src, dst, line) must be UPGRADED, not
// duplicated. A duplicate would make `cortex callers` show the same call
// twice, once as a guess — and would double every count built on top of it.
func TestImportUpgradesHeuristicCallInPlace(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)
	recordCall(t, reg, runID, apiPath, "updateStatus", "", 7)

	heuristic := model.Edge{
		SrcID: ids["Handle"], DstID: ids["updateStatus"],
		Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: apiPath,
			Range:    model.Range{Start: model.Position{Line: 7, Col: 8}},
			Source:   model.SourceSourceCode,
			Detail:   "import + receiver type",
		},
	}
	if _, err := reg.AddResolvedEdges(ctx, runID, []model.Edge{heuristic}); err != nil {
		t.Fatal(err)
	}

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Edges.Upgraded == 0 {
		t.Errorf("no upgrade reported; the compiler confirmed a guess and nobody was told:\n%s",
			res.Summary())
	}

	after := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
	if after.Kind != model.EdgeCalls || after.Confidence != model.ConfidenceExact {
		t.Fatalf("the heuristic call was not upgraded in place: %s/%s", after.Kind, after.Confidence)
	}
	if n := countEdges(t, reg, runID, ids["Handle"], ids["updateStatus"]); n != 1 {
		t.Errorf("%d rows for one call; the join duplicated instead of upgrading", n)
	}
}

// TestImportCallsAreDeterministic. The promotion must be a function of the two
// inputs alone. Repeated imports into fresh registries must agree exactly —
// including on the counts, since a report that drifts is a report nobody can
// gate on.
func TestImportCallsAreDeterministic(t *testing.T) {
	ctx := context.Background()
	var want string
	for i := range 8 {
		reg, runID, ids := repoIndex(t)
		recordCall(t, reg, runID, apiPath, "updateStatus", "", 7)
		res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
			Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
		if err != nil {
			t.Fatal(err)
		}
		e := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"])
		got := fmt.Sprintf("%+v|%s|%s|%s", res.Promote, e.Kind, e.Confidence, e.Evidence.Source)
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("run %d produced %q, run 0 produced %q", i, got, want)
		}
	}
}

// ---------------------------------------------------------------- helpers

func edgeAt(t *testing.T, reg *registry.Registry, runID, src, dst int64) model.Edge {
	t.Helper()
	edges, err := reg.Edges(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.SrcID == src && e.DstID == dst {
			return e
		}
	}
	t.Fatalf("no edge %d -> %d", src, dst)
	return model.Edge{}
}

func countEdges(t *testing.T, reg *registry.Registry, runID, src, dst int64) int {
	t.Helper()
	edges, err := reg.Edges(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range edges {
		if e.SrcID == src && e.DstID == dst {
			n++
		}
	}
	return n
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// TestImportDeclinesACallWhoseNameDisagrees: the parser saw a call on line 7,
// but it was a call to something else — a library function RepoCortex never
// indexed — and the compiler's only in-repository target on that line is
// updateStatus appearing in some other role. Promoting would attribute a call
// to a symbol that was merely nearby. On a real repository this pattern
// accounted for 11,459 edges, and reading a sample of them found almost none
// that were calls at all.
func TestImportDeclinesACallWhoseNameDisagrees(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)
	recordCall(t, reg, runID, apiPath, "relationship", "", 7)

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Promote.Promoted != 0 {
		t.Fatalf("promoted %d edge(s) on a name mismatch: %+v", res.Promote.Promoted, res.Promote)
	}
	if res.Promote.NameMismatch != 1 {
		t.Errorf("the decline was not reported: %+v", res.Promote)
	}
	if got := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"]); got.Kind != model.EdgeReferences {
		t.Errorf("Handle -> updateStatus is %s, want REFERENCES", got.Kind)
	}
}

// TestImportDeclinesAReceiver end-to-end: the parser recorded `result` called
// ON `updateStatus` at line 7, so `updateStatus` is what the call was written
// against, not what was called.
func TestImportDeclinesAReceiver(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := repoIndex(t)
	recordCall(t, reg, runID, apiPath, "updateStatus", "", 7)
	recordCall(t, reg, runID, apiPath, "result", "updateStatus", 7)

	res, err := NewImporter(reg, runID, scip.Options{RepoRoot: "/build/repo"}).
		Import(ctx, scipFile(t, serviceDoc(), handlerDoc()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Promote.Promoted != 0 || res.Promote.Receivers != 1 {
		t.Fatalf("a receiver was promoted: %+v", res.Promote)
	}
	if got := edgeAt(t, reg, runID, ids["Handle"], ids["updateStatus"]); got.Kind != model.EdgeReferences {
		t.Errorf("Handle -> updateStatus is %s, want REFERENCES", got.Kind)
	}
}
