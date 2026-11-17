package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Definition-level full-text rows (defs_fts, migration 0021).
//
// files_fts answers "which files mention these words"; on a long file that is
// the wrong grain twice over. bm25 ranks the file below short files that say
// a word once, and when it does match, nothing tells its functions apart.
// defs_fts holds one row per function or method, so a context pack can rank
// the definition that uses the task's words rather than the file around it.

// DefinitionSpan is one definition whose body is indexed: its symbol id, name and
// 1-based inclusive line range.
type DefinitionSpan struct {
	ID         int64
	Name       string
	Start, End int
}

// HasDefinitionText reports whether a symbol of this kind gets a defs_fts
// row. Functions and methods: the units a task's change lands in. A class
// would repeat its methods' text, and a test is not where a change lands.
func HasDefinitionText(k model.EntityKind) bool {
	return k == model.KindFunction || k == model.KindMethod
}

// definitionTexts returns the indexed text of each span: the identifier parts
// of its name and body, de-duplicated. The file is split into lines once.
func definitionTexts(content string, spans []DefinitionSpan) []string {
	if len(spans) == 0 {
		return nil
	}
	lines := strings.Split(content, "\n")
	out := make([]string, len(spans))
	for i, sp := range spans {
		start, end := sp.Start, sp.End
		if start < 1 {
			start = 1
		}
		if end > len(lines) {
			end = len(lines)
		}
		var b strings.Builder
		b.WriteString(sp.Name)
		if start <= end {
			for _, l := range lines[start-1 : end] {
				b.WriteByte('\n')
				b.WriteString(l)
			}
		}
		out[i] = search.ExpandIdentifiers(b.String())
	}
	return out
}

// DefinitionSpansOf picks the symbols that get definition text, in order.
func DefinitionSpansOf(symbols []model.Symbol) []DefinitionSpan {
	var out []DefinitionSpan
	for _, s := range symbols {
		if s.ID > 0 && HasDefinitionText(s.Kind) {
			out = append(out, DefinitionSpan{ID: s.ID, Name: s.Name, Start: s.Range.Start.Line, End: s.Range.End.Line})
		}
	}
	return out
}

// insertDefinitionTexts writes the rows for one file's definitions.
func insertDefinitionTexts(ctx context.Context, stmt *sql.Stmt, content string, spans []DefinitionSpan) error {
	for i, text := range definitionTexts(content, spans) {
		if text == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, spans[i].ID, text); err != nil {
			return fmt.Errorf("index definition %d: %w", spans[i].ID, err)
		}
	}
	return nil
}

const insertDefSQL = `INSERT INTO defs_fts (rowid, expanded) VALUES (?, ?)`

