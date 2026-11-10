package web

import (
	"context"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// structuralIndex is the whole committed run, folded into the shape the four
// screens actually ask for.
//
// # Why it is built once and cached
//
// Three of the four screens need facts no aggregate query answers: the
// blind-spot matrix is a cross-tabulation of edge confidence by (language, edge
// kind), the graph explorer needs container membership for every symbol, and the
// blast-radius facets need to know which impacted symbols write a table or sit
// behind an endpoint. All three derive from the same three whole-run reads
// (files, symbols, edges) that capability/architecture already performs, so
// running them per request would pay for the same scan several times per page
// load.
//
// # Why the raw rows are dropped
//
// model.Edge carries full evidence — file, line, column, source, detail — and a
// large repository has hundreds of thousands of them. Holding the slice as read
// keeps every column and every free-text detail alive for the process's
// lifetime. compactEdge keeps only what a portal renders (the two endpoints, the
// kind, the confidence, and the file:line that proves it) and interns the
// repeated strings, because "CALLS" appearing 80,000 times should be one
// allocation rather than 80,000.
//
// # Concurrency
//
// A structuralIndex is immutable once built and safe for concurrent readers. The
// cache around it holds exactly one run, keyed by run id, so an index swap
// invalidates it rather than serving a run CommitRun has already retired.
type structuralIndex struct {
	runID int64

	files map[int64]fileInfo
	syms  map[int64]symInfo
	edges []compactEdge

	// Language mix. Files and symbols are counted separately because they
	// answer different questions: a repository can be 60% YAML by file and 0%
	// by symbol, and only the second number bounds what the graph can say.
	fileLangs map[string]int
	symLangs  map[string]int

	// edgeConf is every edge by tier — the stacked bar on the overview.
	edgeConf Histogram

	// byKind and byLangKind are the blind-spot cross-tabulation.
	byKind     map[model.EdgeKind]*Histogram
	byLangKind map[langKind]*Histogram

	// Containers group symbols for the graph explorer: declared components when
	// the repository declares an architecture, directories when it does not.
	containerKind  string
	containers     map[string]*containerInfo
	containerOrder []string
	containerEdges map[containerPair]*aggregatedEdge

	// unassigned counts symbols no declared component matches. Zero when
	// containers are directories, since every file has one.
	unassigned int
}

type fileInfo struct {
	Path     string
	Language string

	// PartialParse marks a file the parser could not read completely. The run
	// reports only how MANY such files there were, so without this a reader is
	// told the repository holds 69 of them and given no way to see which — and
	// every symbol count on a screen showing one is a lower bound the reader
	// cannot locate.
	PartialParse bool
}

type symInfo struct {
	Name       string
	FQN        string
	Kind       model.EntityKind
	FileID     int64
	Exported   bool
	Confidence model.Confidence
	Container  string
	Degree     int

	// Line is where the symbol is declared. Carried here because a node a reader
	// cannot open is a node they have to go and find by hand: the graph explorer
	// prints it on every symbol card, and the database screen uses it to say
	// which declaration of a table name it means when several share one.
	Line int
}

// compactEdge is one relationship, reduced to what a portal renders and proves.
type compactEdge struct {
	Src, Dst   int64
	Kind       model.EdgeKind
	Confidence model.Confidence
	File       string
	Line       int32
}

type langKind struct {
	Lang string
	Kind model.EdgeKind
}

type containerPair struct {
	From, To string
}

// containerInfo holds one grouping's membership.
//
// # Two file counts, because there are two facts
//
// SymbolFiles is the files that contributed at least one symbol. AllFiles is
// every file the grouping matched, symbol-bearing or not — READMEs, YAML,
// generated JSON, anything the parser had nothing to extract from.
//
// Both are kept because both were already being reported, under the same word,
// by two different screens: the graph explorer showed `task-app 551 files` and
// governance showed `3,425` for the same component in the same session, and
// there was nothing on either screen to reconcile them. 551 was symbol-bearing
// files; 3,425 was every matched file. Neither was wrong and neither was
// labelled, which made both untrustworthy.
//
// The fix is not to pick one. It is that there is now exactly ONE place either
// number is computed, and every screen reports both under names that say which
// is which — see ContainerNode.FilesWithSymbols / FilesMatched.
type containerInfo struct {
	Name    string
	Symbols []int64

	// SymbolFiles are the files that produced a symbol. The denominator behind
	// every graph the explorer can draw: a file with no symbol has no node.
	SymbolFiles map[int64]struct{}

	// AllFiles is every file this grouping matched. The denominator an
	// architecture rule is written against — a component's include patterns
	// match paths, not symbols.
	AllFiles int

	Languages map[string]int
	Exported  int
}

// aggregatedEdge is every relationship between two containers, collapsed.
//
// Aggregation is what keeps the explorer from being a hairball, and the collapse
// is only honest if it preserves the evidential spread: Weakest is the lowest
// tier among the collapsed edges (a bundle is no stronger than its worst
// member), Conf carries the full distribution so the reader can see whether a
// thick line is one proven edge or four hundred guesses, and Example keeps one
// real file:line so the bundle can still be checked.
//
// # Why Kinds is a cross-tabulation and not a count
//
// It used to be map[EdgeKind]int — a histogram of kinds, unconditioned on
// confidence, sitting beside a Count that WAS conditioned on the floor. The two
// were rendered in adjacent columns of the same row, and produced
// `(no declared component) → gateway · edges 50 · CALLS×572`. Both numbers were
// correct about different populations and nothing in the row said so.
//
// Storing the full kind×tier cross-tab costs one Histogram per (pair, kind) —
// bounded by the container graph, which is the small graph by construction —
// and buys exact arithmetic: under any floor and any kind filter, the count and
// the per-kind breakdown are sums over the same cells. It also retires the
// "subtract from the weakest tiers first" approximation filterKinds used to
// carry, which biased a filtered bundle towards looking weaker than it was.
type aggregatedEdge struct {
	From, To string
	Count    int
	Weakest  model.Confidence
	Conf     Histogram
	Kinds    map[model.EdgeKind]*Histogram
	Example  compactEdge
}

// maxContainers is the CEILING a caller may raise the container budget to.
//
// Well above any declared architecture (arch.Model itself caps components at
// 512), so a declared model is never cut by it.
const maxContainers = 512

// defaultContainers is how many are drawn when nobody asks for more.
//
// A readability bound, not a cost one. At 400 nodes on a 1038x610 canvas the
// graph is a hairball: about 2.6 square pixels per node, edges crossing
// everything, and no label legible — measured against a real 537-directory
// repository, where it drew 400 containers and 119,619 painted pixels of
// unreadable overlap.
//
// 48 is roughly what a layered graph fits at a legible node size, and the
// remainder is stated in the omission block like every other bound in this
// portal. A reader who wants the full set raises ?nodes=; a reader who wants a
// meaningful picture declares components, which is what the note says.
const defaultContainers = 48

// structural returns the cached structural index for runID, building it once.
//
// The lock is held across the build, deliberately, for the same reason
// capability.symbolCache holds its own: without it, N concurrent page loads on a
// cold cache each perform three whole-run table scans. The build is bounded work
// over one run, so serialising it trades a little concurrency for a bounded
// memory ceiling.
func (s *Server) structural(ctx context.Context, runID int64) (*structuralIndex, error) {
	s.mu.RLock()
	if s.cached != nil && s.cachedID == runID {
		idx := s.cached
		s.mu.RUnlock()
		return idx, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.cachedID == runID {
		return s.cached, nil
	}
	idx, err := s.buildStructural(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.cachedID, s.cached = runID, idx
	return idx, nil
}

func (s *Server) buildStructural(ctx context.Context, runID int64) (*structuralIndex, error) {
	reg := s.deps.Registry

	// THE THREE READS RUN TOGETHER, NOT ONE AFTER ANOTHER.
	//
	// Measured on a 14,614-file repository: files 128ms, symbols 1,272ms, edges
	// 301ms — 1.70s of the 2.05s a cold first request cost, spent waiting on
	// three queries that do not depend on each other. Run concurrently the wall
	// clock is the slowest of the three rather than their sum.
	//
	// A fixed fan-out of three, not a pool: the work is three named queries, and
	// CLAUDE.md §2's rule is that goroutines are bounded, which three literally
	// is. The pool behind the store admits up to eight connections, and on the
	// single-connection in-memory configuration the three simply queue — each
	// drains and releases before the next needs it, so nothing here can deadlock
	// on connection supply.
	//
	// The first error wins and is returned wrapped, naming the read that failed;
	// the others are still waited for, so no goroutine outlives this call.
	var (
		files   []model.File
		symbols []model.Symbol
		edges   []model.Edge
		errs    [3]error
		wg      sync.WaitGroup
	)
	reads := [3]struct {
		what string
		run  func() error
	}{
		{"files", func() (err error) { files, err = reg.Files(ctx, runID); return }},
		{"symbols", func() (err error) { symbols, err = reg.Symbols(ctx, runID); return }},
		{"edges", func() (err error) { edges, err = reg.Edges(ctx, runID); return }},
	}
	wg.Add(len(reads))
	for i := range reads {
		go func(i int) {
			defer wg.Done()
			if err := reads[i].run(); err != nil {
				errs[i] = fmt.Errorf("web: read %s for run %d: %w", reads[i].what, runID, err)
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	idx := &structuralIndex{
		runID:          runID,
		files:          make(map[int64]fileInfo, len(files)),
		syms:           make(map[int64]symInfo, len(symbols)),
		edges:          make([]compactEdge, 0, len(edges)),
		fileLangs:      map[string]int{},
		symLangs:       map[string]int{},
		byKind:         map[model.EdgeKind]*Histogram{},
		byLangKind:     map[langKind]*Histogram{},
		containers:     map[string]*containerInfo{},
		containerEdges: map[containerPair]*aggregatedEdge{},
	}

	archModel := s.deps.Config.Architecture
	declared := len(archModel.Components) > 0
	idx.containerKind = "directory"
	if declared {
		idx.containerKind = "component"
	}
	// arch.NewResolver memoises glob matching per path. ComponentOf walks every
	// component's every pattern, so calling it per SYMBOL on a 100k-symbol
	// repository repeats the same match for every symbol in a file — the same
	// reason capability/architecture resolves per file.
	resolver := arch.NewResolver(archModel)

	containerOfFile := make(map[int64]string, len(files))
	for _, f := range files {
		idx.files[f.ID] = fileInfo{
			Path: f.Path, Language: f.Language,
			PartialParse: f.ParseStatus == "partial",
		}
		if f.Language != "" {
			idx.fileLangs[f.Language]++
		}
		container := containerFor(declared, resolver, f.Path)
		containerOfFile[f.ID] = container
		// Counted here, over FILES, so AllFiles is the same population an
		// architecture include pattern matches. Counting it during the symbol
		// loop instead is what produced two different "files" figures.
		idx.container(container).AllFiles++
	}

	for _, sym := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		container := containerOfFile[sym.FileID]
		if container == "" {
			container = unassignedContainer
		}
		if container == unassignedContainer {
			idx.unassigned++
		}
		lang := idx.files[sym.FileID].Language
		idx.syms[sym.ID] = symInfo{
			Name: sym.Name, FQN: sym.FQN, Kind: sym.Kind, FileID: sym.FileID,
			Exported: sym.Exported, Confidence: sym.Confidence, Container: container,
			Line: sym.Range.Start.Line,
		}
		if lang != "" {
			idx.symLangs[lang]++
		}
		c := idx.container(container)
		c.Symbols = append(c.Symbols, sym.ID)
		c.SymbolFiles[sym.FileID] = struct{}{}
		if lang != "" {
			c.Languages[lang]++
		}
		if sym.Exported {
			c.Exported++
		}
	}

	pool := newInterner()
	for i := range edges {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		e := edges[i]
		ce := compactEdge{
			Src:        e.SrcID,
			Dst:        e.DstID,
			Kind:       model.EdgeKind(pool.get(string(e.Kind))),
			Confidence: model.Confidence(pool.get(string(e.Confidence))),
			File:       pool.get(e.Evidence.FilePath),
			Line:       line32(e.Evidence.Range.Start.Line),
		}
		idx.edges = append(idx.edges, ce)
		idx.edgeConf.add(ce.Confidence)
		histogramFor(idx.byKind, ce.Kind).add(ce.Confidence)

		src, srcOK := idx.syms[ce.Src]
		if srcOK {
			lang := idx.files[src.FileID].Language
			if lang == "" {
				lang = "unknown"
			}
			histogramFor(idx.byLangKind, langKind{Lang: lang, Kind: ce.Kind}).add(ce.Confidence)
			src.Degree++
			idx.syms[ce.Src] = src
		}
		dst, dstOK := idx.syms[ce.Dst]
		if dstOK {
			dst.Degree++
			idx.syms[ce.Dst] = dst
		}
		if srcOK && dstOK && src.Container != dst.Container {
			idx.aggregate(src.Container, dst.Container, ce)
		}
	}

	idx.containerOrder = make([]string, 0, len(idx.containers))
	for name := range idx.containers {
		idx.containerOrder = append(idx.containerOrder, name)
	}
	sort.Strings(idx.containerOrder)
	for _, c := range idx.containers {
		sort.Slice(c.Symbols, func(i, j int) bool { return c.Symbols[i] < c.Symbols[j] })
	}
	return idx, nil
}

// histogramFor is the get-or-create both cross-tabulations need. A free
// function rather than a method because Go methods cannot take type parameters,
// and the two call sites key on different types.
func histogramFor[K comparable](m map[K]*Histogram, key K) *Histogram {
	h, ok := m[key]
	if !ok {
		h = &Histogram{}
		m[key] = h
	}
	return h
}

func (idx *structuralIndex) container(name string) *containerInfo {
	c, ok := idx.containers[name]
	if !ok {
		c = &containerInfo{Name: name, SymbolFiles: map[int64]struct{}{}, Languages: map[string]int{}}
		idx.containers[name] = c
	}
	return c
}

func (idx *structuralIndex) aggregate(from, to string, e compactEdge) {
	key := containerPair{From: from, To: to}
	agg, ok := idx.containerEdges[key]
	if !ok {
		agg = &aggregatedEdge{
			From: from, To: to, Weakest: e.Confidence,
			Kinds: map[model.EdgeKind]*Histogram{}, Example: e,
		}
		idx.containerEdges[key] = agg
	}
	agg.Count++
	agg.Conf.add(e.Confidence)
	histogramFor(agg.Kinds, e.Kind).add(e.Confidence)
	if e.Confidence.Rank() < agg.Weakest.Rank() {
		agg.Weakest = e.Confidence
	}
	// Keep the strongest edge as the shown example: if a bundle can be checked
	// at all, point the reader at its best evidence — while Weakest still
	// reports the bundle's true floor, so the line is not drawn stronger than it
	// is.
	if e.Confidence.Rank() > agg.Example.Confidence.Rank() {
		agg.Example = e
	}
}

// unassignedContainer is the bucket for symbols no declared component matches.
//
// Named rather than empty, because it must be visible in the explorer: a
// forbidden dependency routed through an unassigned file is invisible to every
// architecture rule, and the only way a reader notices is by seeing how much
// code sits outside the model.
const unassignedContainer = "(no declared component)"

// repositoryRootLabel names the directory holding files with no directory.
//
// Shared with the metrics projection's directory level rather than spelled
// twice: path.Dir returns "." for a root file, and a screen that calls the same
// directory "." in one table and "(repository root)" in another is asking the
// reader to work out that they are the same place.
const repositoryRootLabel = "(repository root)"

// containerFor picks the grouping a file belongs to.
//
// Declared components where the repository declares them, the file's directory
// otherwise. Never an inferred component: a grouping RepoCortex guessed and then
// rendered as architecture would present a description of the code's current
// shape as an intention, which is the same overclaim capability/architecture
// refuses to make.
func containerFor(declared bool, resolver *arch.Resolver, filePath string) string {
	if declared {
		if c := resolver.ComponentOf(filePath); c != "" {
			return c
		}
		return unassignedContainer
	}
	dir := path.Dir(filePath)
	if dir == "." || dir == "/" || dir == "" {
		return repositoryRootLabel
	}
	return dir
}

// interner collapses the repeated strings a whole-run edge scan produces.
//
// Every row's kind, confidence and evidence path arrives as a fresh allocation
// from the driver. On a 500k-edge run that is 1.5M strings holding perhaps
// twenty distinct kinds, four confidences and a few thousand paths.
type interner struct{ m map[string]string }

func newInterner() *interner { return &interner{m: make(map[string]string, 4096)} }

func (i *interner) get(s string) string {
	if s == "" {
		return ""
	}
	if got, ok := i.m[s]; ok {
		return got
	}
	i.m[s] = s
	return s
}

// line32 narrows a 1-based line number for the compact in-memory edge table;
// every reader widens it back to int. A file with more than 2^31 lines does
// not exist, but a narrowing conversion the linter cannot see the bound of is
// a narrowing conversion the next reader cannot either.
func line32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 0 {
		return 0
	}
	return int32(n)
}

// ---------------------------------------------------------------- derived views

// LanguageMix is one language's share of the repository.
type LanguageMix struct {
	Language string  `json:"language"`
	Files    int     `json:"files"`
	Symbols  int     `json:"symbols"`
	FilePct  float64 `json:"file_pct"`
}

func (idx *structuralIndex) languageMix() []LanguageMix {
	total := 0
	for _, n := range idx.fileLangs {
		total += n
	}
	out := make([]LanguageMix, 0, len(idx.fileLangs))
	for lang, n := range idx.fileLangs {
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(n) / float64(total)
		}
		out = append(out, LanguageMix{
			Language: lang, Files: n, Symbols: idx.symLangs[lang], FilePct: round1(pct),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Language < out[j].Language
	})
	return out
}

// impactKinds are the relationship kinds that carry "a change here can affect
// there". CONTAINS and IMPORTS are structural bookkeeping — every file contains
// its symbols — so a blind spot in them says nothing about analysis quality.
var impactKinds = []model.EdgeKind{
	model.EdgeCalls, model.EdgeReferences, model.EdgeImplements, model.EdgeExtends,
	model.EdgeDependsOn, model.EdgeReads, model.EdgeWrites, model.EdgeExposes,
	model.EdgeTestedBy,
}

// BlindSpot is one (language, edge kind) pair the index cannot speak about.
//
// This is the most useful thing the overview can say, and no LLM-asserted graph
// can say it: a graph that asserts an edge for everything has no notion of a
// region it cannot see. A blind spot here is a measured absence — this
// repository has 412 Python symbols and zero resolved WRITES edges from them, so
// every data-access answer about Python is empty for a reason that is not "there
// is no data access".
type BlindSpot struct {
	Language string `json:"language"`
	Kind     string `json:"kind"`
	Symbols  int    `json:"symbols"`

	Edges    int    `json:"edges"`
	Severity string `json:"severity"` // blind | weak
	Detail   string `json:"detail"`
}

// blindSpots cross-tabulates confidence by (language, impact-carrying kind).
//
// Two severities, and the distinction is the point. `blind` means the index
// produced no edge of that kind from that language at all — every question of
// that shape returns empty, and an empty answer reads as "no such relationship".
// `weak` means edges exist but none clears `high`, so the answers exist and are
// mostly wrong (docs/accuracy.md puts medium at 5.7–37.5%).
func (idx *structuralIndex) blindSpots() []BlindSpot {
	var out []BlindSpot
	langs := make([]string, 0, len(idx.symLangs))
	for l := range idx.symLangs {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	for _, lang := range langs {
		symbols := idx.symLangs[lang]
		if symbols == 0 {
			continue
		}
		for _, kind := range impactKinds {
			h := idx.byLangKind[langKind{Lang: lang, Kind: kind}]
			switch {
			case h == nil || h.Total == 0:
				out = append(out, BlindSpot{
					Language: lang, Kind: string(kind), Symbols: symbols,
					Severity: "blind",
					Detail: fmt.Sprintf(
						"%d %s symbol(s) are indexed and not one %s edge was resolved from them. "+
							"Every question of this shape returns an empty answer, and an empty answer "+
							"is not evidence that no such relationship exists.",
						symbols, lang, kind),
				})
			case h.AtOrAbove(model.ConfidenceHigh) == 0:
				out = append(out, BlindSpot{
					Language: lang, Kind: string(kind), Symbols: symbols,
					Edges: h.Total, Severity: "weak",
					Detail: fmt.Sprintf(
						"%d %s edge(s) from %s, none of which reached `high`. Measured precision below "+
							"`high` is 0.0–37.5%%, so these are leads rather than findings.",
						h.Total, kind, lang),
				})
			}
		}
	}
	return out
}

// KindCoverage is the confidence spread for one edge kind.
type KindCoverage struct {
	Kind       string    `json:"kind"`
	Histogram  Histogram `json:"histogram"`
	Actionable int       `json:"actionable"`
}

func (idx *structuralIndex) kindCoverage() []KindCoverage {
	out := make([]KindCoverage, 0, len(idx.byKind))
	for kind, h := range idx.byKind {
		out = append(out, KindCoverage{
			Kind: string(kind), Histogram: *h, Actionable: h.AtOrAbove(model.ConfidenceHigh),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Histogram.Total != out[j].Histogram.Total {
			return out[i].Histogram.Total > out[j].Histogram.Total
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// symbolLanguage returns the language of a symbol's file, or "unknown".
func (idx *structuralIndex) symbolLanguage(id int64) string {
	s, ok := idx.syms[id]
	if !ok {
		return "unknown"
	}
	if lang := idx.files[s.FileID].Language; lang != "" {
		return lang
	}
	return "unknown"
}

func (idx *structuralIndex) symbolPath(id int64) string {
	s, ok := idx.syms[id]
	if !ok {
		return ""
	}
	return idx.files[s.FileID].Path
}

// matchesLanguage treats an empty filter as "no filter" rather than "match
// nothing", so a URL carrying a dangling parameter shows everything instead of a
// blank canvas.
func (idx *structuralIndex) matchesLanguage(id int64, want string) bool {
	return want == "" || strings.EqualFold(idx.symbolLanguage(id), want)
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
