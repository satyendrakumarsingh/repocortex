package infer

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # A21 — the deferred pass for inherited method dispatch
//
// A call to a method a type INHERITS cannot be bound by the tiers in pick().
// `payment.settle()` where Payment extends BasePayment and only BasePayment
// declares settle: tier 2 asks whether a candidate is OWNED by Payment, the
// answer is no for every candidate, and the reference ends up ambiguous. The
// evidence needed — that Payment IS a BasePayment — is an EXTENDS edge, and
// until internal/parse began emitting inheritance and interface references
// there were none in the heuristic index to consult.
//
// Why this is a SECOND PASS and not another tier. The ancestry chain is itself
// built out of resolved references: an EXTENDS reference from Payment to
// BasePayment is bound by the same loop that is binding this call. The set of
// supertype edges therefore GROWS while resolution runs, and a tier consulting
// it mid-loop would answer differently depending on which file was resolved
// first — the same repository indexed twice would produce different edges. A
// reference that needs ancestry is instead PARKED, and drained by
// ResolveDeferred once the loop is over and the hierarchy has stopped moving.
//
// What this pass deliberately does NOT do, and why. It never revisits a
// reference that pass one BOUND, only ones it refused. That keeps it strictly
// additive: it can turn "no edge" into "an edge or a recorded ambiguity", and
// it can do nothing else. The two guards recorded above ResolveFile were both
// reverted because they changed which references resolved — A19 in particular
// lost 2.4pp of high-tier precision not by emitting wrong edges directly but by
// SHRINKING candidate sets until honest ambiguity tipped into confident wrong
// answers. A pass that only ever looks at references already recorded as
// ambiguous cannot repeat that mistake: there is no ambiguity left for it to
// destroy.

// MaxAncestryDepth bounds how far up a hierarchy the walk will look.
//
// It is a termination guarantee first and a precision knob second. The cycle
// guard in inheritedBinding already stops A -> B -> A, but a heuristic index
// can also produce a long chain of DISTINCT types that is simply wrong, and
// walking it costs work per deferred reference. Eight also comfortably exceeds
// real hierarchies: a Java framework class four or five levels below Object is
// deep, and by depth two this pass is already reporting Low confidence, so
// nothing above the cap would carry weight worth having.
const MaxAncestryDepth = 8

// deferChunk is how many parked references are drained at once.
//
// The drain asks for candidates and supertypes in bulk, and both requests are
// proportional to the chunk. Draining the whole queue in one request would put
// every deferred name resident at once, which is the unbounded accumulation
// this pipeline has already been bitten by. 512 keeps each round trip
// worthwhile while keeping the working set flat.
const deferChunk = 512

// MaxDeferred bounds the parked queue.
//
// Each entry is small, but "small times unbounded" is how resolution peaked at
// 6.5 GB once before. Past the cap a reference simply keeps the outcome pass
// one gave it. Which references are dropped is deterministic — the queue fills
// in reference order — so this bounds memory without making output depend on
// anything but the input.
const MaxDeferred = 200_000

// Ancestry supplies the supertypes of a named type.
//
// Declared HERE, at the consumer, for the reason internal/context/ports.go
// records: this package must depend on an abstraction, never on the registry's
// concrete type. One method, taking a batch and returning a lookup, because
// that is the single question this pass needs answered and a port a fake cannot
// cheaply implement has stopped being a test aid.
//
// Contract:
//   - Keys in the returned map are the requested names, VERBATIM. A type with
//     no supertypes may be absent or present with an empty slice; the two mean
//     the same thing.
//   - Each slice is ordered DETERMINISTICALLY for a given index. This pass
//     never iterates the map — it indexes it by a name from a slice it
//     controls — but it does walk the slices, and two ancestors at the same
//     depth must be seen in the same order on every run.
//   - "Supertypes" means both channels: a superclass (EXTENDS) and an
//     implemented interface (IMPLEMENTS). A method reached through either is
//     inherited as far as a call site is concerned.
//
// No compile-time `var _ Ancestry = ...` proof is asserted here: nothing in
// the registry answers this question yet, and manufacturing an adapter in this
// package would be exactly the dependency the port exists to avoid. Until a
// caller supplies one, the pass is inert — see ResolveDeferred.
type Ancestry interface {
	Supertypes(ctx context.Context, types []string) (map[string][]string, error)
}

// deferredRef is a reference parked for the second pass.
//
// It carries COPIES of everything the drain needs, and in particular srcID,
// resolved from the file's symbols at the moment it was parked.
// registry.EachFilePageWithRefs states that a FileRefs must not be retained
// beyond the callback, so holding the reference and looking its enclosing
// symbol up later is not available: by drain time that slice belongs to another
// page.
type deferredRef struct {
	refID    int64
	name     string
	kind     string
	receiver string
	// spec is the referencing file's language strategy, carried rather than
	// re-selected at drain time for the same reason srcID is: by then the
	// FileRefs this came from belongs to another page.
	spec     LanguageSpec
	filePath string
	line     int
	col      int
	srcID    int64
	hasSrc   bool
}

