package registry

import (
	"context"
	"fmt"
	"strings"
)

// ResolutionSourceRuledOut marks an unbound reference that a compiler indexer
// resolved, at that position, to something that is not defined in this
// repository. The reference keeps the resolver's own verdict in `resolution`;
// this value says that a compiler looked at the same spot and found something
// else, which is what lets an answer stop counting it as a possible use of a
// definition that happens to share the name.
const ResolutionSourceRuledOut = "compiler_other"

// ResolutionSourceInstalled marks an unbound reference at a position where a
// compiler indexer resolved the repository's OWN package to an installed copy
// instead of this checkout (pallets/click's tests run against the click in
// site-packages, a different version). What the compiler said there describes
// that copy: it neither proves the reference is a use of this repository's
// definition nor that it is not, so the reference stays unbound and counted. The
// mark says why, so an answer can tell the reader what to change.
const ResolutionSourceInstalled = "compiler_installed"

// ClearCompilerProof forgets what an earlier import of compiler indexes
// recorded for the run: the files it covered and the references it ruled out.
// An import writes both again, so a second resolve describes the indexes it
// has now and never the ones it had before.
func (r *Registry) ClearCompilerProof(ctx context.Context, runID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: clear compiler proof: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM compiler_coverage WHERE index_run_id = ?`, runID); err != nil {
		return fmt.Errorf("registry: clear compiler coverage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE symbol_references SET resolution_source = ''
		 WHERE index_run_id = ? AND resolution_source IN (?, ?)`, runID,
		ResolutionSourceRuledOut, ResolutionSourceInstalled); err != nil {
		return fmt.Errorf("registry: clear ruled-out references: %w", err)
	}
	return tx.Commit()
}

