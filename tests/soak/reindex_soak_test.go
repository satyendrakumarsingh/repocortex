//go:build soak

package soak

import (
	"context"
	"testing"
	"time"
)

// minReindexIterations is the floor regardless of -soak.duration.
//
// Three, because two proves nothing about a per-iteration leak: any pair of
// numbers has a trend. Three is the smallest count where "returns to baseline"
// and "climbs by a fixed amount each time" look different, which keeps the
// short local default honest instead of merely fast.
const minReindexIterations = 3

// TestRepeatedFullIndexStability indexes the same unchanging tree over and
// over, in the same store, and checks two things after every pass.
//
// DETERMINISM. The registry digest must be byte-identical every time. That is
// the property digest.go exists to protect, and the reason it is worth
// re-checking in a loop rather than once is that the failure mode is
// probabilistic: a digest that depends on goroutine scheduling or map
// iteration order agrees with itself most of the time. One comparison catches
// it with low probability; a hundred catches it with near certainty.
//
// RESOURCE RETURN. Each pass builds a staging run, swaps it in and retires the
// previous one, so a correct implementation ends every iteration owning
// exactly what it owned at the start. A leak that is invisible in a single run
// — one connection, one parser, one goroutine per pass — becomes a straight
// line here, which is the entire reason this test loops instead of asserting
// once.
//
// Deliberately the same store across iterations rather than a fresh one. A new
// database per pass would allocate a new 64 MiB page cache each time and the
// heap series would be dominated by an artefact of the test, hiding whatever
// it was supposed to measure.
func TestRepeatedFullIndexStability(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t, *flagFiles)

	// The initial index in newRepo is the warm-up: grammars loaded, pool
	// populated, statements prepared. Baseline after it, not before.
	want, err := r.reg.RegistryDigest(ctx, r.runID)
	if err != nil {
		t.Fatalf("read baseline digest: %v", err)
	}

	tr := newTracker("repeated full index")
	deadline := time.Now().Add(budget())

	var (
		iterations int
		digests    = map[string]int{want: 1}
		goroutines []int
	)
	for i := 0; i < minReindexIterations || time.Now().Before(deadline); i++ {
		runID, err := r.fullIndex(ctx)
		if err != nil {
			t.Fatalf("full index iteration %d: %v", i+1, err)
		}
		iterations++

		got, err := r.reg.RegistryDigest(ctx, runID)
		if err != nil {
			t.Fatalf("digest iteration %d: %v", i+1, err)
		}
		digests[got]++
		if got != want {
			// Fatal, not Errorf: once determinism has broken, every later
			// iteration reports the same thing and the useful information is
			// which iteration first diverged.
			t.Fatalf("registry digest changed on iteration %d over an unchanged tree:\n  first = %s\n  now   = %s\n"+
				"something in the pipeline depends on scheduling, map order or walk order",
				i+1, want, got)
		}

		// Sample every iteration. Passes are seconds apart at most, so the
		// settling cost is bounded, and a per-iteration series is what turns
		// "it leaked" into "it leaks 1.2 MiB per pass".
		s := tr.sample()
		goroutines = append(goroutines, s.goroutines)
	}

	tr.notef("iterations   %d full index passes over an unchanged %d-file tree", iterations, *flagFiles)
	tr.notef("digest       %s, identical on every pass (%d distinct value(s) seen)", want, len(digests))
	tr.notef("goroutines   per-iteration series %v", goroutines)
	tr.finish(t)
}
