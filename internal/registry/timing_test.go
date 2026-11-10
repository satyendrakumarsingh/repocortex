//go:build !race

package registry

import (
	"path/filepath"
	"testing"
	"time"
)

// TestWriteThroughput reports wall-clock timings for the required workload
// (50,000 symbols + 100,000 edges) during an ordinary `go test` run, so the
// numbers are visible without remembering to pass -bench.
//
// Excluded under -race deliberately, for two reasons. The race detector inflates
// this workload roughly 25x, which turns a 4-second test into a two-minute one
// for every developer running the standard `go test -race ./...` from
// CONTRIBUTING. And a timing report produced under the race detector is worse
// than none: it would be quoted as the indexer's throughput when it is off by
// more than an order of magnitude. The concurrency tests are what -race is for
// here; this one measures.
func TestWriteThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput report skipped under -short")
	}
	db := openDiskStore(t, filepath.Join(t.TempDir(), "index.db"))
	defer func() { _ = db.Close() }()

	p := writeBenchRun(t, New(db))

	rate := func(rows int, d time.Duration) float64 { return float64(rows) / d.Seconds() }
	t.Logf("on-disk WAL store, batch size %d", DefaultBatchSize)
	t.Logf("  %6d files + %d symbols: %8v (%7.0f rows/s)",
		benchFiles, benchSymbols, p.symbols.Round(time.Millisecond),
		rate(benchFiles+benchSymbols, p.symbols))
	t.Logf("  %6d edges:               %8v (%7.0f rows/s)",
		benchEdges, p.edges.Round(time.Millisecond), rate(benchEdges, p.edges))
	t.Logf("  commit (validate+digest+swap): %v", p.commit.Round(time.Millisecond))
	t.Logf("  total: %v for %d rows (%.0f rows/s)",
		p.total.Round(time.Millisecond), benchFiles+benchSymbols+benchEdges,
		rate(benchFiles+benchSymbols+benchEdges, p.total))
}