// deferInherited parks a reference pass one refused, if ancestry could
// plausibly settle it.
//
// Called only from the AMBIGUOUS arms of ResolveFile. An unresolved reference —
// one whose name matches nothing indexed at all — is not parked: no ancestor
// can define a method the repository does not contain, so walking for it would
// be work with no reachable outcome.
func (r *Resolver) deferInherited(
	spec LanguageSpec,
	fr registry.FileRefs,
	ref registry.Reference,
	stats *Stats,
) {
	if r.ancestry == nil || !receiverNamesAType(spec, ref.Receiver) {
		return
	}
	if len(r.deferred) >= MaxDeferred {
		return
	}
	d := deferredRef{
		refID:    ref.ID,
		name:     ref.Name,
		kind:     ref.Kind,
		receiver: strings.TrimPrefix(ref.Receiver, "*"),
		spec:     spec,
		filePath: fr.Path,
		line:     ref.Line,
		col:      ref.Col,
	}
	d.srcID, d.hasSrc = enclosing(fr.Symbols, ref.Line)
	r.deferred = append(r.deferred, d)
	stats.Deferred++
}

// receiverNamesAType reports whether a captured receiver may be treated as the
// NAME OF A TYPE, which is the premise the whole ancestry walk rests on.
//
// Reads LanguageSpec.ReceiverDenotesType, the field that records which languages
// have the parser rewrite a receiver to its declared type (see varTypeQueries
// in internal/parse/queries.go). Elsewhere the receiver is a VARIABLE name, and
// asking "what does `s` extend" is not a type question at all — it is the same
// mis-inference A16 laundered into facts, one indirection earlier.
//
// The length floor and the self-name exclusion come from the same spec, through
// LanguageSpec.namesAType, for the reason they exist in strictReceiverMatch: at
// repository scale, short identifiers collide with real type names by accident.
//
// `this.settle()` calling an inherited method is the single most common shape
// of this problem in Java, and it is deliberately NOT handled: the receiver
// names no type, and recovering the enclosing CLASS from a reference's
// enclosing symbol (which is the method) is a separate piece of evidence this
// pass does not have. Recorded as a known gap rather than guessed at.
func receiverNamesAType(spec LanguageSpec, receiver string) bool {
	return spec.namesAType(receiver) && spec.ReceiverDenotesType
}

// Deferred reports how many references are parked awaiting the second pass.
func (r *Resolver) Deferred() int { return len(r.deferred) }

// ResolveDeferred drains the parked references once inheritance edges exist.
//
// Call it after the last ResolveFile of a run and write what it returns exactly
// as ResolveFile's output is written: the outcomes UPDATE rows pass one already
// wrote (registry.MarkResolutions is an update keyed by reference id), so a
// reference recorded ambiguous and later bound here ends up resolved.
//
// Inert in two ways, both of which matter. With no Ancestry supplied nothing is
// ever parked, so a caller that has not been wired up is byte-for-byte
// unchanged. With an Ancestry that reports no supertypes — a repository with no
// inheritance, or a language whose queries emit none — the queue drains to
// nothing: no edges, no outcomes, no stat movement.
//
// Determinism. The queue is a slice in reference order and is chunked in that
// order; supertype lists are walked as slices; every map here is indexed by a
// key taken from a slice this pass controls and is never ranged over. Same
// index in, same edges out, in the same order.
func (r *Resolver) ResolveDeferred(ctx context.Context, stats *Stats) ([]model.Edge, []registry.RefOutcome, error) {
	queued := r.deferred
	r.deferred = nil

	if r.ancestry == nil || len(queued) == 0 {
		return nil, nil, nil
	}

	edges := make([]model.Edge, 0, len(queued)/4)
	outcomes := make([]registry.RefOutcome, 0, len(queued)/4)

	for start := 0; start < len(queued); start += deferChunk {
		end := start + deferChunk
		if end > len(queued) {
			end = len(queued)
		}
		if err := r.drainChunk(ctx, queued[start:end], stats, &edges, &outcomes); err != nil {
			return nil, nil, err
		}
	}
	return edges, outcomes, nil
}

