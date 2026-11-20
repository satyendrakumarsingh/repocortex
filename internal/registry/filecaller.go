package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// FileCallerKind is the kind of the synthetic symbol that stands for the code of
// a file that sits outside every extracted definition.
//
// WHY IT EXISTS. A call needs a caller, and an edge needs a symbol at each end.
// In a JavaScript or TypeScript test file nearly every call sits inside
// `describe(...)` and `it(...)` callbacks, which are not definitions; a Python
// module runs its top level on import; a Go package initialises its variables
// outside any function. Those calls were recorded as references and bound, and
// had no edge, so "who calls this" never listed them. The answer layer counted
// them as unlisted uses and could not say it was complete.
//
// WHAT IT IS. One row per file that has such code, created by `resolve` (never
// by `index`, so the parser's definitions and the conformance fixtures stay as
// they are), of kind `file`, named FileCallerName(path) so a caller list reads
// "(top level of src/foo.test.ts)", with the qualified name
// "<path>:(top level of <path>)". It is the SOURCE of edges only: nothing is ever
// bound to it, and no tool that lists, searches or counts definitions shows it:
// each query that does carries `kind <> 'file'`, and a query that follows an edge
// to its source does not, so a caller can be named. Anonymous callbacks nested
// inside a named function stay with that function: the symbol is only the
// fallback when no extracted definition encloses the line.
const FileCallerKind = string(model.KindFile)

// FileCallerName names the synthetic caller of a file the way an answer reads
// it.
func FileCallerName(path string) string { return "(top level of " + path + ")" }

// FileCallerFQN is its qualified name: unique per file, and not the spelling of
// anything a reference can name.
func FileCallerFQN(path string) string { return path + ":" + FileCallerName(path) }

const insertFileCallerSQL = `
	INSERT OR IGNORE INTO symbols
		(index_run_id, file_id, kind, name, fqn, start_line, start_col, end_line, end_col,
		 signature, doc, exported, confidence, source)`

// EnsureFileCallers creates the synthetic caller of every file that still holds
// a reference to be resolved outside every extracted definition, and returns
// how many it created. It is idempotent: a file that has one keeps it.
//
// A definition spans start_line to end_line, and a definition whose span is
// unknown (end_line 0) covers only its own line, exactly as the resolver reads
// it (infer.enclosing), so the two agree on what "outside" means.
//
// ONE PASS. This was a single INSERT ... WHERE EXISTS (pending reference AND
// NOT EXISTS (definition covering its line)): for every pending reference a
// probe of the file's symbols, 30% of the resolve time on a synthetic Perl
// repository. A line is covered when some definition starting at or before it
// ends at or after it, which is the same as the largest end among the
// definitions starting at or before it reaching it. So both streams are read
// once, ordered by file and line (a definition before a reference on the
// same line), and one running maximum answers every reference: work
// proportional to the rows, not to references times symbols. The result is
// the one the SQL gave; see TestEnsureFileCallersMatchesTheQuery.
func (r *Registry) EnsureFileCallers(ctx context.Context, runID int64) (int, error) {
	pending := `symbol_references WHERE index_run_id = ? AND resolution = 'pending' AND start_line > 0`
	rows, err := r.db.QueryContext(ctx, `
		SELECT file_id, start_line, 0 AS ref, MAX(end_line, start_line)
		  FROM symbols
		 WHERE index_run_id = ? AND kind <> 'file'
		   AND file_id IN (SELECT file_id FROM `+pending+`)
		UNION
		SELECT file_id, start_line, 1, 0 FROM `+pending+`
		ORDER BY 1, 2, 3`, runID, runID, runID)
	if err != nil {
		return 0, fmt.Errorf("registry: create file callers: %w", err)
	}
	var need []int64
	var file, reach int64 = -1, 0
	flagged := false
	for rows.Next() {
		var f, line, isRef, end int64
		if err := rows.Scan(&f, &line, &isRef, &end); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("registry: create file callers: scan: %w", err)
		}
		if f != file {
			file, reach, flagged = f, 0, false
		}
		switch {
		case flagged:
		case isRef == 0:
			reach = max(reach, end)
		case reach < line:
			flagged = true
			need = append(need, f)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("registry: create file callers: %w", err)
	}
	_ = rows.Close()
	if len(need) == 0 {
		return 0, nil
	}

	// Chunked, one statement per chunk like the single INSERT ... SELECT this
	// replaced: a statement per file costs several times more.
	const chunk = 500
	created := 0
	for len(need) > 0 {
		n := min(chunk, len(need))
		args := make([]any, 0, n+1)
		args = append(args, runID)
		for _, id := range need[:n] {
			args = append(args, id)
		}
		res, err := r.db.ExecContext(ctx, insertFileCallerSQL+`
			SELECT f.index_run_id, f.id, 'file', '(top level of ' || f.path || ')', f.path || ':(top level of ' || f.path || ')',
			       1, 0, 1, 0, '', '', 0, 'high', 'source_code'
			  FROM files f WHERE f.index_run_id = ? AND f.id IN (?`+strings.Repeat(",?", n-1)+`)
			 ORDER BY f.id`, args...)
		if err != nil {
			return 0, fmt.Errorf("registry: create file callers: %w", err)
		}
		c, _ := res.RowsAffected()
		created += int(c)
		need = need[n:]
	}
	return created, nil
}

