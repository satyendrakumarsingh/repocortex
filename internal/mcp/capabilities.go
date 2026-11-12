package mcp

import (
	stdctx "context"
	"errors"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/planchange"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type targetArgs struct {
	Symbol string `json:"symbol" jsonschema:"The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'."`

	minConfidenceArg
	hubDegreeLimitArg
}

// impactArgs takes its own min_confidence wording, and that is the ONE place
// this parameter is described twice on purpose.
//
// analyze_impact has always walked at impact.DefaultMinConfidence (medium),
// not low, and it must keep doing so when the parameter is absent: dropping
// the floor to low would widen every existing caller's blast radius and weaken
// the risk score computed from it. Embedding minConfidenceArg here would
// advertise "Default low (everything)" on a tool whose default is medium,
// which is the advertised-schema-versus-reality lie this whole task exists to
// remove. The wording differs exactly where the behaviour differs; the
// decoding is still parseMinConfidence's, via parseImpactFloor.
type impactArgs struct {
	Symbol string `json:"symbol" jsonschema:"The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'."`

	MinConfidence string `json:"min_confidence,omitempty" jsonschema:"Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives."`

	hubDegreeLimitArg
}

// registerCapabilities adds the graph-backed analysis tools.
//
// Like get_task_context these need the graph, so they are registered
// separately: an index whose references were never resolved should still serve
// symbol lookups rather than failing to start.
func (s *Server) registerCapabilities(allowed map[string]bool) {
	if s.deps.Graph == nil {
		return
	}

	descs := toolDescriptions()

	addTool(s, allowed, &sdk.Tool{
		Name:        "analyze_impact",
		Description: descs["analyze_impact"],
	}, s.impactAnalysis)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_tests",
		Description: descs["find_tests"],
	}, s.findTests)

	addTool(s, allowed, &sdk.Tool{
		Name:        "plan_change",
		Description: descs["plan_change"],
	}, s.planChange)

	addTool(s, allowed, &sdk.Tool{
		Name:        "list_blindspots",
		Description: descs["list_blindspots"],
	}, s.detectBlindspots)
}

func (s *Server) impactAnalysis(ctx stdctx.Context, req *sdk.CallToolRequest, args impactArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call analyze_impact again.",
			"symbol must not be empty")
	}
	// Zero value when absent, so AnalyzeWith applies its own default. See
	// parseImpactFloor.
	minConf, err := parseImpactFloor(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	deps := s.capDeps
	// No floorNote here: the report already names what its floor kept out, as
	// a "NOT VISIBLE (below_confidence_floor)" unknown rendered below. Saying
	// it twice would spend the item budget on a repeat.
	rep, err := impact.AnalyzeWith(ctx, deps, runID, args.Symbol, impact.Options{
		MinConfidence:  minConf,
		HubDegreeLimit: args.HubDegreeLimit,
	})
	if err != nil {
		// "That symbol is not in the index" is a fact about the repository, not
		// a malfunction of the analyser. Everything else here is: a traversal
		// that errored produced no report, and an agent must not read that as
		// "nothing depends on this symbol".
		if errors.Is(err, capability.ErrTargetNotFound) {
			return s.missingSymbol(ctx, req, runID, status, args.Symbol)
		}
		return failFrom(err, "impact analysis: %v", err)
	}

	items, reach := impactItems(rep)

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%s: %s. Risk %s (score %d).",
			args.Symbol, reach.summary(), rep.Risk.Level, rep.Risk.Score),
		Items: items,
	}

	// The gate goes ahead of the risk breakdown, the factors and the unknowns.
	// A risk score computed over a graph RepoCortex could barely resolve is a
	// precise-looking number built on absent evidence, and its precision is
	// exactly what makes it dangerous to read without this sentence first.
	//
	// The stats come from the report's own Completeness block rather than a
	// second query: the number shown must be the number the analysis actually
	// used.
	if gate := coverageOf(rep.Completeness.Resolution, rep.Target.FilePath).caveat("impact"); gate != nil {
		resp.Notes = append(resp.Notes, gate...)
	}

	// What the dispatch rows mean, and what was folded into a stronger claim.
	// Both are stated once per response rather than once per row: the per-row
	// claim is on the item's own Why, and repeating the section's meaning 40
	// times would spend the budget the items need.
	resp.Notes = append(resp.Notes, reach.notes(rep)...)

	// Show the risk score's inputs. A bare "Risk: HIGH" is unactionable — the
	// factors are what tell a developer whether to slow down and why.
	if rep.Risk.Formula != "" {
		resp.Notes = append(resp.Notes, "Risk = "+rep.Risk.Formula)
	}
	for _, f := range rep.Risk.Factors {
		resp.Notes = append(resp.Notes, fmt.Sprintf("  %s (+%d): %s", f.Name, f.Points, f.Detail))
	}
	if rep.Risk.CertaintyReason != "" {
		resp.Notes = append(resp.Notes,
			fmt.Sprintf("Certainty %s — %s. Certainty is a SEPARATE axis from risk: "+
				"how much could break, and how much to trust that, are different questions.",
				rep.Risk.Certainty, rep.Risk.CertaintyReason))
	}

	// The unknowns are the most important part of an impact report. A caller
	// who cannot see what the analysis missed will treat a short list as a
	// complete one.
	for _, u := range rep.Unknown {
		resp.Notes = append(resp.Notes, fmt.Sprintf("NOT VISIBLE (%s): %s", u.Kind, u.Detail))
	}
	if rep.Truncated {
		resp.Truncated = true
		resp.TruncatedReason = rep.TruncatedReason
	}
	resp.Notes = append(resp.Notes,
		"This is a LOWER BOUND. Static analysis cannot see reflection, dynamic dispatch "+
			"or runtime wiring, and unresolved references are listed above. Do not treat an "+
			"empty result as proof that nothing breaks.")
	return s.reply(ctx, req, resp)
}

