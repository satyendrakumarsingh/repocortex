package context

import (
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
)

// TestCompoundFormsEnumeratesPairsAndTheWholeName pins the exact set, because
// two callers depend on it agreeing.
//
// nameSignal uses it to decide whether a name matches a term, and indexTokens
// uses it to decide which names to offer as candidates. If they enumerate
// different sets the index silently withholds symbols the scorer would have
// scored, which is a recall loss with no symptom.
func TestCompoundFormsEnumeratesPairsAndTheWholeName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []string
		want  []string
	}{
		{"a single word compounds with nothing", []string{"handler"}, nil},
		{"nothing to compound", nil, nil},
		{"two words yield the pair only", []string{"data", "source"}, []string{"datasource"}},
		{
			"three words yield both pairs and the whole",
			[]string{"data", "source", "health"},
			[]string{"datasource", "sourcehealth", "datasourcehealth"},
		},
		{
			"four words: adjacent pairs and the whole, not every run",
			[]string{"data", "source", "health", "indicator"},
			[]string{"datasource", "sourcehealth", "healthindicator", "datasourcehealthindicator"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compoundForms(tc.parts); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("compoundForms(%v) = %v, want %v", tc.parts, got, tc.want)
			}
		})
	}
}

// TestCompoundTermMatchesASplitIdentifier is the defect this exists for.
//
// SplitIdentifier turns DataSourceHealthIndicator into
// [data source health indicator], and matchesTerm compares whole tokens, so a
// task saying "datasource" matched nothing: not "data", not "source", and not
// the full name either, since equality against the whole identifier fails too.
//
// Found on spring-boot, where "add retry to the datasource health check"
// returned ZERO mentions of DataSourceHealthIndicator while "data source"
// returned five — with the class indexed at high confidence the whole time.
// The phrasing that failed is the one Spring itself uses, in
// `spring.datasource.url`.
func TestCompoundTermMatchesASplitIdentifier(t *testing.T) {
	const name = "DataSourceHealthIndicator"

	// Sanity: the premise. If SplitIdentifier ever stops splitting this, the
	// test below would pass for the wrong reason.
	parts := search.SplitIdentifier(name)
	if len(parts) < 2 {
		t.Fatalf("SplitIdentifier(%q) = %v; this test assumes it splits", name, parts)
	}

	for _, tc := range []struct {
		term  string
		match bool
		why   string
	}{
		{"datasource", true, "adjacent pair, the reported defect"},
		{"healthindicator", true, "adjacent pair at the tail"},
		{"datasourcehealthindicator", true, "the whole identifier run together"},
		{"data", true, "a single word still matches"},
		{"health", true, "a single word still matches"},
		{"sourceindicator", false, "not adjacent — must not match"},
		{"datasink", false, "unrelated compound"},
	} {
		score, _ := nameSignal(name, []Term{{Text: tc.term, Weight: 1, Stem: tc.term}})
		got := score > 0
		if got != tc.match {
			t.Errorf("nameSignal(%q, %q) matched=%v, want %v (%s)", name, tc.term, got, tc.match, tc.why)
		}
	}
}

// TestCompoundCandidatesAreOfferedByTheIndex closes the loop: the scorer
// matching is useless if the index never offers the symbol to be scored.
//
// findSeeds iterates seedCandidates rather than the whole catalogue, so a
// symbol absent from the token index is never scored at all — which is
// precisely how a scorer-only fix would look correct in a unit test and change
// nothing in the product.
func TestCompoundCandidatesAreOfferedByTheIndex(t *testing.T) {
	c := &catalogue{
		nameTokens: map[string][]int64{},
		stemTokens: map[string][]int64{},
	}
	const name = "DataSourceHealthIndicator"
	c.indexTokens(42, name, "datasourcehealthindicator")

	for _, term := range []string{"datasource", "healthindicator", "datasourcehealthindicator"} {
		ids := c.seedCandidates([]Term{{Text: term, Weight: 1, Stem: term}})
		if len(ids) != 1 || ids[0] != 42 {
			t.Errorf("seedCandidates(%q) = %v, want [42] — the index does not offer "+
				"the symbol, so the scorer never sees it", term, ids)
		}
	}
	if ids := c.seedCandidates([]Term{{Text: "sourceindicator", Weight: 1, Stem: "sourceindicator"}}); len(ids) != 0 {
		t.Errorf("seedCandidates(\"sourceindicator\") = %v, want none: "+
			"non-adjacent words must not compound", ids)
	}
}
