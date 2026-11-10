package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ReplaceOption tunes ReplaceFile.
type ReplaceOption func(*replaceOpts)

type replaceOpts struct {
	content        string
	hasContent     bool
	allowCommitted bool
}

// WithContent supplies the file's text so its full-text rows are rebuilt.
//
// Without it ReplaceFile still deletes the file's stale FTS rows — leaving them
// would make search return hits against source that no longer exists — but it
// cannot repopulate them, so the file drops out of search until the next full
// index. Watchers that have the bytes in hand should always pass this.
func WithContent(text string) ReplaceOption {
	return func(o *replaceOpts) { o.content, o.hasContent = text, true }
}

// ReplaceFile atomically swaps one file's contribution to a staging run.
//
// This is the incremental path: one file changed on disk, so everything that
// file previously contributed must go and be replaced, in a single transaction.
// Either the whole file's new state lands or none of it does; a half-replaced
// file would be a graph containing symbols from one revision and edges from
// another.
//
// Stale-edge removal is the subtle part, and there are two distinct kinds of
// staleness:
//
//   - Edges *incident* to the file's symbols. Deleting the old file row
//     cascades to its symbols, which cascades to every edge whose src or dst
//     was one of them. This catches inbound edges as well as outbound.
//
//   - Edges *produced by parsing* the file, whose endpoints both live
//     elsewhere — an import wiring two other modules together, say. Nothing
//     cascades to those, and they would survive as claims sourced from a
//     version of the file that no longer exists. They are identified by
//     evidence_file and deleted explicitly. This is why evidence is not
//     decoration: it is also the ownership record that makes incremental
//     update correct.
//
// Note the deliberate asymmetry: inbound edges from *other* files into this
// file's symbols are deleted and not recreated here, because only the resolver
// that reparsed those other files can know whether they still point anywhere.
// Dropping a real edge degrades recall; keeping one that points at a deleted
// symbol corrupts the graph.
//
// Symbol identity for edges: the caller cannot know the rowids of symbols that
// do not exist yet, so symbols in the replacement set are addressed by a
// caller-assigned negative placeholder in Symbol.ID, and edges referring to
// those placeholders are remapped to the real rowids. A positive SrcID/DstID is
// taken as an existing rowid in this run and used as-is.
func (r *Registry) ReplaceFile(
	ctx context.Context,
	runID int64,
	file *model.File,
	symbols []model.Symbol,
	edges []model.Edge,
	opts ...ReplaceOption,
) error {
	if file == nil || file.Path == "" {
		return errors.New("registry: ReplaceFile: file path must be set")
	}
	var o replaceOpts
	for _, opt := range opts {
		opt(&o)
	}

	// Validate everything before touching the database. A rejected edge should
	// leave the previous state of the file intact, not a hole where it was.
	for i := range symbols {
		if err := validateReplacementSymbol(&symbols[i]); err != nil {
			return err
		}
	}
	placeholders := map[int64]int{}
	for i := range symbols {
		if symbols[i].ID < 0 {
			if _, dup := placeholders[symbols[i].ID]; dup {
				return fmt.Errorf("registry: ReplaceFile: duplicate placeholder id %d", symbols[i].ID)
			}
			placeholders[symbols[i].ID] = i
		}
	}
	for i := range edges {
		// Invariant 1: no edge reaches storage without passing Validate. Run it
		// on the caller's placeholder ids too — Validate only rejects zero ids,
		// and every other check (kind, confidence, exact-implies-compiler) is
		// independent of how the endpoints are addressed.
		if err := edges[i].Validate(); err != nil {
			return fmt.Errorf("registry: ReplaceFile: %w", err)
		}
		if edges[i].SrcID < 0 {
			if _, ok := placeholders[edges[i].SrcID]; !ok {
				return fmt.Errorf("registry: ReplaceFile: edge src placeholder %d has no symbol", edges[i].SrcID)
			}
		}
		if edges[i].DstID < 0 {
			if _, ok := placeholders[edges[i].DstID]; !ok {
				return fmt.Errorf("registry: ReplaceFile: edge dst placeholder %d has no symbol", edges[i].DstID)
			}
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: begin: %w", file.Path, err)
	}
	defer func() { _ = tx.Rollback() }()

	var committed int
	err = tx.QueryRowContext(ctx, `SELECT committed FROM index_runs WHERE id = ?`, runID).Scan(&committed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registry: ReplaceFile %q: %w", file.Path, ErrRunNotFound)
	}
	if err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: read run: %w", file.Path, err)
	}
	if committed == 1 && !o.allowCommitted {
		// Full indexing must never mutate the live run: that is what the
		// two-phase swap exists for, and an interrupted full index has to
		// leave the previous revision untouched.
		//
		// Incremental indexing opts in with AllowCommittedRun, which trades
		// cross-file consistency during a pass for update cost proportional to
		// what changed. See inplace.go for why that trade is the right one and
		// what it does not give up (per-file atomicity is preserved).
		return fmt.Errorf("registry: ReplaceFile %q: run %d is committed and is not writable "+
			"(pass registry.AllowCommittedRun() for incremental updates)", file.Path, runID)
	}

	if err := deleteFileRows(ctx, tx, runID, file.Path); err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: %w", file.Path, err)
	}

	st, err := prepareWriterStmts(ctx, tx)
	if err != nil {
		return err
	}

	res, err := st.file.ExecContext(ctx, runID, file.Path, file.Language, file.ContentHash,
		file.SizeBytes, file.ModTimeUnix, defaultParseStatus(file.ParseStatus), nullString(file.ParseError))
	if err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: insert file: %w", file.Path, err)
	}
	fileID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: read file id: %w", file.Path, err)
	}
	file.ID = fileID

	realID := make(map[int64]int64, len(placeholders))
	for i := range symbols {
		s := &symbols[i]
		placeholder := s.ID
		res, err := st.symbol.ExecContext(ctx,
			runID, fileID, string(s.Kind), s.Name, s.FQN,
			s.Range.Start.Line, s.Range.Start.Col, s.Range.End.Line, s.Range.End.Col,
			nullString(s.Signature), nullString(s.Doc), boolToInt(s.Exported),
			string(s.Confidence), string(s.Source), int(s.ParamCount))
		if err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: insert symbol %q: %w", file.Path, s.FQN, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: read symbol id: %w", file.Path, err)
		}
		s.ID = id
		s.FileID = fileID
		if placeholder < 0 {
			realID[placeholder] = id
		}
	}

	for i := range edges {
		e := &edges[i]
		src, dst := e.SrcID, e.DstID
		if id, ok := realID[src]; ok {
			src = id
		}
		if id, ok := realID[dst]; ok {
			dst = id
		}
		if _, err := st.edge.ExecContext(ctx,
			runID, src, dst, string(e.Kind), string(e.Confidence),
			e.Evidence.FilePath, e.Evidence.Range.Start.Line, e.Evidence.Range.Start.Col,
			string(e.Evidence.Source), nullString(e.Evidence.Detail)); err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: insert edge %s: %w", file.Path, e.Kind, err)
		}
		e.SrcID, e.DstID, e.IndexRunID = src, dst, runID
	}

	if o.hasContent {
		res, err := st.fts.ExecContext(ctx, file.Path, o.content, search.ExpandIdentifiers(o.content))
		if err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: index text: %w", file.Path, err)
		}
		rowid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: read fts rowid: %w", file.Path, err)
		}
		if _, err := st.ftsMap.ExecContext(ctx, rowid, fileID); err != nil {
			return fmt.Errorf("registry: ReplaceFile %q: map fts row: %w", file.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: ReplaceFile %q: commit: %w", file.Path, err)
	}
	return nil
}

