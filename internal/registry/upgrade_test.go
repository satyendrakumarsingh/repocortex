package registry

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestConfidenceRankMatchesSQL holds the two halves of one comparison together.
//
// UpgradeEdges decides whether to replace an edge inside a SQL WHERE clause,
// using a CASE that duplicates model.Confidence.Rank. The duplication is
// deliberate — deciding in Go would be a read-modify-write race against
// `repocortex watch` — but a duplicated rule is a rule that can drift, and drift
// here is silent: the upgrade simply stops happening, or starts happening in
// the wrong direction, and nothing errors.
//
// So the SQL is executed and compared against the Go function, for every
// confidence the model defines.
func TestConfidenceRankMatchesSQL(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	for _, c := range []model.Confidence{
		model.ConfidenceExact, model.ConfidenceHigh,
		model.ConfidenceMedium, model.ConfidenceLow,
	} {
		var got int
		if err := db.QueryRowContext(ctx, "SELECT "+rankOfParam, string(c)).Scan(&got); err != nil {
			t.Fatalf("rank %q: %v", c, err)
		}
		if want := c.Rank(); got != want {
			t.Errorf("SQL ranks %q as %d, model.Confidence.Rank says %d; "+
				"UpgradeEdges would upgrade the wrong way round", c, got, want)
		}
	}

	// The stored-column spelling must encode the same rule as the parameter
	// spelling. They are compared textually because the only difference
	// permitted between them is the operand; anything else is drift that would
	// make the comparison in upgradeSQL asymmetric, and an asymmetric
	// comparison is how an upgrade quietly becomes a downgrade.
	if a, b := strings.Replace(rankOfParam, "CASE ?", "CASE X", 1),
		strings.Replace(rankOfStored, "CASE confidence", "CASE X", 1); a != b {
		t.Errorf("the two rank expressions disagree:\n  param  %s\n  stored %s", a, b)
	}

	// And the production statement must actually use them.
	if !strings.Contains(upgradeSQL, rankOfParam) || !strings.Contains(upgradeSQL, rankOfStored) {
		t.Error("upgradeSQL no longer contains the rank expressions this test verifies")
	}
}

// TestUpgradeEdgesRefusesUnprovenExact keeps the central invariant in front of
// the new write path.
//
// Every route into the edges table has to enforce it independently, because the
// day one of them does not is the day "exact" stops meaning "a compiler proved
// it" — and this path exists specifically to write exact edges, so it is the
// most dangerous one to get wrong.
func TestUpgradeEdgesRefusesUnprovenExact(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := upgradeFixture(t)

	bad := model.Edge{
		SrcID: ids[0], DstID: ids[1], Kind: model.EdgeReferences,
		Confidence: model.ConfidenceExact,
		Evidence: model.Evidence{
			FilePath: "a.go", Range: model.Range{Start: model.Position{Line: 1}},
			Source: model.SourceInferred, // the lie
		},
	}
	if _, err := reg.UpgradeEdges(ctx, runID, []model.Edge{bad}); err == nil {
		t.Fatal("UpgradeEdges accepted confidence=exact with source=inferred")
	}

	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("the refused edge was written anyway: %+v", edges)
	}
}

// TestUpgradeEdgesCountsAreExact checks the three outcomes are distinguished.
//
// They answer different questions — did the compiler DISCOVER relationships, or
// CONFIRM ones already found? — and a caller that cannot tell them apart has
// learned nothing from the import.
func TestUpgradeEdgesCountsAreExact(t *testing.T) {
	ctx := context.Background()
	reg, runID, ids := upgradeFixture(t)

	weak := model.Edge{
		SrcID: ids[0], DstID: ids[1], Kind: model.EdgeReferences,
		Confidence: model.ConfidenceMedium,
		Evidence: model.Evidence{
			FilePath: "a.go", Range: model.Range{Start: model.Position{Line: 7}},
			Source: model.SourceInferred, Detail: "guess",
		},
	}
	st, err := reg.UpgradeEdges(ctx, runID, []model.Edge{weak})
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 1 || st.Upgraded != 0 || st.Unchanged != 0 {
		t.Fatalf("first write: %+v, want 1 inserted", st)
	}

	strong := weak
	strong.Confidence = model.ConfidenceExact
	strong.Evidence.Source = model.SourceCompiler
	strong.Evidence.Detail = "scip: scip-go v1"
	st, err = reg.UpgradeEdges(ctx, runID, []model.Edge{strong})
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 0 || st.Upgraded != 1 || st.Unchanged != 0 {
		t.Fatalf("upgrade: %+v, want 1 upgraded", st)
	}

	st, err = reg.UpgradeEdges(ctx, runID, []model.Edge{strong})
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 0 || st.Upgraded != 0 || st.Unchanged != 1 {
		t.Fatalf("repeat: %+v, want 1 unchanged", st)
	}

	// The detail must travel with the upgrade, or a user reading `references`
	// sees exact confidence attributed to "guess".
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("%d rows for one relationship", len(edges))
	}
	if edges[0].Evidence.Detail != "scip: scip-go v1" {
		t.Errorf("upgraded edge kept the old detail %q", edges[0].Evidence.Detail)
	}
}

func upgradeFixture(t *testing.T) (*Registry, int64, []int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := New(db)
	run, err := reg.BeginRun(ctx, "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := w.AddFile(ctx, &model.File{Path: "a.go", Language: "go", ContentHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, name := range []string{"A", "B"} {
		s := model.Symbol{
			FileID: fileID, Kind: model.KindFunction, Name: name, FQN: "a.go:" + name,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, err := w.AddSymbol(ctx, &s)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
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
