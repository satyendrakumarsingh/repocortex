package diffgraph

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Defaults for the bounds. A diff across distant commits is unbounded in
// principle, and an unbounded report is unreadable and unshippable.
const (
	DefaultMaxPerSection    = 200
	DefaultMaxEvidenceSites = 3
	DefaultMaxCaveatPaths   = 25
)

// Side names one end of the comparison: which index to read, and which run
// inside it.
//
// Source is per-side rather than per-Differ because of a fact about the
// registry that the CLI has to live with: CommitRun retires the previous
// committed run and deletes its rows, so ONE index database holds exactly one
// committed run. The two sides of `repocortex diff <ref>..<ref>` therefore
// normally live in two databases — the working index and a second index built
// at the other ref. Nothing here assumes they share a store, and nothing
// assumes they do not: two Sides over the same *registry.Registry with two run
// ids is equally valid.
type Side struct {
	Source IndexSource
	RunID  int64
}

// Differ compares two index runs.
//
// Holds no mutable state at all: the sources arrive per call and the snapshots
// and report are per-Compare locals, so one Differ is safe to share across
// goroutines and cannot leak facts from one comparison into the next.
type Differ struct {
	maxPerSection    int
	maxEvidenceSites int
	maxCaveatPaths   int
}

// Option configures a Differ. Functional options per CLAUDE.md §2 — the
// alternative is a config struct that grows a field per caller.
type Option func(*Differ) error

// WithMaxPerSection caps how many changes each section reports.
func WithMaxPerSection(n int) Option {
	return func(d *Differ) error {
		if n <= 0 {
			return fmt.Errorf("diffgraph: max per section must be positive, got %d", n)
		}
		d.maxPerSection = n
		return nil
	}
}

// WithMaxEvidenceSites caps how many evidence sites each edge fact carries.
func WithMaxEvidenceSites(n int) Option {
	return func(d *Differ) error {
		if n <= 0 {
			return fmt.Errorf("diffgraph: max evidence sites must be positive, got %d", n)
		}
		d.maxEvidenceSites = n
		return nil
	}
}

// WithMaxCaveatPaths caps the sample of unreliable file paths.
func WithMaxCaveatPaths(n int) Option {
	return func(d *Differ) error {
		if n <= 0 {
			return fmt.Errorf("diffgraph: max caveat paths must be positive, got %d", n)
		}
		d.maxCaveatPaths = n
		return nil
	}
}

