// Package registry is RepoCortex's write layer: it owns index runs, the
// two-phase atomic swap that publishes them, and the deterministic digest that
// proves two indexing passes over the same tree produced the same content.
//
// Three invariants are enforced here, and no code path in this package may
// relax them:
//
//  1. Every edge carries provenance. model.Edge.Validate runs before every
//     edge write. There is no way to record a relationship through this
//     package without also recording how it was determined.
//
//  2. confidence=exact requires source=compiler. Checked in Go on the way in
//     *and* by a SQL CHECK in migration 0001. Two independent checks is
//     deliberate: a bug in one writer must not be able to turn a guess into a
//     claim.
//
//  3. Readers only ever see a committed run. index_runs.committed gates
//     visibility and a partial unique index guarantees at most one committed
//     run exists. A crash mid-index can therefore leave garbage behind, but it
//     can never expose it.
//
// Concurrency model: SQLite is a single-writer store, so at most one Writer
// may be open per database at a time, and a Writer is not safe for concurrent
// use by multiple goroutines. Readers may run throughout an indexing pass —
// that is precisely why writes are broken into bounded batches instead of one
// long transaction (see writer.go).
package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ErrNoCommittedRun is returned by readers when the store has never had a run
// committed. It is a normal state (a freshly initialised index), not a fault,
// so callers are expected to test for it rather than treat it as corruption.
var ErrNoCommittedRun = errors.New("registry: no committed index run")

// ErrRunNotFound means the requested index_runs row does not exist.
var ErrRunNotFound = errors.New("registry: index run not found")

// Registry is the write and read facade over the registry tables.
//
// It holds no mutable state of its own; all coordination happens in SQLite, so
// a Registry value is safe to share between goroutines (subject to the
// single-writer rule above).
type Registry struct {
	db *store.DB

	// now is injectable so tests can control started/finished timestamps.
	// Timestamps deliberately never feed the digest — see digest.go — but they
	// do drive abandoned-run reporting.
	now func() int64

	// batchSize bounds how many rows a Writer accumulates before committing.
	// See writer.go for why this must stay bounded.
	batchSize int

	// producerVersion stamps each run with the build that wrote it, so an index
	// produced by different resolution rules is detectable rather than silently
	// wrong. Empty means unknown, which is what every run written before
	// migration 0007 honestly is.
	//
	// Injected rather than read from the binary here because the version lives
	// in the CLI layer, and an adapter must not depend upward on it
	// (CLAUDE.md §1).
	producerVersion string
}

// Option configures a Registry at construction.
//
// Variadic so the existing New(db) calls keep compiling — a constructor change
// that touches every call site to add one optional field is churn, not design.
type Option func(*Registry)

// WithProducerVersion records which build produced the runs this registry
// writes. See migration 0008 for why over-reporting staleness is the safe
// direction.
func WithProducerVersion(v string) Option {
	return func(r *Registry) { r.producerVersion = v }
}

// DefaultBatchSize is the number of rows a Writer buffers per transaction.
//
// The value trades two costs against each other: SQLite's per-transaction
// commit overhead (which wants large batches) and how long a write transaction
// blocks concurrent readers on a single connection (which wants small ones).
// 1000 puts commit overhead well under 1% of insert cost while keeping any
// single write transaction in the low-millisecond range.
const DefaultBatchSize = 1000