// drainChunk resolves one bounded slice of the parked queue.
func (r *Resolver) drainChunk(
	ctx context.Context,
	chunk []deferredRef,
	stats *Stats,
	edges *[]model.Edge,
	outcomes *[]registry.RefOutcome,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	symbols, err := r.candidates.Candidates(ctx, deferredNames(chunk))
	if err != nil {
		return fmt.Errorf("candidates for deferred references: %w", err)
	}
	known, err := r.ancestryClosure(ctx, chunk)
	if err != nil {
		return err
	}

	for _, d := range chunk {
		cands := symbols[d.name]
		if len(cands) == 0 {
			continue
		}
		best, ok, ambiguous := inheritedBinding(d, cands, known)

		if ambiguous {
			// Two ancestors define the same method and nothing here can say
			// which one this call reaches. That is a FACT about the hierarchy,
			// and recording it is the whole lesson of the two reverted guards:
			// picking one would be a coin flip presented as a resolution.
			stats.InheritedAmbiguous++
			*outcomes = append(*outcomes, registry.RefOutcome{
				RefID: d.refID, Resolution: registry.ResolutionAmbiguous})
			continue
		}
		if !ok || !best.confidence.AtLeast(r.opts.MinConfidence) {
			// No ancestor defines it, or the binding is weaker than the caller
			// accepts. Either way pass one's outcome stands untouched.
			continue
		}

		// Pass one counted this reference as ambiguous; it is now resolved.
		// Keeping the totals consistent matters because resolution precision is
		// published from them.
		stats.Ambiguous--
		stats.Resolved++
		stats.Inherited++
		if stats.ByConfidence == nil {
			stats.ByConfidence = map[model.Confidence]int64{}
		}
		stats.ByConfidence[best.confidence]++

		*outcomes = append(*outcomes, registry.RefOutcome{
			RefID:      d.refID,
			SymbolID:   best.entry.ID,
			Resolution: registry.ResolutionResolved,
		})

		if !d.hasSrc {
			// Same rule ResolveFile applies: a reference outside any indexed
			// symbol has no meaningful source to hang an edge on.
			continue
		}
		*edges = append(*edges, model.Edge{
			SrcID:      d.srcID,
			DstID:      best.entry.ID,
			Kind:       edgeKindFor(d.kind),
			Confidence: best.confidence,
			Evidence: model.Evidence{
				FilePath: d.filePath,
				Range: model.Range{
					Start: model.Position{Line: d.line, Col: d.col},
					End:   model.Position{Line: d.line, Col: d.col},
				},
				Source: model.SourceSourceCode,
				Detail: best.reason,
			},
		})
		stats.Edges++
	}
	return nil
}

// ancestryClosure walks the hierarchy above a chunk's receivers, breadth first,
// one batched request per level.
//
// Level by level rather than per reference on purpose: a per-reference walk is
// up to MaxAncestryDepth round trips PER REFERENCE, which is the per-reference
// query pattern the preloaded symbol index existed to avoid. This is at most
// MaxAncestryDepth requests for the entire chunk, after which every walk is
// local map lookups.
//
// A name absent from the result is recorded with a nil slice, so it is treated
// as "walked, has no supertypes" and never requested twice. A name still on the
// frontier when the depth cap is reached is left ABSENT, which the per-reference
// walk reads as "not walked" and stops at — unknown, rather than asserted empty.
func (r *Resolver) ancestryClosure(ctx context.Context, chunk []deferredRef) (map[string][]string, error) {
	known := make(map[string][]string, len(chunk))
	pending := make(map[string]struct{}, len(chunk))

	// The frontier is a SLICE, in first-appearance order, so the requests this
	// makes and the order supertypes are discovered in do not depend on map
	// iteration.
	var frontier []string
	for _, d := range chunk {
		key := strings.ToLower(d.receiver)
		if _, dup := pending[key]; dup {
			continue
		}
		pending[key] = struct{}{}
		frontier = append(frontier, d.receiver)
	}

	for depth := 0; depth < MaxAncestryDepth && len(frontier) > 0; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sup, err := r.ancestry.Supertypes(ctx, frontier)
		if err != nil {
			return nil, fmt.Errorf("supertypes at depth %d: %w", depth, err)
		}
		var next []string
		for _, name := range frontier {
			parents := sup[name]
			known[strings.ToLower(name)] = parents
			for _, p := range parents {
				if p == "" {
					continue
				}
				key := strings.ToLower(p)
				if _, walked := known[key]; walked {
					continue
				}
				if _, queued := pending[key]; queued {
					continue
				}
				pending[key] = struct{}{}
				next = append(next, p)
			}
		}
		frontier = next
	}
	return known, nil
}

