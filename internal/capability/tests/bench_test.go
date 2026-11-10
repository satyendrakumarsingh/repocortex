package tests

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
)

// Test discovery has a cost profile impact analysis does not: the naming signal
// issues ten prefix probes per query against the whole symbol-name key set, and
// co-location scans a directory. Both are O(symbols) shaped, so they are
// measured on the same 100k-symbol fixture rather than assumed cheap.

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

func runFind(b *testing.B, deps capability.Deps, runID int64, target string, opts Options) {
	ctx := context.Background()
	warm, err := FindTestsWith(ctx, deps, runID, target, opts)
	if err != nil {
		b.Fatalf("find tests: %v", err)
	}

	samples := make([]time.Duration, 0, b.N)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := FindTestsWith(ctx, deps, runID, target, opts); err != nil {
			b.Fatalf("find tests: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	b.ReportMetric(msOf(percentile(samples, 0.95)), "p95-ms")
	b.ReportMetric(msOf(percentile(samples, 0.50)), "p50-ms")
	b.ReportMetric(float64(len(warm.Tests)), "tests-found")
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

// BenchmarkFindTestsAllSignals is the shape an agent actually issues.
func BenchmarkFindTestsAllSignals(b *testing.B) {
	deps, runID, info := fixture(b)
	runFind(b, deps, runID, info.TargetTestedName, Options{})
}

// BenchmarkFindTestsBySignal isolates each signal's cost, so a regression can be
// attributed rather than guessed at.
func BenchmarkFindTestsBySignal(b *testing.B) {
	deps, runID, info := fixture(b)
	b.Run("graph", func(b *testing.B) {
		runFind(b, deps, runID, info.TargetTestedName, Options{SkipNaming: true, SkipColocation: true})
	})
	b.Run("naming", func(b *testing.B) {
		runFind(b, deps, runID, info.TargetTestedName, Options{SkipGraph: true, SkipColocation: true})
	})
	b.Run("colocation", func(b *testing.B) {
		runFind(b, deps, runID, info.TargetTestedName, Options{SkipGraph: true, SkipNaming: true})
	})
}

// BenchmarkFindTestsHub is the pathological case: discovery from a symbol a
// thousand things call, where the graph signal has a large frontier to filter.
func BenchmarkFindTestsHub(b *testing.B) {
	deps, runID, info := fixture(b)
	runFind(b, deps, runID, info.TargetHubName, Options{})
}
