package registry

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// UpgradeStats reports what an UpgradeEdges call actually did.
//
// The three outcomes are kept apart because they answer different questions. A
// run that inserts thousands and upgrades none means the compiler found
// relationships the heuristic resolver never saw. One that upgrades thousands
// means the resolver was right and can now say so with certainty. One that does
// neither means the index did not line up with the repository at all — and
// that is the failure this whole path is most likely to produce silently.
type UpgradeStats struct {
	// Inserted is edges that did not exist at all.
	Inserted int
	// Upgraded is edges that existed with weaker confidence and were replaced.
	Upgraded int
	// Unchanged is edges already recorded at equal or better confidence.
	Unchanged int
}

func (s UpgradeStats) Total() int { return s.Inserted + s.Upgraded + s.Unchanged }

// The confidence ranking, mirrored from model.Confidence.Rank into SQL.
//
// Duplicated rather than computed in Go because the comparison has to happen
// inside the UPDATE's WHERE clause: reading the stored confidence, deciding in
// Go and writing back would be a read-modify-write race against any concurrent
// writer, and `repocortex watch` is a concurrent writer by design.
//
// Two spellings of one rule, one reading the bound parameter and one the stored
// column. They are separate constants rather than a format string because the
// query below must be a compile-time constant end to end — a SQL string built
// at runtime is indistinguishable, to a reader or a linter, from one built out
// of user input.
//
// All three must agree: these two with each other, and both with
// model.Confidence.Rank. TestConfidenceRankMatchesSQL executes them and holds
// them to it.
const (
	rankOfParam = `(CASE ? WHEN 'exact' THEN 4 WHEN 'high' THEN 3 ` +
		`WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END)`
	rankOfStored = `(CASE confidence WHEN 'exact' THEN 4 WHEN 'high' THEN 3 ` +
		`WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END)`
)

// upgradeSQL replaces an edge only when the incoming confidence is strictly
// stronger than the stored one. The final comparison is the whole point: it is
// what makes a downgrade impossible and a re-import a no-op.
//
// It also preserves what it overwrites. prior_confidence/prior_source hold the
// claim the row carried before the FIRST upgrade to touch it — see migration
// 0012 — because an upgraded edge is not a vanished heuristic edge, it is a
// heuristic edge the compiler agreed with, and `repocortex accuracy` has to be
// able to tell those apart. The CASE guards against a second upgrade
// overwriting the original claim with an intermediate one; no path produces
// that today, since exact is the ceiling and only the importer upgrades, but a
// history column that can be silently rewritten is not a history column.
const upgradeSQL = `
	UPDATE edges
	   SET prior_confidence = CASE WHEN prior_confidence = '' THEN confidence
	                               ELSE prior_confidence END,
	       prior_source     = CASE WHEN prior_confidence = '' THEN source
	                               ELSE prior_source END,
	       confidence   = ?,
	       source       = ?,
	       detail       = ?,
	       evidence_col = ?
	 WHERE index_run_id = ? AND src_id = ? AND dst_id = ? AND kind = ?
	   AND evidence_file = ? AND evidence_line = ?
	   AND ` + rankOfParam + ` > ` + rankOfStored

// UpgradeEdges writes edges into an existing run, replacing weaker facts about
// the same relationship with stronger ones.
//
// THIS IS THE MECHANISM BEHIND THE PRODUCT'S CENTRAL CLAIM. RepoCortex promises
// that a heuristic edge is upgraded in place when compiler-backed evidence
// arrives, rather than sitting alongside it as a second opinion. Without this,
// importing a SCIP index would leave every already-known relationship recorded
// as a guess, because the edges table's UNIQUE key spans
// (run, src, dst, kind, evidence_file, evidence_line) and NOT confidence — so
// the compiler's version of a call the resolver had already found collides with
// it, and an INSERT OR IGNORE keeps the weaker row.
//
// It differs from AddResolvedEdges deliberately. That path ignores conflicts,
// which is right for the heuristic resolver: re-running it must be idempotent
// and must never churn rows. This path resolves conflicts by strength, which is
// right for an import that carries better evidence than whatever is there.
//
// Downgrades are impossible. The WHERE clause admits only strictly stronger
// confidence, so importing a stale or weaker index cannot degrade the graph,
// and re-importing the same index twice is a no-op reported as Unchanged.
//
// Every edge is validated first — an 'exact' edge that did not come from a
// compiler is refused here, and again by the SQL CHECK, because neither check
// is trusted alone.
func (r *Registry) UpgradeEdges(ctx context.Context, runID int64, edges []model.Edge) (UpgradeStats, error) {
	var st UpgradeStats
	if len(edges) == 0 {
		return st, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return st, fmt.Errorf("registry: upgrade edges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insert, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO edges
			(index_run_id, src_id, dst_id, kind, confidence,
			 evidence_file, evidence_line, evidence_col, source, detail)
		VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return st, fmt.Errorf("registry: upgrade edges: prepare insert: %w", err)
	}
	defer func() { _ = insert.Close() }()

	// The UPDATE runs only when the INSERT found a conflicting row. Splitting
	// the two, rather than using ON CONFLICT DO UPDATE, is what makes the
	// Inserted and Upgraded counts exact: a single upsert reports one row
	// changed either way and cannot say which happened, and a caller that
	// cannot tell "the compiler found 4,000 new relationships" from "the
	// compiler confirmed 4,000 known ones" has not learned anything.
	upd, err := tx.PrepareContext(ctx, upgradeSQL)
	if err != nil {
		return st, fmt.Errorf("registry: upgrade edges: prepare update: %w", err)
	}
	defer func() { _ = upd.Close() }()

	for i := range edges {
		e := edges[i]
		e.IndexRunID = runID
		if err := e.Validate(); err != nil {
			return st, fmt.Errorf("registry: refusing invalid edge: %w", err)
		}

		res, err := insert.ExecContext(ctx,
			runID, e.SrcID, e.DstID, string(e.Kind), string(e.Confidence),
			e.Evidence.FilePath, e.Evidence.Range.Start.Line, e.Evidence.Range.Start.Col,
			string(e.Evidence.Source), e.Evidence.Detail)
		if err != nil {
			return st, fmt.Errorf("registry: insert edge %d->%d: %w", e.SrcID, e.DstID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			st.Inserted++
			continue
		}

		res, err = upd.ExecContext(ctx,
			string(e.Confidence), string(e.Evidence.Source), e.Evidence.Detail,
			e.Evidence.Range.Start.Col,
			runID, e.SrcID, e.DstID, string(e.Kind),
			e.Evidence.FilePath, e.Evidence.Range.Start.Line,
			string(e.Confidence))
		if err != nil {
			return st, fmt.Errorf("registry: upgrade edge %d->%d: %w", e.SrcID, e.DstID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			st.Upgraded++
		} else {
			st.Unchanged++
		}
	}

	if err := tx.Commit(); err != nil {
		return st, fmt.Errorf("registry: upgrade edges: commit: %w", err)
	}
	return st, nil
}
