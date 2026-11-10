package web

import (
	"fmt"
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The overview answers one question: how much of this repository does RepoCortex
// actually understand, and what should I do about the part it does not?
//
// Every section therefore ends in a NextAction. A dashboard that reports a
// number and stops has told the reader they have a problem without telling them
// what to do, which is how a coverage figure becomes wallpaper. The actions are
// concrete commands or a named screen, never "consider reviewing".

// NextAction is the concrete thing to do about a section's finding.
type NextAction struct {
	Label  string `json:"label"`
	Detail string `json:"detail"`

	// Command is a shell command that changes the number above it.
	Command string `json:"command,omitempty"`

	// Screen names a portal view that goes deeper: graph | blast | governance.
	Screen string `json:"screen,omitempty"`
}

// Fingerprint is the repository at a glance.
type Fingerprint struct {
	Files   int64 `json:"files"`
	Symbols int64 `json:"symbols"`
	Edges   int64 `json:"edges"`

	Exported   int `json:"exported_symbols"`
	Containers int `json:"containers"`
	// ContainerKind is "component" when the repository declares an
	// architecture and "directory" when it does not. Stated, because a reader
	// must not mistake a directory grouping for a declared one.
	ContainerKind string `json:"container_kind"`

	Endpoints int `json:"endpoints"`
	Tables    int `json:"tables"`

	Next NextAction `json:"next_action"`
}

// LanguageSection is the language mix.
type LanguageSection struct {
	Mix  []LanguageMix `json:"mix"`
	Next NextAction    `json:"next_action"`
}

// ResolutionSection is how much of the repository's references were bound.
//
// THE RATE IS OVER Bindable, NOT OVER Total, and the portal must agree with the
// CLI about that or a user reading 48% in the terminal and 19% in the browser
// has no way to tell which is broken. See registry.ResolutionStats.BindingRate:
// a reference that matched no definition in this index cannot be bound by any
// improvement to binding, so counting it as a failure turns a description of
// how much library code a project calls into what reads as a defect rate.
//
// resolved_pct WAS that rate and is deliberately GONE rather than left beside
// the new one. Two percentages for one fact in one payload is how the page ends
// up rendering the wrong one again, and the raw counts below let any consumer
// compute either.
type ResolutionSection struct {
	Total      int64 `json:"total_references"`
	Resolved   int64 `json:"resolved"`
	Ambiguous  int64 `json:"ambiguous"`
	Unresolved int64 `json:"unresolved"`
	Pending    int64 `json:"pending"`
	Ignored    int64 `json:"ignored"`

	// Bindable is the references that name something this index defines:
	// bound, plus too ambiguous to choose between. BoundPct is Resolved over
	// it. Unresolved is rendered beside them, never folded away — the exclusion
	// is only honest while the excluded population is on screen.
	Bindable int64   `json:"bindable_references"`
	BoundPct float64 `json:"bound_pct"`
	// ByCompiler is how many of Resolved a compiler index settled rather than
	// the heuristic resolver. Different claims, different failure modes.
	ByCompiler int64 `json:"bound_by_compiler"`

	// Caveat states what an unresolved reference means for every other number
	// on the page, in the same words the capability layer uses.
	Caveat string     `json:"caveat"`
	Next   NextAction `json:"next_action"`
}

// ConfidenceSection is the stacked bar: every edge in the index by tier.
type ConfidenceSection struct {
	Edges Histogram `json:"edges"`

	// Actionable is the count at `high` or better — the number that should be
	// read as "relationships this index can support a decision with".
	Actionable    int     `json:"actionable"`
	ActionablePct float64 `json:"actionable_pct"`

	ByKind []KindCoverage `json:"by_kind"`
	Next   NextAction     `json:"next_action"`
}

// BlindSpotSection is where the index cannot see.
type BlindSpotSection struct {
	Spots []BlindSpot `json:"spots"`
	Blind int         `json:"blind_count"`
	Weak  int         `json:"weak_count"`
	Next  NextAction  `json:"next_action"`
}

// FreshnessSection is how current the index is.
type FreshnessSection struct {
	AgeSeconds  int64  `json:"age_seconds"`
	AgeHuman    string `json:"age_human"`
	Stale       bool   `json:"stale"`
	GitRevision string `json:"git_revision,omitempty"`

	// Coverage is what the run itself said about its completeness: a partial
	// run is a different fact from a degraded one, and both must be visible.
	IndexState       string `json:"index_state"`
	FilesDiscovered  int64  `json:"files_discovered"`
	FilesIndexed     int64  `json:"files_indexed"`
	FilesUnaccounted int64  `json:"files_unaccounted"`
	ParseFailures    int64  `json:"parse_failures"`

	Next NextAction `json:"next_action"`
}

// OverviewResponse is the whole first screen.
type OverviewResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Fingerprint Fingerprint       `json:"fingerprint"`
	Languages   LanguageSection   `json:"languages"`
	Resolution  ResolutionSection `json:"resolution"`
	Confidence  ConfidenceSection `json:"confidence"`
	BlindSpots  BlindSpotSection  `json:"blind_spots"`
	Freshness   FreshnessSection  `json:"freshness"`

	Omissions Omissions `json:"omissions"`
}

