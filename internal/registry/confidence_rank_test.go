package registry

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestConfidenceRankCaseMatchesModelRank pins confidenceRankCase to
// pkg/model.Confidence.Rank().
//
// confidenceRankCase's own comment argues drift is impossible because
// EdgeFilter's WHERE floor is computed once, in Go, via
// model.Confidence.Rank() — true for the floor, but confidenceRankCase has a
// SECOND, load-bearing use: the ORDER BY in edgesFor ranks the SQL column
// directly through this CASE, with no Go-side Rank() in between. A tier added
// to pkg/model.Confidence without a matching WHEN clause here would silently
// rank 0 in ORDER BY — sorting a newly added tier as weaker than 'low' — while
// nothing about the WHERE floor would catch it, because the floor is never
// exercised for a tier nobody passes as a minimum yet.
//
// This reads both source files rather than re-encoding either side's mapping
// as a second Go literal, on the same principle basis_test.go's
// TestTokenBasisMatchesTheEstimatorInUse uses: a coupling test that hardcodes
// its own copy of the thing it is checking can drift right alongside it and
// still pass.
func TestConfidenceRankCaseMatchesModelRank(t *testing.T) {
	modelSrc, err := os.ReadFile(filepath.Join("..", "..", "pkg", "model", "model.go"))
	if err != nil {
		t.Fatalf("read model.go: %v", err)
	}
	querySrc, err := os.ReadFile(filepath.Join("query.go"))
	if err != nil {
		t.Fatalf("read query.go: %v", err)
	}

	// Confidence<Name> Confidence = "<value>" — the four (or however many)
	// tiers pkg/model declares.
	modelTierRe := regexp.MustCompile(`Confidence\w+\s+Confidence\s*=\s*"(\w+)"`)
	modelMatches := modelTierRe.FindAllStringSubmatch(string(modelSrc), -1)
	if len(modelMatches) == 0 {
		t.Fatal("found no Confidence tier constants in model.go; regex is broken or the type moved")
	}
	modelTiers := make(map[string]bool, len(modelMatches))
	for _, m := range modelMatches {
		modelTiers[m[1]] = true
	}

	// WHEN '<tier>' THEN <rank> — the tiers confidenceRankCase itself knows
	// about, read from the SQL string in query.go rather than from Go's
	// runtime representation of the constant, so a hand-edit to the CASE is
	// caught the same way a hand-edit to model.go is.
	sqlTierRe := regexp.MustCompile(`WHEN\s+'(\w+)'\s+THEN\s+(\d+)`)
	sqlMatches := sqlTierRe.FindAllStringSubmatch(string(querySrc), -1)
	if len(sqlMatches) == 0 {
		t.Fatal("found no WHEN clauses in query.go; confidenceRankCase moved or its shape changed")
	}
	sqlRanks := make(map[string]int, len(sqlMatches))
	for _, m := range sqlMatches {
		rank, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("parse rank %q for tier %q: %v", m[2], m[1], err)
		}
		sqlRanks[m[1]] = rank
	}

	for tier := range modelTiers {
		if _, ok := sqlRanks[tier]; !ok {
			t.Errorf("model.Confidence tier %q has no WHEN clause in confidenceRankCase; "+
				"ORDER BY would rank it 0 (same as unrecognised) while Rank() ranks it %d",
				tier, model.Confidence(tier).Rank())
		}
	}
	for tier := range sqlRanks {
		if !modelTiers[tier] {
			t.Errorf("confidenceRankCase has a WHEN clause for %q but pkg/model.Confidence "+
				"declares no such tier", tier)
		}
	}

	for tier, sqlRank := range sqlRanks {
		if !modelTiers[tier] {
			continue // already reported above
		}
		if want := model.Confidence(tier).Rank(); sqlRank != want {
			t.Errorf("confidenceRankCase ranks %q as %d, model.Confidence(%q).Rank() = %d; they must agree",
				tier, sqlRank, tier, want)
		}
	}
}
