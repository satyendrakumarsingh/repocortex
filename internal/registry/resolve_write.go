package registry

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// EnclosingSymbol is a symbol's line span, used to attribute a reference to the
// function or method that contains it.
//
// This is what makes "who calls this" answerable. An edge whose source is a
// FILE tells an agent that some code in TransactionService.java calls
// updateStatus; an edge whose source is a SYMBOL tells it that
// TransactionService.processCallback does. Only the second is actionable.
type EnclosingSymbol struct {
	ID        int64
	Name      string
	StartLine int
	EndLine   int
}

// FileSymbols returns a file's symbols ordered by position, for enclosure
// lookups during resolution.
func (r *Registry) FileSymbols(ctx context.Context, fileID int64) ([]EnclosingSymbol, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, start_line, end_line
		  FROM symbols
		 WHERE file_id = ?
		 ORDER BY start_line, end_line DESC`, fileID)
	if err != nil {
		return nil, fmt.Errorf("registry: file symbols %d: %w", fileID, err)
	}
	defer rows.Close()

	var out []EnclosingSymbol
	for rows.Next() {
		var s EnclosingSymbol
		if err := rows.Scan(&s.ID, &s.Name, &s.StartLine, &s.EndLine); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AddResolvedEdges writes edges produced by resolution.
//
// Every edge is validated before it is written. That check is not defensive
// boilerplate: it is the mechanism preventing an inferred relationship from
// ever being recorded as compiler-proven, and it is backed by a SQL CHECK so a
// buggy writer cannot bypass it either.
//
// Duplicates are ignored rather than treated as errors. Resolution is
// idempotent by design — re-running it after a SCIP index arrives, or after the
// resolver improves, must be safe.
// Returns the number of edges that were actually INSERTED. That is not
// len(edges): the statement is INSERT OR IGNORE, so an edge already present is
// silently skipped, and reporting the input length made `repocortex resolve`
// claim 7,233 edges on a repository whose graph held 7,076 — the difference
// being duplicates the caller had no way to know about.
func (r *Registry) AddResolvedEdges(ctx context.Context, runID int64, edges []model.Edge) (int, error) {
	if len(edges) == 0 {
		return 0, nil
	}

	// Validate the whole batch before opening a transaction. The old code
	// validated each edge just before its own INSERT, which is the same
	// guarantee — a refused edge aborted the transaction and nothing landed —
	// but doing it up front keeps an invalid edge from ever reaching SQLite.
	for i := range edges {
		e := edges[i]
		e.IndexRunID = runID
		if err := e.Validate(); err != nil {
			return 0, fmt.Errorf("registry: refusing invalid edge: %w", err)
		}
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("registry: add edges: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var inserted int
	args := make([]any, 0, edgeChunk*edgeCols)
	insert := func(stmt *sql.Stmt, batch []model.Edge) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		args = args[:0]
		for i := range batch {
			e := batch[i]
			args = append(args,
				runID, e.SrcID, e.DstID, string(e.Kind), string(e.Confidence),
				e.Evidence.FilePath, e.Evidence.Range.Start.Line, e.Evidence.Range.Start.Col,
				string(e.Evidence.Source), e.Evidence.Detail)
		}
		res, err := stmt.ExecContext(ctx, args...)
		if err != nil {
			return fmt.Errorf("registry: insert %d edge(s) starting %d->%d: %w",
				len(batch), batch[0].SrcID, batch[0].DstID, err)
		}
		// RowsAffected on a multi-row INSERT OR IGNORE is sqlite3_changes(),
		// which counts only the rows that actually landed — a duplicate inside
		// the statement is skipped exactly as it was one statement at a time, so
		// the reported total is unchanged.
		if n, err := res.RowsAffected(); err == nil {
			inserted += int(n)
		}
		return nil
	}

	// Full chunks share one prepared statement; only the short trailing chunk
	// needs SQL of its own.
	whole, rest := len(edges)/edgeChunk, len(edges)%edgeChunk
	if whole > 0 {
		stmt, err := tx.PrepareContext(ctx, insertEdgesSQL(edgeChunk))
		if err != nil {
			return 0, fmt.Errorf("registry: add edges: prepare: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for i := 0; i < whole; i++ {
			if err := insert(stmt, edges[i*edgeChunk:(i+1)*edgeChunk]); err != nil {
				return 0, err
			}
		}
	}
	if rest > 0 {
		stmt, err := tx.PrepareContext(ctx, insertEdgesSQL(rest))
		if err != nil {
			return 0, fmt.Errorf("registry: add edges: prepare remainder: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		if err := insert(stmt, edges[len(edges)-rest:]); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// edgeCols is the column count of one edge row; edgeChunk is how many rows go
// into one INSERT. 200 x 10 = 2,000 bound parameters, well inside SQLite's
// 32,766 limit.
const (
	edgeCols  = 10
	edgeChunk = 200
)

// insertEdgesSQL builds a multi-row INSERT OR IGNORE for n edges.
func insertEdgesSQL(n int) string {
	var b strings.Builder
	b.Grow(160 + n*24)
	b.WriteString(`INSERT OR IGNORE INTO edges
			(index_run_id, src_id, dst_id, kind, confidence,
			 evidence_file, evidence_line, evidence_col, source, detail)
		VALUES `)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?,?,?,?)")
	}
	return b.String()
}
