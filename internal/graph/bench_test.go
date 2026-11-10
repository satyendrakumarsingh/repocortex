package graph

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Benchmark shape: 100,000 symbols and 500,000 edges — the order of magnitude a
// ~11k-file tree produces (~94k symbols, ~531k references). CONTRIBUTING
// forbids publishing numbers no run produced, so these exist to produce them.
//
// The topology matters more than the totals. A uniform out-degree of 5 makes a
// depth-3 walk touch ~150 nodes, which would benchmark nothing. Real call
// graphs are hub-heavy: a logging helper or an error constructor has thousands
// of callers, and it is hitting one of those that turns a cheap traversal into
// an expensive one. So the fixture is locality-biased with an explicit hub
// tier, and the headline case starts from 100 roots, which is what "these files
// changed, what is affected" actually looks like.
const (
	benchSymbols = 100_000
	benchLocal   = 4   // ordinary out-edges per symbol
	benchHubs    = 100 // symbols with pathological in-degree
	benchHubFan  = 1000
)

var (
	benchOnce  sync.Once
	benchGraph *Graph
)

func benchmarkGraph(tb testing.TB) *Graph {
	tb.Helper()
	benchOnce.Do(func() { benchGraph = buildBenchGraph(tb) })
	if benchGraph == nil {
		tb.Fatal("benchmark graph unavailable")
	}
	return benchGraph
}

func buildBenchGraph(tb testing.TB) *Graph {
	tb.Helper()
	ctx := context.Background()
	start := time.Now()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}

	var runID, fileID int64
	res, err := db.ExecContext(ctx,
		`INSERT INTO index_runs (status, committed, started_unix) VALUES ('ready', 1, 0)`)
	if err != nil {
		tb.Fatalf("insert run: %v", err)
	}
	if runID, err = res.LastInsertId(); err != nil {
		tb.Fatalf("run id: %v", err)
	}
	res, err = db.ExecContext(ctx,
		`INSERT INTO files (index_run_id, path, language, content_hash) VALUES (?, 'bench.go', 'go', 'h')`, runID)
	if err != nil {
		tb.Fatalf("insert file: %v", err)
	}
	if fileID, err = res.LastInsertId(); err != nil {
		tb.Fatalf("file id: %v", err)
	}

	// The three edge indexes are dropped for the load and rebuilt afterwards.
	// Maintaining them per row triples the insert cost of a fixture nobody is
	// measuring; the recreated indexes are byte-identical to migration 0001's,
	// so the queries under benchmark see exactly the production schema.
	dropEdgeIndexes(tb, db)

	bulk(tb, db, `
		INSERT INTO symbols (id, index_run_id, file_id, kind, name, fqn, start_line, confidence, source)
		VALUES (?,?,?,'function',?,?,?,'high','source_code')`,
		func(emit func(...any)) {
			for i := 1; i <= benchSymbols; i++ {
				name := fmt.Sprintf("sym%d", i)
				emit(i, runID, fileID, name, "bench.go#"+name, i)
			}
		})

	rng := rand.New(rand.NewSource(20260816)) // #nosec G404 -- deterministic fixture, not security
	confidences := []model.Confidence{
		model.ConfidenceHigh, model.ConfidenceHigh,
		model.ConfidenceMedium, model.ConfidenceLow,
	}
	kinds := []model.EdgeKind{model.EdgeCalls, model.EdgeCalls, model.EdgeReferences, model.EdgeImports}

	line := 0
	bulk(tb, db, `
		INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, evidence_col, source)
		VALUES (?,?,?,?,?,'bench.go',?,0,'source_code')`,
		func(emit func(...any)) {
			for src := 1; src <= benchSymbols; src++ {
				for k := 0; k < benchLocal; k++ {
					var dst int
					if k == benchLocal-1 {
						dst = rng.Intn(benchSymbols) + 1 // cross-package reference
					} else {
						// Locality: most calls stay inside a ~200-symbol
						// neighbourhood, the way a package does.
						dst = src - 100 + rng.Intn(200)
						if dst < 1 || dst > benchSymbols {
							dst = rng.Intn(benchSymbols) + 1
						}
					}
					line++
					emit(runID, src, dst, string(kinds[line%len(kinds)]),
						string(confidences[line%len(confidences)]), line)
				}
			}
			for h := 1; h <= benchHubs; h++ {
				hub := h * (benchSymbols / benchHubs)
				for j := 0; j < benchHubFan; j++ {
					line++
					emit(runID, rng.Intn(benchSymbols)+1, hub, string(model.EdgeCalls),
						string(model.ConfidenceHigh), line)
				}
			}
		})

	createEdgeIndexes(tb, db)

	var edges int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM edges`).Scan(&edges); err != nil {
		tb.Fatalf("count edges: %v", err)
	}
	tb.Logf("bench fixture: %d symbols, %d edges, built in %s", benchSymbols, edges, time.Since(start).Round(time.Millisecond))
	return New(db)
}

func dropEdgeIndexes(tb testing.TB, db *store.DB) {
	tb.Helper()
	for _, name := range []string{"idx_edges_src", "idx_edges_dst", "idx_edges_conf"} {
		if _, err := db.ExecContext(context.Background(), "DROP INDEX "+name); err != nil {
			tb.Fatalf("drop %s: %v", name, err)
		}
	}
}

func createEdgeIndexes(tb testing.TB, db *store.DB) {
	tb.Helper()
	stmts := []string{
		`CREATE INDEX idx_edges_src  ON edges (index_run_id, src_id, kind)`,
		`CREATE INDEX idx_edges_dst  ON edges (index_run_id, dst_id, kind)`,
		`CREATE INDEX idx_edges_conf ON edges (index_run_id, confidence)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			tb.Fatalf("create index: %v", err)
		}
	}
}

