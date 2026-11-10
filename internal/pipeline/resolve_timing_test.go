//go:build !race

package pipeline

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestResolveScalesLinearly measures the curve the fix exists to flatten.
//
// Excluded under -race for the reason internal/registry's timing test gives:
// the detector inflates this workload enough that the number would be quoted
// and wrong. The bound is deliberately loose — 2.5x the per-reference cost for
// 4x the references — because it is a shape assertion on a shared machine, not
// a benchmark. Quadratic behaviour lands at 4x and the pre-fix code was already
// at 1.6x per 5x step on a corpus twenty times this size, so a real regression
// clears the bound while ordinary scheduling noise does not.
func TestResolveScalesLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement skipped under -short")
	}
	ctx := context.Background()

	// Best of two, because this runs on whatever machine CI gave it. A slow
	// sample can only be slow for reasons unrelated to the shape being
	// measured, so the minimum is the honest estimate of the work involved.
	measure := func(files int) (time.Duration, int64) {
		t.Helper()
		best := time.Duration(1<<63 - 1)
		var refs int64
		for attempt := 0; attempt < 2; attempt++ {
			reg, runID := resolveFixture(t, t.TempDir(), files, 10, 100)
			runtime.GC()
			start := time.Now()
			stats, err := Resolve(ctx, reg, runID, ResolveOptions{})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("resolve %d files: %v", files, err)
			}
			if elapsed < best {
				best = elapsed
			}
			refs = stats.References
		}
		return best, refs
	}

	smallT, smallRefs := measure(250)
	largeT, largeRefs := measure(1000)

	if smallRefs == 0 || largeRefs == 0 {
		t.Fatalf("fixture produced no references (%d, %d)", smallRefs, largeRefs)
	}
	smallPer := float64(smallT) / float64(smallRefs)
	largePer := float64(largeT) / float64(largeRefs)
	growth := largePer / smallPer

	t.Logf("%d refs in %v (%.2f µs/ref); %d refs in %v (%.2f µs/ref); growth %.2fx",
		smallRefs, smallT.Round(time.Millisecond), smallPer/1000,
		largeRefs, largeT.Round(time.Millisecond), largePer/1000, growth)

	const maxGrowth = 2.5
	if growth > maxGrowth {
		t.Errorf("per-reference cost grew %.2fx when the reference count grew 4x "+
			"(want <= %.1fx). Resolution has gone superlinear again; the usual cause "+
			"is a per-row write whose order or statement count scales with the run — "+
			"see registry.MarkResolutions.", growth, maxGrowth)
	}
}
