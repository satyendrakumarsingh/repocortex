package scale

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// The check for a function passed as a value reads stored text, which no other
// fixture here has: a synthetic tree with no text makes the check free, and it
// is not free on a real one. This fixture gives the first hundred files two
// thousand lines each, every line naming several symbols, so a symbol's name
// occurs in a dozen files and the check reads all of them.
//
// IT IS A SECOND FIXTURE, NOT TEXT IN THE FIRST. With stored text the shared
// fixture turns list_dead_code from milliseconds into seconds, which is that
// tool's cost and not this check's, and a budget test that fails for another
// tool's reason teaches nobody anything.
var (
	textOnce sync.Once
	textFix  fixture
)

func loadWithText(tb testing.TB) fixture {
	tb.Helper()
	textOnce.Do(func() {
		n := min(*flagSymbols, 20_000)
		spec := captest.SyntheticSpec{
			Symbols:   n,
			LocalFan:  4,
			Hubs:      max(n/1_000, 5),
			HubFan:    max(min(n/100, 1_000), 50),
			TestEvery: 20,
			Files:     max(n/100, 50),
			TextFiles: 100,
			TextLines: 2_000,
			Refs:      captest.Refs{Resolved: n * 4 / 10, Ambiguous: n / 200, Unresolved: n / 20},
		}
		deps, runID, info := captest.BuildSynthetic(tb, spec)
		reg, _ := deps.Registry.(*registry.Registry)
		g, _ := deps.Graph.(*graph.Graph)
		textFix = fixture{deps: deps, reg: reg, graph: g, runID: runID, info: info, spec: spec}
	})
	if textFix.reg == nil {
		tb.Fatal("the fixture with text failed to build in an earlier test")
	}
	return textFix
}

// TestTheUsedAsAValueCheckCostsAFixedAmount: at most 1 + MaxPathChecks names
// are looked up in the text index, whatever the size of the answer, so the
// check has a budget with no term for the size of the repository.
func TestTheUsedAsAValueCheckCostsAFixedAmount(t *testing.T) {
	f := loadWithText(t)
	ctx := context.Background()

	tgt, err := capability.ResolveTarget(ctx, f.deps, f.runID, f.info.TargetHubName)
	if err != nil {
		t.Fatalf("resolve the hub: %v", err)
	}
	idx, err := f.deps.SymbolIndex(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := impact.AnalyzeTarget(ctx, f.deps, f.runID, tgt, impact.Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("impact analysis: %v", err)
	}
	var reached []int64
	for _, list := range [][]impact.Impacted{rep.Direct, rep.Indirect} {
		for _, imp := range list {
			reached = append(reached, imp.ID)
		}
	}
	if len(reached) < 100 {
		t.Fatalf("the hub reaches %d symbols; the fixture is not hub-shaped", len(reached))
	}

	var vu capability.ValueUse
	var samples []time.Duration
	for range 5 {
		m := measure(func() {
			var cerr error
			if vu, cerr = f.deps.ValueUse(ctx, f.runID, tgt, idx, reached); cerr != nil {
				t.Fatalf("value use: %v", cerr)
			}
		})
		samples = append(samples, m.elapsed)
	}
	// Every name up to the bound, or fewer when the check stopped at its own
	// budget: on a loaded machine (every package's tests at once, -race) the
	// budget is reached first, and stopping is the behaviour under test.
	switch {
	case vu.Checked > 1+capability.MaxPathChecks:
		t.Fatalf("checked %d names; the bound is %d", vu.Checked, 1+capability.MaxPathChecks)
	case vu.Checked < 1+capability.MaxPathChecks && !vu.OutOfTime:
		t.Fatalf("checked %d names and did not run out of time; the bound is %d, and the fixture's hub has far more callers than that",
			vu.Checked, 1+capability.MaxPathChecks)
	case vu.OutOfTime:
		t.Logf("the check stopped at its %s budget after %d names", capability.TextBudget, vu.Checked)
	}
	// The check stops itself at capability.TextBudget, so it cannot take much
	// longer than that: the limit is the every-tool budget's fixed term, 1.5s,
	// with no term for the size of the repository. Measured alone it is about
	// 450ms; it was 818ms with every package's tests running at once.
	requireWithin(t, "used-as-a-value check", measurement{elapsed: p95(samples)},
		budget{fixed: 1500 * time.Millisecond}, f.info.Symbols)

	// And as the tools pay it: the same analysis with and without.
	with := p95Of(5, func() {
		if _, err := impact.AnalyzeTarget(ctx, f.deps, f.runID, tgt, impact.Options{MaxDepth: 3, CheckValueUse: true}); err != nil {
			t.Fatal(err)
		}
	})
	without := p95Of(5, func() {
		if _, err := impact.AnalyzeTarget(ctx, f.deps, f.runID, tgt, impact.Options{MaxDepth: 3}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("analyze_impact on the hub: %s without the check, %s with it", without.Round(time.Millisecond), with.Round(time.Millisecond))
	if extra := with - without; extra > (budget{fixed: 1500 * time.Millisecond}).at(0) {
		t.Errorf("the check added %s to analyze_impact on a hub, over its fixed budget", extra.Round(time.Millisecond))
	}

	tt := p95Of(5, func() {
		if _, err := tests.FindTestsForTarget(ctx, f.deps, f.runID, tgt, tests.Options{CheckValueUse: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("find_tests on the hub: %s with the check", tt.Round(time.Millisecond))
}

func p95Of(n int, fn func()) time.Duration {
	samples := make([]time.Duration, 0, n)
	for range n {
		samples = append(samples, measure(fn).elapsed)
	}
	return p95(samples)
}

// TestTheSubtypeDeclarationCheckCostsAFixedAmount: find_implementations reads
// the text of a bounded number of files for the target's name. The bound is
// larger than the one for the value-use check, because a base class that many
// files mention has its subclasses and anonymous instances spread among them.
// The cost has no term for the size of the repository, and it is held to the
// same fixed term as the other text checks.
func TestTheSubtypeDeclarationCheckCostsAFixedAmount(t *testing.T) {
	f := loadWithText(t)
	ctx := context.Background()

	var got registry.UnrecordedSubtypes
	samples := make([]time.Duration, 0, 5)
	for range 5 {
		m := measure(func() {
			var err error
			if got, err = f.reg.UnrecordedSubtypeDeclarations(ctx, f.runID, "x", nil); err != nil {
				t.Fatalf("subtype declarations: %v", err)
			}
		})
		samples = append(samples, m.elapsed)
	}
	t.Logf("subtype declaration check on a name every file holds: %s (known=%v complete=%v)", p95(samples).Round(time.Millisecond), got.Known, got.Complete)
	requireWithin(t, "subtype declaration check", measurement{elapsed: p95(samples)},
		budget{fixed: 1500 * time.Millisecond}, f.info.Symbols)
}