// Item kinds an impact report can carry.
//
// Three kinds because the report makes three different claims. A direct caller
// is a recorded reference to the target; an indirect one is reachable through
// other symbols; a dispatch row is a symbol the runtime MAY execute when
// something names the target, with no call site proven to take that path.
// Flattening them would let a possibility be read as a fact.
const (
	kindDirect   = "direct"
	kindIndirect = "indirect"
	kindDispatch = "dispatch"
)

// reachCounts records what impactItems actually rendered, as opposed to what
// the report contained.
//
// The summary must count the rows an agent can see. A summary that counts the
// report's slices instead would over-report every time a symbol was suppressed
// as a duplicate, which is the same double-count the dedup exists to prevent.
type reachCounts struct {
	direct   int
	indirect int
	dispatch int

	// merged counts dispatch rows suppressed because the symbol was already
	// listed as a caller. Reported rather than dropped silently: the reader
	// should know the stronger claim absorbed a weaker one.
	merged int
}

// impactItems renders a report's reachability sets as response items, strongest
// claim first, each symbol at most once.
//
// Order is load-bearing twice over. It is the order the dedup resolves ties in
// — direct beats indirect beats dispatch, so a symbol that is both keeps the
// stronger claim — and it is the order Response.Bound truncates in, since Bound
// drops trailing items, so a response that cannot carry everything sheds the
// weakest claims first.
func impactItems(rep impact.Report) ([]Item, reachCounts) {
	var (
		items []Item
		n     reachCounts
	)

	// impact.Analyze already excludes callers from the dispatch expansion, so
	// on a report it produced this map finds nothing. It is here because
	// rendering is the last place the guarantee can be checked, and a duplicated
	// row is a double-counted blast radius rather than a cosmetic defect.
	seen := make(map[string]struct{}, rep.TotalReached())
	claim := func(sym capability.Symbol) bool {
		key := symbolKey(sym)
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}

	// Prefer the CALL SITE over the symbol's own declaration line. "Which line
	// calls this" is what a developer about to change a signature needs; the
	// declaration is a click away either way. For a dispatch row the evidence is
	// the recorded subtype edge rather than a call — which is exactly what the
	// row's Why says it is.
	callSite := func(d impact.Impacted) (string, int) {
		if len(d.Evidence) > 0 {
			return d.Evidence[0].FilePath, d.Evidence[0].Range.Start.Line
		}
		return d.FilePath, 0
	}

	for _, d := range rep.Direct {
		if !claim(d.Symbol) {
			continue
		}
		path, line := callSite(d)
		items = append(items, Item{
			Kind: kindDirect, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence), Why: "calls the target directly",
		})
		n.direct++
	}
	for _, d := range rep.Indirect {
		if !claim(d.Symbol) {
			continue
		}
		path, line := callSite(d)
		items = append(items, Item{
			Kind: kindIndirect, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence),
			Why:        fmt.Sprintf("reaches the target at depth %d (confidence is the weakest link on that path)", d.Depth),
		})
		n.indirect++
	}
	for _, d := range rep.Dispatch {
		if !claim(d.Symbol) {
			// Already rendered under the stronger caller claim. Counted, so the
			// notes can say the two sets overlapped, never re-listed.
			n.merged++
			continue
		}
		path, line := callSite(d.Impacted)
		items = append(items, Item{
			Kind: kindDispatch, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence), Why: whyDispatched(d),
		})
		n.dispatch++
	}
	return items, n
}