// FileCaller returns the synthetic caller of one file, creating it when the
// file has none.
func (r *Registry) FileCaller(ctx context.Context, runID, fileID int64, path string) (int64, error) {
	if _, err := r.db.ExecContext(ctx, insertFileCallerSQL+`
		VALUES (?, ?, 'file', ?, ?, 1, 0, 1, 0, '', '', 0, 'high', 'source_code')`,
		runID, fileID, FileCallerName(path), FileCallerFQN(path)); err != nil {
		return 0, fmt.Errorf("registry: create file caller of %s: %w", path, err)
	}
	var id int64
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM symbols WHERE index_run_id = ? AND file_id = ? AND kind = 'file' LIMIT 1`,
		runID, fileID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("registry: file caller of %s was not created", path)
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read file caller of %s: %w", path, err)
	}
	return id, nil
}

// PruneFileCallers deletes the synthetic callers that no edge starts or ends
// at, and returns how many went. A file can have code outside its definitions
// whose references all stayed unbound; its caller would be an unused row.
func (r *Registry) PruneFileCallers(ctx context.Context, runID int64) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM symbols
		 WHERE index_run_id = ? AND kind = 'file'
		   AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.src_id = symbols.id)
		   AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.dst_id = symbols.id)`, runID)
	if err != nil {
		return 0, fmt.Errorf("registry: prune file callers: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// FileCallerIDs returns the synthetic caller of every file that has one, keyed
// by the file's repository path.
func (r *Registry) FileCallerIDs(ctx context.Context, runID int64) (map[string]int64, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, s.id FROM symbols s JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = 'file'`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: file callers: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var id int64
		if err := rows.Scan(&p, &id); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}

// TopLevelEdge is an edge that starts at a file's synthetic caller, with the
// path of the file whose top level it stands for.
type TopLevelEdge struct {
	model.Edge
	CallerPath string
}

// TopLevelEdges returns the edges that Edges leaves out: those that start at a
// file's synthetic caller. A whole-graph reader that attributes an edge by the
// FILE its source sits in (an architecture boundary, a component) can use them,
// where one that draws a node per definition cannot. Ordered like Edges.
func (r *Registry) TopLevelEdges(ctx context.Context, runID int64) ([]TopLevelEdge, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.src_id, e.dst_id, e.kind, e.confidence,
		       e.evidence_file, e.evidence_line, e.evidence_col, e.source, COALESCE(e.detail,''), f.path
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		  CROSS JOIN edges e INDEXED BY idx_edges_src
		          ON e.index_run_id = s.index_run_id AND e.src_id = s.id
		 WHERE s.index_run_id = ? AND s.kind = 'file'
		 ORDER BY e.src_id, e.dst_id, e.kind, e.evidence_file, e.evidence_line`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: top-level edges: %w", err)
	}
	defer rows.Close()
	var out []TopLevelEdge
	for rows.Next() {
		var t TopLevelEdge
		var kind, conf, src string
		if err := rows.Scan(&t.ID, &t.SrcID, &t.DstID, &kind, &conf,
			&t.Evidence.FilePath, &t.Evidence.Range.Start.Line, &t.Evidence.Range.Start.Col,
			&src, &t.Evidence.Detail, &t.CallerPath); err != nil {
			return nil, fmt.Errorf("registry: top-level edges: scan: %w", err)
		}
		t.Kind = model.EdgeKind(kind)
		t.Confidence = model.Confidence(conf)
		t.Evidence.Source = model.SourceType(src)
		t.IndexRunID = runID
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReferenceLines returns the lines of a file on which the parser recorded a
// reference: a call, a construction or a mention of a name. An import or an
// export-from statement is not one of them.
func (r *Registry) ReferenceLines(ctx context.Context, fileID int64) (map[int]struct{}, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT start_line FROM symbol_references WHERE file_id = ? AND start_line > 0`, fileID)
	if err != nil {
		return nil, fmt.Errorf("registry: reference lines of file %d: %w", fileID, err)
	}
	defer rows.Close()
	out := map[int]struct{}{}
	for rows.Next() {
		var l int
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out[l] = struct{}{}
	}
	return out, rows.Err()
}

// TopLevel is the part of a run's graph that Symbols and Edges leave out: the
// synthetic top-level callers and the edges that start at them.
type TopLevel struct {
	// Callers are the file-kind symbols that start at least one edge, ordered by
	// file path.
	Callers []model.Symbol
	// Edges are the edges that start at one of Callers, in the order Edges uses.
	Edges []model.Edge
}

// TopLevelCalls reads the top-level callers of a run and their edges, so a
// whole-graph consumer (export, docs, diff) can add them to what Symbols and
// Edges return and show the same calls `callers` and `callees` list. A caller
// that starts no edge is not returned: it is an empty row, not a fact.
func (r *Registry) TopLevelCalls(ctx context.Context, runID int64) (TopLevel, error) {
	var out TopLevel
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, s.kind, s.name, s.fqn,
		       s.start_line, s.start_col, s.end_line, s.end_col,
		       COALESCE(s.signature,''), COALESCE(s.doc,''), s.exported, s.confidence, s.source
		  FROM symbols s JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = 'file'
		   AND EXISTS (SELECT 1 FROM edges e WHERE e.index_run_id = s.index_run_id AND e.src_id = s.id)
		 ORDER BY f.path, s.id`, runID)
	if err != nil {
		return out, fmt.Errorf("registry: list top-level callers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s model.Symbol
		var kind, conf, src string
		var exported int
		if err := rows.Scan(&s.ID, &s.FileID, &kind, &s.Name, &s.FQN,
			&s.Range.Start.Line, &s.Range.Start.Col, &s.Range.End.Line, &s.Range.End.Col,
			&s.Signature, &s.Doc, &exported, &conf, &src); err != nil {
			return out, fmt.Errorf("registry: list top-level callers: scan: %w", err)
		}
		s.Kind = model.EntityKind(kind)
		s.Confidence = model.Confidence(conf)
		s.Source = model.SourceType(src)
		s.Exported = exported == 1
		out.Callers = append(out.Callers, s)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	erows, err := r.db.QueryContext(ctx, `
		SELECT id, src_id, dst_id, kind, confidence,
		       evidence_file, evidence_line, evidence_col, source, COALESCE(detail,'')
		  FROM edges WHERE index_run_id = ?
		   AND EXISTS (SELECT 1 FROM symbols fc WHERE fc.id = edges.src_id AND fc.kind = 'file')
		 ORDER BY src_id, dst_id, kind, evidence_file, evidence_line`, runID)
	if err != nil {
		return out, fmt.Errorf("registry: list top-level edges: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var e model.Edge
		var kind, conf, src string
		if err := erows.Scan(&e.ID, &e.SrcID, &e.DstID, &kind, &conf,
			&e.Evidence.FilePath, &e.Evidence.Range.Start.Line, &e.Evidence.Range.Start.Col,
			&src, &e.Evidence.Detail); err != nil {
			return out, fmt.Errorf("registry: list top-level edges: scan: %w", err)
		}
		e.Kind = model.EdgeKind(kind)
		e.Confidence = model.Confidence(conf)
		e.Evidence.Source = model.SourceType(src)
		e.IndexRunID = runID
		out.Edges = append(out.Edges, e)
	}
	return out, erows.Err()
}
