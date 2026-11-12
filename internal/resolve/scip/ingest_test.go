package scip

import (
	"bytes"
	"context"
	"strings"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A small two-file "repository", indexed the way scip-go would index it:
// a struct with two methods, a cross-file call, an import, and a local
// variable.
var (
	symService      = goSymbol("internal/svc/Service#")
	symUpdateStatus = goSymbol("internal/svc/Service#updateStatus().")
	symRun          = goSymbol("internal/svc/Service#Run().")
	symHandler      = goSymbol("internal/api/Handler#")
	symServe        = goSymbol("internal/api/Handler#Serve().")
	symFmtPrintln   = "scip-go gomod fmt v1.0.0 fmt/Println()."
)

func serviceDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath:     "internal/svc/service.go",
		Language:         "go",
		PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		Symbols: []*scippb.SymbolInformation{
			{Symbol: symService, Kind: scippb.SymbolInformation_Struct, DisplayName: "Service",
				Documentation: []string{"Service coordinates status updates."}},
			{Symbol: symUpdateStatus, Kind: scippb.SymbolInformation_Method, DisplayName: "updateStatus",
				SignatureDocumentation: &scippb.Signature{Language: "go", Text: "func (s *Service) updateStatus(id string) error"}},
			{Symbol: symRun, Kind: scippb.SymbolInformation_Method, DisplayName: "Run"},
		},
		Occurrences: []*scippb.Occurrence{
			// An import at the top of the file, outside every definition body.
			ref(symFmtPrintln, rng(1, 1, 1, 6), scippb.SymbolRole_Import),
			def(symService, rng(5, 5, 5, 12), rng(5, 0, 20, 1)),
			def(symUpdateStatus, rng(8, 18, 8, 30), rng(8, 0, 11, 1)),
			// Inside updateStatus's body.
			ref(symFmtPrintln, rng(9, 4, 9, 15), scippb.SymbolRole_ReadAccess),
			def(symRun, rng(13, 18, 13, 21), rng(13, 0, 18, 1)),
			// Inside Run's body, and also inside Service's — innermost wins.
			ref(symUpdateStatus, rng(15, 8, 15, 20), scippb.SymbolRole_ReadAccess),
		},
	}
}

func handlerDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath:     "internal/api/handler.go",
		Language:         "go",
		PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
		Symbols: []*scippb.SymbolInformation{
			{Symbol: symHandler, Kind: scippb.SymbolInformation_Struct, DisplayName: "Handler",
				Relationships: []*scippb.Relationship{
					{Symbol: goSymbol("net/http/Handler#"), IsImplementation: true},
				}},
			{Symbol: symServe, Kind: scippb.SymbolInformation_Method, DisplayName: "Serve"},
			{Symbol: "local 0", Kind: scippb.SymbolInformation_Variable, DisplayName: "svc"},
		},
		Occurrences: []*scippb.Occurrence{
			def(symHandler, rng(3, 5, 3, 12), rng(3, 0, 9, 1)),
			def(symServe, rng(4, 18, 4, 23), rng(4, 0, 8, 1)),
			def("local 0", rng(5, 1, 5, 4), rng(5, 0, 5, 30)),
			ref("local 0", rng(6, 1, 6, 4), scippb.SymbolRole_ReadAccess),
			// Cross-file: this is the edge tree-sitter cannot produce.
			ref(symUpdateStatus, rng(6, 5, 6, 17), scippb.SymbolRole_ReadAccess),
		},
	}
}

func ghostDoc() *scippb.Document {
	return &scippb.Document{
		RelativePath: "generated/ghost.go",
		Language:     "go",
		Occurrences:  []*scippb.Occurrence{def(goSymbol("generated/Ghost#"), rng(0, 0, 0, 5), rng(0, 0, 3, 0))},
	}
}

func knownPaths() []string {
	return []string{"internal/svc/service.go", "internal/api/handler.go", "cmd/repocortex/main.go"}
}

func ingestFixture(t *testing.T, opts Options, docs ...*scippb.Document) ([]FileFacts, Stats) {
	t.Helper()
	data := indexBytes(t, metadata("file:///build/repo"), docs...)
	facts, st, err := Collect(context.Background(), bytes.NewReader(data), opts)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return facts, st
}

func factsFor(t *testing.T, facts []FileFacts, path string) *FileFacts {
	t.Helper()
	for i := range facts {
		if facts[i].Path == path {
			return &facts[i]
		}
	}
	t.Fatalf("no facts emitted for %q (got %d document(s))", path, len(facts))
	return nil
}