// inheritedBinding finds the nearest ancestor of the receiver that defines the
// referenced method.
//
// Reports (binding, found, ambiguous). Ambiguous wins over found: if the
// nearest level that defines the method defines it TWICE, there is no answer
// to give.
//
// # Nearest wins, and why that is not a guess
//
// A method found at depth 1 beats one at depth 2 without ambiguity being
// recorded, because that is what the languages themselves specify: a subclass
// override shadows its parent's definition, so the nearest declaration IS the
// one a call reaches. Two definitions at the SAME depth — a superclass and an
// implemented interface, or two interfaces — have no such rule to appeal to,
// and that is the case reported as ambiguous.
//
// # Termination
//
// Two independent bounds, because either alone can be defeated. `seen` holds
// every type already visited, so a cycle (A extends B extends A, which a
// heuristic index absolutely can produce) revisits nothing and the frontier
// empties. MaxAncestryDepth bounds the acyclic case, where a wrong chain can be
// long without ever repeating.
func inheritedBinding(
	d deferredRef,
	cands []registry.SymbolIndexEntry,
	known map[string][]string,
) (candidate, bool, bool) {
	seen := map[string]struct{}{strings.ToLower(d.receiver): {}}
	frontier := []string{d.receiver}

	for depth := 1; depth <= MaxAncestryDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, t := range frontier {
			for _, p := range known[strings.ToLower(t)] {
				if p == "" {
					continue
				}
				key := strings.ToLower(p)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				next = append(next, p)
			}
		}
		if len(next) == 0 {
			return candidate{}, false, false
		}

		// Which candidates does an ancestor at THIS depth own? The ownership
		// test is receiverMatches — the same validated one tier 2 uses, whole
		// segments and exact suffixes, never containment — asked of the ancestor
		// instead of the receiver. Reusing it is the point: the ancestry walk
		// changes WHICH type is asked about, not how loosely the answer is
		// accepted.
		var owners []registry.SymbolIndexEntry
		var ownerNames []string
		for _, anc := range next {
			for _, c := range cands {
				if !receiverMatches(d.spec, anc, c) || containsID(owners, c.ID) {
					// Deduplicated by symbol id: one definition matched through
					// two ancestor names (or through both the qualified-name and
					// the file-name channel) is ONE definition, not a conflict.
					continue
				}
				owners = append(owners, c)
				ownerNames = append(ownerNames, anc)
			}
		}

		switch len(owners) {
		case 0:
			frontier = next
		case 1:
			return candidate{
				entry:      owners[0],
				confidence: inheritedConfidence(depth),
				reason: fmt.Sprintf(
					"not defined on %s; defined on %s, %d level(s) up its inheritance chain",
					d.receiver, ownerNames[0], depth),
			}, true, false
		default:
			return candidate{}, false, true
		}
	}
	return candidate{}, false, false
}

// inheritedConfidence is what a binding through N levels of inheritance earns.
//
// Never High, at any depth. High in this package means the evidence constrained
// the TYPE as well as the name — a definition in the same file, an owner the
// candidate's qualified name actually carries, a package the file DECLARED it
// imports. An inherited binding has neither of those. It rests on two
// inferences stacked: that the receiver token names the type it appears to, and
// that a chain of EXTENDS/IMPLEMENTS edges — themselves produced by this same
// heuristic resolver, not by a compiler — is correct end to end. Publishing
// that as High would claim more than the evidence supports, which is the one
// thing the package doc forbids outright.
//
// Depth 1 earns Medium: a single inheritance step, and the method is not
// declared on the receiver's own type, which is precisely the shape of ordinary
// inherited dispatch. It is capped there rather than raised because nothing
// here can see an override introduced by a type the index did not link, and
// nothing here confirms the receiver's STATIC type — an interface-typed
// variable named after its implementation is invisible to this pass.
//
// Depth 2 and beyond earns Low, and does not decay further because the
// confidence scale has no rung below it. Every additional level multiplies in
// another unverified EXTENDS edge and another chance that an intermediate type
// overrode the method with a definition the index never linked; by the second
// level the honest summary is already "weak signal, thresholdable", which is
// exactly what Low is defined to mean.
func inheritedConfidence(depth int) model.Confidence {
	if depth <= 1 {
		return model.ConfidenceMedium
	}
	return model.ConfidenceLow
}

// containsID reports whether a symbol is already in a set.
//
// Linear over a slice rather than a map lookup: the sets here hold at most
// MaxCandidates entries, and a slice keeps the surrounding decision free of map
// iteration entirely.
func containsID(cs []registry.SymbolIndexEntry, id int64) bool {
	for _, c := range cs {
		if c.ID == id {
			return true
		}
	}
	return false
}

// deferredNames returns the distinct names a chunk references, in first
// appearance order — the same contract as distinctNames, over parked entries.
func deferredNames(chunk []deferredRef) []string {
	seen := make(map[string]struct{}, len(chunk))
	out := make([]string, 0, len(chunk))
	for _, d := range chunk {
		if _, dup := seen[d.name]; dup {
			continue
		}
		seen[d.name] = struct{}{}
		out = append(out, d.name)
	}
	return out
}
