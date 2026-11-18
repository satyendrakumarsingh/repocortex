package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Reads and writes for choosing which references an incremental pass must
// resolve again. The choice itself, and why each rule is there, lives in
// internal/pipeline/reresolve.go; this file only answers its questions.
//
// Every question is answered per FILE, never per reference. A file is the
// unit resolution works in: the call that produced a variable's value is
// looked up among the calls bound in the same pass over the same file
// (symbol_references.value_line), and two references on one line share one
// edge row (the edges table's unique key has no column). Re-resolving part of
// a file gets both wrong, so a file is reset whole or not at all.

// resolverEdgeKinds are the edge kinds the reference resolver writes. Nothing
// else writes them with source 'source_code' (analysis writes EXPOSES, READS
// and the rest), which is what lets a reset remove exactly the resolver's
// output for a file and leave everything else the file produced in place.
const resolverEdgeKinds = `'CALLS','INSTANTIATES','EXTENDS','IMPLEMENTS','REFERENCES'`

// idChunk bounds how many ids or names one statement binds.
const idChunk = 500

// SymbolSignature is what resolution can see of a definition other than its
// row id: the fields of SymbolIndexEntry that a candidate is judged by. Two
// versions of a file whose signatures are equal offer every reference the
// same candidates.
type SymbolSignature struct {
	Name       string
	Kind       string
	FQN        string
	Exported   bool
	ParamCount int
	ReturnType string
}

// Key is the signature as one comparable string.
func (s SymbolSignature) Key() string {
	exp := "0"
	if s.Exported {
		exp = "1"
	}
	return strings.Join([]string{s.Name, s.Kind, s.FQN, exp, fmt.Sprint(s.ParamCount), s.ReturnType}, "\x00")
}

// FileSnapshot is a file as the committed run holds it, read before an
// incremental pass replaces or deletes it.
type FileSnapshot struct {
	Path     string
	Language string
	Symbols  []SymbolSignature
	Imports  []Import
}

// SnapshotFile reads one file's definitions and imports. A path the run does
// not hold returns ok=false.
func (r *Registry) SnapshotFile(ctx context.Context, runID int64, path string) (FileSnapshot, bool, error) {
	snap := FileSnapshot{Path: path}
	var fileID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT id, language FROM files WHERE index_run_id = ? AND path = ?`, runID, path).
		Scan(&fileID, &snap.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, false, nil
	}
	if err != nil {
		return snap, false, fmt.Errorf("registry: snapshot %s: %w", path, err)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT name, kind, fqn, exported, param_count, return_type
		  FROM symbols WHERE file_id = ? AND kind <> ?`, fileID, FileCallerKind)
	if err != nil {
		return snap, false, fmt.Errorf("registry: snapshot %s: symbols: %w", path, err)
	}
	for rows.Next() {
		var s SymbolSignature
		var exported int
		if err := rows.Scan(&s.Name, &s.Kind, &s.FQN, &exported, &s.ParamCount, &s.ReturnType); err != nil {
			_ = rows.Close()
			return snap, false, err
		}
		s.Exported = exported == 1
		snap.Symbols = append(snap.Symbols, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return snap, false, err
	}
	if snap.Imports, err = r.importsForFile(ctx, fileID); err != nil {
		return snap, false, err
	}
	return snap, true, nil
}

// InheritancePair is one supertype relation as resolution reads it: by type
// name (see Supertypes), attributed to the file whose reference stated it.
type InheritancePair struct {
	File, Kind, Sub, Super string
}