func symbolByFQN(t *testing.T, f *FileFacts, fqn string) model.Symbol {
	t.Helper()
	for _, s := range f.Symbols {
		if s.FQN == fqn {
			return s
		}
	}
	t.Fatalf("%s: no symbol with FQN %q", f.Path, fqn)
	return model.Symbol{}
}

func TestRoundTripSymbolsAndRelations(t *testing.T) {
	facts, st := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		serviceDoc(), handlerDoc())

	if st.Tool.Name != "scip-go" || st.Tool.Version != "v0.9.9" {
		t.Fatalf("tool provenance lost: %+v", st.Tool)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 documents, got %d", len(facts))
	}

	svc := factsFor(t, facts, "internal/svc/service.go")

	// The file anchor is placeholder -1 and is always present.
	anchor := symbolByFQN(t, svc, "internal/svc/service.go")
	if anchor.ID != -1 || anchor.Kind != model.KindFile || anchor.Name != "service.go" {
		t.Fatalf("bad file anchor: %+v", anchor)
	}

	// SCIP symbol strings are preserved verbatim as FQNs. This is the property
	// that lets exact and heuristic facts converge on one node.
	svcSym := symbolByFQN(t, svc, symService)
	if svcSym.Kind != model.KindClass || svcSym.Name != "Service" {
		t.Fatalf("Service symbol: %+v", svcSym)
	}
	if svcSym.Doc != "Service coordinates status updates." {
		t.Fatalf("documentation dropped: %q", svcSym.Doc)
	}
	if svcSym.Confidence != model.ConfidenceExact || svcSym.Source != model.SourceCompiler {
		t.Fatalf("symbol provenance: %v/%v", svcSym.Confidence, svcSym.Source)
	}

	upd := symbolByFQN(t, svc, symUpdateStatus)
	if upd.Kind != model.KindMethod {
		t.Fatalf("updateStatus kind = %v, want method", upd.Kind)
	}
	if !strings.Contains(upd.Signature, "updateStatus(id string) error") {
		t.Fatalf("signature dropped: %q", upd.Signature)
	}
	// SCIP lines are 0-indexed; model lines are 1-indexed.
	if upd.Range.Start.Line != 9 || upd.Range.Start.Col != 18 {
		t.Fatalf("range not converted: %+v", upd.Range)
	}

	// Placeholder ids are negative, distinct, and reference-able by ReplaceFile.
	seen := map[int64]bool{}
	for _, s := range svc.Symbols {
		if s.ID >= 0 {
			t.Fatalf("symbol %q has non-placeholder id %d", s.FQN, s.ID)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate placeholder id %d", s.ID)
		}
		seen[s.ID] = true
	}

	// The reference inside Run's body is attributed to Run, not to Service and
	// not to the file, because Run's enclosing range is the innermost one that
	// contains it.
	var found bool
	for _, r := range svc.Relations {
		if r.DstFQN == symUpdateStatus && r.Kind == model.EdgeReferences {
			found = true
			if r.SrcFQN != symRun {
				t.Fatalf("reference attributed to %q, want %q", r.SrcFQN, symRun)
			}
			if r.Evidence.FilePath != "internal/svc/service.go" || r.Evidence.Range.Start.Line != 16 {
				t.Fatalf("evidence: %+v", r.Evidence)
			}
			if !strings.Contains(r.Evidence.Detail, "scip-go v0.9.9") {
				t.Fatalf("detail does not name the indexer: %q", r.Evidence.Detail)
			}
		}
	}
	if !found {
		t.Fatal("no REFERENCES relation to updateStatus")
	}
}

