package registry

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

// FileDirectives are what one file says about the files it brings into its
// library without importing them: the libraries it re-exports and the parts it
// names. Paths are as the parser wrote them, without extension.
type FileDirectives struct {
	Exports []string
	Parts   []string
}

// Directives returns the export and part directives of every file of a
// language, keyed by the file's path without its extension, which is the
// spelling an import of it uses. Read once per language and kept: a run's
// imports do not change under a resolver, the rows are few (a barrel has a
// handful), and a per-file query would be one per file resolved.
//
// Rows come back in file and line order, so a file's lists are in source order
// whatever order the files were indexed in.
func (c *CandidateCache) Directives(ctx context.Context, language string) (map[string]FileDirectives, error) {
	if got, ok := c.directives[language]; ok {
		return got, nil
	}
	rows, err := c.reg.db.QueryContext(ctx,
		`SELECT f.path, i.path, i.alias
		   FROM file_imports i JOIN files f ON f.id = i.file_id
		  WHERE i.index_run_id = ? AND f.language = ? AND i.alias IN ('export', 'include')
		  ORDER BY f.path, i.start_line, i.path`, c.runID, language)
	if err != nil {
		return nil, fmt.Errorf("registry: load directives: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]FileDirectives{}
	for rows.Next() {
		var file, target, alias string
		if err := rows.Scan(&file, &target, &alias); err != nil {
			return nil, err
		}
		key := strings.TrimSuffix(file, path.Ext(file))
		d := out[key]
		if alias == "export" {
			d.Exports = append(d.Exports, target)
		} else {
			d.Parts = append(d.Parts, target)
		}
		out[key] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if c.directives == nil {
		c.directives = map[string]map[string]FileDirectives{}
	}
	c.directives[language] = out
	return out, nil
}

// ImportGraph returns, for every file of a language, the paths it imports, keyed
// by the file's path without extension (the spelling an import of it uses). Rows
// are in file and line order. It is what a language that follows imports
// transitively walks (Bash and PowerShell sourcing, a C-family #include): the
// file's own rows say what it reaches directly, and these say what each of those
// reaches in turn. A directive row (`export`, `include`) and a member row are no
// import of a file and are left out. Every file of the language is a key, one
// that imports nothing included, so that a chain can end at it. Read once per language and kept, as
// Directives is. Two files of one stem (`util.c` and `util.h`) are one key and
// their rows are joined, which is the pairing the resolver's own import matching
// makes.
func (c *CandidateCache) ImportGraph(ctx context.Context, language string) (map[string][]string, error) {
	if got, ok := c.importGraphs[language]; ok {
		return got, nil
	}
	rows, err := c.reg.db.QueryContext(ctx,
		`SELECT f.path, i.path
		   FROM files f LEFT JOIN file_imports i
		     ON i.file_id = f.id AND i.index_run_id = f.index_run_id
		    AND i.alias NOT IN ('export', 'include') AND i.alias NOT LIKE '.%'
		  WHERE f.index_run_id = ? AND f.language = ?
		  ORDER BY f.path, i.start_line, i.path`, c.runID, language)
	if err != nil {
		return nil, fmt.Errorf("registry: load import graph: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var file string
		var target sql.NullString
		if err := rows.Scan(&file, &target); err != nil {
			return nil, err
		}
		key := strings.TrimSuffix(file, path.Ext(file))
		imports := out[key]
		if imports == nil {
			// A file that imports nothing is still a file: a chain ends at it.
			imports = []string{}
		}
		if target.Valid {
			imports = append(imports, target.String)
		}
		out[key] = imports
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if c.importGraphs == nil {
		c.importGraphs = map[string]map[string][]string{}
	}
	c.importGraphs[language] = out
	return out, nil
}
