package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// TestWebDepsSeparatesUnusedFromIndirectAndSaysWhyItIsAPrompt.
//
// An indirect dependency with no importer is not a finding — the manifest
// already says it is transitive — and reporting it as unused would restate a
// fact the manifest makes. A direct one with no importer IS a finding, and it
// is a prompt rather than a verdict, which the row has to say itself.
func TestWebDepsSeparatesUnusedFromIndirectAndSaysWhyItIsAPrompt(t *testing.T) {
	t.Parallel()

	list := []registry.DependencyUsage{
		{SymbolID: 1, Name: "github.com/spf13/cobra", Ecosystem: "go", Version: "v1.8.0",
			Manifest: "go.mod", Line: 7, Importers: 12},
		{SymbolID: 2, Name: "github.com/stretchr/testify", Ecosystem: "go", Version: "v1.9.0",
			Manifest: "go.mod", Line: 8, Importers: 0},
		{SymbolID: 3, Name: "golang.org/x/sys", Ecosystem: "go", Version: "v0.18.0",
			Manifest: "go.mod", Line: 20, Importers: 0, Indirect: true},
	}
	sawMatcher := false
	_, deps, _ := mindFixture(t)
	srv := newServerWithDependencies(t, deps, stubDependencyReader{list: list, gotMatcherPtr: &sawMatcher})

	var got DepsResponse
	rec := get(t, srv, "/api/deps", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if !sawMatcher {
		t.Error("no ImportMatcher was passed to DependencyReport. With a nil matcher every " +
			"Importers is left at zero, which the registry documents as `not measured` and this " +
			"screen would publish as `nothing imports this`.")
	}

	cases := []struct {
		name        string
		wantUnused  bool
		wantCaveat  bool
		wantImports int
	}{
		{"github.com/spf13/cobra", false, false, 12},
		{"github.com/stretchr/testify", true, true, 0},
		{"golang.org/x/sys", false, false, 0},
	}
	byName := map[string]DependencyRow{}
	for _, d := range got.Declared {
		byName[d.Name] = d
	}
	unused := map[string]bool{}
	for _, d := range got.NotImported {
		unused[d.Name] = true
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := byName[tc.name]
			if !ok {
				t.Fatalf("%s missing from the inventory", tc.name)
			}
			if row.Importers != tc.wantImports {
				t.Errorf("importers = %d, want %d", row.Importers, tc.wantImports)
			}
			if !row.Measured {
				t.Error("importers_measured is false; zero would then be unreadable")
			}
			if unused[tc.name] != tc.wantUnused {
				t.Errorf("listed as declared_but_not_imported = %v, want %v", unused[tc.name], tc.wantUnused)
			}
			if (row.Caveat != "") != tc.wantCaveat {
				t.Errorf("caveat = %q, want present = %v", row.Caveat, tc.wantCaveat)
			}
			if tc.wantCaveat && !strings.Contains(row.Caveat, "prompt, not a verdict") {
				t.Errorf("caveat does not say it is a prompt: %q", row.Caveat)
			}
			if row.Evidence.FilePath == "" || row.Evidence.Line == 0 {
				t.Errorf("evidence = %+v, want the manifest file:line", row.Evidence)
			}
		})
	}

	if got.Counts.Declared != 3 || got.Counts.Direct != 2 || got.Counts.Indirect != 1 {
		t.Errorf("counts = %+v, want declared 3 / direct 2 / indirect 1", got.Counts)
	}
	if got.Counts.NotImported != 1 {
		t.Errorf("counts.declared_but_not_imported = %d, want 1 (the indirect one is excluded)",
			got.Counts.NotImported)
	}
	if !strings.Contains(got.VulnerabilityPolicy, "NO VULNERABILITY DATA") {
		t.Errorf("vulnerability_policy does not state the refusal: %q", got.VulnerabilityPolicy)
	}
	if !strings.Contains(got.Coverage, "not lock files") {
		t.Errorf("coverage_limit does not say manifests-not-lock-files: %q", got.Coverage)
	}
}