// whyDispatched states HOW a dispatch row was reached, on the row itself.
//
// It leads with the mechanism, not with the detail, for two reasons. A reader
// scanning the list sees "not a direct call" before anything that could be
// mistaken for one — a dispatch row carries a file:line like every other row,
// and without this sentence it reads as a call site. And Limits.MaxWhyBytes
// clips from the end, so the words that must survive clipping go first.
func whyDispatched(d impact.Dispatched) string {
	var how string
	switch d.Reason {
	case impact.ReasonImplementsTarget:
		how = "it implements or overrides the target, so a call naming the target can execute it at run time"
	case impact.ReasonThroughImplementation:
		// Depth counts from the target, so the subtype hop spends one of it. A
		// row that did not carry a usable depth says so by omitting the number
		// rather than by printing a computed one.
		how = "it was reached beyond an implementation of the target, and every path to it " +
			"crosses a subtype edge"
		if d.Depth > 1 {
			how = fmt.Sprintf("it is %d hop(s) beyond an implementation of the target, and every "+
				"path to it crosses a subtype edge", d.Depth-1)
		}
	default:
		// An unrecognised reason still has to say something true. Naming the raw
		// reason is honest; inventing a sentence for it would not be.
		how = fmt.Sprintf("reason %q", string(d.Reason))
	}
	return "NOT a direct call — reached through interface/virtual dispatch: " + how +
		". No call site is proven to take that path"
}

// summary is the one-line count of what the response carries.
func (n reachCounts) summary() string {
	s := fmt.Sprintf("%d direct, %d indirect", n.direct, n.indirect)
	if n.dispatch > 0 {
		s += fmt.Sprintf(", %d reached by interface dispatch", n.dispatch)
	}
	return s
}

// maxThroughNamed bounds the implementations named in the dispatch note.
//
// Notes are rendered in full and are NOT dropped by Response.Bound, which drops
// items — so an unbounded note eats the budget the results need. Three names
// tell the reader where the expansion hinged; a list of forty would not.
const maxThroughNamed = 3

// notes explains the dispatch section once per response.
func (n reachCounts) notes(rep impact.Report) []string {
	var out []string
	if n.dispatch > 0 {
		note := "Items marked 'dispatch' were reached through an interface or an overridden " +
			"method, NOT by a call to the target."
		if named := namesOf(rep.DispatchThrough, maxThroughNamed); named != "" {
			note += " The expansion went through " + named + "."
		}
		out = append(out, note)
	}
	if n.merged > 0 {
		out = append(out, fmt.Sprintf(
			"%d symbol(s) are reachable by dispatch AND reference the target. Each is listed "+
				"once, under the stronger direct/indirect claim.", n.merged))
	}
	return out
}

