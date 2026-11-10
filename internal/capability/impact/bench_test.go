package impact

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
)

// Benchmarks run against a 100,000-symbol / ~500,000-edge fixture — the order
// of magnitude a ~11k-file tree produces. CONTRIBUTING forbids publishing
// numbers no run produced, so these exist to produce them.
//
// p95 rather than mean is the number that matters: impact analysis sits in
// front of an interactive agent, and it is the slow call a user notices, not
// the average one. go test's own mean cannot express that, so every benchmark
// here reports p50 and p95 as custom metrics alongside it.

var (
	benchOnce sync.Once
	benchDeps capability.Deps
	benchRun  int64
	benchInfo captest.SyntheticInfo
)

func fixture(tb testing.TB) (capability.Deps, int64, captest.SyntheticInfo) {
	tb.Helper()
	benchOnce.Do(func() {
		benchDeps, benchRun, benchInfo = captest.BuildSynthetic(tb, captest.DefaultSyntheticSpec())
	})
	return benchDeps, benchRun, benchInfo
}

// runAnalyze times b.N analyses and reports the tail.
//
// The symbol index is warmed first, deliberately. A long-lived MCP server loads
// it once and answers thousands of queries from it, so including the cold load
// in every sample would measure a cost the product pays once per index swap.
// BenchmarkAnalyzeColdSymbolIndex measures that separately, because pretending
// it is free would be its own kind of dishonesty.
func runAnalyze(b *testing.B, deps capability.Deps, runID int64, target string, opts Options) {
	ctx := context.Background()

	warm, err := AnalyzeWith(ctx, deps, runID, target, opts)
	if err != nil {
		b.Fatalf("analyze: %v", err)
	}

	samples := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := AnalyzeWith(ctx, deps, runID, target, opts); err != nil {
			b.Fatalf("analyze: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()

	b.ReportMetric(msOf(percentile(samples, 0.95)), "p95-ms")
	b.ReportMetric(msOf(percentile(samples, 0.50)), "p50-ms")
	b.ReportMetric(float64(len(warm.Direct)), "direct")
	b.ReportMetric(float64(len(warm.Indirect)), "indirect")
	if warm.Truncated {
		b.ReportMetric(1, "truncated")
	}
}

func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(p*float64(len(sorted)-1))]
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// BenchmarkAnalyzeOrdinary is the common shape: a symbol with a normal fan-in.
func BenchmarkAnalyzeOrdinary(b *testing.B) {
	deps, runID, info := fixture(b)
	for _, depth := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			runAnalyze(b, deps, runID, info.TargetOrdinaryName, Options{MaxDepth: depth})
		})
	}
}

// BenchmarkAnalyzeHub is the worst realistic case: changing a symbol that a
// thousand other symbols call. This is the query the latency target is about.
func BenchmarkAnalyzeHub(b *testing.B) {
	deps, runID, info := fixture(b)
	for _, depth := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			runAnalyze(b, deps, runID, info.TargetHubName, Options{MaxDepth: depth})
		})
	}
}

// BenchmarkAnalyzeNodeBudget sweeps MaxNodes, which is the knob that actually
// decides whether a depth-3 impact query is interactive — see internal/graph's
// package comment for why it is MaxNodes and not depth.
func BenchmarkAnalyzeNodeBudget(b *testing.B) {
	deps, runID, info := fixture(b)
	for _, cap := range []int{500, 2_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("maxnodes=%d", cap), func(b *testing.B) {
			runAnalyze(b, deps, runID, info.TargetHubName, Options{MaxDepth: 3, MaxNodes: cap})
		})
	}
}

// BenchmarkAnalyzeColdSymbolIndex measures the cost the memoised symbol index
// hides: the first query after an index swap pays for hydrating 100k symbols.
//
// Reported separately rather than folded into the others, because a warm number
// presented as the whole story would understate what a freshly restarted server
// does on its first call.
func BenchmarkAnalyzeColdSymbolIndex(b *testing.B) {
	_, runID, info := fixture(b)
	ctx := context.Background()

	samples := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A fresh Deps over the same store means a cold cache every iteration.
		cold := capability.Deps{Registry: benchDeps.Registry, Graph: benchDeps.Graph}
		start := time.Now()
		if _, err := AnalyzeWith(ctx, cold, runID, info.TargetOrdinaryName, Options{MaxDepth: 3}); err != nil {
			b.Fatalf("analyze: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	b.ReportMetric(msOf(percentile(samples, 0.95)), "p95-ms")
	b.ReportMetric(msOf(percentile(samples, 0.50)), "p50-ms")
	b.ReportMetric(float64(info.Symbols), "symbols-hydrated")
}

// TestBenchmarkSpecIsStillTheAdvertisedShape keeps the published numbers
// honest. The figures in the package comment describe a 100k-symbol /
// ~500k-edge graph; if the spec drifts, those figures quietly stop being about
// the thing they claim to be about.
//
// A constant check rather than a build, deliberately: building the real fixture
// under -race costs about six minutes, and CONTRIBUTING has CI run
// `go test -race ./...`. The generator's behaviour is exercised at reduced
// scale by TestSyntheticGeneratorProducesAHubShapedGraph below.
func TestBenchmarkSpecIsStillTheAdvertisedShape(t *testing.T) {
	t.Parallel()
	spec := captest.DefaultSyntheticSpec()
	if spec.Symbols != 100_000 {
		t.Fatalf("benchmark spec has %d symbols; the published p95 figures claim 100,000", spec.Symbols)
	}
	if got := spec.Symbols * spec.LocalFan; got < 400_000 {
		t.Fatalf("benchmark spec generates ~%d local edges; the published figures claim ~500,000", got)
	}
	if spec.Hubs == 0 || spec.HubFan == 0 {
		t.Fatal("benchmark spec has no hub tier; a uniform out-degree benchmarks nothing")
	}
}

// TestSyntheticGeneratorProducesAHubShapedGraph exercises the generator and the
// at-scale truncation path at a size that stays cheap under the race detector.
func TestSyntheticGeneratorProducesAHubShapedGraph(t *testing.T) {
	t.Parallel()
	spec := captest.SyntheticSpec{
		Symbols: 5_000, LocalFan: 4, Hubs: 5, HubFan: 300, TestEvery: 20, Files: 100,
		Refs: captest.Refs{Resolved: 400, Unresolved: 60, Ambiguous: 10},
	}
	deps, runID, info := captest.BuildSynthetic(t, spec)
	if info.Symbols != spec.Symbols {
		t.Fatalf("fixture has %d symbols, want %d", info.Symbols, spec.Symbols)
	}
	if info.Edges < int64(spec.Symbols*spec.LocalFan) {
		t.Fatalf("fixture has %d edges, want at least %d", info.Edges, spec.Symbols*spec.LocalFan)
	}

	rep, err := AnalyzeWith(context.Background(), deps, runID, info.TargetHubName, Options{MaxDepth: 3})
	if err != nil {
		t.Fatalf("analyze hub: %v", err)
	}
	if len(rep.Direct) == 0 {
		t.Fatal("hub target has no direct callers; the fixture is not hub-shaped")
	}
	// At repository scale the default bounds must bite, and must say so.
	if !rep.Truncated {
		t.Fatalf("a depth-3 walk from a 1000-caller hub was not truncated at MaxNodes=%d", DefaultMaxNodes)
	}
	if !rep.Completeness.Truncated {
		t.Fatal("truncation at scale did not reach the completeness block")
	}
	if _, ok := findUnknown(rep, UnknownUnexpandedFrontier); !ok {
		t.Fatal("truncation at scale produced no unexpanded_frontier unknown")
	}
}