// New builds a Differ.
func New(opts ...Option) (*Differ, error) {
	d := &Differ{
		maxPerSection:    DefaultMaxPerSection,
		maxEvidenceSites: DefaultMaxEvidenceSites,
		maxCaveatPaths:   DefaultMaxCaveatPaths,
	}
	for _, opt := range opts {
		if err := opt(d); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Compare diffs base (the older side) against head (the newer side).
//
// Comparing a run against itself returns a report where Empty() is true. That
// is the property the whole feature rests on: if a run does not compare equal
// to itself, every other output is noise. There is deliberately no
// short-circuit for "same source, same run id" — the equal case is loaded and
// compared like any other, so the property is actually tested rather than
// asserted by an early return.
func (d *Differ) Compare(ctx context.Context, base, head Side) (*Report, error) {
	if base.Source == nil || head.Source == nil {
		return nil, errors.New("diffgraph: both sides must name an index source")
	}
	baseSnap, err := loadSnapshot(ctx, base.Source, base.RunID, d.maxEvidenceSites, d.maxCaveatPaths)
	if err != nil {
		return nil, err
	}
	headSnap, err := loadSnapshot(ctx, head.Source, head.RunID, d.maxEvidenceSites, d.maxCaveatPaths)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("diffgraph: compare run %d..%d: %w", base.RunID, head.RunID, err)
	}

	rep := &Report{Base: baseSnap.info, Head: headSnap.info}
	rep.Symbols = diffSymbols(baseSnap, headSnap)
	rep.Edges = diffEdges(baseSnap, headSnap)
	rep.Endpoints = diffEndpoints(baseSnap, headSnap)
	rep.Tables = diffTables(baseSnap, headSnap)
	rep.Caveats = buildCaveats(baseSnap, headSnap, rep)

	d.bound(rep)
	return rep, nil
}

// ---------------------------------------------------------------- symbols

// diffSymbols classifies symbol-level differences.
//
// Two passes, and the second one is the reason this feature is usable. The
// first walks the two key sets in content order and reports moves for symbols
// whose identity matched but whose position did not — that is what stops an
// added import from reporting every symbol below it as removed-and-added. The
// second takes what is left over and pairs a removal with an addition when the
// two agree on (kind, name, signature) and each side has exactly one candidate,
// which recovers the cross-file move that path-qualified FQNs would otherwise
// present as an unrelated deletion and creation.
func diffSymbols(base, head *snapshot) []SymbolChange {
	var out []SymbolChange
	var removedKeys, addedKeys []SymbolKey

	mergeSorted(base.symKeys, head.symKeys, compareSymbolKey,
		func(k SymbolKey) { // both sides
			b, h := base.syms[k], head.syms[k]
			if b.Start != h.Start || b.End != h.End {
				reason, uncertain := eitherUnreliable(base, head, k.Path)
				out = append(out, SymbolChange{
					Change:    SymbolMoved,
					Key:       k,
					From:      b,
					To:        h,
					LineDelta: h.Start.Line - b.Start.Line,
					Uncertain: uncertain,
					Reason:    reason,
				})
			}
			// "This symbol lost its last caller" is a fact about the symbol,
			// not about any one edge, so it is reported here rather than left
			// for a reader to reconstruct from the edge section.
			if b.Inbound > 0 && h.Inbound == 0 {
				reason, uncertain := eitherUnreliable(base, head, k.Path)
				out = append(out, SymbolChange{
					Change:    SymbolOrphaned,
					Key:       k,
					From:      b,
					To:        h,
					Uncertain: uncertain,
					Reason:    reason,
				})
			}
		},
		func(k SymbolKey) { removedKeys = append(removedKeys, k) },
		func(k SymbolKey) { addedKeys = append(addedKeys, k) },
	)

	moved := pairCrossFileMoves(base, head, removedKeys, addedKeys)
	for _, k := range removedKeys {
		if _, ok := moved.movedFrom[k]; ok {
			continue
		}
		reason, uncertain := eitherUnreliable(base, head, k.Path)
		out = append(out, SymbolChange{
			Change: SymbolRemoved, Key: k, From: base.syms[k],
			Uncertain: uncertain, Reason: reason,
		})
	}
	for _, k := range addedKeys {
		if from, ok := moved.movedTo[k]; ok {
			reason, uncertain := eitherUnreliable(base, head, k.Path, from.Path)
			fromKey := from
			out = append(out, SymbolChange{
				Change:    SymbolMoved,
				Key:       k,
				FromKey:   &fromKey,
				From:      base.syms[from],
				To:        head.syms[k],
				LineDelta: head.syms[k].Start.Line - base.syms[from].Start.Line,
				Uncertain: uncertain,
				Reason:    reason,
			})
			continue
		}
		reason, uncertain := eitherUnreliable(base, head, k.Path)
		out = append(out, SymbolChange{
			Change: SymbolAdded, Key: k, To: head.syms[k],
			Uncertain: uncertain, Reason: reason,
		})
	}

	slices.SortFunc(out, func(a, b SymbolChange) int {
		if c := compareSymbolKey(a.Key, b.Key); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Change), string(b.Change))
	})
	return out
}

type movePairs struct {
	movedFrom map[SymbolKey]SymbolKey // removed key -> added key
	movedTo   map[SymbolKey]SymbolKey // added key -> removed key
}

// pairCrossFileMoves matches a removed symbol to an added one when they are
// unambiguously the same definition in a new place.
//
// Deliberately conservative: the identity tuple must occur EXACTLY once among
// the removals and exactly once among the additions. Anything looser starts
// pairing overloads and copy-pasted helpers, and a diff that invents moves is
// worse than one that reports two honest facts.
func pairCrossFileMoves(base, head *snapshot, removed, added []SymbolKey) movePairs {
	type ident struct {
		kind      model.EntityKind
		name      string
		signature string
	}
	identOf := func(f *SymbolFact) ident {
		return ident{kind: f.Kind, name: f.Name, signature: f.Signature}
	}

	pairs := movePairs{
		movedFrom: map[SymbolKey]SymbolKey{},
		movedTo:   map[SymbolKey]SymbolKey{},
	}

	remByIdent := map[ident][]SymbolKey{}
	for _, k := range removed { // removed is in content order
		remByIdent[identOf(base.syms[k])] = append(remByIdent[identOf(base.syms[k])], k)
	}
	addByIdent := map[ident][]SymbolKey{}
	for _, k := range added {
		addByIdent[identOf(head.syms[k])] = append(addByIdent[identOf(head.syms[k])], k)
	}

	// Iterating `removed` — a sorted slice — rather than either map keeps the
	// pairing independent of Go's map ordering.
	for _, k := range removed {
		id := identOf(base.syms[k])
		if id.name == "" {
			continue
		}
		rem, add := remByIdent[id], addByIdent[id]
		if len(rem) != 1 || len(add) != 1 {
			continue
		}
		pairs.movedFrom[k] = add[0]
		pairs.movedTo[add[0]] = k
	}
	return pairs
}

