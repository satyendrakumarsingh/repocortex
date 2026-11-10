package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// cycleFixture seeds two independent 3-cycles, so a budget of one cycle has
// something to refuse and a default budget has something to complete.
//
// Every symbol is seeded at `high`, which is not incidental: it is the shape of
// the live apm index (11,100 symbols, all `high`), and it is what makes a
// remedy reading "raise min_confidence to shrink the graph" a dead end. A
// fixture where confidences varied would have let that advice pass review.
func cycleFixture(t *testing.T, opts ...Option) *Server {
	t.Helper()
	syms := []captest.Sym{
		captest.S(1, "alpha", "pkg/a/one.go").Export(),
		captest.S(2, "beta", "pkg/a/two.go").Export(),
		captest.S(3, "gamma", "pkg/a/three.go").Export(),
		captest.S(4, "delta", "pkg/b/one.go").Export(),
		captest.S(5, "epsilon", "pkg/b/two.go").Export(),
		captest.S(6, "zeta", "pkg/b/three.go").Export(),
	}
	edges := []captest.Edge{
		captest.E(1, 2), captest.E(2, 3), captest.E(3, 1),
		captest.E(4, 5), captest.E(5, 6), captest.E(6, 4),
	}
	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 6})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "cycles"}, opts...)
}

// TestMetricsCycleBudgetAnswersAtTheLevelAskedForAndDiscloses drives the bound
// that keeps this screen interactive.
//
// It replaces a node-count downgrade that did not bound anything: the graph
// order it gated on was the symbol count, which is independent of the edge
// structure that cycle enumeration actually costs, so a 20,000-node graph at
// out-degree 10 passed the limit and cost 26s. The bound now lives in
// metrics.Cycles and caps work, so the level asked for is the level served.
func TestMetricsCycleBudgetAnswersAtTheLevelAskedForAndDiscloses(t *testing.T) {
	t.Parallel()

	// wantCaveat / notCaveat hold the SECTION's own sentence to the same
	// standard as the response-level omission. The caveat was written before
	// CycleBudget existed and blamed the length bound for every short list, so
	// a run stopped by MaxCycles read "only those of length 5 or less" beside
	// an omission saying the search stopped early — one fact, two vocabularies,
	// disagreeing.
	cases := []struct {
		name        string
		budget      metrics.CycleBudget
		wantCycles  int
		wantOmitted bool
		wantWords   []string
		wantCaveat  []string
		notCaveat   []string
	}{
		{
			name:       "a budget nothing exhausts enumerates both cycles and discloses no truncation",
			budget:     metrics.DefaultCycleBudget(),
			wantCycles: 2,
			notCaveat:  []string{"work budget"},
		},
		{
			name:        "the cycle count budget stops the list and names itself",
			budget:      metrics.CycleBudget{MaxLen: 5, MaxCycles: 1},
			wantCycles:  1,
			wantOmitted: true,
			wantWords:   []string{"1 cycle", "not the complete set"},
			wantCaveat:  []string{"work budget"},
		},
		{
			name:        "the step budget stops the search and names itself",
			budget:      metrics.CycleBudget{MaxLen: 5, MaxSteps: 1},
			wantCycles:  0,
			wantOmitted: true,
			wantWords:   []string{"step", "not the complete set"},
			wantCaveat:  []string{"work budget"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := cycleFixture(t, WithCycleBudget(tc.budget))

			var got MetricsResponse
			rec := get(t, srv, "/api/metrics?level=symbol", &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}

			// The bound must not change the question. A downgrade to file level
			// would answer a different one and was the previous design.
			if got.Level != LevelSymbol {
				t.Errorf("level = %q, want symbol — a work bound must not change the level answered at", got.Level)
			}
			for _, o := range got.Omissions.Omitted {
				if strings.Contains(o.Reason, "downgrad") {
					t.Errorf("the response still downgrades the level: %+v", o)
				}
			}

			if n := len(got.Sections[1].Cycles); n != tc.wantCycles {
				t.Errorf("listed %d cycle(s), want %d", n, tc.wantCycles)
			}

			caveat := got.Sections[1].Caveat
			for _, w := range tc.wantCaveat {
				if !strings.Contains(caveat, w) {
					t.Errorf("the section's own caveat never mentions %q while the response-level "+
						"omission says the search stopped early: %q", w, caveat)
				}
			}
			for _, w := range tc.notCaveat {
				if strings.Contains(caveat, w) {
					t.Errorf("a complete enumeration's caveat blames %q: %q", w, caveat)
				}
			}

			var budgetOm *Omission
			for i, o := range got.Omissions.Omitted {
				if o.Reason == ReasonCycleBudget {
					budgetOm = &got.Omissions.Omitted[i]
				}
			}
			if !tc.wantOmitted {
				if budgetOm != nil {
					t.Errorf("a complete enumeration still claims it was truncated: %+v", budgetOm)
				}
			} else {
				if budgetOm == nil {
					t.Fatalf("the enumeration was cut short and nothing says so: %+v", got.Omissions.Omitted)
				}
				said := budgetOm.Detail + " " + budgetOm.Remedy
				for _, w := range tc.wantWords {
					if !strings.Contains(said, w) {
						t.Errorf("the disclosure never says %q: %q", w, said)
					}
				}
			}

			assertRemediesAreActionable(t, srv, got)
		})
	}
}

// assertRemediesAreActionable checks every remedy the response offers by TAKING
// it, not by reading it.
//
// This is the assertion whose absence let a remedy ship that could not work:
// "raise min_confidence to shrink the symbol graph" is a dead end on an index
// where every symbol carries the same confidence, which is every real index
// measured so far. A remedy naming a request parameter is a claim about what
// that parameter does, and the claim is checkable here.
func assertRemediesAreActionable(t *testing.T, srv *Server, resp MetricsResponse) {
	t.Helper()

	considered := func(r MetricsResponse) int { return r.Sections[0].Considered }
	base := considered(resp)

	for _, o := range resp.Omissions.Omitted {
		if strings.TrimSpace(o.Remedy) == "" {
			t.Errorf("omission %q states no remedy at all", o.Reason)
			continue
		}
		low := strings.ToLower(o.Remedy)

		if strings.Contains(low, "raise min_confidence") {
			// The remedy claims a higher floor shrinks the graph. It is only
			// true if some higher floor removes SOME nodes and leaves some.
			works := false
			for _, f := range []model.Confidence{model.ConfidenceHigh, model.ConfidenceExact} {
				var at MetricsResponse
				rec := get(t, srv, fmt.Sprintf("/api/metrics?level=%s&min_confidence=%s", resp.Level, f), &at)
				if rec.Code != http.StatusOK {
					continue
				}
				if n := considered(at); n > 0 && n < base {
					works = true
				}
			}
			if !works {
				t.Errorf("omission %q tells the reader to raise min_confidence, but no higher floor "+
					"leaves a smaller non-empty graph on this index: %q", o.Reason, o.Remedy)
			}
		}

		for _, lvl := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
			if !strings.Contains(o.Remedy, "level="+string(lvl)) {
				continue
			}
			var at MetricsResponse
			rec := get(t, srv, "/api/metrics?level="+string(lvl), &at)
			if rec.Code != http.StatusOK {
				t.Errorf("omission %q sends the reader to level=%s, which answers %d", o.Reason, lvl, rec.Code)
				continue
			}
			if at.Level != lvl {
				t.Errorf("omission %q sends the reader to level=%s, which is answered at level=%s instead",
					o.Reason, lvl, at.Level)
			}
		}
	}
}
