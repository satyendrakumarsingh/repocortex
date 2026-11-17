package scipimport

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Package scipimport attaches a pre-built SCIP index to a repository RepoCortex
// has already indexed.
//
// IT IS A SEPARATE PACKAGE FROM internal/resolve/scip ON PURPOSE. That package
// documents, and depends on, performing no database writes: it returns values
// shaped for the registry and lets the caller decide what a transaction is,
// because ingesting a 200 MB index inside one would hold a write lock for
// minutes. Landing the facts is a different job with a different dependency,
// so it lives one layer up rather than eroding that boundary.
//
// This is also a different problem from Ingest. Ingest converts a .scip into
// facts. Import has to attach those facts to a repository already indexed with
// tree-sitter, where every symbol has a rowid and a heuristic story about what
// it relates to. The compiler's view and the parser's view must be reconciled
// before a single edge can be written.
//
// WHAT MAKES THAT HARD: the two sides do not agree on names. A SCIP symbol
// string is a structured, tool-specific identifier; RepoCortex's tree-sitter FQN
// is `path:Name`. They will never match textually, so the join is positional —
// a definition at a file, a name and a line — which is the one thing both sides
// observe directly from the same source text.
//
// WHAT THIS DELIBERATELY DOES NOT DO: insert symbols. registry.NewWriter
// refuses a committed run on purpose, because writing symbols into what readers
// are currently looking at is what the two-phase swap exists to prevent. So a
// compiler definition with no counterpart in the index is REPORTED, never
// invented, and the edges that needed it are counted as unattached rather than
// quietly dropped. An import that matches nothing must look like a failure, not
// like a repository with no relationships.

// ImportResult is the full report of an import.
//
// It is verbose on purpose. The characteristic failure of this path is silence:
// an index built in a container with different paths, or against a different
// commit, produces zero edges and no error, and is indistinguishable from a
// repository that genuinely has no cross-references. Every number here exists to
// make one of those explanations checkable.
type ImportResult struct {
	// Ingest is the parse-side report: documents seen, matched, unmatched.
	Ingest scip.Stats

	// Edges is what reached the registry.
	Edges registry.UpgradeStats

	// SymbolsMatched is compiler definitions joined to an existing indexed
	// symbol. SymbolsUnmatched is those with no counterpart.
	SymbolsMatched   int
	SymbolsUnmatched int

	// UnmatchedSymbols is a bounded sample of unmatched definitions, as
	// "path:line name", in encounter order.
	UnmatchedSymbols []string

	// RelationsUnresolved counts references whose target could not be resolved
	// to a symbol in this repository. Most are genuine — a call into a
	// third-party package RepoCortex never indexed — so this is reported rather
	// than treated as an error.
	RelationsUnresolved int

	// Reattributed counts relations whose source the compiler named as a
	// definition this index does not hold, or as the file itself, written as
	// edges from the enclosing indexed definition or from the file's synthetic
	// caller (registry.FileCallerKind).
	Reattributed int

	// FileAnchored counts relations SCIP attributed to a file rather than to an
	// enclosing definition. RepoCortex's index has no file-scope symbols, so
	// these have no source node to hang off and are skipped.
	FileAnchored int64

	// ParsedCallSites is how many distinct positions the parser recorded a call
	// at, across the whole run. It is the other half of the CALLS join, and it
	// is reported because a zero here explains a zero in Promote completely: no
	// parser facts means nothing to join against, not a compiler index that
	// disagreed.
	ParsedCallSites int

	// Promote is what the CALLS join did. See scip.PromoteCalls.
	Promote scip.PromoteStats

	// Contradicted is heuristic call edges withdrawn because the compiler
	// resolved the same call, on the same line, to something else.
	Contradicted int

	// Retracted is what an earlier pass of the same producer had written and
	// this one no longer proves.
	Retracted registry.RetractStats

	// InstalledCopies names the packages the repository itself provides that the
	// indexer resolved to an installed copy instead of this checkout.
	InstalledCopies []string

	// Resolutions is what the compiler settled in symbol_references — the
	// column that answers "what does this name refer to", which until now only
	// the heuristic resolver ever wrote. See registry.MarkCompilerResolutions.
	Resolutions registry.CompilerResolutionStats
}

// maxUnmatchedSymbolSamples bounds the retained sample. Enough to see a pattern
// — one wrong directory prefix repeats immediately — without holding a slice
// proportional to a wholly-mismatched index.
const maxUnmatchedSymbolSamples = 20

