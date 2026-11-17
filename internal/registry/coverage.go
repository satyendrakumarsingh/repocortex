package registry

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// CoverageFile is one file of a coverage profile: the path as the profile
// spells it and the hit count of every instrumented line.
//
// It is the registry's own shape and not the coverage package's, so the
// registry does not depend on a report parser.
type CoverageFile struct {
	Path  string
	Lines map[int]int
}

// CoverageInput is a parsed profile ready to be stored.
type CoverageInput struct {
	ContentHash string // hash of the profile bytes
	Format      string
	GitRevision string // commit HEAD named at import, "" outside a repository
	Files       []CoverageFile
}

// CoverageImportResult says what an import matched.
type CoverageImportResult struct {
	Symbols        int      // definitions that received a row
	FilesMatched   int      // profile files that name an indexed file
	FilesUnmatched []string // profile files that match nothing, sorted
}

// CoverageProfile is the stored profile of a run.
type CoverageProfile struct {
	ContentHash    string
	Format         string
	GitRevision    string
	ImportedUnix   int64
	FilesMatched   int
	FilesUnmatched int
}

// CoverageState is how much a stored profile says about one definition.
type CoverageState int

const (
	// CoverageMeasured: the profile counted lines in this definition and its
	// file is unchanged since the import.
	CoverageMeasured CoverageState = iota
	// CoverageStale: the file changed after the import. Its numbers describe
	// other code and must not be used.
	CoverageStale
	// CoverageUnmeasured: the file is in the profile but none of this
	// definition's lines is instrumented (a declaration, an empty body).
	CoverageUnmeasured
	// CoverageNotInProfile: the profile has nothing on this definition's file.
	// A package no test was run for looks exactly like this, so it is not zero.
	CoverageNotInProfile
)

// SymbolCoverage is the stored coverage of one definition.
type SymbolCoverage struct {
	State   CoverageState
	Covered int
	Total   int
	MaxHits int
}

// coverageKinds are the definitions given a row: the ones whose line range is
// the code that runs. A type's range spans its methods, so its figure is their
// sum; a type with no executable line of its own (a Go struct) has none and
// gets no row.
const coverageKinds = `('function','method','class')`

