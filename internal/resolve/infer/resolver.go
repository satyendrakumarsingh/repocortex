// Package infer binds unresolved references to symbol definitions using
// imports and receiver-type inference, and attaches a confidence level to every
// binding it makes.
//
// This package is where RepoCortex's central promise is either kept or broken.
// Tree-sitter produces a syntax tree per file; it cannot tell you that
// `svc.updateStatus()` refers to `com.pay.TransactionService#updateStatus`.
// Everything an agent actually wants — who calls this, what breaks if I change
// it, which tests cover it — rests on that binding being right.
//
// The design principle is therefore NOT "resolve as much as possible". It is
// "never claim more than the evidence supports". A wrong edge presented as fact
// is worse than a missing one: an impact analysis that misses a caller makes a
// developer cautious, while one that invents a caller makes them distrust the
// entire tool.
//
// Concretely, that means:
//
//   - A reference whose name matches 200 symbols produces NO edge. Emitting 200
//     weak edges would flood the graph and make every downstream traversal
//     useless. It is recorded as ambiguous, which is a fact worth having.
//   - Confidence reflects the actual disambiguating evidence, not a default.
//   - Nothing here ever emits ConfidenceExact. That is reserved for
//     compiler-backed SCIP data and enforced in Go and in SQL.
package infer

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// MaxCandidates is the point past which a name is treated as too common to
// resolve.
//
// Names like `Get`, `Save`, `handle` and `process` occur hundreds of times in a
// large repository. Binding to an arbitrary one would be a coin flip dressed up
// as an answer, and binding to all of them would add hundreds of edges per call
// site — a graph that is technically complete and practically unusable.
//
// Eight is chosen because genuine overload sets and small interface hierarchies
// stay under it, while genuinely generic names blow past it immediately.
//
// This is the CEILING, and it is a cross-package contract: internal/pipeline
// sizes its candidate cache at MaxCandidates+1 rows per name. A language may
// lower its own cap through LanguageSpec.MaxCandidates; it may not raise it
// past this, because the rows would not be there. TestSpecsRespectCandidateCap
// enforces that.
const MaxCandidates = 8

// Options configures resolution.
type Options struct {
	// MinConfidence drops bindings weaker than this instead of recording them.
	// Defaults to ConfidenceLow (record everything, labelled).
	MinConfidence model.Confidence

	// Ancestry enables the deferred pass for inherited method dispatch. Nil
	// disables it entirely: nothing is parked and ResolveDeferred is a no-op,
	// so a caller that has not been wired up resolves exactly as before. See
	// inherited.go.
	Ancestry Ancestry
}

// Stats reports what a resolution pass achieved. Every field is counted.
type Stats struct {
	References int64
	Resolved   int64
	Ambiguous  int64
	Unresolved int64
	Edges      int64

	// Deferred counts references parked for the inherited-dispatch pass, and
	// Inherited/InheritedAmbiguous what draining them produced. An inherited
	// binding also moves Resolved and Ambiguous, so these three describe WHERE
	// the second pass acted without double-counting the outcome.
	Deferred           int64
	Inherited          int64
	InheritedAmbiguous int64

	// A8 — CapExceeded and CapDropped separate the two reasons a reference ends
	// up in Ambiguous, which until now were indistinguishable.
	//
	// Ambiguous conflates "two candidates and no evidence to choose" with "this
	// name has 200 definitions and was never weighable at all". They are
	// different products of failure and call for different fixes: the first
	// wants better evidence, the second wants a narrower query or an accepted
	// gap. Reporting one number for both hides which one a repository has.
	//
	// CapExceeded counts references whose RAW candidate set was larger than the
	// language's LanguageSpec.MaxCandidates. CapDropped counts the subset the
	// receiver could not then rescue, and is therefore the part that actually
	// lands in Ambiguous — CapExceeded minus CapDropped is what narrowByReceiver
	// saved. Both are pure counters: nothing below reads them, so they cannot
	// change a binding.
	//
	// This matters more than it did, because `repocortex accuracy` now publishes
	// recall, and a cap-drop is a named, countable component of the calls that
	// produce no edge.
	CapExceeded int64
	CapDropped  int64

	// CrossLanguageDropped counts candidates refused because the two languages
	// share no module system. Reported so the exclusion is visible rather than
	// silent — the same reason CapDropped exists.
	CrossLanguageDropped int64

	ByConfidence map[model.Confidence]int64
}

// Candidates supplies the symbols a name could refer to.
//
// Declared here, at the consumer, rather than in the registry: this package
// needs one question answered and should not be able to reach anything else.
// It is what lets resolution hold a bounded working set instead of the whole
// run — see registry.CandidateCache.
type Candidates interface {
	// Candidates returns the possible targets for each name. A name with no
	// candidates may be absent from the map or present with an empty slice;
	// the two mean the same thing.
	Candidates(ctx context.Context, names []string) (map[string][]registry.SymbolIndexEntry, error)
}

// Resolver binds references for one index run.
//
// A Resolver carries the queue of references parked for the inherited-dispatch
// pass, so it is stateful across ResolveFile calls and is NOT safe for
// concurrent use. That matches how it is driven: pipeline.Resolve streams files
// through one resolver sequentially, and the queue's order — which is
// reference order — is what makes the second pass deterministic.
type Resolver struct {
	candidates Candidates
	ancestry   Ancestry
	opts       Options

	// specs answers every question resolution has about a language. Held per
	// Resolver rather than reached through a package-level table, so nothing
	// here depends on global state or on package init order. See language.go.
	specs *Specs

	// deferred holds references awaiting ancestry, in reference order. See
	// inherited.go for why they cannot be settled during the main loop.
	deferred []deferredRef
}

// New builds a Resolver over a preloaded symbol index.
//
// Retained for callers that already hold the whole run in memory — tests, and
// anything resolving a tree small enough that loading it is free. Streaming
// callers should use NewStreaming, which is what keeps memory flat.
func New(symbols map[string][]registry.SymbolIndexEntry, opts Options) *Resolver {
	return NewStreaming(mapCandidates(symbols), opts)
}