// Summary renders the result as the few lines that decide whether the import
// worked.
//
// Deliberately NOT Stats.Summary: that one inlines a sample of unmatched
// document paths, which on a real index is twenty lines of Go build-cache
// filenames and buries the numbers underneath them. Samples belong in the
// guidance that explains what to do about them, printed once.
func (r ImportResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d document(s), %d matched\n",
		r.Ingest.Tool, r.Ingest.Documents, r.Ingest.DocumentsMatched)
	fmt.Fprintf(&b, "symbols: %d matched, %d with no counterpart in the index\n",
		r.SymbolsMatched, r.SymbolsUnmatched)
	fmt.Fprintf(&b, "edges:   %d new, %d upgraded from heuristic, %d already known\n",
		r.Edges.Inserted, r.Edges.Upgraded, r.Edges.Unchanged)
	fmt.Fprintf(&b, "calls:   %d compiler reference(s) sit on a parsed call line and became "+
		"CALLS at 'exact'\n", r.Promote.Promoted)
	if r.Promote.Ambiguous > 0 {
		fmt.Fprintf(&b, "         %d call(s) left as REFERENCES: the compiler's occurrences "+
			"of that name on the line could not be paired one to one with the calls the "+
			"parser recorded (a method value beside a call, or a call resolved outside the repository)\n",
			r.Promote.Ambiguous)
	}
	if r.Promote.Receivers > 0 {
		fmt.Fprintf(&b, "         %d call line(s) left as REFERENCES: the compiler's target "+
			"there is what a call on that line was written against, not what was called\n",
			r.Promote.Receivers)
	}
	if r.Resolutions.Bound > 0 {
		fmt.Fprintf(&b, "refs:    %d reference(s) were settled by the compiler (%d the resolver "+
			"had found no candidate for, %d it could not choose between, %d it had bound to "+
			"another definition)\n",
			r.Resolutions.Bound, r.Resolutions.FromUnresolved, r.Resolutions.FromAmbiguous,
			r.Resolutions.Corrected)
	}
	if r.Reattributed > 0 {
		fmt.Fprintf(&b, "         %d relation(s) the compiler placed outside any definition the "+
			"index holds were attached to the enclosing definition or to the file's top level\n",
			r.Reattributed)
	}
	if r.Promote.NameMismatch > 0 {
		fmt.Fprintf(&b, "         %d call line(s) left as REFERENCES: the compiler's target "+
			"there is not named as any call the parser recorded on that line\n",
			r.Promote.NameMismatch)
	}
	if r.RelationsUnresolved > 0 {
		fmt.Fprintf(&b, "         %d reference(s) point outside the indexed repository\n",
			r.RelationsUnresolved)
	}
	if r.FileAnchored > 0 {
		fmt.Fprintf(&b, "         %d reference(s) the indexer attributed to a file rather than "+
			"a definition were skipped\n", r.FileAnchored)
	}
	return b.String()
}

// MatchRate is the share of the index's documents that reconciled against an
// indexed file. It is the single number that separates "this index describes
// this repository" from "this index describes something else".
//
// Returns 0 when nothing could be verified, which is itself the answer.
func (r ImportResult) MatchRate() float64 {
	if !r.Ingest.PathsVerified || r.Ingest.Documents == 0 {
		return 0
	}
	return float64(r.Ingest.DocumentsMatched) / float64(r.Ingest.Documents)
}

// symbolKey joins the compiler's view to the parser's.
//
// Path, name and start line. Not the FQN, which the two sides construct
// differently by design, and not the range, whose end the two disagree about
// (SCIP records a definition's extent, tree-sitter the declaration node). The
// start line is where both are looking at the same token.
type symbolKey struct {
	path string
	name string
	line int
}

// symbolTable is the repository's indexed symbols, keyed for matching.
type symbolTable struct {
	byKey map[symbolKey]int64
	// byPathName resolves a name that matched no exact line, but ONLY when it
	// is unique in its file. Indexers and parsers occasionally disagree by a
	// line on a decorated or annotated declaration; they never disagree about
	// which file a definition is in. Ambiguous names are left out entirely
	// rather than guessed between, because picking one of two overloads is how
	// an "exact" edge ends up pointing at the wrong function.
	byPathName map[string][]int64
}