// staleAfter is when an index stops being a description of the working tree.
//
// A day, not an hour: an index is rebuilt by a person or a watcher, and warning
// every hour would train the reader to ignore the warning. The number is stated
// in the response so the threshold is visible rather than implied.
const staleAfterSeconds = 24 * 60 * 60

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusOK, OverviewResponse{Meta: meta, Legend: Legend()})
		return
	}

	reg := s.deps.Registry
	counts, err := reg.Counts(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stats, err := reg.ResolutionStats(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	endpoints, err := reg.EndpointReport(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tables, err := reg.TableUsageReport(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	resp := OverviewResponse{Meta: meta, Legend: Legend()}

	// ---- fingerprint
	exported := 0
	for _, c := range idx.containers {
		exported += c.Exported
	}
	resp.Fingerprint = Fingerprint{
		Files: counts.Files, Symbols: counts.Symbols, Edges: counts.Edges,
		Exported: exported, Containers: len(idx.containers), ContainerKind: idx.containerKind,
		Endpoints: len(endpoints), Tables: len(tables),
		Next: NextAction{
			Label:  "Open the graph explorer",
			Detail: fmt.Sprintf("Walk the %d %s(s) this index groups the repository into.", len(idx.containers), idx.containerKind),
			Screen: "graph",
		},
	}
	if idx.containerKind == "directory" {
		resp.Fingerprint.Next = NextAction{
			Label: "Declare your components",
			Detail: "Containers here are DIRECTORIES, not architecture. Declaring components in " +
				".cortex.yml is what turns the explorer into a picture of the system you meant to " +
				"build, and it is what makes the governance screen able to say anything at all.",
			Command: "cortex arch --help",
			Screen:  "governance",
		}
	}

	// ---- languages
	mix := idx.languageMix()
	resp.Languages = LanguageSection{
		Mix: capped(mix, 40, &om, ReasonRowBudget,
			"Languages beyond the 40 largest are not listed.", "no control; the tail is negligible by file count"),
		Next: NextAction{
			Label:  "Check the blind spots for your main language",
			Detail: "A language with many symbols and few actionable edges is where this index will quietly answer 'nothing found'.",
			Screen: "overview#blind-spots",
		},
	}

	// ---- resolution
	unaccounted := stats.Unresolved + stats.Ambiguous + stats.Pending
	resp.Resolution = ResolutionSection{
		Total: stats.Total, Resolved: stats.Resolved, Ambiguous: stats.Ambiguous,
		Unresolved: stats.Unresolved, Pending: stats.Pending, Ignored: stats.Ignored,
		Bindable: stats.Bindable(), BoundPct: round1(100 * stats.BindingRate()),
		ByCompiler: stats.ByCompiler,
	}
	switch {
	case stats.Total == 0:
		resp.Resolution.Caveat = "This run recorded no references at all, so resolution coverage is " +
			"UNKNOWN rather than complete. A 0/0 must not render as a reassuring 100%."
		resp.Resolution.Next = NextAction{
			Label:   "Resolve references",
			Detail:  "Without resolved references there are no relationship edges, and every other screen is empty for that reason rather than because the repository has no relationships.",
			Command: "cortex resolve",
		}
	case stats.Pending == stats.Total:
		resp.Resolution.Caveat = "Every reference in this run is still PENDING: they were extracted but never bound to a definition."
		resp.Resolution.Next = NextAction{
			Label: "Resolve references", Command: "cortex resolve",
			Detail: "The extraction half is done; the binding half has not run.",
		}
	default:
		// The rate and the lower bound are DIFFERENT claims over different
		// populations, and both are true, so both are stated with their own
		// denominator rather than one being derived from the other by
		// subtraction. The rate answers "of what we could have bound, how much
		// did we"; the lower bound answers "how many relationships might be
		// missing from this portal", and for that second question an
		// unresolved reference counts — 119 of a real repository's 324,342 of
		// them turned out to be in-repository code once a compiler was asked.
		resp.Resolution.Caveat = fmt.Sprintf(
			"%d of the %d reference(s) that name something this index defines are bound to a "+
				"definition (%.1f%%)%s; %d remain too ambiguous to choose between. A further %d "+
				"matched no definition here at all — mostly calls into libraries, which an index "+
				"of this repository cannot bind, though some are code this index did not cover. "+
				"All %d unbound references are relationships that could be missing from every "+
				"view in this portal, which is what makes each answer here a LOWER BOUND rather "+
				"than a complete picture.",
			stats.Resolved, stats.Bindable(), 100*stats.BindingRate(),
			compilerShare(stats), stats.Ambiguous, stats.Unresolved, unaccounted)
		resp.Resolution.Next = NextAction{
			Label:   "Raise coverage with a compiler-backed index",
			Detail:  "A SCIP index turns relationships inferred from names into proven ones — the only tier a compiler stands behind.",
			Command: "cortex scip import <index.scip>",
		}
	}
	// The resolution gap is not a filter and not a bound, but it IS something
	// every view leaves out, so it is stated in the omission block too rather
	// than only in prose a reader can skip.
	if unaccounted > 0 {
		om.Hide(ReasonUnresolvedReferences, int(unaccounted),
			"References the index never bound to any definition. Each is a use of a name that could "+
				"have been an edge in these views and is not.",
			"cortex resolve, or import a compiler-backed SCIP index")
	}

	// ---- confidence distribution
	actionable := idx.edgeConf.AtOrAbove(model.ConfidenceHigh)
	pct := 0.0
	if idx.edgeConf.Total > 0 {
		pct = 100 * float64(actionable) / float64(idx.edgeConf.Total)
	}
	resp.Confidence = ConfidenceSection{
		Edges: idx.edgeConf, Actionable: actionable, ActionablePct: round1(pct),
		ByKind: capped(idx.kindCoverage(), 25, &om, ReasonRowBudget,
			"Edge kinds beyond the 25 most numerous are not broken out.", "no control; the tail is negligible"),
		Next: NextAction{
			Label: "Read the tiers as measured precision, not as adjectives",
			Detail: fmt.Sprintf(
				"%d of %d edges (%.1f%%) are very likely or better. A very likely edge is measured 87–96%% right, "+
					"an uncertain one 6–38%% and a guess under 3%% (docs/accuracy.md, 30,975 edges checked against a compiler), which is why this "+
					"portal draws them dashed and ghosted rather than as lines you can follow.",
				actionable, idx.edgeConf.Total, round1(pct)),
			Screen: "graph",
		},
	}
	// `low` is off by default in every view. That exclusion is counted here so
	// the default never reads as "this repository has no weak edges".
	om.Hide(ReasonBelowConfidenceFloor, idx.edgeConf.Below(DefaultFloor),
		fmt.Sprintf("Edges weaker than %s are not drawn by default. They are still in the index; "+
			"at 0.0–2.8%% measured precision most of them name a relationship that does not exist — "+
			"most, not all, which is why the count is stated rather than the edges quietly dropped.",
			tierWord(DefaultFloor)),
		"set the standard of proof to `low` in any view")

	// ---- blind spots
	spots := idx.blindSpots()
	blind, weak := 0, 0
	for _, sp := range spots {
		if sp.Severity == "blind" {
			blind++
		} else {
			weak++
		}
	}
	resp.BlindSpots = BlindSpotSection{
		Spots: capped(spots, 60, &om, ReasonRowBudget,
			"Blind spots beyond the first 60 (language, kind) pairs are not listed.",
			"query a specific language with the graph explorer's language filter"),
		Blind: blind, Weak: weak,
		Next: NextAction{
			Label:   "Install a compiler-backed indexer for the blindest language",
			Detail:  "A (language, kind) pair with zero edges is not a repository without those relationships; it is an index that cannot see them.",
			Command: "cortex indexers status",
		},
	}

	// ---- freshness
	age := meta.Run.AgeSeconds
	resp.Freshness = FreshnessSection{
		AgeSeconds: age, AgeHuman: humanAge(age), Stale: age > staleAfterSeconds,
		GitRevision:      run.GitRevision,
		IndexState:       string(run.IndexState),
		FilesDiscovered:  run.FilesDiscovered,
		FilesIndexed:     run.FilesIndexed,
		FilesUnaccounted: run.FilesUnaccounted,
		ParseFailures:    run.ParseFailures,
	}
	switch {
	case !run.Complete():
		resp.Freshness.Next = NextAction{
			Label: "This index is PARTIAL", Command: "cortex index",
			Detail: fmt.Sprintf(
				"%d discovered file(s) reached none of the three terminal outcomes. The run was published "+
					"anyway — a partial index beats none — but it must not be read as a full picture.",
				run.FilesUnaccounted),
		}
	case age > staleAfterSeconds:
		resp.Freshness.Next = NextAction{
			Label: "Reindex", Command: "cortex index",
			Detail: fmt.Sprintf("This index is %s old. Everything in this portal describes the tree as it was then.", humanAge(age)),
		}
	default:
		resp.Freshness.Next = NextAction{
			Label: "Keep it current while you work", Command: "cortex watch",
			Detail: "Incremental reindexing on file change, so the portal keeps describing the tree in front of you.",
		}
	}
	if run.FilesUnaccounted > 0 {
		om.Hide(ReasonRowBudget, int(run.FilesUnaccounted),
			"Discovered files that reached no terminal outcome in this run. Their symbols and edges are absent from every view here.",
			"cortex index")
	}
	if run.ParseFailures > 0 {
		om.Hide(ReasonRowBudget, int(run.ParseFailures),
			"Files that failed or only partly parsed. Symbols they define are missing, so relationships through them are missing too.",
			"cortex doctor")
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

func humanAge(sec int64) string {
	switch {
	case sec < 0:
		return "just now"
	case sec < 90:
		return fmt.Sprintf("%d second(s)", sec)
	case sec < 90*60:
		return fmt.Sprintf("%d minute(s)", sec/60)
	case sec < 48*3600:
		return fmt.Sprintf("%d hour(s)", sec/3600)
	default:
		return fmt.Sprintf("%d day(s)", sec/86400)
	}
}

// handleHealth reports that the process is up and which index it is serving.
//
// Separate from /api/overview because a reader hitting a blank page needs to
// know whether the server is broken or the index is absent, and those must not
// share a response.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	meta, _, _, err := s.run(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		// "status" is the PROCESS's health, and it is "ok" by the fact that
		// this handler ran. The INDEX's health is a different question with a
		// different answer, and conflating them is how a degraded run came to
		// present as a healthy one: this endpoint returned status ok beside a
		// run whose own status was "degraded".
		"status": "ok",
		// Reality, not aspiration. A portal started with --allow-refresh serves
		// a route that writes, and an operator checking this endpoint to decide
		// whether it is safe to leave running must not be told otherwise.
		"read_only":      s.reindexer == nil,
		"index_health":   meta.Health,
		"meta":           meta,
		"default_floor":  string(DefaultFloor),
		"bound_to_local": !s.allowRemoteHosts,
	})
}

// compilerShare names how many references a compiler index settled, or nothing
// at all when none did.
//
// A clause rather than a separate sentence because it belongs to the number it
// qualifies: "bound to a definition" covers two claims of very different
// strength — a typechecker's answer, and name matching plus an import
// heuristic — and a reader deciding whether to trust an empty view needs them
// distinguished at the point the rate is stated, not further down.
func compilerShare(stats registry.ResolutionStats) string {
	if stats.ByCompiler == 0 {
		return ""
	}
	return fmt.Sprintf(", %d of them by an imported compiler index", stats.ByCompiler)
}
