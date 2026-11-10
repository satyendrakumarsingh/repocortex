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
		fmt.Fprintf(&b, "         %d call line(s) left as REFERENCES: the compiler resolved "+
			"more than one target there, so no single call target is defined\n",
			r.Promote.Ambiguous)
	}
	if r.Promote.Receivers > 0 {
		fmt.Fprintf(&b, "         %d call line(s) left as REFERENCES: the compiler's target "+
			"there is what a call on that line was written against, not what was called\n",
			r.Promote.Receivers)
	}
	if r.Resolutions.Bound > 0 {
		fmt.Fprintf(&b, "refs:    %d reference(s) the resolver could not bind were settled by "+
			"the compiler (%d it had found no candidate for, %d it could not choose between)\n",
			r.Resolutions.Bound, r.Resolutions.FromUnresolved, r.Resolutions.FromAmbiguous)
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
		if !ok {
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

// Importer attaches a pre-built SCIP index to an already-indexed repository.
type Importer struct {
	reg   *registry.Registry
	runID int64
	opts  scip.Options
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
	var res ImportResult

	files, err := imp.reg.Files(ctx, imp.runID)
	if err != nil {
		return res, fmt.Errorf("scip import: read indexed files: %w", err)
	}
	if len(files) == 0 {
		return res, fmt.Errorf("scip import: run %d has no indexed files; "+
			"run 'cortex index' before importing", imp.runID)
	}
	pathOf := make(map[int64]string, len(files))
	known := make([]string, 0, len(files))
	for _, f := range files {
		pathOf[f.ID] = f.Path
		known = append(known, f.Path)
	}

	syms, err := imp.reg.Symbols(ctx, imp.runID)
	if err != nil {
		return res, fmt.Errorf("scip import: read indexed symbols: %w", err)
	}
	table := newSymbolTable(syms, pathOf)

	// The parser's half of the CALLS join, read once for the whole import.
	// Loaded here rather than queried per occurrence because an import touches
	// hundreds of thousands of them; see scip.CallSiteSet.
	sites, err := imp.reg.ParsedCallSites(ctx, imp.runID)
	if err != nil {
		return res, fmt.Errorf("scip import: read parsed call sites: %w", err)
	}
	calls := scip.NewCallSiteSet(len(sites))
	for _, cs := range sites {
		calls.Add(cs.Path, cs.Line, cs.Name, cs.Receiver)
	}
	res.ParsedCallSites = calls.Len()

	// RepoCortex's own name for every indexed symbol, so the promotion can check
	// that the compiler's target on a call line is named as the call the parser
	// recorded. Built from syms, which is already in memory.
	nameOf := make(map[int64]string, len(syms))
	for i := range syms {
		nameOf[syms[i].ID] = syms[i].Name
	}

	opts := imp.opts
	if opts.KnownPaths == nil {
		opts.KnownPaths = known
	}
	ing := scip.New(opts)

	// Pass 1: join compiler definitions to indexed symbols.
	fqnToID := make(map[string]int64, len(syms))
	st, err := ing.IngestFile(ctx, path, func(_ context.Context, f *scip.FileFacts) error {
		for i := range f.Symbols {
			s := &f.Symbols[i]
			if s.Kind == model.KindFile {
				// The file-scope anchor. It has no counterpart in an index
				// whose symbols all come from declarations, and NewWriter
				// refuses to add one to a committed run, so relations rooted
				// here cannot be attached. Counted below via Stats.FileAnchored
				// rather than silently discarded.
				continue
			}
			id, ok := table.lookup(f.Path, s.Name, s.Range.Start.Line)
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
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	res.Ingest = st
	res.FileAnchored = st.FileAnchored

	if res.SymbolsMatched == 0 {
		return res, fmt.Errorf("scip import: none of the index's %d definition(s) matched a symbol "+
			"in this repository — the index was probably built from a different commit or with "+
			"different paths (%s)", res.SymbolsUnmatched, st.Summary())
	}

	// Pass 2: resolve relations against the completed table and write edges.
	lookup := func(fqn string) (int64, bool) { id, ok := fqnToID[fqn]; return id, ok }
	var pending []model.Edge

	_, err = ing.IngestFile(ctx, path, func(ctx context.Context, f *scip.FileFacts) error {
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

		edges, unresolved, err := f.Edges(lookup)
		if err != nil {
			return fmt.Errorf("resolve edges for %s: %w", f.Path, err)
		}
		res.RelationsUnresolved += len(unresolved)

		// Promote per file, before the batch, and that is not an optimisation:
		// the unambiguity test is "how many distinct targets did the compiler
		// put on THIS line of THIS file", so it must see one file's edges
		// whole and nothing else. Running it over the flush batch would let a
		// second file's line N make the first file's line N look ambiguous.
		ps, err := scip.PromoteCalls(f.Path, edges, nameOf, calls)
		if err != nil {
			return err
		}
		res.Promote.Add(ps)

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

// edgeFlushBatch bounds how many edges are held before a write. Keeps peak
// memory proportional to the batch rather than to the index, matching the
// streaming posture of the ingester it sits on top of.
const edgeFlushBatch = 2000

func addUpgrade(dst *registry.UpgradeStats, src registry.UpgradeStats) {
	dst.Inserted += src.Inserted
	dst.Upgraded += src.Upgraded
	dst.Unchanged += src.Unchanged
}
