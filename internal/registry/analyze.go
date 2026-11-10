package registry

import (
	"context"
	"fmt"
)

// Analyze refreshes SQLite's query-planner statistics.
//
// WHY THIS IS NOT OPTIONAL, measured on a 100k-symbol / 500k-edge graph:
//
// Without `sqlite_stat1`, SQLite's planner guesses index selectivity, and on
// this schema it guesses badly. Two cases found while benchmarking traversal:
//
//   - Filtering by confidence made the planner choose idx_edges_conf and scan
//     every edge in the run — roughly 400x slower than running the same query
//     with no filter at all.
//   - Adding ORDER BY src_id to the incoming-neighbour query made it choose
//     idx_edges_src and scan 500,000 rows: 235 ms instead of 1.9 ms.
//
// Both were worked around with INDEXED BY hints, but hints are a blunt tool:
// they pin one plan forever and stop the planner adapting as data changes.
// Statistics fix the cause instead of the symptom.
//
// PRAGMA analysis_limit bounds the work so this stays proportionate. Without
// it, ANALYZE scans entire indexes and a large repository would pay seconds at
// every commit. The limit samples enough rows to get selectivity approximately
// right, which is all the planner needs — the difference between "this index
// matches 1% of rows" and "99%" is what drives the decision, not precision.
func (r *Registry) Analyze(ctx context.Context) error {
	// 400 is SQLite's own documented recommendation for bounded ANALYZE.
	if _, err := r.db.ExecContext(ctx, `PRAGMA analysis_limit=400`); err != nil {
		return fmt.Errorf("registry: set analysis limit: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `ANALYZE`); err != nil {
		return fmt.Errorf("registry: analyze: %w", err)
	}
	// Statistics live in sqlite_stat1, and the planner caches its reading of
	// them per connection. Without this, connections opened before ANALYZE keep
	// using the old (or absent) statistics for their lifetime — which on a
	// pooled connection means most queries never see the improvement.
	if _, err := r.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("registry: optimize: %w", err)
	}
	return nil
}