// TestEveryEdgeIsExactAndCompiler is the system's central promise: SCIP is the
// only source of exact edges, and an exact edge must carry compiler evidence.
func TestEveryEdgeIsExactAndCompiler(t *testing.T) {
	facts, _ := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		serviceDoc(), handlerDoc())

	// A repository-wide symbol table, as the caller would build it.
	table := map[string]int64{}
	var nextID int64 = 1000
	for i := range facts {
		for _, s := range facts[i].Symbols {
			table[s.FQN] = nextID
			nextID++
		}
	}
	lookup := func(fqn string) (int64, bool) { id, ok := table[fqn]; return id, ok }

	var total int
	for i := range facts {
		edges, unresolved, err := facts[i].Edges(lookup)
		if err != nil {
			t.Fatalf("%s: Edges: %v", facts[i].Path, err)
		}
		for _, e := range edges {
			total++
			if e.Confidence != model.ConfidenceExact {
				t.Fatalf("%s: edge %s has confidence %q", facts[i].Path, e.Kind, e.Confidence)
			}
			if e.Evidence.Source != model.SourceCompiler {
				t.Fatalf("%s: exact edge %s has source %q", facts[i].Path, e.Kind, e.Evidence.Source)
			}
			if err := e.Validate(); err != nil {
				t.Fatalf("%s: edge rejected by model.Edge.Validate: %v", facts[i].Path, err)
			}
			if e.Evidence.FilePath == "" || e.Evidence.Detail == "" {
				t.Fatalf("%s: edge %s has no evidence: %+v", facts[i].Path, e.Kind, e.Evidence)
			}
		}
		for _, r := range unresolved {
			// Unresolved destinations must be reported, not silently dropped.
			if r.DstFQN == "" {
				t.Fatalf("%s: unresolved relation with empty destination", facts[i].Path)
			}
		}
	}
	if total == 0 {
		t.Fatal("no edges produced; the assertion above proved nothing")
	}
}

// TestExactEdgeCheckCanFail injects the defect the previous test guards
// against, so a passing suite is evidence that the gate works rather than
// evidence that it was never exercised. See CONTRIBUTING, "Testing
// expectations".
func TestExactEdgeCheckCanFail(t *testing.T) {
	bad := model.Edge{
		SrcID: 1, DstID: 2, Kind: model.EdgeReferences,
		Confidence: model.ConfidenceExact,
		Evidence:   model.Evidence{Source: model.SourceInferred},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("model.Edge.Validate accepted confidence=exact with source=inferred")
	}

	// Relation.Edge cannot produce that edge even if Evidence.Source is set to
	// something else on the way in: it stamps compiler itself.
	r := Relation{
		SrcFQN: "a", DstFQN: "b", Kind: model.EdgeReferences,
		Evidence: model.Evidence{FilePath: "x.go", Source: model.SourceInferred},
	}
	e, err := r.Edge(1, 2)
	if err != nil {
		t.Fatalf("Relation.Edge: %v", err)
	}
	if e.Evidence.Source != model.SourceCompiler {
		t.Fatalf("Relation.Edge did not stamp compiler source: %q", e.Evidence.Source)
	}
}

func TestRelationEdgeRejectsMissingEndpoints(t *testing.T) {
	r := Relation{SrcFQN: "a", DstFQN: "b", Kind: model.EdgeReferences}
	if _, err := r.Edge(0, 5); err == nil {
		t.Fatal("Edge accepted a zero src id")
	}
	if _, err := r.Edge(5, 0); err == nil {
		t.Fatal("Edge accepted a zero dst id")
	}
}

func TestDefinitionAndReferenceRoles(t *testing.T) {
	facts, st := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		serviceDoc())
	svc := factsFor(t, facts, "internal/svc/service.go")

	// Three definitions plus the file anchor; nothing else becomes a symbol.
	if len(svc.Symbols) != 4 {
		t.Fatalf("want 4 symbols (anchor + 3 definitions), got %d", len(svc.Symbols))
	}
	if st.Definitions != 3 || st.References != 3 {
		t.Fatalf("roles miscounted: %d definitions, %d references", st.Definitions, st.References)
	}

	// A definition occurrence must not also become a relation pointing at
	// itself; that would make every symbol appear to reference itself.
	for _, r := range svc.Relations {
		if r.SrcFQN == r.DstFQN {
			t.Fatalf("definition emitted as a self-reference: %+v", r)
		}
	}
	if len(svc.Relations) != 3 {
		t.Fatalf("want 3 relations, got %d: %+v", len(svc.Relations), svc.Relations)
	}

	// The import sits outside every definition body, so it anchors to the file
	// and is typed IMPORTS rather than REFERENCES.
	var imports int
	for _, r := range svc.Relations {
		if r.Kind != model.EdgeImports {
			continue
		}
		imports++
		if r.SrcFQN != "internal/svc/service.go" {
			t.Fatalf("import should anchor to the file, got src %q", r.SrcFQN)
		}
		if r.DstFQN != symFmtPrintln {
			t.Fatalf("import destination %q", r.DstFQN)
		}
	}
	if imports != 1 {
		t.Fatalf("want 1 IMPORTS relation, got %d", imports)
	}
	if st.FileAnchored != 1 {
		t.Fatalf("FileAnchored = %d, want 1", st.FileAnchored)
	}

	// SCIP records read/write access but this package folds both into
	// REFERENCES; the roles are still counted so the information is not lost.
	if st.ReadAccesses == 0 {
		t.Fatal("read-access role not counted")
	}
	for _, r := range svc.Relations {
		if r.Kind == model.EdgeReads || r.Kind == model.EdgeWrites || r.Kind == model.EdgeCalls {
			t.Fatalf("SCIP cannot prove %s; relation %+v must not exist", r.Kind, r)
		}
	}
}