// New returns a Registry over db. db must already be migrated.
func New(db *store.DB, opts ...Option) *Registry {
	r := &Registry{
		db:        db,
		now:       func() int64 { return time.Now().Unix() },
		batchSize: DefaultBatchSize,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// BeginRun opens a staging index run.
//
// The row lands with status='indexing' and committed=0, which makes everything
// subsequently written against it invisible to readers. Nothing about this run
// becomes visible until CommitRun swaps it in.
func (r *Registry) BeginRun(ctx context.Context, accuracyMode, gitRev string) (*model.IndexRun, error) {
	if accuracyMode == "" {
		accuracyMode = "auto"
	}
	switch accuracyMode {
	case "fast", "auto", "exact":
	default:
		// Caught by a SQL CHECK too, but a typo deserves a readable error
		// rather than a constraint violation from three layers down.
		return nil, fmt.Errorf("registry: invalid accuracy_mode %q (want fast|auto|exact)", accuracyMode)
	}

	started := r.now()
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO index_runs (status, committed, started_unix, git_revision, accuracy_mode, producer_version)
		 VALUES ('indexing', 0, ?, ?, ?, ?)`,
		started, nullString(gitRev), accuracyMode, r.producerVersion)
	if err != nil {
		return nil, fmt.Errorf("registry: begin run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("registry: begin run: read id: %w", err)
	}
	return &model.IndexRun{
		ID:              id,
		Status:          model.StatusIndexing,
		StartedUnix:     started,
		GitRevision:     gitRev,
		AccuracyMode:    accuracyMode,
		ProducerVersion: r.producerVersion,
	}, nil
}

// Completeness is what a caller knows about a run's coverage and the registry
// does not.
//
// Plain scalars, and a registry-owned struct rather than a pipeline type, for
// the reason CLAUDE.md §1 gives: the dependency runs pipeline -> registry, and
// an adapter that imported the layer above it to name its own parameter would
// invert the arrow. The pipeline translates its Reconciliation into this; the
// registry never learns what a Reconciliation is.
type Completeness struct {
	// FilesDiscovered is what discovery handed the pipeline.
	FilesDiscovered int64

	// FilesUnaccounted is how many of those reached none of the terminal
	// outcomes. May be negative, which means a file was counted twice.
	FilesUnaccounted int64

	// State is the verdict. Empty is treated as complete.
	State model.IndexState
}

// Partial reports whether this records a shortfall.
func (c Completeness) Partial() bool { return c.State == model.IndexStatePartial }

// CommitOption adjusts what CommitRun records. Functional options rather than a
// wider signature (CLAUDE.md §2): completeness is knowledge some callers have
// and some genuinely do not, and a caller that cannot measure coverage must not
// be forced to invent a number for it.
type CommitOption func(*commitConfig)

type commitConfig struct {
	completeness *Completeness
}

// WithCompleteness stamps the run with the caller's coverage proof.
//
// Without it CommitRun preserves whatever RecordCompleteness already stamped on
// the staging row, and falls back to "complete, discovered == indexed" for a
// run that measured nothing. That fallback is a statement of ignorance, not a
// claim: it is exactly what the pre-completeness schema said, so an old caller
// is no worse off than it was.
func WithCompleteness(c Completeness) CommitOption {
	return func(cfg *commitConfig) { cfg.completeness = &c }
}

// CommitRun publishes runID and retires whatever was published before it.
//
// Everything happens in one transaction, so from a reader's point of view the
// index changes from "entirely the old content" to "entirely the new content"
// with no observable state in between. That is the whole point of the two-
// phase design: a crash at any instant either loses the new run completely or
// publishes it completely.
//
// Statement order inside the transaction matters. The partial unique index
// idx_index_runs_current forbids two rows with committed=1, and SQLite checks
// unique indexes per statement, not at commit. So the previous run must be
// un-committed *before* the new one is committed. The ordering is invisible
// outside the transaction.
//
// Completeness is recorded here too, and deliberately in the same statement
// that publishes the run: a row that is visible but not yet stamped would be an
// index claiming completeness it has not proved, for however long the second
// UPDATE took to arrive.
func (r *Registry) CommitRun(ctx context.Context, runID int64, opts ...CommitOption) error {
	var cfg commitConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: commit run %d: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	var committed int
	// The staged completeness comes back with the lifecycle fields: the run may
	// already have been stamped by RecordCompleteness while it was being
	// written, and publishing must not quietly erase that.
	staged := Completeness{}
	var stagedState string
	err = tx.QueryRowContext(ctx,
		`SELECT status, committed, files_discovered, index_state, files_unaccounted
		   FROM index_runs WHERE id = ?`, runID).
		Scan(&status, &committed, &staged.FilesDiscovered, &stagedState, &staged.FilesUnaccounted)
	staged.State = model.IndexState(stagedState)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registry: commit run %d: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return fmt.Errorf("registry: commit run %d: read run: %w", runID, err)
	}
	if committed == 1 {
		return fmt.Errorf("registry: commit run %d: already committed", runID)
	}
	if status != string(model.StatusIndexing) {
		return fmt.Errorf("registry: commit run %d: status is %q, want %q", runID, status, model.StatusIndexing)
	}

	if err := validateRun(ctx, tx, runID); err != nil {
		return err
	}

	counts, err := countRun(ctx, tx, runID)
	if err != nil {
		return err
	}

	// The digest is computed over the rows about to become visible and stored
	// with them, so a later determinism check compares like with like without
	// having to re-derive it from a tree that may have moved on.
	dg, err := digest(ctx, tx, runID)
	if err != nil {
		return err
	}

	var prevID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM index_runs WHERE committed = 1`).Scan(&prevID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registry: commit run %d: find current run: %w", runID, err)
	}
	if prevID.Valid && prevID.Int64 == runID {
		return fmt.Errorf("registry: commit run %d: already the committed run", runID)
	}

	if prevID.Valid {
		if _, err := tx.ExecContext(ctx,
			`UPDATE index_runs SET committed = 0 WHERE id = ?`, prevID.Int64); err != nil {
			return fmt.Errorf("registry: commit run %d: uncommit run %d: %w", runID, prevID.Int64, err)
		}
	}

	// A run with parse failures is still worth publishing — a partial index
	// beats none — but it must not claim to be complete. Spec §37.
	newStatus := model.StatusReady
	if counts.ParseFailures > 0 {
		newStatus = model.StatusDegraded
	}

	// Completeness is recorded alongside, never instead. A run can be degraded
	// AND partial: files that failed to parse are accounted for, files that
	// vanished are not, and the two facts are about different failures.
	//
	// The run is still published when it is partial. A partial index beats
	// none, and the atomic swap keeps its meaning either way — what changes is
	// that the row now says atomic is not the same as complete, so a reader can
	// stop treating "the query returned nothing" as "the repository contains
	// nothing".
	final := staged
	if cfg.completeness != nil {
		final = *cfg.completeness
	}
	if final.State == "" {
		final.State = model.IndexStateComplete
	}
	if final.FilesDiscovered == 0 && final.State == model.IndexStateComplete {
		// Nobody measured discovery. Saying "we found as many as we stored" is
		// the same claim the schema made before this column existed, and it
		// keeps the invariant discovered >= indexed true for old callers.
		final.FilesDiscovered = counts.Files
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE index_runs
		    SET committed = 1, status = ?, finished_unix = ?,
		        files_indexed = ?, symbols_found = ?, edges_found = ?, parse_failures = ?,
		        registry_digest = ?,
		        files_discovered = ?, index_state = ?, files_unaccounted = ?
		  WHERE id = ?`,
		string(newStatus), r.now(),
		counts.Files, counts.Symbols, counts.Edges, counts.ParseFailures,
		dg,
		final.FilesDiscovered, string(final.State), final.FilesUnaccounted,
		runID); err != nil {
		return fmt.Errorf("registry: commit run %d: publish: %w", runID, err)
	}

	if prevID.Valid {
		if err := deleteRunRows(ctx, tx, prevID.Int64); err != nil {
			return fmt.Errorf("registry: commit run %d: retire run %d: %w", runID, prevID.Int64, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: commit run %d: commit: %w", runID, err)
	}
	return nil
}

// AbortRun discards a staging run and everything written against it.
//
// Safe to call on a run that was never written to, and safe to call twice; the
// second call reports ErrRunNotFound. Refuses to touch a committed run — that
// would be a deletion of live data, which is CommitRun's job and only ever as
// part of a swap.
func (r *Registry) AbortRun(ctx context.Context, runID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: abort run %d: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var committed int
	err = tx.QueryRowContext(ctx, `SELECT committed FROM index_runs WHERE id = ?`, runID).Scan(&committed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("registry: abort run %d: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return fmt.Errorf("registry: abort run %d: read run: %w", runID, err)
	}
	if committed == 1 {
		return fmt.Errorf("registry: abort run %d: refusing to abort the committed run", runID)
	}

	if err := deleteRunRows(ctx, tx, runID); err != nil {
		return fmt.Errorf("registry: abort run %d: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: abort run %d: commit: %w", runID, err)
	}
	return nil
}

// ReclaimAbandoned deletes every uncommitted run and returns the ids removed.
//
// This is the crash-recovery entry point. A process killed mid-index leaves a
// staging run and up to batchSize rows of partially written content behind;
// they are invisible to readers but they are not free, and a long-lived index
// would otherwise accumulate one carcass per crash.
//
// It must be called when no Writer is open — at startup, before BeginRun —
// because it cannot distinguish a run abandoned by a dead process from one
// that a live process is still filling. Callers get the same single-writer
// guarantee SQLite already requires of them, so this is not a new constraint.
func (r *Registry) ReclaimAbandoned(ctx context.Context) ([]int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: reclaim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM index_runs WHERE committed = 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("registry: reclaim: list runs: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("registry: reclaim: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("registry: reclaim: scan: %w", err)
	}
	_ = rows.Close()

	for _, id := range ids {
		if err := deleteRunRows(ctx, tx, id); err != nil {
			return nil, fmt.Errorf("registry: reclaim run %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("registry: reclaim: commit: %w", err)
	}
	return ids, nil
}

// deleteRunRows removes a run and all content hanging off it.
//
// files/symbols/edges/files_fts_map cascade from index_runs via ON DELETE
// CASCADE (which is why store.applyPragmas verifies foreign_keys actually took
// effect). files_fts does NOT: it is an FTS5 virtual table, so it has no
// foreign key to cascade through and its rows would be orphaned — silently
// polluting every future search with hits on deleted files. It is therefore
// deleted explicitly, first, while the mapping rows that identify it still
// exist.
func deleteRunRows(ctx context.Context, tx *sql.Tx, runID int64) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM files_fts
		  WHERE rowid IN (
		        SELECT m.fts_rowid FROM files_fts_map m
		          JOIN files f ON f.id = m.file_id
		         WHERE f.index_run_id = ?)`, runID); err != nil {
		return fmt.Errorf("delete fts rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM index_runs WHERE id = ?`, runID); err != nil {
		return fmt.Errorf("delete run: %w", err)
	}
	return nil
}

// Counts are the row totals for one run.
type Counts struct {
	Files         int64
	Symbols       int64
	Edges         int64
	ParseFailures int64
}

func countRun(ctx context.Context, q queryer, runID int64) (Counts, error) {
	var c Counts
	err := q.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM files   WHERE index_run_id = ?),
		       (SELECT count(*) FROM symbols WHERE index_run_id = ?),
		       (SELECT count(*) FROM edges   WHERE index_run_id = ?),
		       (SELECT count(*) FROM files   WHERE index_run_id = ? AND parse_status IN ('failed','partial'))`,
		runID, runID, runID, runID).
		Scan(&c.Files, &c.Symbols, &c.Edges, &c.ParseFailures)
	if err != nil {
		return c, fmt.Errorf("registry: count run %d: %w", runID, err)
	}
	return c, nil
}

// validateRun re-checks the graph invariants across the whole staging run just
// before it becomes visible.
//
// Every one of these is also enforced on the way in, per row. Re-checking here
// is not redundancy for its own sake: the per-row checks live in the writer,
// and this is the gate that a *future* writer — or a hand-run SQL statement
// during debugging — cannot get around. A run that fails here is never
// published.
func validateRun(ctx context.Context, q queryer, runID int64) error {
	// Invariant 2, in SQL. The schema CHECK covers this on INSERT; asserting it
	// again here means the gate still holds if a later migration ever relaxes
	// the constraint.
	var bad int64
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM edges WHERE index_run_id = ? AND confidence = 'exact' AND source <> 'compiler'`,
		runID).Scan(&bad); err != nil {
		return fmt.Errorf("registry: validate run %d: exact-edge check: %w", runID, err)
	}
	if bad > 0 {
		return fmt.Errorf("registry: run %d has %d edge(s) with confidence=exact but source<>compiler", runID, bad)
	}

	// Cross-run references. The schema's foreign keys guarantee an edge points
	// at symbols that exist, but NOT that they belong to the same run — nothing
	// in migration 0001 ties edges.index_run_id to symbols.index_run_id. An
	// edge straddling two runs would be destroyed by the very next swap, so it
	// is treated as corruption and blocks the commit.
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM edges e
		  JOIN symbols s ON s.id = e.src_id
		 WHERE e.index_run_id = ? AND s.index_run_id <> e.index_run_id`, runID).Scan(&bad); err != nil {
		return fmt.Errorf("registry: validate run %d: src-run check: %w", runID, err)
	}
	if bad > 0 {
		return fmt.Errorf("registry: run %d has %d edge(s) whose src symbol belongs to another run", runID, bad)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM edges e
		  JOIN symbols s ON s.id = e.dst_id
		 WHERE e.index_run_id = ? AND s.index_run_id <> e.index_run_id`, runID).Scan(&bad); err != nil {
		return fmt.Errorf("registry: validate run %d: dst-run check: %w", runID, err)
	}
	if bad > 0 {
		return fmt.Errorf("registry: run %d has %d edge(s) whose dst symbol belongs to another run", runID, bad)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND f.index_run_id <> s.index_run_id`, runID).Scan(&bad); err != nil {
		return fmt.Errorf("registry: validate run %d: symbol-file-run check: %w", runID, err)
	}
	if bad > 0 {
		return fmt.Errorf("registry: run %d has %d symbol(s) whose file belongs to another run", runID, bad)
	}
	return nil
}

// queryer is the read surface shared by *sql.DB and *sql.Tx, so validation and
// digesting can run either standalone or inside the commit transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// StampProducer records that runID has been brought up to date by this build.
//
// The full-index path stamps at BeginRun because it creates the run. The
// incremental path does not create a run — it updates the committed one in place
// — so without this the stamp on a pre-existing index stays stale forever.
//
// That is not cosmetic. The differ treats a version mismatch as "every file is
// dirty" (see pipeline.diffAgainst), so an incremental pass that re-parsed the
// whole repository under the new build and then failed to record the fact would
// re-parse the whole repository again on the next run, and every run after that.
// The correctness fix would have become a permanent full re-index.
//
// Called only after a pass completes successfully. Stamping a run that failed
// part-way would claim the current build's rules for files the current build
// never touched.
func (r *Registry) StampProducer(ctx context.Context, runID int64, version string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE index_runs SET producer_version = ? WHERE id = ?`, version, runID)
	if err != nil {
		return fmt.Errorf("registry: stamp producer on run %d: %w", runID, err)
	}
	return nil
}

// StampGitRevision records the commit a run's contents now describe.
//
// Same shape as StampProducer, and for the same reason: only the full path
// creates a run, so only the full path stamps at BeginRun. An incremental pass
// updates the committed run in place, and without this the run keeps naming
// whichever commit it was FIRST built at — while its contents describe the
// working tree as of the latest pass.
//
// That gap is not cosmetic either, because provenance is a claim this product
// makes to a reader and to an agent. `head_matches_build` compares this field
// against HEAD; a run whose contents are current but whose stamp is three
// commits old reports "the index does not describe your HEAD" when it does, and
// a reader who is told that twice stops believing the field at all. The portal's
// index bar and the MCP provenance block both read it.
//
// Called only after a pass completes, for the same reason as StampProducer: a
// run that failed part-way must not claim a revision it did not finish indexing.
func (r *Registry) StampGitRevision(ctx context.Context, runID int64, rev string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE index_runs SET git_revision = ? WHERE id = ?`, nullString(rev), runID)
	if err != nil {
		return fmt.Errorf("registry: stamp git revision on run %d: %w", runID, err)
	}
	return nil
}
