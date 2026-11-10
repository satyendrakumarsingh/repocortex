package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Blast radius: "what breaks if I change this?"
//
// # Nothing here is computed in this package
//
// The impacted set is capability/impact, the covering tests are
// capability/tests, and both are returned with their own types intact — the
// impact.Impacted rows keep their per-row Evidence and their path-minimum
// Confidence, and the report's capability.Completeness block travels to the
// client verbatim. Remapping them into portal-local structs would have been the
// obvious thing to do and would have quietly dropped exactly the fields this
// product exists to carry.
//
// # The one thing this package adds
//
// Facets: which TABLES the impacted set writes and reads, and which ENDPOINTS
// expose it. Those are read straight off the run's WRITES / READS / EXPOSES
// edges — no traversal, no inference — and each carries the weakest confidence
// among the edges that produced it plus a real file:line. They are here because
// "47 symbols are affected" is not a decision; "47 symbols are affected, two of
// them write the payments table, one of them is behind POST /refunds" is.
//
// # What the floor hid
//
// impact.Report already counts the symbols an identical unfloored walk would
// have reached, and the facet scan counts its own sub-floor edges. Both land in
// the response's omission block, so a reader who sees four callers also sees how
// many the floor kept out and which control gets them back.

// BlastStatus is the outcome of resolving the requested symbol.
type BlastStatus string

const (
	// BlastOK: exactly one symbol matched and the report is present.
	BlastOK BlastStatus = "ok"

	// BlastNoQuery: no symbol was asked for. The screen renders its search box.
	BlastNoQuery BlastStatus = "no_query"

	// BlastNotFound: nothing in the index matches. A normal outcome — a typo,
	// or a symbol the index could not extract — and therefore a 200, not a 404:
	// the portal answered correctly, and a network-level error would teach the
	// reader the tool is broken when they have merely mistyped a name.
	BlastNotFound BlastStatus = "not_found"

	// BlastAmbiguous: a bare name matched several definitions. Resolving it by
	// picking the exported one, or the first, would be exactly the silent
	// overclaim the capability layer refuses to make — an impact analysis of the
	// wrong `Close` is not a partial answer, it is a wrong one.
	BlastAmbiguous BlastStatus = "ambiguous"
)

