package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Architecture governance: does the code match the architecture we said we had?
//
// The check is capability/architecture's, unchanged — including its refusal to
// report anything when no components are declared. "0 violations" for a
// repository that declared no architecture is a pass mark nobody earned, and
// rendering one on a dashboard is how a governance screen becomes decorative.
//
// What this handler adds is the CI gate projection: for each severity a pipeline
// could gate on, whether `repocortex arch --fail-on <severity>` would fail right
// now. That turns a list of findings into the one thing a team acts on — a
// decision about whether to turn the gate on — and it is computed from the same
// severities the command uses, not from a second table that could drift.

// GateOutcome is what a CI gate at one severity would do.
type GateOutcome struct {
	FailOn  string `json:"fail_on"`
	Passes  bool   `json:"passes"`
	Command string `json:"command"`
	Detail  string `json:"detail"`
}

// GovernanceResponse is the governance screen.
type GovernanceResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Declared is false when the repository declares no architecture. The whole
	// screen then renders guidance instead of a green tick.
	Declared bool      `json:"architecture_declared"`
	Help     *Guidance `json:"guidance,omitempty"`

	// Components is capability/architecture's summary, re-projected so the two
	// file counts are NAMED. See ComponentRow.
	Components []ComponentRow           `json:"components"`
	Crossings  []architecture.Crossing  `json:"crossings"`
	Violations []architecture.Violation `json:"violations"`

	// EmptyComponents counts declared components that matched no file. The most
	// useful single number on this screen: a component matching nothing means
	// every rule naming it has been silently inert.
	EmptyComponents int `json:"empty_components"`

	Unassigned      int `json:"unassigned_symbols"`
	EdgesConsidered int `json:"edges_considered"`
	RulesEvaluated  int `json:"rules_evaluated"`

	// ViolationsByConfidence is the spread that decides whether a violation
	// list is a fact sheet or a lead list. A violation on a compiler-proven edge
	// is a fact; one inferred from a `low` heuristic is a lead, and presenting
	// them identically makes the report either alarmist or ignorable.
	ViolationsByConfidence Histogram `json:"violations_by_confidence"`

	WorstSeverity string        `json:"worst_severity,omitempty"`
	Gates         []GateOutcome `json:"ci_gates"`
	Filters       GovFilters    `json:"filters"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// ComponentRow is one declared component with BOTH of its sizes, named.
//
// # The defect this type exists to close
//
// The graph explorer showed `task-app · 551 files`. This screen showed 3,425
// for the same component, in the same session, against the same index, with
// nothing on either screen to reconcile them. Neither number was wrong:
// capability/architecture counts every file a component's include patterns
// MATCH, and the explorer counted the files that produced a symbol. Both were
// labelled "files". A reader could not trust either while both were on screen,
// and in a measurement product that is the most expensive kind of defect there
// is — it costs the credibility of every other number on the page.
//
// Picking one and deleting the other would have been the cheap fix and the
// wrong one: an architecture rule is written against matched paths, and a graph
// can only draw symbol-bearing files, so both populations are load-bearing. So
// both are reported, at every screen that reports either, under names that say
// which is which, computed in ONE place — structuralIndex, which resolves each
// file to its container exactly once — so they cannot drift apart again.
type ComponentRow struct {
	Name    string `json:"name"`
	Symbols int    `json:"symbols"`

	// FilesMatched is every file this component's include patterns select,
	// symbol-bearing or not: READMEs, YAML, generated JSON, fixtures. The
	// population an architecture rule is written over.
	FilesMatched int `json:"files_matched"`

	// FilesWithSymbols is the subset that produced at least one symbol: the
	// population the graph can draw and every edge in this report comes from.
	FilesWithSymbols int `json:"files_with_symbols"`

	// FilesWithoutSymbols is the difference, stated rather than left to be
	// subtracted. A component that matches 3,425 files and extracts symbols
	// from 551 of them is 84% invisible to every rule on this screen, and that
	// is the finding.
	FilesWithoutSymbols int `json:"files_without_symbols"`

	// Empty marks a component that matched no file at all — the single most
	// useful row on this screen, because every rule naming it is inert.
	Empty bool `json:"empty,omitempty"`
}

// componentRows joins architecture's summary to the structural index's file
// resolution. Ordered by name so two runs serialise identically.
func componentRows(summaries []architecture.ComponentSummary, idx *structuralIndex) []ComponentRow {
	out := make([]ComponentRow, 0, len(summaries))
	for _, c := range summaries {
		row := ComponentRow{
			Name: c.Name, Symbols: c.Symbols,
			FilesMatched: c.Files,
		}
		if idx != nil {
			if ci, ok := idx.containers[c.Name]; ok {
				row.FilesWithSymbols = len(ci.SymbolFiles)
				// The same resolver, the same run, the same globs: when the
				// structural index also matched this component, ITS count of
				// matched files is the authoritative one, because it is the one
				// the explorer draws from. Taking architecture's figure here
				// instead is how the two screens came to disagree.
				row.FilesMatched = ci.AllFiles
			}
		}
		row.FilesWithoutSymbols = row.FilesMatched - row.FilesWithSymbols
		if row.FilesWithoutSymbols < 0 {
			row.FilesWithoutSymbols = 0
		}
		row.Empty = row.Symbols == 0 && row.FilesMatched == 0
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GovFilters echoes the applied confidence floor.
type GovFilters struct {
	MinConfidence string `json:"min_confidence"`
}

func (s *Server) handleGovernance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp := GovernanceResponse{
		Meta: meta, Legend: Legend(),
		Components: []ComponentRow{},
		Crossings:  []architecture.Crossing{},
		Violations: []architecture.Violation{},
		Gates:      []GateOutcome{},
	}
	if !ok {
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// Unset by default rather than DefaultFloor: capability/architecture reports
	// everything and carries the confidence on each violation, because a reader
	// filtering for themselves is better served than one shown a filtered list
	// they did not choose. The portal keeps that behaviour and lets the reader
	// raise the floor explicitly.
	floor := model.Confidence("")
	if raw := r.URL.Query().Get("min_confidence"); raw != "" {
		floor = confidenceParam(r, "min_confidence", "")
	}
	resp.Filters = GovFilters{MinConfidence: string(floor)}

	rep, err := architecture.Check(ctx, s.deps.Registry, run.ID, s.deps.Config.Architecture, floor)
	if errors.Is(err, architecture.ErrNoModel) {
		resp.Declared = false
		resp.Help = &Guidance{
			Title: "This repository declares no architecture",
			Detail: "Components are DECLARED, never inferred. A component model RepoCortex guessed would be a " +
				"description of the code's current shape, and checking code against a description of itself " +
				"finds nothing by construction — so this screen reports nothing rather than reporting a pass.",
			Steps: []string{
				"Add an `architecture:` block to .repocortex.yml naming your components and the rules between them.",
				"repocortex arch --help  — the block's shape, with a worked example",
				"repocortex arch --fail-on high  — the CI gate, once the rules exist",
			},
		}
		resp.Next = NextAction{
			Label:   "Declare components in .repocortex.yml",
			Detail:  "Until then, the graph explorer groups by directory, and no rule can be broken because no rule exists.",
			Command: "repocortex arch --help",
			Screen:  "graph",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	resp.Declared = true
	resp.Unassigned = rep.Unassigned
	resp.EdgesConsidered = rep.EdgesConsidered
	resp.RulesEvaluated = rep.RulesEvaluated
	resp.WorstSeverity = rep.HighestSeverity()

	// The structural index resolves every file to its container once. Reading
	// the file counts off it is what makes this screen and the graph explorer
	// agree by construction rather than by coincidence.
	idx, ierr := s.structural(ctx, run.ID)
	if ierr != nil {
		s.fail(w, r, ierr)
		return
	}
	rows := componentRows(rep.Components, idx)
	for _, c := range rows {
		if c.Symbols == 0 {
			resp.EmptyComponents++
		}
	}
	for _, v := range rep.Violations {
		resp.ViolationsByConfidence.add(model.Confidence(v.Confidence))
	}

	resp.Components = capped(rows, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Components beyond the first %d (alphabetical) are not listed.", maxRows), "")
	resp.Crossings = capped(rep.Crossings, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Component-to-component dependencies beyond the first %d (violations first) are not listed.", maxRows),
		"raise the standard of proof to see only the crossings worth acting on")
	resp.Violations = capped(rep.Violations, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Violations beyond the first %d (worst severity first) are not listed.", maxRows), "")

	// The coverage caveat, as a counted omission rather than a footnote. A
	// forbidden dependency routed through an unassigned symbol is invisible to
	// every rule on this screen, which makes every rule weaker than it looks.
	om.Hide(ReasonOutsideDeclaredComponents, rep.Unassigned,
		"Symbols no declared component matches. No rule was evaluated against them, so a forbidden "+
			"dependency routed through one of them does not appear above.",
		"widen your component include patterns in .repocortex.yml")
	if floor != "" {
		om.Hide(ReasonBelowConfidenceFloor, idx.edgeConf.Below(floor),
			fmt.Sprintf("Relationships weaker than %s were excluded from this check entirely, so a "+
				"boundary crossing recorded only on those edges is not reported.", tierWord(floor)),
			"clear the standard of proof to check every recorded relationship")
	}

	// Files a component matched but nothing was extracted from. Counted here
	// because it bounds every rule on this screen: a forbidden dependency
	// written in a file the parser produced no symbol for cannot be seen.
	invisible := 0
	for _, c := range rows {
		invisible += c.FilesWithoutSymbols
	}
	om.Hide(ReasonUnresolvedReferences, invisible,
		"Files inside a declared component that produced no symbol — configuration, documentation, "+
			"generated output, or a language this build cannot parse. No edge can come from them, so no "+
			"rule was evaluated over them. This is the difference between a component's files_matched and "+
			"its files_with_symbols, and it is why the two are reported separately rather than as one "+
			"number called `files`.",
		"repocortex indexers status — a parser for the missing language raises this coverage")

	// The CI gate projection, in the same order and with the same semantics as
	// `repocortex arch --fail-on`.
	for _, sev := range []string{"high", "medium", "low"} {
		worst := rep.HighestSeverity()
		fails := worst != "" && severityRank(worst) <= severityRank(sev)
		detail := fmt.Sprintf("No violation at %q or worse.", sev)
		if fails {
			detail = fmt.Sprintf("%d violation(s), worst severity %q — this gate would fail the build.",
				len(rep.Violations), worst)
		}
		resp.Gates = append(resp.Gates, GateOutcome{
			FailOn: sev, Passes: !fails,
			Command: "repocortex arch --fail-on " + sev,
			Detail:  detail,
		})
	}

	switch {
	case resp.EmptyComponents > 0:
		resp.Next = NextAction{
			Label: fmt.Sprintf("%d component(s) matched nothing", resp.EmptyComponents),
			Detail: "A component whose include patterns match no file is a component every rule naming it " +
				"silently ignores. This is the single most useful finding on this screen.",
			Command: "repocortex arch --crossings",
		}
	case len(rep.Violations) > 0:
		resp.Next = NextAction{
			Label: fmt.Sprintf("Turn on the gate at %q", rep.HighestSeverity()),
			Detail: "Fix the violations first, then wire the command into CI so the next one fails the build " +
				"rather than landing. Read the confidence on each row: a violation on a compiler-proven edge " +
				"is a fact, one on a `low` edge is a lead.",
			Command: "repocortex arch --fail-on " + rep.HighestSeverity(),
		}
	case rep.Unassigned > 0:
		resp.Next = NextAction{
			Label:   fmt.Sprintf("%d symbol(s) are outside every component", rep.Unassigned),
			Detail:  "No rule was checked against them. Widen the include patterns until this number is small.",
			Command: "repocortex arch --crossings",
		}
	default:
		resp.Next = NextAction{
			Label:   "Wire the gate into CI",
			Detail:  fmt.Sprintf("%d rule(s) evaluated, no violations. Keep it that way.", rep.RulesEvaluated),
			Command: "repocortex arch --fail-on high",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// severityRank orders severities so a gate at "medium" also fails on "high".
// Mirrors internal/cli's own ranking; kept here so this package does not reach
// into a sibling for three lines and does not depend on the CLI.
func severityRank(s string) int {
	switch s {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	}
	return 3
}
