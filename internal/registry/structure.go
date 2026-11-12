package registry

import (
	"context"
	"fmt"
	"strings"
)

// This file holds the read queries behind the two whole-repository structural
// reports: unreferenced definitions (internal/capability/deadcode) and per-file
// structure for a set of paths (internal/capability/hotspots).
//
// Both are streamed or bounded by the caller, never materialised for the whole
// run. A repository with a million symbols has a few hundred thousand
// definitions nothing points at before any filtering, and loading them to
// return the first fifty would break the bounded-memory invariant for a
// question whose answer is one page long.

// UnreferencedSymbol is a definition no relationship in the run points at.
//
// It is a CANDIDATE, not a verdict. The three boolean signals are carried
// because "nothing points at it" has three ordinary explanations that are not
// "it is dead", and the capability layer needs each one to grade the row.
type UnreferencedSymbol struct {
	ID        int64
	Kind      string
	Name      string
	FQN       string
	FilePath  string
	Language  string
	StartLine int
	EndLine   int
	Exported  bool

	// PartialParse is true when the defining file did not parse cleanly, so
	// references INSIDE that file may be missing from the index.
	PartialParse bool

	// NameUnbound is true when at least one reference spelled exactly like
	// this symbol was extracted and never bound to a definition. Any such
	// reference might be a use of this symbol; the index cannot say.
	NameUnbound bool

	// Overrides is true when the symbol declares that it implements or extends
	// something. A method reached only through its interface has no direct
	// caller by construction.
	Overrides bool
}

// unreferencedKinds are the definition kinds worth reporting.
//
// Fields, constants and variables are left out on purpose: tree-sitter tag
// queries record a reference for a call far more reliably than for a plain
// read of a name, so "nothing references this constant" is mostly a statement
// about the extractor.
var unreferencedKinds = []string{"function", "method", "class", "interface"}

// unreferencedSQL is the unreferenced-definition query for n kinds.
//
// Every probe names its index (INDEXED BY). That is not decoration. With no
// statistics in the database — a fresh index, or any index `ANALYZE` has not
// run on — the planner preferred the edges table's UNIQUE autoindex, whose
// leading column is index_run_id, and so read every edge in the run once per
// candidate symbol. On a 5,000-symbol, 20,000-edge fixture that was 8.5
// seconds; with the indexes named it is milliseconds. The scale suite found
// it (tests/scale), and TestUnreferencedSymbolsUsesItsIndexes pins the plan.
func unreferencedSQL(n int) string {
	return `
		SELECT s.id, s.kind, s.name, s.fqn, f.path, f.language,
		       s.start_line, s.end_line, s.exported,
		       f.parse_status <> 'ok',
		       EXISTS (SELECT 1 FROM symbol_references sr INDEXED BY idx_refs_run_name
		                WHERE sr.index_run_id = s.index_run_id
		                  AND sr.name = s.name
		                  AND sr.resolution IN ('pending','ambiguous','unresolved')),
		       EXISTS (SELECT 1 FROM edges o INDEXED BY idx_edges_src
		                WHERE o.index_run_id = s.index_run_id
		                  AND o.src_id = s.id
		                  AND o.kind IN ('IMPLEMENTS','EXTENDS'))
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ?
		   AND s.id > ?
		   AND s.kind IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)
		   AND NOT EXISTS (SELECT 1 FROM edges e INDEXED BY idx_edges_dst
		                    WHERE e.index_run_id = s.index_run_id
		                      AND e.dst_id = s.id
		                      AND e.kind <> 'CONTAINS'
		                      AND e.src_id <> s.id)
		 ORDER BY s.id
		 LIMIT ?`
}

