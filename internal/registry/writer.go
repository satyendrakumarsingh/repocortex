package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Writer inserts files, symbols, edges and search rows into one staging run.
//
// Why it is shaped this way. Indexing a mid-sized repository writes hundreds of
// thousands of rows. Two naive designs both fail:
//
//   - One INSERT per implicit transaction. SQLite fsyncs per commit, so this is
//     one or two orders of magnitude too slow to be usable.
//
//   - One transaction spanning the whole run. Fast, but it holds the write lock
//     for as long as parsing takes — minutes — during which no other writer can
//     make progress and, on a single-connection store, readers stall too. The
//     spec forbids holding a long write transaction while parsing for exactly
//     this reason.
//
// So the Writer keeps a transaction open across at most batchSize rows and then
// commits and starts a fresh one. Insert cost is amortised across the batch
// while no single transaction lives longer than a few milliseconds. The
// partially written run is not a hazard: it is uncommitted, so no reader can
// see it, and ReclaimAbandoned cleans it up if the process dies.
//
// A Writer is not safe for concurrent use by multiple goroutines.
type Writer struct {
	reg   *Registry
	runID int64

	tx  *sql.Tx
	st  *writerStmts
	n   int // rows written into the current transaction
	max int

	// failed latches the first error. Once a statement inside a batch fails the
	// transaction is doomed, and letting the caller keep adding rows would turn
	// one clear error into a pile of confusing ones.
	//
	// RollbackFile clears it, and that is the one sanctioned way: a successful
	// ROLLBACK TO SAVEPOINT proves the transaction survived, so the error was
	// local to the rows just undone rather than fatal to the batch.
	failed error
	closed bool

	// inFile is set between BeginFile and CommitFile. While it is set, rotate
	// does not commit: a savepoint cannot span a commit, so the batch boundary
	// has to wait for the file boundary. Files are tens of rows, so the
	// deferral is bounded by one file rather than by the repository.
	inFile     bool
	rotateDue  bool
	fileFailed bool
}

type writerStmts struct {
	file   *sql.Stmt
	symbol *sql.Stmt
	edge   *sql.Stmt
	fts    *sql.Stmt
	ftsMap *sql.Stmt
}

// NewWriter opens a Writer against a staging run.
//
// The run must exist and still be uncommitted; writing into a committed run
// would mutate what readers are currently looking at, which the two-phase
// design exists to prevent.
func (r *Registry) NewWriter(ctx context.Context, runID int64) (*Writer, error) {
	var committed int
	err := r.db.QueryRowContext(ctx, `SELECT committed FROM index_runs WHERE id = ?`, runID).Scan(&committed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("registry: writer for run %d: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("registry: writer for run %d: %w", runID, err)
	}
	if committed == 1 {
		return nil, fmt.Errorf("registry: writer for run %d: run is committed and is not writable", runID)
	}
	return &Writer{reg: r, runID: runID, max: r.batchSize}, nil
}

// RunID reports the staging run this Writer targets.
func (w *Writer) RunID() int64 { return w.runID }

// AddFile inserts a file row and returns its rowid, which is also written back
// into f.ID so the caller can attach symbols to it.
func (w *Writer) AddFile(ctx context.Context, f *model.File) (int64, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if f.Path == "" {
		return 0, w.fail(errors.New("registry: file path must be set"))
	}
	if f.ParseStatus == "" {
		f.ParseStatus = "ok"
	}
	if err := w.ensureTx(ctx); err != nil {
		return 0, err
	}
	res, err := w.st.file.ExecContext(ctx,
		w.runID, f.Path, f.Language, f.ContentHash, f.SizeBytes, f.ModTimeUnix,
		f.ParseStatus, nullString(f.ParseError))
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert file %q: %w", f.Path, err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert file %q: read id: %w", f.Path, err))
	}
	f.ID = id
	return id, w.rotate(ctx)
}

