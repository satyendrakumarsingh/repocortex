//go:build !race

package pipeline

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// peakHeapDuring samples HeapAlloc while fn runs and returns the highest value
// seen.
//
// Residency, not churn. An earlier version of this test measured TotalAlloc,
// which counts every byte ever allocated and therefore grows with the WORK —
// five times the references allocate roughly five times regardless of what is
// held. It passed against the very bug it was written to catch, reporting 4.42x
// growth for the preloaded index against 4.72x for the bounded cache: the
// defect was invisible because churn dominated the signal.
//
// Sampling can only ever under-report a spike, never invent one, so a
// measurement that exceeds the bound is real.
func peakHeapDuring(fn func()) uint64 {
	var peak uint64
	done := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	fn()
	close(done)
	return peak
}

// TestResolveMemoryIsFlatInRepositorySize pins the property the candidate cache
// exists to provide.
//
// Resolution used to load every symbol in the run into one map before binding
// anything, so peak heap tracked the repository rather than the work: a
// 25,000-file tree held 725,000 entries and peaked at 132 MB of Go heap against
// 25 MB after this change, and RSS on a 50,000-file tree went 819 MB -> 207 MB.
// On a 5,000-file tree, 69% of what it loaded had a name that no reference in
// the run ever mentioned.
//
// Nothing caught that, because every functional test passes at any footprint —
// an index that fits and an index that swaps produce identical tables.
//
// The fixture is deliberately symbol-heavy rather than reference-heavy. What
// used to scale with the repository was the SYMBOL index, so the corpora differ
// in symbol count while doing nearly identical binding work; a
// reference-heavy fixture would drown the signal in churn, which is exactly how
// the first version of this test failed to catch anything.
func TestResolveMemoryIsFlatInRepositorySize(t *testing.T) {
	if testing.Short() {
		t.Skip("memory measurement skipped under -short")
	}
	ctx := context.Background()

	// Best of two: a sample can only be higher than the true peak, so the
	// minimum is the honest estimate.
	measure := func(files, perFile int) uint64 {
		t.Helper()
		best := ^uint64(0)
		for attempt := 0; attempt < 2; attempt++ {
			reg, runID := resolveFixture(t, t.TempDir(), files, perFile, 20)
			runtime.GC()

			var err error
			peak := peakHeapDuring(func() {
				_, err = Resolve(ctx, reg, runID, ResolveOptions{})
			})
			if err != nil {
				t.Fatalf("resolve %d files: %v", files, err)
			}
			if peak < best {
				best = peak
			}
		}
		return best
	}

	// Same file count and the same reference count; only the symbol table
	// differs, by 30x. Anything holding the run's symbols shows up as a 30x
	// heap difference, and anything bounded shows up as no difference.
	const files = 120
	small := measure(files, 20)  // 2,400 symbols
	large := measure(files, 600) // 72,000 symbols

	growth := float64(large) / float64(small)
	t.Logf("%d symbols peaked at %d KB; %d symbols peaked at %d KB; growth %.2fx for 30x the symbol table",
		files*20, small/1024, files*600, large/1024, growth)

	// 30x the symbols, identical work. A preloaded index tracks the symbol
	// table; a bounded cache does not.
	//
	// Measured on both implementations: 1.78x with the cache, 4.31x with the
	// preloaded index. 3.0 sits between them with room on each side, so the
	// bound separates the two designs rather than the two machines it might
	// run on. The remaining 1.78x is the fixture's own growth — 30x the
	// symbols is 30x the rows to insert before resolution starts.
	const bound = 3.0
	if growth > bound {
		t.Errorf("peak heap grew %.2fx for 30x the symbol table (bound %.1fx): "+
			"resolution is holding something proportional to repository size again, "+
			"which is what registry.CandidateCache exists to prevent", growth, bound)
	}
}