// deleteFileRows removes one file's previous contribution to a run: its FTS
// rows, the edges it produced with endpoints elsewhere, and the file row itself
// (whose cascade takes the symbols, and through them every incident edge).
func deleteFileRows(ctx context.Context, tx *sql.Tx, runID int64, path string) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM files_fts
		 WHERE rowid IN (
		       SELECT m.fts_rowid FROM files_fts_map m
		         JOIN files f ON f.id = m.file_id
		        WHERE f.index_run_id = ? AND f.path = ?)`, runID, path); err != nil {
		return fmt.Errorf("delete fts rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges WHERE index_run_id = ? AND evidence_file = ?`, runID, path); err != nil {
		return fmt.Errorf("delete edges evidenced by file: %w", err)
	}
	// Un-resolve references that were bound to a symbol in this file, BEFORE
	// the file — and so its symbols — are deleted.
	//
	// symbol_references.resolved_symbol_id is ON DELETE SET NULL, which clears
	// the pointer but leaves resolution = 'resolved'. Resolve only visits
	// references marked 'pending', so a reference orphaned this way was never
	// looked at again: the row claimed to be resolved while pointing at
	// nothing, and the call edge stayed missing until a full re-index.
	//
	// Editing a definition therefore silently deleted its callers' edges.
	// `cortex arch` reported "No violations" on a repo that had one, which
	// for a governance command is the worst possible failure — a false clean
	// bill of health is acted on, an error is investigated.
	//
	// This must run before the DELETE: afterwards the cascade has already
	// erased the link, and nothing identifies which references to repair.
	//
	// The provenance columns from 0013 are cleared with it. A row reset to
	// 'pending' has no resolution left to have a source for, and leaving
	// resolution_source = 'compiler' behind would let a later read report that
	// a typechecker bound a reference that is currently bound to nothing.
	if _, err := tx.ExecContext(ctx, `
		UPDATE symbol_references
		   SET resolution = 'pending', resolved_symbol_id = NULL,
		       resolution_source = '', prior_resolution = ''
		 WHERE resolved_symbol_id IN (
		       SELECT s.id FROM symbols s
		         JOIN files f ON f.id = s.file_id
		        WHERE f.index_run_id = ? AND f.path = ?)`, runID, path); err != nil {
		return fmt.Errorf("un-resolve references into file: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM files WHERE index_run_id = ? AND path = ?`, runID, path); err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	return nil
}

// validateReplacementSymbol is validateSymbol without the FileID requirement:
// ReplaceFile assigns the file id itself, since the file row it belongs to is
// created by the same call.
func validateReplacementSymbol(s *model.Symbol) error {
	saved := s.FileID
	s.FileID = 1
	err := validateSymbol(s)
	s.FileID = saved
	return err
}

func defaultParseStatus(s string) string {
	if s == "" {
		return "ok"
	}
	return s
}
