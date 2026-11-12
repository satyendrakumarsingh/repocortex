package mcp

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// bindingFloor is capability.CoverageFloor: the one floor every surface that
// publishes a coverage verdict now uses.
//
// It used to be a second, independently-argued constant, checked against
// BindingRate while the relationship tools checked ResolutionStats.Rate
// against 0.5 — and the two disagreed out loud, calling one index 37% and
// unusable in the same session as 79% and unremarkable. The argument that
// produced 0.6 was the better one and it is now recorded at
// capability.CoverageFloor, which is where both surfaces read it.
//
// It is checked beside the rate and the Ambiguous/Unresolved counts (see
// resolutionSummary) so a reader who would draw the line elsewhere can.
const bindingFloor = capability.CoverageFloor

// resolutionCoverage answers "how much of this index actually resolved?".
//
// # Why a tool of its own when get_project_overview already says it
//
// The overview says it ONCE, as one line among file and symbol counts, and an
// agent reaches it at the start of a session. The question this tool answers is
// asked at a different moment and for a different reason: an analyze_impact or
// find_callers result has come back short, and the agent has to decide whether
// that is a fact about the repository or a fact about the index. Making it ask
// for the whole overview again to find out is how an agent stops asking.
//
// It needs only the Registry, so it registers with the plain tools rather than
// with registerCapabilities: an index whose references were never resolved is
// PRECISELY the index this tool exists to describe, and gating it on the graph
// would silence it exactly there.
func (s *Server) resolutionCoverage(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	// Read through capDeps, not the registry directly. ResolutionStats is a
	// GROUP BY over every reference row in the run; capability.Deps memoises it
	// for the server's lifetime, and this tool asking the same question a
	// second time in a session should cost nothing.
	rs, err := s.capDeps.ResolutionStats(ctx, runID)
	if err != nil {
		return failFrom(err, "read resolution stats: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       resolutionSummary(rs),
	}
	resp.Notes = append(resp.Notes, resolutionNotes(rs)...)
	resp.Notes = append(resp.Notes, capability.LowerBoundCaveat)
	return s.reply(ctx, req, resp)
}

// resolutionSummary states the rate and, in the same sentence, everything the
// rate's denominator left out.
//
// # Which of the two rates is published, and why
//
// BindingRate, not Rate. registry.ResolutionStats documents both and is unusually
// explicit about the difference: Rate divides by EVERY reference, including the
// ones that matched zero candidates in this index, which are overwhelmingly
// calls into third-party libraries. A project that imports heavily therefore
// scores badly on Rate no matter how well RepoCortex resolved it, so Rate mostly
// measures a dependency surface while reading like a defect rate — and an agent
// that reads "46%" concludes the index is broken when the index is fine.
// BindingRate divides by the references that name something this index actually
// defines, which is the population any improvement to binding could ever move.
//
// # The exclusion is only honest because the excluded are printed
//
// ResolutionStats.Bindable states the condition on which those references may be
// excluded: "Callers must print Unresolved beside this rate, never instead of
// it." An unresolved reference is not proof of anything about a library — it can
// equally be a definition in this repository that the extractor never emitted —
// so this summary names the unresolved count, the ambiguous count and the total
// alongside the percentage. A reader who would draw the denominator differently
// has every number needed to do so. This is also the wording internal/cli's
// bindingCaveat and get_project_overview already use, so the CLI and the MCP
// surface cannot drift into publishing two different coverage numbers.
func resolutionSummary(rs registry.ResolutionStats) string {
	switch {
	case rs.Total == 0:
		return "No reference data is recorded for this index, so resolution coverage cannot be stated."
	case rs.Pending == rs.Total:
		return fmt.Sprintf("%d reference(s) were extracted and none resolved — the call graph "+
			"has not been built.", rs.Total)
	}

	by := ""
	if rs.ByCompiler > 0 {
		by = fmt.Sprintf(", %d of them by an imported compiler index", rs.ByCompiler)
	}
	pending := ""
	if rs.Pending > 0 {
		pending = fmt.Sprintf("; %d reference(s) are still pending", rs.Pending)
	}
	return fmt.Sprintf(
		"resolved %d of %d references that name something this index defines (%.1f%%)%s; "+
			"%d ambiguous, %d matched no definition here at all (mostly calls into libraries), "+
			"%d references in total%s.",
		rs.Resolved, rs.Bindable(), 100*rs.BindingRate(), by,
		rs.Ambiguous, rs.Unresolved, rs.Total, pending)
}

// resolutionNotes says what to DO about the number above.
//
// A coverage figure with no next step is a number an agent can only worry
// about. Each branch below names the one command that moves it, and the
// low-coverage branch says what a low figure does to every OTHER answer this
// server gives — which is the part that changes behaviour.
func resolutionNotes(rs registry.ResolutionStats) []string {
	switch {
	case rs.Total == 0:
		return []string{
			"Nothing has been resolved because nothing was extracted. Run 'repocortex index' " +
				"followed by 'repocortex resolve'. Until then an empty find_callers or " +
				"analyze_impact result says nothing about the code.",
		}
	case rs.Pending == rs.Total:
		return []string{
			"The references exist but none has been bound to a definition, so find_callers, " +
				"find_references and analyze_impact will return nothing regardless of the code. " +
				"Run 'repocortex resolve'.",
		}
	}

	notes := []string{
		"AMBIGUOUS means the name matched several definitions here and RepoCortex refused to " +
			"guess between them. UNRESOLVED means it matched none, which usually indicates a " +
			"library call and sometimes a definition this index failed to cover — those are " +
			"different problems with different fixes, which is why they are counted apart.",
	}
	if rs.Pending > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d reference(s) have not been resolved since the last index — run 'repocortex resolve' "+
				"so this figure describes the whole index.", rs.Pending))
	}
	if rs.BindingRate() < bindingFloor {
		notes = append(notes, fmt.Sprintf(
			"COVERAGE TOO LOW — fewer than %.0f%% of bindable references are bound (%.0f%%). "+
				"Relationship answers from this index are a minority report: treat find_callers, "+
				"find_references and analyze_impact results as leads to confirm with search_code, "+
				"not as answers.", 100*bindingFloor, 100*rs.BindingRate()))
	}
	return notes
}