// ---------------------------------------------------------------- edges

// diffEdges classifies relationship-level differences.
//
// A confidence transition on a matched key is its own class, not an add and a
// remove. `exact -> high` means a compiler stopped proving a relationship it
// used to prove — usually a build that no longer produces a SCIP index, or a
// call that became dynamic — and it is invisible to every line-based tool.
func diffEdges(base, head *snapshot) []EdgeChange {
	var out []EdgeChange

	mergeSorted(base.edgeKeys, head.edgeKeys, compareEdgeKey,
		func(k EdgeKey) {
			b, h := base.edges[k], head.edges[k]
			if b.Confidence == h.Confidence {
				return
			}
			dir := Upgraded
			if h.Confidence.Rank() < b.Confidence.Rank() {
				dir = Downgraded
			}
			reason, uncertain := eitherUnreliable(base, head, k.Src.Path, k.Dst.Path)
			out = append(out, EdgeChange{
				Change: EdgeConfidenceChanged, Key: k, From: b, To: h,
				Direction: dir, Uncertain: uncertain, Reason: reason,
			})
		},
		func(k EdgeKey) {
			reason, uncertain := eitherUnreliable(base, head, k.Src.Path, k.Dst.Path)
			out = append(out, EdgeChange{
				Change: EdgeRemoved, Key: k, From: base.edges[k],
				Uncertain: uncertain, Reason: reason,
			})
		},
		func(k EdgeKey) {
			reason, uncertain := eitherUnreliable(base, head, k.Src.Path, k.Dst.Path)
			out = append(out, EdgeChange{
				Change: EdgeAdded, Key: k, To: head.edges[k],
				Uncertain: uncertain, Reason: reason,
			})
		},
	)

	slices.SortFunc(out, func(a, b EdgeChange) int {
		if c := compareEdgeKey(a.Key, b.Key); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Change), string(b.Change))
	})
	return out
}

// ---------------------------------------------------------------- endpoints

func diffEndpoints(base, head *snapshot) []EndpointChange {
	var out []EndpointChange

	mergeSorted(base.epKeys, head.epKeys, compareEndpointKey,
		func(k EndpointKey) {
			b, h := base.eps[k], head.eps[k]
			if slices.Equal(b.Handlers, h.Handlers) {
				return
			}
			out = append(out, EndpointChange{Change: EndpointHandlerChanged, Key: k, From: b, To: h})
		},
		func(k EndpointKey) {
			out = append(out, EndpointChange{Change: EndpointRemoved, Key: k, From: base.eps[k]})
		},
		func(k EndpointKey) {
			out = append(out, EndpointChange{Change: EndpointAdded, Key: k, To: head.eps[k]})
		},
	)

	slices.SortFunc(out, func(a, b EndpointChange) int {
		if c := compareEndpointKey(a.Key, b.Key); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Change), string(b.Change))
	})
	return out
}

// ---------------------------------------------------------------- tables

func diffTables(base, head *snapshot) []TableChange {
	var out []TableChange

	mergeSorted(base.tableNames, head.tableNames, cmp.Compare[string],
		func(name string) {
			b, h := base.tables[name], head.tables[name]
			if *b == *h {
				return
			}
			out = append(out, TableChange{
				Change: TableAccessChanged, Table: name, From: b, To: h,
				ReadersDelta: h.Readers - b.Readers,
				WritersDelta: h.Writers - b.Writers,
			})
		},
		func(name string) {
			out = append(out, TableChange{Change: TableAccessRemoved, Table: name, From: base.tables[name],
				ReadersDelta: -base.tables[name].Readers, WritersDelta: -base.tables[name].Writers})
		},
		func(name string) {
			out = append(out, TableChange{Change: TableAccessAdded, Table: name, To: head.tables[name],
				ReadersDelta: head.tables[name].Readers, WritersDelta: head.tables[name].Writers})
		},
	)

	slices.SortFunc(out, func(a, b TableChange) int {
		if c := cmp.Compare(a.Table, b.Table); c != 0 {
			return c
		}
		return cmp.Compare(string(a.Change), string(b.Change))
	})
	return out
}

// ---------------------------------------------------------------- caveats