// IndexDefinitionText writes the definition rows of the file just indexed.
// content is the text given to IndexFileText, already redacted.
func (w *Writer) IndexDefinitionText(ctx context.Context, content string, spans []DefinitionSpan) error {
	if w.failed != nil {
		return w.failed
	}
	if len(spans) == 0 {
		return nil
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if err := insertDefinitionTexts(ctx, w.st.defs, content, spans); err != nil {
		return w.fail(fmt.Errorf("registry: %w", err))
	}
	return w.rotate(ctx)
}

// DefinitionMatch is one definition a full-text query matched.
type DefinitionMatch struct {
	SymbolID int64
	FileID   int64
	// Score is FTS5 bm25: negative, more negative is better.
	Score float64
}

// SearchDefinitions runs a full-text query over function and method bodies
// of the committed run, best first.
func (r *Registry) SearchDefinitions(ctx context.Context, query string, limit int) ([]DefinitionMatch, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.file_id, bm25(defs_fts)
		  FROM defs_fts
		  JOIN symbols s    ON s.id = defs_fts.rowid
		  JOIN index_runs r ON r.id = s.index_run_id AND r.committed = 1
		 WHERE defs_fts MATCH ?
		 ORDER BY bm25(defs_fts), s.id
		 LIMIT ?`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: search definitions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DefinitionMatch
	for rows.Next() {
		var m DefinitionMatch
		if err := rows.Scan(&m.SymbolID, &m.FileID, &m.Score); err != nil {
			return nil, fmt.Errorf("registry: search definitions: scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HasDefinitionIndex reports whether any definition text is stored. An index
// built before migration 0021 has none until it is backfilled, and a context
// pack then works from file-level text alone.
func (r *Registry) HasDefinitionIndex(ctx context.Context) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM defs_fts LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("registry: definition index: %w", err)
	}
	return true, nil
}

// BackfillDefinitionText fills defs_fts for a run indexed before it existed,
// from the file text files_fts already stores (redacted when it was written).
// It does nothing when any definition text is present, so it is safe to call
// on every refresh. One transaction: an interrupted backfill leaves none, and
// the next refresh starts it again.
func (r *Registry) BackfillDefinitionText(ctx context.Context, runID int64) (int, error) {
	if has, err := r.HasDefinitionIndex(ctx); err != nil || has {
		return 0, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("registry: backfill definitions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	spans := map[int64][]DefinitionSpan{}
	srows, err := tx.QueryContext(ctx, `
		SELECT s.file_id, s.id, s.name, s.start_line, s.end_line
		  FROM symbols s
		 WHERE s.index_run_id = ? AND s.kind IN (?, ?)
		 ORDER BY s.file_id, s.id`,
		runID, string(model.KindFunction), string(model.KindMethod))
	if err != nil {
		return 0, fmt.Errorf("registry: backfill definitions: symbols: %w", err)
	}
	for srows.Next() {
		var fileID int64
		var sp DefinitionSpan
		if err := srows.Scan(&fileID, &sp.ID, &sp.Name, &sp.Start, &sp.End); err != nil {
			_ = srows.Close()
			return 0, fmt.Errorf("registry: backfill definitions: scan: %w", err)
		}
		spans[fileID] = append(spans[fileID], sp)
	}
	if err := srows.Close(); err != nil {
		return 0, err
	}
	if len(spans) == 0 {
		return 0, nil
	}

	stmt, err := tx.PrepareContext(ctx, insertDefSQL)
	if err != nil {
		return 0, fmt.Errorf("registry: backfill definitions: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	frows, err := tx.QueryContext(ctx, `
		SELECT m.file_id, files_fts.content
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f         ON f.id = m.file_id
		 WHERE f.index_run_id = ?`, runID)
	if err != nil {
		return 0, fmt.Errorf("registry: backfill definitions: files: %w", err)
	}
	// Streamed: one file's text in memory at a time. Writing defs_fts while
	// a cursor over files_fts is open on the same connection is allowed, as
	// they are different tables.
	n := 0
	for frows.Next() {
		var fileID int64
		var content string
		if err := frows.Scan(&fileID, &content); err != nil {
			_ = frows.Close()
			return 0, fmt.Errorf("registry: backfill definitions: scan file: %w", err)
		}
		sp := spans[fileID]
		if len(sp) == 0 {
			continue
		}
		if err := insertDefinitionTexts(ctx, stmt, content, sp); err != nil {
			_ = frows.Close()
			return 0, fmt.Errorf("registry: backfill definitions: %w", err)
		}
		n += len(sp)
	}
	if err := frows.Close(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("registry: backfill definitions: commit: %w", err)
	}
	return n, nil
}

// deleteDefinitionRows removes the defs_fts rows of a run's symbols, or of one
// file's when path is set. FTS5 has no foreign key to cascade through, so this
// runs before the symbols are deleted.
func deleteDefinitionRows(ctx context.Context, tx *sql.Tx, runID int64, path string) error {
	var err error
	if path == "" {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM defs_fts
			 WHERE rowid IN (SELECT s.id FROM symbols s WHERE s.index_run_id = ?)`, runID)
	} else {
		_, err = tx.ExecContext(ctx, `
			DELETE FROM defs_fts
			 WHERE rowid IN (
			       SELECT s.id FROM symbols s
			         JOIN files f ON f.id = s.file_id
			        WHERE f.index_run_id = ? AND f.path = ?)`, runID, path)
	}
	if err != nil {
		return fmt.Errorf("delete definition text: %w", err)
	}
	return nil
}

// DefinitionsMatching returns which of ids a full-text query matches. It is
// how a caller learns which words each definition of a ranked result
// contains, without a ranked query per word over the whole index.
func (r *Registry) DefinitionsMatching(ctx context.Context, query string, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var b strings.Builder
	args := make([]any, 0, len(ids)+1)
	args = append(args, query)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT rowid FROM defs_fts WHERE defs_fts MATCH ? AND rowid IN (`+b.String()+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: definitions matching: %w", err)
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