// AddSymbol inserts a symbol and returns its rowid, written back into s.ID.
//
// Invariant 2 is applied to symbols as well as edges even though the schema
// only CHECKs it for edges: "exact" means a compiler proved it, and that is a
// claim about the fact, not about which table it lives in.
func (w *Writer) AddSymbol(ctx context.Context, s *model.Symbol) (int64, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if err := validateSymbol(s); err != nil {
		return 0, w.fail(err)
	}
	if err := w.ensureTx(ctx); err != nil {
		return 0, err
	}
	res, err := w.st.symbol.ExecContext(ctx,
		w.runID, s.FileID, string(s.Kind), s.Name, s.FQN,
		s.Range.Start.Line, s.Range.Start.Col, s.Range.End.Line, s.Range.End.Col,
		nullString(s.Signature), nullString(s.Doc), boolToInt(s.Exported),
		string(s.Confidence), string(s.Source), int(s.ParamCount))
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert symbol %q: %w", s.FQN, err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert symbol %q: read id: %w", s.FQN, err))
	}
	s.ID = id
	return id, w.rotate(ctx)
}

// AddEdge inserts a relationship after validating its provenance.
//
// model.Edge.Validate is called on every single edge — invariant 1. There is no
// bulk path that skips it, because "just this once, for speed" is how a graph
// full of unattributed guesses gets built.
func (w *Writer) AddEdge(ctx context.Context, e *model.Edge) (int64, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if err := e.Validate(); err != nil {
		return 0, w.fail(fmt.Errorf("registry: %w", err))
	}
	if e.Evidence.Source == "" {
		return 0, w.fail(fmt.Errorf("registry: edge %s %d -> %d: evidence source must be set",
			e.Kind, e.SrcID, e.DstID))
	}
	if err := w.ensureTx(ctx); err != nil {
		return 0, err
	}
	res, err := w.st.edge.ExecContext(ctx,
		w.runID, e.SrcID, e.DstID, string(e.Kind), string(e.Confidence),
		e.Evidence.FilePath, e.Evidence.Range.Start.Line, e.Evidence.Range.Start.Col,
		string(e.Evidence.Source), nullString(e.Evidence.Detail))
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert edge %s %d -> %d: %w", e.Kind, e.SrcID, e.DstID, err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: insert edge %s: read id: %w", e.Kind, err))
	}
	e.ID = id
	e.IndexRunID = w.runID
	return id, w.rotate(ctx)
}

// IndexFileText populates the full-text search rows for a file.
//
// content is stored verbatim so snippet() returns real source. The `expanded`
// column holds search.ExpandIdentifiers(content), which is what lets a query
// for "callback" find handleCallback: FTS5's unicode61 tokenizer treats an
// identifier as one opaque token, so without the expansion a bare-word query —
// the most common shape in code search — silently misses.
func (w *Writer) IndexFileText(ctx context.Context, fileID int64, path, content string) error {
	if w.failed != nil {
		return w.failed
	}
	if fileID == 0 {
		return w.fail(errors.New("registry: IndexFileText: file id must be set"))
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	res, err := w.st.fts.ExecContext(ctx, path, content, search.ExpandIdentifiers(content))
	if err != nil {
		return w.fail(fmt.Errorf("registry: index text for %q: %w", path, err))
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		return w.fail(fmt.Errorf("registry: index text for %q: read rowid: %w", path, err))
	}
	// files_fts is a plain (non external-content) FTS5 table, so nothing links
	// its rowid back to a real file. That is what files_fts_map is for, and it
	// must be written in the same transaction or a crash between the two leaves
	// an unattributable search hit.
	if _, err := w.st.ftsMap.ExecContext(ctx, rowid, fileID); err != nil {
		return w.fail(fmt.Errorf("registry: map fts row for %q: %w", path, err))
	}
	return w.rotate(ctx)
}

// Flush commits any buffered rows. Call it before CommitRun.
//
// The context is CHECKED, not merely accepted. A method that takes a ctx and
// ignores it is worse than one that takes none: it advertises cancellation the
// caller does not get, and this is the commit point of a batched writer — the
// one place a cancelled index run must not quietly persist a partial batch.
func (w *Writer) Flush(ctx context.Context) error {
	if w.failed != nil {
		return w.failed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("registry: flush cancelled: %w", err)
	}
	return w.commitTx()
}

// Close releases the Writer, rolling back anything not yet flushed.
//
// Safe to defer unconditionally: after a successful Flush there is nothing left
// to roll back, and on the error path it guarantees the last partial batch is
// discarded rather than left behind holding a lock.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.tx != nil {
		err := w.tx.Rollback()
		w.tx, w.st, w.n = nil, nil, 0
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			return err
		}
	}
	return nil
}

