package registry

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Reference is an unresolved use of a name, persisted so resolution can run as
// a second pass over the complete symbol table.
type Reference struct {
	ID       int64
	FileID   int64
	Name     string
	Kind     string
	Receiver string
	Line     int
	Col      int

	// ArgCount is how many arguments this call site passes, or
	// model.ArityUnknown when the parser could not say. See migration
	// 0010_arity for why unknown and zero must not share a representation.
	ArgCount model.Arity
}

// Import is a dependency declared by a file.
type Import struct {
	FileID int64
	Path   string
	Alias  string
	Line   int
}

// Resolution records what happened to a reference.
type Resolution string

const (
	ResolutionPending    Resolution = "pending"
	ResolutionResolved   Resolution = "resolved"
	ResolutionAmbiguous  Resolution = "ambiguous"
	ResolutionUnresolved Resolution = "unresolved"
	ResolutionIgnored    Resolution = "ignored"
)

// AddReference stages one reference. Batched like every other writer method;
// a 10,920-file tree produces over half a million of these.
func (w *Writer) AddReference(ctx context.Context, fileID int64, r Reference) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO symbol_references
			(index_run_id, file_id, name, kind, receiver, start_line, start_col, arg_count, resolution)
		VALUES (?,?,?,?,?,?,?,?, 'pending')`,
		w.runID, fileID, r.Name, r.Kind, r.Receiver, r.Line, r.Col, int(r.ArgCount),
	); err != nil {
		return w.fail(fmt.Errorf("registry: add reference %q: %w", r.Name, err))
	}
	return w.rotate(ctx)
}

// ReplaceFileReferences re-attaches one file's references and imports after
// ReplaceFile has recreated it.
//
// The full indexing path writes these through the batched Writer. The
// incremental path does not use the Writer, and never wrote them at all: it
// extracted references and imports into FileFacts and dropped them on the
// floor. Re-indexing a file therefore left it with zero references, so nothing
// in it could ever resolve again and its call edges were gone until a full
// re-index — a repository's intra-file edges bled away one edited file at a
// time.
//
// It is the same omission ReplaceFileAnalysis exists to fix, on a different
// table, and it is insert-only for the same reason: the caller has just
// replaced the file, so there is nothing left to delete.
func (r *Registry) ReplaceFileReferences(
	ctx context.Context,
	runID, fileID int64,
	refs []Reference,
	imports []Import,
) error {
	if len(refs) == 0 && len(imports) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: replace file references: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO symbol_references
				(index_run_id, file_id, name, kind, receiver, start_line, start_col, arg_count, resolution)
			VALUES (?,?,?,?,?,?,?,?, 'pending')`,
			runID, fileID, ref.Name, ref.Kind, ref.Receiver, ref.Line, ref.Col, int(ref.ArgCount),
		); err != nil {
			return fmt.Errorf("registry: re-add reference %q: %w", ref.Name, err)
		}
	}
	for _, imp := range imports {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO file_imports (index_run_id, file_id, path, alias, start_line)
			VALUES (?,?,?,?,?)`,
			runID, fileID, imp.Path, imp.Alias, imp.Line,
		); err != nil {
			return fmt.Errorf("registry: re-add import %q: %w", imp.Path, err)
		}
	}
	return tx.Commit()
}

// AddImport stages one import.
func (w *Writer) AddImport(ctx context.Context, fileID int64, imp Import) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	// A file may legitimately repeat an import across overlapping tag patterns;
	// the unique constraint makes that harmless rather than fatal.
	if _, err := w.tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO file_imports (index_run_id, file_id, path, alias, start_line)
		VALUES (?,?,?,?,?)`,
		w.runID, fileID, imp.Path, imp.Alias, imp.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add import %q: %w", imp.Path, err))
	}
	return w.rotate(ctx)
}

// FileRefs is one file's references plus the imports needed to resolve them.
//
// Resolution is file-scoped because imports are: the meaning of a bare name
// depends on what its file brought into scope.
type FileRefs struct {
	FileID   int64
	Path     string
	Language string
	Imports  []Import
	Refs     []Reference

	// Symbols defined in this file, ordered by position. Used to attribute a
	// reference to the function or method containing it, which is what makes
	// "who calls this" answerable rather than "which file mentions this".
	Symbols []EnclosingSymbol
}

