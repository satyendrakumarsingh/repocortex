package scale

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/hotspots"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

var (
	flagSymbols     = flag.Int("scale.symbols", envInt("REPOCORTEX_SCALE_SYMBOLS", 5_000), "symbols in the synthetic graph")
	flagSourceFiles = flag.Int("scale.source-files", envInt("REPOCORTEX_SCALE_SOURCE_FILES", 150), "files in the generated source tree")
	flagCommits     = flag.Int("scale.commits", envInt("REPOCORTEX_SCALE_COMMITS", 400), "commits in the synthetic history")
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// fixture is the shared synthetic graph. It is built once per test binary: at
// a million symbols the build is the expensive part, and every test here reads
// it without writing.
type fixture struct {
	deps  capability.Deps
	reg   *registry.Registry
	graph *graph.Graph
	runID int64
	info  captest.SyntheticInfo
	spec  captest.SyntheticSpec
}

var (
	fixtureOnce sync.Once
	shared      fixture
)

// load returns the shared fixture, building it on first use.
func load(tb testing.TB) fixture {
	tb.Helper()
	fixtureOnce.Do(func() {
		n := *flagSymbols
		spec := captest.SyntheticSpec{
			Symbols:  n,
			LocalFan: 4,
			// Hub count and fan-in follow the default spec's proportions
			// (100 hubs of 1,000 callers per 100,000 symbols), so a larger
			// fixture is a larger repository of the same shape and not a
			// differently shaped one.
			Hubs:      max(n/1_000, 5),
			HubFan:    max(min(n/100, 1_000), 50),
			TestEvery: 20,
			Files:     max(n/100, 50),
			Refs: captest.Refs{
				Resolved:   n * 4 / 10,
				Ambiguous:  n / 200,
				Unresolved: n / 20,
				Pending:    0,
			},
		}
		deps, runID, info := captest.BuildSynthetic(tb, spec)
		reg, ok := deps.Registry.(*registry.Registry)
		if !ok {
			tb.Fatal("synthetic fixture did not return the real registry")
		}
		g, ok := deps.Graph.(*graph.Graph)
		if !ok {
			tb.Fatal("synthetic fixture did not return the real graph")
		}
		shared = fixture{deps: deps, reg: reg, graph: g, runID: runID, info: info, spec: spec}
	})
	if shared.reg == nil {
		tb.Fatal("the shared fixture failed to build in an earlier test")
	}
	return shared
}

// budget is an allowance that grows with the size of the fixture.
//
// Two terms, both stated at the call site. The fixed term covers what a query
// costs on an empty index (statement preparation, a cold cache). The per-unit
// term is the slope: if a change turns a linear scan quadratic, the measured
// time grows faster than this line at ANY fixture size, which is what lets the
// 5,000-symbol CI run catch what the million-symbol run would.
type budget struct {
	fixed   time.Duration
	perUnit time.Duration
}

func (b budget) at(units int) time.Duration {
	return (b.fixed + time.Duration(units)*b.perUnit) * raceFactor
}

// measurement is what one operation cost.
type measurement struct {
	elapsed time.Duration
	// retained is the growth in live heap across the operation, after a
	// collection on each side. It is what the operation left behind, which is
	// the figure a bounded-memory claim is about.
	retained int64
}

// measure runs fn once and reports its wall time and retained heap.
func measure(fn func()) measurement {
	before := liveHeap()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	return measurement{elapsed: elapsed, retained: int64(liveHeap()) - int64(before)}
}

// liveHeap is heap in use after the collector has settled. Two collections,
// because finalizers run by the first can free more for the second.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// requireWithin fails the test when an operation overran its time budget.
func requireWithin(t *testing.T, what string, m measurement, b budget, units int) {
	t.Helper()
	limit := b.at(units)
	t.Logf("%-34s %10s  (budget %s at %d units)  retained %s",
		what, m.elapsed.Round(time.Microsecond), limit.Round(time.Millisecond), units, signedBytes(m.retained))
	if m.elapsed > limit {
		t.Errorf("%s took %s, over its budget of %s at %d units (%s fixed + %s per unit, x%d under the race detector). "+
			"A budget this loose is only exceeded when the cost changed shape: look for a scan that became quadratic.",
			what, m.elapsed.Round(time.Millisecond), limit.Round(time.Millisecond), units, b.fixed, b.perUnit, raceFactor)
	}
}

// requireRetainedUnder fails the test when an operation left more heap behind
// than a bounded operation may.
func requireRetainedUnder(t *testing.T, what string, m measurement, limit int64) {
	t.Helper()
	limit *= raceFactor
	if m.retained > limit {
		t.Errorf("%s retained %s of heap; a bounded operation may retain at most %s. "+
			"Something is holding what it scanned.", what, signedBytes(m.retained), signedBytes(limit))
	}
}

func signedBytes(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%s%.1f MB", sign, float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%s%.1f KB", sign, float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%s%d B", sign, n)
	}
}

// p95 returns the 95th-percentile duration of samples.
func p95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[(len(sorted)*95+99)/100-1]
}

// syntheticHistory is a deterministic commit log over the fixture's files.
//
// Shaped like a real one: most commits touch a few files in one directory, a
// tenth of them are sweeps across many directories, and a small set of files
// is touched far more often than the rest, so the ranking has a head to find.
type syntheticHistory struct {
	commits []hotspots.Commit
}

func (h syntheticHistory) Commits(_ context.Context, limit int) ([]hotspots.Commit, error) {
	if len(h.commits) > limit {
		return h.commits[:limit], nil
	}
	return h.commits, nil
}

func newSyntheticHistory(files, commits int) syntheticHistory {
	path := func(i int) string {
		i %= files
		if i%10 == 9 {
			return fmt.Sprintf("pkg%d/file%d_test.go", i/10, i)
		}
		return fmt.Sprintf("pkg%d/file%d.go", i/10, i)
	}
	// A linear congruential sequence: deterministic, and with no dependency
	// on math/rand's algorithm staying the same between Go releases.
	state := uint32(20261112)
	next := func(n int) int {
		state = state*1664525 + 1013904223
		return int(state>>8) % n
	}

	out := make([]hotspots.Commit, 0, commits)
	for c := 0; c < commits; c++ {
		var paths []string
		switch {
		case c%10 == 0: // a sweep: many files, many directories
			for k := 0; k < 60; k++ {
				paths = append(paths, path(next(files)))
			}
		default:
			base := next(files)
			// Every third ordinary commit also touches one of twenty "hot"
			// files, which is what gives the ranking its head.
			if c%3 == 0 {
				paths = append(paths, path(next(20)))
			}
			for k := 0; k < 1+next(5); k++ {
				paths = append(paths, path(base+k))
			}
		}
		out = append(out, hotspots.Commit{
			SHA:   fmt.Sprintf("%040x", c),
			When:  int64(1_700_000_000 + c*3600),
			Paths: paths,
		})
	}
	return syntheticHistory{commits: out}
}