// UnreferencedSymbols returns up to limit definitions with no incoming
// relationship, in ascending id order starting after afterID.
//
// Keyset pagination on the primary key, for the reason references.go gives:
// OFFSET rescans everything it skips, and the caller here pages until it has
// collected enough rows that survive its own filters.
//
// "No incoming relationship" excludes CONTAINS, which records nesting rather
// than use, and excludes a symbol's edges to itself, so plain recursion does
// not keep a function alive.
//
// Both EXISTS probes are index lookups: idx_edges_dst (index_run_id, dst_id,
// kind) for the incoming check and idx_refs_run_name (index_run_id, name) for
// the unbound-name check. Neither scans.
func (r *Registry) UnreferencedSymbols(ctx context.Context, runID, afterID int64, limit int) ([]UnreferencedSymbol, error) {
	if limit <= 0 {
		limit = 500
	}
	args := make([]any, 0, len(unreferencedKinds)+3)
	args = append(args, runID, afterID)
	for _, k := range unreferencedKinds {
		args = append(args, k)
	}
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, unreferencedSQL(len(unreferencedKinds)), args...)
	if err != nil {
		return nil, fmt.Errorf("registry: unreferenced symbols for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []UnreferencedSymbol
	for rows.Next() {
		var u UnreferencedSymbol
		if err := rows.Scan(&u.ID, &u.Kind, &u.Name, &u.FQN, &u.FilePath, &u.Language,
			&u.StartLine, &u.EndLine, &u.Exported,
			&u.PartialParse, &u.NameUnbound, &u.Overrides); err != nil {
			return nil, fmt.Errorf("registry: scan unreferenced symbol: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FileStructure is what the index knows about one file's size and how much of
// the rest of the repository depends on it.
type FileStructure struct {
	Path      string
	Language  string
	SizeBytes int64
	Symbols   int

	// Dependents counts relationships that start in ANOTHER file and end at a
	// symbol defined here. It is a count of edges, not of distinct files, so a
	// file called from one place a hundred times scores as high as one called
	// from a hundred places; the report says "inbound relationships", never
	// "dependent files".
	Dependents int
}

// structureChunk bounds one IN list. SQLite's default variable limit is 32766;
// this stays far below it and keeps each statement's plan a handful of index
// probes.
const structureChunk = 400

// fileStructureSQL is the per-file structure query for n paths.
//
// The join order in the dependents subquery is FORCED (CROSS JOIN and INDEXED
// BY), not left to the planner. Left alone it started from edges, reading every
// edge in the run once per file: correct, and on this repository's own
// 19,000-edge index slow enough that `repocortex hotspots` did not return in two
// minutes. The intended plan starts from the file's own symbols
// (idx_symbols_file) and probes idx_edges_dst for each, so the cost follows the
// file, not the repository. TestFileStructureForUsesTheDestinationIndex pins
// it, because a plan is exactly the kind of thing that regresses silently.
func fileStructureSQL(n int) string {
	return `
		SELECT f.path, f.language, f.size_bytes,
		       (SELECT COUNT(*) FROM symbols s WHERE s.file_id = f.id),
		       (SELECT COUNT(*)
		          FROM symbols d
		          CROSS JOIN edges e INDEXED BY idx_edges_dst
		                  ON e.index_run_id = f.index_run_id AND e.dst_id = d.id
		          CROSS JOIN symbols sr ON sr.id = e.src_id
		         WHERE d.file_id = f.id
		           AND sr.file_id <> f.id
		           AND e.kind <> 'CONTAINS')
		  FROM files f
		 WHERE f.index_run_id = ?
		   AND f.path IN (` + strings.TrimSuffix(strings.Repeat("?,", n), ",") + `)`
}

// FileStructureFor returns structure for the named files that exist in runID.
//
// The caller names the paths, so memory follows the size of the question (the
// files a commit window touched) rather than the size of the repository. Paths
// the run does not contain are simply absent from the result — a file deleted
// since the commit that touched it is the ordinary case.
func (r *Registry) FileStructureFor(ctx context.Context, runID int64, paths []string) (map[string]FileStructure, error) {
	out := make(map[string]FileStructure, len(paths))
	for start := 0; start < len(paths); start += structureChunk {
		end := min(start+structureChunk, len(paths))
		chunk := paths[start:end]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, runID)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := r.db.QueryContext(ctx, fileStructureSQL(len(chunk)), args...)
		if err != nil {
			return nil, fmt.Errorf("registry: file structure for run %d: %w", runID, err)
		}
		for rows.Next() {
			var fs FileStructure
			if err := rows.Scan(&fs.Path, &fs.Language, &fs.SizeBytes, &fs.Symbols, &fs.Dependents); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("registry: scan file structure: %w", err)
			}
			out[fs.Path] = fs
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// EdgeBetween returns the strongest relationship from src to dst, or false when
// none exists. It is how a path of symbol ids is turned back into evidence: the
// traversal returns ids, and a path an agent cannot check hop by hop is an
// assertion rather than an answer.
func (r *Registry) EdgeBetween(ctx context.Context, runID, srcID, dstID int64) (EdgeHit, bool, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.src_id, src.name, srcf.path, e.dst_id, dst.name,
		       e.kind, e.confidence, e.source, COALESCE(e.detail,''),
		       e.evidence_file, e.evidence_line, e.evidence_col
		  FROM edges e
		  JOIN symbols src  ON src.id = e.src_id
		  JOIN files   srcf ON srcf.id = src.file_id
		  JOIN symbols dst  ON dst.id = e.dst_id
		 WHERE e.index_run_id = ? AND e.src_id = ? AND e.dst_id = ?
		 ORDER BY `+fmt.Sprintf(confidenceRankCase, "e.confidence")+` DESC, e.kind, e.evidence_line
		 LIMIT 1`, runID, srcID, dstID)
	if err != nil {
		return EdgeHit{}, false, fmt.Errorf("registry: edge %d -> %d: %w", srcID, dstID, err)
	}
	defer rows.Close()

	if !rows.Next() {
		return EdgeHit{}, false, rows.Err()
	}
	var e EdgeHit
	if err := rows.Scan(&e.SrcID, &e.SrcName, &e.SrcPath, &e.DstID, &e.DstName,
		&e.Kind, &e.Confidence, &e.Source, &e.Detail,
		&e.EvidenceFile, &e.EvidenceLine, &e.EvidenceCol); err != nil {
		return EdgeHit{}, false, fmt.Errorf("registry: scan edge: %w", err)
	}
	return e, true, rows.Err()
}

// SymbolByID returns one symbol with its file path, or false when the run does
// not hold it.
func (r *Registry) SymbolByID(ctx context.Context, runID, id int64) (SymbolHit, bool, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn, s.kind, f.path, s.start_line, s.end_line,
		       s.confidence, s.source, s.exported
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.id = ?`, runID, id)
	if err != nil {
		return SymbolHit{}, false, fmt.Errorf("registry: symbol %d: %w", id, err)
	}
	defer rows.Close()

	if !rows.Next() {
		return SymbolHit{}, false, rows.Err()
	}
	var h SymbolHit
	if err := rows.Scan(&h.ID, &h.Name, &h.FQN, &h.Kind, &h.FilePath, &h.StartLine, &h.EndLine,
		&h.Confidence, &h.Source, &h.Exported); err != nil {
		return SymbolHit{}, false, fmt.Errorf("registry: scan symbol: %w", err)
	}
	return h, true, rows.Err()
}

// Mention is one file whose indexed text uses an identifier in code.
type Mention struct {
	Path string
	// Lines are the 1-based lines on which the identifier occurs as a whole
	// word outside a comment-only line, in ascending order, at most
	// mentionLineCap of them.
	Lines []int
}

const (
	// mentionFileCap bounds how many candidate files one identifier's text is
	// read from. The question a caller asks is "does this name appear anywhere
	// besides its definition", which is answered by the first few files; a
	// name common enough to exceed the cap is certainly mentioned.
	mentionFileCap = 12
	// mentionLineCap bounds the lines recorded per file.
	mentionLineCap = 64
)

// IdentifierMentions reports where name occurs as a whole identifier in the
// indexed text, on lines that are not comments.
//
// It exists because the symbol graph records CALLS far more reliably than it
// records a function used as a value — passed as a callback, stored in a
// table, registered with a framework. Those uses leave no edge, and they do
// leave the identifier in the text. A caller deciding whether a definition is
// unused needs both.
//
// The full-text index finds candidate files; the lines are then read from the
// stored text with identifier boundaries, because the index's tokenizer stems
// and splits on underscores and would otherwise report `get_user` for a file
// that only contains `get` and `user`.
//
// COMMENT-ONLY LINES ARE SKIPPED, and the reason is a measured one: most
// languages' documentation conventions open a definition's comment with the
// definition's own name ("// orphan is never called"), so counting comments
// made every documented function look used. Detection is by the line's leading
// marker (see commentOnly), which is deliberately crude and deliberately
// one-sided: a commented-out call is read as no use, and a line of code is
// never mistaken for a comment unless it starts with one of those markers.
//
// The line a definition itself sits on is returned like any other; the caller
// knows where its definitions are and discounts them.
//
// ok is false when the answer is incomplete: the name has nothing the tokenizer
// can index (no letter or digit), or so many files matched that not all were
// read. Either way the caller must treat the name as unknown rather than as
// unmentioned.
func (r *Registry) IdentifierMentions(ctx context.Context, runID int64, name string) (mentions []Mention, ok bool, err error) {
	phrase := ftsPhrase(name)
	if phrase == "" {
		return nil, false, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, files_fts.content
		  FROM files_fts
		  JOIN files_fts_map m ON m.fts_rowid = files_fts.rowid
		  JOIN files f         ON f.id = m.file_id
		 WHERE files_fts MATCH ?
		   AND f.index_run_id = ?
		 LIMIT ?`, "content : "+phrase, runID, mentionFileCap)
	if err != nil {
		return nil, false, fmt.Errorf("registry: mentions of %q: %w", name, err)
	}
	defer rows.Close()

	files := 0
	for rows.Next() {
		var path, content string
		if err := rows.Scan(&path, &content); err != nil {
			return nil, false, fmt.Errorf("registry: scan mention: %w", err)
		}
		files++
		if lines := identifierLines(content, name); len(lines) > 0 {
			mentions = append(mentions, Mention{Path: path, Lines: lines})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	// The cap was reached, so files that were not read may hold the name. The
	// tokenizer folds case and stems, so a name like `builder` matches every
	// file that says strings.Builder, and the twelve returned need not include
	// the one that matters. That is "could not look everywhere", which the
	// contract reports as unknown.
	if files >= mentionFileCap {
		return mentions, false, nil
	}
	return mentions, true, nil
}

// identifierLines returns the lines of text on which name occurs as a whole
// identifier, skipping comment-only lines.
func identifierLines(text, name string) []int {
	var out []int
	line := 0
	for len(text) > 0 && len(out) < mentionLineCap {
		line++
		var cur string
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			cur, text = text[:i], text[i+1:]
		} else {
			cur, text = text, ""
		}
		if !strings.Contains(cur, name) || commentOnly(cur) {
			continue
		}
		if countIdentifier(cur, name) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// commentMarkers open a line that holds nothing but a comment, across the
// languages the index binds references for. `*` covers the continuation lines
// of a block comment, which is where Java and JavaScript documentation lives.
// Python docstrings are absent on purpose: they sit inside the definition they
// document, and the caller already discounts a definition's own lines.
var commentMarkers = []string{"//", "#", "*", "/*", "--", ";", "<!--", "%"}

func commentOnly(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	for _, m := range commentMarkers {
		if !strings.HasPrefix(trimmed, m) {
			continue
		}
		// `#include`, a Rust attribute and a C# directive are code: a `#`
		// followed by a letter, `[` or `!` is not treated as a comment.
		if m == "#" && len(trimmed) > 1 && (identByte(trimmed[1]) || trimmed[1] == '[' || trimmed[1] == '!') {
			return false
		}
		// A leading `*` is a comment continuation only when it is not a
		// pointer dereference: require a space, a slash or another `*`
		// after it.
		if m == "*" && len(trimmed) > 1 && trimmed[1] != ' ' && trimmed[1] != '/' && trimmed[1] != '*' {
			return false
		}
		return true
	}
	return false
}

// ftsPhrase renders name as an FTS5 phrase: its alphanumeric runs, quoted.
// Everything else is a separator to the unicode61 tokenizer, so `get_user`
// becomes the two-token phrase "get user". Returns "" when no run exists.
func ftsPhrase(name string) string {
	var b strings.Builder
	inRun := false
	wrote := false
	for _, r := range name {
		alnum := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
		if alnum {
			if !inRun && wrote {
				b.WriteByte(' ')
			}
			b.WriteRune(r)
			inRun, wrote = true, true
			continue
		}
		inRun = false
	}
	if !wrote {
		return ""
	}
	return `"` + b.String() + `"`
}

// countIdentifier counts occurrences of name in text that are not part of a
// longer identifier on either side.
func countIdentifier(text, name string) int {
	if name == "" {
		return 0
	}
	n := 0
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], name)
		if i < 0 {
			break
		}
		start := from + i
		end := start + len(name)
		if (start == 0 || !identByte(text[start-1])) && (end == len(text) || !identByte(text[end])) {
			n++
		}
		from = end
	}
	return n
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 128
}

// FanInSymbol is an exported function or method with its inbound edge count.
type FanInSymbol struct {
	ID       int64
	Name     string
	FQN      string
	Kind     string
	FilePath string
	// InDegree counts every relationship pointing at the symbol, of any kind
	// and confidence: the same figure IncomingEdgeCount returns for it.
	InDegree int
}

// HighFanInSymbols returns the exported functions and methods with at least
// minInDegree inbound relationships, the widest first, at most limit of them.
//
// ONE aggregate query, in place of a count query per symbol. The blindspot
// detector used to load every symbol in the run and ask for each exported one's
// in-degree separately, which is one round trip per exported function: 2.4
// seconds on a 5,000-symbol fixture and unbounded memory on a large one. This
// reads idx_edges_dst once and keeps the top `limit`.
//
// Ties break on id so the order — and therefore which symbols fall inside the
// limit — is the same on every run.
func (r *Registry) HighFanInSymbols(ctx context.Context, runID int64, minInDegree, limit int) ([]FanInSymbol, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn, s.kind, f.path, COUNT(*) AS in_degree
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		  CROSS JOIN edges e INDEXED BY idx_edges_dst
		          ON e.index_run_id = s.index_run_id AND e.dst_id = s.id
		 WHERE s.index_run_id = ?
		   AND s.exported = 1
		   AND s.kind IN ('function','method')
		 GROUP BY s.id
		HAVING COUNT(*) >= ?
		 ORDER BY in_degree DESC, s.id
		 LIMIT ?`, runID, minInDegree, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: high fan-in symbols for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []FanInSymbol
	for rows.Next() {
		var f FanInSymbol
		if err := rows.Scan(&f.ID, &f.Name, &f.FQN, &f.Kind, &f.FilePath, &f.InDegree); err != nil {
			return nil, fmt.Errorf("registry: scan fan-in symbol: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