// ensureTx opens a transaction and prepares the insert statements if none is
// open. Statements are prepared per transaction because SQLite closes a Tx's
// statements with the Tx; at batchSize rows apiece the preparation cost is a
// rounding error against the inserts it serves.
func (w *Writer) ensureTx(ctx context.Context) error {
	if w.closed {
		return errors.New("registry: writer is closed")
	}
	if w.tx != nil {
		return nil
	}
	tx, err := w.reg.db.BeginTx(ctx, nil)
	if err != nil {
		return w.fail(fmt.Errorf("registry: begin batch: %w", err))
	}
	st, err := prepareWriterStmts(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return w.fail(err)
	}
	w.tx, w.st, w.n = tx, st, 0
	return nil
}

func (w *Writer) rotate(ctx context.Context) error {
	w.n++
	if w.n < w.max {
		return nil
	}
	if w.inFile {
		// Committing here would discard the savepoint protecting this file.
		// Remember that a rotation is owed and take it at CommitFile.
		w.rotateDue = true
		return nil
	}
	// Checked here too, and this is where it matters most: rotate is called
	// once per row, so it is the loop a cancelled index run has to escape
	// from. Accepting the ctx and ignoring it made every Add* method advertise
	// a cancellation that never arrived.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("registry: batch cancelled: %w", err)
	}
	return w.commitTx()
}

func (w *Writer) commitTx() error {
	if w.tx == nil {
		return nil
	}
	tx := w.tx
	w.tx, w.st, w.n = nil, nil, 0
	if err := tx.Commit(); err != nil {
		return w.fail(fmt.Errorf("registry: commit batch: %w", err))
	}
	return nil
}

// fileSavepoint names the savepoint guarding one file's rows. A constant is
// enough: the pipeline funnels every write through one Writer, and savepoints
// nest by name only when deliberately reused.
const fileSavepoint = "repocortex_file"

// BeginFile opens a savepoint around one file's rows.
//
// This is what stops a single file from destroying an index. Extraction can
// produce rows the schema rejects — two declarations that differ only in a
// column the UNIQUE constraint omits, for example — and before this the failed
// statement latched an error, poisoned the writer and aborted the whole run.
// On a real 8,094-file repository that meant one line of one TypeScript test
// left nothing committed at all.
//
// Parser failures were already isolated per file, as the Parser contract
// requires. Persistence failures were not, and a defect that only shows up on
// an unusual language or an unusual project is exactly the kind this layer
// cannot enumerate in advance.
func (w *Writer) BeginFile(ctx context.Context) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, "SAVEPOINT "+fileSavepoint); err != nil {
		return w.fail(fmt.Errorf("registry: begin file: %w", err))
	}
	w.inFile, w.fileFailed = true, false
	return nil
}

