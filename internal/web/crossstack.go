package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The cross-stack view: a frontend component, the route it calls, the handler
// that answers it, the code that handler runs, and the tables that code touches
// — as one chain, with every hop carrying its own evidence and its own tier.
//
// # Why nobody else draws this
//
// Every hop already exists as a recorded edge, and each was produced by a
// different, conservative pass:
//
//	component --CONSUMES--> endpoint     internal/registry/clientcalls.go, which
//	                                     refuses to bind when the evidence admits
//	                                     more than one route — an unbound call is
//	                                     a visible gap, a wrong edge is a lie.
//	endpoint  --EXPOSES-->  handler      the route table, as the framework declared it.
//	handler   --CALLS*-->   service      internal/graph, bounded and confidence-aware.
//	service   --READS/WRITES--> table    internal/registry/dataaccess.go.
//
// A graph asserted by a language model can draw a line from a React component
// to a Postgres table too. What it cannot do is tell you that the second hop
// was proven by a route table and the third was a 5.7–37.5%-precision guess, or
// that eleven other components call this route and were left out because their
// call target is assembled at run time. That is the entire difference, and this
// screen is built so the difference is the first thing on it.
//
// # The chain is only as strong as its weakest hop
//
// Weakest is reported per chain and is what the line is drawn at, for the same
// reason an aggregated container edge reports its bundle's floor. A chain whose
// CONSUMES hop is `medium` is a MEDIUM chain however proven its other three
// hops are, because the claim "this component reaches that table" fails exactly
// where the weakest link fails.
//
// # No traversal is invented here
//
// The three edge hops are read off the cached structural index — a WRITES edge
// from a handler to a table IS the fact. The handler-to-service hop is
// internal/graph's bounded, confidence-aware Traverse, used and not
// reimplemented: a second breadth-first search in this package would be a
// second place for "reports depth-3 reachability as high confidence" to come
// back.

// StackStage names one hop's position in the chain, so a client can render the
// four hops distinctly without pattern-matching on edge kinds.
type StackStage string

const (
	StageClient   StackStage = "client"   // CONSUMES: the caller of the route
	StageEndpoint StackStage = "endpoint" // the route itself
	StageHandler  StackStage = "handler"  // EXPOSES: what answers the route
	StageService  StackStage = "service"  // CALLS: what the handler runs
	StageTable    StackStage = "table"    // READS / WRITES: what that touches
)

