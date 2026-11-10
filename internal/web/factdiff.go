package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/diffgraph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The fact diff: what changed about the code's FACTS between two index runs.
//
// # Why this is not `git diff` with a nicer font
//
// git reports changed LINES and leaves the reader to reconstruct the facts. A
// one-line import that shifts a thousand definitions down produces a
// thousand-line patch and an empty fact diff — that inversion is the whole
// point. What this reports instead is that an endpoint now writes `refunds`,
// that a symbol lost its last caller, that a CALLS edge fell from `exact` to
// `high` because the compiler stopped proving it. The last one is invisible to
// every line-based tool that exists.
//
// # Nothing is computed here
//
// internal/diffgraph does the comparison and this renders its report. The
// diffgraph types travel to the client INTACT — an EdgeChange keeps its own
// Confidence and its own evidence Sites, exactly as blast.go keeps
// impact.Impacted — because remapping them into portal-local rows is how a
// screen quietly loses the tier and the file:line that make a claim checkable.
//
// # Both sides must already be indexed
//
// This shells out to nothing. A ref names a run that exists in this index, and
// a ref that matches no run is refused rather than silently falling back to
// whatever is committed — a diff against the wrong run is worse than no diff.
// Committing a run retires the one before it, so a working index usually holds
// exactly ONE comparable run, and that is why this screen is normally empty and
// why the empty answer names the two commands that produce a second one.

// DiffSource is the read surface internal/diffgraph needs.
//
// Declared at the consumer as an alias of the port diffgraph itself declares,
// rather than restated: the six methods are diffgraph's requirement, not this
// package's opinion, and a restatement would be a second thing to keep in step.
// It is resolved by asserting on the Registry the server holds, which the
// production registry satisfies.
type DiffSource = diffgraph.IndexSource

// RunLookup resolves a run by id. Separate from DiffSource because this screen
// needs it for a different purpose — deciding whether a requested ref EXISTS
// before comparing against it — and because a fake that only has to answer that
// question should not have to implement five bulk reads.
type RunLookup interface {
	Run(ctx context.Context, id int64) (*model.IndexRun, error)
}

// Compile-time proof the production adapter satisfies both.
var (
	_ DiffSource = (*registry.Registry)(nil)
	_ RunLookup  = (*registry.Registry)(nil)
)

// diffProbeDepth bounds the search for a default base side.
//
// The portal has no way to enumerate the runs an index holds — the registry's
// read port is deliberately single-run — so a default base is found by asking
// for the ids below the head, in order, and stopping at the first that exists.
// Bounded because an unbounded descent over a long-lived index would be a
// linear scan disguised as a default, and stated in the response because a
// default a reader cannot see is a default they will misread.
const diffProbeDepth = 25

// FactDiffFilters echoes the two sides actually compared and how they were
// chosen.
type FactDiffFilters struct {
	FromRequested string `json:"from_requested,omitempty"`
	ToRequested   string `json:"to_requested,omitempty"`

	From int64 `json:"from_run"`
	To   int64 `json:"to_run"`

	// Defaulted names each side that the caller did not choose, and Detail is
	// the sentence saying what was chosen instead and why.
	Defaulted []string `json:"defaulted"`
	Detail    string   `json:"detail,omitempty"`

	MaxPerSection int `json:"max_per_section"`
	ProbeDepth    int `json:"probe_depth"`
}

// FactDiffCounts is the arithmetic of the change.
//
// Relationships first, because that is the half no line-based tool can produce.
type FactDiffCounts struct {
	RelationshipsGained   int `json:"relationships_gained"`
	RelationshipsLost     int `json:"relationships_lost"`
	RelationshipsRegraded int `json:"relationships_regraded"`

	// Upgraded and Downgraded split the regrades. A downgrade is the
	// interesting one: a compiler stopped proving something it used to prove,
	// and every consumer filtering on `exact` silently lost that edge.
	Upgraded   int `json:"relationships_upgraded"`
	Downgraded int `json:"relationships_downgraded"`

	// TestCoverageGained and TestCoverageLost are the TESTED_BY subset of the
	// above, hoisted because "this change lost a covering test" is a different
	// sentence from "this change lost an edge".
	TestCoverageGained int `json:"test_coverage_gained"`
	TestCoverageLost   int `json:"test_coverage_lost"`

	SymbolsAdded    int `json:"symbols_added"`
	SymbolsRemoved  int `json:"symbols_removed"`
	SymbolsMoved    int `json:"symbols_moved"`
	SymbolsOrphaned int `json:"symbols_orphaned"`

	EndpointsAdded          int `json:"endpoints_added"`
	EndpointsRemoved        int `json:"endpoints_removed"`
	EndpointsHandlerChanged int `json:"endpoints_handler_changed"`

	TablesChanged int `json:"tables_changed"`

	// Complete is false when a section was capped, which makes every count
	// above it a floor. The exact totals are in the omission block.
	Complete bool `json:"complete"`

	// Scope is the sentence that stops these being read as totals when they
	// are not.
	Scope string `json:"scope"`
}

