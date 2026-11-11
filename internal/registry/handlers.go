package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// HandlerBindStats reports the outcome of binding endpoint handlers across files.
type HandlerBindStats struct {
	// Bound endpoints got exactly one candidate handler and an exposes edge.
	Bound int
	// Ambiguous endpoints named a handler defined in more than one file. They are
	// left unbound rather than guessed at.
	Ambiguous int
	// Unmatched endpoints named a handler that no function or method in the run
	// declares outside test code.
	Unmatched int
}

// handlerNameChunk bounds the IN (...) lists so a large run stays under SQLite's
// variable limit.
const handlerNameChunk = 500

// BindEndpointHandlers binds endpoints whose handler lives in a different file
// from the route that registers it.
//
// Same-file bindings are written during parsing. This is the only pass that can
// see a handler declared in a file parsed later, so it runs once the whole run
// has been seen, in the same position as BindClientCalls.
//
// The binding rests on a name match, so each edge is medium confidence, and a
// name shared by more than one candidate is refused. Test code is never a
// candidate: a test helper called the same as a handler would otherwise be
// reported as the endpoint's implementation.
func (r *Registry) BindEndpointHandlers(ctx context.Context, runID int64) (HandlerBindStats, error) {
	var st HandlerBindStats

	type unbound struct {
		id      int64
		handler string
		path    string
		line    int
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.signature, ef.path, e.start_line
		  FROM symbols e
		  JOIN files ef ON ef.id = e.file_id
		 WHERE e.index_run_id = ?
		   AND e.kind = ?
		   AND COALESCE(e.signature, '') <> ''
		   AND NOT EXISTS (SELECT 1 FROM edges x WHERE x.src_id = e.id AND x.kind = ?)
		 ORDER BY ef.path, e.start_line, e.id`,
		runID, string(model.KindEndpoint), string(model.EdgeExposes))
	if err != nil {
		return st, fmt.Errorf("registry: read unbound endpoints: %w", err)
	}
	var pending []unbound
	for rows.Next() {
		var u unbound
		if err := rows.Scan(&u.id, &u.handler, &u.path, &u.line); err != nil {
			_ = rows.Close()
			return st, err
		}
		pending = append(pending, u)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return st, err
	}
	_ = rows.Close()
	if len(pending) == 0 {
		return st, nil
	}

	names := map[string]struct{}{}
	for _, u := range pending {
		names[u.handler] = struct{}{}
	}
	candidates, err := r.handlerCandidates(ctx, runID, names)
	if err != nil {
		return st, err
	}

	var edges []model.Edge
	for _, u := range pending {
		var matches []handlerCandidate
		for _, c := range candidates[u.handler] {
			if c.path == u.path {
				// Same file: parse-time binding already handled it, and a name
				// that did not bind there does not match here either.
				continue
			}
			matches = append(matches, c)
		}
		switch len(matches) {
		case 0:
			st.Unmatched++
		case 1:
			edges = append(edges, model.Edge{
				SrcID: u.id, DstID: matches[0].id, Kind: model.EdgeExposes,
				Confidence: model.ConfidenceMedium,
				Evidence: model.Evidence{
					FilePath: u.path,
					Range:    model.Range{Start: model.Position{Line: u.line}},
					Source:   model.SourceSourceCode,
					Detail:   "handler bound by name across files",
				},
			})
			st.Bound++
		default:
			st.Ambiguous++
		}
	}

	if _, err := r.AddResolvedEdges(ctx, runID, edges); err != nil {
		return st, fmt.Errorf("registry: write handler edges: %w", err)
	}
	return st, nil
}

type handlerCandidate struct {
	id   int64
	path string
}

// handlerCandidates returns the non-test functions and methods in the run, keyed
// by name, for the names given.
func (r *Registry) handlerCandidates(ctx context.Context, runID int64, names map[string]struct{}) (map[string][]handlerCandidate, error) {
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	out := map[string][]handlerCandidate{}
	for start := 0; start < len(list); start += handlerNameChunk {
		end := min(start+handlerNameChunk, len(list))
		chunk := list[start:end]
		args := []any{runID, string(model.KindFunction), string(model.KindMethod)}
		marks := make([]string, len(chunk))
		for i, n := range chunk {
			marks[i] = "?"
			args = append(args, n)
		}
		q := `
			SELECT s.id, s.name, f.path
			  FROM symbols s
			  JOIN files f ON f.id = s.file_id
			 WHERE s.index_run_id = ?
			   AND s.kind IN (?, ?)
			   AND s.name IN (` + strings.Join(marks, ",") + `)
			 ORDER BY f.path, s.start_line, s.id`
		rows, err := r.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("registry: handler candidates: %w", err)
		}
		for rows.Next() {
			var (
				c    handlerCandidate
				name string
			)
			if err := rows.Scan(&c.id, &name, &c.path); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if isTestSource(c.path) {
				continue
			}
			out[name] = append(out[name], c)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// isTestSource reports whether a path is test code. The forms are the ones the
// languages this indexer parses use for test files and fixtures.
func isTestSource(path string) bool {
	p := strings.ReplaceAll(path, `\`, "/")
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasPrefix(base, "test_"),
		strings.Contains(base, ".test."),
		strings.Contains(base, ".spec."):
		return true
	case strings.Contains(p, "/testdata/"), strings.HasPrefix(p, "testdata/"):
		return true
	}
	return false
}