// StackNode is one symbol on a chain.
type StackNode struct {
	SymbolID  int64  `json:"symbol_id"`
	Name      string `json:"name"`
	FQN       string `json:"fqn,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Path      string `json:"file_path,omitempty"`
	Language  string `json:"language,omitempty"`
	Container string `json:"container,omitempty"`
}

// StackHop is one node plus the relationship that reached it.
type StackHop struct {
	Stage StackStage `json:"stage"`
	Node  StackNode  `json:"node"`

	// Relation is the edge kind that produced this hop: CONSUMES, EXPOSES,
	// CALLS, READS or WRITES. Stated rather than implied by Stage, because a
	// table reached by a WRITES edge and one reached by a READS edge are
	// opposite consequences of the same change.
	Relation string `json:"relation"`

	// Confidence is the tier of THIS hop, not of the chain.
	Confidence string `json:"confidence"`

	// Depth is hops from the handler, for service nodes. 1 is a direct callee.
	Depth int `json:"depth,omitempty"`

	// Evidence is the file:line that proves this hop. Empty for a service hop
	// reached at depth > 1, which has no single location that justifies it —
	// and empty is the honest answer there rather than the last edge's line
	// dressed up as proof of the whole path.
	Evidence Evidence `json:"evidence,omitempty"`
}

// StackGap is a place a chain stops, and why.
//
// The most important type on this screen. A chain that ends at the handler
// because no CALLS edge left it looks identical, in a picture, to a handler
// that genuinely touches no data — and those are opposite facts. Every stop is
// therefore named, staged and counted.
type StackGap struct {
	Stage  StackStage `json:"stage"`
	Reason string     `json:"reason"`
	Detail string     `json:"detail"`
	Remedy string     `json:"remedy,omitempty"`
	Count  int        `json:"count,omitempty"`
}

// StackChain is one route, from whoever calls it to whatever it touches.
type StackChain struct {
	Endpoint StackNode `json:"endpoint"`

	// Clients are the declarations that call this route, each with the
	// CONSUMES edge's own confidence and evidence.
	Clients []StackHop `json:"clients"`

	// Handler is what the route table says answers this route.
	Handler *StackHop `json:"handler,omitempty"`

	// Services are what the handler reaches through CALLS, bounded.
	Services []StackHop `json:"services"`

	// Tables are the data consequences of everything above.
	Tables []StackHop `json:"tables"`

	// Weakest is the lowest tier along the whole chain — the tier the chain is
	// drawn at. A chain is no stronger than its weakest hop.
	Weakest string `json:"weakest_confidence"`

	// Reach is how far the chain actually got: client | endpoint | handler |
	// service | table. A reader sorting by this sees the complete chains first
	// and the stumps, with their reasons, after.
	Reach StackStage `json:"reach"`

	// Gaps names every place this chain stops short and why.
	Gaps []StackGap `json:"gaps"`

	// Writes is true when any table hop is a WRITES. The single fact that turns
	// "this component talks to the database" into a decision.
	Writes bool `json:"writes"`
}

// CrossStackFilters echoes the bounds actually applied.
type CrossStackFilters struct {
	MinConfidence string `json:"min_confidence"`
	MaxDepth      int    `json:"max_depth"`
	Limit         int    `json:"limit"`
	Offset        int    `json:"offset"`

	// Selection states the rule a capped view kept by.
	Selection string `json:"selection"`
}

// Page is pagination as a first-class object rather than a sentence.
//
// # Why this type exists
//
// This endpoint already accepted `?offset=`. What it did with it was record a
// human sentence — "Showing routes 1-12 of 411 … ?offset=12 for the next page"
// — inside an omission's Detail, and the client parsed that prose with a regular
// expression to decide whether to enable its Next button. Two consequences, both
// reproduced live: the parse was wired to the wrong payload so Next was disabled
// on every load and 399 of 411 routes were unreachable from the UI; and the
// sentence only existed AT ALL when the page was truncated, so the last page —
// and any complete result — carried no paging information whatsoever.
//
// Paging is not a remark about a truncation. It is state: where you are, how big
// the step is, how many there are, and whether there is a next one. All four are
// fields here, on every response including the ones that fit, so a client renders
// a pager from data instead of inferring one from prose.
//
// # Ordering is part of the contract
//
// A page boundary is only coherent if the underlying order is TOTAL. Ordering
// here ends in the endpoint's symbol id, which is unique, so no two routes can
// compare equal and no route can appear on two pages or fall between them. The
// rule is echoed in Ordering so a caller can verify the claim rather than take
// it — and so a future change to the sort has to change the sentence a reader
// is reading.
type Page struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`

	// Total is the size of the whole ordered set, not of this page.
	Total int `json:"total"`

	// Returned is how many items this body carries.
	Returned int `json:"returned"`

	// First and Last are 1-based inclusive positions, or 0 on an empty page.
	// Computed here so seven clients do not compute seven off-by-ones.
	First int `json:"first"`
	Last  int `json:"last"`

	HasPrev bool `json:"has_prev"`
	HasNext bool `json:"has_next"`

	// PrevOffset / NextOffset are the exact values to send back. -1 when the
	// corresponding direction does not exist, so a client that ignores the
	// booleans still cannot construct a nonsense request.
	PrevOffset int `json:"prev_offset"`
	NextOffset int `json:"next_offset"`

	// Ordering is the total order the offsets index into.
	Ordering string `json:"ordering"`

	// MaxLimit is the ceiling on `?limit=`.
	MaxLimit int `json:"max_limit"`
}