// ImportCoverage stores a profile for the run, replacing any earlier one.
//
// Report paths are matched to indexed paths by the longest tail they share, at
// a directory boundary. A Go profile names files by import path
// ("github.com/acme/app/svc/pay.go"), LCOV and Cobertura often by absolute path,
// JaCoCo relative to a source root, and none of them agree with the repository
// root the index uses. A path that matches more than one indexed file by its
// short tail alone is left out and reported as unmatched: guessing would put
// one file's lines on another's functions.
func (r *Registry) ImportCoverage(ctx context.Context, runID int64, in CoverageInput) (CoverageImportResult, error) {
	var res CoverageImportResult
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("registry: import coverage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := clearCoverage(ctx, tx, runID); err != nil {
		return res, err
	}

	indexed, err := coveragePaths(ctx, tx, runID)
	if err != nil {
		return res, err
	}
	m := newPathMatcher(indexed)

	byPath := make(map[string]map[int]int, len(in.Files))
	for _, f := range in.Files {
		p, ok := m.match(f.Path)
		if !ok {
			res.FilesUnmatched = append(res.FilesUnmatched, f.Path)
			continue
		}
		dst := byPath[p]
		if dst == nil {
			dst = make(map[int]int, len(f.Lines))
			byPath[p] = dst
		}
		// Two report files can resolve to one indexed file (a Go profile that
		// lists a package twice, a multi-module JaCoCo report): hits add.
		for l, h := range f.Lines {
			dst[l] += h
		}
	}
	sort.Strings(res.FilesUnmatched)
	res.FilesMatched = len(byPath)

	ins, err := tx.PrepareContext(ctx,
		`INSERT INTO coverage_files (index_run_id, path, content_hash) VALUES (?, ?, ?)`)
	if err != nil {
		return res, fmt.Errorf("registry: import coverage: prepare files: %w", err)
	}
	defer func() { _ = ins.Close() }()
	for p := range byPath {
		if _, err := ins.ExecContext(ctx, runID, p, indexed[p]); err != nil {
			return res, fmt.Errorf("registry: import coverage: file %q: %w", p, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, f.path, s.start_line, s.end_line
		  FROM symbols s JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind IN `+coverageKinds+`
		   AND s.start_line > 0 AND s.end_line >= s.start_line`, runID)
	if err != nil {
		return res, fmt.Errorf("registry: import coverage: read symbols: %w", err)
	}
	type row struct {
		id                 int64
		covered, total, mx int
	}
	var out []row
	for rows.Next() {
		var id int64
		var path string
		var start, end int
		if err := rows.Scan(&id, &path, &start, &end); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("registry: import coverage: scan: %w", err)
		}
		lines, ok := byPath[path]
		if !ok {
			continue
		}
		c, t, mx := rangeCoverage(lines, start, end)
		if t > 0 {
			out = append(out, row{id, c, t, mx})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return res, fmt.Errorf("registry: import coverage: rows: %w", err)
	}
	_ = rows.Close()

	put, err := tx.PrepareContext(ctx, `
		INSERT INTO symbol_coverage (symbol_id, index_run_id, covered_lines, total_lines, max_hits)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return res, fmt.Errorf("registry: import coverage: prepare symbols: %w", err)
	}
	defer func() { _ = put.Close() }()
	for _, o := range out {
		if _, err := put.ExecContext(ctx, o.id, runID, o.covered, o.total, o.mx); err != nil {
			return res, fmt.Errorf("registry: import coverage: symbol %d: %w", o.id, err)
		}
	}
	res.Symbols = len(out)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coverage_imports
		       (index_run_id, content_hash, format, git_revision, imported_unix, files_matched, files_unmatched)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		runID, in.ContentHash, in.Format, in.GitRevision, r.now(), res.FilesMatched, len(res.FilesUnmatched)); err != nil {
		return res, fmt.Errorf("registry: import coverage: record import: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("registry: import coverage: commit: %w", err)
	}
	return res, nil
}

// rangeCoverage counts the instrumented lines in [start, end], how many ran,
// and the highest hit count. It walks the smaller of the range and the file's
// line set: a function of 20 lines in a 5,000-line profile entry must not cost
// 5,000 steps, nor a 3,000-line class 3,000 lookups in a sparse one.
func rangeCoverage(lines map[int]int, start, end int) (covered, total, maxHits int) {
	note := func(h int) {
		total++
		if h > 0 {
			covered++
		}
		if h > maxHits {
			maxHits = h
		}
	}
	if end-start+1 <= len(lines) {
		for l := start; l <= end; l++ {
			if h, ok := lines[l]; ok {
				note(h)
			}
		}
		return covered, total, maxHits
	}
	for l, h := range lines {
		if l >= start && l <= end {
			note(h)
		}
	}
	return covered, total, maxHits
}

func clearCoverage(ctx context.Context, tx *sql.Tx, runID int64) error {
	for _, q := range []string{
		`DELETE FROM symbol_coverage WHERE index_run_id = ?`,
		`DELETE FROM coverage_files WHERE index_run_id = ?`,
		`DELETE FROM coverage_imports WHERE index_run_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, runID); err != nil {
			return fmt.Errorf("registry: clear coverage: %w", err)
		}
	}
	return nil
}

// coveragePaths maps every indexed path of the run to its content hash.
func coveragePaths(ctx context.Context, tx *sql.Tx, runID int64) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path, content_hash FROM files WHERE index_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: import coverage: list files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return nil, fmt.Errorf("registry: import coverage: scan file: %w", err)
		}
		out[p] = h
	}
	return out, rows.Err()
}

// pathMatcher resolves a profile's spelling of a path to an indexed path.
type pathMatcher struct {
	indexed map[string]string
	// byTail maps each directory-aligned tail of an indexed path to the paths
	// that end with it, built on first use: only a profile that spells paths
	// shorter than the index needs it.
	byTail map[string][]string
}

func newPathMatcher(indexed map[string]string) *pathMatcher { return &pathMatcher{indexed: indexed} }

func (m *pathMatcher) match(report string) (string, bool) {
	p := strings.TrimPrefix(strings.ReplaceAll(report, `\`, "/"), "./")
	// The longest tail of the report path that is an indexed path: the Go import
	// path and an absolute path both lose their leading directories until the
	// repository-relative path is what is left.
	for t := p; t != ""; {
		if _, ok := m.indexed[t]; ok {
			return t, true
		}
		i := strings.IndexByte(t, '/')
		if i < 0 {
			break
		}
		t = t[i+1:]
	}
	// A profile relative to a source root (JaCoCo) is shorter than the index's
	// path: accept it only when exactly one indexed file ends with it.
	if m.byTail == nil {
		m.byTail = make(map[string][]string, len(m.indexed))
		for ip := range m.indexed {
			for t := ip; ; {
				m.byTail[t] = append(m.byTail[t], ip)
				i := strings.IndexByte(t, '/')
				if i < 0 {
					break
				}
				t = t[i+1:]
			}
		}
	}
	if c := m.byTail[p]; len(c) == 1 {
		return c[0], true
	}
	return "", false
}

// CoverageProfile returns the run's stored profile, or nil when none was
// imported.
func (r *Registry) CoverageProfile(ctx context.Context, runID int64) (*CoverageProfile, error) {
	var p CoverageProfile
	err := r.db.QueryRowContext(ctx, `
		SELECT content_hash, format, git_revision, imported_unix, files_matched, files_unmatched
		  FROM coverage_imports WHERE index_run_id = ?`, runID).
		Scan(&p.ContentHash, &p.Format, &p.GitRevision, &p.ImportedUnix, &p.FilesMatched, &p.FilesUnmatched)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: coverage profile: %w", err)
	}
	return &p, nil
}

// SymbolCoverage reads the stored coverage of the given definitions. Every id
// asked for is in the result; one the profile says nothing about is
// CoverageNotInProfile.
//
// Staleness is decided here: the file's hash now against the hash recorded at
// import. A changed file's definitions have new ids and no symbol_coverage row,
// so the comparison is made through the file, by path.
func (r *Registry) SymbolCoverage(ctx context.Context, ids []int64) (map[int64]SymbolCoverage, error) {
	out := make(map[int64]SymbolCoverage, len(ids))
	const chunk = 500
	for i := 0; i < len(ids); i += chunk {
		part := ids[i:min(i+chunk, len(ids))]
		args := make([]any, len(part))
		ph := make([]string, len(part))
		for j, id := range part {
			args[j], ph[j] = id, "?"
		}
		rows, err := r.db.QueryContext(ctx, `
			SELECT s.id, f.content_hash, cf.content_hash,
			       sc.covered_lines, sc.total_lines, sc.max_hits
			  FROM symbols s
			  JOIN files f ON f.id = s.file_id
			  LEFT JOIN coverage_files cf ON cf.index_run_id = s.index_run_id AND cf.path = f.path
			  LEFT JOIN symbol_coverage sc ON sc.symbol_id = s.id
			 WHERE s.id IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("registry: symbol coverage: %w", err)
		}
		for rows.Next() {
			var id int64
			var now string
			var then sql.NullString
			var c, t, mx sql.NullInt64
			if err := rows.Scan(&id, &now, &then, &c, &t, &mx); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("registry: symbol coverage: scan: %w", err)
			}
			sc := SymbolCoverage{State: CoverageNotInProfile}
			switch {
			case !then.Valid:
			case then.String != now:
				sc.State = CoverageStale
			case !t.Valid:
				sc.State = CoverageUnmeasured
			default:
				sc = SymbolCoverage{State: CoverageMeasured, Covered: int(c.Int64), Total: int(t.Int64), MaxHits: int(mx.Int64)}
			}
			out[id] = sc
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("registry: symbol coverage: rows: %w", err)
		}
	}
	return out, nil
}
