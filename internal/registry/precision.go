package registry

import (
	"context"
	"fmt"
)

// PrecisionRow is the agreement between the heuristic resolver and a
// compiler-backed index, for one confidence tier.
type PrecisionRow struct {
	Confidence string
	Judged     int64 // heuristic edges a compiler fact could adjudicate
	Agree      int64 // …of which the compiler resolved to the same definition
	Disagree   int64
}

// Precision is Agree over Judged, or 0 when nothing was judgeable.
func (r PrecisionRow) Precision() float64 {
	if r.Judged == 0 {
		return 0
	}
	return float64(r.Agree) / float64(r.Judged)
}

// PrecisionReport is the whole comparison plus what it could not cover.
type PrecisionReport struct {
	Rows []PrecisionRow

	// HeuristicEdges is every CALLS edge the resolver produced.
	HeuristicEdges int64
	// CompilerEdges is every fact imported from the compiler-backed index.
	CompilerEdges int64
	// AmbiguousLines are lines the compiler resolved to more than one target,
	// excluded from judgement because which one the heuristic "should" have
	// picked is not defined.
	AmbiguousLines int64
	// JudgeableLines are lines the compiler resolved to exactly one target.
	JudgeableLines int64
}

// ResolutionPrecision measures the heuristic resolver against compiler facts
// already imported into the same run.
//
// This is the number the accuracy claim rests on, so the method matters as much
// as the result.
//
// WHAT IS COMPARED. A heuristic CALLS edge is adjudicated by asking the
// compiler what the reference at that source position resolves to. Agreement
// means both name the same definition.
//
// WHY BY LINE, NOT BY COLUMN. The two tools do not share a column convention:
// for `svc.Method()` the resolver records the column of `svc` and scip-go
// records the column of `Method`, and the gap varies with the receiver's name,
// so exact column matching silently discards two thirds of the comparison
// (5,805 judgeable edges became 3,541). Matching by line alone is instead too
// generous, because a quarter of referenced lines carry more than one target.
//
// So judgement is restricted to lines where the compiler resolved EXACTLY ONE
// distinct target. There, the answer is unambiguous whatever either tool
// believes about columns. Everything else is reported as uncovered rather than
// scored, because a measurement that quietly counts the cases it cannot decide
// is worse than a smaller one that says so.
//
// WHAT THE RESOLVER CLAIMED, NOT WHAT THE ROW NOW SAYS. UpgradeEdges rewrites a
// weaker edge in place, so a call the resolver got right is rewritten to
// source='compiler' the moment a compiler fact confirms it — and reading
// source='source_code' off the live row would then count only the calls it got
// WRONG. Migration 0012 preserves the overwritten claim; resolverSource and
// resolverConfidence below are the only correct way to read it back.
//
// AccuracyAdjudication, in this package, reads the same pair through the same
// two constants rather than through a second spelling of them. It used to carry
// its own copy in internal/cli, held together with this one by a test; the copy
// is gone, and TestResolverClaimPredicatesAgree now asserts the two READINGS
// agree rather than that two strings still match.
//
// WHAT THIS IS NOT. It is not recall. SCIP records that one symbol references
// another, not that it calls it, so the compiler's edge set is a superset of
// the resolver's by construction and dividing by it would understate the
// resolver for doing exactly what it was designed to do.
const (
	resolverSource     = `(CASE WHEN e.prior_source <> '' THEN e.prior_source ELSE e.source END)`
	resolverConfidence = `(CASE WHEN e.prior_source <> '' THEN e.prior_confidence ELSE e.confidence END)`
)

func (r *Registry) ResolutionPrecision(ctx context.Context, runID int64) (PrecisionReport, error) {
	var rep PrecisionReport

	counts := []struct {
		dst   *int64
		query string
	}{
		{&rep.HeuristicEdges, `SELECT COUNT(*) FROM edges e
		   WHERE e.index_run_id = ? AND e.kind = 'CALLS'
		     AND ` + resolverSource + ` = 'source_code'`},
		{&rep.CompilerEdges, `SELECT COUNT(*) FROM edges
		   WHERE index_run_id = ? AND source = 'compiler'`},
		{&rep.JudgeableLines, `SELECT COUNT(*) FROM (
		     SELECT 1 FROM edges WHERE index_run_id = ? AND source = 'compiler'
		      GROUP BY evidence_file, evidence_line HAVING COUNT(DISTINCT dst_id) = 1)`},
		{&rep.AmbiguousLines, `SELECT COUNT(*) FROM (
		     SELECT 1 FROM edges WHERE index_run_id = ? AND source = 'compiler'
		      GROUP BY evidence_file, evidence_line HAVING COUNT(DISTINCT dst_id) > 1)`},
	}
	for _, c := range counts {
		if err := r.db.QueryRowContext(ctx, c.query, runID).Scan(c.dst); err != nil {
			return rep, fmt.Errorf("registry: resolution precision: %w", err)
		}
	}

	rows, err := r.db.QueryContext(ctx, `
		WITH unambiguous AS (
		    SELECT evidence_file AS f, evidence_line AS l, MIN(dst_id) AS dst
		      FROM edges
		     WHERE index_run_id = ? AND source = 'compiler'
		     GROUP BY evidence_file, evidence_line
		    HAVING COUNT(DISTINCT dst_id) = 1),
		heuristic AS (
		    SELECT e.dst_id, `+resolverConfidence+` AS confidence,
		           e.evidence_file AS f, e.evidence_line AS l
		      FROM edges e
		     WHERE e.index_run_id = ? AND e.kind = 'CALLS'
		       AND `+resolverSource+` = 'source_code'),
		judged AS (
		    SELECT h.confidence, (h.dst_id = u.dst) AS agrees
		      FROM heuristic h
		      JOIN unambiguous u ON u.f = h.f AND u.l = h.l)
		SELECT confidence, COUNT(*), SUM(agrees)
		  FROM judged
		 GROUP BY confidence
		 ORDER BY confidence`, runID, runID)
	if err != nil {
		return rep, fmt.Errorf("registry: resolution precision: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var row PrecisionRow
		if err := rows.Scan(&row.Confidence, &row.Judged, &row.Agree); err != nil {
			return rep, err
		}
		row.Disagree = row.Judged - row.Agree
		rep.Rows = append(rep.Rows, row)
	}
	return rep, rows.Err()
}