// newPage computes the pager from the three numbers that define it.
func newPage(offset, limit, total, returned int, ordering string, maxLimit int) Page {
	p := Page{
		Offset: offset, Limit: limit, Total: total, Returned: returned,
		PrevOffset: -1, NextOffset: -1, Ordering: ordering, MaxLimit: maxLimit,
	}
	if returned > 0 {
		p.First = offset + 1
		p.Last = offset + returned
	}
	if offset > 0 {
		p.HasPrev = true
		p.PrevOffset = max(0, offset-limit)
	}
	if offset+returned < total {
		p.HasNext = true
		p.NextOffset = offset + returned
	}
	return p
}

// CrossStackCounts is the arithmetic of the screen.
//
// # Two scopes, and they must not be mixed
//
// The headline row used to read "411 routes · 0 with a recorded caller · 12 with
// a declared handler". 411 was every route in the index; the other three were
// counted over the twelve chains on the current PAGE. Same row, same font, two
// denominators, and turning the page silently changed three of the four numbers
// while the first stayed put.
//
// So the whole-index figures — the ones a reader quotes — are computed over
// every endpoint before paging, and the page-scoped ones live in their own
// struct with a name that says so. WithService and WithTable are page-scoped by
// necessity, not by choice: each requires a bounded traversal per chain, and
// running 411 of them to fill a headline would make this screen cost what it
// exists to avoid. That limit is stated rather than hidden behind a number.
type CrossStackCounts struct {
	// Endpoints, WithClient, WithHandler and ConsumesEdges are computed over
	// the WHOLE index, independent of paging.
	Endpoints int `json:"endpoints"`

	// WithClient is how many routes anything in this repository is recorded as
	// calling. The gap between Endpoints and WithClient is the number that
	// matters: it is either a public API or an index that cannot see its own
	// callers, and the blind-spot section on the overview says which.
	WithClient    int `json:"with_client"`
	WithHandler   int `json:"with_handler"`
	ConsumesEdges int `json:"consumes_edges"`

	// Page holds everything scoped to the chains in THIS body.
	Page PageCounts `json:"page"`
}

// PageCounts is the arithmetic of the chains actually returned.
type PageCounts struct {
	Chains      int `json:"chains"`
	WithClient  int `json:"with_client"`
	WithHandler int `json:"with_handler"`
	WithService int `json:"with_service"`
	WithTable   int `json:"with_table"`

	// Complete counts chains on this page that ran the whole way to a table.
	Complete int `json:"complete_to_table"`
	Writing  int `json:"writing_to_a_table"`

	// Scope is the sentence that stops these being read as repository totals.
	Scope string `json:"scope"`
}