// depsIndexServer is a portal over an index holding a manifest AND the imports
// the reverse direction is a subtraction over.
//
// captest cannot build one: it seeds no file_imports rows at all, so every
// declared dependency reads as unimported and the reverse direction reads as
// empty. The rows are therefore seeded directly.
//
//	declared:  github.com/spf13/cobra   (direct)
//	           github.com/unused/pkg    (direct, imported by nothing)
//	imports:   github.com/spf13/cobra        matches the declaration
//	           github.com/spf13/cobra/doc    matches it too, by module prefix
//	           github.com/undeclared/thing   MATCHES NOTHING
//	           fmt                           matches nothing either, and is the
//	                                         standard library — which is why the
//	                                         detail is a caveat and not a verdict
//	           ./internal/local              relative: excluded, no manifest
//	                                         declares a path in this tree
func depsIndexServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(1,1,'go.mod','gomod','h1')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(2,1,'internal/app/app.go','go','h2')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(3,1,'internal/app/run.go','go','h3')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, signature,
		                     start_line, exported, confidence, source)
		   VALUES(1,1,1,'dependency','github.com/spf13/cobra','go.mod#cobra','go v1.8.0',
		          5,1,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, signature,
		                     start_line, exported, confidence, source)
		   VALUES(2,1,1,'dependency','github.com/unused/pkg','go.mod#unused','go v0.1.0',
		          6,1,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, start_line,
		                     exported, confidence, source)
		   VALUES(3,1,2,'function','Run','internal/app/app.go#Run',10,1,'high','source_code')`,
	}
	imp := func(file int, path string) string {
		return fmt.Sprintf(
			`INSERT INTO file_imports(index_run_id, file_id, path, alias, start_line)
			   VALUES(1,%d,'%s','',1)`, file, path)
	}
	stmts = append(stmts,
		imp(2, "github.com/spf13/cobra"),
		imp(3, "github.com/spf13/cobra"),
		imp(2, "github.com/spf13/cobra/doc"),
		imp(2, "github.com/undeclared/thing"),
		imp(3, "fmt"),
		imp(2, "./internal/local"))

	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed deps index: %v\n%s", err, q)
		}
	}

	reg := registry.New(db)
	return newServer(t, reg, capability.NewDeps(reg, graph.New(db)),
		config.Config{ProjectName: "deps-index"}, opts...)
}

// TestWebDepsNamesTheImportsNoDeclarationMatches is the reverse direction as an
// answer rather than as a stated gap.
//
// It is a SUBTRACTION, and the two properties that make it honest are asserted
// together: the paths a declaration matches are absent from the list, and the
// count published beside it is the count of what remained — not of every import
// in the run, which is the number a missing subtraction produces.
func TestWebDepsNamesTheImportsNoDeclarationMatches(t *testing.T) {
	t.Parallel()
	srv := depsIndexServer(t)

	var got DepsResponse
	rec := get(t, srv, "/api/deps", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	rev := got.ImportedNotDeclared
	if !rev.Available {
		t.Fatalf("the reverse direction is unavailable over an index that records imports: %q",
			rev.Detail)
	}

	byPath := map[string]int{}
	for _, r := range rev.Rows {
		byPath[r.Path] = r.Files
	}
	for _, absent := range []string{
		"github.com/spf13/cobra",     // declared outright
		"github.com/spf13/cobra/doc", // a package beneath the declared module
		"./internal/local",           // relative: no manifest declares one
	} {
		if _, listed := byPath[absent]; listed {
			t.Errorf("%q is listed as undeclared: %+v", absent, rev.Rows)
		}
	}
	if got, want := byPath["github.com/undeclared/thing"], 1; got != want {
		t.Errorf("github.com/undeclared/thing importing files = %d, want %d (rows: %+v)",
			got, want, rev.Rows)
	}
	if _, listed := byPath["fmt"]; !listed {
		t.Error("`fmt` is absent. The index records what was imported, never who provides " +
			"it, so a standard-library path cannot be filtered out on evidence — hiding it " +
			"would be a guess dressed as a finding. It belongs in the list, under the caveat.")
	}
	if len(rev.Rows) != 2 {
		t.Errorf("%d row(s), want 2: %+v", len(rev.Rows), rev.Rows)
	}
	if got.Counts.ImportedNotDeclared != 2 {
		t.Errorf("counts.imported_but_not_declared = %d, want 2", got.Counts.ImportedNotDeclared)
	}

	// The caveat travels in the payload. A count of undeclared imports read
	// without it is the most misreadable number on this screen.
	for _, phrase := range []string{"PROMPT, NOT A VERDICT", "never who provides it", "relative path(s) excluded"} {
		if !strings.Contains(rev.Detail, phrase) {
			t.Errorf("detail does not carry %q: %q", phrase, rev.Detail)
		}
	}

	// And the declared side still measures its importers off the same table.
	byName := map[string]DependencyRow{}
	for _, d := range got.Declared {
		byName[d.Name] = d
	}
	if c := byName["github.com/spf13/cobra"]; c.Importers != 3 {
		t.Errorf("cobra importers = %d, want 3 (two files import the module, one a package "+
			"beneath it)", c.Importers)
	}
	if u := byName["github.com/unused/pkg"]; u.Importers != 0 || u.Caveat == "" {
		t.Errorf("github.com/unused/pkg = %+v, want no importers and a caveat", u)
	}
}

// registryWithoutImports is a Registry that can report the declared set and
// cannot enumerate imports.
//
// It keeps the unavailable path reachable, which matters because that path is
// what any Registry implementation predating registry.DistinctImports takes,
// and because the alternative to reaching it in a test is trusting that an
// empty list would never be published as an answer.
type registryWithoutImports struct {
	Registry
	inner *registry.Registry
}

func (n registryWithoutImports) DependencyReport(ctx context.Context, runID int64,
	m registry.ImportMatcher) ([]registry.DependencyUsage, error) {
	return n.inner.DependencyReport(ctx, runID, m)
}

// TestWebDepsReverseDirectionIsStatedNotFakedWithoutAnEnumerator.
//
// Without the port, "imported but never declared" cannot be computed. An empty
// list would ASSERT that every import matches a declaration, which nothing
// checked — so the response must say it is unavailable, say why, keep the count
// at -1, and record it as an omission.
func TestWebDepsReverseDirectionIsStatedNotFakedWithoutAnEnumerator(t *testing.T) {
	t.Parallel()
	full := depsIndexServer(t)
	inner, ok := full.deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("fixture registry is %T", full.deps.Registry)
	}
	narrowed := registryWithoutImports{Registry: inner, inner: inner}
	if _, wired := any(narrowed).(ImportEnumerator); wired {
		t.Fatal("the narrowed registry still enumerates imports, so this test exercises " +
			"the answerable path and proves nothing about the stated gap")
	}
	srv := newServer(t, narrowed, full.deps.Caps, config.Config{ProjectName: "no-enumerator"})

	var got DepsResponse
	get(t, srv, "/api/deps", &got)

	rev := got.ImportedNotDeclared
	if rev.Available {
		t.Fatal("the reverse direction reports itself available; no port supplies it")
	}
	if rev.Rows == nil {
		t.Error("rows is null rather than an empty array")
	}
	if len(rev.Rows) != 0 {
		t.Errorf("rows is populated on an unavailable answer: %+v", rev.Rows)
	}
	if !strings.Contains(rev.Detail, "not published here") && !strings.Contains(rev.Detail, "not a missing fact") {
		t.Errorf("detail does not explain the gap: %q", rev.Detail)
	}
	if rev.Remedy == "" {
		t.Error("no remedy for the unavailable direction")
	}
	if got.Counts.ImportedNotDeclared != -1 {
		t.Errorf("counts.imported_but_not_declared = %d, want -1 — a 0 would be a claim",
			got.Counts.ImportedNotDeclared)
	}
	if got.Omissions.Complete {
		t.Error("omissions.complete is true while half the question went unanswered")
	}
	var stated bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "import paths") {
			stated = true
		}
	}
	if !stated {
		t.Errorf("the unanswerable direction is not recorded in the omission block: %+v", got.Omissions.Omitted)
	}
}

// TestWebDepsReportsTheLanguageTiersFromTheParserItself.
//
// The tiers are what `repocortex languages` computes, derived from the query maps
// at run time. Joining them against the repository's own language mix is what
// makes an empty dependency answer interpretable.
func TestWebDepsReportsTheLanguageTiersFromTheParserItself(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got DepsResponse
	get(t, srv, "/api/deps", &got)

	if len(got.LanguageTiers) != 3 {
		t.Fatalf("language_tiers has %d entries, want typed / resolved / parsed", len(got.LanguageTiers))
	}
	byTier := map[string]LanguageTier{}
	for _, lt := range got.LanguageTiers {
		byTier[lt.Tier] = lt
	}
	for _, tier := range []parse.Tier{parse.TierTyped, parse.TierResolved, parse.TierParsed} {
		lt, ok := byTier[tier.String()]
		if !ok {
			t.Fatalf("tier %q is missing", tier)
		}
		if lt.WhatYouGet != tierWhatYouGet(tier) {
			t.Errorf("tier %q description = %q, want %q — the parser's own description, worded; two "+
				"phrasings of one capability is how the CLI and the portal come to disagree",
				tier, lt.WhatYouGet, tierWhatYouGet(tier))
		}
		want := parse.LanguagesInTier(tier)
		if lt.Count != len(want) {
			t.Errorf("tier %q supported_count = %d, want %d", tier, lt.Count, len(want))
		}
		if lt.Present == nil {
			t.Errorf("tier %q present_in_this_repository is null rather than an array", tier)
		}
	}

	// The fixture is Go and SQL, so `go` must be present at the typed tier with
	// a real weight rather than merely being listed as supported.
	typed := byTier[parse.TierTyped.String()]
	var sawGo bool
	for _, p := range typed.Present {
		if p.Language == "go" {
			sawGo = true
			if p.Symbols == 0 {
				t.Error("go is present with zero symbols; the join against the repository is not happening")
			}
		}
	}
	if !sawGo && typed.PresentCount > 0 {
		t.Errorf("typed tier present = %+v, expected the fixture's Go files", typed.Present)
	}
}

// TestWebDepsEmptyAndUnwiredAreDifferentAnswers is the three-empties table, as
// for /api/config. The language tiers survive all three, because they are a
// property of the binary rather than of the index.
func TestWebDepsEmptyAndUnwiredAreDifferentAnswers(t *testing.T) {
	t.Parallel()

	_, deps, _ := bareFixture(t)

	cases := []struct {
		name      string
		server    func(t *testing.T) *Server
		wantTitle string
	}{
		{"no manifest found", func(t *testing.T) *Server { s, _, _ := bareFixture(t); return s },
			"no dependency manifest"},
		{"no committed index", func(t *testing.T) *Server { return unindexedServer(t) },
			"no committed index"},
		{"read port not wired", func(t *testing.T) *Server { return serverWithoutNarrowPorts(t, deps) },
			"without a dependency read port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.server(t)
			var got DepsResponse
			rec := get(t, srv, "/api/deps", &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(got.Declared) != 0 {
				t.Fatalf("expected no declared dependencies, got %+v", got.Declared)
			}
			if got.Help == nil {
				t.Fatal("an empty payload was returned with no guidance")
			}
			if !strings.Contains(got.Help.Title, tc.wantTitle) {
				t.Errorf("guidance title = %q, want it to contain %q", got.Help.Title, tc.wantTitle)
			}
			if len(got.Help.Steps) == 0 {
				t.Error("guidance names no next step")
			}
			if got.Next.Label == "" {
				t.Error("the empty screen ends in no next action")
			}
			if len(got.LanguageTiers) != 3 {
				t.Errorf("language_tiers has %d entries on an empty screen; the tiers are a property "+
					"of the binary and are true before anything is indexed", len(got.LanguageTiers))
			}
		})
	}
}

// newServerWithDependencies wires a stub DependencyReader through the
// functional option.
func newServerWithDependencies(t *testing.T, deps capability.Deps, stub stubDependencyReader) *Server {
	t.Helper()
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "deps-stub"},
		WithDependencyReader(stub))
}
