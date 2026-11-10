package pipeline

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/infer"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ResolveOptions configures a resolution pass.
type ResolveOptions struct {
	// MinConfidence drops bindings weaker than this. Defaults to low.
	MinConfidence model.Confidence

	// BatchSize bounds how many edges are buffered before writing.
	BatchSize int

	// CandidateBudget bounds how many candidate rows resolution keeps
	// resident. Zero uses registry.DefaultCandidateBudget. This is the knob
	// that decides resolution's memory ceiling; it is exposed so a test can
	// make eviction happen at a size a test can afford.
	CandidateBudget int

	// OnProgress reports files completed, if set.
	OnProgress func(done int64)

	// OnFlush reports the size of each batch as it is written, if set.
	//
	// The buffers are what bound this pass's memory, and they are the part of it
	// a caller cannot otherwise see: the table contents are identical whether
	// resolution flushed a thousand times or once at the end, which is how an
	// unbounded outcome buffer survived every correctness test the package had.
	OnFlush func(edges, outcomes int)

	// OnClientCalls reports the outcome of binding staged outbound HTTP calls,
	// if set.
	//
	// A callback rather than a field on the returned stats because the return
	// type belongs to internal/resolve/infer, which knows about references and
	// nothing else. Widening it to carry an unrelated pass's counters would put
	// API analysis inside the reference resolver.
	OnClientCalls func(registry.CallBindStats)

	// OnInfraUnits reports the outcome of joining deployment units to the code
	// they run, if set.
	//
	// A callback for the same reason OnClientCalls is one: infer.Stats is about
	// references, and widening it to carry a deployment pass's counters would
	// put manifest analysis inside the reference resolver.
	OnInfraUnits func(InfraBindSummary)
}

// DefaultResolveBatch is how many edges accumulate before a write.
const DefaultResolveBatch = 2000

// outcomeBatchFactor bounds the outcome buffer relative to the edge buffer.
//
// Most references produce no edge — they are unresolved calls into the standard
// library or a third-party package — so outcomes accumulate several times
// faster than edges. Four keeps the two buffers filling at roughly the same
// rate on a repository that resolves well, and caps the outcome buffer on one
// that resolves badly, where it used to be the run's entire reference table.
const outcomeBatchFactor = 4

