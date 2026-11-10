package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DeleteFile removes a file and everything it contributed from a run.
//
// This is the counterpart to ReplaceFile for the case where a file is gone
// rather than changed: deleted, moved, renamed away, or newly excluded by an
// ignore rule. All four look identical to the registry and demand the same
// treatment — the file's symbols, its search rows, and every edge it evidenced
// must go. A symbol left behind for a file that no longer exists is worse than
// a missing symbol: an agent asked "where is this defined?" would be sent to a
// path that is not there.
//
// ReplaceFile deliberately refuses a nil file, because "replace with nothing"
// and "delete" are different intents and conflating them makes it easy to
// erase a file by passing a zero value through by accident. Deletion gets its
// own name.
//
// The whole removal runs in one transaction: a partial delete would leave the
// graph referencing symbols whose file row is already gone.
func (r *Registry) DeleteFile(ctx context.Context, runID int64, path string, opts ...ReplaceOption) error {
	if path == "" {
		return errors.New("registry: DeleteFile: path must be set")
	}

	var o replaceOpts
	for _, opt := range opts {
		opt(&o)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: DeleteFile %q: begin: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Deletion must honour the same committed-run guard as ReplaceFile.
	// An earlier draft of this function omitted the check, which meant a
	// caller could erase files from the live index while being refused
	// permission to modify one — an asymmetry that would have let a bug
	// delete more than it could ever replace.
	var committed int
	err = tx.QueryRowContext(ctx, `SELECT committed FROM index_runs WHERE id = ?`, runID).Scan(&committed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registry: DeleteFile %q: %w", path, ErrRunNotFound)
	}
	if err != nil {
		return fmt.Errorf("registry: DeleteFile %q: read run: %w", path, err)
	}
	if committed == 1 && !o.allowCommitted {
		return fmt.Errorf("registry: DeleteFile %q: run %d is committed and is not writable "+
			"(pass registry.AllowCommittedRun() for incremental updates)", path, runID)
	}

	// deleteFileRows removes FTS rows (which have no foreign-key cascade,
	// being an FTS5 virtual table), edges evidenced by this file, and the file
	// row itself — whose cascade takes its symbols and any edge incident to
	// them.
	if err := deleteFileRows(ctx, tx, runID, path); err != nil {
		return fmt.Errorf("registry: DeleteFile %q: %w", path, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: DeleteFile %q: commit: %w", path, err)
	}
	return nil
}
