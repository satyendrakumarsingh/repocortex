package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// THE INSTRUMENT MUST NOT MOVE WHEN THE GRAPH IMPROVES.
//
// `repocortex accuracy` measures the heuristic resolver. Importing a compiler
// index does not change what the resolver did, so the report must not change
// either — and before migration 0012 it changed catastrophically, in the one
// direction nobody would question. UpgradeEdges rewrites a weaker edge in place,
// so every call the resolver got RIGHT was the call the compiler confirmed, was
// rewritten to source='compiler', and left the query; every call it got WRONG
// collided with nothing and stayed. On an 8,500-file repository the same
// resolver scored 51.2% recall before an import and 0.5% after.
//
// A measurement that reads best when the graph is worst is worse than none, and
// this project has reverted nine changes on this instrument's evidence.

// promoteCorrectCalls does to the fixture exactly what `repocortex scip import`
// does to a real repository: the calls the resolver got right are confirmed by
// the compiler and upgraded IN PLACE, so their rows stop saying 'source_code'.
//
// Lines 101-105 (high, correct) and 107 (medium, correct) are the resolver's
// hits. Its misses — 106, 108, 109 — point at a different definition, so they
// collide with nothing and survive untouched. That asymmetry is the whole bug.
func promoteCorrectCalls(t *testing.T, root string) registry.UpgradeStats {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// The importer promotes a REFERENCES edge into a CALLS edge rather than
	// adding one beside it, so the fixture's compiler rows at those lines move
	// too. Mirrored here so the adjudication set is what a real import leaves.
	if _, err := db.ExecContext(ctx, `
		DELETE FROM edges
		 WHERE index_run_id = 1 AND source = 'compiler' AND kind = 'REFERENCES'
		   AND evidence_file = 'pkg/a.go'
		   AND evidence_line IN (101,102,103,104,105,107)`); err != nil {
		t.Fatal(err)
	}

	var edges []model.Edge
	for _, line := range []int{101, 102, 103, 104, 105, 107} {
		edges = append(edges, model.Edge{
			SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact,
			Evidence: model.Evidence{
				FilePath: "pkg/a.go",
				Range:    model.Range{Start: model.Position{Line: line}},
				Source:   model.SourceCompiler,
				Detail:   "scip: scip-go v0.9.9",
			},
		})
	}
	reg := registry.New(db)
	st, err := reg.UpgradeEdges(ctx, 1, edges)
	if err != nil {
		t.Fatal(err)
	}

	// A real import also writes symbol_references.resolution (migration 0013),
	// and LINE 110 IS THE ONE THAT MATTERS. It is the fixture's missed call
	// site: the resolver bound it to nothing, and the report names that as the
	// cause of the miss. The compiler has a fact there, so an import settles it
	// — and if accuracyMisses reads the live column it will then report that
	// the resolver succeeded, silently deleting the only miss the fixture has.
	//
	// The seed names every reference 'callee' while the compiler's target is
	// the symbol 'B', so the name-keyed join needs them lined up first. Then
	// the REAL marking runs, rather than a hand-written UPDATE, so this test
	// moves whenever that code does. No accuracy query reads a reference's
	// name, which is what makes the rename safe — and if one ever starts, this
	// test fails, which is the correct outcome.
	if _, err := db.ExecContext(ctx, `
		UPDATE symbol_references SET name = 'B'
		 WHERE index_run_id = 1 AND kind LIKE '%call%' AND start_line = 110`); err != nil {
		t.Fatal(err)
	}
	marked, err := reg.MarkCompilerResolutions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if marked.Bound == 0 {
		t.Fatal("setup: the compiler settled no references, so this test would not " +
			"exercise migration 0013 at all")
	}
	return st
}

// TestAccuracyIsUnchangedByAnImport is the acceptance test for migration 0012.
func TestAccuracyIsUnchangedByAnImport(t *testing.T) {
	root := accuracyFixture(t, true)

	before, err := runAccuracyCmd(t, root, "--json")
	if err != nil {
		t.Fatalf("accuracy before: %v\n%s", err, before)
	}

	st := promoteCorrectCalls(t, root)
	if st.Upgraded != 6 {
		t.Fatalf("setup: %d edge(s) upgraded, want 6 — the fixture no longer reproduces "+
			"the condition this test exists for: %+v", st.Upgraded, st)
	}

	after, err := runAccuracyCmd(t, root, "--json")
	if err != nil {
		t.Fatalf("accuracy after: %v\n%s", err, after)
	}

	if before != after {
		t.Errorf("the report moved when the graph improved; the resolver did not change.\n"+
			"BEFORE IMPORT:\n%s\nAFTER IMPORT:\n%s", before, after)
	}
}

// TestAccuracyStillSeesTheResolverAfterAnUpgrade pins the direction of the fix.
//
// Equality alone could be satisfied by a report that reads nothing — two empty
// answers are equal. This holds the numbers to being the real ones: 6 of the 10
// adjudicated call sites still answered correctly, and the high tier still
// scores 5 right out of 6.
func TestAccuracyStillSeesTheResolverAfterAnUpgrade(t *testing.T) {
	root := accuracyFixture(t, true)
	promoteCorrectCalls(t, root)

	out, err := runAccuracyCmd(t, root)
	if err != nil {
		t.Fatalf("accuracy: %v\n%s", err, out)
	}
	flat := flattenSpace(out)

	for _, want := range []struct{ what, substr string }{
		{"high precision 5/6 survives the upgrade", "83.3%"},
		{"high recall 5/10 survives the upgrade", "50.0%"},
		{"all-tiers recall 6/10 survives the upgrade", "60.0%"},
		{"the denominator is untouched", "10 adjudicated call site(s)"},
	} {
		if !strings.Contains(flat, want.substr) {
			t.Errorf("after an import the report no longer shows %s (%q):\n%s",
				want.what, want.substr, out)
		}
	}
}

// TestResolverClaimPredicatesAgree holds the two READINGS of the resolver's
// claim together.
//
// They used to be two duplicated query strings — one in this package, one in
// registry/precision.go — and the test existed because duplication is only safe
// while something executes both. The copy in this package is gone: the query
// moved to registry.AccuracyAdjudication, which reads the same two constants
// ResolutionPrecision does. The test stays anyway, because sharing a constant
// is not the same as agreeing on an answer: the two functions still count
// different relations with different joins, and if `repocortex accuracy` and
// doctor ever disagree about how many CALLS edges the resolver produced on one
// index, nobody would be able to locate why.
func TestResolverClaimPredicatesAgree(t *testing.T) {
	ctx := context.Background()
	root := accuracyFixture(t, true)

	check := func(stage string) {
		db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()

		reg := registry.New(db)
		rep, err := collectAccuracy(ctx, reg, 1)
		if err != nil {
			t.Fatal(err)
		}
		prec, err := reg.ResolutionPrecision(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Coverage.HeuristicEdges != prec.HeuristicEdges {
			t.Errorf("%s: accuracy counts %d resolver CALLS edge(s), doctor counts %d",
				stage, rep.Coverage.HeuristicEdges, prec.HeuristicEdges)
		}
		if rep.Coverage.HeuristicEdges == 0 {
			t.Errorf("%s: both report zero resolver edges, which agrees by reading nothing", stage)
		}
	}

	check("before import")
	promoteCorrectCalls(t, root)
	check("after import")
}