// Resolve binds a run's references to symbol definitions and writes the
// resulting edges.
//
// Memory behaviour is the design constraint. A 10,920-file tree holds ~94,000
// symbols and ~531,000 references; materialising both plus their edges would be
// gigabytes, and this project has already been bitten once by unbounded
// accumulation. So:
//
//   - The symbol index is loaded ONCE (~10 MB per 100k symbols) because every
//     reference consults it, and per-reference SQL would be half a million
//     queries.
//   - References stream file by file and are never all resident.
//   - Edges are flushed in batches rather than collected.
func Resolve(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	opts ResolveOptions,
) (infer.Stats, error) {
	var stats infer.Stats

	batch := opts.BatchSize
	if batch <= 0 {
		batch = DefaultResolveBatch
	}

	// Candidates are read per file rather than preloaded for the whole run.
	//
	// LoadSymbolIndex pulled every symbol into one map before resolving
	// anything, so peak memory tracked repository size: 725,000 entries on a
	// 25,000-file tree, of which 69% had a name no reference in the run ever
	// mentioned. A bounded cache answers the same questions with a working set
	// that does not grow with the repository.
	// Candidate cache reading up to DefaultPerNameLimit (1,000) candidates per name.
	// We cannot cap at MaxCandidates+1 because narrowByReceiver (and inherited dispatch)
	// rescues over-subscribed names (e.g. build() with 250 definitions) by filtering
	// candidates on receiver class/file identity. Truncating candidates to MaxCandidates+1
	// at the SQL level starves narrowByReceiver and drops valid calls as ambiguous.
	cands := registry.NewCandidateCache(reg, runID, opts.CandidateBudget, registry.DefaultPerNameLimit)
	resolver := infer.NewStreaming(cands, infer.Options{
		MinConfidence: opts.MinConfidence,
		// A21 — supply the Ancestry port and the deferred inherited-dispatch
		// pass switches on. Without it the resolver parks nothing and
		// ResolveDeferred is a no-op, which is how this pass sat inert.
		Ancestry: runAncestry{reg: reg, runID: runID},
	})

	var (
		pending  = make([]model.Edge, 0, batch)
		outcomes = make([]registry.RefOutcome, 0, batch*outcomeBatchFactor)
		files    int64
		// written counts edges that reached the table, which is NOT the number
		// the resolver built: two references on different lines can produce the
		// same edge, and INSERT OR IGNORE drops the repeat. Reporting the built
		// count made `resolve` claim more relationships than `status` could
		// find, and two commands disagreeing about one number is worse than
		// either number being slightly off.
		written int
	)

	flush := func() error {
		if len(pending) == 0 && len(outcomes) == 0 {
			return nil
		}
		if opts.OnFlush != nil {
			opts.OnFlush(len(pending), len(outcomes))
		}
		n, err := reg.AddResolvedEdges(ctx, runID, pending)
		if err != nil {
			return fmt.Errorf("write edges: %w", err)
		}
		written += n
		if err := reg.MarkResolutions(ctx, outcomes); err != nil {
			return fmt.Errorf("record outcomes: %w", err)
		}
		pending = pending[:0]
		outcomes = outcomes[:0]
		return nil
	}

	err := reg.EachFilePageWithRefs(ctx, runID, func(page []registry.FileRefs) error {
		// Warm the cache for the whole page before resolving any of it.
		//
		// Resolution asks for candidates per file, which on its own costs one
		// round trip per file — 50,000 of them on a 50,000-file tree, and that
		// showed up as a 31% regression against the preloaded index. Asking
		// once per page amortises the trip across ~500 files while keeping the
		// working set bounded by the page, not by the repository.
		if names := pageNames(page); len(names) > 0 {
			if _, err := cands.Candidates(ctx, names); err != nil {
				return fmt.Errorf("prefetch candidates: %w", err)
			}
		}
		for _, fr := range page {
			if err := resolveOne(ctx, resolver, fr, &stats, &pending, &outcomes, flush, opts, &files); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	if err := flush(); err != nil {
		return stats, err
	}

	// A21 — the second pass for inherited method dispatch, and the ordering
	// here IS the feature.
	//
	// `payment.settle()` where Payment extends BasePayment and only BasePayment
	// declares settle cannot be bound while the loop above is running: the
	// EXTENDS edge that proves Payment IS a BasePayment is itself produced by
	// that loop, from an inheritance reference in some other file, and a tier
	// consulting a hierarchy that is still growing would answer differently
	// depending on which file was resolved first. Those references were parked
	// instead.
	//
	// So this runs at the only honest moment: AFTER the last page (the
	// hierarchy has stopped moving) and AFTER the flush above (the EXTENDS and
	// IMPLEMENTS edges are in the table, which is where runAncestry reads
	// them). Draining before that flush would ask the registry about edges
	// still sitting in the buffer and find none — an inert pass that looked
	// wired up.
	//
	// It is still inside Resolve, and therefore before any caller commits: the
	// two-phase swap must publish a run whose graph includes these edges, not a
	// run that has to be resolved a second time to get them.
	deferredEdges, deferredOutcomes, err := resolver.ResolveDeferred(ctx, &stats)
	if err != nil {
		return stats, fmt.Errorf("resolve inherited dispatch: %w", err)
	}
	// Written in the same bounded batches the main loop uses. The parked queue
	// is capped (infer.MaxDeferred) but that cap is 200,000, so appending its
	// whole yield to one buffer would be the unbounded accumulation the
	// buffers exist to prevent.
	for len(deferredEdges) > 0 || len(deferredOutcomes) > 0 {
		ne := min(len(deferredEdges), batch)
		no := min(len(deferredOutcomes), batch*outcomeBatchFactor)
		pending = append(pending, deferredEdges[:ne]...)
		outcomes = append(outcomes, deferredOutcomes[:no]...)
		deferredEdges, deferredOutcomes = deferredEdges[ne:], deferredOutcomes[no:]
		if err := flush(); err != nil {
			return stats, err
		}
	}

	// The resolver counted edges it BUILT; report edges that landed. Set after
	// the deferred drain so the total covers both passes.
	stats.Edges = int64(written)

	// Client calls bind in the same pass, for exactly the reason references and
	// table accesses do: the endpoint a call targets may be declared by a file
	// parsed long after the call was, so the earliest honest moment to bind is
	// once the whole run has been seen. Doing it here rather than in a command
	// of its own means a user who resolves gets a complete graph instead of one
	// that is complete except for the part nobody remembered to run.
	//
	// A binding failure fails the pass. The rows are staged and idempotent, so
	// the alternative — swallowing the error — would leave CONSUMES silently
	// absent, which is the failure mode this whole feature exists to end.
	calls, err := reg.BindClientCalls(ctx, runID)
	if err != nil {
		return stats, fmt.Errorf("bind client calls: %w", err)
	}
	if opts.OnClientCalls != nil {
		opts.OnClientCalls(calls)
	}

	// Deployment units bind last, and only after everything above.
	//
	// The join is "which endpoints are built from this unit's context", so it
	// needs the endpoints to exist — they are written during indexing, so they
	// do — and it needs the whole run to have been seen, because a build
	// context is a directory and a directory is only complete once the walk is.
	// Running it here rather than in a command of its own is the same argument
	// the two passes above make: a user who resolves gets a graph that includes
	// what their code runs as, instead of one that is complete except for the
	// part nobody remembered to run.
	//
	// A failure fails the pass. The rows are staged and the pass is idempotent,
	// so swallowing the error would leave a repository silently unable to
	// answer "which service does this code run in" — which is the single
	// question this whole feature exists for.
	unitsBound, err := bindInfra(ctx, reg, runID)
	if err != nil {
		return stats, err
	}
	if opts.OnInfraUnits != nil {
		opts.OnInfraUnits(unitsBound)
	}

	if err := reg.UnifyEventSymbols(ctx, runID); err != nil {
		return stats, fmt.Errorf("unify event symbols: %w", err)
	}
	return stats, nil
}

// runAncestry binds the registry's supertype read to one run, which is what
// makes it satisfy infer.Ancestry.
//
// The adapter lives here rather than in the registry because the run id is
// wiring, not a fact about the query: registry.Supertypes takes the run
// explicitly like every other read in that file, and the port takes only the
// names. Two lines here are cheaper than a second run-scoped type in the
// adapter, and they keep the dependency pointing the way CLAUDE.md §1 requires
// — the consumer declares the interface, the pipeline supplies something that
// fits it, and internal/resolve/infer never learns that a registry exists.
type runAncestry struct {
	reg   *registry.Registry
	runID int64
}

// Supertypes answers the deferred pass's one question, run-scoped.
func (a runAncestry) Supertypes(ctx context.Context, types []string) (map[string][]string, error) {
	return a.reg.Supertypes(ctx, a.runID, types)
}

// Compile-time proof the adapter still satisfies the port. Cheap, and it fails
// at build time rather than as a silently inert pass — which is exactly how
// this pass spent its first life.
var _ infer.Ancestry = runAncestry{}

// pageNames collects the distinct names a page of files references.
func pageNames(page []registry.FileRefs) []string {
	seen := make(map[string]struct{}, 512)
	var out []string
	for _, fr := range page {
		for _, ref := range fr.Refs {
			if _, dup := seen[ref.Name]; dup {
				continue
			}
			seen[ref.Name] = struct{}{}
			out = append(out, ref.Name)
		}
	}
	return out
}

// resolveOne binds one file and buffers what it produced.
func resolveOne(
	ctx context.Context,
	resolver *infer.Resolver,
	fr registry.FileRefs,
	stats *infer.Stats,
	pending *[]model.Edge,
	outcomes *[]registry.RefOutcome,
	flush func() error,
	opts ResolveOptions,
	files *int64,
) error {
	{
		edges, outs, err := resolver.ResolveFile(ctx, fr, stats)
		if err != nil {
			return err
		}

		*pending = append(*pending, edges...)
		*outcomes = append(*outcomes, outs...)

		*files++
		if opts.OnProgress != nil {
			opts.OnProgress(*files)
		}

		// Either buffer full is enough. Keying the flush on edges alone let the
		// outcome buffer grow without bound on a repository that resolves
		// poorly: a tree whose references are nearly all third-party produces
		// almost no edges, so the edge buffer never filled and every one of the
		// run's outcomes stayed resident until the final flush.
		if len(*pending) >= resolveBatchOf(opts) || len(*outcomes) >= resolveBatchOf(opts)*outcomeBatchFactor {
			return flush()
		}
		return nil
	}
}

// resolveBatchOf is the effective edge-buffer bound for these options.
func resolveBatchOf(opts ResolveOptions) int {
	if opts.BatchSize > 0 {
		return opts.BatchSize
	}
	return DefaultResolveBatch
}
