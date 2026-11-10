package registry

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Stamping a run's coverage while it is still staging.
//
// The pipeline knows the reconciliation the moment its worker pool is joined,
// which is before the Writer closes and long before anything publishes the run.
// Recording it there means the fact is on disk even if the process dies between
// the last file and the swap: an abandoned run then carries its own explanation
// instead of looking like a healthy run that simply never committed.
//
// It goes through the Writer rather than the Registry on purpose. The Writer
// holds an open transaction on its own connection for the length of a batch,
// and a second connection issuing an UPDATE against the same staging row would
// be contending with it for SQLite's single write lock. Reusing the Writer's
// transaction is not an optimisation here, it is the difference between a write
// that lands and one that waits on a lock the caller itself is holding.

// RecordCompleteness stamps the staging run with what the caller measured.
//
// Idempotent, and safe to call before or after any Add*: it touches only the
// index_runs row. It lands with the Writer's next Flush, exactly as the rows
// around it do — Close rolls an open batch back, so a caller that discards its
// writes discards this measurement with them, which is the correct outcome for
// a run nobody is going to publish.
//
// CommitRun preserves what this recorded unless the committer passes
// WithCompleteness, which wins because it is the more recent measurement.
func (w *Writer) RecordCompleteness(ctx context.Context, c Completeness) error {
	if w.failed != nil {
		return w.failed
	}
	state := c.State
	if state == "" {
		state = model.IndexStateComplete
	}
	if state != model.IndexStateComplete && state != model.IndexStatePartial {
		// Caught by the CHECK too, but a typo deserves a readable error rather
		// than a constraint violation from three layers down.
		return w.fail(fmt.Errorf("registry: invalid index_state %q (want complete|partial)", state))
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		UPDATE index_runs
		   SET files_discovered = ?, index_state = ?, files_unaccounted = ?
		 WHERE id = ?`,
		c.FilesDiscovered, string(state), c.FilesUnaccounted, w.runID,
	); err != nil {
		return w.fail(fmt.Errorf("registry: record completeness for run %d: %w", w.runID, err))
	}
	return nil
}