// NewStreaming builds a Resolver that asks for candidates as it needs them.
func NewStreaming(c Candidates, opts Options) *Resolver {
	if !opts.MinConfidence.Valid() {
		opts.MinConfidence = model.ConfidenceLow
	}
	return &Resolver{candidates: c, ancestry: opts.Ancestry, opts: opts, specs: NewSpecs()}
}

// mapCandidates adapts a preloaded index to the Candidates port.
type mapCandidates map[string][]registry.SymbolIndexEntry

func (m mapCandidates) Candidates(_ context.Context, names []string) (map[string][]registry.SymbolIndexEntry, error) {
	out := make(map[string][]registry.SymbolIndexEntry, len(names))
	for _, n := range names {
		if c := m[n]; len(c) > 0 {
			out[n] = c
		}
	}
	return out, nil
}

// candidate pairs a possible target with the reason it was preferred.
type candidate struct {
	entry      registry.SymbolIndexEntry
	confidence model.Confidence
	reason     string
}

// # Why candidates are not filtered before the tiers
//
// Two guards were written to drop candidates before pick() weighs them. Both
// were plausible, both were implemented, and both were reverted on measurement
// against compiler ground truth. They are recorded here, with their numbers, so
// that neither is re-derived from first principles and re-landed.
//
// A18 — "a bare call cannot name a method". In Go and Rust a call with no
// receiver cannot be a method call, so method candidates could be dropped
// outright. The rule reads ref.Receiver == "" as "the source wrote no
// receiver". It does not mean that. receiverQueries (internal/parse/queries.go)
// captures only a SIMPLE IDENTIFIER receiver — the Go pattern is
// `operand: (identifier)` — so in s.repo.Save(id) the operand is a
// selector_expression, nothing matches, and Receiver is left empty. An empty
// receiver conflates "no receiver in the source" with "a receiver the query
// cannot express", and chained calls are ordinary Go. Measured: it took
// `repocortex callers Save` on the CLI fixture from two bound callers to zero.
// Unit tests over synthetic references passed throughout, because they set
// Receiver directly; only the end-to-end path exercises the parser that leaves
// it empty. The rule becomes sound the moment parse can distinguish the two
// cases — an explicit "receiver present but unparsed" signal would do it.
//
// A19 — "production code must not bind to a test definition". Test trees are
// full of stubs and doubles that reuse production names, so a candidate in a
// test file was dropped when the referencing file was not test code. The
// premise is true and the guard still lost precision. Measured on prometheus,
// 727 Go files, adjudicated against a 57 MB scip-go index, same corpus and same
// index for both arms:
//
//	                 baseline    with A19    delta
//	high             86.7%       84.3%       -2.4pp
//	medium            8.0%        8.0%        0
//	all              64.9%       62.5%       -2.4pp
//
// In the high tier: judged 12496 -> 12922, agree 10830 -> 10894, disagree
// 1666 -> 2028. A19 newly resolved 426 references, of which 64 were right and
// 362 were wrong — a 15% hit rate on everything it added.
//
// The mechanism is the part worth keeping. A19 did not only delete wrong
// candidates. By SHRINKING the candidate set it tipped references across the
// disambiguation thresholds — under MaxCandidates, or into a tier that now
// matched uniquely — so references previously recorded as AMBIGUOUS began to
// resolve, mostly to the wrong production candidate. It converted honest
// ambiguity into confident wrong answers, which is precisely what the package
// doc forbids.
//
// The general lesson, which is not obvious and cost two guards to learn: a
// filter that removes candidates is not automatically safe. Removing candidates
// also removes the AMBIGUITY that was protecting the caller. Every exclusion
// rule must therefore be measured on end-to-end precision, not justified by the
// soundness of the thing it excludes.
//
// A3 — "give Go a receiver type environment". Not a filter, and reverted
// anyway, which is why it is recorded here rather than filed as unrelated: the
// two rules above are about deleting candidates, and this is the third way to
// lose precision, by changing which TIER decides.
//
// internal/parse gained a varTypeQuery for Go built from declarations and
// composite literals — `svc *Service`, `var q *Queue`, `c := &Client{}` — so a
// receiver arrived as its type instead of as `svc`, `q`, `c`. The parser half
// worked exactly as designed. Measured on RepoCortex's own source, 419 Go files,
// against a 17 MB scip-go 0.2.7 index, one frozen tree and one index across all
// arms:
//
//	                          high    high    medium   all
//	                          prec    recall  prec     recall
//	baseline                  98.6%   60.4%   35.4%    60.6%
//	+ Go type environment     98.3%   60.2%   50.0%    60.5%
//	+ ReceiverDenotesType     98.3%   60.6%   54.5%    61.0%
//
// `medium` improved from 35.4% to 54.5% and all-tiers recall gained 0.4pp. The
// price was `high`: 18 correct edges gained and 14 wrong ones, a 56% hit rate
// on everything added, into a tier measuring 98.6%. A tier is only worth its
// label while what it admits meets that label, so both arms were reverted.
//
// The MECHANISM is the reusable part. Tier 2 (receiver) sits ABOVE tiers 3 and
// 4 (import, same directory). In Go the directory IS the package, so proximity
// is strong evidence — and while Go receivers arrived as `s` and `c`, tier 2
// almost never fired and proximity effectively decided. Supplying real type
// names woke tier 2 up, and a file-stem coincidence (`Store` matching store.go)
// began out-ranking the in-package definition that was correct.
//
// That is the SAME result the import-vs-proximity ordering already records,
// reached from a new direction: in a directory-is-package language, INFERRED
// type evidence must not outrank package proximity. So A3 is not "Go type
// inference does not work" — it is "the tier order was tuned for a receiver
// that carried no type". Retrying it means moving tier 2 below pickSameDir
// where PackageIsDirectory holds, and measuring THAT.
//
// A3 also disproved a safety property this package had been assuming. The
// argument for inferring types at all is that a mis-inference is inert, because
// receiverMatches validates rather than contains (A16). That holds for `high`
// and not in general: a receiver matching two candidates by file stem takes
// tier 2's overload branch and emits a deterministic preference at `medium`.
// TestMisInferredReceiverYieldsNoEdge pins both halves.
//
// ResolveFile binds every reference in one file.
//
// Returns the edges to write and the per-reference outcomes to record. Both are
// returned rather than written, so the caller owns transaction boundaries and
// this package stays pure and testable.
//
// Outcomes come back as a slice in reference order, not a map. The order is not
// cosmetic: the registry applies them to the reference table in reference-id
// order, and a map handed it a fresh random permutation on every batch. See
// registry.MarkResolutions.
func (r *Resolver) ResolveFile(
	ctx context.Context,
	fr registry.FileRefs,
	stats *Stats,
) ([]model.Edge, []registry.RefOutcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	// Select this file's language strategy once. Every per-language question
	// below reads a field of it rather than re-switching on the name, which is
	// what stops the three switches this replaced from drifting apart again.
	spec := r.specs.For(fr.Language)

	// Index this file's imports once; every reference in the file consults them.
	imports := buildImportSet(spec, fr.Imports)

	// Ask for this file's names in ONE request, before resolving any of them.
	// Per-reference lookups would be a query per reference, which is the design
	// the preloaded index existed to avoid; per-file keeps that property while
	// bounding what is resident to the names one file mentions.
	symbols, err := r.candidates.Candidates(ctx, distinctNames(fr.Refs))
	if err != nil {
		return nil, nil, fmt.Errorf("candidates for %s: %w", fr.Path, err)
	}

	edges := make([]model.Edge, 0, len(fr.Refs))
	outcomes := make([]registry.RefOutcome, 0, len(fr.Refs))

	for _, ref := range fr.Refs {
		stats.References++

		// Every indexed symbol of this name is a candidate, and the tiers below
		// weigh all of them. Nothing is excluded up here — see "Why candidates
		// are not filtered before the tiers" above for the two guards that
		// tried, and what they cost.
		cands := symbols[ref.Name]

		// The ONE exclusion made before the tiers, and it is not a heuristic
		// about likelihood — it is a fact about module systems. A Vue component
		// cannot call a Python function: the two share no mechanism by which
		// one could resolve the other, however well the names match.
		//
		// A18 and A19 both filtered here and both were reverted, so the bar is
		// high. The difference is falsifiability: those guards guessed that a
		// candidate was UNLIKELY (a bare name is not a method; production does
		// not call into tests) and were wrong about the parser and about
		// ambiguity respectively. This one drops candidates that are
		// IMPOSSIBLE, and it fails open on any pair it cannot classify.
		//
		// Measured cause: `readiness.exceptions(...)` in a .vue file bound to a
		// Python function named `exceptions` in another service at `high`
		// confidence, and surfaced as an architecture violation reading "the
		// warehouse UI reaches into inventory-service". 62 python->go edges at
		// `high` shared the cause.
		if len(cands) > 0 {
			kept := cands[:0:0]
			for _, c := range cands {
				if spec.bindableAcross(c.FilePath) {
					kept = append(kept, c)
				}
			}
			if len(kept) != len(cands) {
				stats.CrossLanguageDropped += int64(len(cands) - len(kept))
			}
			cands = kept
		}

		switch {
		case len(cands) == 0:
			// The name resolves to nothing indexed. Usually a standard-library
			// or third-party call — real, but outside this repository. Recording
			// it as unresolved keeps the gap visible instead of pretending the
			// call site does not exist.
			stats.Unresolved++
			outcomes = append(outcomes, registry.RefOutcome{
				RefID: ref.ID, Resolution: registry.ResolutionUnresolved})
			continue

		case len(cands) > spec.MaxCandidates:
			// Too many to weigh — UNLESS the receiver settles it.
			//
			// This cap used to run before pick(), so an over-subscribed name
			// was abandoned without the receiver tier ever seeing it. On a
			// 2,681-file Java service that discarded 643 references to
			// `build`, which has 250 definitions and is almost always called
			// as `someBuilder.build()` — the one piece of evidence that
			// identifies it exactly.
			//
			// Narrowing first is not a weaker test than the cap, it is a
			// stronger one: a receiver that leaves a single candidate has
			// constrained the type as well as the name, which is precisely
			// what the cap exists to admit it cannot do on names alone.
			// A8 — count the drop before attempting the rescue, so the two
			// counters bracket what narrowByReceiver saved.
			stats.CapExceeded++
			narrowed := narrowByReceiver(spec, ref, cands)
			if len(narrowed) == 0 || len(narrowed) > spec.MaxCandidates {
				stats.CapDropped++
				// A21 — the receiver failed to name an owner among hundreds of
				// candidates, which is exactly what an inherited method looks
				// like from here. Park it; the outcome recorded now stands
				// unless ResolveDeferred later finds the definition on an
				// ancestor. See inherited.go.
				r.deferInherited(spec, fr, ref, stats)
				stats.Ambiguous++
				outcomes = append(outcomes, registry.RefOutcome{
					RefID: ref.ID, Resolution: registry.ResolutionAmbiguous})
				continue
			}
			cands = narrowed
		}

		best, ok := r.pick(spec, fr, imports, ref, cands)
		if !ok || !best.confidence.AtLeast(r.opts.MinConfidence) {
			// A21 — nothing here could bind it. If it has a receiver, the
			// ancestry chain may name the type that owns it; park it for the
			// second pass. Bound references are never parked, so this cannot
			// change an answer pass one already gave.
			r.deferInherited(spec, fr, ref, stats)
			stats.Ambiguous++
			outcomes = append(outcomes, registry.RefOutcome{
				RefID: ref.ID, Resolution: registry.ResolutionAmbiguous})
			continue
		}

		stats.Resolved++
		if stats.ByConfidence == nil {
			stats.ByConfidence = map[model.Confidence]int64{}
		}
		stats.ByConfidence[best.confidence]++

		outcomes = append(outcomes, registry.RefOutcome{
			RefID:      ref.ID,
			SymbolID:   best.entry.ID,
			Resolution: registry.ResolutionResolved,
		})

		// Attribute the call site to the symbol containing it. A reference
		// outside any indexed symbol (a package-level initialiser, a
		// top-level statement) yields no edge: the resolution is still
		// recorded, but there is no meaningful source to hang an edge on and
		// inventing one would corrupt caller queries.
		src, ok := enclosing(fr.Symbols, ref.Line)
		if !ok {
			continue
		}

		edges = append(edges, model.Edge{
			SrcID:      src,
			DstID:      best.entry.ID,
			Kind:       edgeKindFor(ref.Kind),
			Confidence: best.confidence,
			Evidence: model.Evidence{
				FilePath: fr.Path,
				Range: model.Range{
					Start: model.Position{Line: ref.Line, Col: ref.Col},
					End:   model.Position{Line: ref.Line, Col: ref.Col},
				},
				Source: model.SourceSourceCode,
				Detail: best.reason,
			},
		})
		stats.Edges++
	}

	return edges, outcomes, nil
}

