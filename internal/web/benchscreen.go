package web

import (
	"fmt"
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/bench"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

// The retrieval benchmark: does the context engine return the right code?
//
// # Why the forbid list IS the score
//
// Recall alone is trivially gamed by returning the whole repository. A case
// that only says what a pack MUST contain measures how much the engine drags
// in, and rewards dragging in more. The forbid list — the unrelated module a
// naive keyword search pulls in — is what turns the number into a claim, so
// this payload carries BOTH lists and a score for each, per case and in
// aggregate. A client cannot render this screen showing recall alone.
//
// A forbidden item retrieved is worse than a missing one and is never averaged
// away: it is context an agent will read and act on.
//
// # Scored against the pack the engine actually produces
//
// The pack comes from internal/context through the server's own assembler — the
// same one the contract and verify screens use, and the same one `repocortex
// bench` builds — so a change to ranking, budgeting or extraction shows up here
// as a number rather than as a surprise in an agent session weeks later. The
// scoring is bench.Score's, not this package's.
//
// # What this does not measure
//
// Whether an agent completed the task. Only whether the context contained what
// a human said it should. Those are different claims and conflating them would
// be the most flattering possible mistake.

// benchCasesFile is the conventional name looked for when ?cases= is absent.
const benchCasesFile = "repocortex.bench.yml"

// maxBenchCases bounds how many cases one request will run.
//
// Each case costs a full context assembly, which is the most expensive thing
// this portal does. Whatever the bound cuts is counted rather than dropped, and
// the CLI runs the whole file without it.
const maxBenchCases = 25

// maxBenchCaseFileBytes bounds the case file this screen will read.
const maxBenchCaseFileBytes = 1 << 20

// BenchCase is one scored case, with both halves of the score.
//
// Expected and Forbidden are the case as written; Found, Missing and Violations
// are what the pack did about them. Both lists are present on every row because
// a row showing recall without its forbid list is a number with no upper bound
// on how it was earned.
type BenchCase struct {
	ID   string `json:"id"`
	Task string `json:"task"`

	Expected  []string `json:"expected"`
	Forbidden []string `json:"forbidden"`

	Found      []string `json:"found"`
	Missing    []string `json:"missing"`
	Violations []string `json:"violations"`

	// Recall is Found over Expected.
	Recall float64 `json:"recall"`

	// ForbidScore is the fraction of Forbidden entries the pack kept OUT.
	// ForbidMeasured is false when the case names none, in which case this
	// case measures recall only — and recall alone is gamed by returning
	// everything, which is why the flag is a field rather than an inference
	// from an empty list.
	ForbidScore    float64 `json:"forbid_score"`
	ForbidMeasured bool    `json:"forbid_measured"`

	TokenEstimate int  `json:"token_estimate"`
	OverBudget    bool `json:"over_budget"`
	Passed        bool `json:"passed"`
	Items         int  `json:"items"`
}

// BenchScores is the suite aggregate, with the two halves kept apart.
type BenchScores struct {
	Cases  int `json:"cases"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`

	MeanRecall float64 `json:"mean_recall"`

	// MeanForbidScore is averaged over the cases that HAVE a forbid list.
	// CasesWithoutForbid is how many did not, and it is the number that says
	// how much of MeanRecall is trustworthy.
	MeanForbidScore    float64 `json:"mean_forbid_score"`
	CasesWithForbid    int     `json:"cases_with_forbid"`
	CasesWithoutForbid int     `json:"cases_without_forbid"`

	// TotalViolations counts forbidden items retrieved across every case. A
	// precision failure, never a rounding error.
	TotalViolations int `json:"total_violations"`

	OverBudget int `json:"over_budget_cases"`

	// Scope states what these were computed over, because a capped run's
	// aggregate is not the file's aggregate.
	Scope string `json:"scope"`
}

// BenchResponse is the whole retrieval-benchmark screen.
type BenchResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Cases is the case file this screen tried to read, echoed whether or not
	// it was found.
	CaseFile FileRef `json:"case_file"`

	// Scored is false when no case file was read. Every number below is then
	// absent for that reason and no other.
	Scored bool `json:"scored"`

	// CasesInFile is how many cases the file declares, before the bound.
	CasesInFile int `json:"cases_in_file"`

	Scores BenchScores `json:"scores"`
	Cases  []BenchCase `json:"cases"`

	Caveats []string `json:"caveats"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

func (s *Server) handleBench(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := BenchResponse{
		Meta: meta, Legend: Legend(),
		Cases: []BenchCase{}, Caveats: benchCaveats(),
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label:   "Index this repository first",
			Detail:  "The benchmark scores the packs the context engine assembles from an index.",
			Command: "repocortex index",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	pr := s.packReader()
	if pr == nil {
		resp.Help = &Guidance{
			Title:  "This portal cannot assemble a context pack",
			Detail: errNoPackReader.Error() + " The benchmark scores packs, so without that port there is nothing to score.",
			Steps:  []string{"repocortex bench " + benchCasesFile},
		}
		resp.Next = NextAction{Label: "Run the benchmark from the command line", Command: "repocortex bench " + benchCasesFile}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.CaseFile = s.repoFile(r, "cases", benchCasesFile)
	var om Omissions

	src, readErr := readRepoFile(&resp.CaseFile, maxBenchCaseFileBytes)
	if readErr != nil {
		resp.Help = benchMissingGuidance(resp.CaseFile)
		resp.Next = benchNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	cases, err := bench.LoadCases(src)
	if err == nil {
		err = bench.Validate(cases)
	}
	if err != nil {
		resp.CaseFile.Problem = err.Error()
		resp.Help = &Guidance{
			Title:  fmt.Sprintf("%s is not a valid case file", resp.CaseFile.Path),
			Detail: err.Error(),
			Steps: []string{
				"A case names an id, a task, an `expect` list and — the half that makes the score mean something — a `forbid` list.",
				"repocortex bench " + resp.CaseFile.Path + " — the same validation from the command line",
			},
		}
		resp.Next = benchNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.CasesInFile = len(cases)
	if len(cases) == 0 {
		resp.Help = &Guidance{
			Title:  fmt.Sprintf("%s declares no cases", resp.CaseFile.Path),
			Detail: "An empty suite scores 0 of 0, which reads as a pass and is not one.",
			Steps:  []string{"Write a case: an id, a task, an `expect` list and a `forbid` list."},
		}
		resp.Next = benchNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	scored := capped(cases, maxBenchCases, &om, ReasonRowBudget,
		fmt.Sprintf("Cases beyond the first %d (file order) were not RUN. Each case costs a full "+
			"context assembly, which is the most expensive thing this portal does, so the aggregate "+
			"below is over the cases that ran and not over the file.", maxBenchCases),
		"repocortex bench "+resp.CaseFile.Path)

	results := make([]bench.Result, 0, len(scored))
	byID := make(map[string]bench.Case, len(scored))
	for _, c := range scored {
		if err := ctx.Err(); err != nil {
			s.fail(w, r, fmt.Errorf("web: score case %s: %w", c.ID, err))
			return
		}
		pack, err := s.assembler.Assemble(ctx, pr, s.deps.Caps.Graph, run.ID, devctx.Request{
			Task:   c.Task,
			Budget: devctx.BudgetFrom(s.deps.Config),
		})
		if err != nil {
			s.fail(w, r, fmt.Errorf("web: assemble context for case %s: %w", c.ID, err))
			return
		}
		byID[c.ID] = c
		results = append(results, bench.Score(c, pack))
	}

	suite := bench.Summarise(results)
	resp.Scored = true
	resp.Cases, resp.Scores = benchRows(suite, byID, len(scored) < len(cases))

	if suite.TotalViolations > 0 {
		om.Note(ReasonUnresolvedReferences,
			fmt.Sprintf("%d forbidden item(s) were retrieved across %d case(s). A forbidden item is "+
				"not a rounding error and is not averaged into the recall figure: it is context an "+
				"agent will read and act on.", suite.TotalViolations, suite.Cases),
			"read the violating cases before trusting mean_recall")
	}
	if resp.Scores.CasesWithoutForbid > 0 {
		om.Note(ReasonEvidenceBudget,
			fmt.Sprintf("%d of %d scored case(s) declare no forbid list, so nothing in them can "+
				"detect over-retrieval. Their recall is a lower bound on quality, not a measure of it.",
				resp.Scores.CasesWithoutForbid, suite.Cases),
			"add a `forbid:` list naming the module a naive search drags in")
	}

	resp.Next = benchNextAction(resp)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// benchRows projects the scored suite into the payload.
//
// Both halves of every score are carried: the case's own expect and forbid
// lists beside what the pack did about them. The aggregate keeps the two means
// apart and counts the cases that cannot contribute to the second one.
func benchRows(suite bench.Suite, byID map[string]bench.Case, capped bool) ([]BenchCase, BenchScores) {
	rows := make([]BenchCase, 0, len(suite.Results))
	scores := BenchScores{
		Cases: suite.Cases, Passed: suite.Passed, Failed: suite.Cases - suite.Passed,
		MeanRecall: suite.MeanRecall, TotalViolations: suite.TotalViolations,
	}

	var forbidTotal float64
	for _, res := range suite.Results {
		c := byID[res.CaseID]
		row := BenchCase{
			ID: res.CaseID, Task: res.Task,
			Expected: orEmpty(c.Expect), Forbidden: orEmpty(c.Forbid),
			Found: orEmpty(res.Found), Missing: orEmpty(res.Missing),
			Violations: orEmpty(res.Violations),
			Recall:     res.Recall, TokenEstimate: res.TokenEstimate,
			OverBudget: res.OverBudget, Passed: res.Passed, Items: res.Items,
		}
		if len(c.Forbid) > 0 {
			row.ForbidMeasured = true
			row.ForbidScore = float64(len(c.Forbid)-len(res.Violations)) / float64(len(c.Forbid))
			forbidTotal += row.ForbidScore
			scores.CasesWithForbid++
		} else {
			scores.CasesWithoutForbid++
		}
		if res.OverBudget {
			scores.OverBudget++
		}
		rows = append(rows, row)
	}
	if scores.CasesWithForbid > 0 {
		scores.MeanForbidScore = forbidTotal / float64(scores.CasesWithForbid)
	}

	scores.Scope = fmt.Sprintf(
		"Computed over the %d case(s) this request ran. mean_forbid_score is averaged over the %d "+
			"case(s) that declare a forbid list; the other %d measure recall only.",
		scores.Cases, scores.CasesWithForbid, scores.CasesWithoutForbid)
	if capped {
		scores.Scope += " The case file holds more cases than this request ran, so these are not the file's scores."
	}
	return rows, scores
}

// orEmpty keeps a list non-null, so a client never has to distinguish null from
// empty on a field whose emptiness is a finding.
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// benchCaveats are the standing limits of the number, printed every time.
func benchCaveats() []string {
	return []string{
		"This measures whether the CONTEXT contained what a human said it should. It does not " +
			"measure whether an agent completed the task — conflating those would be the most " +
			"flattering possible mistake.",
		"Recall alone is trivially gamed by returning the whole repository. A case with no forbid " +
			"list measures recall only, and the count of such cases is reported beside the mean.",
		"A forbidden item retrieved is a precision failure, not a rounding error: it is context an " +
			"agent will read and act on.",
		"Precision is measured against the forbid list rather than against everything, because a " +
			"pack legitimately contains supporting context no case author would enumerate.",
	}
}

func benchMissingGuidance(ref FileRef) *Guidance {
	detail := fmt.Sprintf(
		"The retrieval benchmark is the one claim in this product that would otherwise rest on "+
			"assertion, and it needs cases a human wrote: the code a correct answer MUST contain, "+
			"and the code it must NOT. Nothing has been written here yet, so this screen is empty "+
			"because the measurement has not been defined — not because the engine scored zero. %s",
		ref.Problem)
	if ref.Defaulted {
		detail += " " + ref.DefaultedFrom
	}
	return &Guidance{
		Title:  fmt.Sprintf("No benchmark case file at %s", ref.Path),
		Detail: detail,
		Steps: []string{
			"Write cases in " + benchCasesFile + ", or pass ?cases=<repo-relative path>",
			"  - id: idempotent-callback\n    task: Add idempotency to payment callback processing\n    expect: [CallbackController, TransactionService, transactions]\n    forbid: [MarketingReport]",
			"repocortex bench " + benchCasesFile + " — the same scoring from the command line",
		},
	}
}

func benchNextAction(resp BenchResponse) NextAction {
	switch {
	case !resp.Scored:
		return NextAction{
			Label:   "Write the cases that define the measurement",
			Detail:  "Without a forbid list there is no way to tell a good pack from a big one.",
			Command: "repocortex bench " + benchCasesFile,
		}
	case resp.Scores.TotalViolations > 0:
		return NextAction{
			Label: fmt.Sprintf("%d forbidden item(s) were retrieved", resp.Scores.TotalViolations),
			Detail: "A precision failure, not a rounding error: each is context an agent will read " +
				"and act on. Read the violating cases before trusting the recall figure beside them.",
			Screen: "contract",
		}
	case resp.Scores.CasesWithoutForbid > 0:
		return NextAction{
			Label: fmt.Sprintf("%d case(s) declare no forbid list", resp.Scores.CasesWithoutForbid),
			Detail: "Those cases measure recall only, and recall alone is gamed by returning the " +
				"whole repository. Their contribution to the mean is not a precision claim.",
			Command: "repocortex bench " + benchCasesFile,
		}
	case resp.Scores.Failed > 0:
		return NextAction{
			Label:  fmt.Sprintf("%d of %d case(s) failed", resp.Scores.Failed, resp.Scores.Cases),
			Detail: "Each failure names what the pack was missing. Start with the case whose recall is lowest.",
			Screen: "contract",
		}
	default:
		return NextAction{
			Label:  "Read the mean recall beside the forbid score",
			Detail: "Either number alone can be made to look good by an engine that is bad in the other direction.",
			Screen: "overview",
		}
	}
}