// Candidate is one definition a bare name matched.
type Candidate struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	FQN      string `json:"fqn"`
	Kind     string `json:"kind"`
	Path     string `json:"file_path"`
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`
}

// Facet is one externally visible consequence of the impacted set: a table it
// writes, a table it reads, or an endpoint that exposes it.
type Facet struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Path     string `json:"file_path,omitempty"`

	// Relation is WRITES | READS | EXPOSES — the edge kind that put this facet
	// in the report, so the claim is legible without reading the field name.
	Relation string `json:"relation"`

	// Confidence is the WEAKEST tier among the edges linking the impacted set
	// to this facet. A facet reached only through a `medium` edge is a question,
	// not a finding, and is drawn dashed for that reason.
	Confidence string `json:"confidence"`

	// Through names the impacted symbols that reach it, bounded.
	Through []string `json:"through"`

	// Evidence is one real source location proving the link.
	Evidence Evidence `json:"evidence"`
}

// TestSection is the covering-test answer, from capability/tests.
type TestSection struct {
	Tests []tests.TestHit `json:"tests"`

	// Untested is a positive finding, not an empty list: "we found nothing" and
	// "we did not look" must not render identically.
	Untested bool `json:"untested"`

	// ProvenByGraph is the count reached through recorded edges — the only
	// number here that is evidence of execution rather than of intent.
	ProvenByGraph int            `json:"proven_by_graph"`
	SignalCounts  map[string]int `json:"signal_counts"`

	Completeness capability.Completeness `json:"completeness"`
	Truncated    bool                    `json:"truncated"`
}

// BlastResponse is the whole blast-radius screen.
type BlastResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Status BlastStatus `json:"status"`
	Query  string      `json:"query"`

	// Guidance is present for every status except ok, so an empty screen always
	// says what to do next.
	Help *Guidance `json:"guidance,omitempty"`

	// Candidates is populated for ambiguous queries: the caller re-asks with an
	// FQN rather than the portal guessing.
	Candidates []Candidate `json:"candidates,omitempty"`

	Filters BlastFilters `json:"filters"`

	Target *capability.Target `json:"target,omitempty"`

	// The impact sets, kept as three fields because they are three different
	// claims. Flattening them into one list with a depth column destroys the
	// distinction the report exists to make.
	Direct   []impact.Impacted   `json:"direct"`
	Indirect []impact.Impacted   `json:"indirect"`
	Dispatch []impact.Dispatched `json:"dispatch"`

	// DispatchThrough names the implementations the expansion went through, so
	// the hinge of the claim is visible rather than inferred from the rows.
	DispatchThrough []capability.Symbol `json:"dispatch_through,omitempty"`

	Counts BlastCounts `json:"counts"`

	Risk         impact.Risk             `json:"risk"`
	Unknown      []impact.Unknown        `json:"unknown"`
	Completeness capability.Completeness `json:"completeness"`

	Tests TestSection `json:"tests"`

	TablesWritten []Facet `json:"tables_written"`
	TablesRead    []Facet `json:"tables_read"`
	Endpoints     []Facet `json:"endpoints_exposed"`

	// The floor's own count is NOT hoisted here. It used to be, beside an
	// Omissions block that recorded the same figure with its remedy, and the
	// portal rendered both — one headline number appearing twice, reading as two
	// populations. It lives in Omissions.Totals now, once, where the reader also
	// gets the control that undoes it.
	Omissions Omissions `json:"omissions"`
}

// BlastFilters echoes the bounds actually applied.
type BlastFilters struct {
	MinConfidence string `json:"min_confidence"`
	MaxDepth      int    `json:"max_depth"`
	MaxNodes      int    `json:"max_nodes"`
}

// BlastCounts is the headline arithmetic, with the sets kept apart.
type BlastCounts struct {
	Direct   int `json:"direct"`
	Indirect int `json:"indirect"`
	Dispatch int `json:"dispatch"`

	// Impacted is direct + indirect: symbols reached through RECORDED
	// references. Dispatch is excluded on purpose — those are symbols the
	// runtime MAY execute, and folding them in would restore the conflation the
	// three fields exist to remove.
	Impacted int `json:"impacted_through_references"`
	Reached  int `json:"reached_including_dispatch"`

	Tests         int `json:"covering_tests"`
	ProvenByGraph int `json:"tests_proven_by_graph"`
}

// facetThrough bounds how many impacted symbol names are listed per facet.
const facetThrough = 8

func (s *Server) handleBlast(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("symbol"))

	floor := confidenceParam(r, "min_confidence", impact.DefaultMinConfidence)
	depth := intParam(r, "depth", impact.DefaultMaxDepth, 1, 6)
	nodes := intParam(r, "max_nodes", impact.DefaultMaxNodes, 50, 20000)

	resp := BlastResponse{
		Meta: meta, Legend: Legend(), Query: query,
		Filters: BlastFilters{MinConfidence: string(floor), MaxDepth: depth, MaxNodes: nodes},
		// Non-nil so the client never has to distinguish null from empty.
		Direct: []impact.Impacted{}, Indirect: []impact.Impacted{},
		Dispatch: []impact.Dispatched{}, TablesWritten: []Facet{},
		TablesRead: []Facet{}, Endpoints: []Facet{},
	}
	if !ok {
		resp.Status = BlastNoQuery
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if query == "" {
		resp.Status = BlastNoQuery
		resp.Help = &Guidance{
			Title:  "Pick a symbol",
			Detail: "Blast radius answers what breaks if you change one thing, so it needs to know which thing.",
			Steps: []string{
				"Search for a symbol by name in the box above.",
				"A bare name matching several definitions returns the candidates rather than guessing — re-ask with a fully-qualified name.",
			},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	opts := impact.Options{MaxDepth: depth, MaxNodes: nodes, MinConfidence: floor}
	rep, err := impact.AnalyzeWith(ctx, s.deps.Caps, run.ID, query, opts)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		switch {
		case errors.As(err, &amb):
			resp.Status = BlastAmbiguous
			resp.Candidates = candidatesOf(amb.Candidates)
			resp.Help = &Guidance{
				Title:  fmt.Sprintf("%q matches %d definitions", query, len(amb.Candidates)),
				Detail: "The portal will not pick one. An impact analysis of the wrong symbol is not a partial answer, it is a wrong one.",
				Steps:  []string{"Choose a candidate below, or re-ask with its fully-qualified name."},
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		case errors.Is(err, capability.ErrTargetNotFound):
			resp.Status = BlastNotFound
			resp.Help = &Guidance{
				Title:  fmt.Sprintf("No symbol named %q in this index", query),
				Detail: "That is a normal outcome — a typo, or a symbol this index could not extract from its language.",
				Steps: []string{
					"Check the spelling, and try the fully-qualified name.",
					"Check the overview's blind spots: a language with no extracted symbols of that kind will never match.",
				},
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		default:
			s.fail(w, r, err)
			return
		}
	}

	var om Omissions

	resp.Status = BlastOK
	target := rep.Target
	resp.Target = &target
	resp.Risk = rep.Risk
	resp.Unknown = rep.Unknown
	resp.Completeness = rep.Completeness
	resp.DispatchThrough = rep.DispatchThrough

	resp.Counts = BlastCounts{
		Direct: len(rep.Direct), Indirect: len(rep.Indirect), Dispatch: len(rep.Dispatch),
		Impacted: rep.TotalImpacted(), Reached: rep.TotalReached(),
	}

	resp.Direct = capped(rep.Direct, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Direct callers beyond the first %d (ordered by depth then symbol id) are not listed. "+
			"The COUNT above is the full figure — only the rows are shortened.", maxRows),
		"narrow the query, or read the full report with `cortex context`")
	resp.Indirect = capped(rep.Indirect, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Indirectly impacted symbols beyond the first %d are not listed.", maxRows),
		"lower --depth to see a tighter set")
	resp.Dispatch = capped(rep.Dispatch, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Dispatch-expansion rows beyond the first %d are not listed.", maxRows), "")

	// The floor's own count, from impact rather than recomputed here.
	om.Hide(ReasonBelowConfidenceFloor, rep.LowConfidenceHidden,
		fmt.Sprintf("Symbols that reach %s only through edges weaker than %s. They are still in the index. "+
			"They are excluded by default because name-inferred `low` edges are measured at 0.0–2.8%% precision "+
			"(docs/accuracy.md), so most describe a relationship that does not exist — most, not all, which "+
			"is why this number is stated rather than the edges quietly dropped.", target.Name, tierWord(floor)),
		"set the standard of proof to `low`")

	if rep.Truncated {
		om.Note(ReasonTraversalBound,
			"The traversal stopped before exhausting the reachable set: "+rep.TruncatedReason+
				". Symbols beyond that boundary are affected-status UNKNOWN, not unaffected.",
			"raise ?max_nodes= or lower ?depth=")
	}
	if rep.DirectEvidenceOmitted {
		om.Note(ReasonEvidenceBudget, rep.DirectEvidenceReason,
			"query the symbol directly with `cortex callers`")
	}

	// ---- covering tests, from capability/tests
	testRep, err := tests.FindTestsWith(ctx, s.deps.Caps, run.ID, target.FQN, tests.Options{
		MaxDepth: depth + 1, MinConfidence: floor,
	})
	if err != nil {
		// A failure to find tests must not blank the impact answer, which is the
		// part the reader came for. Say so instead of returning nothing.
		s.log.Warn("web: covering tests", "target", target.FQN, "error", err)
		om.Note(ReasonRowBudget,
			"Covering tests could not be computed for this target: "+err.Error(),
			"cortex doctor")
		resp.Tests = TestSection{Tests: []tests.TestHit{}, SignalCounts: map[string]int{}}
	} else {
		signals := make(map[string]int, len(testRep.SignalCounts))
		for k, v := range testRep.SignalCounts {
			signals[string(k)] = v
		}
		hits := capped(testRep.Tests, maxRows, &om, ReasonRowBudget,
			fmt.Sprintf("Covering tests beyond the first %d are not listed.", maxRows), "")
		if hits == nil {
			hits = []tests.TestHit{}
		}
		resp.Tests = TestSection{
			Tests: hits, Untested: testRep.Untested, ProvenByGraph: testRep.ProvenByGraph,
			SignalCounts: signals, Completeness: testRep.Completeness, Truncated: testRep.Truncated,
		}
		resp.Counts.Tests = len(testRep.Tests)
		resp.Counts.ProvenByGraph = testRep.ProvenByGraph
		if testRep.Truncated {
			om.Note(ReasonTraversalBound,
				"The test-discovery traversal was truncated: "+testRep.TruncatedReason+
					". A test beyond that boundary would not appear above.", "")
		}
	}

	// ---- facets
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	written, read, exposed, hiddenFacets := idx.facets(rep, floor)
	resp.TablesWritten = capped(written, maxRows, &om, ReasonRowBudget,
		"Tables written beyond the first "+fmt.Sprint(maxRows)+" are not listed.", "")
	resp.TablesRead = capped(read, maxRows, &om, ReasonRowBudget,
		"Tables read beyond the first "+fmt.Sprint(maxRows)+" are not listed.", "")
	resp.Endpoints = capped(exposed, maxRows, &om, ReasonRowBudget,
		"Endpoints beyond the first "+fmt.Sprint(maxRows)+" are not listed.", "")
	om.Hide(ReasonBelowConfidenceFloor, hiddenFacets,
		fmt.Sprintf("READS / WRITES / EXPOSES relationships between the impacted set and a table or "+
			"endpoint, recorded weaker than %s. Each is a data or API consequence this screen is not showing.", tierWord(floor)),
		"set the standard of proof to `low`")

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

func candidatesOf(in []capability.Target) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		out = append(out, Candidate{
			SymbolID: c.SymbolID, Name: c.Name, FQN: c.FQN, Kind: c.Kind,
			Path: c.FilePath, Line: c.Line, Exported: c.Exported,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FQN < out[j].FQN })
	return out
}

// facets reads the data and API consequences of an impacted set off the run's
// edges.
//
// One linear scan over the cached compact edges. No traversal: a WRITES edge
// from an impacted symbol to a table IS the fact, and inferring anything beyond
// it would be this layer inventing a relationship the index did not record.
//
// The returned int is how many qualifying edges fell below the floor — stated,
// because "no tables are written" and "the two tables written are known only
// through medium-confidence edges" are opposite conclusions.
func (idx *structuralIndex) facets(rep impact.Report, floor model.Confidence) (written, read, exposed []Facet, hidden int) {
	inSet := make(map[int64]struct{}, rep.TotalReached()+1)
	inSet[rep.Target.SymbolID] = struct{}{}
	for _, i := range rep.Direct {
		inSet[i.ID] = struct{}{}
	}
	for _, i := range rep.Indirect {
		inSet[i.ID] = struct{}{}
	}
	for _, d := range rep.Dispatch {
		inSet[d.ID] = struct{}{}
	}

	type acc struct {
		f       Facet
		through map[string]struct{}
	}
	buckets := map[model.EdgeKind]map[int64]*acc{
		model.EdgeWrites:  {},
		model.EdgeReads:   {},
		model.EdgeExposes: {},
	}

	add := func(kind model.EdgeKind, facetID, throughID int64, e compactEdge) {
		bucket := buckets[kind]
		a, ok := bucket[facetID]
		if !ok {
			sym := idx.syms[facetID]
			name := sym.Name
			if name == "" {
				name = fmt.Sprintf("symbol#%d", facetID)
			}
			a = &acc{
				f: Facet{
					SymbolID: facetID, Name: name, Kind: string(sym.Kind),
					Path: idx.symbolPath(facetID), Relation: string(kind),
					Confidence: tierOf(e.Confidence),
					Evidence:   Evidence{FilePath: e.File, Line: int(e.Line)},
				},
				through: map[string]struct{}{},
			}
			bucket[facetID] = a
		}
		// Weakest wins: a facet reached by one proven edge and one guess is
		// still only as certain as the guess if the guess is what links it here.
		if model.Confidence(a.f.Confidence).Rank() > e.Confidence.Rank() {
			a.f.Confidence = tierOf(e.Confidence)
			a.f.Evidence = Evidence{FilePath: e.File, Line: int(e.Line)}
		}
		if s, ok := idx.syms[throughID]; ok && s.Name != "" {
			a.through[s.Name] = struct{}{}
		}
	}

	for i := range idx.edges {
		e := idx.edges[i]
		switch e.Kind {
		case model.EdgeWrites, model.EdgeReads:
			if _, ok := inSet[e.Src]; !ok {
				continue
			}
			if !e.Confidence.AtLeast(floor) {
				hidden++
				continue
			}
			add(e.Kind, e.Dst, e.Src, e)
		case model.EdgeExposes:
			// EXPOSES is recorded src=endpoint, dst=handler, so an impacted
			// handler is found on the DESTINATION side.
			if _, ok := inSet[e.Dst]; !ok {
				continue
			}
			if !e.Confidence.AtLeast(floor) {
				hidden++
				continue
			}
			add(e.Kind, e.Src, e.Dst, e)
		}
	}

	finish := func(kind model.EdgeKind) []Facet {
		bucket := buckets[kind]
		out := make([]Facet, 0, len(bucket))
		for _, a := range bucket {
			names := make([]string, 0, len(a.through))
			for n := range a.through {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) > facetThrough {
				extra := len(names) - facetThrough
				names = append(names[:facetThrough:facetThrough],
					fmt.Sprintf("… and %d more impacted symbol(s)", extra))
			}
			a.f.Through = names
			out = append(out, a.f)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Name != out[j].Name {
				return out[i].Name < out[j].Name
			}
			return out[i].SymbolID < out[j].SymbolID
		})
		return out
	}
	return finish(model.EdgeWrites), finish(model.EdgeReads), finish(model.EdgeExposes), hidden
}