// EachFileWithRefs streams files that have unresolved references.
//
// Streaming, not slice-returning, and deliberately so: half a million
// references with their surrounding context would be hundreds of megabytes held
// at once, which is exactly the kind of unbounded accumulation that made
// indexing peak at 6.5 GB before it was fixed.
//
// fn must not retain the FileRefs it is given beyond the call.
func (r *Registry) EachFilePageWithRefs(ctx context.Context, runID int64, fn func([]FileRefs) error) error {
	// A reference marked resolved but pointing at nothing is a broken
	// invariant, not a resolved reference. It is picked up here as well as
	// 'pending' so that an index already carrying the damage repairs itself on
	// the next `cortex resolve` instead of requiring a full re-index.
	//
	// ReplaceFile now prevents the state arising (see deleteFileRows), but
	// indexes written before that fix still contain it, and they are the ones
	// that most need it — the damage is invisible until someone notices an
	// edge that should exist and does not.
	if _, err := r.db.ExecContext(ctx, `
		UPDATE symbol_references
		   SET resolution = 'pending'
		 WHERE index_run_id = ?
		   AND resolution <> 'pending'
		   AND resolved_symbol_id IS NULL
		   AND resolution = 'resolved'`, runID); err != nil {
		return fmt.Errorf("registry: repair orphaned references: %w", err)
	}

	// Paged rather than materialised, and paged by KEY rather than by OFFSET.
	//
	// The whole list used to be read into a slice before the first file was
	// resolved, which is a per-file cost that grows with the repository: the
	// last O(files) term in a pass whose memory is otherwise bounded. It could
	// not simply stream from an open cursor, because fn writes to this same
	// database — resolution flushes edges and outcomes as it goes — and holding
	// a read cursor across those writes is what SQLite has locking rules about.
	//
	// A keyset page reads a bounded slice, closes the cursor, and only then
	// hands the rows to fn. OFFSET would be wrong here as well as slower: fn
	// marks references resolved, so rows leave the result set as it runs and
	// the meaning of "skip the first N" changes underneath the query. Asking
	// for the next path instead is stable under that mutation, and a file
	// cannot be visited twice because the cursor only ever moves forward.
	var after string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := r.db.QueryContext(ctx, `
			SELECT f.id, f.path, f.language
			  FROM files f
			 WHERE f.index_run_id = ?
			   AND f.path > ?
			   AND EXISTS (SELECT 1 FROM symbol_references sr
			                WHERE sr.file_id = f.id AND sr.resolution = 'pending')
			 ORDER BY f.path
			 LIMIT ?`, runID, after, filePageSize)
		if err != nil {
			return fmt.Errorf("registry: list files with references: %w", err)
		}

		files := make([]fileRow, 0, filePageSize)
		for rows.Next() {
			var fr fileRow
			if err := rows.Scan(&fr.id, &fr.path, &fr.language); err != nil {
				_ = rows.Close()
				return err
			}
			files = append(files, fr)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(files) == 0 {
			return nil
		}
		after = files[len(files)-1].path

		page, err := r.loadFilePage(ctx, files)
		if err != nil {
			return err
		}
		if err := fn(page); err != nil {
			return err
		}
	}
}

// fileRow is one row of the paged file list.
type fileRow struct {
	id             int64
	path, language string
}

// filePageSize is how many files one keyset page reads. Bounded so the page is
// a rounding error next to the candidate cache; large enough that paging costs
// one query per few hundred files rather than one per file.
const filePageSize = 512

// loadFilePage reads one page of files' references, imports and symbols.
func (r *Registry) loadFilePage(ctx context.Context, files []fileRow) ([]FileRefs, error) {
	page := make([]FileRefs, 0, len(files))
	var err error
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fr := FileRefs{FileID: f.id, Path: f.path, Language: f.language}

		if fr.Imports, err = r.importsForFile(ctx, f.id); err != nil {
			return nil, err
		}
		if fr.Refs, err = r.refsForFile(ctx, f.id); err != nil {
			return nil, err
		}
		if fr.Symbols, err = r.FileSymbols(ctx, f.id); err != nil {
			return nil, err
		}
		page = append(page, fr)
	}
	return page, nil
}