// namesOf renders at most max symbol names, saying how many it left out.
func namesOf(syms []capability.Symbol, max int) string {
	if len(syms) == 0 || max <= 0 {
		return ""
	}
	shown := syms
	if len(shown) > max {
		shown = shown[:max]
	}
	names := make([]string, 0, len(shown))
	for _, s := range shown {
		names = append(names, clip(s.Name, 60))
	}
	out := strings.Join(names, ", ")
	if rest := len(syms) - len(shown); rest > 0 {
		out += fmt.Sprintf(" and %d more", rest)
	}
	return out
}

// symbolKey identifies a symbol for deduplication across the report's sets.
//
// The symbol id is the real identity and is what a report from impact.Analyze
// always carries. The fallbacks exist because a key that collapses every
// id-less symbol onto one bucket would silently drop rows, and dropping a row
// is a worse failure than listing one twice.
func symbolKey(s capability.Symbol) string {
	switch {
	case s.ID != 0:
		return fmt.Sprintf("id:%d", s.ID)
	case s.FQN != "":
		return "fqn:" + s.FQN
	default:
		return "sym:" + s.FilePath + "#" + s.Name
	}
}

// testsMissingFrom counts the tests in unfiltered that do not appear in
// filtered at all — the honest measure of what a confidence floor cost a
// find_tests caller.
//
// Deliberately NOT len(unfiltered)-len(filtered): a test can survive the
// floor via a weaker signal (naming, co-location) after the floor removes
// only its graph signal, which changes nothing about which tests the caller
// sees even though the two lengths could still differ by coincidence on an
// unrelated symbol. Presence, keyed by symbolKey, is what floorNote's claim
// is actually about — a result the caller would have seen and now does not.
func testsMissingFrom(unfiltered, filtered []tests.TestHit) int {
	present := make(map[string]bool, len(filtered))
	for _, t := range filtered {
		present[symbolKey(t.Symbol)] = true
	}
	missing := 0
	for _, t := range unfiltered {
		if !present[symbolKey(t.Symbol)] {
			missing++
		}
	}
	return missing
}