func TestLocalSymbolsAreQualifiedByDocument(t *testing.T) {
	other := handlerDoc()
	other.RelativePath = "cmd/repocortex/main.go"

	facts, _ := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		handlerDoc(), other)

	a := factsFor(t, facts, "internal/api/handler.go")
	b := factsFor(t, facts, "cmd/repocortex/main.go")

	wantA := "internal/api/handler.go#local 0"
	wantB := "cmd/repocortex/main.go#local 0"
	symbolByFQN(t, a, wantA)
	symbolByFQN(t, b, wantB)

	// Two documents each defining "local 0" must not collapse into one node.
	for _, s := range a.Symbols {
		if s.FQN == "local 0" {
			t.Fatal("unqualified local symbol would collide across documents")
		}
	}
	// The local reference must resolve inside its own file and nowhere else.
	edges, unresolved, err := a.Edges(nil)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	var local int
	for _, e := range edges {
		_ = e
		local++
	}
	if local == 0 {
		t.Fatal("local reference did not resolve within its own document")
	}
	for _, r := range unresolved {
		if r.DstFQN == wantB {
			t.Fatal("local symbol leaked across documents")
		}
	}
}

func TestImplementsRelationFromSymbolRelationships(t *testing.T) {
	facts, st := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		handlerDoc())
	api := factsFor(t, facts, "internal/api/handler.go")

	var found bool
	for _, r := range api.Relations {
		if r.Kind == model.EdgeImplements {
			found = true
			if r.SrcFQN != symHandler || r.DstFQN != goSymbol("net/http/Handler#") {
				t.Fatalf("IMPLEMENTS relation: %+v", r)
			}
		}
	}
	if !found || st.ImplementsRelations != 1 {
		t.Fatalf("IMPLEMENTS not derived (found=%v, count=%d)", found, st.ImplementsRelations)
	}
}

func TestCrossFileRelationResolvesThroughSymbolTable(t *testing.T) {
	facts, _ := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		serviceDoc(), handlerDoc())

	svc := factsFor(t, facts, "internal/svc/service.go")
	api := factsFor(t, facts, "internal/api/handler.go")

	// The service's symbols were written first and have real rowids now.
	written := map[string]int64{}
	var id int64 = 500
	for _, s := range svc.Symbols {
		written[s.FQN] = id
		id++
	}

	edges, unresolved, err := api.Edges(func(fqn string) (int64, bool) {
		v, ok := written[fqn]
		return v, ok
	})
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}

	var crossFile bool
	for _, e := range edges {
		if e.DstID == written[symUpdateStatus] {
			crossFile = true
			if e.Confidence != model.ConfidenceExact {
				t.Fatalf("cross-file edge is not exact: %v", e.Confidence)
			}
		}
	}
	if !crossFile {
		t.Fatal("cross-file reference to Service.updateStatus did not resolve")
	}

	// net/http/Handler# is outside the index, so its IMPLEMENTS relation is
	// reported unresolved rather than dropped.
	var reportedExternal bool
	for _, r := range unresolved {
		if r.DstFQN == goSymbol("net/http/Handler#") {
			reportedExternal = true
		}
	}
	if !reportedExternal {
		t.Fatal("unresolvable destination was dropped instead of reported")
	}
}

func TestSupersedesNeverDowngradesCompilerEvidence(t *testing.T) {
	for _, weaker := range []model.Confidence{
		model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow, model.Confidence("garbage"),
	} {
		if !Supersedes(weaker) {
			t.Fatalf("SCIP should supersede %q", weaker)
		}
	}
	if Supersedes(model.ConfidenceExact) {
		t.Fatal("an exact edge must never be replaced by another claim")
	}
}