// RecordCompilerCoverage records that tool read each of the files, named by
// repository path. A path with no indexed file is ignored; the number recorded
// is returned.
func (r *Registry) RecordCompilerCoverage(ctx context.Context, runID int64, tool string, paths []string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("registry: record compiler coverage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO compiler_coverage (index_run_id, file_id, tool)
		SELECT ?, id, ? FROM files WHERE index_run_id = ? AND path = ?`)
	if err != nil {
		return 0, fmt.Errorf("registry: record compiler coverage: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	n := 0
	for _, p := range paths {
		res, err := stmt.ExecContext(ctx, runID, tool, runID, p)
		if err != nil {
			return n, fmt.Errorf("registry: record compiler coverage of %s: %w", p, err)
		}
		if c, err := res.RowsAffected(); err == nil {
			n += int(c)
		}
	}
	return n, tx.Commit()
}

// RefSite is one reference row as the proof pass needs it: where it is, what
// it is spelled as and whether anything bound it.
type RefSite struct {
	ID   int64
	Path string
	Line int
	// Col is the 0-based column the parser recorded: the start of the call
	// expression, which for a call with no receiver is the callee's own column.
	Col int
	// ReceiverKind is what the call was written against (model.ReceiverBare for
	// none), as the parser classified it.
	ReceiverKind string
	Name         string
	Resolved     bool
	// ByCompiler is true when a compiler settled the reference itself.
	ByCompiler bool
}

// CoveredReferences streams the references of every file a compiler indexer
// covered in this run, in file order, to fn. It stops at the first error fn
// returns.
func (r *Registry) CoveredReferences(ctx context.Context, runID int64, fn func(RefSite) error) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT sr.id, f.path, sr.start_line, sr.start_col, sr.receiver_kind, sr.name, sr.resolution = 'resolved', sr.resolution_source = 'compiler'
		  FROM compiler_coverage c
		  JOIN files f ON f.id = c.file_id
		  JOIN symbol_references sr ON sr.file_id = f.id
		 WHERE c.index_run_id = ? AND sr.index_run_id = ? AND sr.start_line > 0
		 ORDER BY f.path, sr.start_line, sr.id`, runID, runID)
	if err != nil {
		return fmt.Errorf("registry: references of covered files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s RefSite
		if err := rows.Scan(&s.ID, &s.Path, &s.Line, &s.Col, &s.ReceiverKind, &s.Name, &s.Resolved, &s.ByCompiler); err != nil {
			return fmt.Errorf("registry: references of covered files: scan: %w", err)
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MarkRuledOut stamps the given references as ruled out by a compiler indexer
// (ResolutionSourceRuledOut). An unbound reference keeps the resolver's verdict
// and gains the stamp. A reference the RESOLVER bound is moved to 'unresolved'
// and loses its binding, because the compiler resolved that very position to
// something this repository does not define: the binding was a name match with
// a stranger, and `json.Valid(...)` beside a `Valid` method is the case. The
// resolver's verdict stays in prior_resolution, which is what the accuracy
// report scores it by. A reference a compiler itself bound is left alone.
//
// It returns how many references were stamped and, of those, how many had been
// bound.
func (r *Registry) MarkRuledOut(ctx context.Context, ids []int64) (marked, unbound int, err error) {
	const chunk = 500
	for len(ids) > 0 {
		n := min(chunk, len(ids))
		ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
		args := make([]any, 0, n+1)
		args = append(args, ResolutionSourceRuledOut)
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		res, err := r.db.ExecContext(ctx, `
			UPDATE symbol_references
			   SET prior_resolution   = CASE WHEN prior_resolution = '' THEN resolution
			                                 ELSE prior_resolution END,
			       resolution         = 'unresolved',
			       resolved_symbol_id = NULL,
			       resolution_source  = ?
			 WHERE resolution = 'resolved' AND resolution_source <> 'compiler' AND id IN (`+ph+`)`, args...)
		if err != nil {
			return marked, unbound, fmt.Errorf("registry: mark bound references ruled out: %w", err)
		}
		if c, err := res.RowsAffected(); err == nil {
			marked += int(c)
			unbound += int(c)
		}
		res, err = r.db.ExecContext(ctx, `
			UPDATE symbol_references
			   SET prior_resolution   = CASE WHEN prior_resolution = '' THEN resolution
			                                 ELSE prior_resolution END,
			       resolution         = 'unresolved',
			       resolution_source  = ?
			 WHERE resolution <> 'resolved' AND resolution_source = '' AND id IN (`+ph+`)`, args...)
		if err != nil {
			return marked, unbound, fmt.Errorf("registry: mark references ruled out: %w", err)
		}
		if c, err := res.RowsAffected(); err == nil {
			marked += int(c)
		}
		ids = ids[n:]
	}
	return marked, unbound, nil
}

// MarkInstalledCopy stamps the given unbound references as seen by a compiler
// against an installed copy of the repository's own package
// (ResolutionSourceInstalled). A reference that is bound, or that something else
// already settled, is left alone. It returns how many were stamped.
func (r *Registry) MarkInstalledCopy(ctx context.Context, ids []int64) (int, error) {
	const chunk = 500
	marked := 0
	for len(ids) > 0 {
		n := min(chunk, len(ids))
		ph := strings.TrimSuffix(strings.Repeat("?,", n), ",")
		args := make([]any, 0, n+1)
		args = append(args, ResolutionSourceInstalled)
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		res, err := r.db.ExecContext(ctx, `
			UPDATE symbol_references
			   SET prior_resolution   = CASE WHEN prior_resolution = '' THEN resolution
			                                 ELSE prior_resolution END,
			       resolution         = 'unresolved',
			       resolution_source  = ?
			 WHERE resolution <> 'resolved' AND resolution_source = '' AND id IN (`+ph+`)`, args...)
		if err != nil {
			return marked, fmt.Errorf("registry: mark references seen against an installed copy: %w", err)
		}
		if c, err := res.RowsAffected(); err == nil {
			marked += int(c)
		}
		ids = ids[n:]
	}
	return marked, nil
}

// CheckedLanguage is how much of one language's code a compiler stands
// behind.
type CheckedLanguage struct {
	Language string `json:"language"`
	Files    int64  `json:"files"`
	// Checked is the files a compiler read: the documents of an imported
	// compiler index, or for Go the files the type check wrote an edge in.
	Checked int64 `json:"checked_files"`
	// ExactEdges is the compiler-proven edges whose evidence is in those files.
	ExactEdges int64 `json:"exact_edges"`
	// Tool names the compiler indexer or checker, when there is one.
	Tool string `json:"tool,omitempty"`
}

// CompilerCoverage reports, for each language with indexed files that carry
// definitions, how many files a compiler read. goDetail is the detail the Go
// type check writes on its edges; a Go file counts as read when one of them has
// its evidence there.
func (r *Registry) CompilerCoverage(ctx context.Context, runID int64, goDetail string) ([]CheckedLanguage, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.language, COUNT(*),
		       SUM(CASE WHEN c.file_id IS NOT NULL THEN 1 ELSE 0 END),
		       COALESCE(MAX(c.tool), '')
		  FROM files f
		  LEFT JOIN compiler_coverage c ON c.file_id = f.id AND c.index_run_id = f.index_run_id
		 WHERE f.index_run_id = ? AND f.language <> ''
		   AND EXISTS (SELECT 1 FROM symbols s WHERE s.file_id = f.id AND s.kind <> 'file')
		 GROUP BY f.language ORDER BY f.language`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: compiler coverage: %w", err)
	}
	var out []CheckedLanguage
	for rows.Next() {
		var l CheckedLanguage
		if err := rows.Scan(&l.Language, &l.Files, &l.Checked, &l.Tool); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("registry: compiler coverage: scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	for i := range out {
		l := &out[i]
		switch l.Language {
		case "go":
			l.Checked, l.ExactEdges, l.Tool = 0, 0, ""
			if err := r.db.QueryRowContext(ctx, `
				SELECT count(DISTINCT evidence_file), count(*) FROM edges
				 WHERE index_run_id = ? AND source = 'compiler' AND detail = ?`,
				runID, goDetail).Scan(&l.Checked, &l.ExactEdges); err != nil {
				return nil, fmt.Errorf("registry: count type-checked edges: %w", err)
			}
			if l.Checked > 0 {
				l.Tool = "go/types"
			}
		default:
			if l.Checked == 0 {
				continue
			}
			if err := r.db.QueryRowContext(ctx, `
				SELECT count(*) FROM edges e
				 WHERE e.index_run_id = ? AND e.source = 'compiler'
				   AND e.evidence_file IN (
				       SELECT f.path FROM files f JOIN compiler_coverage c ON c.file_id = f.id
				        WHERE f.index_run_id = ? AND f.language = ?)`,
				runID, runID, l.Language).Scan(&l.ExactEdges); err != nil {
				return nil, fmt.Errorf("registry: count compiler edges of %s: %w", l.Language, err)
			}
		}
	}
	return out, nil
}

// CoveredLanguages lists the languages of the files a compiler indexer covered
// in this run.
func (r *Registry) CoveredLanguages(ctx context.Context, runID int64) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT f.language FROM compiler_coverage c JOIN files f ON f.id = c.file_id
		 WHERE c.index_run_id = ? AND f.language <> '' ORDER BY f.language`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: covered languages: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("registry: covered languages: scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SymbolsNamed returns the ids of the definitions the run indexed in the file at
// path that are spelled name. A compiler definition the index holds no symbol
// for can be told from one the join merely missed only by this: a missed join
// leaves an indexed symbol of that name, which no compiler definition accounts
// for, in that very file.
func (r *Registry) SymbolsNamed(ctx context.Context, runID int64, path, name string) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id FROM files f JOIN symbols s ON s.file_id = f.id
		 WHERE f.index_run_id = ? AND f.path = ? AND s.name = ? AND s.kind <> 'file'`,
		runID, path, name)
	if err != nil {
		return nil, fmt.Errorf("registry: definitions of %q in %s: %w", name, path, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("registry: definitions of %q in %s: scan: %w", name, path, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DefinitionReadByCompiler reports whether a compiler indexer this tool ran
// read the file a symbol is defined in (compiler_coverage).
func (r *Registry) DefinitionReadByCompiler(ctx context.Context, runID, symbolID int64) (bool, error) {
	var read bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM symbols t
		                JOIN compiler_coverage c ON c.file_id = t.file_id AND c.index_run_id = ?
		               WHERE t.id = ?)`, runID, symbolID).Scan(&read)
	if err != nil {
		return false, fmt.Errorf("registry: was the file of symbol %d read by a compiler indexer: %w", symbolID, err)
	}
	return read, nil
}