// pick chooses the best candidate and the confidence that choice earns.
//
// The ordering of these rules IS the confidence model. Each tier exists because
// it rules out a specific way of being wrong.
func (r *Resolver) pick(
	spec LanguageSpec,
	fr registry.FileRefs,
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	// Tier 1 — same file. Nothing beats a definition in the very file that
	// references it: no import resolution, no visibility question, no shadowing
	// from another package.
	var sameFile []registry.SymbolIndexEntry
	for _, c := range cands {
		if c.FileID == fr.FileID {
			sameFile = append(sameFile, c)
		}
	}
	if len(sameFile) == 1 {
		return candidate{sameFile[0], model.ConfidenceHigh, "defined in the same file"}, true
	}

	// Tier 2 — receiver type matches the symbol's owner. `svc.updateStatus()`
	// where a candidate is OWNED by a type the receiver plausibly names is far
	// stronger evidence than a bare name match, because it constrains both the
	// name AND the type.
	//
	// Failing to confirm an owner is NOT evidence against a candidate — see
	// receiverMatches — so an unconfirmed receiver falls through to the tiers
	// below rather than excluding anything.
	if ref.Receiver != "" {
		// A5 — before treating the receiver as a TYPE, check whether it is the
		// local name of an import. `pay.Process()` where the file wrote
		// `import pay "github.com/satyendrakumarsingh/repocortex/internal/payments"` is not type inference at
		// all: the file DECLARED where that name comes from, which is the
		// strongest evidence available short of a compiler. The alias was
		// parsed, stored and then discarded here.
		//
		// Inside tier 2 rather than above it, so the tier order the rest of
		// this function documents is untouched.
		if c, ok := r.pickAliased(imports, ref, cands); ok {
			return c, true
		}

		var byReceiver []registry.SymbolIndexEntry
		for _, c := range cands {
			if receiverMatches(spec, ref.Receiver, c) {
				byReceiver = append(byReceiver, c)
			}
		}
		if len(byReceiver) == 1 {
			return candidate{byReceiver[0], model.ConfidenceHigh, "receiver type matches"}, true
		}
		if len(byReceiver) > 1 {
			// The receiver narrowed it but did not settle it — typically an
			// overload set. Prefer deterministically so repeated runs agree,
			// and be honest that it was a preference, not a deduction.
			sortCandidates(byReceiver)
			if c, ok := preferByArity(ref, byReceiver); ok {
				return candidate{c, model.ConfidenceMedium,
					"receiver matches, overloaded; argument count selects one"}, true
			}
			return candidate{byReceiver[0], model.ConfidenceMedium, "receiver matches, overloaded"}, true
		}
	}

	// Tiers 3 and 4 — an explicit import, and the same directory.
	//
	// Their ORDER is language-dependent, and getting it wrong is the single
	// largest source of wrong high-confidence edges. Both orders were measured
	// against compiler ground truth:
	//
	//   - Where a directory IS the package (Go, Java, Kotlin), an unqualified
	//     name resolves IN the package. `Foo()` means this package's Foo; the
	//     imported one would have to be written `pkg.Foo()`. Same directory
	//     must win. Putting imports first cost prometheus 0.8 points of
	//     high-confidence precision.
	//
	//   - Where each file is its own module (Python, TypeScript, JavaScript),
	//     adjacency means nothing and the import is a DECLARATION that this
	//     file depends on that module. Imports must win. Putting proximity
	//     first cost scrapy 4.7 points: `tests/test_addons.py` imports
	//     `get_crawler` from `scrapy.utils.test`, defines nothing of that name,
	//     and was bound to an unrelated helper in a neighbouring test file —
	//     144 edges from that one name, and roughly 45% of every
	//     high-confidence disagreement in the repository.
	//
	// Inferred evidence must not outrank a declaration, except where the
	// language makes proximity a declaration in itself.
	if spec.PackageIsDirectory {
		if c, ok := r.pickSameDir(spec, fr, cands); ok {
			return c, true
		}
		if c, ok := r.pickImported(imports, ref, cands); ok {
			return c, true
		}
	} else {
		if c, ok := r.pickImported(imports, ref, cands); ok {
			return c, true
		}
		if c, ok := r.pickSameDir(spec, fr, cands); ok {
			return c, true
		}
	}

	// Tier 5 — a unique name anywhere in the repository. Weak, but a name that
	// occurs exactly once is meaningfully different from one that occurs eight
	// times, and callers can threshold on it.
	if len(cands) == 1 {
		if !cands[0].Exported {
			// An unexported symbol in another file is probably not the target;
			// the reference more likely points outside the repository.
			return candidate{}, false
		}
		// A20 — uniqueness is only evidence for a name that could plausibly be
		// unique. A repository containing exactly one `get` says nothing about
		// what a given `get(...)` meant, because the competing definitions are
		// in dependencies that were never indexed: the uniqueness is a property
		// of the index's boundary, not of the program. See genericNames.
		//
		// Refusing HERE and nowhere else is what keeps this outside the class
		// of change A19 belongs to. Every stronger tier has already declined,
		// so no candidate set is being shrunk and no other reference's
		// disambiguation can move.
		if r.specs.IsGenericName(ref.Name) {
			return candidate{}, false
		}
		return candidate{cands[0], model.ConfidenceLow, "unique name in repository, no import evidence"}, true
	}

	// Several candidates and nothing to choose between them. Guessing here is
	// exactly the behaviour this package exists to avoid.
	return candidate{}, false
}

