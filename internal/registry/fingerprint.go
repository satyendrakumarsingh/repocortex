package registry

import (
	"context"
	"fmt"
)

// Fingerprint identifies the content of a run cheaply enough to check on every
// query.
//
// It exists so callers can cache expensive derived structures — the context
// engine's in-memory catalogue is the motivating case, at 55 ms per build on a
// 10,000-symbol run and over a second at 94,000 — without risking a stale
// answer.
//
// KEYING ON runID ALONE WOULD BE WRONG. A committed run is not immutable:
// incremental indexing updates it IN PLACE (see registry/inplace.go), so the
// same run id can describe different content minute to minute while a developer
// edits. A cache keyed only on the id would serve pre-edit results indefinitely,
// which is the exact staleness this product exists to eliminate.
type Fingerprint struct {
	RunID       int64
	Files       int64
	Symbols     int64
	MaxSymbolID int64
	MaxFileID   int64
}

// CatalogueFingerprint reads a run's fingerprint.
//
// Two indexed aggregate scans — one per table — paid on every request in
// exchange for skipping the rebuild.
//
// It is TWO and not four. `count(*)` and `max(id)` over the same run are two
// aggregates of one row set, but written as four independent scalar subqueries
// SQLite has no reason to notice that and reads each table twice: 184 ms of a
// 1.88 s warm context request on a 500,000-symbol run, for a check whose whole
// justification is being cheaper than what it avoids. Pairing the aggregates
// per table halves it. There is no O(1) form available — `count(*)` with a
// WHERE clause is a scan of the index range however it is spelled — so this is
// the floor for an exact answer, and an inexact one would reintroduce precisely
// the staleness documented below.
//
// WHY THESE FOUR FIELDS DETECT EVERY MUTATION:
//
//	Insert  — both id columns are AUTOINCREMENT, so max always advances, even
//	          when a delete-then-insert leaves the count unchanged. This is
//	          exactly what ReplaceFile does on every incremental update.
//	Delete  — the count falls.
//	Replace — caught by the advancing max, as above.
//
// The combination that would fool it — deleting rows and inserting exactly as
// many while the maximum id stays put — cannot occur, because AUTOINCREMENT
// never reissues an id.
func (r *Registry) CatalogueFingerprint(ctx context.Context, runID int64) (Fingerprint, error) {
	fp := Fingerprint{RunID: runID}
	err := r.db.QueryRowContext(ctx, `
		SELECT f.n, s.n, s.m, f.m
		  FROM (SELECT count(*) AS n, COALESCE(max(id),0) AS m
		          FROM files   WHERE index_run_id = ?) AS f,
		       (SELECT count(*) AS n, COALESCE(max(id),0) AS m
		          FROM symbols WHERE index_run_id = ?) AS s`,
		runID, runID,
	).Scan(&fp.Files, &fp.Symbols, &fp.MaxSymbolID, &fp.MaxFileID)
	if err != nil {
		return fp, fmt.Errorf("registry: catalogue fingerprint for run %d: %w", runID, err)
	}
	return fp, nil
}