// HeuristicInheritance lists the EXTENDS and IMPLEMENTS edges the reference
// resolver wrote that are evidenced in the given files or touch a definition
// in them. Compiler edges are left out: resolution does not re-create them.
func (r *Registry) HeuristicInheritance(ctx context.Context, runID int64, paths []string) ([]InheritancePair, error) {
	var out []InheritancePair
	for start := 0; start < len(paths); start += idChunk {
		chunk := paths[start:min(start+idChunk, len(paths))]
		args := []any{runID}
		for _, p := range chunk {
			args = append(args, p)
		}
		args = append(args, runID)
		for _, p := range chunk {
			args = append(args, p)
		}
		args = append(args, runID)
		for _, p := range chunk {
			args = append(args, p)
		}
		in := placeholders(len(chunk))
		rows, err := r.db.QueryContext(ctx, `
			SELECT e.evidence_file, e.kind, s.name, d.name
			  FROM edges e
			  JOIN symbols s ON s.id = e.src_id
			  JOIN symbols d ON d.id = e.dst_id
			 WHERE e.id IN (
			       SELECT id FROM edges WHERE index_run_id = ? AND evidence_file IN (`+in+`)
			       UNION
			       SELECT e2.id FROM files f JOIN symbols x ON x.file_id = f.id
			         JOIN edges e2 ON e2.src_id = x.id
			        WHERE f.index_run_id = ? AND f.path IN (`+in+`)
			       UNION
			       SELECT e3.id FROM files f JOIN symbols x ON x.file_id = f.id
			         JOIN edges e3 ON e3.dst_id = x.id
			        WHERE f.index_run_id = ? AND f.path IN (`+in+`))
			   AND e.kind IN ('EXTENDS','IMPLEMENTS') AND e.source <> 'compiler'`, args...)
		if err != nil {
			return nil, fmt.Errorf("registry: heuristic inheritance: %w", err)
		}
		for rows.Next() {
			var p InheritancePair
			if err := rows.Scan(&p.File, &p.Kind, &p.Sub, &p.Super); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, p)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EdgeEvidenceTouching lists the files that evidence a resolver edge whose
// source or target is a definition in one of the given files.
//
// Replacing or deleting a file removes those edges by cascade. A reference
// bound to one of the file's definitions is reset to pending by the replace
// itself (deleteFileRows), but an edge can also START in the file while its
// evidence is elsewhere (`impl Trait for Type` in Rust, where Type is defined
// in another file), and nothing resets the reference behind that one.
func (r *Registry) EdgeEvidenceTouching(ctx context.Context, runID int64, paths []string) ([]string, error) {
	seen := map[string]struct{}{}
	for start := 0; start < len(paths); start += idChunk {
		chunk := paths[start:min(start+idChunk, len(paths))]
		args := []any{runID}
		for _, p := range chunk {
			args = append(args, p)
		}
		args = append(args, runID)
		for _, p := range chunk {
			args = append(args, p)
		}
		in := placeholders(len(chunk))
		rows, err := r.db.QueryContext(ctx, `
			SELECT DISTINCT e.evidence_file FROM files f JOIN symbols x ON x.file_id = f.id
			  JOIN edges e ON e.src_id = x.id
			 WHERE f.index_run_id = ? AND f.path IN (`+in+`)
			   AND e.source <> 'compiler' AND e.kind IN (`+resolverEdgeKinds+`)
			UNION
			SELECT DISTINCT e.evidence_file FROM files f JOIN symbols x ON x.file_id = f.id
			  JOIN edges e ON e.dst_id = x.id
			 WHERE f.index_run_id = ? AND f.path IN (`+in+`)
			   AND e.source <> 'compiler' AND e.kind IN (`+resolverEdgeKinds+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("registry: edge evidence touching files: %w", err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				_ = rows.Close()
				return nil, err
			}
			seen[p] = struct{}{}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return sortedKeys(seen), nil
}

// FileRef is a file id with its path and language.
type FileRef struct {
	ID       int64
	Path     string
	Language string
}

// RunFiles lists every file of a run, by path.
func (r *Registry) RunFiles(ctx context.Context, runID int64) ([]FileRef, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, path, language FROM files WHERE index_run_id = ? ORDER BY path`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: run files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FileRef
	for rows.Next() {
		var f FileRef
		if err := rows.Scan(&f.ID, &f.Path, &f.Language); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FilesWithPendingReferences lists the files holding a reference no pass has
// decided yet.
func (r *Registry) FilesWithPendingReferences(ctx context.Context, runID int64) ([]int64, error) {
	return r.fileIDs(ctx, `
		SELECT DISTINCT file_id FROM symbol_references
		 WHERE index_run_id = ? AND resolution = 'pending'`, runID)
}

// FilesReferencingNames lists the files holding a reference, in any outcome,
// written with one of the given names. Exact spelling: idx_refs_run_name.
func (r *Registry) FilesReferencingNames(ctx context.Context, runID int64, names []string) ([]int64, error) {
	return r.chunkedFileIDs(ctx, names, func(n int) string {
		return `SELECT DISTINCT file_id FROM symbol_references
		         WHERE index_run_id = ? AND name IN (` + placeholders(n) + `)`
	}, runID)
}

// FilesReferencingNamesFolded is FilesReferencingNames ignoring case, over the
// files of the given languages only: the languages whose names resolution
// folds (infer.LanguageSpec.FoldNames). lowered must be lower-case.
func (r *Registry) FilesReferencingNamesFolded(ctx context.Context, runID int64, languages, lowered []string) ([]int64, error) {
	if len(languages) == 0 {
		return nil, nil
	}
	pre := []any{runID}
	for _, l := range languages {
		pre = append(pre, l)
	}
	return r.chunkedFileIDs(ctx, lowered, func(n int) string {
		return `SELECT DISTINCT sr.file_id FROM files f JOIN symbol_references sr ON sr.file_id = f.id
		         WHERE f.index_run_id = ? AND f.language IN (` + placeholders(len(languages)) + `)
		           AND lower(sr.name) IN (` + placeholders(n) + `)`
	}, pre...)
}

// DistinctReceivers lists every receiver a reference of the run is written
// on. Read from idx_refs_run_receiver (0025), so it costs one pass over that
// index, not over the table.
func (r *Registry) DistinctReceivers(ctx context.Context, runID int64) ([]string, error) {
	return r.strings(ctx, `
		SELECT DISTINCT receiver FROM symbol_references INDEXED BY idx_refs_run_receiver
		 WHERE index_run_id = ? AND receiver <> ''`, runID)
}

// FilesWithReceivers lists the files holding a reference written on one of
// the given receivers, exactly as spelled.
func (r *Registry) FilesWithReceivers(ctx context.Context, runID int64, receivers []string) ([]int64, error) {
	return r.chunkedFileIDs(ctx, receivers, func(n int) string {
		return `SELECT DISTINCT file_id FROM symbol_references INDEXED BY idx_refs_run_receiver
		         WHERE index_run_id = ? AND receiver <> '' AND receiver IN (` + placeholders(n) + `)`
	}, runID)
}

// ImportRow is one distinct import written in a run.
type ImportRow struct {
	Path, Alias string
}

// DistinctImportRows lists every distinct (path, alias) a file of the run
// imports.
func (r *Registry) DistinctImportRows(ctx context.Context, runID int64) ([]ImportRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT path, alias FROM file_imports WHERE index_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: distinct imports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ImportRow
	for rows.Next() {
		var i ImportRow
		if err := rows.Scan(&i.Path, &i.Alias); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// FileIDsImporting lists the files that import one of the given paths.
func (r *Registry) FileIDsImporting(ctx context.Context, runID int64, paths []string) ([]int64, error) {
	return r.chunkedFileIDs(ctx, paths, func(n int) string {
		return `SELECT DISTINCT file_id FROM file_imports
		         WHERE index_run_id = ? AND path IN (` + placeholders(n) + `)`
	}, runID)
}

// FilesBoundToReturnTypes lists the files holding a reference bound to a
// definition that declares it returns one of the given type names: the calls
// a variable's type is read from (see migration 0023).
func (r *Registry) FilesBoundToReturnTypes(ctx context.Context, runID int64, types []string) ([]int64, error) {
	return r.chunkedFileIDs(ctx, types, func(n int) string {
		return `SELECT DISTINCT sr.file_id FROM symbols s
		          JOIN symbol_references sr ON sr.resolved_symbol_id = s.id
		         WHERE s.index_run_id = ? AND s.return_type IN (` + placeholders(n) + `)`
	}, runID)
}

// NamesInFilesDefining lists every definition name in a file that defines a
// type of one of the given names, together with every definition whose
// qualified name places it inside such a type. It is the over-approximation
// of "the members of these types" that a change of ancestry needs.
func (r *Registry) NamesInFilesDefining(ctx context.Context, runID int64, types []string) ([]string, error) {
	seen := map[string]struct{}{}
	for start := 0; start < len(types); start += idChunk {
		chunk := types[start:min(start+idChunk, len(types))]
		args := []any{runID, runID}
		for _, t := range chunk {
			args = append(args, t)
		}
		got, err := r.strings(ctx, `
			SELECT DISTINCT s.name FROM symbols s
			 WHERE s.index_run_id = ? AND s.kind <> 'file' AND s.file_id IN (
			       SELECT t.file_id FROM symbols t
			        WHERE t.index_run_id = ? AND t.name IN (`+placeholders(len(chunk))+`))`, args...)
		if err != nil {
			return nil, err
		}
		for _, n := range got {
			seen[n] = struct{}{}
		}
	}
	// Members declared outside the type's own file (a Go method in another
	// file of the package, an Objective-C category, a C# partial class) are
	// found by their qualified name. One scan of the run's symbols, and only
	// when an ancestry changed.
	if len(types) > 0 {
		rows, err := r.db.QueryContext(ctx,
			`SELECT name, fqn FROM symbols WHERE index_run_id = ? AND kind <> 'file'`, runID)
		if err != nil {
			return nil, fmt.Errorf("registry: member names: %w", err)
		}
		want := make([]string, 0, len(types)*2)
		for _, t := range types {
			want = append(want, ":"+t+".", "."+t+".", "("+t+")", "*"+t+")")
		}
		for rows.Next() {
			var name, fqn string
			if err := rows.Scan(&name, &fqn); err != nil {
				_ = rows.Close()
				return nil, err
			}
			for _, w := range want {
				if strings.Contains(fqn, w) {
					seen[name] = struct{}{}
					break
				}
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return sortedKeys(seen), nil
}

// SymbolNamesUnder lists every definition name in the files under dir ("" is
// the whole run).
func (r *Registry) SymbolNamesUnder(ctx context.Context, runID int64, dir string) ([]string, error) {
	if dir == "" {
		return r.strings(ctx, `SELECT DISTINCT name FROM symbols WHERE index_run_id = ? AND kind <> 'file'`, runID)
	}
	return r.strings(ctx, `
		SELECT DISTINCT s.name FROM files f JOIN symbols s ON s.file_id = f.id
		 WHERE f.index_run_id = ? AND f.path >= ? AND f.path < ? AND s.kind <> 'file'`,
		runID, dir+"/", dir+"0")
}

// ResetFileResolutions returns every reference of the given files to pending
// and removes the edges the resolver wrote for them, so the next Resolve binds
// those files again exactly as a first resolve would.
//
// A reference a compiler settled (resolution_source 'compiler') keeps its
// outcome and its edge: resolution does not re-create either, and the passes
// that do (the Go type check, RestoreCompilerProof) run after it.
//
// The edges removed are exactly those of the references being reset, matched
// on file, line and target, which is how the resolver writes them. A resolver
// edge no reset reference accounts for stays: the Go type check leaves the
// heuristic edge of a call into a file excluded by build constraints beside
// the reference it settled, and a reset must not remove what resolution will
// not write again.
func (r *Registry) ResetFileResolutions(ctx context.Context, runID int64, fileIDs []int64) (refs, edges int, err error) {
	if len(fileIDs) == 0 {
		return 0, 0, nil
	}
	ids := append([]int64(nil), fileIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("registry: reset file resolutions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for start := 0; start < len(ids); start += idChunk {
		n, err := resetResolutions(ctx, tx, runID, ids[start:min(start+idChunk, len(ids))])
		if err != nil {
			return 0, 0, err
		}
		edges += n
	}
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE symbol_references
		   SET resolution = 'pending', resolved_symbol_id = NULL,
		       resolution_source = '', prior_resolution = '', external = 0
		 WHERE file_id = ? AND resolution <> 'pending' AND resolution_source <> 'compiler'`)
	if err != nil {
		return 0, 0, fmt.Errorf("registry: reset file resolutions: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, id)
		if err != nil {
			return 0, 0, fmt.Errorf("registry: reset file resolutions: references: %w", err)
		}
		m, _ := res.RowsAffected()
		refs += int(m)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("registry: reset file resolutions: commit: %w", err)
	}
	return refs, edges, nil
}

// resetResolutions deletes the resolver edges of the heuristically decided
// references of the given files (nil: every file of the run), and returns how
// many went. The references themselves are left to the caller.
func resetResolutions(ctx context.Context, tx *sql.Tx, runID int64, fileIDs []int64) (int, error) {
	args := []any{runID}
	filter := `f.index_run_id = ?`
	if fileIDs != nil {
		filter += ` AND f.id IN (` + placeholders(len(fileIDs)) + `)`
		for _, id := range fileIDs {
			args = append(args, id)
		}
	}
	q := `
		DELETE FROM edges WHERE id IN (
		  SELECT e.id FROM files f
		    JOIN edges e ON e.index_run_id = f.index_run_id AND e.evidence_file = f.path
		   WHERE ` + filter + `
		     AND e.source <> 'compiler' AND e.kind IN (` + resolverEdgeKinds + `)
		     AND EXISTS (
		         SELECT 1 FROM symbol_references sr
		          WHERE sr.file_id = f.id AND sr.start_line = e.evidence_line
		            AND sr.resolved_symbol_id = e.dst_id
		            AND sr.resolution = 'resolved' AND sr.resolution_source <> 'compiler'))`
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("registry: reset resolutions: edges: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *Registry) fileIDs(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: file ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *Registry) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: strings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// chunkedFileIDs runs a file-id query once per chunk of values, the values
// bound after the fixed leading arguments.
func (r *Registry) chunkedFileIDs(ctx context.Context, values []string, query func(n int) string, lead ...any) ([]int64, error) {
	seen := map[int64]struct{}{}
	for start := 0; start < len(values); start += idChunk {
		chunk := values[start:min(start+idChunk, len(values))]
		args := append(append([]any(nil), lead...), toAny(chunk)...)
		ids, err := r.fileIDs(ctx, query(len(chunk)), args...)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			seen[id] = struct{}{}
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