// preferByArity picks the one candidate whose parameter count matches the call
// site's argument count.
//
// # A4 — this is a RANKING rule, and the distinction is the whole design
//
// When several candidates share a name, how many arguments the call passes is
// evidence about which one was meant. `Save(id)` and `Save(ctx, id, opts)` are
// different functions and a call site names one of them.
//
// What this must never become is a filter. Two guards that removed candidates
// were measured and reverted — see "Why candidates are not filtered before the
// tiers" above — and A19 recorded the mechanism precisely: shrinking a
// candidate set does not merely delete wrong candidates, it tips references
// across the disambiguation thresholds, so references that were honestly
// AMBIGUOUS start resolving, mostly wrongly. An arity rule written as "drop the
// candidates whose parameter count differs" would be that same change with a
// new justification.
//
// So this is called from exactly three places, and every one of them is a point
// where pick() has ALREADY decided which candidate wins and is about to report
// ConfidenceMedium with the reason "several matched, preferred
// deterministically". All this does is change WHICH of those several is
// preferred. It cannot:
//
//   - remove a candidate, so no reference's ambiguity is disturbed;
//   - resolve a reference that would otherwise have been ambiguous;
//   - reach a ConfidenceHigh branch, all of which return before this runs.
//
// That last property is what makes the change safe to attempt at all. `high` is
// the tier the product tells a consumer to act on and it measures 97.8% on this
// repository; A3 was reverted for moving it by 0.3pp. A rule confined to the
// medium branches cannot move `high` by construction, not merely by measurement
// — the edges it reorders were already going to be emitted, at the same tier,
// pointing at some member of the same set.
//
// # Why unknown is not a mismatch
//
// A candidate or a call site with no determined arity is skipped, never
// counted against. model.Arity exists so "the parser did not work out a count"
// and "the count is zero" are different values, because the languages where
// they would be confused are exactly the languages where an arity mismatch is a
// parser artefact rather than evidence: a Python method's implicit `self`, a
// TypeScript optional parameter, a default argument, a variadic signature.
// internal/parse reports ArityUnknown for all of those, so they never reach
// here — and a language whose parser has no arity query at all sees this
// function do nothing, which is why nothing gates it on the language.
//
// # Why EXACTLY one
//
// A preference is only worth expressing when it points somewhere. Two
// candidates of the same arity are as ambiguous as they were, and zero matches
// means the call disagrees with every candidate — which is far more likely to
// mean the parser miscounted, or the real target is not indexed, than that one
// of these is right. Both cases fall back to the existing deterministic order
// rather than inventing a tie-break.
//
// # Measured
//
// On RepoCortex's own source, 431 Go files, adjudicated against an 18 MB scip-go
// 0.2.7 index. One FROZEN tree and one index in both arms, only the binary
// differing:
//
//	tier     baseline                      with A4
//	high     97.8%  5,747 judged  129 wrong  97.8%  5,747 judged  129 wrong
//	medium   35.4%     48 judged   31 wrong  45.8%     48 judged   26 wrong
//	low       0.3%    309 judged  308 wrong   0.3%    309 judged  308 wrong
//	all      92.3%  6,104 judged  468 wrong  92.4%  6,104 judged  463 wrong
//
// `high` is identical in all three numbers, not merely close — which is what
// the design predicts, since no ConfidenceHigh branch reaches this function.
// Recall is unchanged at every tier (`high` 60.3%, all 60.5%), as are the miss
// counts (2,891 unresolved, 407 ambiguous): the same call sites resolved, at
// the same tiers, and five of them changed which definition they name. All five
// changed from wrong to right.
//
// The honest caveat is the sample. `medium` is 48 judged edges out of 13,394
// heuristic ones on this corpus, so 10.4pp is five edges. What justifies
// shipping is not the size of that number but the shape of the change: it
// cannot subtract, and every edge it moved it moved in the right direction.
func preferByArity(ref registry.Reference, cands []registry.SymbolIndexEntry) (registry.SymbolIndexEntry, bool) {
	if !ref.ArgCount.Known() {
		return registry.SymbolIndexEntry{}, false
	}
	var match registry.SymbolIndexEntry
	found := 0
	for _, c := range cands {
		if !c.ParamCount.Known() || c.ParamCount.Count() != ref.ArgCount.Count() {
			continue
		}
		found++
		if found > 1 {
			return registry.SymbolIndexEntry{}, false
		}
		match = c
	}
	return match, found == 1
}

