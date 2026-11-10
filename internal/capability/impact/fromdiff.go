package impact

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// DefaultMaxSeeds bounds how many changed symbols are analysed.
//
// A seed costs a full traversal, so an unbounded seed set turns a 900-file
// refactor into 900 walks of a hub-shaped graph. The cap is small enough that
// the worst realistic diff still answers interactively, and hitting it sets
// DiffReport.Truncated rather than quietly analysing a prefix — which is the
// same bargain internal/graph strikes with MaxNodes.
const DefaultMaxSeeds = 200

// ChangeSource is the slice of a git repository this package needs.
//
// Declared here, at the consumer, rather than taking a *git.Repo: a blast
// radius is computable from any answer to "which paths differ between these two
// revisions", and a test should be able to supply that answer without building
// a repository whose rename heuristics it would then be asserting on.
// *git.Repo satisfies it structurally.
type ChangeSource interface {
	ChangedSince(ctx context.Context, from, to string) ([]git.Change, error)
}

// RunSource answers "which commit does this index describe?".
//
// Declared here rather than added to capability.Registry on purpose: the run
// row is provenance about the index, not part of the read surface every
// capability queries, and widening the shared port would make every fake in
// every capability package implement a method only this file calls.
// *registry.Registry satisfies it structurally.
type RunSource interface {
	Run(ctx context.Context, runID int64) (*model.IndexRun, error)
}

// RevisionResolver turns a revision NAME into a commit SHA.
//
// It is separate from ChangeSource because it answers a different question and
// because a test that wants to exercise the staleness rule should not have to
// supply a diff to do it. *git.Repo satisfies both.
type RevisionResolver interface {
	ResolveRevision(ctx context.Context, rev string) (string, error)
}

// Rooter reports the absolute root a set of paths is relative to.
//
// It exists for one bug: git.Open searches parent directories for .git, so
// ChangedSince returns REPO-root-relative paths, while model.File.Path — and
// therefore every path in the symbol index — is WALKER-root-relative. Index a
// subdirectory and the two path spaces silently stop overlapping. *git.Repo
// satisfies this with its existing Root method.
type Rooter interface {
	Root() string
}

// Compile-time proof the production types satisfy the ports without adapters.
var (
	_ ChangeSource     = (*git.Repo)(nil)
	_ RevisionResolver = (*git.Repo)(nil)
	_ Rooter           = (*git.Repo)(nil)
	_ RunSource        = (*registry.Registry)(nil)
)

// DiffRequest asks what a change puts at risk.
type DiffRequest struct {
	// Changes supplies the diff. Required.
	Changes ChangeSource

	// BaseRev and HeadRev are anything `git rev-parse` accepts. They are passed
	// through untouched, so ChangedSince's own conventions for the empty string
	// (empty tree on the left, HEAD on the right) still apply.
	BaseRev string
	HeadRev string

	// Runs supplies the run row so the report can state WHICH revision the
	// index describes. Optional only in the sense that a nil one does not fail
	// the call: it is reported as an Unknown, because an unverifiable index is
	// not a verified one. See checkIndexedRevision for the matching rule.
	Runs RunSource

	// Revisions resolves revision names to SHAs for that comparison. When nil,
	// Changes is used if it happens to resolve revisions too — the production
	// wiring passes one *git.Repo for both, and requiring the caller to pass it
	// twice would be ceremony whose only effect is a missed check.
	Revisions RevisionResolver

	// IndexRoot is the absolute directory the INDEX was walked from, needed
	// only when it is not the git root. Paths from ChangedSince are rebased
	// onto it; see rebasePrefix for why an empty value is a silent wrong answer
	// waiting to happen rather than a safe default.
	IndexRoot string

	// MaxSeeds bounds the changed symbols analysed. <= 0 uses DefaultMaxSeeds.
	MaxSeeds int

	// Options bounds each seed's traversal, exactly as it bounds Analyze's.
	Options
}

// Seed is one changed symbol and the blast radius computed from it.
type Seed struct {
	capability.Symbol

	// ChangeKind is what git said happened to the file this symbol is defined
	// in. A deletion and a modification are different risks and the reader must
	// be able to tell them apart.
	ChangeKind git.ChangeKind `json:"change_kind"`

	// IndexedPath is the path the symbol has in the INDEXED run, which for a
	// rename is the pre-rename name. It is stated because it can differ from
	// the path the diff reported, and a reader comparing the two would
	// otherwise think the tool had lost the file.
	IndexedPath string `json:"indexed_path"`

	// Report is this seed's analysis, carried through unaltered. Everything the
	// aggregate summarises is recoverable here — including each walk's own
	// truncation and unknowns.
	Report Report `json:"report"`
}

