package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// CurrentRun returns the committed run, or ErrNoCommittedRun if there is none.
//
// This is the only entry point a reader should use to discover which run to
// query. Everything else in the store may be a half-built staging run or the
// debris of a crash; committed=1 is the sole visibility gate (invariant 3).
func (r *Registry) CurrentRun(ctx context.Context) (*model.IndexRun, error) {
	return scanRun(r.db.QueryRowContext(ctx, runSelect+` WHERE committed = 1`))
}

// StagingRun returns the newest uncommitted run still marked 'indexing', or
// (nil, nil) when there is none.
//
// Readers never query a staging run — committed=1 stays the sole visibility
// gate. This exists so a reader can SAY that one exists: "no index yet" and
// "the first index is still being built" call for different actions, and an
// answer served from run N while run N+1 is being built may describe code that
// has since changed.
//
// It cannot tell a live run from one abandoned by a crashed process (see
// ReclaimAbandoned); callers word their message to cover both.
func (r *Registry) StagingRun(ctx context.Context) (*model.IndexRun, error) {
	run, err := scanRun(r.db.QueryRowContext(ctx,
		runSelect+` WHERE committed = 0 AND status = 'indexing' ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, ErrNoCommittedRun) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: staging run: %w", err)
	}
	return run, nil
}

// Run returns a run by id regardless of its committed state. Intended for
// operational tooling (status, doctor) — not for query paths.
func (r *Registry) Run(ctx context.Context, id int64) (*model.IndexRun, error) {
	run, err := scanRun(r.db.QueryRowContext(ctx, runSelect+` WHERE id = ?`, id))
	if errors.Is(err, ErrNoCommittedRun) {
		return nil, ErrRunNotFound
	}
	return run, err
}

// Counts returns the row totals for a run, computed from the rows themselves
// rather than read back from the summary columns, so it stays honest if a
// writer ever miscounts.
func (r *Registry) Counts(ctx context.Context, runID int64) (Counts, error) {
	return countRun(ctx, r.db, runID)
}

const runSelect = `
	SELECT id, status, committed, started_unix, COALESCE(finished_unix,0),
	       COALESCE(git_revision,''), accuracy_mode,
	       files_indexed, symbols_found, edges_found, parse_failures,
	       COALESCE(registry_digest,''), COALESCE(error,''),
	       files_discovered, index_state, files_unaccounted,
	       producer_version
	  FROM index_runs`

func scanRun(row *sql.Row) (*model.IndexRun, error) {
	var run model.IndexRun
	var status string
	var committed int
	var indexState string
	err := row.Scan(&run.ID, &status, &committed, &run.StartedUnix, &run.FinishedUnix,
		&run.GitRevision, &run.AccuracyMode, &run.FilesIndexed, &run.SymbolsFound,
		&run.EdgesFound, &run.ParseFailures, &run.RegistryDigest, &run.Error,
		&run.FilesDiscovered, &indexState, &run.FilesUnaccounted,
		&run.ProducerVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCommittedRun
	}
	if err != nil {
		return nil, fmt.Errorf("registry: read run: %w", err)
	}
	run.Status = model.IndexStatus(status)
	run.IndexState = model.IndexState(indexState)
	return &run, nil
}

// Files lists a run's files ordered by path.
func (r *Registry) Files(ctx context.Context, runID int64) ([]model.File, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, path, language, content_hash, size_bytes, mod_time_unix,
		       parse_status, COALESCE(parse_error,''), detected_by
		  FROM files WHERE index_run_id = ? ORDER BY path`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: list files: %w", err)
	}
	defer rows.Close()

	var out []model.File
	for rows.Next() {
		var f model.File
		if err := rows.Scan(&f.ID, &f.Path, &f.Language, &f.ContentHash, &f.SizeBytes,
			&f.ModTimeUnix, &f.ParseStatus, &f.ParseError, &f.DetectedBy); err != nil {
			return nil, fmt.Errorf("registry: list files: scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FilesWithoutText lists a run's files that have no full-text row — skipped
// as too large or binary, or redacted — ordered by path. SearchFiles cannot
// see them; a caller that must account for every file reads these directly.
func (r *Registry) FilesWithoutText(ctx context.Context, runID int64) ([]model.File, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.id, f.path, f.language, f.content_hash, f.size_bytes, f.mod_time_unix,
		       f.parse_status, COALESCE(f.parse_error,'')
		  FROM files f
		 WHERE f.index_run_id = ?
		   AND NOT EXISTS (SELECT 1 FROM files_fts_map m WHERE m.file_id = f.id)
		 ORDER BY f.path`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: list files without text: %w", err)
	}
	defer rows.Close()

	var out []model.File
	for rows.Next() {
		var f model.File
		if err := rows.Scan(&f.ID, &f.Path, &f.Language, &f.ContentHash, &f.SizeBytes,
			&f.ModTimeUnix, &f.ParseStatus, &f.ParseError); err != nil {
			return nil, fmt.Errorf("registry: list files without text: scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Symbols lists a run's symbols ordered by file path then position.
func (r *Registry) Symbols(ctx context.Context, runID int64) ([]model.Symbol, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.kind, s.name, s.fqn,
		       s.start_line, s.start_col, s.end_line, s.end_col,
		       COALESCE(s.signature,''), COALESCE(s.doc,''), s.exported, s.confidence, s.source,
		       s.param_count
		  FROM symbols s JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind <> 'file'
		 ORDER BY f.path, s.start_line, s.fqn`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: list symbols: %w", err)
	}
	defer rows.Close()

	var out []model.Symbol
	for rows.Next() {
		var s model.Symbol
		var kind, conf, src string
		var exported, params int
		if err := rows.Scan(&s.ID, &s.FileID, &kind, &s.Name, &s.FQN,
			&s.Range.Start.Line, &s.Range.Start.Col, &s.Range.End.Line, &s.Range.End.Col,
			&s.Signature, &s.Doc, &exported, &conf, &src, &params); err != nil {
			return nil, fmt.Errorf("registry: list symbols: scan: %w", err)
		}
		// The stored value is the Arity encoding itself (zero = unknown).
		s.ParamCount = model.Arity(params)
		s.Kind = model.EntityKind(kind)
		s.Confidence = model.Confidence(conf)
		s.Source = model.SourceType(src)
		s.Exported = exported == 1
		out = append(out, s)
	}
	return out, rows.Err()
}

// Edges lists a run's edges with their provenance intact. An edge is never
// returned without its confidence and evidence — dropping them at the read
// boundary would defeat the point of storing them.
//
// Edges that start at a file's synthetic top-level caller (FileCallerKind) are
// left out: the consumers of this list build their nodes from Symbols, which
// does not hold that symbol, so the edge would have no source to draw. The
// caller queries read the edges table by destination and do return them, and
// TopLevelEdges returns them to a reader that attributes an edge by its file.
func (r *Registry) Edges(ctx context.Context, runID int64) ([]model.Edge, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, src_id, dst_id, kind, confidence,
		       evidence_file, evidence_line, evidence_col, source, COALESCE(detail,'')
		  FROM edges WHERE index_run_id = ?
		   AND NOT EXISTS (SELECT 1 FROM symbols fc WHERE fc.id = edges.src_id AND fc.kind = 'file')
		 ORDER BY src_id, dst_id, kind, evidence_file, evidence_line`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: list edges: %w", err)
	}
	defer rows.Close()

	var out []model.Edge
	for rows.Next() {
		var e model.Edge
		var kind, conf, src string
		if err := rows.Scan(&e.ID, &e.SrcID, &e.DstID, &kind, &conf,
			&e.Evidence.FilePath, &e.Evidence.Range.Start.Line, &e.Evidence.Range.Start.Col,
			&src, &e.Evidence.Detail); err != nil {
			return nil, fmt.Errorf("registry: list edges: scan: %w", err)
		}
		e.Kind = model.EdgeKind(kind)
		e.Confidence = model.Confidence(conf)
		e.Evidence.Source = model.SourceType(src)
		e.IndexRunID = runID
		out = append(out, e)
	}
	return out, rows.Err()
}

// supertypeChunk is how many type names go into one supertype lookup.
//
// Matched to candidateChunk for the same reason: SQLite's default parameter
// limit is 32,766, and the caller asks about one bounded frontier at a time, so
// the only thing this ever guards against is a pathological chunk. The cost of
// an extra round trip is trivial next to a statement that fails to prepare.
const supertypeChunk = 512

// maxSupertypesPerName caps how many supertypes are reported for one NAME.
//
// The bound is per name rather than per type because a name is what the caller
// can ask about: `Handler` may be declared by fifty classes in a repository,
// and this query unions what all of them extend. Sixty-four is far above any
// real union — Java allows one superclass and a handful of interfaces — while
// keeping a query on a degenerate name bounded (CLAUDE.md §4.4). The rows kept
// are the alphabetically first, so which ones survive the cap does not depend
// on the query plan.
const maxSupertypesPerName = 64

// Supertypes returns the direct supertypes of each named type in one run.
//
// This is the read behind the Ancestry port that internal/resolve/infer
// declares (see inherited.go): `Payment` -> `[Auditable, BasePayment]`, so the
// deferred inherited-dispatch pass can walk up a hierarchy. It answers in
// NAMES, not rows or symbol ids, because a name is the whole question the port
// asks — leaking a *sql.Rows or a driver value outward is what CLAUDE.md §1/§2
// hold this adapter to.
//
// "Supertype" means both channels, EXTENDS and IMPLEMENTS: a method reached
// through either is inherited as far as a call site is concerned. Both come
// from the same place — an inheritance reference bound by resolution, whose
// enclosing symbol is the declaring type — so this query is only meaningful
// AFTER a run's references have been resolved and its edges written.
//
// Run-scoped like every other read here. A supertype from a staging run
// reaching a run being resolved would be a visibility leak (invariant 3), and
// the edge table holds every run at once.
//
// Contract, which the port depends on:
//
//   - Keys are the requested names VERBATIM. Matching is exact, not folded:
//     both sides are the source's own spelling of the type — the parser rewrites
//     a receiver to its DECLARED type — and a NOCASE comparison would also give
//     up idx_symbols_name.
//   - A type with no supertypes is absent from the map rather than present with
//     an empty slice; the port states the two mean the same thing.
//   - Each slice is deduplicated and ordered by name, so the same index answers
//     identically on every run and across worker counts. The deferred pass walks
//     these slices, and the registry digest depends on the edges that walk
//     produces — an order that varied with the query plan would silently break
//     the determinism gate.
func (r *Registry) Supertypes(ctx context.Context, runID int64, types []string) (map[string][]string, error) {
	return r.supertypes(ctx, runID, types, false)
}

// HeuristicSupertypes is Supertypes as the heuristic resolver may read it: an
// inheritance edge a compiler alone established is left out.
//
// WHY. The heuristic pass of a full index runs BEFORE any compiler index is
// imported, so the hierarchy it sees holds only what it bound itself. An
// incremental update runs it AFTER, with every compiler edge of the untouched
// files already in the table; reading those would let the changed file bind a
// call through an ancestor the same file bound to nothing in a full index, and
// the two indexes of one tree would differ. An edge the heuristic bound and a
// compiler later confirmed is still there (it keeps its prior confidence), so
// it still counts.
func (r *Registry) HeuristicSupertypes(ctx context.Context, runID int64, types []string) (map[string][]string, error) {
	return r.supertypes(ctx, runID, types, true)
}

func (r *Registry) supertypes(ctx context.Context, runID int64, types []string, heuristicOnly bool) (map[string][]string, error) {
	if len(types) == 0 {
		return map[string][]string{}, nil
	}

	// Deduplicate in first-appearance order. A slice, not a map range: what is
	// asked for must not depend on map iteration either.
	seen := make(map[string]struct{}, len(types))
	want := make([]string, 0, len(types))
	for _, t := range types {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		want = append(want, t)
	}

	out := make(map[string][]string, len(want))
	for start := 0; start < len(want); start += supertypeChunk {
		end := min(start+supertypeChunk, len(want))
		if err := r.supertypeChunkInto(ctx, runID, want[start:end], out, heuristicOnly); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// supertypeSQL is the statement supertypeChunkInto runs for n names; a function
// so the plan test can explain the very text that runs.
//
// The joins are CROSS JOINs on purpose: in SQLite that keeps the written order,
// which starts from the few symbols with these names (idx_symbols_name) and
// follows each one's edges (the unique index on edges). Left to itself the
// planner, which has no statistics here, starts from the edges table instead
// and walks every edge of the run for each chunk, then throws away the ones
// whose source name is not in the list: on a 1.5-million-edge index that was
// a full scan per call, hundreds of calls per resolve.
func supertypeSQL(n int) string { return supertypeSQLFor(n, false) }

// supertypeSQLFor is supertypeSQL, optionally without the edges only a compiler
// established (see HeuristicSupertypes). prior_confidence is empty for an edge
// a compiler inserted and holds the heuristic tier of one it upgraded.
func supertypeSQLFor(n int, heuristicOnly bool) string {
	only := ""
	if heuristicOnly {
		only = " AND (e.source <> 'compiler' OR e.prior_confidence <> '')"
	}
	return `
		SELECT src_name, dst_name FROM (
		  SELECT src_name, dst_name,
		         ROW_NUMBER() OVER (PARTITION BY src_name ORDER BY dst_name) AS rn
		    FROM (SELECT DISTINCT src.name AS src_name, dst.name AS dst_name
		            FROM symbols src
		            CROSS JOIN edges   e   ON e.src_id = src.id AND e.index_run_id = ?
		            CROSS JOIN symbols dst ON dst.id = e.dst_id AND dst.index_run_id = ?
		           WHERE src.index_run_id = ?
		             AND src.name IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)
		             AND e.kind IN (?,?)` + only + `))
		 WHERE rn <= ?
		 ORDER BY src_name, dst_name`
}

// supertypeChunkInto reads one bounded chunk of names into out.
//
// DISTINCT before ROW_NUMBER, not after: the cap must count distinct supertype
// NAMES, so that a type extended by one parent declared in three files does not
// spend three of its sixty-four slots on the same answer.
func (r *Registry) supertypeChunkInto(
	ctx context.Context,
	runID int64,
	names []string,
	out map[string][]string,
	heuristicOnly bool,
) error {
	args := make([]any, 0, len(names)+6)
	args = append(args, runID, runID, runID)
	for _, n := range names {
		args = append(args, n)
	}
	args = append(args, string(model.EdgeExtends), string(model.EdgeImplements), maxSupertypesPerName)

	rows, err := r.db.QueryContext(ctx, supertypeSQLFor(len(names), heuristicOnly), args...)
	if err != nil {
		return fmt.Errorf("registry: load supertypes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return fmt.Errorf("registry: load supertypes: scan: %w", err)
		}
		if parent == "" {
			continue
		}
		out[child] = append(out[child], parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: load supertypes: %w", err)
	}
	return nil
}

// FileMatch is one full-text hit.
type FileMatch struct {
	FileID int64   `json:"file_id"`
	Path   string  `json:"path"`
	Score  float64 `json:"score"` // FTS5 bm25; more negative is a better match
}

// SearchFiles runs a full-text query against the committed run only.
//
// The join through files_fts_map to files is what enforces invariant 3 on the
// search path: files_fts holds rows for whatever runs happen to exist, so
// without the join a query could surface a file from a staging run that no
// reader is supposed to see yet.
//
// The query is matched against both `content` and `expanded`, so a bare word
// finds identifiers that merely contain it.
func (r *Registry) SearchFiles(ctx context.Context, query string, limit int) ([]FileMatch, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.id, f.path, bm25(files_fts)
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f         ON f.id = m.file_id
		  JOIN index_runs r    ON r.id = f.index_run_id AND r.committed = 1
		 WHERE files_fts MATCH ?
		 ORDER BY bm25(files_fts), f.path
		 LIMIT ?`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: search: %w", err)
	}
	defer rows.Close()

	var out []FileMatch
	for rows.Next() {
		var m FileMatch
		if err := rows.Scan(&m.FileID, &m.Path, &m.Score); err != nil {
			return nil, fmt.Errorf("registry: search: scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