// importSet answers "does this file live in something the referencing file
// imported". It carries the referencing file's language because what an import
// path NAMES differs by language — a directory in Go and Java, usually a file
// in Python, TypeScript and JavaScript.
type importSet struct {
	// spec is the REFERENCING file's language strategy. What an import path
	// names differs by language — a directory in Go and Java, usually a file in
	// Python, TypeScript and JavaScript — and matching the wrong one either
	// misses real imports or invents coincidences. The field that decides it is
	// LanguageSpec.ImportPathNamesFile.
	spec LanguageSpec

	// imports is every declared import, in the order the file wrote them, with
	// its path pre-split into lowercased segments. A SLICE, scanned in order,
	// because resolution must not depend on map iteration.
	imports []importEntry

	// byLocalName indexes the imports that declared an explicit local name, for
	// lookup only — never iterated.
	byLocalName map[string]int
}

// importEntry is one declared import, prepared for matching.
type importEntry struct {
	alias string
	// segments is the path split on both '/' and '.', lowercased. Import paths
	// and file paths are written in different alphabets — "com.pay.repository"
	// vs "src/main/java/com/pay/repository/X.java", "github.com/pay/repo" vs
	// "repo/x.go" — and splitting both into segments is what makes them
	// comparable at all.
	segments []string
}

func buildImportSet(spec LanguageSpec, imports []registry.Import) importSet {
	s := importSet{
		spec:    spec,
		imports: make([]importEntry, 0, len(imports)),
	}
	for _, imp := range imports {
		p := strings.Trim(imp.Path, "./")
		if p == "" {
			continue
		}
		e := importEntry{alias: imp.Alias, segments: pathSegments(p)}
		if len(e.segments) == 0 {
			continue
		}
		if e.alias != "" {
			if s.byLocalName == nil {
				s.byLocalName = make(map[string]int, len(imports))
			}
			// First declaration wins on a repeated local name; a file that
			// binds one name twice is malformed, and picking deterministically
			// beats picking by scan order downstream.
			if _, dup := s.byLocalName[strings.ToLower(e.alias)]; !dup {
				s.byLocalName[strings.ToLower(e.alias)] = len(s.imports)
			}
		}
		s.imports = append(s.imports, e)
	}
	return s
}