func newSymbolTable(syms []model.Symbol, pathOf map[int64]string) *symbolTable {
	t := &symbolTable{
		byKey:      make(map[symbolKey]int64, len(syms)),
		byPathName: make(map[string][]int64, len(syms)),
	}
	for _, s := range syms {
		path, ok := pathOf[s.FileID]
		if !ok || s.Kind == model.KindFile {
			continue
		}
		t.byKey[symbolKey{path: path, name: s.Name, line: s.Range.Start.Line}] = s.ID
		pn := path + "\x00" + s.Name
		t.byPathName[pn] = append(t.byPathName[pn], s.ID)
	}
	return t
}

// lookup finds the indexed symbol a compiler definition refers to.
func (t *symbolTable) lookup(path, name string, line int) (int64, bool) {
	if id, ok := t.byKey[symbolKey{path: path, name: name, line: line}]; ok {
		return id, true
	}
	if ids := t.byPathName[path+"\x00"+name]; len(ids) == 1 {
		return ids[0], true
	}
	return 0, false
}

// lookupMode is lookup, optionally without the by-name fallback.
func (t *symbolTable) lookupMode(path, name string, line int, strict bool) (int64, bool) {
	if !strict {
		return t.lookup(path, name, line)
	}
	id, ok := t.byKey[symbolKey{path: path, name: name, line: line}]
	return id, ok
}

// Importer attaches a pre-built SCIP index to an already-indexed repository.
type Importer struct {
	reg   *registry.Registry
	runID int64
	opts  scip.Options
	proof *Proof

	shared  *SharedTables
	callers map[string]int64
}

// Collect makes Import record into p which files the index covers and where it
// resolved a name outside the repository. See Proof for what that is for, and
// why an index the user supplied must not be collected.
func (imp *Importer) Collect(p *Proof) *Importer {
	imp.proof = p
	return imp
}

// NewImporter returns an Importer targeting one committed run.
//
// opts.KnownPaths is filled in from the registry if the caller left it nil,
// because an import that skips path verification cannot report whether it
// matched anything — and that report is the only defence against the silent
// failure this path is prone to.
func NewImporter(reg *registry.Registry, runID int64, opts scip.Options) *Importer {
	return &Importer{reg: reg, runID: runID, opts: opts}
}

// Import reads the .scip file at path and writes its facts into the run.
//
// TWO PASSES OVER THE FILE, and the reason is structural rather than lazy: a
// reference in one document points at a definition in another, so no edge can
// be resolved until every document's definitions have been seen. The
// alternative — holding every document's relations in memory — would undo the
// streaming design that bounds Ingest's memory by the largest single document.
// Reading the file twice is the cheaper resource to spend.
func (imp *Importer) Import(ctx context.Context, path string) (ImportResult, error) {
	return imp.run(ctx, runMode{
		label:   "scip import",
		promote: true,
		proof:   imp.proof,
		ingest: func(ctx context.Context, known []string, emit scip.EmitFunc) (scip.Stats, error) {
			opts := imp.opts
			if opts.KnownPaths == nil {
				opts.KnownPaths = known
			}
			opts.KeepSites = imp.proof != nil
			return scip.New(opts).IngestFile(ctx, path, emit)
		},
	})
}

// runMode is what differs between landing a SCIP file and landing facts that
// are already in memory.
type runMode struct {
	label string
	// promote joins compiler references to the parser's call lines. Facts that
	// already say whether a use is a call do not need it.
	promote bool
	// strict refuses the by-name fallback when matching a definition to an
	// indexed symbol: a definition must match on path, name AND line.
	strict bool
	// detail, when set, is the evidence detail this run writes; edges an
	// earlier run wrote under it and this one did not are retracted.
	detail string
	// sites are every call the compiler resolved in the files it checked, so
	// that a heuristic edge the compiler contradicts can be withdrawn. Used
	// with language: the language of the files the sites describe.
	sites    []CallSite
	language string
	// proof, when set, collects coverage and out-of-repository sites.
	proof *Proof
	// restore, when set, limits the run to the edges Restore describes.
	restore *RestoreSpec
	// ingest streams the facts, given the repository's indexed paths.
	ingest func(ctx context.Context, known []string, emit scip.EmitFunc) (scip.Stats, error)
}