// bulk runs one prepared statement over everything gen emits, inside a single
// transaction.
func bulk(tb testing.TB, db *store.DB, query string, gen func(emit func(...any))) {
	tb.Helper()
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	st, err := tx.PrepareContext(ctx, query)
	if err != nil {
		_ = tx.Rollback()
		tb.Fatalf("prepare: %v", err)
	}
	var failed error
	gen(func(args ...any) {
		if failed != nil {
			return
		}
		if _, err := st.ExecContext(ctx, args...); err != nil {
			failed = err
		}
	})
	_ = st.Close()
	if failed != nil {
		_ = tx.Rollback()
		tb.Fatalf("bulk insert: %v", failed)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// benchRoots picks n deterministic, well-spread roots.
func benchRoots(n int) []int64 {
	roots := make([]int64, n)
	stride := benchSymbols / n
	for i := range roots {
		roots[i] = int64(i*stride + 7)
	}
	return roots
}

// runTraversal times b.N traversals and reports p95 latency alongside the usual
// mean. The 500ms target is a tail requirement — an interactive agent notices
// the slow call, not the average one — and go test's mean cannot express it.
func runTraversal(b *testing.B, g *Graph, q Query) {
	ctx := context.Background()

	// Warm the page cache so the first iteration does not dominate p95.
	if _, err := g.Traverse(ctx, q); err != nil {
		b.Fatalf("traverse: %v", err)
	}

	samples := make([]time.Duration, 0, b.N)
	var nodes int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		res, err := g.Traverse(ctx, q)
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("traverse: %v", err)
		}
		samples = append(samples, elapsed)
		nodes = len(res.Nodes)
	}
	b.StopTimer()

	b.ReportMetric(msOf(percentile(samples, 0.95)), "p95-ms")
	b.ReportMetric(msOf(percentile(samples, 0.50)), "p50-ms")
	b.ReportMetric(float64(nodes), "nodes")
}

func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func msOf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// BenchmarkTraverseSingleRoot is the "what does this function reach" shape.
func BenchmarkTraverseSingleRoot(b *testing.B) {
	g := benchmarkGraph(b)
	for _, depth := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			runTraversal(b, g, Query{
				Roots: []int64{42}, MaxDepth: depth, MaxNodes: 100_000, Direction: Outgoing,
			})
		})
	}
}

// BenchmarkTraverseImpact is the headline case: 100 changed symbols, walk
// backwards to find everything affected. This is the query the 500ms target is
// about.
func BenchmarkTraverseImpact(b *testing.B) {
	g := benchmarkGraph(b)
	roots := benchRoots(100)
	for _, depth := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			runTraversal(b, g, Query{
				Roots: roots, MaxDepth: depth, MaxNodes: 100_000, Direction: Incoming,
			})
		})
	}
}

// BenchmarkTraverseBoth is the worst case the API allows: undirected expansion
// with no kind or confidence filter, which is two indexed queries per level and
// the largest possible frontier.
func BenchmarkTraverseBoth(b *testing.B) {
	g := benchmarkGraph(b)
	roots := benchRoots(100)
	for _, depth := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			runTraversal(b, g, Query{
				Roots: roots, MaxDepth: depth, MaxNodes: 100_000, Direction: Both,
			})
		})
	}
}