// pathSegments splits an import or file path on both separators that appear in
// one, lowercased, dropping empties.
func pathSegments(p string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '.'
	}) {
		if part == "" || part == "." {
			continue
		}
		out = append(out, strings.ToLower(part))
	}
	return out
}

// fileSegments renders a candidate's path as the segments an import could name.
//
// In JavaScript, TypeScript and Python a module usually IS a file:
// `import { X } from './sibling'` resolves to sibling.ts, and
// `from .helpers import X` to helpers.py, so the file's own stem is the last
// segment. In Go and Java an import path's last segment is a DIRECTORY and a
// file's own name means nothing to it — matching on it there only invents
// coincidences, and measured, enabling it for Go cost prometheus 0.4 points.
//
// Which of the two applies is LanguageSpec.ImportPathNamesFile.
func fileSegments(spec LanguageSpec, filePath string) []string {
	dir := pathSegments(path.Dir(filePath))
	if !spec.ImportPathNamesFile {
		return dir
	}
	base := fileBaseName(filePath)
	if base == "" {
		return dir
	}
	return append(dir, strings.ToLower(base))
}

// covers reports how well a file matches something the referencing file
// imported, as the number of TRAILING segments the two share.
//
// # A6 — why this is anchored at the end
//
// The rule used to be: any lowercased segment of the file's path equal to any
// segment of any import path. That is not a containment test on packages, it is
// a bag-of-words match, and `internal` or `src` or `com` is a segment of nearly
// every import in a repository. `internal/config/x.go` therefore "lived in" an
// import of `repocortex/internal/anything`, and because pickImported reports
// ConfidenceHigh whenever the match is UNIQUE, one such coincidence was enough
// to publish a high-confidence edge to a file the reference never imported.
//
// Matching TRAILING segments instead is what an import path actually asserts.
// `repocortex/internal/config` names the package whose directory ENDS with
// internal/config; it says nothing about a file that merely happens to sit
// under some other `internal`. Anchoring costs nothing real, because the
// alphabets still differ in their prefixes — an import of `com.pay.repository`
// still matches `src/main/java/com/pay/repository/X.java` on three segments.
//
// The DEPTH is returned rather than a bool so the caller can prefer the longest
// suffix. Two imports can both cover a candidate, and the one that shares more
// of the path is the one that named it.
func (s importSet) covers(filePath string) (int, bool) {
	if len(s.imports) == 0 {
		return 0, false
	}
	segs := fileSegments(s.spec, filePath)
	if len(segs) == 0 {
		return 0, false
	}
	best := 0
	for i := range s.imports {
		if d := commonSuffixLen(s.imports[i].segments, segs); d > best {
			best = d
		}
	}
	return best, best > 0
}

// coversFrom is covers restricted to ONE declared import, for the case where
// the reference named that import outright.
func (s importSet) coversFrom(e importEntry, filePath string) bool {
	segs := fileSegments(s.spec, filePath)
	return len(segs) > 0 && commonSuffixLen(e.segments, segs) > 0
}

// commonSuffixLen counts the trailing segments two paths agree on.
func commonSuffixLen(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) {
		if a[len(a)-1-n] != b[len(b)-1-n] {
			break
		}
		n++
	}
	return n
}

// pickAliased resolves a reference whose receiver is an import's local name.
//
// A5 — registry.Import.Alias is populated by the parser (see the @alias
// captures in internal/parse/queries.go, which fire only where the syntax
// declares a local name: Go's `import pay "..."`, Python's `import x as y`) and
// was read by nothing. It is the one piece of evidence in this package that is
// DECLARED rather than inferred at the level of an individual reference: the
// file states that this name denotes that package, so a reference qualified by
// it is package-qualified and the candidate set collapses to whatever lives
// there.
//
// Restricted to EXPLICIT aliases. An unaliased import also binds a local name
// in Go — `registry.New()` after `import "github.com/satyendrakumarsingh/repocortex/internal/registry"` — but
// that name is the package clause, not the last path segment, and the two
// differ often enough (package `pay` in directory `payments`) that inferring it
// would be a guess wearing a declaration's confidence. Reading it properly
// needs the package name in the symbol index; until then this handles the case
// the parser can actually prove.
//
// A receiver that names an import but matches no candidate falls through
// rather than excluding anything. The overwhelmingly common reason is that the
// import is third-party and simply is not in the index, which is absence of
// evidence — the distinction A18 got wrong.
func (r *Resolver) pickAliased(
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	if len(imports.byLocalName) == 0 {
		return candidate{}, false
	}
	i, ok := imports.byLocalName[strings.ToLower(strings.TrimPrefix(ref.Receiver, "*"))]
	if !ok {
		return candidate{}, false
	}
	entry := imports.imports[i]

	var inPackage []registry.SymbolIndexEntry
	for _, c := range cands {
		if imports.coversFrom(entry, c.FilePath) {
			inPackage = append(inPackage, c)
		}
	}
	switch len(inPackage) {
	case 0:
		return candidate{}, false
	case 1:
		return candidate{inPackage[0], model.ConfidenceHigh,
			"receiver is the local name of an import declaring this package"}, true
	default:
		// The package is right and the member is overloaded within it. Prefer
		// deterministically and say it was a preference.
		sortCandidates(inPackage)
		if c, ok := preferByArity(ref, inPackage); ok {
			return candidate{c, model.ConfidenceMedium,
				"receiver is an import's local name; argument count selects one in that package"}, true
		}
		return candidate{inPackage[0], model.ConfidenceMedium,
			"receiver is an import's local name, several matches in that package"}, true
	}
}

