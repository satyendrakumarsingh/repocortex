package mcp

import (
	stdctx "context"
	"fmt"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// The two notes below turn an imported coverage profile (`repocortex coverage
// import`) into a sentence on analyze_impact and find_tests.
//
// WHAT A PROFILE CAN AND CANNOT SAY. Go coverprofile, LCOV, Cobertura and
// JaCoCo count how often each line ran over the WHOLE run; none records which
// test ran it. So these notes say "executed" or "never executed in that run"
// and never re-rank tests or name the test that covers a line.
//
// SILENCE WITHOUT A PROFILE. Nothing is added when none is stored: a note on
// every answer saying "no coverage imported" would be noise for the majority
// of repositories that never import one.

// coverageImpactBudget bounds how many impacted symbols are looked up. A hub
// symbol can reach tens of thousands; the lookup is chunked, and the note is
// about the nearest ones, which the report lists first.
const coverageImpactBudget = 2000

// coverageStamp says which code the numbers describe: the commit at import and
// the day, e.g. "at 087b9cb on 2026-11-17".
func coverageStamp(p *registry.CoverageProfile) string {
	when := time.Unix(p.ImportedUnix, 0).UTC().Format("2006-01-02")
	if p.GitRevision == "" {
		return "on " + when
	}
	rev := p.GitRevision
	if len(rev) > 7 {
		rev = rev[:7]
	}
	return "at " + rev + " on " + when
}

// impactCoverageNotes words the target's covered share and the impacted
// callers that no test run executed.
func (s *Server) impactCoverageNotes(ctx stdctx.Context, runID int64, rep impact.Report) []string {
	if s.deps.Registry == nil {
		return nil
	}
	prof, err := s.deps.Registry.CoverageProfile(ctx, runID)
	if err != nil || prof == nil {
		return nil
	}
	stamp := coverageStamp(prof)

	type who struct {
		id   int64
		name string
	}
	var callers []who
	seen := map[int64]bool{rep.Target.SymbolID: true}
	for _, group := range [][]impact.Impacted{rep.Direct, rep.Indirect} {
		for _, c := range group {
			if c.IsTest || seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			callers = append(callers, who{c.ID, c.Name})
		}
	}
	truncated := len(callers) > coverageImpactBudget
	if truncated {
		callers = callers[:coverageImpactBudget]
	}
	ids := make([]int64, 0, len(callers)+1)
	ids = append(ids, rep.Target.SymbolID)
	for _, c := range callers {
		ids = append(ids, c.id)
	}
	cov, err := s.deps.Registry.SymbolCoverage(ctx, ids)
	if err != nil {
		return nil
	}

	var notes []string
	t := cov[rep.Target.SymbolID]
	switch t.State {
	case registry.CoverageMeasured:
		notes = append(notes, fmt.Sprintf(
			"Coverage: %s: %d of %d instrumented lines covered in the coverage imported %s.",
			rep.Target.Name, t.Covered, t.Total, stamp))
	case registry.CoverageStale:
		notes = append(notes, fmt.Sprintf(
			"Coverage: STALE for %s: its file changed after the coverage imported %s, so those numbers "+
				"are not used. Re-run the tests and `repocortex coverage import` again.", rep.Target.Name, stamp))
	case registry.CoverageUnmeasured:
		notes = append(notes, fmt.Sprintf(
			"Coverage: no line of %s is instrumented in the coverage imported %s.", rep.Target.Name, stamp))
	case registry.CoverageNotInProfile:
		notes = append(notes, fmt.Sprintf(
			"Coverage: %s's file is not in the coverage imported %s, so nothing is known about it "+
				"(a package no test was run for looks the same).", rep.Target.Name, stamp))
	}

	var bare []string
	stale, measured := 0, 0
	for _, c := range callers {
		switch cov[c.id].State {
		case registry.CoverageStale:
			stale++
		case registry.CoverageMeasured:
			measured++
			if cov[c.id].Covered == 0 {
				bare = append(bare, c.name)
			}
		}
	}
	if len(bare) > 0 {
		shown := bare
		more := ""
		if len(shown) > 5 {
			shown, more = shown[:5], fmt.Sprintf(" and %d more", len(bare)-5)
		}
		notes = append(notes, fmt.Sprintf(
			"Coverage: %d of %d measured impacted caller(s) have NO covered lines in that run: %s%s.",
			len(bare), measured, strings.Join(shown, ", "), more))
	}
	if stale > 0 {
		notes = append(notes, fmt.Sprintf(
			"Coverage: %d impacted caller(s) are in files that changed since the import and are not counted.", stale))
	}
	if truncated {
		notes = append(notes, fmt.Sprintf(
			"Coverage was read for the first %d impacted symbols only.", coverageImpactBudget))
	}
	return notes
}

// testsCoverageNote says when the imported run never executed the target.
//
// It is said "whatever the graph found": a test the graph links to the target
// may be skipped, may not have been part of the run that produced the profile,
// or may reach the target through a call the graph guessed. The list is not
// re-ranked by it: the profile has no per-test data to rank with.
func (s *Server) testsCoverageNote(ctx stdctx.Context, runID, symbolID int64, name string) string {
	if s.deps.Registry == nil {
		return ""
	}
	prof, err := s.deps.Registry.CoverageProfile(ctx, runID)
	if err != nil || prof == nil {
		return ""
	}
	cov, err := s.deps.Registry.SymbolCoverage(ctx, []int64{symbolID})
	if err != nil {
		return ""
	}
	c := cov[symbolID]
	if c.State != registry.CoverageMeasured || c.Covered != 0 || c.Total == 0 {
		return ""
	}
	return fmt.Sprintf(
		"Coverage: no test executed %s in the coverage imported %s (0 of %d instrumented lines ran), "+
			"whatever the graph found. A coverage profile holds no per-test data, so the tests above are "+
			"not re-ranked by it; check that the tests listed were part of that run.",
		name, coverageStamp(prof), c.Total)
}
