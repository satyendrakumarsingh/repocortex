//go:build perfrepro

package context

import (
	stdctx "context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// Reproduction + stage-attribution harness for the context-engine latency
// investigation. Behind its own tag so it never slows `go test ./...` or
// `go test -tags perf ./...`:
//
//	go test -tags perfrepro -run TestReproWarmLatency -timeout 60m ./internal/context/
//
// One Assembler is reused for every sample, exactly as internal/mcp does, so
// the catalogue cache is actually exercised.

const reproTask = "Add idempotency to payment callback processing"

func pctOf(ds []time.Duration, p int) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	i := (len(ds) * p) / 100
	if i >= len(ds) {
		i = len(ds) - 1
	}
	return ds[i]
}

func TestReproWarmLatency(t *testing.T) {
	sizes := []int{5000, 25000}
	if v := os.Getenv("REPRO_SIZES"); v == "50000" {
		sizes = []int{50000}
	} else if v == "5000" {
		sizes = []int{5000}
	} else if v == "25000" {
		sizes = []int{25000}
	}
	for _, files := range sizes {
		t.Run(fmt.Sprintf("files=%d", files), func(t *testing.T) {
			build := time.Now()
			reg, g, runID := syntheticRepo(t, files) // one file per class
			t.Logf("corpus built in %v", time.Since(build).Round(time.Millisecond))

			asm := NewAssembler()
			ctx := stdctx.Background()

			cold := time.Now()
			if _, err := asm.Assemble(ctx, reg, g, runID, Request{Task: reproTask}); err != nil {
				t.Fatal(err)
			}
			coldD := time.Since(cold)

			const samples = 25
			ds := make([]time.Duration, 0, samples)
			for i := 0; i < samples; i++ {
				start := time.Now()
				pack, err := asm.Assemble(ctx, reg, g, runID, Request{Task: reproTask})
				if err != nil {
					t.Fatal(err)
				}
				ds = append(ds, time.Since(start))
				if i == 0 {
					t.Logf("pack: %d items, %d excluded, ~%d tokens",
						len(pack.Items), len(pack.Excluded), pack.TokenEstimate)
				}
			}
			hits, misses := asm.cache.stats()
			t.Logf("files=%d cold=%v warm p50=%v p95=%v max=%v (n=%d) cache %d hits/%d misses",
				files, coldD.Round(time.Millisecond),
				pctOf(ds, 50).Round(time.Millisecond), pctOf(ds, 95).Round(time.Millisecond),
				pctOf(ds, 100).Round(time.Millisecond), samples, hits, misses)
		})
	}
}

// TestReproGraphExpansion checks the "unbounded frontier" hypothesis directly.
//
// The name-derived seeds in this corpus land on class symbols, which carry no
// edges, so TestReproStageBreakdown under-measures expansion. This drives
// expand() from method roots that do have fan-out, which is the shape the
// hypothesis is about.
func TestReproGraphExpansion(t *testing.T) {
	files := 25000
	if os.Getenv("REPRO_SIZES") == "5000" {
		files = 5000
	}
	reg, g, runID := syntheticRepo(t, files)
	ctx := stdctx.Background()

	cat, err := loadCatalogue(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	tuning := DefaultTuning()
	budget := Request{}.Budget.withDefaults()

	// Method symbols, which is where the synthetic edges live.
	var roots []seed
	for _, id := range cat.order {
		if cat.symbols[id].Kind == "method" {
			roots = append(roots, seed{symbolID: id, score: 1})
		}
		if len(roots) >= tuning.MaxSeeds {
			break
		}
	}

	var ds []time.Duration
	var reached int
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		out, _, err := expand(ctx, g, roots, budget, tuning.Thresholds)
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(t0))
		reached = len(out)
	}
	t.Logf("files=%d expand from %d method roots (depth %d, MaxNodes %d): reached=%d p50=%v p95=%v",
		files, len(roots), budget.MaxGraphDepth, tuning.MaxGraphNodes, reached,
		pctOf(ds, 50).Round(time.Microsecond), pctOf(ds, 95).Round(time.Microsecond))
}

// TestReproStageBreakdown attributes warm latency to individual stages by
// replaying the same sequence assemble() runs.
func TestReproStageBreakdown(t *testing.T) {
	files := 25000
	if os.Getenv("REPRO_SIZES") == "5000" {
		files = 5000
	}
	reg, g, runID := syntheticRepo(t, files)
	ctx := stdctx.Background()

	cache := &catalogueCache{}
	if _, err := assemble(ctx, cache, reg, g, runID, Request{Task: reproTask}); err != nil {
		t.Fatal(err)
	}

	type stage struct {
		name string
		d    time.Duration
	}
	agg := map[string]time.Duration{}
	var order []string
	add := func(name string, d time.Duration) {
		if _, ok := agg[name]; !ok {
			order = append(order, name)
		}
		agg[name] += d
	}

	const reps = 10
	for i := 0; i < reps; i++ {
		budget := Request{Task: reproTask}.Budget.withDefaults()
		tuning := DefaultTuning()
		intent := ParseTask(reproTask)

		t0 := time.Now()
		cat, err := cache.get(ctx, reg, runID)
		if err != nil {
			t.Fatal(err)
		}
		add("catalogue(cached)", time.Since(t0))

		t0 = time.Now()
		seeds := findSeeds(cat, intent, tuning.Thresholds)
		add("findSeeds", time.Since(t0))

		t0 = time.Now()
		lexical, err := searchLexical(ctx, reg, intent, tuning.Thresholds)
		if err != nil {
			t.Fatal(err)
		}
		add("searchLexical", time.Since(t0))

		t0 = time.Now()
		reached, _, err := expand(ctx, g, seeds, budget, tuning.Thresholds)
		if err != nil {
			t.Fatal(err)
		}
		add("graphExpand", time.Since(t0))

		b := newBuilder(cat, intent, tuning, budget, reg, runID)
		t0 = time.Now()
		b.fromSeeds(seeds)
		add("fromSeeds", time.Since(t0))
		t0 = time.Now()
		b.fromGraph(reached)
		add("fromGraph", time.Since(t0))
		t0 = time.Now()
		b.fromLexical(lexical)
		add("fromLexical", time.Since(t0))
		t0 = time.Now()
		if err := b.fromData(ctx); err != nil {
			t.Fatal(err)
		}
		add("fromData(file reads)", time.Since(t0))
		t0 = time.Now()
		b.fromTests()
		add("fromTests", time.Since(t0))
		t0 = time.Now()
		b.finishSignals(seeds)
		add("finishSignals", time.Since(t0))
		t0 = time.Now()
		_, _, _ = b.choose(ctx)
		add("choose(+excerpts)", time.Since(t0))

		if i == 0 {
			t.Logf("seeds=%d reached=%d lexical=%d units=%d",
				len(seeds), len(reached), len(lexical), len(b.units))
		}
	}

	var stages []stage
	var total time.Duration
	for _, n := range order {
		stages = append(stages, stage{n, agg[n] / reps})
		total += agg[n] / reps
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].d > stages[j].d })
	t.Logf("files=%d  mean per-call stage cost (n=%d)", files, reps)
	for _, s := range stages {
		t.Logf("  %-22s %10v  %5.1f%%", s.name, s.d.Round(time.Microsecond),
			100*float64(s.d)/float64(total))
	}
	t.Logf("  %-22s %10v", "TOTAL", total.Round(time.Millisecond))
}