func TestKeyIsStableAcrossResolvers(t *testing.T) {
	a := Relation{SrcFQN: "x", DstFQN: "y", Kind: model.EdgeReferences,
		Evidence: model.Evidence{FilePath: "a.go"}}
	b := Relation{SrcFQN: "x", DstFQN: "y", Kind: model.EdgeReferences,
		Evidence: model.Evidence{FilePath: "b.go"}}
	if a.Key() != b.Key() {
		t.Fatal("Key must ignore evidence so the same relationship dedupes")
	}
	c := Relation{SrcFQN: "x", DstFQN: "y", Kind: model.EdgeImports}
	if a.Key() == c.Key() {
		t.Fatal("Key must distinguish edge kinds")
	}
}

func TestImplementsEvidencePointsAtTheDeclaration(t *testing.T) {
	facts, _ := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		handlerDoc())
	api := factsFor(t, facts, "internal/api/handler.go")

	for _, r := range api.Relations {
		if r.Kind != model.EdgeImplements {
			continue
		}
		// Handler is declared on SCIP line 3, which is model line 4.
		if r.Evidence.Range.Start.Line != 4 || r.Evidence.Range.Start.Col != 5 {
			t.Fatalf("IMPLEMENTS evidence points at %+v, want the declaring identifier", r.Evidence.Range)
		}
		return
	}
	t.Fatal("no IMPLEMENTS relation")
}

// TestIndexerWithoutEnclosingRanges covers the indexers that never emit
// enclosing_range. Every reference then anchors to the file symbol, which is
// weaker attribution but still a fact the compiler established, and no
// enclosing function is invented to fill the gap.
func TestIndexerWithoutEnclosingRanges(t *testing.T) {
	owner := goSymbol("internal/svc/Service#")
	target := goSymbol("internal/svc/Service#updateStatus().")

	noBody := func(symbol string, at scippb.Range) *scippb.Occurrence {
		o := &scippb.Occurrence{Symbol: symbol, SymbolRoles: int32(scippb.SymbolRole_Definition)}
		o.SetSourceRange(at)
		return o
	}
	doc := &scippb.Document{
		RelativePath: "internal/svc/service.go",
		Language:     "go",
		Occurrences: []*scippb.Occurrence{
			noBody(owner, rng(2, 5, 2, 12)),
			noBody(target, rng(6, 18, 6, 30)),
			ref(target, rng(9, 4, 9, 16), scippb.SymbolRole_ReadAccess),
		},
	}

	facts, st := ingestFixture(t, Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}, doc)
	svc := factsFor(t, facts, "internal/svc/service.go")

	if len(svc.Relations) != 1 {
		t.Fatalf("want 1 relation, got %d", len(svc.Relations))
	}
	if svc.Relations[0].SrcFQN != "internal/svc/service.go" {
		t.Fatalf("reference should anchor to the file, got %q", svc.Relations[0].SrcFQN)
	}
	if st.FileAnchored != 1 {
		t.Fatalf("FileAnchored = %d, want 1", st.FileAnchored)
	}
	// The anchored edge is still exact: the compiler resolved the target.
	edges, _, err := svc.Edges(nil)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(edges) != 1 || edges[0].Confidence != model.ConfidenceExact {
		t.Fatalf("anchored edge: %+v", edges)
	}
}

// TestIngesterIsReusable backs the claim that an Ingester holds no per-index
// state; a leak would show up as the second index inheriting the first's
// project-root prefix.
func TestIngesterIsReusable(t *testing.T) {
	ing := New(Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()})

	first := indexBytes(t, metadata("file:///build/repo"), serviceDoc())
	second := indexBytes(t, metadata("file:///build/repo/services/nested"), handlerDoc())

	var a []string
	if _, err := ing.Ingest(context.Background(), bytes.NewReader(first),
		func(_ context.Context, f *FileFacts) error { a = append(a, f.Path); return nil }); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	var b []string
	st, err := ing.Ingest(context.Background(), bytes.NewReader(second),
		func(_ context.Context, f *FileFacts) error { b = append(b, f.Path); return nil })
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if len(a) != 1 || a[0] != "internal/svc/service.go" {
		t.Fatalf("first ingest emitted %v", a)
	}
	// The second index's project_root points at a directory that does not
	// contain the document, so it falls back rather than reusing anything.
	if len(b) != 1 || b[0] != "internal/api/handler.go" {
		t.Fatalf("second ingest emitted %v (stats %s)", b, st.Summary())
	}
}