// DiffReport is what a change puts at risk.
type DiffReport struct {
	BaseRev string `json:"base_rev"`
	HeadRev string `json:"head_rev"`

	// IndexedRevision is the commit the queried index run was built from, and
	// RevisionMatchesRange says whether it is one of the two endpoints of the
	// analysed range.
	//
	// The field is named for the comparison it actually performs, following
	// mcp.Provenance.HeadMatchesBuild: it is not called "fresh", because a
	// matching commit says nothing about staged, unstaged or untracked changes.
	// A false value always has an Unknown beside it explaining which of the
	// several ways to fail this was.
	IndexedRevision      string `json:"indexed_revision,omitempty"`
	RevisionMatchesRange bool   `json:"revision_matches_range"`

	// ChangedFiles is every path the diff reported, post-change name, in the
	// REPOSITORY's path space — exactly the strings git produced, never rebased.
	//
	// One space, stated, because two is a wrong answer that looks like a right
	// one. When the index root is a subdirectory, "sub/svc/x.go" and "svc/x.go"
	// are different files that rebase to the same index-space string, and a list
	// holding both would name one file twice and the other never. Everything
	// file-shaped on this report — ChangedFiles, UnindexedFiles — is therefore
	// repo-space; everything symbol-shaped — Seed.IndexedPath, Impacted.FilePath
	// — is index-space, because that is what the index recorded. IndexRootPrefix
	// is the mapping between the two.
	ChangedFiles []string `json:"changed_files"`

	// UnindexedFiles are changed paths the committed run holds no symbols for:
	// non-code files, files added since the index was built, files whose parse
	// failed, files outside the indexed subtree entirely. They are named rather
	// than counted because each one is a hole in the answer that a reader may
	// recognise as important. Repo-space, like ChangedFiles.
	UnindexedFiles []string `json:"unindexed_files,omitempty"`

	// IndexRootPrefix is the leading segment stripped from each repo-space path
	// to reach the index's path space, or "" when the two spaces are the same.
	//
	// It is published rather than kept internal because it is the only way a
	// reader can line ChangedFiles up against Seeds[i].IndexedPath: without it,
	// a report whose diff says "sub/svc/x.go" and whose seed says "svc/x.go"
	// reads as a tool that lost the file.
	IndexRootPrefix string `json:"index_root_prefix,omitempty"`

	// SeedsFound is how many changed symbols were located, before MaxSeeds.
	// len(Seeds) is how many were analysed; the two differ exactly when the cap
	// bit, and that difference is also stated in Truncated.
	SeedsFound int    `json:"seeds_found"`
	Seeds      []Seed `json:"seeds"`

	// Impacted is the union of every seed's Direct and Indirect sets, each
	// symbol once. See mergeImpacted for which of two claims about the same
	// symbol survives.
	Impacted []Impacted `json:"impacted"`

	// Dispatch is the union of every seed's dispatch expansion, kept separate
	// from Impacted for the reason Report.Dispatch documents: a symbol the
	// runtime MAY execute is not a symbol that references the change.
	Dispatch []Dispatched `json:"dispatch,omitempty"`

	// Unknown is what could not be determined: the provenance doubts about the
	// index itself, then the gaps gathered from the seed reports. Identical
	// gaps reported by several seeds are listed once. Nothing is dropped, and
	// the single place anything is summed — the below-confidence-floor row —
	// says in its own text that it is a sum and what that costs. See
	// unknownSet.
	Unknown []Unknown `json:"unknown"`

	// Risk is the single highest-scoring seed's risk, copied unchanged, and
	// RiskiestSeed names the seed it came from.
	//
	// It is a MAXIMUM, not a total. There is no defensible arithmetic for
	// combining two risk scores — 40 + 40 is not 80 — so no combined score is
	// invented; the reader is pointed at the worst thing in the diff and can
	// read its factors in Seeds.
	Risk         Risk   `json:"risk"`
	RiskiestSeed string `json:"riskiest_seed,omitempty"`

	Completeness capability.Completeness `json:"completeness"`

	// Truncated is set if ANY seed's walk was bounded, or if the seed set
	// itself was capped. Copied from the traversals, never re-derived from the
	// row counts.
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`

	// ConfidenceFloor is the floor every seed walk ran with.
	ConfidenceFloor string `json:"confidence_floor,omitempty"`
}

// TotalImpacted is the number of distinct symbols the diff puts at risk through
// recorded references. A lower bound, like every count in this package.
func (r DiffReport) TotalImpacted() int { return len(r.Impacted) }

// TotalReached counts the dispatch expansion too.
func (r DiffReport) TotalReached() int { return len(r.Impacted) + len(r.Dispatch) }

// AnalyzeDiff computes what a change puts at risk.
//
// # The direction is the whole design
//
// A change to a symbol threatens the code that DEPENDS ON it, not the code it
// calls. This function does not choose that direction itself: it seeds
// analyzeTarget, which walks graph.Incoming, and reuses it for every seed
// precisely so there is exactly one place in this repository where the
// direction of a blast radius is decided. Running it outbound would produce a
// plausible-looking list of everything the changed code touches — a different
// question, and a dangerous answer to confuse with this one.
//
// # Why an empty diff is handled before anything else
//
// An empty root set is the classic way to return the whole graph by accident.
// internal/graph is careful about it today, but this function must not depend
// on that remaining true three layers down: a diff that changed nothing, or
// changed only files the index has no symbols for, returns without asking the
// graph anything at all, and says which files it could not seed from.
//
// # What is not aggregated
//
// Truncation and unknowns come out of the seed reports as they are. This
// function never decides a result "looks complete"; only the traversal knows
// whether it stopped early, and its answer is copied rather than inferred.
func AnalyzeDiff(ctx context.Context, deps capability.Deps, runID int64, req DiffRequest) (DiffReport, error) {
	if err := deps.Validate(); err != nil {
		return DiffReport{}, err
	}
	if req.Changes == nil {
		return DiffReport{}, errors.New("impact: DiffRequest.Changes is nil; no diff to analyse")
	}
	if err := ctx.Err(); err != nil {
		return DiffReport{}, err
	}
	opts := req.withDefaults()
	maxSeeds := req.MaxSeeds
	if maxSeeds <= 0 {
		maxSeeds = DefaultMaxSeeds
	}

	changes, err := req.Changes.ChangedSince(ctx, req.BaseRev, req.HeadRev)
	if err != nil {
		return DiffReport{}, fmt.Errorf("impact: changed files %s..%s: %w", req.BaseRev, req.HeadRev, err)
	}
	prefix, prefixUnknowns, err := rebasePrefix(req)
	if err != nil {
		return DiffReport{}, err
	}
	inside, outside := rebaseChanges(changes, prefix)

	rep := DiffReport{
		BaseRev:         req.BaseRev,
		HeadRev:         req.HeadRev,
		ChangedFiles:    changedPaths(changes),
		IndexRootPrefix: prefix,
		ConfidenceFloor: string(opts.MinConfidence),
	}
	sort.Strings(rep.ChangedFiles)

	// Asked before any counting, and recorded on the report whatever the
	// answer, because this is the question that decides whether the rest of the
	// report is about the reader's change at all.
	indexedRev, revUnknowns, err := checkIndexedRevision(ctx, req, runID)
	if err != nil {
		return DiffReport{}, err
	}
	rep.IndexedRevision = indexedRev
	rep.RevisionMatchesRange = len(revUnknowns) == 0

	// provenance is the set of doubts about the index and the wiring, as
	// opposed to the gaps a traversal found. It is carried separately because
	// it must reach Completeness.Caveats on every return path — including the
	// two that never traverse, which are precisely the paths a stale index most
	// often produces. See finishDiff.
	provenance := make([]Unknown, 0, len(revUnknowns)+len(prefixUnknowns))
	provenance = append(provenance, revUnknowns...)
	provenance = append(provenance, prefixUnknowns...)
	rep.Unknown = append(rep.Unknown, provenance...)

	// Loaded before the empty-diff guard, not after, so IndexedSymbols is the
	// real denominator on every path. It costs one query on a no-op diff; the
	// alternative was a report whose completeness block said the index held
	// zero symbols, which is the same shape of false reassurance this package
	// exists to refuse.
	idx, err := deps.SymbolIndex(ctx, runID)
	if err != nil {
		return DiffReport{}, err
	}

	if len(changes) == 0 {
		// Nothing changed. Returning here rather than falling through with an
		// empty root set is the guard this function's doc comment describes.
		if err := finishDiff(ctx, deps, runID, &rep, idx.Len(), provenance); err != nil {
			return DiffReport{}, err
		}
		return rep, nil
	}

	seeds, unindexed := seedsFromChanges(idx, inside)
	rep.UnindexedFiles = append(unrebase(unindexed, prefix), outside...)
	sort.Strings(rep.UnindexedFiles)
	rep.SeedsFound = len(seeds)
	if len(seeds) == 0 {
		// Files changed, but the committed run defines no symbol in any of
		// them. Same guard, different cause. UnindexedFiles names them, and the
		// Unknown says what a report of "nothing at risk" over a non-empty diff
		// could mean — including the one cause that is a bug in the caller's
		// wiring rather than a property of the diff.
		rep.Unknown = append(rep.Unknown, unindexedChangesUnknown(len(rep.UnindexedFiles), prefix, idx.Len()))
		if err := finishDiff(ctx, deps, runID, &rep, idx.Len(), provenance); err != nil {
			return DiffReport{}, err
		}
		return rep, nil
	}
	// trunc accumulates every bound that fired, across every seed, as a SET of
	// distinct reasons with how many walks hit each. See its type comment for
	// why this cannot be a string concatenation.
	var trunc truncationSet
	trunc.add(rep.Truncated, rep.TruncatedReason)
	if len(seeds) > maxSeeds {
		seeds = seeds[:maxSeeds]
		trunc.add(true, fmt.Sprintf(
			"seed limit %d reached, and %d of %d changed symbol(s) were not analysed",
			maxSeeds, rep.SeedsFound-maxSeeds, rep.SeedsFound))
	}

	// isSeed lets the aggregation drop a changed symbol reached from another
	// changed symbol. It is an input to this analysis, not collateral from it,
	// and listing it as impacted reads as a finding when it is a premise. It is
	// not lost: it has its own entry in Seeds.
	isSeed := make(map[int64]struct{}, len(seeds))
	for _, s := range seeds {
		isSeed[s.ID] = struct{}{}
	}

	byID := make(map[int64]Impacted, len(seeds)*8)
	dispatchByID := make(map[int64]Dispatched)
	unknowns := newUnknownSet(len(seeds), opts.MinConfidence)

	for i := range seeds {
		// Checked per seed rather than only at the top: a diff at the seed cap
		// is many traversals, and a cancelled request must stop between them
		// even if each individual walk is fast.
		if err := ctx.Err(); err != nil {
			return DiffReport{}, err
		}
		tgt := targetForSeed(seeds[i].Symbol)
		seedRep, err := analyzeTarget(ctx, deps, runID, tgt, opts)
		if err != nil {
			return DiffReport{}, fmt.Errorf("impact: blast radius of changed symbol %s: %w", tgt.FQN, err)
		}
		seeds[i].Report = seedRep

		for _, list := range [][]Impacted{seedRep.Direct, seedRep.Indirect} {
			for _, imp := range list {
				if _, ok := isSeed[imp.ID]; ok {
					continue
				}
				if prev, ok := byID[imp.ID]; ok {
					byID[imp.ID] = mergeImpacted(prev, imp)
					continue
				}
				byID[imp.ID] = imp
			}
		}
		for _, d := range seedRep.Dispatch {
			if _, ok := isSeed[d.ID]; ok {
				continue
			}
			if prev, ok := dispatchByID[d.ID]; ok {
				prev.Impacted = mergeImpacted(prev.Impacted, d.Impacted)
				dispatchByID[d.ID] = prev
				continue
			}
			dispatchByID[d.ID] = d
		}
		unknowns.addAll(seedRep.Unknown)
		trunc.add(seedRep.Truncated, seedRep.TruncatedReason)
		if seedRep.Risk.Score > rep.Risk.Score || rep.RiskiestSeed == "" {
			rep.Risk, rep.RiskiestSeed = seedRep.Risk, tgt.FQN
		}
	}

	rep.Truncated, rep.TruncatedReason = trunc.result()
	rep.Seeds = seeds
	rep.Unknown = append(rep.Unknown, unknowns.list()...)
	rep.Impacted = make([]Impacted, 0, len(byID))
	for _, imp := range byID {
		rep.Impacted = append(rep.Impacted, imp)
	}
	rankImpacted(rep.Impacted)
	rep.Dispatch = make([]Dispatched, 0, len(dispatchByID))
	for _, d := range dispatchByID {
		rep.Dispatch = append(rep.Dispatch, d)
	}
	sort.Slice(rep.Dispatch, func(i, j int) bool {
		if rep.Dispatch[i].Depth != rep.Dispatch[j].Depth {
			return rep.Dispatch[i].Depth < rep.Dispatch[j].Depth
		}
		return rep.Dispatch[i].ID < rep.Dispatch[j].ID
	})

	if err := finishDiff(ctx, deps, runID, &rep, idx.Len(), provenance); err != nil {
		return DiffReport{}, err
	}
	return rep, nil
}

// finishDiff fills the completeness block, and is called on EVERY return path
// that produces a report.
//
// It is a function rather than four inlined copies because the three cheap
// paths were the ones that skipped it, and they are the paths where the doubts
// matter MOST. "Nothing at risk" over an empty diff, or over a diff whose every
// file is unindexed, is exactly what a stale index or a mis-wired index root
// produces — so a report that stays silent there is silent in the conditions it
// was built to speak up in, and reassuring in the conditions it was built to
// doubt. capability.Completeness.LowerBound is documented as always true; it is
// true here too, because a bounded index is a floor whether or not a traversal
// ran.
func finishDiff(ctx context.Context, deps capability.Deps, runID int64,
	rep *DiffReport, indexedSymbols int, provenance []Unknown) error {
	stats, err := deps.ResolutionStats(ctx, runID)
	if err != nil {
		return err
	}
	rep.Completeness = capability.NewCompleteness(stats, indexedSymbols, rep.Truncated, rep.TruncatedReason)
	// Appended, not substituted: a provenance doubt belongs beside the
	// resolution gap in the one block a reader is expected to read, not only in
	// a list they may filter by kind.
	for _, u := range provenance {
		rep.Completeness.Caveats = append(rep.Completeness.Caveats, u.Detail)
	}
	for _, u := range rep.Unknown {
		if u.Kind == UnknownUnindexedChanges {
			rep.Completeness.Caveats = append(rep.Completeness.Caveats, u.Detail)
		}
	}
	if len(rep.Dispatch) > 0 {
		rep.Completeness.Caveats = append(rep.Completeness.Caveats, capability.DispatchExpansionCaveat)
	}
	return nil
}

// mergeImpacted decides which of two claims about the same symbol survives.
//
// The shallower one wins, matching internal/graph's own rule that a node is
// recorded at its shallowest depth: "two hops from something you changed" is
// the more urgent and the more reliable of the two statements. Only when the
// depths tie does confidence break it, and then the STRONGER path wins — the
// symbol really is reachable that way, and reporting the weaker of two true
// paths would understate a risk rather than overstate it. Neither branch ever
// invents a confidence: both values came out of a traversal's path minimum.
func mergeImpacted(a, b Impacted) Impacted {
	switch {
	// Cost first, for the same reason rankImpacted sorts by it first: it is
	// the key the traversal ranked by, so keeping the row with the other cost
	// would place the symbol somewhere neither seed's own report put it.
	// Cheaper is the better-evidenced route. Dispatch rows leave cost unset and
	// tie here, falling through to the depth and confidence keys this has
	// always used.
	case b.cost != a.cost:
		if b.cost < a.cost {
			return b
		}
		return a
	case b.Depth < a.Depth:
		return b
	case b.Depth > a.Depth:
		return a
	case b.Confidence.Rank() > a.Confidence.Rank():
		return b
	default:
		return a
	}
}

// targetForSeed builds the resolved target a seed already is.
//
// Line, Confidence and Source are left unset: the symbol index does not carry
// them, and capability.ResolveTarget takes exactly this shortcut on its own
// fallback path. Filling them with plausible values would be a fabrication in a
// package whose entire premise is that it does not do that.
func targetForSeed(s capability.Symbol) capability.Target {
	return capability.Target{
		Query: s.FQN, SymbolID: s.ID, Name: s.Name, FQN: s.FQN,
		Kind: s.Kind, FilePath: s.FilePath, Exported: s.Exported,
	}
}

// unrebase returns index-space paths to the repository space every file-shaped
// field on DiffReport is published in. Rebasing was a pure prefix strip, so
// putting the prefix back is exact rather than a guess.
func unrebase(paths []string, prefix string) []string {
	if prefix == "" || len(paths) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, prefix+p)
	}
	return out
}

func changedPaths(changes []git.Change) []string {
	if len(changes) == 0 {
		return nil
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

// seedsFromChanges resolves changed paths to the symbols defined in them.
//
// Both names of a rename are probed, and a deletion is probed at the only name
// it has. This is not a nicety: the run was indexed at the BASE revision, so a
// renamed or deleted file's symbols are filed under the old path, and seeding
// from the post-change name alone would report an empty blast radius for
// exactly the two changes most likely to break a caller.
//
// Lookup goes through SymbolIndex.InDir rather than a scan of the whole symbol
// table, so the cost is proportional to the changed directories' contents
// rather than to repository size.
func seedsFromChanges(idx *capability.SymbolIndex, changes []git.Change) (seeds []Seed, unindexed []string) {
	seen := make(map[int64]struct{})
	// byDir is filled once per directory, however many of its files changed.
	byDir := make(map[string][]capability.Symbol)
	symbolsIn := func(p string) []capability.Symbol {
		dir := path.Dir(p)
		syms, ok := byDir[dir]
		if !ok {
			syms = idx.InDir(dir)
			byDir[dir] = syms
		}
		var out []capability.Symbol
		for _, s := range syms {
			if s.FilePath == p {
				out = append(out, s)
			}
		}
		return out
	}

	for _, c := range changes {
		found := false
		for _, p := range indexedNames(c) {
			for _, s := range symbolsIn(p) {
				if _, dup := seen[s.ID]; dup {
					continue
				}
				seen[s.ID] = struct{}{}
				seeds = append(seeds, Seed{Symbol: s, ChangeKind: c.Kind, IndexedPath: p})
				found = true
			}
		}
		if !found {
			unindexed = append(unindexed, c.Path)
		}
	}

	// Sorted so the MaxSeeds cap takes a deterministic prefix: the same diff
	// analysed twice must produce the same answer, cap or no cap.
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].ID < seeds[j].ID })
	return seeds, unindexed
}

// indexedNames are the paths a change's symbols might be filed under.
func indexedNames(c git.Change) []string {
	if c.OldPath != "" && c.OldPath != c.Path {
		return []string{c.Path, c.OldPath}
	}
	return []string{c.Path}
}

// checkIndexedRevision decides whether the index describes the change being
// analysed, and returns the doubts if it cannot say that it does.
//
// # The rule
//
// The index is a snapshot of ONE commit. A diff has TWO ends. The analysis is
// sound when the snapshot is one of those two ends, and only then:
//
//   - index at HeadRev — the normal `--cached`-style question, "I have
//     committed this; who depended on what I touched". The symbols seeded from
//     are the post-change ones.
//   - index at BaseRev — "I am about to make this change". The symbols seeded
//     from are the pre-change ones, which is the case seedsFromChanges is
//     already built for (it probes a rename's old name for exactly this
//     reason).
//
// Anything else is an index of code that is at neither end of the range, and
// every symbol id in the report then belongs to a file the diff never mentions.
//
// # Why the comparison is on SHAs, not strings
//
// "HEAD", "main", "v2.0" and a SHA are routinely the same commit and never the
// same string. String equality alone would warn on almost every correct index —
// and a warning that fires when nothing is wrong is a warning nobody reads,
// which is how the real one gets ignored. So a literal match is accepted as a
// shortcut, and everything else is resolved through RevisionResolver.
//
// The empty string is handled with ChangedSince's own conventions rather than a
// second set: an empty HeadRev means HEAD, and an empty BaseRev is the EMPTY
// TREE, which is a position and not a commit — no index can have been built at
// it, so it is not offered as a candidate match.
//
// # Not knowing is an answer
//
// Every branch that cannot complete the comparison — no RunSource, no recorded
// revision, no resolver, a revision that does not resolve — returns an Unknown
// rather than nothing. An unverified index reported as verified is the failure
// this whole function exists to prevent, and defaulting to silence would
// reintroduce it in the exact conditions where it is most likely.
func checkIndexedRevision(ctx context.Context, req DiffRequest, runID int64) (string, []Unknown, error) {
	rangeName := fmt.Sprintf("%s..%s", orAll(req.BaseRev), orHead(req.HeadRev))

	if req.Runs == nil {
		return "", []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: "No run source was supplied, so the commit this index was built from is unknown and " +
				"could not be compared against " + rangeName + ". If the index describes different code " +
				"than the diff, every symbol below is about that other code; nothing here detects it.",
		}}, nil
	}

	run, err := req.Runs.Run(ctx, runID)
	if err != nil {
		return "", nil, fmt.Errorf("impact: indexed revision of run %d: %w", runID, err)
	}
	if run == nil {
		// Distinct from a run that exists and recorded nothing: this one says
		// the caller is querying a run id the registry has never heard of, and
		// naming the wrong cause would send a reader to re-index when the fix
		// is to pass a run that exists.
		return "", []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: fmt.Sprintf("Index run %d does not exist in this registry, so there is no record of "+
				"which commit the symbols below were indexed from and no comparison against %s was "+
				"possible. Either the run id is wrong or the database was rebuilt; the report below is "+
				"unverified provenance either way.", runID, rangeName),
		}}, nil
	}
	if run.GitRevision == "" {
		return "", []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: fmt.Sprintf("Index run %d recorded no git revision, so it cannot be compared against %s. "+
				"This is the state of an index built outside a git repository, or by a version of "+
				"`cortex index` that did not stamp one; either way the report below is unverified "+
				"against the change it claims to describe.", runID, rangeName),
		}}, nil
	}
	rev := run.GitRevision

	// The two ends of the range that a commit can equal. BaseRev == "" is the
	// empty tree and is deliberately absent.
	candidates := make([]string, 0, 2)
	if req.BaseRev != "" {
		candidates = append(candidates, req.BaseRev)
	}
	candidates = append(candidates, orHead(req.HeadRev))
	for _, c := range candidates {
		if c == rev {
			return rev, nil, nil
		}
	}

	resolver := req.Revisions
	if resolver == nil {
		// The production wiring passes one *git.Repo as Changes; using it is
		// what makes the check work by default instead of only when remembered.
		if r, ok := req.Changes.(RevisionResolver); ok {
			resolver = r
		}
	}
	if resolver == nil {
		return rev, []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: fmt.Sprintf("The index was built at %s and the range analysed is %s. No revision "+
				"resolver was supplied, so RepoCortex could not tell whether those name the same commit. "+
				"They may; nothing here checked.", shortRev(rev), rangeName),
		}}, nil
	}

	revSHA, err := resolver.ResolveRevision(ctx, rev)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return rev, nil, cerr
		}
		return rev, []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: fmt.Sprintf("The index records revision %s, which this repository could not resolve "+
				"(%v). It may be a commit from a different repository, or one that has since been "+
				"garbage-collected; either way the report below could not be checked against %s.",
				shortRev(rev), err, rangeName),
		}}, nil
	}

	var unresolved []string
	for _, c := range candidates {
		sha, err := resolver.ResolveRevision(ctx, c)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return rev, nil, cerr
			}
			unresolved = append(unresolved, fmt.Sprintf("%s (%v)", c, err))
			continue
		}
		if sha != "" && sha == revSHA {
			return rev, nil, nil
		}
	}

	if len(unresolved) > 0 {
		return rev, []Unknown{{
			Kind: UnknownStaleIndex,
			Detail: fmt.Sprintf("The index was built at %s, and %s of the analysed range %s could not be "+
				"resolved, so RepoCortex could not confirm the index describes either end of this change. "+
				"Treat the report below as unverified provenance, not as a checked one.",
				shortRev(revSHA), strings.Join(unresolved, ", "), rangeName),
		}}, nil
	}

	return rev, []Unknown{{
		Kind: UnknownStaleIndex,
		Detail: fmt.Sprintf("STALE INDEX: this index was built at commit %s, which is neither end of the "+
			"analysed range %s. The blast radius below was computed from symbols, files and edges as they "+
			"existed at %s — it does not describe the change you asked about. Re-run `cortex index` at "+
			"one end of the range and ask again.", shortRev(revSHA), rangeName, shortRev(revSHA)),
	}}, nil
}

// unindexedChangesUnknown states an empty answer over a non-empty diff.
//
// The honest part is the second cause. Every changed file being unindexed is
// occasionally true — a docs-only commit — and is also exactly what a path-space
// mismatch looks like, because git.Open walks up to the enclosing .git and
// returns REPO-relative paths while the symbol index holds WALKER-relative
// ones. From inside this function the two are indistinguishable, so both are
// named rather than the reassuring one being assumed.
func unindexedChangesUnknown(changed int, prefix string, indexedSymbols int) Unknown {
	rebased := "no path rebasing was applied (DiffRequest.IndexRoot was unset or equal to the repository root)"
	if prefix != "" {
		rebased = fmt.Sprintf("changed paths were rebased onto the index root by stripping %q", prefix)
	}
	return Unknown{
		Kind: UnknownUnindexedChanges,
		Detail: fmt.Sprintf("All %d changed path(s) are unindexed: the run holds no symbol in any of them, "+
			"so NOTHING was analysed and the empty result above is not a finding of \"nothing at risk\". "+
			"Either the diff genuinely touches no indexed code, or the diff's paths and the index's paths "+
			"are in different spaces — git reports paths relative to the repository root, the index stores "+
			"them relative to the directory it was walked from. Here, %s, against an index of %d symbol(s).",
			changed, rebased, indexedSymbols),
		Count: int64(changed),
	}
}

// rebasePrefix is the path segment that turns a repo-relative path into an
// index-relative one, or "" when they are the same space.
//
// An unset IndexRoot yields "", which is correct whenever the index root IS the
// git root and wrong otherwise — and the wrongness is not silent: it surfaces
// as every changed file unindexed, which unindexedChangesUnknown names.
func rebasePrefix(req DiffRequest) (string, []Unknown, error) {
	if req.IndexRoot == "" {
		return "", nil, nil
	}
	// A caller who set IndexRoot has said the two spaces differ. Every branch
	// below that cannot act on that says so, because the failure mode is not an
	// error: it is a report where some paths happen to match, the rest silently
	// do not, and nothing distinguishes "this file has no symbols" from "this
	// file was looked up under a name it never had".
	rooter, ok := req.Changes.(Rooter)
	if !ok {
		return "", []Unknown{notRebasedUnknown(req.IndexRoot,
			"the change source does not report the root its paths are relative to")}, nil
	}
	repoRoot := rooter.Root()
	if repoRoot == "" {
		return "", []Unknown{notRebasedUnknown(req.IndexRoot,
			"the change source reported an empty root")}, nil
	}
	rel, err := filepath.Rel(repoRoot, req.IndexRoot)
	if err != nil {
		return "", nil, fmt.Errorf("impact: locate index root %q inside repository %q: %w",
			req.IndexRoot, repoRoot, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" {
		// The index root IS the repository root: the spaces already agree and
		// there is nothing to doubt.
		return "", nil, nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", []Unknown{notRebasedUnknown(req.IndexRoot,
			fmt.Sprintf("it lies outside the repository root %q, so no prefix relates the two", repoRoot))}, nil
	}
	return rel + "/", nil, nil
}

// notRebasedUnknown states an IndexRoot that was set and could not be applied.
func notRebasedUnknown(indexRoot, because string) Unknown {
	return Unknown{
		Kind: UnknownPathSpace,
		Detail: fmt.Sprintf("DiffRequest.IndexRoot is %q, but no path rebasing could be applied because %s. "+
			"The diff's paths were matched against the index unchanged, so if the index really was walked "+
			"from a subdirectory the two are in different path spaces: files that missed may exist, and "+
			"files that matched may be different files of the same name. Fix the wiring rather than reading "+
			"this report as a finding.", indexRoot, because),
	}
}

// rebaseChanges splits a diff into the part the index could hold and the part
// it could not.
//
// Paths outside the index root are returned separately rather than passed
// through: "other/x.go" in a repository whose index root is "sub/" is NOT the
// indexed "other/x.go" (which is "sub/other/x.go"), and matching them would
// seed the blast radius from an unrelated file — a wrong answer where the bug
// being fixed only produced an empty one.
func rebaseChanges(changes []git.Change, prefix string) (inside []git.Change, outside []string) {
	if prefix == "" {
		return changes, nil
	}
	inside = make([]git.Change, 0, len(changes))
	for _, c := range changes {
		p, ok := strings.CutPrefix(c.Path, prefix)
		if !ok {
			outside = append(outside, c.Path)
			continue
		}
		c.Path = p
		if c.OldPath != "" {
			if op, ok := strings.CutPrefix(c.OldPath, prefix); ok {
				c.OldPath = op
			} else {
				// A file renamed into the indexed subtree from outside it: the
				// old name has no index-space form, so it is dropped rather
				// than probed at a name that would mean a different file.
				c.OldPath = ""
			}
		}
		inside = append(inside, c)
	}
	return inside, outside
}

// unknownSet merges the seeds' unknowns without inventing or losing one.
//
// Below-floor unknowns are the reason this is a type rather than a map. Their
// Detail embeds their own count, so a 200-seed diff emitted ~200 rows that
// differed only in a number — noise that buries the unknowns a reader must act
// on. They are folded into one row whose arithmetic is stated: a SUM of
// per-seed figures, which double-counts a symbol hidden from two seeds and is
// therefore a bound on the excluded region rather than a distinct-symbol count.
// Every other kind keys on Kind and Detail, so two genuinely different reasons
// stay two rows.
type unknownSet struct {
	order []Unknown
	seen  map[string]struct{}

	floor        model.Confidence
	floorTotal   int64
	floorSeeds   int
	floorBounded bool
}

func newUnknownSet(seeds int, floor model.Confidence) *unknownSet {
	return &unknownSet{
		order: make([]Unknown, 0, seeds*2),
		seen:  make(map[string]struct{}, seeds*2),
		floor: floor,
	}
}

func (s *unknownSet) addAll(us []Unknown) {
	for _, u := range us {
		if u.Kind == UnknownBelowConfidenceFloor {
			s.floorTotal += u.Count
			s.floorSeeds++
			s.floorBounded = s.floorBounded || u.CountIsLowerBound
			continue
		}
		key := string(u.Kind) + "|" + u.Detail
		if _, dup := s.seen[key]; dup {
			continue
		}
		s.seen[key] = struct{}{}
		s.order = append(s.order, u)
	}
}

func (s *unknownSet) list() []Unknown {
	if s.floorSeeds == 0 {
		return s.order
	}
	return append(s.order, Unknown{
		Kind: UnknownBelowConfidenceFloor,
		Detail: fmt.Sprintf("%d exclusion(s) across %d changed symbol(s): symbols that reach a changed "+
			"symbol only through edges weaker than %q are left out of the counts above. This figure is "+
			"the SUM of the per-seed counts, so a symbol excluded from two seeds is counted twice — it "+
			"bounds the excluded region, it is not a distinct-symbol total; each seed's own figure is in "+
			"Seeds[i].Report. Re-run with MinConfidence=low (--min-confidence low) to see them. They are "+
			"excluded by default because heuristic `low` edges are measured at 0.0-2.8%% precision "+
			"(docs/accuracy.md).", s.floorTotal, s.floorSeeds, s.floor),
		Count:             s.floorTotal,
		CountIsLowerBound: s.floorBounded,
	})
}

// orHead applies ChangedSince's convention that an empty right-hand revision
// means HEAD, so the message a reader sees names the commit actually compared.
func orHead(rev string) string {
	if rev == "" {
		return "HEAD"
	}
	return rev
}

// orAll names the empty tree for the same reason.
func orAll(rev string) string {
	if rev == "" {
		return "<empty tree>"
	}
	return rev
}

// shortRev abbreviates a SHA for prose and leaves a symbolic name alone.
func shortRev(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}

// truncationSet accumulates the distinct reasons a diff report is incomplete,
// with how many of its walks hit each.
//
// It exists because concatenation was wrong twice over. A 151-seed diff merged
// 151 reasons into one ~700-byte string that said the same three things fifty
// times each, which reached --json verbatim; and because nothing had counted
// them, each renderer condensed that string for itself, so the same field
// printed under TRUNCATED: and under WHAT THIS DOES NOT SAY disagreed about
// the counts. One fact is now counted once, here, and every surface reads the
// same answer.
//
// Reasons arrive as strings because that is what graph.Result publishes, and
// they are split on the separator that type reserves for exactly this — see
// graph.Result.TruncatedReason. A reason is otherwise opaque: this type counts
// sentences, it does not parse them.
type truncationSet struct {
	order  []string
	counts map[string]int
}

// truncationSep is graph.Result.TruncatedReason's reserved reason separator.
const truncationSep = "; "

func (t *truncationSet) add(truncated bool, reason string) {
	if !truncated || reason == "" {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	for _, r := range strings.Split(reason, truncationSep) {
		if r == "" {
			continue
		}
		if _, seen := t.counts[r]; !seen {
			t.order = append(t.order, r)
		}
		t.counts[r]++
	}
}

// result renders the set in first-seen order, which is seed order and so is
// deterministic. A reason more than one walk hit carries its multiplicity
// rather than being repeated: dropping the count would understate how much of
// the answer is missing, and repeating the sentence is what this replaced.
func (t *truncationSet) result() (bool, string) {
	if len(t.order) == 0 {
		return false, ""
	}
	parts := make([]string, 0, len(t.order))
	for _, r := range t.order {
		if n := t.counts[r]; n > 1 {
			r = fmt.Sprintf("%s (×%d)", r, n)
		}
		parts = append(parts, r)
	}
	return true, strings.Join(parts, truncationSep)
}