// FactDiffCoverage is how much of the repository each side saw.
//
// Reported on every diff, not only when it moved. A section that shrank because
// the head run indexed four hundred fewer files is not a deletion, and those
// two render identically in a list of removals.
type FactDiffCoverage struct {
	BaseFiles   int64 `json:"base_files"`
	HeadFiles   int64 `json:"head_files"`
	FilesDelta  int64 `json:"files_delta"`
	BaseSymbols int64 `json:"base_symbols"`
	HeadSymbols int64 `json:"head_symbols"`
	SymbolDelta int64 `json:"symbols_delta"`
	BaseEdges   int64 `json:"base_edges"`
	HeadEdges   int64 `json:"head_edges"`
	EdgeDelta   int64 `json:"edges_delta"`

	BaseParseFailures int64 `json:"base_parse_failures"`
	HeadParseFailures int64 `json:"head_parse_failures"`

	// Detail says what the movement means for reading the sections.
	Detail string `json:"detail"`
}

// FactDiffResponse is the whole fact-diff screen.
type FactDiffResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Filters FactDiffFilters `json:"filters"`

	// Compared is false when the two sides could not be established. Every
	// section below is then empty for that reason and no other.
	Compared bool `json:"compared"`

	Base diffgraph.RunInfo `json:"base"`
	Head diffgraph.RunInfo `json:"head"`

	Counts   FactDiffCounts   `json:"counts"`
	Coverage FactDiffCoverage `json:"coverage"`

	// The diffgraph rows, with their own confidence and their own evidence
	// sites intact. Relationships excludes the TESTED_BY changes, which are
	// carried separately in TestCoverage — one population, two questions, and
	// the counts above keep them addable.
	Relationships []diffgraph.EdgeChange     `json:"relationships"`
	TestCoverage  []diffgraph.EdgeChange     `json:"test_coverage"`
	Symbols       []diffgraph.SymbolChange   `json:"symbols"`
	Endpoints     []diffgraph.EndpointChange `json:"endpoints"`
	Tables        []diffgraph.TableChange    `json:"tables"`

	// Caveats is diffgraph's own statement of what it could NOT compare.
	Caveats diffgraph.Caveats `json:"caveats"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

func (s *Server) handleFactDiff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	maxPerSection := intParam(r, "max_per_section", diffgraph.DefaultMaxPerSection, 1, maxRows)

	resp := FactDiffResponse{
		Meta: meta, Legend: Legend(),
		Relationships: []diffgraph.EdgeChange{}, TestCoverage: []diffgraph.EdgeChange{},
		Symbols: []diffgraph.SymbolChange{}, Endpoints: []diffgraph.EndpointChange{},
		Tables: []diffgraph.TableChange{},
		Filters: FactDiffFilters{
			FromRequested: strings.TrimSpace(r.URL.Query().Get("from")),
			ToRequested:   strings.TrimSpace(r.URL.Query().Get("to")),
			Defaulted:     []string{},
			MaxPerSection: maxPerSection,
			ProbeDepth:    diffProbeDepth,
		},
		// Stated before anything is compared, so an uncompared response never
		// ships a block of zeroes that reads as "nothing changed".
		Counts: FactDiffCounts{Scope: uncomparedScope},
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label:   "Index this repository first",
			Detail:  "A fact diff compares two runs. There is not yet one.",
			Command: "cortex index && cortex resolve",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	lookup, hasLookup := s.deps.Registry.(RunLookup)
	source, hasSource := s.deps.Registry.(DiffSource)
	if !hasLookup || !hasSource {
		resp.Help = &Guidance{
			Title: "This portal cannot read a second run",
			Detail: "A fact diff needs to open two runs of the index. The registry this portal was " +
				"given resolves only the committed one, so there is no base side to compare against " +
				"— a gap in this build's wiring, not a statement that nothing changed.",
			Steps: []string{"cortex diff --from <run> --to <run>"},
		}
		resp.Next = NextAction{Label: "Run the comparison from the command line",
			Command: "cortex diff --from 41 --to 42"}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	head, err := s.resolveDiffSide(ctx, lookup, resp.Filters.ToRequested, run.ID, &resp.Filters, "to")
	if err != nil {
		resp.Help = diffRefGuidance(resp.Filters.ToRequested, err)
		resp.Next = diffNextAction(resp)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Filters.To = head

	base, err := s.resolveDiffBase(ctx, lookup, resp.Filters.FromRequested, head, &resp.Filters)
	if err != nil {
		resp.Help = diffRefGuidance(resp.Filters.FromRequested, err)
		resp.Next = diffNextAction(resp)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Filters.From = base

	if base == head {
		resp.Help = &Guidance{
			Title: fmt.Sprintf("Both sides name run %d", head),
			Detail: "A run compared against itself reports nothing, by construction — identity here " +
				"is content-based, so two reads of one run cannot differ. This is not evidence that " +
				"the code has not changed.",
			Steps: []string{
				"Pass ?from= and ?to= naming two different runs.",
				"cortex index — index the current tree to produce a second run",
			},
		}
		resp.Next = diffNextAction(resp)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	differ, err := diffgraph.New(diffgraph.WithMaxPerSection(maxPerSection))
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: build differ: %w", err))
		return
	}
	rep, err := differ.Compare(ctx,
		diffgraph.Side{Source: source, RunID: base},
		diffgraph.Side{Source: source, RunID: head})
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: compare runs %d..%d: %w", base, head, err))
		return
	}

	var om Omissions
	resp.Compared = true
	resp.Base, resp.Head = rep.Base, rep.Head
	resp.Caveats = rep.Caveats
	resp.Coverage = diffCoverage(rep)

	for _, e := range rep.Edges {
		if e.Key.Kind == model.EdgeTestedBy {
			resp.TestCoverage = append(resp.TestCoverage, e)
			continue
		}
		resp.Relationships = append(resp.Relationships, e)
	}
	if rep.Symbols != nil {
		resp.Symbols = rep.Symbols
	}
	if rep.Endpoints != nil {
		resp.Endpoints = rep.Endpoints
	}
	if rep.Tables != nil {
		resp.Tables = rep.Tables
	}
	resp.Counts = diffCounts(resp, rep.Truncations)

	for _, t := range rep.Truncations {
		om.Hide(ReasonRowBudget, t.Omitted,
			fmt.Sprintf("The %s section holds %d change(s) and %d are listed, ordered by %s. "+
				"The counts on this screen are computed over the listed rows, so they are a LOWER BOUND "+
				"for this section.", t.Section, t.Total, t.Shown, t.OrderedBy),
			"raise ?max_per_section=, or read the whole report with `cortex diff --json`")
	}
	if !rep.Caveats.Comparable {
		om.Note(ReasonUnresolvedReferences,
			"The two runs are not cleanly comparable: "+strings.Join(rep.Caveats.Notes, " ")+
				" An absence below may be an indexing gap rather than a deletion.",
			"cortex doctor")
	}
	if rep.Caveats.UnreliableFiles > 0 {
		om.Hide(ReasonUnresolvedReferences, rep.Caveats.UnreliableFiles,
			"File(s) did not parse cleanly on one side, so every symbol and edge they would have "+
				"contributed is missing rather than deleted. A removal attributed to one of them is "+
				"not a removal.",
			"cortex doctor")
	}
	if rep.Caveats.DanglingEdges > 0 {
		om.Hide(ReasonUnresolvedReferences, rep.Caveats.DanglingEdges,
			"Edge(s) referenced a symbol that is not in the snapshot they belong to, so they could "+
				"not be keyed and are not compared.", "cortex doctor --verify-index")
	}

	resp.Next = diffNextAction(resp)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// resolveDiffSide turns a requested ref into a run id that exists.
//
// An empty ref takes the fallback and RECORDS that it did. An unmatched ref is
// an error, never a silent fallback: a diff against a run the caller did not
// name is the one failure that would make the whole report a lie.
func (s *Server) resolveDiffSide(
	ctx context.Context, lookup RunLookup, ref string, fallback int64,
	filters *FactDiffFilters, side string,
) (int64, error) {
	if ref == "" {
		filters.Defaulted = append(filters.Defaulted, side)
		filters.Detail = appendSentence(filters.Detail, fmt.Sprintf(
			"No ?%s= was given, so the head side defaulted to run %d — the committed run this "+
				"portal serves.", side, fallback))
		return fallback, nil
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("web: %q is not a run id: %w", ref, err)
	}
	if _, err := lookup.Run(ctx, id); err != nil {
		return 0, fmt.Errorf("web: run %d: %w", id, err)
	}
	return id, nil
}

// resolveDiffBase finds a base side, probing downward when none was named.
func (s *Server) resolveDiffBase(
	ctx context.Context, lookup RunLookup, ref string, head int64, filters *FactDiffFilters,
) (int64, error) {
	if ref != "" {
		return s.resolveDiffSide(ctx, lookup, ref, 0, filters, "from")
	}
	filters.Defaulted = append(filters.Defaulted, "from")
	for id := head - 1; id > 0 && id > head-int64(diffProbeDepth)-1; id-- {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("web: locate a base run: %w", err)
		}
		if _, err := lookup.Run(ctx, id); err == nil {
			filters.Detail = appendSentence(filters.Detail, fmt.Sprintf(
				"No ?from= was given, so the base side defaulted to run %d — the highest run id "+
					"below the head that this index still holds, found by asking for the %d ids "+
					"beneath it.", id, diffProbeDepth))
			return id, nil
		}
	}
	filters.Detail = appendSentence(filters.Detail, fmt.Sprintf(
		"No ?from= was given and none of the %d run ids below %d exists in this index.",
		diffProbeDepth, head))
	return 0, errNoBaseRun
}

// errNoBaseRun is the ordinary state of a healthy index, not a fault: committing
// a run retires the one before it, so one index file normally holds one run.
var errNoBaseRun = errors.New("web: this index holds no second run to compare against")

// uncomparedScope is what every count says when no comparison ran.
//
// Without it the block is thirteen zeroes, and thirteen zeroes are exactly what
// a diff that found no changes looks like. Those are opposite facts.
const uncomparedScope = "No comparison was run, so every count here is a placeholder rather than " +
	"a finding. A zero below is not \"this did not change\" — it is \"this was never compared\"."

func diffRefGuidance(ref string, err error) *Guidance {
	if errors.Is(err, errNoBaseRun) {
		return &Guidance{
			Title: "This index holds only one run, so there is nothing to compare",
			Detail: "That is the normal state of a healthy index rather than a fault: committing a " +
				"run retires and deletes the one before it, so one index file holds one committed " +
				"run. A fact diff needs a second one, which is why the two sides of a comparison " +
				"across commits usually live in two index files.",
			Steps: []string{
				"cortex index — index the current tree; the run it produces becomes the head side",
				"cortex diff --from-index ../release-1.4/.cortex/index.db --from <sha> --to <run>",
				"Then re-open this screen with ?from=<run>&to=<run>",
			},
		}
	}
	return &Guidance{
		Title:  fmt.Sprintf("No indexed run matches %q", ref),
		Detail: "A ref names a run that is ALREADY in this index. It never falls back to whatever happens to be committed, because a diff against the wrong run is worse than no diff at all. " + err.Error(),
		Steps: []string{
			"Pass a run id: ?from=41&to=42",
			"git checkout <ref> && cortex index — to compare against a commit this index has never seen",
		},
	}
}

// diffCoverage states how much of the repository each side saw.
func diffCoverage(rep *diffgraph.Report) FactDiffCoverage {
	c := FactDiffCoverage{
		BaseFiles: rep.Base.Files, HeadFiles: rep.Head.Files,
		BaseSymbols: rep.Base.Symbols, HeadSymbols: rep.Head.Symbols,
		BaseEdges: rep.Base.Edges, HeadEdges: rep.Head.Edges,
		BaseParseFailures: rep.Base.ParseFailures, HeadParseFailures: rep.Head.ParseFailures,
	}
	c.FilesDelta = c.HeadFiles - c.BaseFiles
	c.SymbolDelta = c.HeadSymbols - c.BaseSymbols
	c.EdgeDelta = c.HeadEdges - c.BaseEdges

	switch {
	case c.FilesDelta == 0 && c.HeadParseFailures == c.BaseParseFailures:
		c.Detail = fmt.Sprintf("Both runs covered %d file(s) and failed on the same number, so a "+
			"removal below is a removal rather than a file that stopped being indexed.", c.HeadFiles)
	case c.FilesDelta < 0:
		c.Detail = fmt.Sprintf("The head run covered %d FEWER file(s) than the base (%d against %d). "+
			"Some of the removals below are facts that were never re-indexed, not facts that were "+
			"deleted, and nothing in the sections themselves tells the two apart.",
			-c.FilesDelta, c.HeadFiles, c.BaseFiles)
	default:
		c.Detail = fmt.Sprintf("The head run covered %d MORE file(s) than the base (%d against %d), "+
			"so part of what reads as new below is code the base run never saw.",
			c.FilesDelta, c.HeadFiles, c.BaseFiles)
	}
	if c.HeadParseFailures > 0 {
		c.Detail += fmt.Sprintf(" %d file(s) failed to parse in the head run; every relationship "+
			"through one of them is missing rather than absent.", c.HeadParseFailures)
	}
	return c
}

// diffCounts tallies the sections actually returned.
func diffCounts(resp FactDiffResponse, truncs []diffgraph.Truncation) FactDiffCounts {
	c := FactDiffCounts{Complete: len(truncs) == 0}

	tally := func(rows []diffgraph.EdgeChange, gained, lost *int) {
		for _, e := range rows {
			switch e.Change {
			case diffgraph.EdgeAdded:
				*gained++
			case diffgraph.EdgeRemoved:
				*lost++
			case diffgraph.EdgeConfidenceChanged:
				c.RelationshipsRegraded++
				switch e.Direction {
				case diffgraph.Upgraded:
					c.Upgraded++
				case diffgraph.Downgraded:
					c.Downgraded++
				}
			}
		}
	}
	tally(resp.Relationships, &c.RelationshipsGained, &c.RelationshipsLost)
	tally(resp.TestCoverage, &c.TestCoverageGained, &c.TestCoverageLost)

	for _, sym := range resp.Symbols {
		switch sym.Change {
		case diffgraph.SymbolAdded:
			c.SymbolsAdded++
		case diffgraph.SymbolRemoved:
			c.SymbolsRemoved++
		case diffgraph.SymbolMoved:
			c.SymbolsMoved++
		case diffgraph.SymbolOrphaned:
			c.SymbolsOrphaned++
		}
	}
	for _, ep := range resp.Endpoints {
		switch ep.Change {
		case diffgraph.EndpointAdded:
			c.EndpointsAdded++
		case diffgraph.EndpointRemoved:
			c.EndpointsRemoved++
		case diffgraph.EndpointHandlerChanged:
			c.EndpointsHandlerChanged++
		}
	}
	c.TablesChanged = len(resp.Tables)

	if c.Complete {
		c.Scope = "Counted over every change in the comparison; nothing was capped."
	} else {
		c.Scope = "Counted over the rows on this response. At least one section was capped, so " +
			"these are LOWER BOUNDS — the exact per-section totals are under what this view can't see."
	}
	return c
}

func diffNextAction(resp FactDiffResponse) NextAction {
	switch {
	case !resp.Compared:
		return NextAction{
			Label:   "Produce a second run to compare against",
			Detail:  "The fact diff is the only view here that reports a change rather than a state.",
			Command: "cortex index",
		}
	case resp.Counts.Downgraded > 0:
		return NextAction{
			Label: fmt.Sprintf("%d relationship(s) were DOWNGRADED", resp.Counts.Downgraded),
			Detail: "A compiler stopped proving something it used to prove. Every consumer filtering " +
				"on the stronger tier silently lost those edges, and no line-based diff can see it.",
			Screen: "graph",
		}
	case resp.Counts.TestCoverageLost > 0:
		return NextAction{
			Label: fmt.Sprintf("%d covering-test relationship(s) were lost", resp.Counts.TestCoverageLost),
			Detail: "Code that was reachable from a test in the base run is not reachable from one in " +
				"the head run. Open the blast radius for the symbols involved before shipping.",
			Screen: "blast",
		}
	case resp.Counts.RelationshipsLost > 0:
		return NextAction{
			Label:  fmt.Sprintf("%d relationship(s) were lost", resp.Counts.RelationshipsLost),
			Detail: "Read them against the coverage block above: a run that indexed fewer files loses relationships it never re-read.",
			Screen: "graph",
		}
	default:
		return NextAction{
			Label:  "Read the caveats before reading the absences",
			Detail: "A degraded run, a file that did not parse, or two runs in different accuracy modes all mean an absence may be an indexing gap rather than a deletion.",
			Screen: "overview",
		}
	}
}

// appendSentence joins two sentences without producing a leading space.
func appendSentence(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + " " + add
}
