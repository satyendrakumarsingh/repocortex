package captest

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// SyntheticSpec describes a generated repository-scale fixture.
//
// The shape matters more than the totals. A uniform out-degree makes a depth-3
// walk touch a couple of hundred nodes and benchmarks nothing; real call graphs
// are hub-heavy, and it is landing on a hub — a logging helper, an error
// constructor — that turns a cheap impact query into an expensive one. The
// defaults mirror internal/graph's own benchmark fixture so numbers from the
// two packages are comparable.
type SyntheticSpec struct {
	// Symbols is the total symbol count. 100,000 is the measured order of
	// magnitude for a ~11k-file tree.
	Symbols int

	// LocalFan is ordinary out-edges per symbol, biased towards a nearby
	// neighbourhood the way a package is.
	LocalFan int

	// Hubs and HubFan add symbols with pathological in-degree.
	Hubs, HubFan int

	// TestEvery makes every Nth symbol a test that calls its predecessor, so
	// the test-discovery signals have something realistic to find. 0 disables.
	TestEvery int

	// Files spreads symbols over this many files (across Files/10 directories),
	// which is what the co-location signal indexes on.
	Files int

	Refs Refs
}

// DefaultSyntheticSpec is the 100k-symbol / ~500k-edge shape.
func DefaultSyntheticSpec() SyntheticSpec {
	return SyntheticSpec{
		Symbols:   100_000,
		LocalFan:  4,
		Hubs:      100,
		HubFan:    1000,
		TestEvery: 20,
		Files:     1000,
		Refs:      Refs{Resolved: 40_000, Ambiguous: 500, Unresolved: 5_000, Pending: 100},
	}
}

// SyntheticInfo reports what was actually built, so a benchmark can print the
// denominators rather than assume them.
type SyntheticInfo struct {
	Symbols int
	Edges   int64
	Files   int
	BuildIn time.Duration

	// TargetHub is a symbol with ~HubFan direct callers — the worst realistic
	// impact query.
	TargetHub int64
	// TargetHubName is that symbol's name, for passing to Analyze.
	TargetHubName string
	// TargetOrdinary is a symbol with an ordinary fan-in.
	TargetOrdinary     int64
	TargetOrdinaryName string
	// TargetTested is a symbol that has a conventionally-named test which also
	// calls it, so all three discovery signals have something to find.
	TargetTestedName string
}