// EachFileWithRefs streams files that have unresolved references, one at a time.
//
// A convenience over EachFilePageWithRefs for callers that have no use for the
// page boundary. Resolution does use it — batching its candidate lookups across
// a page is what keeps the per-file lookup from costing a round trip per file.
func (r *Registry) EachFileWithRefs(ctx context.Context, runID int64, fn func(FileRefs) error) error {
	return r.EachFilePageWithRefs(ctx, runID, func(page []FileRefs) error {
		for _, fr := range page {
			if err := fn(fr); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Registry) importsForFile(ctx context.Context, fileID int64) ([]Import, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT path, alias, start_line FROM file_imports WHERE file_id = ? ORDER BY path`, fileID)
	if err != nil {
		return nil, fmt.Errorf("registry: imports for file %d: %w", fileID, err)
	}
	defer rows.Close()

	var out []Import
	for rows.Next() {
		imp := Import{FileID: fileID}
		if err := rows.Scan(&imp.Path, &imp.Alias, &imp.Line); err != nil {
			return nil, err
		}
		out = append(out, imp)
	}
	return out, rows.Err()
}

func (r *Registry) refsForFile(ctx context.Context, fileID int64) ([]Reference, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, kind, receiver, start_line, start_col, arg_count
		  FROM symbol_references
		 WHERE file_id = ? AND resolution = 'pending'
		 ORDER BY start_line, start_col, name`, fileID)
	if err != nil {
		return nil, fmt.Errorf("registry: references for file %d: %w", fileID, err)
	}
	defer rows.Close()

	var out []Reference
	for rows.Next() {
		ref := Reference{FileID: fileID}
		var argCount int
		if err := rows.Scan(&ref.ID, &ref.Name, &ref.Kind, &ref.Receiver, &ref.Line, &ref.Col, &argCount); err != nil {
			return nil, err
		}
		ref.ArgCount = model.Arity(argCount)
		out = append(out, ref)
	}
	return out, rows.Err()
}

// SymbolIndexEntry is the minimum needed to decide whether a symbol is a
// plausible target for a reference. Deliberately narrow: the whole index is
// held in memory during resolution, so every field costs ~100 kB per 1,000
// symbols.
type SymbolIndexEntry struct {
	ID       int64
	Name     string
	FQN      string
	Kind     string
	FileID   int64
	FilePath string
	Exported bool

	// ParamCount is how many parameters this symbol declares, or
	// model.ArityUnknown. Carried here because A4 compares it against a
	// reference's argument count to PREFER a candidate — never to remove one.
	ParamCount model.Arity
}

// LoadSymbolIndex returns every symbol in a run, keyed by name.
//
// Resolution is a join between half a million references and ~94,000 symbols.
// Doing that in SQL per reference is half a million queries; doing it as a
// nested loop is 47 billion comparisons. An in-memory index keyed by name makes
// each lookup O(1) against a handful of candidates, and at roughly 10 MB per
// 100,000 symbols it comfortably fits.
//
// The scan interns the two columns that repeat. A file path is shared by every
// symbol the file declares and a kind by thousands of symbols, but database/sql
// hands back a freshly allocated string per row per column, so a 250,000-symbol
// run allocated 250,000 copies of 25,000 distinct paths. Interning them costs
// one map lookup per row and is the difference between the index holding the
// distinct strings and holding one per symbol.
func (r *Registry) LoadSymbolIndex(ctx context.Context, runID int64) (map[string][]SymbolIndexEntry, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn, s.kind, s.file_id, f.path, s.exported, s.param_count
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: load symbol index: %w", err)
	}
	defer rows.Close()

	index := make(map[string][]SymbolIndexEntry, 4096)
	paths := make(map[string]string, 4096)
	kinds := make(map[string]string, 32)
	intern := func(pool map[string]string, s string) string {
		if got, ok := pool[s]; ok {
			return got
		}
		pool[s] = s
		return s
	}
	for rows.Next() {
		var e SymbolIndexEntry
		var exported, paramCount int
		if err := rows.Scan(&e.ID, &e.Name, &e.FQN, &e.Kind, &e.FileID, &e.FilePath,
			&exported, &paramCount); err != nil {
			return nil, err
		}
		e.Exported = exported == 1
		e.ParamCount = model.Arity(paramCount)
		e.FilePath = intern(paths, e.FilePath)
		e.Kind = intern(kinds, e.Kind)
		// The name is already this bucket's key; reuse that copy rather than
		// keeping one more per candidate.
		if bucket, ok := index[e.Name]; ok {
			e.Name = bucket[0].Name
			index[e.Name] = append(bucket, e)
			continue
		}
		index[e.Name] = []SymbolIndexEntry{e}
	}
	return index, rows.Err()
}

// markChunk is how many outcomes go into one UPDATE statement.
//
// Three bound parameters per outcome, against SQLite's default limit of 32,766,
// so 512 leaves an order of magnitude of head-room. Larger chunks stop paying:
// the win is amortising statement dispatch and B-tree descent, and both are
// already amortised away by a few hundred rows.
const markChunk = 512

// MarkResolutions records outcomes for a batch of references.
//
// Takes an ordered slice rather than a map, and that is the performance fix,
// not a style preference. The previous signature was map[int64]ResolvedRef and
// this loop was `for refID := range results` — Go randomises map iteration, so
// 2.6 million single-row UPDATEs arrived at the B-tree in shuffled primary-key
// order. Every one of them was a fresh root-to-leaf descent into a table and
// two indexes that had long outgrown the page cache, so the cost per row rose
// with the corpus and resolution went superlinear. Measured on a 25,000-file
// tree: 111s of a 180s pass was spent here.
//
// Sorting by reference id turns those descents sequential, and folding the
// batch into UPDATE ... FROM (VALUES ...) statements of markChunk rows turns
// 2.6 million statement dispatches into about 5,000.
//
// The outcomes themselves are untouched: every reference still gets exactly the
// resolution and resolved_symbol_id the resolver chose for it.
func (r *Registry) MarkResolutions(ctx context.Context, results []RefOutcome) error {
	if len(results) == 0 {
		return nil
	}
	// Sort a copy: the caller owns its slice and reordering it under them would
	// be an invisible side effect.
	ordered := make([]RefOutcome, len(results))
	copy(ordered, results)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RefID < ordered[j].RefID })

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: mark resolutions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	args := make([]any, 0, markChunk*3)
	apply := func(stmt *sql.Stmt, batch []RefOutcome) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		args = args[:0]
		for _, o := range batch {
			// NULL, not zero: the column is a foreign key to symbols, and an
			// ambiguous or unresolved reference points at no symbol.
			var target any
			if o.SymbolID != 0 {
				target = o.SymbolID
			}
			args = append(args, o.RefID, target, string(o.Resolution))
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("registry: mark resolutions for refs %d..%d: %w",
				batch[0].RefID, batch[len(batch)-1].RefID, err)
		}
		return nil
	}

	// Full chunks share one prepared statement; only the short trailing chunk
	// needs SQL of its own.
	whole, rest := len(ordered)/markChunk, len(ordered)%markChunk
	if whole > 0 {
		stmt, err := tx.PrepareContext(ctx, markResolutionsSQL(markChunk))
		if err != nil {
			return fmt.Errorf("registry: mark resolutions: prepare: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for i := 0; i < whole; i++ {
			if err := apply(stmt, ordered[i*markChunk:(i+1)*markChunk]); err != nil {
				return err
			}
		}
	}
	if rest > 0 {
		stmt, err := tx.PrepareContext(ctx, markResolutionsSQL(rest))
		if err != nil {
			return fmt.Errorf("registry: mark resolutions: prepare remainder: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		if err := apply(stmt, ordered[len(ordered)-rest:]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// markResolutionsSQL builds an UPDATE ... FROM over an inline VALUES list of n
// (reference id, symbol id, resolution) triples.
func markResolutionsSQL(n int) string {
	var b strings.Builder
	b.Grow(96 + n*10)
	b.WriteString("WITH v(rid, sid, res) AS (VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?)")
	}
	b.WriteString(`) UPDATE symbol_references
		   SET resolved_symbol_id = v.sid, resolution = v.res
		  FROM v
		 WHERE symbol_references.id = v.rid`)
	return b.String()
}

// RefOutcome is the outcome for one reference.
//
// Carries its own reference id so batches can travel as ordered slices. See
// MarkResolutions for why the order is load-bearing.
type RefOutcome struct {
	RefID      int64
	SymbolID   int64
	Resolution Resolution
}

// ResolutionStats reports how much of a run could actually be resolved.
//
// This is the number published as resolution precision. It is deliberately
// computed from recorded outcomes rather than estimated, because a tool that
// overstates how much of a codebase it understands is worse than one that
// admits its limits.
type ResolutionStats struct {
	Total      int64
	Resolved   int64
	Ambiguous  int64
	Unresolved int64
	Ignored    int64
	Pending    int64

	// ByCompiler is how many of Resolved were settled by an imported compiler
	// index rather than by the heuristic resolver. Reported separately because
	// the two are different claims to anyone deciding whether to trust an empty
	// `callers` result: one is a typechecker's answer, the other is name
	// matching plus an import heuristic.
	ByCompiler int64
}

// There is deliberately no all-references rate here. Resolved over Total reads
// as a defect rate while mostly measuring how much third-party code a project
// calls, and every surface that once published it has converged on BindingRate
// (see capability.CoverageFloor). Total and Unresolved remain fields, so a
// caller with a reason to want that ratio can still compute it — what is
// withheld is a method inviting it to be quoted as coverage.

// Bindable is the references that name something this index actually defines:
// the ones already bound, plus the ones with candidates it could not choose
// between.
//
// Unresolved rows are excluded on a stated ground, not a convenient one. The
// resolver records 'unresolved' when the name matched ZERO candidates in the
// index (see infer.Resolver), so no improvement to binding could ever bind
// them; only indexing more code could. Counting them as failures makes a
// binding rate read as a defect rate when it is largely a description of the
// project's dependency surface.
//
// THE EXCLUSION IS ONLY HONEST BECAUSE THE EXCLUDED ARE STILL REPORTED. An
// unresolved reference is not proof of anything about a library: it can equally
// be a definition in this repository that the extractor never emitted, and
// MarkCompilerResolutions measures exactly that population by binding the ones
// a compiler can settle. Callers must print Unresolved beside this rate, never
// instead of it.
func (s ResolutionStats) Bindable() int64 { return s.Resolved + s.Ambiguous }

// BindingRate is Resolved over Bindable. Zero when nothing was bindable, which
// is the answer rather than a division to hide.
func (s ResolutionStats) BindingRate() float64 {
	if b := s.Bindable(); b > 0 {
		return float64(s.Resolved) / float64(b)
	}
	return 0
}

// ResolutionStats reads outcome counts for a run.
func (r *Registry) ResolutionStats(ctx context.Context, runID int64) (ResolutionStats, error) {
	var s ResolutionStats
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM symbol_references
		  WHERE index_run_id = ? AND resolution_source = 'compiler'`, runID).
		Scan(&s.ByCompiler); err != nil {
		return s, fmt.Errorf("registry: resolution stats: %w", err)
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT resolution, count(*) FROM symbol_references WHERE index_run_id = ? GROUP BY resolution ORDER BY resolution`, runID)
	if err != nil {
		return s, fmt.Errorf("registry: resolution stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return s, err
		}
		s.Total += n
		switch Resolution(kind) {
		case ResolutionResolved:
			s.Resolved = n
		case ResolutionAmbiguous:
			s.Ambiguous = n
		case ResolutionUnresolved:
			s.Unresolved = n
		case ResolutionIgnored:
			s.Ignored = n
		case ResolutionPending:
			s.Pending = n
		}
	}
	return s, rows.Err()
}

// ReferenceResolution is one recorded reference and what resolution did with
// it, located precisely enough to point a reader at the source line.
//
// This is the per-reference counterpart to ResolutionStats. The aggregate says
// a run resolved 84% of its references; this says which side of that number a
// particular reference fell on, and where it is written.
type ReferenceResolution struct {
	RefID      int64
	Name       string
	Kind       string
	Receiver   string
	Path       string
	Line       int
	Col        int
	Resolution Resolution
	SymbolID   int64 // 0 when the reference bound to no definition
}

// maxNameResolutions bounds ResolutionsForName when the caller asks for no
// limit of its own.
//
// Matched to the other by-symbol readers (IncomingEdges, SearchFiles) rather
// than picked fresh: this answers the same shape of question they do, and a
// caller that hands the result to an agent has the same context budget. A
// common name in a large tree has thousands of references, so an unbounded read
// here is how a diagnostic turns into an out-of-memory.
const maxNameResolutions = 50

// ResolutionsForName reports the recorded outcome of every reference written as
// name, so a missing edge can be diagnosed rather than merely observed.
//
// The absence of an edge is the one answer every tool in this space gives
// badly: an empty result reads identically whether the call does not exist, the
// file was never parsed, or the reference was seen and could not be bound. The
// outcomes have been on disk since migration 0002 and until now were read back
// only as a percentage. With them, "no CALLS edge from X to Y" becomes "the
// reference to Get at handler.go:88 is recorded ambiguous" — a blind spot the
// caller can act on instead of a silence it has to trust.
//
// Reads across the whole run rather than one file because the caller asking
// "why is there no edge?" knows the name it wants and not the file it is
// written in. idx_refs_run_name covers exactly (index_run_id, name), so the
// scan is over the matching rows only.
//
// Ordered by file then position: stable across repeated calls, which the
// determinism gate requires, and it also groups a name's uses by the file they
// live in, which is how someone reading them wants them grouped.
func (r *Registry) ResolutionsForName(
	ctx context.Context,
	runID int64,
	name string,
	limit int,
) ([]ReferenceResolution, error) {
	if limit <= 0 {
		limit = maxNameResolutions
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT sr.id, sr.name, sr.kind, sr.receiver, f.path,
		       sr.start_line, sr.start_col, sr.resolution,
		       COALESCE(sr.resolved_symbol_id, 0)
		  FROM symbol_references sr
		  JOIN files f ON f.id = sr.file_id
		 WHERE sr.index_run_id = ? AND sr.name = ?
		 ORDER BY f.path, sr.start_line, sr.start_col, sr.id
		 LIMIT ?`, runID, name, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: resolutions for reference %q: %w", name, err)
	}
	defer rows.Close()

	var out []ReferenceResolution
	for rows.Next() {
		var rr ReferenceResolution
		var resolution string
		if err := rows.Scan(&rr.RefID, &rr.Name, &rr.Kind, &rr.Receiver, &rr.Path,
			&rr.Line, &rr.Col, &resolution, &rr.SymbolID); err != nil {
			return nil, fmt.Errorf("registry: resolutions for reference %q: scan: %w", name, err)
		}
		rr.Resolution = Resolution(resolution)
		out = append(out, rr)
	}
	return out, rows.Err()
}

// UnresolvedReferences reports references that could not be bound or were ambiguous.
func (r *Registry) UnresolvedReferences(
	ctx context.Context,
	runID int64,
	limit int,
) ([]ReferenceResolution, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT sr.id, sr.name, sr.kind, sr.receiver, f.path,
		       sr.start_line, sr.start_col, sr.resolution,
		       COALESCE(sr.resolved_symbol_id, 0)
		  FROM symbol_references sr
		  JOIN files f ON f.id = sr.file_id
		 WHERE sr.index_run_id = ? AND sr.resolution IN ('unresolved', 'ambiguous')
		 ORDER BY f.path, sr.start_line, sr.start_col, sr.id
		 LIMIT ?`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: unresolved references for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []ReferenceResolution
	for rows.Next() {
		var rr ReferenceResolution
		var resolution string
		if err := rows.Scan(&rr.RefID, &rr.Name, &rr.Kind, &rr.Receiver, &rr.Path,
			&rr.Line, &rr.Col, &resolution, &rr.SymbolID); err != nil {
			return nil, fmt.Errorf("registry: unresolved references: scan: %w", err)
		}
		rr.Resolution = Resolution(resolution)
		out = append(out, rr)
	}
	return out, rows.Err()
}

// TableAccess is one statement's use of one table, staged for binding.
//
// Staged rather than resolved inline for the reason migration 0003 records: the
// table may be declared by a file that has not been parsed yet, and on an
// incremental update may not be reparsed at all.
type TableAccess struct {
	Table   string
	Access  string
	Verb    string
	SrcID   int64 // 0 when the statement sits outside every definition
	Line    int
	Certain bool
}

// AddTableAccess stages one table access.
func (w *Writer) AddTableAccess(ctx context.Context, fileID int64, a TableAccess) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	var src any
	if a.SrcID > 0 {
		src = a.SrcID
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO table_accesses
			(index_run_id, file_id, table_name, access, verb, src_symbol_id, start_line, certain, binding)
		VALUES (?,?,?,?,?,?,?,?, 'pending')`,
		w.runID, fileID, a.Table, a.Access, a.Verb, src, a.Line, boolToInt(a.Certain),
	); err != nil {
		return w.fail(fmt.Errorf("registry: add table access %q: %w", a.Table, err))
	}
	return w.rotate(ctx)
}

// ConfigUsage is one configuration read, staged for binding.
//
// There is no value field, and there must never be one. See migration 0004.
type ConfigUsage struct {
	Key       string
	Mechanism string
	SrcID     int64
	Line      int
}

// AddConfigUsage stages one configuration read.
func (w *Writer) AddConfigUsage(ctx context.Context, fileID int64, u ConfigUsage) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	var src any
	if u.SrcID > 0 {
		src = u.SrcID
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO config_usages
			(index_run_id, file_id, key_name, mechanism, src_symbol_id, start_line, binding)
		VALUES (?,?,?,?,?,?, 'pending')`,
		w.runID, fileID, u.Key, u.Mechanism, src, u.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add config usage %q: %w", u.Key, err))
	}
	return w.rotate(ctx)
}

// ParsedCallSite is one call the tree-sitter parser recognised: where it is
// written, and what it is written as.
//
// It carries no target on purpose. Whether the parser could BIND that name to a
// definition is the parser's guess, and the value of joining these against a
// compiler index is that the two sides are independent. The name is not a
// guess — it is the text of the callee expression, which is why it can be held
// against the compiler's answer without circularity.
type ParsedCallSite struct {
	Path string
	Line int
	Name string
	// Receiver is what the call was written against — `svc` in
	// `svc.delete_line(...)` — or empty for a bare call. It is recorded
	// because the parser emits BOTH a call named `delete_line` with receiver
	// `svc` AND a bare call named `svc` at the same column, and only the
	// receiver column says which of the two is the callee.
	Receiver string
}

// ParsedCallSites returns every distinct position in a run where the parser
// recorded a call.
//
// THE PREDICATE IS COPIED, NOT REINVENTED. `kind LIKE '%call%'` and
// `start_line > 0` mirror internal/cli/accuracy.go's call-site denominator and
// infer.edgeKindFor, which maps any reference kind containing "call" onto a
// CALLS edge. Three definitions of "the parser saw a call here" that drift
// apart would let the importer create edges the accuracy report then scores
// against a different population, and nobody could locate the discrepancy.
//
// Ordered by path, line, name then receiver: a total order over the DISTINCT
// rows returned.
// The caller loads these into a set where order cannot matter, but an unordered
// read feeding a first-wins rule is how three determinism bugs have already
// shipped here, so the read is ordered at the source rather than trusted to
// stay order-insensitive.
//
// Unbounded by design, and it is the one read in this file that is: the caller
// needs the whole set to answer membership offline, and a limit would silently
// truncate a repository's calls into a partially promoted graph — worse than
// either extreme.
func (r *Registry) ParsedCallSites(ctx context.Context, runID int64) ([]ParsedCallSite, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT f.path, sr.start_line, sr.name, sr.receiver
		  FROM symbol_references sr
		  JOIN files f ON f.id = sr.file_id
		 WHERE sr.index_run_id = ? AND sr.kind LIKE '%call%' AND sr.start_line > 0
		 ORDER BY f.path, sr.start_line, sr.name, sr.receiver`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: parsed call sites for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []ParsedCallSite
	for rows.Next() {
		var cs ParsedCallSite
		if err := rows.Scan(&cs.Path, &cs.Line, &cs.Name, &cs.Receiver); err != nil {
			return nil, fmt.Errorf("registry: parsed call sites for run %d: scan: %w", runID, err)
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// CompilerResolutionStats reports one MarkCompilerResolutions call.
type CompilerResolutionStats struct {
	// Bound is references the compiler settled that the resolver had not.
	Bound int64
	// FromUnresolved and FromAmbiguous split Bound by what the resolver had
	// concluded. The split is the finding, not bookkeeping: a row the resolver
	// called 'unresolved' had NO candidate in this index, and the standard
	// reading of that outcome is "a call into a library". Every one the
	// compiler then binds to an indexed symbol is a reference that was in this
	// repository all along and the extractor or the resolver could not see —
	// so this number is the direct measure of how wrong that reading is.
	FromUnresolved int64
	FromAmbiguous  int64
	FromPending    int64
}

// MarkCompilerResolutions binds the references a compiler index settled.
//
// THE RULE, and it is deliberately narrower than the edges it is derived from:
// a reference at (file, line) written as `name` is bound to symbol S when the
// compiler's facts at that exact position name EXACTLY ONE symbol called
// `name`, and S is it. Two different symbols called `name` on one line leave
// the reference alone; so does a line where the compiler resolved something
// else entirely.
//
// WHY KEYED BY NAME AND NOT BY LINE. scip.PromoteCalls has to ask "what does
// the call ON this line reach", so it is restricted to lines the compiler
// resolved to a single target — on `total = compute(order.subtotal)` there is
// no single answer. This is a different question. A reference row already says
// which identifier it is, so `compute` and `subtotal` can each be settled on
// that same line without either being a guess. Line ambiguity is a property of
// the question PromoteCalls asks, not of the compiler's evidence.
//
// WHY THE RECEIVER FILTER DOES NOT APPLY HERE EITHER. `svc` in
// `svc.delete_line(...)` is genuinely a reference to `svc`; what was false was
// only the claim that it is CALLED. Binding it is correct, and refusing to
// would discard a fact the compiler actually proved.
//
// ROWS THE RESOLVER ALREADY BOUND ARE NOT TOUCHED. They are already resolved,
// so coverage does not move, and rewriting them would silently replace the
// resolver's answers underneath the two reports that score them —
// `cortex accuracy` and `doctor --resolution-precision`. Correcting a
// resolver hit that the compiler disagrees with is a separate, larger claim and
// belongs behind its own measurement, not inside an import.
//
// DETERMINISM. The bound target is MIN(dst_id) over a group already constrained
// to hold exactly one distinct dst_id, so it is the only member rather than a
// choice — the same construction accuracy.go uses for the same reason. The
// statement is set-based and has no first-wins rule for a row order to feed,
// which is why there is no ORDER BY here and one on every read below.
func (r *Registry) MarkCompilerResolutions(ctx context.Context, runID int64) (CompilerResolutionStats, error) {
	var st CompilerResolutionStats

	// The candidate set is driven from edges, the small side. On a real
	// repository that is ~94k compiler edges against ~514k references, and the
	// lesson from accuracy.go's 160s query is that the two-orders-of-magnitude
	// smaller relation belongs on the outside of the join.
	const candidates = `
		WITH cand AS (
		    SELECT r.id AS rid, e.dst_id AS dst
		      FROM edges e
		      JOIN symbols s ON s.id = e.dst_id
		      JOIN files   f ON f.id = s.file_id
		      JOIN files   ef ON ef.path = e.evidence_file AND ef.index_run_id = e.index_run_id
		      JOIN symbol_references r
		             ON r.file_id = ef.id
		            AND r.start_line = e.evidence_line
		            AND r.name = s.name
		            AND r.index_run_id = e.index_run_id
		     WHERE e.index_run_id = ?
		       AND e.source = 'compiler'
		       AND e.evidence_line > 0
		       AND r.resolution <> 'resolved'),
		bound AS (
		    SELECT rid, MIN(dst) AS dst
		      FROM cand
		     GROUP BY rid
		    HAVING COUNT(DISTINCT dst) = 1)`

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return st, fmt.Errorf("registry: mark compiler resolutions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Counted BEFORE the update, and grouped by the resolver's verdict, because
	// afterwards every one of these rows reads 'resolved' and the split that
	// makes the number mean something is gone.
	rows, err := tx.QueryContext(ctx, candidates+`
		SELECT r.resolution, COUNT(*)
		  FROM bound b JOIN symbol_references r ON r.id = b.rid
		 GROUP BY r.resolution
		 ORDER BY r.resolution`, runID)
	if err != nil {
		return st, fmt.Errorf("registry: mark compiler resolutions: count: %w", err)
	}
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			_ = rows.Close()
			return st, fmt.Errorf("registry: mark compiler resolutions: scan: %w", err)
		}
		st.Bound += n
		switch Resolution(kind) {
		case ResolutionUnresolved:
			st.FromUnresolved = n
		case ResolutionAmbiguous:
			st.FromAmbiguous = n
		case ResolutionPending:
			st.FromPending = n
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("registry: mark compiler resolutions: %w", err)
	}
	if st.Bound == 0 {
		return st, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, candidates+`
		UPDATE symbol_references
		   SET prior_resolution   = CASE WHEN prior_resolution = '' THEN resolution
		                                 ELSE prior_resolution END,
		       resolution_source  = 'compiler',
		       resolution         = 'resolved',
		       resolved_symbol_id = (SELECT dst FROM bound WHERE rid = symbol_references.id)
		 WHERE id IN (SELECT rid FROM bound)`, runID); err != nil {
		return st, fmt.Errorf("registry: mark compiler resolutions: update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return st, fmt.Errorf("registry: mark compiler resolutions: commit: %w", err)
	}
	return st, nil
}
