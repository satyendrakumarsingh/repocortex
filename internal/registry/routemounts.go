package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// FileText is one indexed file's path and text.
type FileText struct {
	Path    string
	Content string
}

// PythonMountFiles returns the indexed Python files that mount a router or
// blueprint: the ones that can give a route in another file its prefix.
//
// Found through the full-text index, so the cost follows the number of such
// files (a handful) rather than the size of the repository. The tokenizer
// splits on '_', so the phrase "include router" is include_router; a file that
// merely says the words is filtered out by the caller's own parse.
func (r *Registry) PythonMountFiles(ctx context.Context, runID int64) ([]FileText, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, files_fts.content
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f         ON f.id = m.file_id
		 WHERE files_fts MATCH ?
		   AND f.index_run_id = ?
		 ORDER BY f.path`,
		`content : "include router" OR content : "register blueprint"`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: python mount files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FileText
	for rows.Next() {
		var ft FileText
		if err := rows.Scan(&ft.Path, &ft.Content); err != nil {
			return nil, err
		}
		if strings.HasSuffix(ft.Path, ".py") {
			out = append(out, ft)
		}
	}
	return out, rows.Err()
}

// EndpointRow is one endpoint symbol with where it was declared.
type EndpointRow struct {
	ID   int64
	Name string
	FQN  string
	Path string
	Line int
}

// PythonEndpoints returns every endpoint the run's Python files declare.
func (r *Registry) PythonEndpoints(ctx context.Context, runID int64) ([]EndpointRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn, f.path, s.start_line
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ? AND f.path LIKE '%.py'
		 ORDER BY f.path, s.start_line, s.id`,
		runID, string(model.KindEndpoint))
	if err != nil {
		return nil, fmt.Errorf("registry: python endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EndpointRow
	for rows.Next() {
		var e EndpointRow
		if err := rows.Scan(&e.ID, &e.Name, &e.FQN, &e.Path, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ExpressMountFiles returns the indexed JS/TS files that may mount an Express
// router: those that call use() and import something. "use" is a common word,
// so the extension is filtered in SQL rather than after every matching file's
// text has been read.
func (r *Registry) ExpressMountFiles(ctx context.Context, runID int64) ([]FileText, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, files_fts.content
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f         ON f.id = m.file_id
		 WHERE files_fts MATCH ?
		   AND f.index_run_id = ?
		   AND `+jsPathSQL+`
		 ORDER BY f.path`,
		`content : "use" AND (content : "require" OR content : "import")`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: express mount files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FileText
	for rows.Next() {
		var ft FileText
		if err := rows.Scan(&ft.Path, &ft.Content); err != nil {
			return nil, err
		}
		if IsJSPath(ft.Path) {
			out = append(out, ft)
		}
	}
	return out, rows.Err()
}

// ExpressEndpoints returns every endpoint the run's JS/TS files declare.
func (r *Registry) ExpressEndpoints(ctx context.Context, runID int64) ([]EndpointRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn, f.path, s.start_line
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ?
		   AND `+jsPathSQL+`
		 ORDER BY f.path, s.start_line, s.id`,
		runID, string(model.KindEndpoint))
	if err != nil {
		return nil, fmt.Errorf("registry: express endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EndpointRow
	for rows.Next() {
		var e EndpointRow
		if err := rows.Scan(&e.ID, &e.Name, &e.FQN, &e.Path, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// jsExts are the JavaScript and TypeScript source extensions.
var jsExts = []string{".js", ".ts", ".mjs", ".cjs", ".jsx", ".tsx", ".mts", ".cts"}

// jsPathSQL restricts f.path to jsExts.
var jsPathSQL = func() string {
	parts := make([]string, len(jsExts))
	for i, e := range jsExts {
		parts[i] = "f.path LIKE '%" + e + "'"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}()

// IsJSPath reports whether a path is a JavaScript or TypeScript source.
func IsJSPath(p string) bool {
	for _, e := range jsExts {
		if strings.HasSuffix(p, e) {
			return true
		}
	}
	return false
}

// RenameEndpoints sets the display name of endpoint symbols, by id. The FQN,
// which is the symbol's identity, is left alone.
func (r *Registry) RenameEndpoints(ctx context.Context, names map[int64]string) error {
	if len(names) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: rename endpoints: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE symbols SET name = ? WHERE id = ? AND kind = ?`)
	if err != nil {
		return fmt.Errorf("registry: rename endpoints: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for id, name := range names {
		if _, err := stmt.ExecContext(ctx, name, id, string(model.KindEndpoint)); err != nil {
			return fmt.Errorf("registry: rename endpoint %d: %w", id, err)
		}
	}
	return tx.Commit()
}
