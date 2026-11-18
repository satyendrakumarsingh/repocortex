package registry

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// FileRec is one indexed file as the guided lookups need to see it: enough to
// turn something a source file says ("imports ../base/lifecycle.js", "is in
// this directory") into the file ids a targeted query takes.
type FileRec struct {
	ID       int64
	Path     string
	Language string
}

// guidedParamBudget bounds the bound parameters of one guided statement. SQLite
// allows 32,766; this stays far under it because a failed statement costs more
// than an extra round trip.
const guidedParamBudget = 900

// PerNameLimit is the most definitions one name's candidate list holds. A list
// that long may have been cut, and is the only kind InFiles needs to be asked
// about: a shorter list already holds every definition of its name.
func (c *CandidateCache) PerNameLimit() int { return c.perName }

// Files returns every file of the run, in id order.
//
// It is what lets the resolver turn an import specifier or a directory into
// file ids without a query per reference. It is one row per FILE, not per
// symbol: about 3 MB on a 20,000-file tree, held once for the run, which is
// not the growth CandidateCache exists to avoid (that is one row per
// definition, and the over-cap names are where there are thousands of them).
func (c *CandidateCache) Files(ctx context.Context) ([]FileRec, error) {
	if c.files != nil {
		return c.files, nil
	}
	rows, err := c.reg.db.QueryContext(ctx,
		`SELECT id, path, language FROM files WHERE index_run_id = ? ORDER BY id`, c.runID)
	if err != nil {
		return nil, fmt.Errorf("registry: list files: %w", err)
	}
	defer rows.Close()
	out := make([]FileRec, 0, 1024)
	for rows.Next() {
		var f FileRec
		if err := rows.Scan(&f.ID, &f.Path, &f.Language); err != nil {
			return nil, err
		}
		f.Language = c.internKind(f.Language)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.files = out
	return out, nil
}

// InFiles returns the definitions of each name that live in one of the files.
//
// This is the guided counterpart of Candidates. Candidates answers "what could
// this name be" and is bounded per name, because with nothing to go on a name
// that has 2,722 definitions cannot be weighed. InFiles answers "does one of
// THESE files define it", which is how an import, the enclosing file or the
// file that defines the receiver's type narrows the question, and the answer is
// bounded by the files and not by the name: it reads one index probe per
// (file, name) pair (idx_symbols_file_name, migration 0016).
//
// Rows come back in the order Candidates returns them (by id) so a rule that
// reads the first of several sees the same one either way. Names with no
// definition in the files are absent from the result. Nothing is cached here:
// the caller memoises per source file, which is the unit that repeats.
func (c *CandidateCache) InFiles(ctx context.Context, names []string, fileIDs []int64) (map[string][]SymbolIndexEntry, error) {
	out := make(map[string][]SymbolIndexEntry, len(names))
	if len(names) == 0 || len(fileIDs) == 0 {
		return out, nil
	}
	// Chunk both lists so one statement never exceeds the parameter budget. The
	// cross product is probed by the index, so splitting it costs round trips
	// and nothing else.
	const nameChunk = 100
	fileChunk := max(guidedParamBudget-nameChunk, 100)
	for ns := 0; ns < len(names); ns += nameChunk {
		nchunk := names[ns:min(ns+nameChunk, len(names))]
		for fs := 0; fs < len(fileIDs); fs += fileChunk {
			fchunk := fileIDs[fs:min(fs+fileChunk, len(fileIDs))]
			if err := c.inFilesChunk(ctx, nchunk, fchunk, out); err != nil {
				return nil, err
			}
		}
	}
	for n := range out {
		b := out[n]
		sort.Slice(b, func(i, j int) bool { return b[i].ID < b[j].ID })
	}
	return out, nil
}

func (c *CandidateCache) inFilesChunk(ctx context.Context, names []string, fileIDs []int64, out map[string][]SymbolIndexEntry) error {
	args := make([]any, 0, len(names)+len(fileIDs)+1)
	args = append(args, c.runID)
	for _, n := range names {
		args = append(args, n)
	}
	for _, id := range fileIDs {
		args = append(args, id)
	}
	q := `SELECT s.id, s.name, s.fqn, s.kind, s.file_id, f.path, s.exported, s.param_count, f.language, s.return_type
	        FROM symbols s INDEXED BY idx_symbols_file_name
	        JOIN files f ON f.id = s.file_id
	       WHERE s.index_run_id = ?
	         AND s.name IN (` + strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + `)
	         AND s.file_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(fileIDs)), ",") + `)`
	rows, err := c.reg.db.QueryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("registry: guided candidates: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e SymbolIndexEntry
		var exported, paramCount int
		if err := rows.Scan(&e.ID, &e.Name, &e.FQN, &e.Kind, &e.FileID, &e.FilePath,
			&exported, &paramCount, &e.Language, &e.ReturnType); err != nil {
			return err
		}
		e.Exported = exported == 1
		e.ParamCount = model.Arity(paramCount)
		e.FilePath = c.internPath(e.FilePath)
		e.Kind = c.internKind(e.Kind)
		e.Language = c.internKind(e.Language)
		e.ReturnType = c.internKind(e.ReturnType)
		out[e.Name] = append(out[e.Name], e)
	}
	return rows.Err()
}
