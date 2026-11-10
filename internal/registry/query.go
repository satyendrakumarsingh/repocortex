package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// SymbolHit is a symbol as returned to a query caller.
type SymbolHit struct {
	ID         int64
	Name       string
	FQN        string
	Kind       string
	FilePath   string
	StartLine  int
	EndLine    int
	Confidence string
	Source     string
	Exported   bool
}

// FindSymbolsByName looks up definitions by exact name.
//
// Exact rather than fuzzy, deliberately. An agent that asks for
// "TransactionService" wants that type, not the forty symbols containing the
// substring; fuzzy matching here would flood a context budget with near-misses.
// search_code exists for the exploratory case.
func (r *Registry) FindSymbolsByName(ctx context.Context, runID int64, name, kind string, limit int) ([]SymbolHit, error) {
	if limit <= 0 {
		limit = 20
	}

	q := `
		SELECT s.id, s.name, s.fqn, s.kind, f.path, s.start_line, s.end_line,
		       s.confidence, s.source, s.exported
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.name = ?`
	args := []any{runID, name}
	if kind != "" {
		q += ` AND s.kind = ?`
		args = append(args, kind)
	}
	// Exported symbols first: a caller asking by name almost always means the
	// public one, and ordering is stable so repeated calls agree.
	q += ` ORDER BY s.exported DESC, f.path, s.start_line LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: find symbols %q: %w", name, err)
	}
	defer rows.Close()

	var out []SymbolHit
	for rows.Next() {
		var h SymbolHit
		var exported int
		if err := rows.Scan(&h.ID, &h.Name, &h.FQN, &h.Kind, &h.FilePath,
			&h.StartLine, &h.EndLine, &h.Confidence, &h.Source, &exported); err != nil {
			return nil, err
		}
		h.Exported = exported == 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// EdgeHit is a relationship with both endpoints named.
type EdgeHit struct {
	SrcID        int64
	SrcName      string
	SrcPath      string
	DstID        int64
	DstName      string
	Kind         string
	Confidence   string
	Source       string
	Detail       string
	EvidenceFile string
	EvidenceLine int
	// EvidenceCol is edges.evidence_col. It is a HINT, not a byte offset:
	// tree-sitter edges record the start of the whole call expression (measured:
	// col 55 for `s.repo.CreateOrder(id)` whose name starts at byte 62), and
	// SCIP edges record the document's PositionEncoding, UTF-16 units for the
	// JVM and TypeScript indexers. A consumer must check what is at the column
	// before relying on it; internal/edit.Locate does.
	EvidenceCol int
}

// EdgeFilter narrows an edge query at the SOURCE, before any LIMIT is applied.
//
// This exists so a caller asking for a subset — one kind, a confidence floor —
// gets that subset filled up to its cap, rather than an unfiltered cap's worth
// of rows with the subset picked out of them afterward. Filtering after a LIMIT
// can silently empty a real answer: a symbol with a handful of CALLS edges and
// a flood of same-or-higher-confidence REFERENCES edges can have every CALLS
// edge crowded out of the capped set by REFERENCES edges that merely sort
// earlier, even though nothing about the REFERENCES edges is more relevant to
// a caller who asked for calls only. Filtering before LIMIT means the cap is
// spent only on rows the caller actually wants.
type EdgeFilter struct {
	// Kinds restricts results to these edge kinds (e.g. "CALLS"). Empty means
	// every kind.
	Kinds []string
	// MinConfidence is a floor edges must meet or exceed, ranked via
	// model.Confidence.Rank() (exact=4 down to low=1). The zero value ("")
	// ranks 0 and means no floor. Typed rather than a raw int so a caller
	// cannot pass an out-of-vocabulary rank (a bare 7 means nothing); the
	// rank is computed once, here, at query time.
	MinConfidence model.Confidence
}

// confidenceRankCase is a SQL CASE expression ranking a confidence column
// strongest to weakest as an integer, matching the order of
// pkg/model.Confidence.Rank (exact=4 down to unrecognised=0). It exists here,
// not as a call into that method, because e.confidence is a bare SQL text
// column: ranking it for ORDER BY has to happen in SQL regardless of how the
// WHERE floor is expressed. internal/registry is free to import pkg/model —
// it already does, in clientcalls.go, candidates.go and completeness.go, and
// per CLAUDE.md §1 this package is the adapter that returns pkg/model types —
// so nothing here duplicates this table to avoid that import; it duplicates
// it because ORDER BY needs a rank on a SQL column no matter what. The WHERE
// floor itself does NOT go through this CASE a second time: EdgeFilter.
// MinConfidence.Rank() is computed once, in Go, at query time, and bound
// directly as an integer, so a new confidence tier can never diverge between
// this table and pkg/model.Confidence.Rank for the WHERE floor — see
// TestConfidenceRankCaseMatchesModelRank, which pins the ORDER BY side too.
const confidenceRankCase = `CASE %s
		            WHEN 'exact'  THEN 4
		            WHEN 'high'   THEN 3
		            WHEN 'medium' THEN 2
		            WHEN 'low'    THEN 1
		            ELSE 0 END`

// IncomingEdges returns relationships pointing AT a symbol — its callers and
// referrers.
//
// Ordered by confidence so the strongest evidence appears first. An agent
// reading a truncated list should see the results most likely to be true, not
// an arbitrary prefix.
func (r *Registry) IncomingEdges(ctx context.Context, runID, symbolID int64, limit int, filter EdgeFilter) ([]EdgeHit, error) {
	return r.edgesFor(ctx, runID, symbolID, limit, true, filter)
}

// OutgoingEdges returns relationships FROM a symbol — what it calls.
func (r *Registry) OutgoingEdges(ctx context.Context, runID, symbolID int64, limit int, filter EdgeFilter) ([]EdgeHit, error) {
	return r.edgesFor(ctx, runID, symbolID, limit, false, filter)
}

func (r *Registry) edgesFor(ctx context.Context, runID, symbolID int64, limit int, incoming bool, filter EdgeFilter) ([]EdgeHit, error) {
	if limit <= 0 {
		limit = 50
	}

	// Both directions are covered by an index — idx_edges_src and
	// idx_edges_dst — so neither is a table scan.
	where := "e.dst_id = ?"
	if !incoming {
		where = "e.src_id = ?"
	}

	args := []any{runID, symbolID}

	q := `
		SELECT e.src_id, src.name, srcf.path, e.dst_id, dst.name,
		       e.kind, e.confidence, e.source, COALESCE(e.detail,''),
		       e.evidence_file, e.evidence_line, e.evidence_col
		  FROM edges e
		  JOIN symbols src  ON src.id = e.src_id
		  JOIN files   srcf ON srcf.id = src.file_id
		  JOIN symbols dst  ON dst.id = e.dst_id
		 WHERE e.index_run_id = ? AND ` + where

	if len(filter.Kinds) > 0 {
		placeholders := make([]string, len(filter.Kinds))
		for i, k := range filter.Kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		q += ` AND e.kind IN (` + strings.Join(placeholders, ",") + `)`
	}

	if rank := filter.MinConfidence.Rank(); rank > 0 {
		q += ` AND ` + fmt.Sprintf(confidenceRankCase, "e.confidence") + ` >= ?`
		args = append(args, rank)
	}

	q += ` ORDER BY ` + fmt.Sprintf(confidenceRankCase, "e.confidence") + ` DESC,
		          srcf.path, e.evidence_line
		 LIMIT ?`
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: edges for symbol %d: %w", symbolID, err)
	}
	defer rows.Close()

	var out []EdgeHit
	for rows.Next() {
		var e EdgeHit
		if err := rows.Scan(&e.SrcID, &e.SrcName, &e.SrcPath, &e.DstID, &e.DstName,
			&e.Kind, &e.Confidence, &e.Source, &e.Detail,
			&e.EvidenceFile, &e.EvidenceLine, &e.EvidenceCol); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// IncomingEdgeCount reports how many relationships point AT a symbol and pass
// filter, excluding any edge whose kind appears in excludeKinds — a
// COUNT(*), not a row fetch.
//
// This exists so a caller can report how many further relationships exist
// beyond a capped, kind-restricted fetch without that report saturating at
// the same cap the fetch used: fetching rows only to count and discard them
// caps the count at whatever LIMIT the fetch used, so a note built from it
// understates an arbitrarily large true total as if it were exact. A
// COUNT(*) has no such cap — it is exact regardless of how many rows match.
func (r *Registry) IncomingEdgeCount(ctx context.Context, runID, symbolID int64, filter EdgeFilter, excludeKinds []string) (int, error) {
	where := `e.index_run_id = ? AND e.dst_id = ?`
	args := []any{runID, symbolID}

	if len(filter.Kinds) > 0 {
		placeholders := make([]string, len(filter.Kinds))
		for i, k := range filter.Kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		where += ` AND e.kind IN (` + strings.Join(placeholders, ",") + `)`
	}
	if len(excludeKinds) > 0 {
		placeholders := make([]string, len(excludeKinds))
		for i, k := range excludeKinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		where += ` AND e.kind NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if rank := filter.MinConfidence.Rank(); rank > 0 {
		where += ` AND ` + fmt.Sprintf(confidenceRankCase, "e.confidence") + ` >= ?`
		args = append(args, rank)
	}

	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges e WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("registry: count incoming edges for symbol %d: %w", symbolID, err)
	}
	return n, nil
}