// BenchmarkTraverseFiltered checks that pushing the confidence filter into SQL
// actually pays, by comparing an identical walk with and without it.
func BenchmarkTraverseFiltered(b *testing.B) {
	g := benchmarkGraph(b)
	roots := benchRoots(100)
	b.Run("min=high", func(b *testing.B) {
		runTraversal(b, g, Query{
			Roots: roots, MaxDepth: 3, MaxNodes: 100_000,
			Direction: Incoming, MinConfidence: model.ConfidenceHigh,
		})
	})
	b.Run("kinds=CALLS", func(b *testing.B) {
		runTraversal(b, g, Query{
			Roots: roots, MaxDepth: 3, MaxNodes: 100_000,
			Direction: Incoming, Kinds: []model.EdgeKind{model.EdgeCalls},
		})
	})
}

// BenchmarkTraverseCapped sweeps the node budget, which is what actually
// decides whether a depth-3 walk is interactive. This is the benchmark to read
// against the 500ms target: MaxNodes, not depth, is the knob that keeps a
// traversal inside it.
func BenchmarkTraverseCapped(b *testing.B) {
	g := benchmarkGraph(b)
	roots := benchRoots(100)
	for _, cap := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("maxnodes=%d", cap), func(b *testing.B) {
			runTraversal(b, g, Query{
				Roots: roots, MaxDepth: 3, MaxNodes: cap, Direction: Incoming,
			})
		})
	}
}

func BenchmarkNeighbours(b *testing.B) {
	g := benchmarkGraph(b)
	ctx := context.Background()
	hub := int64(benchSymbols / benchHubs) // a symbol with ~1000 callers

	b.Run("ordinary", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := g.Callees(ctx, 42, ""); err != nil {
				b.Fatalf("callees: %v", err)
			}
		}
	})
	b.Run("hub", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := g.Callers(ctx, hub, ""); err != nil {
				b.Fatalf("callers: %v", err)
			}
		}
	})
}

func BenchmarkPaths(b *testing.B) {
	g := benchmarkGraph(b)
	ctx := context.Background()
	from, to := int64(42), int64(benchSymbols/benchHubs)

	if got, err := g.Paths(ctx, from, to, 5, 3); err != nil || len(got) == 0 {
		b.Skipf("no path %d -> %d within depth 3 in this fixture (err=%v)", from, to, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := g.Paths(ctx, from, to, 5, 3); err != nil {
			b.Fatalf("paths: %v", err)
		}
	}
}

// BenchmarkTraverseHubPruned measures the case hub pruning exists for: the
// depth-3 impact walk that BenchmarkTraverseImpact shows reaching 28,193 nodes
// in ~80ms because it crosses the fixture's hub tier. The comparison that
// matters is against BenchmarkTraverseImpact/depth=3, not against itself —
// pruning is not a micro-optimisation, it is a smaller and more useful answer.
//
// The percentile variant reuses one Graph, so it measures the STEADY state:
// the distribution query is memoised per run and only the first call pays for
// it. BenchmarkHubPercentileResolveCold below measures that first call, which
// is the number a caller choosing between the two forms actually needs.
func BenchmarkTraverseHubPruned(b *testing.B) {
	g := benchmarkGraph(b)
	roots := benchRoots(100)
	base := Query{Roots: roots, MaxDepth: 3, MaxNodes: 100_000, Direction: Incoming}

	b.Run("limit=50", func(b *testing.B) {
		q := base
		q.HubDegreeLimit = 50
		runTraversal(b, g, q)
	})
	b.Run("percentile=0.99", func(b *testing.B) {
		q := base
		q.HubPercentile = 0.99
		runTraversal(b, g, q)
	})
}

// BenchmarkHubPercentileResolveCold measures resolving HubPercentile against a
// cold memo: one grouped, ranked pass over every edge in the run.
//
// It gets its own benchmark because the number is large enough to change how
// the feature should be used, and burying it inside an amortised traversal
// figure would be exactly the kind of flattering measurement CONTRIBUTING
// forbids. A fresh Graph per iteration is what makes it cold — the same DB,
// the same run, no memo.
func BenchmarkHubPercentileResolveCold(b *testing.B) {
	warm := benchmarkGraph(b)
	ctx := context.Background()
	q := Query{HubPercentile: 0.99}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := New(warm.db)
		runID, err := g.run(ctx)
		if err != nil {
			b.Fatalf("run: %v", err)
		}
		if _, err := g.hubLimit(ctx, runID, q, []edgeDir{dirIn}); err != nil {
			b.Fatalf("hubLimit: %v", err)
		}
	}
}