// BuildSynthetic generates a repository-scale fixture and returns Deps over the
// real registry and graph.
//
// Rows go in with direct SQL, indexes dropped for the load and rebuilt exactly
// as migration 0001 defines them. Maintaining three indexes across 500k inserts
// triples the cost of a fixture nobody is measuring, and the recreated indexes
// are byte-identical, so the queries under benchmark see the production schema.
func BuildSynthetic(tb testing.TB, spec SyntheticSpec) (capability.Deps, int64, SyntheticInfo) {
	tb.Helper()
	ctx := context.Background()
	start := time.Now()

	if spec.Files <= 0 {
		spec.Files = 1000
	}

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	// Deliberately NO tb.Cleanup(db.Close). This fixture costs ~10s to build,
	// so callers share one via sync.Once across every benchmark in a package —
	// and a cleanup registered on whichever *testing.B happened to be first
	// would close the database out from under all the later ones. The store is
	// in memory and is reclaimed when the test binary exits.

	res, err := db.ExecContext(ctx,
		`INSERT INTO index_runs (status, committed, started_unix) VALUES ('ready', 1, 0)`)
	if err != nil {
		tb.Fatalf("insert run: %v", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		tb.Fatalf("run id: %v", err)
	}

	paths := make([]string, spec.Files+1)
	// The reference rows need a file to hang off; it is not part of the
	// symbol-bearing file set.
	paths[spec.Files] = "refs.go"
	for i := 0; i < spec.Files; i++ {
		// One directory per ten files, and every tenth file a test file, so
		// InDir and the sibling-test check both operate on realistic fan-out.
		if i%10 == 9 {
			paths[i] = fmt.Sprintf("pkg%d/file%d_test.go", i/10, i)
		} else {
			paths[i] = fmt.Sprintf("pkg%d/file%d.go", i/10, i)
		}
	}
	fileIDs := insertFiles(tb, db, runID, paths, nil)

	dropEdgeIndexes(tb, db)

	info := SyntheticInfo{Files: spec.Files}
	bulk(tb, db, `
		INSERT INTO symbols (id, index_run_id, file_id, kind, name, fqn,
		                     start_line, exported, confidence, source)
		VALUES (?,?,?,'function',?,?,?,?,'high','source_code')`,
		func(emit func(...any)) {
			for i := 1; i <= spec.Symbols; i++ {
				name := fmt.Sprintf("sym%d", i)
				fileIdx := i % spec.Files
				if spec.TestEvery > 0 && i%spec.TestEvery == 0 && i > 1 {
					name = fmt.Sprintf("TestSym%d", i-1)
					// Force test symbols into the test files.
					fileIdx = (i % (spec.Files / 10)) * 10 % spec.Files
					if fileIdx%10 != 9 {
						fileIdx += 9 - fileIdx%10
					}
					if fileIdx >= spec.Files {
						fileIdx = spec.Files - 1
					}
				}
				exported := 0
				if i%7 == 0 {
					exported = 1
				}
				path := paths[fileIdx]
				emit(i, runID, fileIDs[path], name, path+"#"+name, i%1000, exported)
			}
		})
	info.Symbols = spec.Symbols

	// #nosec G404 -- deterministic fixture generation, not security.
	rng := rand.New(rand.NewSource(20260816))
	confidences := []model.Confidence{
		model.ConfidenceHigh, model.ConfidenceHigh,
		model.ConfidenceMedium, model.ConfidenceLow,
	}
	kinds := []model.EdgeKind{
		model.EdgeCalls, model.EdgeCalls, model.EdgeReferences, model.EdgeContains,
	}

	line := 0
	bulk(tb, db, `
		INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, evidence_col, source)
		VALUES (?,?,?,?,?,'bench.go',?,0,'source_code')`,
		func(emit func(...any)) {
			for src := 1; src <= spec.Symbols; src++ {
				for k := 0; k < spec.LocalFan; k++ {
					var dst int
					if k == spec.LocalFan-1 {
						dst = rng.Intn(spec.Symbols) + 1
					} else {
						// Locality: most calls stay inside a ~200-symbol
						// neighbourhood, the way a package does.
						dst = src - 100 + rng.Intn(200)
						if dst < 1 || dst > spec.Symbols {
							dst = rng.Intn(spec.Symbols) + 1
						}
					}
					if dst == src {
						continue
					}
					line++
					emit(runID, src, dst, string(kinds[line%len(kinds)]),
						string(confidences[line%len(confidences)]), line)
				}
				// A test symbol calls the symbol it is named after, so the
				// graph signal has a real chain to follow.
				if spec.TestEvery > 0 && src%spec.TestEvery == 0 && src > 1 {
					line++
					emit(runID, src, src-1, string(model.EdgeCalls), string(model.ConfidenceHigh), line)
				}
			}
			for h := 1; h <= spec.Hubs; h++ {
				// +1 so a hub id is never a multiple of TestEvery: the hub is
				// meant to be ordinary production code with a huge fan-in, not
				// a test.
				hub := (h-1)*(spec.Symbols/spec.Hubs) + 1
				for j := 0; j < spec.HubFan; j++ {
					src := rng.Intn(spec.Symbols) + 1
					if src == hub {
						continue
					}
					line++
					emit(runID, src, hub, string(model.EdgeCalls), string(model.ConfidenceHigh), line)
				}
			}
		})

	createEdgeIndexes(tb, db)
	seedRefs(tb, db, runID, fileIDs["refs.go"], spec.Refs)

	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM edges WHERE index_run_id = ?`, runID).Scan(&info.Edges); err != nil {
		tb.Fatalf("count edges: %v", err)
	}

	// Pick the reporting targets by construction rather than by search, so the
	// benchmark measures the same shape on every run.
	info.TargetHub = int64(spec.Symbols/spec.Hubs) + 1 // h=2
	info.TargetHubName = fmt.Sprintf("sym%d", info.TargetHub)
	info.TargetOrdinary = 4242
	info.TargetOrdinaryName = "sym4242"
	if spec.TestEvery > 0 {
		info.TargetTestedName = fmt.Sprintf("sym%d", spec.TestEvery*3-1)
	}
	info.BuildIn = time.Since(start)

	tb.Logf("synthetic fixture: %d symbols, %d edges, %d files, built in %s",
		info.Symbols, info.Edges, info.Files, info.BuildIn.Round(time.Millisecond))
	return capability.NewDeps(registry.New(db), graph.New(db)), runID, info
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
	// Byte-identical to migration 0001. If that ever diverges, the benchmark is
	// measuring a schema production does not have.
	for _, s := range []string{
		`CREATE INDEX idx_edges_src  ON edges (index_run_id, src_id, kind)`,
		`CREATE INDEX idx_edges_dst  ON edges (index_run_id, dst_id, kind)`,
		`CREATE INDEX idx_edges_conf ON edges (index_run_id, confidence)`,
	} {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			tb.Fatalf("create index: %v", err)
		}
	}
}

// bulk runs one prepared statement over everything gen emits, in one
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