// ImportFacts lands compiler facts that are already in memory, through the
// same reconciliation, validation and writer a SCIP file goes through.
//
// It is the second producer of exact edges and deliberately not a second
// writer: the facts are shaped as scip.FileFacts, so every edge is built by
// scip.Relation.Edge (confidence=exact, source=compiler, validated), matched to
// indexed symbols by path, name and line, and written by Registry.UpgradeEdges.
// Nothing is written to a temporary file because nothing needs to be: the SCIP
// route exists to carry facts between processes, and here there is one.
//
// The facts must already say whether each use is a call (their relation kind
// is final), so the parser's call lines are not consulted. Every edge the run
// wrote under detail by an earlier pass and this one did not is retracted, so
// an incremental index followed by resolve leaves no stale exact edge.
func (imp *Importer) ImportFacts(ctx context.Context, files []scip.FileFacts, sites []CallSite, tool scip.Tool, detail string) (ImportResult, error) {
	return imp.run(ctx, runMode{
		label:    "compiler facts",
		strict:   true,
		detail:   detail,
		sites:    sites,
		language: "go",
		ingest: func(ctx context.Context, _ []string, emit scip.EmitFunc) (scip.Stats, error) {
			st := scip.Stats{Tool: tool, PathsVerified: true}
			for i := range files {
				st.Documents++
				st.DocumentsMatched++
				if err := emit(ctx, &files[i]); err != nil {
					return st, err
				}
			}
			return st, nil
		},
	})
}

// RestoreSpec says which documents of a cached index may be trusted again after
// an incremental refresh replaced some files.
type RestoreSpec struct {
	// Fresh reports whether the document for path still describes the file as it
	// is now: the file's content is what the index was built from. A document
	// that is not fresh has moved positions and is never the evidence of an edge.
	Fresh func(path string) bool
}

// Restore re-imports the index at path for the one purpose of putting back the
// proven edges a file replacement deleted, and writes nothing else.
//
// WHY IT IS NOT Import. Replacing a changed file deletes its symbols, and every
// edge into them goes with them, including the compiler-proven edges from files
// that did not change. Running the whole index again would also be correct, but
// it would write every edge of the project to learn that most are there. Here
// an edge is written only when its source sits in a document that is still
// fresh and its target is a definition in a document that is not: the edges the
// replacement could have removed.
//
// The definitions of a stale document are still read, for one reason. They are
// the targets. Each is matched to the file's current symbols as an import
// matches any definition (path, name and line, else the name when it is unique
// in the file), so a function that moved down a few lines is found and one that
// was deleted matches nothing: the edge is left as the heuristic resolver
// bound it. No other fact of a stale document is used: not its references (the
// positions have moved), not its relationships.
//
// Nothing here is collected into a Proof: coverage rows belong to the pass that
// ran the indexer, and a replaced file already lost its row with the file.
func (imp *Importer) Restore(ctx context.Context, path string, spec RestoreSpec) (ImportResult, error) {
	return imp.run(ctx, runMode{
		label:   "scip restore",
		promote: true,
		restore: &spec,
		ingest: func(ctx context.Context, known []string, emit scip.EmitFunc) (scip.Stats, error) {
			opts := imp.opts
			if opts.KnownPaths == nil {
				opts.KnownPaths = known
			}
			return scip.New(opts).IngestFile(ctx, path, emit)
		},
	})
}

// tables is what an import reads about the repository's own index before it
// touches the compiler's: every indexed path, the symbols keyed for matching
// and the parser's call sites. None of it changes while indexes are imported
// (an import writes edges and marks references, never symbols or the parser's
// references), so several imports of one run can read it once.
type tables struct {
	// fileIDs and spans are what attributes a compiler relation whose source is
	// not an indexed definition: the file it is in, and where each indexed
	// definition of that file starts and ends.
	fileIDs map[string]int64
	spans   map[string][]defSpan
	known   []string
	table   *symbolTable
	calls   *scip.CallSiteSet
	nameOf  map[int64]string
	kindOf  map[int64]model.EntityKind
	symbols int
}

// defSpan is one indexed definition's extent, for finding what encloses a line.
type defSpan struct {
	id         int64
	start, end int
}

// enclosingDef finds the innermost indexed definition of path whose span holds
// line, the way the resolver does (infer.enclosing): spans are ordered by start
// line then by end line descending, so the last match is the innermost, and a
// span of no recorded end covers only its own line.
func (t *tables) enclosingDef(path string, line int) (int64, bool) {
	var best int64
	var found bool
	for _, s := range t.spans[path] {
		if s.start > line {
			break
		}
		end := s.end
		if end == 0 {
			end = s.start
		}
		if line <= end {
			best, found = s.id, true
		}
	}
	return best, found
}

// SharedTables lets the importers of one run read the repository's tables once.
// The zero value is ready; share one between Importers with ShareTables, and do
// not share it across runs or across a write of symbols.
type SharedTables struct{ t *tables }