func (s *Server) findTests(ctx stdctx.Context, req *sdk.CallToolRequest, args targetArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call find_tests again.",
			"symbol must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	deps := s.capDeps
	// Only the floor and the hub bound are set; every other bound stays at
	// Options.withDefaults, which is exactly what FindTests would have applied.
	rep, err := tests.FindTestsWith(ctx, deps, runID, args.Symbol, tests.Options{
		MinConfidence:  minConf,
		HubDegreeLimit: args.HubDegreeLimit,
	})
	if err != nil {
		if errors.Is(err, capability.ErrTargetNotFound) {
			return s.missingSymbol(ctx, req, runID, status, args.Symbol)
		}
		return failFrom(err, "test discovery: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%s: %d test(s), %d proven by a call edge.",
			args.Symbol, len(rep.Tests), rep.ProvenByGraph),
	}

	for _, t := range rep.Tests {
		var sigs []string
		for _, s := range t.Signals {
			sigs = append(sigs, string(s.Signal))
		}
		resp.Items = append(resp.Items, Item{
			Kind: "test", Name: t.Name, Path: t.FilePath,
			Confidence: string(t.Confidence),
			// Naming the signal matters: "found by a proven call edge" and
			// "found because the filename looked right" are different claims,
			// and an agent deciding what to run should know which it has.
			Why: "found by " + strings.Join(sigs, ", "),
		})
	}

	// "Untested" is the strongest claim this tool makes, and the one a low
	// resolution rate most easily fabricates: a test that calls the symbol
	// through an unresolved reference is a test RepoCortex cannot see. The gate
	// runs before that claim is stated, not after.
	if gate := coverageOf(rep.Completeness.Resolution, rep.Target.FilePath).caveat("test discovery"); gate != nil {
		resp.Notes = append(resp.Notes, gate...)
	}
	// Ahead of the "untested" claim below, not after it: a floor is one more
	// reason a symbol can look untested when it is not.
	//
	// dropped can't come from a count already in rep: FindTestsWith applies
	// the floor inside the graph traversal (naming and co-location signals
	// are never confidence-scoped — see tests.go), so a test the floor
	// removed from the graph signal can still be present in rep.Tests via one
	// of those, and rep carries no count of what the floor alone excluded.
	// Re-running the unfiltered discovery and diffing the symbol sets is the
	// only way to answer "did the floor actually remove anything a caller
	// would have seen" from within this package — paid only when a floor was
	// actually requested, not on the default (low) path every other caller
	// takes.
	dropped := 0
	if minConf != "" && minConf != model.ConfidenceLow {
		// The hub bound is carried over: this walk must differ from the one
		// above in exactly one respect, or a test the HUB CUT removed would be
		// counted as removed by the floor.
		unfiltered, uerr := tests.FindTestsWith(ctx, deps, runID, args.Symbol,
			tests.Options{HubDegreeLimit: args.HubDegreeLimit})
		if uerr == nil {
			dropped = testsMissingFrom(unfiltered.Tests, rep.Tests)
		}
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	if rep.Untested {
		resp.Notes = append(resp.Notes,
			"No tests found for this symbol. That is a positive finding — but it is bounded "+
				"by what the index resolved, so confirm before concluding the code is untested.")
	}
	for _, s := range tests.SuggestMissingScenarios(rep.Target, rep) {
		resp.Notes = append(resp.Notes, "Possible gap: "+s.Scenario+" — "+s.Why)
	}
	if rep.Truncated {
		resp.Truncated = true
		resp.TruncatedReason = rep.TruncatedReason
	}
	return s.reply(ctx, req, resp)
}

func (s *Server) planChange(ctx stdctx.Context, req *sdk.CallToolRequest, args targetArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call plan_change again.",
			"symbol must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	planner := planchange.NewPlanner(s.capDeps)
	plan, err := planner.Plan(ctx, runID, args.Symbol, planchange.Options{
		MinConfidence:  minConf,
		HubDegreeLimit: args.HubDegreeLimit,
	})
	if err != nil {
		if errors.Is(err, capability.ErrTargetNotFound) {
			return s.missingSymbol(ctx, req, runID, status, args.Symbol)
		}
		return failFrom(err, "plan change for %q: %v", args.Symbol, err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("Change plan for %s: Risk %s (%d/100), %d direct caller(s), %d total affected.",
			plan.Target.Name, strings.ToUpper(plan.RiskLevel), plan.RiskScore, plan.DirectCallers, plan.TotalCallers),
		Items: []Item{
			{
				Kind:       "target",
				Name:       plan.Target.Name,
				FQN:        plan.Target.FQN,
				Path:       plan.Target.FilePath,
				Line:       plan.Target.Line,
				Confidence: string(plan.Target.Confidence),
				Why:        fmt.Sprintf("target symbol to modify (risk score: %d/100, level: %s)", plan.RiskScore, plan.RiskLevel),
			},
		},
	}

	for _, step := range plan.Steps {
		resp.Items = append(resp.Items, Item{
			Kind: "plan_step",
			Name: fmt.Sprintf("Step %d: %s", step.Order, step.Kind),
			Path: step.Path,
			Line: step.Line,
			Why:  step.Description,
		})
	}

	if len(plan.AffectedTests) > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf("Covering tests to execute: %s", strings.Join(plan.AffectedTests, ", ")))
	}

	return s.reply(ctx, req, resp)
}

func (s *Server) detectBlindspots(ctx stdctx.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	detector := missed.NewDetector(s.deps.Registry, s.capDeps)
	rep, err := detector.Detect(ctx, runID, missed.DefaultOptions())
	if err != nil {
		return failFrom(err, "detect blindspots: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%d code intelligence blindspots detected across repository.", rep.TotalBlindspots),
	}

	for _, b := range rep.Blindspots {
		resp.Items = append(resp.Items, Item{
			Kind: string(b.Kind),
			Name: b.Target,
			Path: b.Path,
			Line: b.Line,
			Why:  fmt.Sprintf("[%s] %s -> %s", b.Severity, b.Description, b.Remediation),
		})
	}

	if rep.TotalBlindspots == 0 {
		resp.Notes = append(resp.Notes, "No code intelligence blindspots detected. Graph and configuration linkages are healthy.")
	}

	return s.reply(ctx, req, resp)
}
