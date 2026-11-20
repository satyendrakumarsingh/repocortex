package docs

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestTopLevelCallCountsAsAComponentCrossing: a call made outside every
// function starts at the file's synthetic caller, and the architecture document
// must show the dependency it makes, as `callers` does.
func TestTopLevelCallCountsAsAComponentCrossing(t *testing.T) {
	reg := smallIndex()
	const symTop = int64(900)
	caller := sym(symTop, fileAPI, model.KindFile, "(top level of internal/api/users.go)",
		"internal/api/users.go:(top level of internal/api/users.go)", 1)
	reg.top = registry.TopLevel{
		Callers: []model.Symbol{caller},
		Edges: []model.Edge{
			edge(90, symTop, symUserServiceList, model.EdgeCalls, "internal/api/users.go", 3, model.ConfidenceHigh),
		},
	}

	without := deriveFixture(t, smallIndex(), smallModel())
	with := deriveFixture(t, reg, smallModel())

	count := func(v *View) int {
		for _, c := range v.Crossings {
			if c.From == "api" && c.To == "service" {
				return c.Count
			}
		}
		return 0
	}
	if got, want := count(with), count(without)+1; got != want {
		t.Errorf("api -> service crossings = %d, want %d (the top-level call)", got, want)
	}
	if with.Counts.Edges != without.Counts.Edges+1 {
		t.Errorf("edge count = %d, want %d", with.Counts.Edges, without.Counts.Edges+1)
	}
	if with.Counts.Symbols != without.Counts.Symbols {
		t.Errorf("the synthetic caller is not a symbol: count went %d -> %d", without.Counts.Symbols, with.Counts.Symbols)
	}
	for _, c := range with.Components {
		for _, o := range without.Components {
			if o.Name == c.Name && c.Symbols != o.Symbols {
				t.Errorf("%s symbols %d -> %d; the synthetic caller must not be counted", c.Name, o.Symbols, c.Symbols)
			}
		}
	}

	// The same output whatever order the rows arrive in.
	again := deriveFixture(t, reg.shuffled(3), smallModel())
	if count(again) != count(with) {
		t.Errorf("crossing count depends on row order: %d vs %d", count(again), count(with))
	}
}