// ShareTables makes this importer read, and fill, the shared tables.
func (imp *Importer) ShareTables(s *SharedTables) *Importer {
	imp.shared = s
	return imp
}

func (imp *Importer) loadTables(ctx context.Context, mode runMode) (*tables, error) {
	if imp.shared != nil && imp.shared.t != nil && (imp.shared.t.calls != nil || !mode.promote) {
		return imp.shared.t, nil
	}
	files, err := imp.reg.Files(ctx, imp.runID)
	if err != nil {
		return nil, fmt.Errorf(mode.label+": read indexed files: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf(mode.label+": run %d has no indexed files; "+
			"run 'repocortex index' before importing", imp.runID)
	}
	pathOf := make(map[int64]string, len(files))
	known := make([]string, 0, len(files))
	for _, f := range files {
		pathOf[f.ID] = f.Path
		known = append(known, f.Path)
	}

	syms, err := imp.reg.Symbols(ctx, imp.runID)
	if err != nil {
		return nil, fmt.Errorf(mode.label+": read indexed symbols: %w", err)
	}
	tb := &tables{known: known, table: newSymbolTable(syms, pathOf), symbols: len(syms),
		fileIDs: make(map[string]int64, len(files)), spans: make(map[string][]defSpan, len(files))}
	for _, f := range files {
		tb.fileIDs[f.Path] = f.ID
	}
	for i := range syms {
		if syms[i].Kind == model.KindFile {
			continue
		}
		path := pathOf[syms[i].FileID]
		tb.spans[path] = append(tb.spans[path], defSpan{
			id: syms[i].ID, start: syms[i].Range.Start.Line, end: syms[i].Range.End.Line})
	}
	for _, sp := range tb.spans {
		sort.SliceStable(sp, func(i, j int) bool {
			if sp[i].start != sp[j].start {
				return sp[i].start < sp[j].start
			}
			return sp[i].end > sp[j].end
		})
	}

	// The parser's half of the CALLS join, read once for the whole import.
	// Loaded here rather than queried per occurrence because an import touches
	// hundreds of thousands of them; see scip.CallSiteSet.
	if mode.promote {
		sites, err := imp.reg.ParsedCallSites(ctx, imp.runID)
		if err != nil {
			return nil, fmt.Errorf(mode.label+": read parsed call sites: %w", err)
		}
		tb.calls = scip.NewCallSiteSet(len(sites))
		for _, cs := range sites {
			tb.calls.AddAt(cs.Path, cs.Line, cs.Col, cs.Name, cs.Receiver)
		}
	}

	// RepoCortex's own name for every indexed symbol, so the promotion can check
	// that the compiler's target on a call line is named as the call the parser
	// recorded. Built from syms, which is already in memory.
	tb.nameOf = make(map[int64]string, len(syms))
	tb.kindOf = make(map[int64]model.EntityKind, len(syms))
	for i := range syms {
		tb.nameOf[syms[i].ID] = syms[i].Name
		tb.kindOf[syms[i].ID] = syms[i].Kind
	}
	if imp.shared != nil {
		imp.shared.t = tb
	}
	return tb, nil
}