// pickImported chooses among candidates the file explicitly imports.
//
// An import is DECLARED evidence: the file said it depends on that module.
//
// Where several candidates are covered, the one matching the LONGEST run of
// trailing segments wins: an import of `repocortex/internal/pay/store` covers
// both `internal/pay/store/x.go` (three segments) and, in a repository that
// also imports `.../store`, `internal/store/x.go` (one), and the deeper match
// is the one that import path actually named. Only a tie among equally deep
// matches is the honest "several, preferred deterministically" case.
func (r *Resolver) pickImported(
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	best := 0
	for _, c := range cands {
		if d, ok := imports.covers(c.FilePath); ok && d > best {
			best = d
		}
	}
	if best == 0 {
		return candidate{}, false
	}
	var imported []registry.SymbolIndexEntry
	for _, c := range cands {
		if d, ok := imports.covers(c.FilePath); ok && d == best {
			imported = append(imported, c)
		}
	}
	if len(imported) == 1 {
		return candidate{imported[0], model.ConfidenceHigh, "single match in an imported package"}, true
	}
	sortCandidates(imported)
	if c, ok := preferByArity(ref, imported); ok {
		return candidate{c, model.ConfidenceMedium,
			"multiple matches among imports; argument count selects one"}, true
	}
	return candidate{imported[0], model.ConfidenceMedium, "multiple matches among imports"}, true
}

// pickSameDir chooses among candidates beside the referencing file.
//
// Confidence depends on whether the language makes that visibility real: in Go
// and Java the directory is the package, so a neighbour is in scope. Elsewhere
// a name that occurs exactly once next door is still better evidence than one
// occurring nowhere, but it is a preference and is labelled as one.
func (r *Resolver) pickSameDir(
	spec LanguageSpec,
	fr registry.FileRefs,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	dir := path.Dir(fr.Path)
	var sameDir []registry.SymbolIndexEntry
	for _, c := range cands {
		if path.Dir(c.FilePath) == dir {
			sameDir = append(sameDir, c)
		}
	}
	if len(sameDir) != 1 {
		return candidate{}, false
	}
	if spec.PackageIsDirectory {
		return candidate{sameDir[0], model.ConfidenceHigh, "same package directory"}, true
	}
	return candidate{sameDir[0], model.ConfidenceMedium, "same directory, but the language requires an import"}, true
}

// narrowByReceiver keeps only the candidates the receiver could name.
//
// Returns nil when there is no receiver, so the caller can tell "not narrowed"
// from "narrowed to nothing" — the second means the receiver contradicted every
// candidate, which is information, and treating it as the first would resolve a
// reference the evidence argues against.
//
// Gated on LanguageSpec.ReceiverDenotesType: where the parser has NOT rewritten
// the receiver to its declared type, this is being asked to rescue an
// over-subscribed name using a variable name, which is a guess. Go receivers are
// `s`, `c`, `r`, and doing it there cost prometheus 1.7 points of
// high-confidence precision while gaining nothing.
func narrowByReceiver(spec LanguageSpec, ref registry.Reference, cands []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	if ref.Receiver == "" || !spec.ReceiverDenotesType {
		return nil
	}
	var out []registry.SymbolIndexEntry
	for _, c := range cands {
		if strictReceiverMatch(spec, ref.Receiver, c) {
			out = append(out, c)
		}
	}
	return out
}

// strictReceiverMatch is the rescue-from-the-cap form of receiverMatches.
//
// The rule that used to live in receiverMatches — the candidate's qualified
// name CONTAINS the receiver — was never safe anywhere, and A16 removed it
// there too. It was least safe HERE, where it decides between hundreds rather
// than the handful pick() sees: a Go receiver named `s` or `c` is a substring
// of nearly every qualified name, so it kept most of them, slipped under the
// cap, and turned an honest "ambiguous" into a guess. Measured: prometheus lost
// 1.7 points of high-confidence precision.
//
// The rule kept is that the candidate lives in a file NAMED for the receiver.
// In Java, where a public class must live in a file of its own name, that is
// the type — and the parser rewrites receivers to their declared type first, so
// `b.build()` arrives here as `Builder`, matching Builder.java exactly.
//
// Deliberately NOT widened to receiverMatches' qualified-name channel. That
// channel is sound, but this path is a rescue from a cap that exists precisely
// because the evidence is thin, and the file-identity rule is the one that was
// measured against ground truth here. Widening it is a change to make with a
// measurement, not in passing.
//
// The length floor is part of that: `s.Save()` naming a file s.go is a
// coincidence at this scale, not evidence. It, and the set of tokens that name
// the enclosing instance rather than a type, are LanguageSpec.MinTypeNameLength
// and LanguageSpec.SelfNames — see LanguageSpec.namesAType.
func strictReceiverMatch(spec LanguageSpec, receiver string, c registry.SymbolIndexEntry) bool {
	if !spec.namesAType(receiver) {
		return false
	}
	r := strings.TrimPrefix(receiver, "*")
	return strings.EqualFold(fileBaseName(c.FilePath), r)
}

// receiverMatches reports whether a receiver expression names the type that
// OWNS a candidate.
//
// # A16 — why this validates rather than contains
//
// The inferred type is cheap: the receiver token itself, compared
// case-insensitively. That covers both the languages where parse has already
// rewritten the receiver to its declared type (`b.build()` arrives as
// `Builder`; see varTypeQueries in internal/parse/queries.go) and the
// convention that names a variable after its type — `transactionService`
// naming a TransactionService.
//
// What is NOT cheap is what is done with it. The old rule accepted any
// candidate whose FQN CONTAINED the receiver, which is not a type test at all:
// a receiver named `db` matched every qualified name with "db" anywhere in it —
// dbutil, feedback, `internal/db/...` — and a single such accident became a
// High-confidence edge to the wrong symbol, since tier 2 reports High whenever
// exactly one candidate survives it. A cheap inference plus a loose test is how
// a mis-inference is laundered into a fact.
//
// So the inference stays cheap and the ACCEPTANCE is exact: the candidate's
// qualified name must actually END with `<InferredType><sep><Name>` for one of
// the language's member separators, with the type occupying a whole segment.
// `com.pay.TransactionService.updateStatus` is owned by TransactionService;
// `com.pay.SubTransactionService.updateStatus` is not, and no longer matches.
// A mis-inferred receiver now yields NO edge instead of a wrong one, which is
// the trade this package exists to make.
//
// The second channel is the same claim through a different piece of evidence:
// a candidate defined in a file NAMED for the receiver. In Java a public class
// must live in a file of its own name, and it is what rescues the qualified
// names RepoCortex synthesises for tree-sitter symbols, which are `path:name`
// (model.DefaultFQN) and carry no owner at all. It is equality, not
// containment.
//
// It still cannot see through interfaces, factories or dependency injection —
// which is why a match yields High and never Exact.
//
// Note what a NON-match means: "this candidate's owner could not be confirmed",
// never "the source says the owner is something else". The distinction is the
// one A18 got wrong (see "Why candidates are not filtered before the tiers"
// above), so callers must treat a failure here as
// absence of evidence and fall through, not as grounds for exclusion.
func receiverMatches(spec LanguageSpec, receiver string, c registry.SymbolIndexEntry) bool {
	if !spec.usableReceiver(receiver) {
		// Empty, or a self-reference that tells us nothing the same-file tier
		// has not. Deliberately WITHOUT MinTypeNameLength: the ownership test
		// below is exact, and adding a floor here would be a new filter — see
		// LanguageSpec.usableReceiver.
		return false
	}
	r := strings.TrimPrefix(receiver, "*")
	if ownsQualifiedName(spec, c, r) {
		return true
	}
	return strings.EqualFold(fileBaseName(c.FilePath), r)
}