// LanguageCounts reports indexed files per language, for project overview.
func (r *Registry) LanguageCounts(ctx context.Context, runID int64) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT language, count(*) FROM files
		 WHERE index_run_id = ? AND language <> ''
		 GROUP BY language ORDER BY count(*) DESC`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: language counts: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var lang string
		var n int
		if err := rows.Scan(&lang, &n); err != nil {
			return nil, err
		}
		out[lang] = n
	}
	return out, rows.Err()
}

// FileExcerpt returns a bounded window of a file's indexed text.
//
// Reads from the FTS copy rather than the filesystem, on purpose: the index is
// the thing the caller asked about, and serving from disk could return content
// that differs from what was indexed — or a file that has since been replaced
// by a symlink. It also keeps MCP tools from touching arbitrary paths.
func (r *Registry) FileExcerpt(ctx context.Context, runID int64, path string, startLine, lines int) (string, error) {
	if lines <= 0 {
		lines = 20
	}
	var content string
	err := r.db.QueryRowContext(ctx, `
		SELECT files_fts.content
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f ON f.id = m.file_id
		 WHERE f.index_run_id = ? AND f.path = ?`, runID, path).Scan(&content)
	if err != nil {
		return "", fmt.Errorf("registry: excerpt for %s: %w", path, err)
	}

	all := strings.Split(content, "\n")
	start := startLine - 1
	if start < 0 {
		start = 0
	}
	if start >= len(all) {
		return "", nil
	}
	end := start + lines
	if end > len(all) {
		end = len(all)
	}
	return strings.Join(all[start:end], "\n"), nil
}
