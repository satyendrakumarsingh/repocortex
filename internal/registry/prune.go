package registry

import (
	"context"
	"fmt"
)

// PruneImpossibleEdges removes heuristic edges that violate fundamental language invariants:
//   - A CALLS edge whose target is a configuration key, table, column, dependency line,
//     field, or deployment unit. None of these are callable in any language.
//   - An EXTENDS or IMPLEMENTS edge whose target is not a type (class, interface, package, type_alias).
//
// Any symbol_references that were bound to those impossible targets are reset to 'unresolved'
// so that the reference table and edge table remain in sync.
func (r *Registry) PruneImpossibleEdges(ctx context.Context, runID int64) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS impossible_edge (id INTEGER PRIMARY KEY)`); err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: create temp: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM impossible_edge`); err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: clear temp: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO impossible_edge (id)
		SELECT e.id
		  FROM edges e
		  JOIN symbols s ON s.id = e.dst_id
		 WHERE e.index_run_id = ?
		   AND e.source <> 'compiler'
		   AND (
		       (e.kind = 'CALLS' AND s.kind IN ('configuration', 'column', 'table', 'dependency', 'field', 'deployment_unit'))
		       OR
		       (e.kind IN ('EXTENDS', 'IMPLEMENTS') AND s.kind NOT IN ('class', 'interface', 'package', 'type_alias'))
		   )`, runID)
	if err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: select: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}

	// Reset any reference.call that pointed to an impossible target
	if _, err := tx.ExecContext(ctx, `
		UPDATE symbol_references
		   SET resolution = 'unresolved', resolved_symbol_id = NULL, resolution_source = ''
		 WHERE index_run_id = ?
		   AND kind = 'reference.call'
		   AND resolved_symbol_id IN (
		       SELECT id FROM symbols
		        WHERE index_run_id = ?
		          AND kind IN ('configuration', 'column', 'table', 'dependency', 'field', 'deployment_unit')
		   )`, runID, runID); err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: reset references: %w", err)
	}

	// Delete the impossible edges
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id IN (SELECT id FROM impossible_edge)`); err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: delete: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("registry: prune impossible edges: commit: %w", err)
	}
	return int(n), nil
}

// ResetHeuristicResolutions returns every reference of the run that the
// heuristic resolver decided, in ANY outcome, to 'pending', and drops the
// resolver edges those decisions wrote, so the next Resolve decides them all
// again. It is `resolve --re-resolve`.
//
// Every outcome, not only 'resolved'. It reset only resolved references, so a
// reference recorded as unresolved or ambiguous was never looked at again,
// however much the index around it had changed: a definition added after the
// reference was first resolved stayed unbound through any number of
// re-resolves. Only a compiler's outcome is kept, with its edges, because
// resolution does not produce those and the compiler passes `resolve` runs
// next do.
//
// Only the resolver's edges go, by the rule ResetFileResolutions states. It
// used to delete every edge whose source was not a compiler, which took the
// analysis layer with it (EXPOSES, PUBLISHES, CONSUMES and the rest are
// written by `index`, not by resolution, so nothing put them back), and the
// heuristic edges of references a compiler had confirmed, which the reset
// itself had kept.
func (r *Registry) ResetHeuristicResolutions(ctx context.Context, runID int64) (refsReset, edgesDropped int, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("registry: reset heuristic: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nEdges, err := resetResolutions(ctx, tx, runID, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("registry: reset heuristic: %w", err)
	}

	resRefs, err := tx.ExecContext(ctx, `
		UPDATE symbol_references
		   SET resolution = 'pending', resolved_symbol_id = NULL,
		       resolution_source = '', prior_resolution = '', external = 0
		 WHERE index_run_id = ?
		   AND resolution <> 'pending'
		   AND resolution_source <> 'compiler'`, runID)
	if err != nil {
		return 0, 0, fmt.Errorf("registry: reset heuristic: update references: %w", err)
	}
	nRefs, _ := resRefs.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("registry: reset heuristic: commit: %w", err)
	}
	return int(nRefs), nEdges, nil
}