// ownsQualifiedName reports whether c.FQN is exactly `<owner><sep><c.Name>`,
// possibly behind further qualification (a package, an outer class).
//
// Three things have to hold, and dropping any one of them re-opens the
// substring hole:
//
//   - the qualified name ENDS with the symbol's own name, so a candidate is
//     never matched on a prefix of some longer member;
//   - immediately before it is one of the language's member separators;
//   - immediately before THAT is the owner as a WHOLE segment — either the
//     qualified name starts there, or the byte in front of it cannot continue
//     an identifier. Without this last test `SubTransactionService.updateStatus`
//     answers to a receiver of `transactionService`, which is the substring bug
//     wearing a suffix check.
//
// The boundary is "not an identifier byte" rather than "one of the separators
// above" because qualified names carry delimiters that are not member
// separators — SCIP writes a descriptor as “src/net.ts`Client#send“, and
// requiring `#` on both sides would reject the very names the `#` was added
// for. What has to be excluded is another identifier running into the owner,
// and that is exactly what this tests.
//
// Compares with EqualFold over slices rather than lowercasing the FQN. This
// runs per candidate per receiver-bearing reference, and the old rule's
// strings.ToLower allocated a copy of every qualified name it looked at.
func ownsQualifiedName(spec LanguageSpec, c registry.SymbolIndexEntry, owner string) bool {
	if c.Name == "" || c.FQN == "" || len(c.FQN) <= len(c.Name)+len(owner) {
		return false
	}
	head, ok := trimFoldSuffix(c.FQN, c.Name)
	if !ok {
		return false
	}
	for _, sep := range spec.MemberSeparators {
		qualifier, ok := trimFoldSuffix(head, sep)
		if !ok {
			continue
		}
		rest, ok := trimFoldSuffix(qualifier, owner)
		if !ok {
			continue
		}
		if rest == "" || !isIdentByte(rest[len(rest)-1]) {
			return true
		}
	}
	return false
}

// trimFoldSuffix removes suffix from s, case-insensitively, reporting whether
// it was there. Allocation-free.
func trimFoldSuffix(s, suffix string) (string, bool) {
	if len(s) < len(suffix) {
		return "", false
	}
	cut := len(s) - len(suffix)
	if !strings.EqualFold(s[cut:], suffix) {
		return "", false
	}
	return s[:cut], true
}

// isIdentByte reports whether a byte can appear inside an identifier, and so
// whether it would make the text before an owner run into the owner itself.
//
// ASCII-only on purpose. A non-ASCII byte has its high bit set and is reported
// as an identifier byte, which is the conservative answer: it REJECTS the
// match rather than accepting one whose segmentation was not understood.
func isIdentByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b >= 0x80:
		return true
	default:
		return false
	}
}

// fileBaseName is a path's file name with its extension removed.
func fileBaseName(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, path.Ext(base))
}

// sortCandidates orders deterministically so repeated resolution of the same
// repository produces the same edges. The registry digest depends on it.
func sortCandidates(cs []registry.SymbolIndexEntry) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].FQN != cs[j].FQN {
			return cs[i].FQN < cs[j].FQN
		}
		return cs[i].ID < cs[j].ID
	})
}

// enclosing finds the innermost symbol whose line span contains line.
//
// Symbols arrive ordered by (start_line ASC, end_line DESC), so a linear scan
// keeping the last match yields the INNERMOST enclosing symbol: a method inside
// a class wins over the class. That matters because "who calls this" should
// name the method, not the file's top-level type.
func enclosing(symbols []registry.EnclosingSymbol, line int) (int64, bool) {
	var best int64
	var found bool
	for _, s := range symbols {
		if s.StartLine > line {
			// Ordered by start line, so nothing further can contain it.
			break
		}
		// end_line of 0 means the extractor gave no span; treat the symbol as
		// covering only its own line rather than the rest of the file, which
		// would attribute every later reference to it.
		end := s.EndLine
		if end == 0 {
			end = s.StartLine
		}
		if line <= end {
			best, found = s.ID, true
		}
	}
	return best, found
}

// edgeKindFor maps a tree-sitter reference kind to a graph edge kind.
func edgeKindFor(refKind string) model.EdgeKind {
	switch {
	case strings.Contains(refKind, "call"):
		return model.EdgeCalls
	case strings.Contains(refKind, "implementation"):
		return model.EdgeImplements
	case strings.Contains(refKind, "extend"), strings.Contains(refKind, "inherit"):
		return model.EdgeExtends
	default:
		return model.EdgeReferences
	}
}

// distinctNames returns the names a file's references mention, deduplicated.
//
// Order is the order of first appearance, not sorted: the caller uses it only
// to build an IN (...) list, and sorting would cost a pass for nothing.
func distinctNames(refs []registry.Reference) []string {
	seen := make(map[string]struct{}, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, dup := seen[ref.Name]; dup {
			continue
		}
		seen[ref.Name] = struct{}{}
		out = append(out, ref.Name)
	}
	return out
}