// CrossStackResponse is the whole cross-stack screen.
type CrossStackResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Filters CrossStackFilters `json:"filters"`
	Counts  CrossStackCounts  `json:"counts"`

	// Page is where in the route list this body sits. Always present.
	Page Page `json:"page"`

	Chains []StackChain `json:"chains"`

	// Gaps is every chain's gaps, aggregated by reason and counted. Hoisted so
	// the page can lead with what the view cannot see rather than ending with
	// it.
	Gaps []StackGap `json:"gaps"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// Bounds. A chain costs one bounded traversal, so the chain count is the number
// that has to stay small; the per-chain lists are cheap and bounded separately.
const (
	// A page, not a dump. At 60 chains this screen rendered a 33,181-pixel page
	// against a real 418-endpoint repository — about 37 screens of continuous
	// scroll with no way to reach the end except the scrollbar. A limit without
	// an offset is not pagination; it is truncation with a bigger number
	// available, and the 358 routes past the cut were unreachable at any limit
	// below 418.
	defaultChainLimit = 12
	maxChainLimit     = 250

	// maxClientsPerChain / maxServicesPerChain / maxTablesPerChain bound one
	// chain's own lists. Whatever they cut is counted in the chain's gaps AND
	// in the response's omission block.
	maxClientsPerChain  = 12
	maxServicesPerChain = 20
	maxTablesPerChain   = 20

	// defaultStackDepth is how far past the handler the CALLS walk goes.
	// Three hops is a handler, the service it calls and that service's
	// collaborator — past which the claim "this route touches that table" is
	// carried by so many inferred edges that it stops being worth drawing.
	defaultStackDepth = 3
	maxStackDepth     = 5
)

func (s *Server) handleCrossStack(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	depth := intParam(r, "depth", defaultStackDepth, 1, maxStackDepth)
	limit := intParam(r, "limit", defaultChainLimit, 1, maxChainLimit)
	offset := intParam(r, "offset", 0, 0, 1<<20)

	resp := CrossStackResponse{
		Meta: meta, Legend: Legend(),
		Chains: []StackChain{}, Gaps: []StackGap{},
		Filters: CrossStackFilters{
			MinConfidence: string(floor), MaxDepth: depth, Limit: limit, Offset: offset,
			Selection: chainOrdering,
		},
	}
	if !ok {
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	chains, counts, page, err := s.buildChains(ctx, idx, floor, depth, limit, offset, &om)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp.Chains = chains
	resp.Counts = counts
	resp.Page = page
	resp.Gaps = aggregateGaps(chains)

	if counts.Endpoints == 0 {
		resp.Help = &Guidance{
			Title: "This index recorded no HTTP endpoints",
			Detail: "The cross-stack view is built on route declarations. Without one there is nothing for a " +
				"CONSUMES edge to bind to, and an outbound call in this repository stays an unbound call " +
				"rather than becoming a chain. That is a gap in what was extractable, not evidence that this " +
				"repository serves nothing.",
			Steps: []string{
				"Check the overview's blind spots for your API framework's language.",
				"repocortex indexers status — a compiler-backed index raises every hop's tier",
			},
		}
	}

	// The floor's own count, over the four edge kinds this screen reads. Stated
	// separately from the other budgets because it is the one a reader undoes
	// with a single control.
	om.Hide(ReasonBelowConfidenceFloor, idx.crossStackBelow(floor),
		fmt.Sprintf("CONSUMES, EXPOSES, READS and WRITES relationships recorded weaker than %s. Each is a "+
			"hop this screen is not drawing, so a component that reaches a table only through one of them "+
			"appears here not to reach it at all.", tierWord(floor)),
		"lower the standard of proof to `low`")

	switch {
	case counts.Endpoints > 0 && counts.WithClient == 0:
		resp.Next = NextAction{
			Label: fmt.Sprintf("None of %d route(s) has a recorded caller", counts.Endpoints),
			Detail: "Either this repository's routes are called from outside it — which is what a public API " +
				"looks like — or its client calls were never extracted. Those are opposite conclusions and " +
				"the blind spots on the overview tell them apart.",
			Screen: "overview#blind-spots",
		}
	case counts.Page.Writing > 0:
		resp.Next = NextAction{
			Label:  fmt.Sprintf("%d chain(s) on this page reach a table they WRITE", counts.Page.Writing),
			Detail: "A change to any component on those chains can alter what lands in a table. Open the blast radius for the handler before touching one.",
			Screen: "blast",
		}
	case counts.WithHandler < counts.Endpoints:
		resp.Next = NextAction{
			Label: fmt.Sprintf("%d route(s) declare no handler", counts.Endpoints-counts.WithHandler),
			Detail: "The framework's syntax did not reveal what answers them. The route is real; its target is unknown, " +
				"and every chain through it stops at the endpoint.",
			Screen: "graph",
		}
	default:
		resp.Next = NextAction{
			Label:  "Read each chain at its weakest hop",
			Detail: "A chain is no stronger than its weakest link. The tier beside each chain is that link, not an average.",
			Screen: "overview",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// buildChains assembles one chain per endpoint.
func (s *Server) buildChains(
	ctx context.Context,
	idx *structuralIndex,
	floor model.Confidence,
	depth, limit, offset int,
	om *Omissions,
) ([]StackChain, CrossStackCounts, Page, error) {
	var counts CrossStackCounts

	// One pass over the cached edges collects everything except the CALLS walk.
	type endpointFacts struct {
		clients   []StackHop
		handlerID int64
		handler   *StackHop
	}
	facts := map[int64]*endpointFacts{}
	get := func(id int64) *endpointFacts {
		f, ok := facts[id]
		if !ok {
			f = &endpointFacts{}
			facts[id] = f
		}
		return f
	}

	// Endpoints are symbols of kind `endpoint`. Collected from the symbol table
	// rather than from the edges, so a route nothing calls and nothing answers
	// is still on this screen — that absence is the finding.
	for id, sym := range idx.syms {
		if strings.EqualFold(string(sym.Kind), string(model.KindEndpoint)) {
			get(id)
		}
	}

	var clientsBelow, handlersBelow int
	tablesBySymbol := map[int64][]StackHop{}
	for i := range idx.edges {
		e := idx.edges[i]
		switch e.Kind {
		case model.EdgeConsumes:
			// src = the declaration containing the outbound call, dst = route.
			f, isEndpoint := facts[e.Dst]
			if !isEndpoint {
				continue
			}
			counts.ConsumesEdges++
			if !e.Confidence.AtLeast(floor) {
				clientsBelow++
				continue
			}
			f.clients = append(f.clients, StackHop{
				Stage: StageClient, Node: idx.stackNode(e.Src),
				Relation: string(model.EdgeConsumes), Confidence: tierOf(e.Confidence),
				Evidence: Evidence{FilePath: e.File, Line: int(e.Line)},
			})
		case model.EdgeExposes:
			// src = route, dst = handler.
			f, isEndpoint := facts[e.Src]
			if !isEndpoint {
				continue
			}
			if !e.Confidence.AtLeast(floor) {
				handlersBelow++
				continue
			}
			if f.handler != nil {
				continue
			}
			f.handlerID = e.Dst
			f.handler = &StackHop{
				Stage: StageHandler, Node: idx.stackNode(e.Dst),
				Relation: string(model.EdgeExposes), Confidence: tierOf(e.Confidence),
				Evidence: Evidence{FilePath: e.File, Line: int(e.Line)},
			}
		case model.EdgeReads, model.EdgeWrites:
			if !e.Confidence.AtLeast(floor) {
				continue
			}
			tablesBySymbol[e.Src] = append(tablesBySymbol[e.Src], StackHop{
				Stage: StageTable, Node: idx.stackNode(e.Dst),
				Relation: string(e.Kind), Confidence: tierOf(e.Confidence),
				Evidence: Evidence{FilePath: e.File, Line: int(e.Line)},
			})
		}
	}
	counts.Endpoints = len(facts)
	// Whole-index figures, computed over every endpoint BEFORE paging, so
	// turning the page cannot change them. See the CrossStackCounts comment.
	for _, f := range facts {
		if len(f.clients) > 0 {
			counts.WithClient++
		}
		if f.handler != nil {
			counts.WithHandler++
		}
	}

	// Order the endpoints before the expensive part, so the cap falls by a
	// stated rule rather than by map iteration order.
	ids := make([]int64, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		fa, fb := facts[ids[a]], facts[ids[b]]
		// Routes with a recorded caller first: they are the ones this screen
		// exists to draw.
		if ca, cb := len(fa.clients) > 0, len(fb.clients) > 0; ca != cb {
			return ca
		}
		if ha, hb := fa.handler != nil, fb.handler != nil; ha != hb {
			return ha
		}
		na, nb := idx.syms[ids[a]].Name, idx.syms[ids[b]].Name
		if na != nb {
			return na < nb
		}
		return ids[a] < ids[b]
	})

	// Page into the ordered list. The order is stable — recorded callers first,
	// then a declared handler, then endpoint name — so a given offset means the
	// same routes on every request against one run, which is what makes paging
	// through them coherent rather than a shuffle.
	total := len(ids)
	start := offset
	if start > total {
		start = total
	}
	window := ids[start:]
	if len(window) > limit {
		window = window[:limit]
	}
	kept := window

	// The pager, computed from the three numbers that define it and returned as
	// STATE rather than as a sentence inside an omission. See the Page type.
	pageState := newPage(start, limit, total, len(kept), chainOrdering, maxChainLimit)

	// The omission is still recorded, because routes past this page really are
	// absent from this body and the omission block is where a reader is trained
	// to look for that. It is no longer the only place the fact exists.
	om.Hide(ReasonRowBudget, total-len(kept),
		fmt.Sprintf("Showing routes %d-%d of %d. Order is: %s.",
			pageState.First, pageState.Last, total, chainOrdering),
		fmt.Sprintf("?offset=%d for the next page, or raise ?limit= (up to %d)",
			max(pageState.NextOffset, 0), maxChainLimit))

	chains := make([]StackChain, 0, len(kept))
	var servicesTruncated int
	for _, id := range kept {
		if err := ctx.Err(); err != nil {
			return nil, counts, Page{}, err
		}
		f := facts[id]
		chain := StackChain{
			Endpoint: idx.stackNode(id),
			Clients:  []StackHop{}, Services: []StackHop{}, Tables: []StackHop{},
			Gaps: []StackGap{}, Reach: StageEndpoint,
		}

		if len(f.clients) > 0 {
			counts.Page.WithClient++
			sort.Slice(f.clients, func(a, b int) bool {
				if f.clients[a].Node.Name != f.clients[b].Node.Name {
					return f.clients[a].Node.Name < f.clients[b].Node.Name
				}
				return f.clients[a].Node.SymbolID < f.clients[b].Node.SymbolID
			})
			if extra := len(f.clients) - maxClientsPerChain; extra > 0 {
				chain.Gaps = append(chain.Gaps, StackGap{
					Stage: StageClient, Reason: ReasonRowBudget, Count: extra,
					Detail: fmt.Sprintf("%d further declaration(s) call this route and are not listed.", extra),
				})
				f.clients = f.clients[:maxClientsPerChain]
			}
			chain.Clients = f.clients
			chain.Reach = StageClient
		} else {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage:  StageClient,
				Reason: "no_recorded_caller",
				Detail: "No declaration in this repository is recorded as calling this route. That means either it is " +
					"called from outside — another service, a browser, a script — or the call was never bound. " +
					"internal/registry refuses to bind a call whose target is assembled at run time or matches " +
					"more than one route, so an unbound call is a stated gap rather than a guess.",
				Remedy: "lower the standard of proof, or check the overview's blind spots for your client language",
			})
		}

		if f.handler == nil {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage:  StageHandler,
				Reason: "no_declared_handler",
				Detail: "No EXPOSES edge names what answers this route, so the chain stops here. The route is real; " +
					"the framework's syntax did not reveal its target.",
				Remedy: "a compiler-backed index resolves handlers the syntax alone cannot",
			})
			chains = append(chains, finishChain(chain))
			continue
		}
		counts.Page.WithHandler++
		chain.Handler = f.handler
		if chain.Reach == StageEndpoint || chain.Reach == StageClient {
			chain.Reach = StageHandler
		}

		// The one hop that is a traversal, and it is internal/graph's.
		res, err := s.deps.Caps.Graph.Traverse(ctx, graph.Query{
			Roots:         []int64{f.handlerID},
			Kinds:         []model.EdgeKind{model.EdgeCalls, model.EdgeReferences},
			MinConfidence: floor,
			MaxDepth:      depth,
			MaxNodes:      maxServicesPerChain * 4,
			Direction:     graph.Outgoing,
		})
		if err != nil {
			return nil, counts, Page{}, fmt.Errorf("web: cross-stack walk from handler %d: %w", f.handlerID, err)
		}
		if res.Truncated {
			servicesTruncated++
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage: StageService, Reason: ReasonTraversalBound,
				Detail: "The walk from the handler stopped before exhausting what it reaches: " + res.TruncatedReason +
					". Code beyond that boundary is UNKNOWN to this chain, not absent from it.",
				Remedy: "raise ?depth= or read the handler's blast radius",
			})
		}

		// Tables are collected from the handler AND from everything it reaches,
		// because a handler that touches no table itself but calls a repository
		// that does is the ordinary shape of a layered service.
		tableSeen := map[int64]struct{}{}
		addTables := func(symID int64) {
			for _, t := range tablesBySymbol[symID] {
				key := t.Node.SymbolID
				if _, dup := tableSeen[key]; dup {
					continue
				}
				tableSeen[key] = struct{}{}
				chain.Tables = append(chain.Tables, t)
			}
		}
		addTables(f.handlerID)

		for _, n := range res.Nodes {
			if n.SymbolID == f.handlerID {
				continue // the root is the handler, already placed
			}
			if len(chain.Services) < maxServicesPerChain {
				chain.Services = append(chain.Services, StackHop{
					Stage: StageService, Node: idx.stackNode(n.SymbolID),
					Relation: string(n.Via), Confidence: tierOf(n.Confidence), Depth: n.Depth,
				})
			}
			addTables(n.SymbolID)
		}
		if extra := len(res.Nodes) - 1 - maxServicesPerChain; extra > 0 {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage: StageService, Reason: ReasonRowBudget, Count: extra,
				Detail: fmt.Sprintf("%d further symbol(s) reachable from this handler are not listed. Tables they "+
					"touch ARE still counted below.", extra),
			})
		}
		if len(chain.Services) > 0 {
			counts.Page.WithService++
			chain.Reach = StageService
		} else {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage:  StageService,
				Reason: "no_outbound_calls",
				Detail: fmt.Sprintf("No CALLS or REFERENCES edge at %s or better leaves this handler. Either it is "+
					"self-contained, or its calls were never resolved — an empty answer here is not evidence "+
					"that the handler runs nothing.", tierWord(floor)),
				Remedy: "lower the standard of proof",
			})
		}

		sort.Slice(chain.Tables, func(a, b int) bool {
			if chain.Tables[a].Node.Name != chain.Tables[b].Node.Name {
				return chain.Tables[a].Node.Name < chain.Tables[b].Node.Name
			}
			return chain.Tables[a].Relation < chain.Tables[b].Relation
		})
		if extra := len(chain.Tables) - maxTablesPerChain; extra > 0 {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage: StageTable, Reason: ReasonRowBudget, Count: extra,
				Detail: fmt.Sprintf("%d further table(s) touched by this chain are not listed.", extra),
			})
			chain.Tables = chain.Tables[:maxTablesPerChain]
		}
		if len(chain.Tables) > 0 {
			counts.Page.WithTable++
			counts.Page.Complete++
			chain.Reach = StageTable
			for _, t := range chain.Tables {
				if t.Relation == string(model.EdgeWrites) {
					chain.Writes = true
				}
			}
			if chain.Writes {
				counts.Page.Writing++
			}
		} else {
			chain.Gaps = append(chain.Gaps, StackGap{
				Stage:  StageTable,
				Reason: "no_data_access",
				Detail: "Nothing on this chain is recorded as reading or writing a table. A handler that touches no " +
					"data and one whose queries were never extracted look identical from here, and the second is " +
					"common: internal/registry stages a table access only where the query text was legible.",
				Remedy: "check the overview's blind spots for READS and WRITES in this language",
			})
		}

		chains = append(chains, finishChain(chain))
	}

	counts.Page.Chains = len(chains)
	counts.Page.Scope = fmt.Sprintf(
		"counted over the %d chain(s) on this page, not over all %d route(s): the service and table hops "+
			"each cost a bounded traversal, so they are computed only for the routes actually returned",
		len(chains), total)

	om.Hide(ReasonBelowConfidenceFloor, clientsBelow,
		fmt.Sprintf("Outbound calls bound to a route by an edge weaker than %s. Each is a component this screen "+
			"does not show calling anything.", tierWord(floor)),
		"lower the standard of proof to `low`")
	om.Hide(ReasonBelowConfidenceFloor, handlersBelow,
		fmt.Sprintf("Route-to-handler links recorded weaker than %s. The routes are drawn; what answers them is not.", tierWord(floor)),
		"lower the standard of proof to `low`")
	if servicesTruncated > 0 {
		om.Note(ReasonTraversalBound,
			fmt.Sprintf("%d chain(s) had their handler walk truncated at the traversal bound. Symbols past that "+
				"boundary — and any table they touch — are missing from those chains.", servicesTruncated),
			"raise ?depth=, or open the handler in the blast-radius screen")
	}

	// Sorted WITHIN the page only. The page was selected by the ordering in
	// chainOrdering; re-sorting the selection by reach is a presentation choice
	// that cannot move a chain onto another page, because the selection has
	// already happened. Ordering ends in the symbol id, which is unique, so the
	// selection order is total and no route can be duplicated across pages or
	// skipped between them.
	sort.Slice(chains, func(a, b int) bool {
		if ra, rb := reachRank(chains[a].Reach), reachRank(chains[b].Reach); ra != rb {
			return ra < rb
		}
		if chains[a].Endpoint.Name != chains[b].Endpoint.Name {
			return chains[a].Endpoint.Name < chains[b].Endpoint.Name
		}
		return chains[a].Endpoint.SymbolID < chains[b].Endpoint.SymbolID
	})
	return chains, counts, pageState, nil
}

// chainOrdering is the total order offsets index into, stated once and echoed in
// every Page so a caller can check the claim.
const chainOrdering = "routes with a recorded caller first, then routes with a declared handler, " +
	"then endpoint name, then endpoint symbol id — total, so a page boundary cannot duplicate or drop a route"

// finishChain computes the chain's weakest tier over every hop it actually has.
//
// The floor of the whole chain, not an average and not the endpoint's own tier:
// "this component reaches that table" fails exactly where its weakest link
// fails, so that is the tier the claim is worth.
func finishChain(c StackChain) StackChain {
	weakest := ""
	consider := func(tier string) {
		if tier == "" {
			return
		}
		if weakest == "" {
			weakest = tier
			return
		}
		if model.Confidence(tier).Rank() < model.Confidence(weakest).Rank() {
			weakest = tier
		}
	}
	for _, h := range c.Clients {
		consider(h.Confidence)
	}
	if c.Handler != nil {
		consider(c.Handler.Confidence)
	}
	for _, h := range c.Services {
		consider(h.Confidence)
	}
	for _, h := range c.Tables {
		consider(h.Confidence)
	}
	if weakest == "" {
		// A route with no hop at all. `unknown` rather than a silent empty
		// string: the chain asserts nothing, and saying so is not the same as
		// leaving the field blank.
		weakest = "unknown"
	}
	c.Weakest = weakest
	return c
}

func reachRank(s StackStage) int {
	switch s {
	case StageTable:
		return 0
	case StageService:
		return 1
	case StageHandler:
		return 2
	case StageClient:
		return 3
	default:
		return 4
	}
}

// aggregateGaps folds every chain's gaps into one counted list, so the screen
// can lead with what it cannot see.
func aggregateGaps(chains []StackChain) []StackGap {
	type key struct {
		stage  StackStage
		reason string
	}
	agg := map[key]*StackGap{}
	for _, c := range chains {
		for _, g := range c.Gaps {
			k := key{g.Stage, g.Reason}
			got, ok := agg[k]
			if !ok {
				cp := g
				cp.Count = 0
				agg[k] = &cp
				got = agg[k]
			}
			// One chain stopped for this reason. Counting chains rather than
			// summing each gap's own count keeps "how many chains does this
			// affect" answerable, which is the question a reader has.
			got.Count++
		}
	}
	out := make([]StackGap, 0, len(agg))
	for _, g := range agg {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Stage != out[j].Stage {
			return reachRank(out[i].Stage) < reachRank(out[j].Stage)
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// stackNode projects one symbol into the shape a chain hop renders.
func (idx *structuralIndex) stackNode(id int64) StackNode {
	s, ok := idx.syms[id]
	if !ok {
		// The graph and the symbol table disagree. Report the id rather than
		// dropping the hop — silently losing a node is what this whole package
		// exists to avoid.
		return StackNode{SymbolID: id, Name: fmt.Sprintf("symbol#%d", id), Kind: "unknown"}
	}
	return StackNode{
		SymbolID: id, Name: s.Name, FQN: s.FQN, Kind: string(s.Kind),
		Path: idx.symbolPath(id), Language: idx.symbolLanguage(id), Container: s.Container,
	}
}

// crossStackBelow counts the edges of the four kinds this screen reads that the
// floor excluded, across the whole run.
func (idx *structuralIndex) crossStackBelow(floor model.Confidence) int {
	n := 0
	for _, kind := range []model.EdgeKind{
		model.EdgeConsumes, model.EdgeExposes, model.EdgeReads, model.EdgeWrites,
	} {
		if h := idx.byKind[kind]; h != nil {
			n += h.Below(floor)
		}
	}
	return n
}