// buildCaveats states what the comparison could not establish.
//
// Notes are appended in a fixed sequence, never from a map, so two runs of the
// same comparison produce the same bytes.
func buildCaveats(base, head *snapshot, rep *Report) Caveats {
	c := Caveats{
		Comparable:    true,
		DanglingEdges: base.danglingEdges + head.danglingEdges,
	}

	for _, side := range []struct {
		label string
		snap  *snapshot
	}{{"base", base}, {"head", head}} {
		switch side.snap.info.Status {
		case model.StatusFailed, model.StatusIndexing:
			c.Comparable = false
			c.Notes = append(c.Notes, fmt.Sprintf(
				"%s run %d is %s: absence of a symbol or edge proves nothing about the code",
				side.label, side.snap.info.RunID, side.snap.info.Status))
		case model.StatusDegraded:
			c.Notes = append(c.Notes, fmt.Sprintf(
				"%s run %d is degraded (%d parse failures): a missing symbol or edge may be an indexing gap, not a deletion",
				side.label, side.snap.info.RunID, side.snap.info.ParseFailures))
		}
		if side.snap.orphanSymbols > 0 {
			c.Notes = append(c.Notes, fmt.Sprintf(
				"%s run %d: %d symbols had no file row and were excluded",
				side.label, side.snap.info.RunID, side.snap.orphanSymbols))
		}
	}

	if base.info.AccuracyMode != head.info.AccuracyMode {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"accuracy mode differs (base %q, head %q): confidence transitions may describe the indexer rather than the code — a run without a compiler index cannot produce exact at all",
			base.info.AccuracyMode, head.info.AccuracyMode))
	}

	// Unreliable files: the union of the two sides, sorted, bounded.
	seen := map[string]bool{}
	var paths []string
	for _, p := range base.badPaths {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, p := range head.badPaths {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	c.UnreliableFiles = len(paths)
	c.UnreliablePaths = paths
	if len(paths) > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"%d files did not parse cleanly in one of the runs; changes inside them are marked uncertain", len(paths)))
	}
	if c.DanglingEdges > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"%d edges referenced a symbol absent from their own run and were excluded from the comparison", c.DanglingEdges))
	}

	// The digest cross-check. Equal digests mean the two runs hashed
	// identically over content, so a non-empty diff is a bug in THIS package
	// rather than a change in the code, and the report says so instead of
	// being believed.
	if base.info.Digest != "" && base.info.Digest == head.info.Digest {
		c.DigestsEqual = true
		c.Notes = append(c.Notes, "registry digests are equal: the two runs indexed identical content")
		if !rep.Empty() {
			c.Notes = append(c.Notes, "INCONSISTENT: digests are equal but differences were reported — the comparison is wrong, not the code")
		}
	}
	return c
}

// eitherUnreliable reports whether any of the given paths failed to parse
// cleanly on either side.
func eitherUnreliable(base, head *snapshot, paths ...string) (string, bool) {
	if r, ok := base.unreliable(paths...); ok {
		return "base: " + r, true
	}
	if r, ok := head.unreliable(paths...); ok {
		return "head: " + r, true
	}
	return "", false
}

// ---------------------------------------------------------------- bounding

// bound caps every section and records exactly what it dropped.
//
// Capping happens AFTER sorting, never before: the entries a bounded report
// keeps are then a function of content order alone, so the same diff run twice
// omits the same entries. Truncation carries the ordering it capped under, so
// a reader can tell what "the first 200" means.
func (d *Differ) bound(rep *Report) {
	var trunc []Truncation
	rep.Symbols, trunc = capSection(rep.Symbols, d.maxPerSection, "symbols", "path, fqn, kind, change", trunc)
	rep.Edges, trunc = capSection(rep.Edges, d.maxPerSection, "edges", "src, dst, kind, change", trunc)
	rep.Endpoints, trunc = capSection(rep.Endpoints, d.maxPerSection, "endpoints", "route, path, change", trunc)
	rep.Tables, trunc = capSection(rep.Tables, d.maxPerSection, "tables", "table, change", trunc)
	rep.Truncations = trunc
}

func capSection[T any](items []T, max int, section, orderedBy string, trunc []Truncation) ([]T, []Truncation) {
	if len(items) <= max {
		return items, trunc
	}
	total := len(items)
	kept := items[:max]
	return kept, append(trunc, Truncation{
		Section:   section,
		Shown:     max,
		Total:     total,
		Omitted:   total - max,
		OrderedBy: orderedBy,
	})
}

// mergeSorted walks two slices that are each sorted by cmpFn and calls exactly
// one callback per distinct key.
//
// A merge rather than a map lookup because the OUTPUT order has to be a
// function of the content order and nothing else. Ranging over a Go map here —
// the obvious implementation — would make the report's ordering depend on the
// runtime's hash seed, which changes per process.
func mergeSorted[K any](a, b []K, cmpFn func(K, K) int, both, onlyA, onlyB func(K)) {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch c := cmpFn(a[i], b[j]); {
		case c == 0:
			both(a[i])
			i++
			j++
		case c < 0:
			onlyA(a[i])
			i++
		default:
			onlyB(b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		onlyA(a[i])
	}
	for ; j < len(b); j++ {
		onlyB(b[j])
	}
}