func (imp *Importer) run(ctx context.Context, mode runMode) (ImportResult, error) {
	var res ImportResult

	tb, err := imp.loadTables(ctx, mode)
	if err != nil {
		return res, err
	}
	known, table, calls, nameOf, kindOf := tb.known, tb.table, tb.calls, tb.nameOf, tb.kindOf
	if calls != nil {
		res.ParsedCallSites = calls.Len()
	}

	// Pass 1: join compiler definitions to indexed symbols.
	fqnToID := make(map[string]int64, tb.symbols)
	checked := map[string]bool{}
	// Restore only: the definitions of documents that are no longer fresh, which
	// are the targets whose incoming edges may have been deleted.
	var restoreDst map[int64]struct{}
	if mode.restore != nil {
		restoreDst = map[int64]struct{}{}
	}
	// Every definition the index makes, matched to the repository's symbols or
	// not: an occurrence resolved to one is a use of repository code.
	// Mapped to where it is defined and what it is.
	definedAll := map[string]definition{}
	// The packages the index's occurrences name, and those its documents define.
	seenPkgs, definedPkgs := map[string]struct{}{}, map[string]struct{}{}
	st, err := mode.ingest(ctx, known, func(_ context.Context, f *scip.FileFacts) error {
		checked[f.Path] = true
		if mode.proof != nil {
			mode.proof.cover(f.Path, f.Tool.Name, f.Packages)
			for _, pk := range f.Packages {
				definedPkgs[normalizePackage(pk)] = struct{}{}
			}
			for i := range f.Sites {
				if pk := f.Sites[i].Package; pk != "" {
					seenPkgs[normalizePackage(pk)] = struct{}{}
				}
			}
		}
		for i := range f.Symbols {
			s := &f.Symbols[i]
			if s.Kind != model.KindFile && mode.proof != nil {
				definedAll[s.FQN] = definition{path: f.Path, kind: s.Kind}
			}
			if s.Kind == model.KindFile {
				// The file-scope anchor. It has no counterpart in an index
				// whose symbols all come from declarations, and NewWriter
				// refuses to add one to a committed run, so relations rooted
				// here cannot be attached. Counted below via Stats.FileAnchored
				// rather than silently discarded.
				continue
			}
			// A document-scoped symbol (a variable, a parameter, a function written
			// inside another) joins on its exact line only: it shares its name with
			// whatever the file declares at its top level, and the by-name fallback
			// would give `const match = ...` inside a function the identity of the
			// function `match` beside it, so that a call of the local became an exact
			// edge to the function.
			id, ok := table.lookupMode(f.Path, s.Name, s.Range.Start.Line, mode.strict || isLocalFQN(s.FQN))
			if !ok {
				res.SymbolsUnmatched++
				if len(res.UnmatchedSymbols) < maxUnmatchedSymbolSamples {
					res.UnmatchedSymbols = append(res.UnmatchedSymbols,
						fmt.Sprintf("%s:%d %s", f.Path, s.Range.Start.Line, s.Name))
				}
				continue
			}
			res.SymbolsMatched++
			fqnToID[s.FQN] = id
			if restoreDst != nil && !mode.restore.Fresh(f.Path) {
				restoreDst[id] = struct{}{}
			}
			if mode.proof != nil {
				mode.proof.matchedSymbol(id)
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Ingest = st
	res.FileAnchored = st.FileAnchored

	// A package the repository provides, which an occurrence names although no
	// document of the index defines it, is an installed copy of the repository's
	// own library: the indexer resolved `import click` to site-packages, and that
	// copy may be another version than this checkout (the installed click 8.1.8
	// has no Option.process_value, which the checkout overrides). What the
	// compiler says about such a position describes that copy, so it is neither
	// a proof that the call is outside the repository nor one that it is inside.
	if mode.proof != nil {
		for pk := range seenPkgs {
			if _, defined := definedPkgs[pk]; !defined && mode.proof.provided(pk) {
				res.InstalledCopies = append(res.InstalledCopies, pk)
			}
		}
		sort.Strings(res.InstalledCopies)
		mode.proof.setInstalled(res.InstalledCopies)
	}

	if res.SymbolsMatched == 0 {
		return res, fmt.Errorf(mode.label+": none of the index's %d definition(s) matched a symbol "+
			"in this repository — the index was probably built from a different commit or with "+
			"different paths (%s)", res.SymbolsUnmatched, st.Summary())
	}

	// Pass 2: resolve relations against the completed table and write edges.
	lookup := func(fqn string) (int64, bool) { id, ok := fqnToID[fqn]; return id, ok }
	var pending []model.Edge

	var written map[registry.EdgeKey]struct{}
	if mode.detail != "" {
		written = make(map[registry.EdgeKey]struct{}, 1<<16)
	}
	_, err = mode.ingest(ctx, known, func(ctx context.Context, f *scip.FileFacts) error {
		if mode.restore != nil && !mode.restore.Fresh(f.Path) {
			// Its positions describe an earlier version of the file.
			return nil
		}
		// Drop symbols that found no counterpart BEFORE resolving. FileFacts
		// resolves intra-file relations through LocalIDs, which reads the ids
		// on these very structs — and an unmatched symbol still carries its
		// negative placeholder. Leaving it in would produce an edge pointing at
		// a row id that does not exist. Removing it instead routes its
		// relations to the unresolved list, which is where they belong and
		// where they are already counted.
		kept := f.Symbols[:0]
		for i := range f.Symbols {
			s := f.Symbols[i]
			if s.Kind == model.KindFile {
				continue
			}
			id, ok := fqnToID[s.FQN]
			if !ok {
				continue
			}
			s.ID = id
			kept = append(kept, s)
		}
		f.Symbols = kept

		if mode.proof != nil {
			for _, site := range f.Sites {
				_, matched := fqnToID[site.DstFQN]
				d, defined := definedAll[site.DstFQN]
				mode.proof.site(f.Path, site, siteTarget{matched: matched, defined: defined, defPath: d.path, kind: d.kind})
			}
		}
		edges, unresolved, err := f.Edges(lookup)
		if err != nil {
			return fmt.Errorf("resolve edges for %s: %w", f.Path, err)
		}
		if !mode.strict && len(unresolved) > 0 {
			// A relation the compiler placed in a definition this index does not
			// hold (an arrow function in a describe() callback, a function nested
			// where the parser records no symbol) or in no definition at all (the
			// file anchor) is attributed the way the resolver attributes a
			// reference: to the innermost indexed definition whose lines hold it,
			// else to the file's own code. What counts as outside is the index's
			// view, not the compiler's, so the two producers agree on a line.
			local := f.LocalIDs()
			known := func(fqn string) (int64, bool) {
				if id, ok := local[fqn]; ok {
					return id, true
				}
				return lookup(fqn)
			}
			var refLines map[int]struct{}
			rest := unresolved[:0]
			for _, r := range unresolved {
				_, srcOK := known(r.SrcFQN)
				dst, dstOK := known(r.DstFQN)
				if srcOK || !dstOK || r.Kind == model.EdgeImports {
					rest = append(rest, r)
					continue
				}
				// Only a position the parser also recorded a reference at. An
				// import, an `export { x } from` and a declaration's own name are
				// occurrences to the compiler and are not uses written in code,
				// and a top level that "references" everything it imports would
				// make every unused import look used.
				if refLines == nil {
					lines, err := imp.reg.ReferenceLines(ctx, tb.fileIDs[f.Path])
					if err != nil {
						return err
					}
					refLines = lines
				}
				if _, ok := refLines[r.Evidence.Range.Start.Line]; !ok {
					rest = append(rest, r)
					continue
				}
				src, ok := tb.enclosingDef(f.Path, r.Evidence.Range.Start.Line)
				if !ok {
					id, err := imp.fileCaller(ctx, tb, f.Path)
					if err != nil {
						return err
					}
					src, ok = id, id != 0
				}
				if !ok {
					rest = append(rest, r)
					continue
				}
				e, err := r.Edge(src, dst)
				if err != nil {
					return err
				}
				edges = append(edges, e)
				res.Reattributed++
			}
			unresolved = rest
		}
		res.RelationsUnresolved += len(unresolved)

		// Promote per file, before the batch, and that is not an optimisation:
		// the unambiguity test is "how many distinct targets did the compiler
		// put on THIS line of THIS file", so it must see one file's edges
		// whole and nothing else. Running it over the flush batch would let a
		// second file's line N make the first file's line N look ambiguous.
		if mode.promote {
			ps, err := scip.PromoteCalls(f.Path, edges, nameOf, calls)
			if err != nil {
				return err
			}
			res.Promote.Add(ps)
			// A call whose target is a type constructs it, and the heuristic
			// resolver records that as INSTANTIATES, so that "what calls this
			// function" is answered from edges that point at callables. A
			// promoted edge follows the same rule.
			for i := range edges {
				if edges[i].Kind == model.EdgeCalls && kindOf[edges[i].DstID] == model.KindClass {
					edges[i].Kind = model.EdgeInstantiates
				}
			}
		}
		if restoreDst != nil {
			// After promotion, which has to see the line's edges whole: keep the
			// ones into a definition of a stale document, and no others.
			kept := edges[:0]
			for _, e := range edges {
				if _, ok := restoreDst[e.DstID]; ok {
					kept = append(kept, e)
				}
			}
			edges = kept
		}
		if written != nil {
			for i := range edges {
				e := &edges[i]
				written[registry.EdgeKey{Src: e.SrcID, Dst: e.DstID, Kind: string(e.Kind),
					File: e.Evidence.FilePath, Line: e.Evidence.Range.Start.Line}] = struct{}{}
			}
		}

		pending = append(pending, edges...)

		if len(pending) >= edgeFlushBatch {
			st, err := imp.reg.UpgradeEdges(ctx, imp.runID, pending)
			if err != nil {
				return err
			}
			addUpgrade(&res.Edges, st)
			pending = pending[:0]
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	if len(pending) > 0 {
		st, err := imp.reg.UpgradeEdges(ctx, imp.runID, pending)
		if err != nil {
			return res, err
		}
		addUpgrade(&res.Edges, st)
	}

	if written != nil {
		rs, err := imp.reg.RetractStale(ctx, imp.runID, mode.detail, written)
		if err != nil {
			return res, err
		}
		res.Retracted = rs
	}

	if len(mode.sites) > 0 {
		n, err := imp.withdrawContradicted(ctx, mode, fqnToID, checked)
		if err != nil {
			return res, err
		}
		res.Contradicted = n
	}

	// Last, and only once every edge is on disk: the reference-level join reads
	// the edges table, so running it per document would settle a reference
	// against a partial view and then never revisit it.
	marked, err := imp.reg.MarkCompilerResolutions(ctx, imp.runID)
	if err != nil {
		return res, err
	}
	res.Resolutions = marked

	sort.Strings(res.UnmatchedSymbols)
	return res, nil
}

// fileCaller returns the synthetic caller of a file, creating it on first use.
func (imp *Importer) fileCaller(ctx context.Context, tb *tables, path string) (int64, error) {
	if id, ok := imp.callers[path]; ok {
		return id, nil
	}
	fileID, ok := tb.fileIDs[path]
	if !ok {
		return 0, nil
	}
	id, err := imp.reg.FileCaller(ctx, imp.runID, fileID, path)
	if err != nil {
		return 0, err
	}
	if imp.callers == nil {
		imp.callers = map[string]int64{}
	}
	imp.callers[path] = id
	return id, nil
}

// edgeFlushBatch bounds how many edges are held before a write. Keeps peak
// memory proportional to the batch rather than to the index, matching the
// streaming posture of the ingester it sits on top of.
const edgeFlushBatch = 2000

func addUpgrade(dst *registry.UpgradeStats, src registry.UpgradeStats) {
	dst.Inserted += src.Inserted
	dst.Upgraded += src.Upgraded
	dst.Unchanged += src.Unchanged
}

// CallSite is what a compiler resolved one call's callee to, in a file it
// checked. Known is false when it could not say; DstFQN is the declaration it
// resolved to when that is one the facts define, else "".
type CallSite struct {
	Path   string
	Line   int
	Name   string
	Known  bool
	DstFQN string
}

type siteKey struct {
	path string
	line int
	name string
}

type siteVerdict struct {
	known bool
	dsts  map[int64]struct{}
}

// withdrawContradicted deletes the heuristic call edges the compiler proves
// false.
//
// A heuristic edge says "this function calls D at path:line". The compiler has
// resolved every call named D's name on that line, and when it knows what each
// one is and none is D, the edge is not a weaker claim, it is a false one, and
// leaving it beside the proven edges is how `find_callers` came to list
// callers that do not exist. The rule is deliberately conservative:
//
//   - the call site must be in a file the compiler checked, and every call of
//     that name on the line must have been RESOLVED by it (an unresolved one
//     could be D);
//   - D must itself be declared in a file the compiler checked, so that a
//     declaration this platform's build constraints excluded (which the same
//     call resolves to on another platform) is never judged.
func (imp *Importer) withdrawContradicted(ctx context.Context, mode runMode, fqnToID map[string]int64, checked map[string]bool) (int, error) {
	verdicts := make(map[siteKey]*siteVerdict, len(mode.sites))
	for _, s := range mode.sites {
		k := siteKey{s.Path, s.Line, s.Name}
		v := verdicts[k]
		if v == nil {
			v = &siteVerdict{known: true, dsts: map[int64]struct{}{}}
			verdicts[k] = v
		}
		if !s.Known {
			v.known = false
		}
		if s.DstFQN != "" {
			if id, ok := fqnToID[s.DstFQN]; ok {
				v.dsts[id] = struct{}{}
			} else {
				// A target the facts define but the index does not hold: the
				// heuristic's target might be it under another symbol.
				v.known = false
			}
		}
	}
	calls, err := imp.reg.HeuristicCalls(ctx, imp.runID, mode.language)
	if err != nil {
		return 0, err
	}
	var drop []int64
	for _, h := range calls {
		if !checked[h.File] || !checked[h.DstPath] {
			continue
		}
		v := verdicts[siteKey{h.File, h.Line, h.DstName}]
		if v == nil || !v.known {
			continue
		}
		if _, ok := v.dsts[h.DstID]; ok {
			continue
		}
		drop = append(drop, h.ID)
	}
	return imp.reg.DeleteEdges(ctx, drop)
}

// definition is where the index defines a symbol and what kind of symbol it is.
type definition struct {
	path string
	kind model.EntityKind
}

// isLocalFQN reports whether fqn is a document-scoped symbol as the ingester
// names one (see scip.qualifyFQN).
func isLocalFQN(fqn string) bool { return strings.Contains(fqn, "#local ") }