// RollbackFile undoes every row written since BeginFile and returns the batch
// to a usable state.
//
// A successful rollback is the proof that the failure was local: the
// transaction is still alive, so the latched error described those rows and
// not the batch. If the rollback ITSELF fails, nothing has been proven and the
// error stays latched, because at that point the transaction really is doomed.
func (w *Writer) RollbackFile(ctx context.Context) error {
	if !w.inFile {
		return nil
	}
	w.inFile = false
	if w.tx == nil {
		return w.failed
	}
	if _, err := w.tx.ExecContext(ctx, "ROLLBACK TO "+fileSavepoint); err != nil {
		return w.fail(fmt.Errorf("registry: rollback file: %w", err))
	}
	if _, err := w.tx.ExecContext(ctx, "RELEASE "+fileSavepoint); err != nil {
		return w.fail(fmt.Errorf("registry: release file: %w", err))
	}
	w.failed = nil
	w.fileFailed = true
	return nil
}

// CommitFile releases the savepoint and takes any rotation the file deferred.
func (w *Writer) CommitFile(ctx context.Context) error {
	if !w.inFile {
		return nil
	}
	w.inFile = false
	if w.failed != nil {
		return w.failed
	}
	if w.tx != nil {
		if _, err := w.tx.ExecContext(ctx, "RELEASE "+fileSavepoint); err != nil {
			return w.fail(fmt.Errorf("registry: commit file: %w", err))
		}
	}
	if w.rotateDue {
		w.rotateDue = false
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("registry: batch cancelled: %w", err)
		}
		return w.commitTx()
	}
	return nil
}

func (w *Writer) fail(err error) error {
	if w.failed == nil {
		w.failed = err
	}
	return err
}

func prepareWriterStmts(ctx context.Context, tx *sql.Tx) (*writerStmts, error) {
	var st writerStmts
	prep := func(dst **sql.Stmt, q string) error {
		s, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("registry: prepare: %w", err)
		}
		*dst = s
		return nil
	}
	if err := prep(&st.file, `
		INSERT INTO files (index_run_id, path, language, content_hash, size_bytes,
		                   mod_time_unix, parse_status, parse_error)
		VALUES (?,?,?,?,?,?,?,?)`); err != nil {
		return nil, err
	}
	if err := prep(&st.symbol, `
		INSERT INTO symbols (index_run_id, file_id, kind, name, fqn,
		                     start_line, start_col, end_line, end_col,
		                     signature, doc, exported, confidence, source, param_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`); err != nil {
		return nil, err
	}
	if err := prep(&st.edge, `
		INSERT INTO edges (index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, evidence_col, source, detail)
		VALUES (?,?,?,?,?,?,?,?,?,?)`); err != nil {
		return nil, err
	}
	if err := prep(&st.fts, `INSERT INTO files_fts (path, content, expanded) VALUES (?,?,?)`); err != nil {
		return nil, err
	}
	if err := prep(&st.ftsMap, `INSERT INTO files_fts_map (fts_rowid, file_id) VALUES (?,?)`); err != nil {
		return nil, err
	}
	return &st, nil
}

func validateSymbol(s *model.Symbol) error {
	if s.FileID == 0 {
		return fmt.Errorf("registry: symbol %q: file id must be set", s.FQN)
	}
	if s.FQN == "" {
		return errors.New("registry: symbol: fqn must be set")
	}
	if s.Kind == "" {
		return fmt.Errorf("registry: symbol %q: kind must be set", s.FQN)
	}
	if !s.Confidence.Valid() {
		return fmt.Errorf("registry: symbol %q: invalid confidence %q", s.FQN, s.Confidence)
	}
	if s.Source == "" {
		return fmt.Errorf("registry: symbol %q: source must be set", s.FQN)
	}
	// Invariant 2 applied to symbols. Migration 0001 CHECKs this for edges only,
	// so without this line a symbol could claim compiler-grade certainty with no
	// compiler behind it and nothing would object.
	if s.Confidence == model.ConfidenceExact && s.Source != model.SourceCompiler {
		return fmt.Errorf("registry: symbol %q: confidence=exact requires source=compiler, got %q",
			s.FQN, s.Source)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
