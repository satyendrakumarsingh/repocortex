package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	"github.com/satyendrakumarsingh/repocortex/internal/verify"
)

// The verify report: the half of the loop nothing else asks.
//
// # Facts and leads are not the same claim and must not look alike
//
// internal/verify separates a finding that rests on a compiler-proven edge from
// one that rests on a heuristic, and gates CI on the first only. That
// distinction is why the gate survives contact with a real team: a report that
// blocks a merge on a 2%-precision edge (docs/accuracy.md) is switched off
// within a week, and it takes the facts with it when it goes.
//
// So this response never returns one undifferentiated findings list. Facts and
// leads are counted separately, carried in separate arrays, and each finding
// keeps its Standing, its Confidence and its Evidence so a reader can check the
// classification rather than trust it. A client that rendered them identically
// would be re-creating the failure the whole vocabulary exists to prevent, and
// the counts here make that visible in the payload rather than only in the
// pixels.
//
// # Unknowns are the top of the page
//
// Report.Unknowns is what the four questions could NOT answer from the inputs.
// It is hoisted to the top level for the same reason the contract's is: a
// verdict rendered without its gaps reads as a clean bill of health, and the
// disposition `unverified` exists precisely because "we looked and found
// nothing" and "there was nothing to look at" must never render the same.
//
// # Read-only, and no git
//
// The change set arrives in the query string. This package shells out to
// nothing and runs no `git diff`, exactly as `cortex verify` refuses to:
// a verifier that discovered its own change set would silently disagree with
// the caller about staged, unstaged and untracked files.

// VerifyStatus is the outcome of building the report at all — distinct from
// the report's own Disposition, which is the verdict on the change.
type VerifyStatus string

const (
	// VerifyOK: a contract was compiled and a change set was marked against it.
	VerifyOK VerifyStatus = "ok"

	// VerifyNoIntent: nothing to compile a contract from.
	VerifyNoIntent VerifyStatus = "no_intent"

	// VerifyNoChanges: a contract exists but nothing was named as changed.
	// Deliberately not run as an empty verification: verify would answer
	// `unverified`, which is correct but tells the reader less than saying the
	// form is incomplete.
	VerifyNoChanges VerifyStatus = "no_changes"

	// VerifyUnavailable: the context-pack port is not wired in this build.
	VerifyUnavailable VerifyStatus = "unavailable"
)

// FileVerdictKind classifies one changed file against the contract's scope.
//
// MEMBERSHIP ONLY. This is not the judgement — `verify` decides what is a
// finding, and it refuses to convert a symbol-level exclusion into a file-level
// accusation. This is the set arithmetic a reader needs to read the findings
// alongside a diff, and it is labelled as such.
type FileVerdictKind string

const (
	// FileInScope: the contract names at least one symbol in this file.
	FileInScope FileVerdictKind = "in_scope"

	// FileExcluded: the contract explicitly named this file as out of scope and
	// nothing in it was in scope. This is the one that produces a finding.
	FileExcluded FileVerdictKind = "excluded"

	// FileUndeclared: the contract says nothing about this file either way. Not
	// a violation — a contract is a brief, not a whitelist — but it is the
	// number that tells a reader how much of the change the brief anticipated.
	FileUndeclared FileVerdictKind = "undeclared"
)

// FileVerdict is one changed file, placed against the contract.
type FileVerdict struct {
	Path string          `json:"path"`
	Kind FileVerdictKind `json:"kind"`

	// Why is the contract's OWN words where it had any — the reason it gave for
	// putting the file in scope, or for excluding it. Never paraphrased.
	Why string `json:"why,omitempty"`

	// Symbols names the contract's scope symbols living in this file.
	Symbols []string `json:"symbols,omitempty"`
}

// VerifyResponse is the whole post-change screen.
type VerifyResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Status VerifyStatus `json:"status"`
	Help   *Guidance    `json:"guidance,omitempty"`

	Intent  string        `json:"intent"`
	Filters VerifyFilters `json:"filters"`

	// Report is internal/verify's own report, whole. Its Findings keep their
	// Standing, Confidence, Severity and Evidence.
	Report *verify.Report `json:"report,omitempty"`

	// Signal is red | amber | green | unverified, derived from Disposition and
	// carried as data so the page's colour and the process's exit code cannot
	// disagree about the same run.
	Signal   string `json:"signal"`
	ExitCode int    `json:"exit_code"`
	Verdict  string `json:"verdict"`

	// Facts and Leads are the SAME findings, partitioned. Separate arrays
	// rather than a flag a renderer may ignore: a violation on a compiler-proven
	// edge and one on a heuristic are different claims, and this is the field
	// that makes rendering them identically a deliberate act.
	Facts []verify.Finding `json:"facts"`
	Leads []verify.Finding `json:"leads"`

	// Unknowns is Report.Unknowns hoisted. See the file comment.
	Unknowns []string `json:"unknowns"`

	// Questions is verify's per-question account of what this run actually
	// engaged, hoisted for the same reason Unknowns is: the four-questions
	// table is the part of this screen that stops a verdict being read as a
	// clean bill of health, and a client should not have to reach into a nested
	// report to draw it.
	//
	// Each row carries `asked` AND `answered`. They are different: a question
	// that ran over a repository and engaged none of the changed files is asked
	// and unanswered, and that combination is exactly what used to render as a
	// green PASS.
	Questions []verify.Question `json:"questions"`

	// Unverified is true when the verdict is `unverified`. A separate boolean
	// beside Signal and Verdict because this is the state a client is most
	// likely to collapse into one of the others, and a boolean cannot be
	// mis-compared the way a string can.
	Unverified bool `json:"unverified"`

	// Files is the change set placed against the contract's scope.
	Files      []FileVerdict `json:"files"`
	FileCounts FileCounts    `json:"file_counts"`

	Counts VerifyCounts `json:"counts"`

	// Contract is the brief the change was marked against, so the reader can
	// see what it was held to without recompiling it on another screen.
	Contract *compile.Contract `json:"contract,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// VerifyFilters echoes the bounds applied.
type VerifyFilters struct {
	MinConfidence string   `json:"min_confidence,omitempty"`
	Changed       []string `json:"changed"`
	MaxScope      int      `json:"max_scope"`
}

// FileCounts is the in-scope / out-of-scope arithmetic.
type FileCounts struct {
	Changed    int `json:"changed"`
	InScope    int `json:"in_scope"`
	Excluded   int `json:"excluded"`
	Undeclared int `json:"undeclared"`
}

// VerifyCounts is the headline, split by the distinction that matters.
type VerifyCounts struct {
	Findings int `json:"findings"`
	Facts    int `json:"facts"`
	Leads    int `json:"leads"`

	// ByKind counts the four questions separately, in the fixed order verify
	// reports them: scope, architecture, missed callers, tests.
	OutOfScopeEdits        int  `json:"out_of_scope_edits"`
	ArchitectureViolations int  `json:"architecture_violations"`
	MissedCallers          int  `json:"missed_callers"`
	TestCoverageGaps       int  `json:"test_coverage_gaps"`
	ArchitectureConsulted  bool `json:"architecture_consulted"`
	RulesEvaluated         int  `json:"rules_evaluated"`
	BlastSymbolsChecked    int  `json:"blast_radius_symbols_checked"`
	NamedTestsChecked      int  `json:"named_tests_checked"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	intent, hasIntent := intentParam(r)
	changed := changedParam(r)
	maxScope := intParam(r, "max_scope", 0, 0, maxScopeCeiling)
	floor := confidenceParam(r, "min_confidence", "")

	resp := VerifyResponse{
		Meta: meta, Legend: Legend(), Intent: intent,
		Filters:    VerifyFilters{MinConfidence: string(floor), Changed: changed, MaxScope: maxScope},
		Facts:      []verify.Finding{},
		Leads:      []verify.Finding{},
		Unknowns:   []string{},
		Questions:  []verify.Question{},
		Files:      []FileVerdict{},
		Signal:     "unverified",
		ExitCode:   verify.DispositionUnverified.ExitCode(),
		Verdict:    string(verify.DispositionUnverified),
		Unverified: true,
	}
	if !ok {
		resp.Status = VerifyNoIntent
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if !hasIntent {
		resp.Status = VerifyNoIntent
		resp.Help = &Guidance{
			Title: "Verification marks a change against the brief that authorised it",
			Detail: "So it needs both: the task the work was compiled from, and the files the work touched. " +
				"The contract is recompiled from the intent against this index, which is why the two screens " +
				"agree by construction rather than by the reader pasting JSON between them.",
			Steps: []string{
				"Enter the same task text you compiled the contract from.",
				"Paste the changed paths: git diff --name-only",
				"cortex verify --contract contract.json --changed $(git diff --name-only | paste -sd, -)",
			},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if len(changed) == 0 {
		resp.Status = VerifyNoChanges
		resp.Help = &Guidance{
			Title:  "Name the files the change touched",
			Detail: "An empty change set is not a pass. With nothing to check, the verdict is `unverified`, which exits 2 — a failure of the setup, not of the change.",
			Steps:  []string{"git diff --name-only | paste -sd, -", "Paste that into the changed-files box."},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	contract, _, err := s.buildContract(ctx, run.ID, intent, maxScope)
	switch {
	case errors.Is(err, errNoPackReader):
		resp.Status = VerifyUnavailable
		resp.Help = &Guidance{
			Title:  "This build cannot compile the contract to verify against",
			Detail: "The context-pack port is not wired in this server. `cortex verify` on the command line has the same sources.",
			Steps:  []string{"cortex verify --contract contract.json --changed <paths>"},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}

	opts := []verify.Option{
		verify.WithArchitecture(archSource{reg: s.deps.Registry, model: s.deps.Config.Architecture}, run.ID),
	}
	if floor != "" {
		// verify can only RAISE the contract's own floor; passing a lower one
		// is ignored there rather than here, so the rule stays in one place.
		opts = append(opts, verify.WithMinConfidence(floor))
	}
	rep, err := verify.New(opts...).Verify(ctx, contract, changed)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	resp.Status = VerifyOK
	resp.Contract = &contract
	resp.Report = &rep
	resp.Verdict = string(rep.Disposition)
	resp.ExitCode = rep.Disposition.ExitCode()
	resp.Signal = signalFor(rep.Disposition)
	resp.Unverified = rep.Disposition == verify.DispositionUnverified
	resp.Questions = rep.Questions
	if resp.Questions == nil {
		resp.Questions = []verify.Question{}
	}
	resp.Unknowns = rep.Unknowns
	if resp.Unknowns == nil {
		resp.Unknowns = []string{}
	}

	for _, f := range rep.Findings {
		if f.Standing == verify.StandingFact {
			resp.Facts = append(resp.Facts, f)
		} else {
			resp.Leads = append(resp.Leads, f)
		}
		switch f.Kind {
		case verify.KindOutOfScopeEdit:
			resp.Counts.OutOfScopeEdits++
		case verify.KindArchitectureViolation:
			resp.Counts.ArchitectureViolations++
		case verify.KindMissedCaller:
			resp.Counts.MissedCallers++
		case verify.KindTestCoverageGap:
			resp.Counts.TestCoverageGaps++
		}
	}
	resp.Counts.Findings = len(rep.Findings)
	resp.Counts.Facts = rep.Facts()
	resp.Counts.Leads = rep.Leads()
	resp.Counts.ArchitectureConsulted = rep.Checked.ArchitectureConsulted
	resp.Counts.RulesEvaluated = rep.Checked.RulesEvaluated
	resp.Counts.BlastSymbolsChecked = rep.Checked.BlastSymbols
	resp.Counts.NamedTestsChecked = rep.Checked.NamedTests

	resp.Files, resp.FileCounts = placeChangedFiles(contract, changed)
	resp.Facts = capped(resp.Facts, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Facts beyond the first %d are not listed. The COUNT above is the full figure.", maxRows), "")
	resp.Leads = capped(resp.Leads, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Leads beyond the first %d are not listed.", maxRows), "")
	resp.Files = capped(resp.Files, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Changed files beyond the first %d are not listed.", maxRows), "")

	// verify's own account of what it did not search, restated as counted
	// omissions so this response's block is complete rather than pointing at a
	// field the reader has to go and find.
	for _, ex := range rep.Excluded {
		om.Hide(ReasonBelowConfidenceFloor, ex.Count, ex.Reason,
			"lower the contract's impact standard of proof, then verify against that contract")
		if ex.Count == 0 {
			om.Note(ReasonBelowConfidenceFloor, ex.Reason, "")
		}
	}
	if !rep.Checked.ArchitectureConsulted {
		om.Note(ReasonOutsideDeclaredComponents,
			"No architecture rule was evaluated for this change. That is NOT 'no rule was broken' — it is "+
				"'no rule was checked', and the two must not render alike.",
			"declare components in .cortex.yml")
	}
	for _, q := range rep.Questions {
		if q.Asked && !q.Answered {
			om.Note(ReasonRowBudget,
				"Question \""+q.Question+"\" ran and engaged nothing in this change: "+q.Detail,
				"widen the contract, or name the files this change was actually about")
		}
	}
	if len(contract.BlastRadius) == 0 {
		om.Note(ReasonUnresolvedReferences,
			"The contract carried no blast radius, so question 3 — did the change miss a caller? — could "+
				"not be asked at all.",
			"name a symbol the index holds in the task text")
	}

	switch rep.Disposition {
	case verify.DispositionFail:
		resp.Next = NextAction{
			Label:  fmt.Sprintf("%d fact(s) block this change", rep.Facts()),
			Detail: "Every fact rests on a compiler-proven relationship or on something directly observed. Fix them, then re-run.",
			Command: "cortex verify --contract contract.json --changed " +
				truncateIntent(strings.Join(changed, ",")),
		}
	case verify.DispositionAdvisory:
		resp.Next = NextAction{
			Label: fmt.Sprintf("%d lead(s) to read, none blocking", rep.Leads()),
			Detail: "A lead rests on a relationship inferred from names, measured below 37.5% precision. Worth a look; not worth " +
				"failing a build on, which is why this exits 0.",
			Command: "cortex verify --fail-on-leads --contract contract.json --changed <paths>",
		}
	case verify.DispositionUnverified:
		detail := "The contract declared no scope, no exclusions, no blast radius and no tests. " +
			"Exit code 2 says the setup failed, not the change."
		if rep.Checked.ChangedFiles > 0 && rep.Checked.DeclaredChangedFiles == 0 {
			detail = fmt.Sprintf("The contract took no position on any of the %d changed file(s), and no "+
				"other question engaged them either. Nothing here was verified — read the questions table: "+
				"every row says `asked` or not, and `answered` or not, and they are different claims. "+
				"Exit code 2 says the setup failed, not the change.", rep.Checked.ChangedFiles)
		}
		resp.Next = NextAction{
			Label:   "Nothing about this change was checked — this is not a pass",
			Detail:  detail,
			Screen:  "contract",
			Command: "cortex compile \"" + truncateIntent(intent) + "\"",
		}
	default:
		resp.Next = NextAction{
			Label:   "Wire this into CI",
			Detail:  fmt.Sprintf("Every question was asked over %d changed file(s) and none produced a finding.", len(changed)),
			Command: "cortex verify --contract contract.json --changed $(git diff --name-only | paste -sd, -)",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// signalFor maps a disposition to the traffic light, once, as data.
//
// `unverified` is its own signal rather than amber: amber says "look at this",
// and unverified says "this told you nothing". Collapsing it into either green
// or amber is the failure DispositionUnverified exists to prevent.
func signalFor(d verify.Disposition) string {
	switch d {
	case verify.DispositionPass:
		return "green"
	case verify.DispositionAdvisory:
		return "amber"
	case verify.DispositionFail:
		return "red"
	default:
		return "unverified"
	}
}

// placeChangedFiles is set membership against the contract's scope, nothing more.
//
// A file is IN SCOPE when the contract names any symbol in it, EXCLUDED when
// the contract named it as out of scope and nothing in it was in scope, and
// UNDECLARED otherwise. The middle rule is verify's, restated here so the two
// agree: a file routinely holds both an in-scope symbol and an excluded one,
// because the context engine ranks symbols independently, and reading that as a
// violation would fire on the first real change in every repository.
func placeChangedFiles(c compile.Contract, changed []string) ([]FileVerdict, FileCounts) {
	inScope := map[string][]string{}
	why := map[string]string{}
	for _, sc := range c.Scope {
		p := normalisePortalPath(sc.Path)
		if sc.Symbol != "" {
			inScope[p] = append(inScope[p], sc.Symbol)
		} else if _, ok := inScope[p]; !ok {
			inScope[p] = nil
		}
		if _, seen := why[p]; !seen {
			why[p] = sc.Why
		}
	}
	excluded := map[string]string{}
	for _, ex := range c.OutOfScope {
		p := normalisePortalPath(ex.Path)
		if _, seen := excluded[p]; !seen {
			excluded[p] = ex.Why
		}
	}

	var counts FileCounts
	seen := map[string]struct{}{}
	out := make([]FileVerdict, 0, len(changed))
	for _, raw := range changed {
		p := normalisePortalPath(raw)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		counts.Changed++

		v := FileVerdict{Path: p}
		switch syms, ok := inScope[p]; {
		case ok:
			v.Kind, v.Why, v.Symbols = FileInScope, why[p], syms
			counts.InScope++
		default:
			if reason, isEx := excluded[p]; isEx {
				v.Kind, v.Why = FileExcluded, reason
				counts.Excluded++
			} else {
				v.Kind = FileUndeclared
				v.Why = "The contract says nothing about this file. Not a violation — a brief is not a whitelist — " +
					"but it is a file the task did not anticipate."
				counts.Undeclared++
			}
		}
		sort.Strings(v.Symbols)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := fileVerdictRank(out[i].Kind), fileVerdictRank(out[j].Kind); ri != rj {
			return ri < rj
		}
		return out[i].Path < out[j].Path
	})
	return out, counts
}

// fileVerdictRank orders the listing so the accusations sort first.
func fileVerdictRank(k FileVerdictKind) int {
	switch k {
	case FileExcluded:
		return 0
	case FileUndeclared:
		return 1
	default:
		return 2
	}
}

// normalisePortalPath mirrors verify's own normalisation so the two agree about
// "./x.go" and "x.go".
func normalisePortalPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// changedParam reads the change set from the query string.
//
// Comma or newline separated, deduplicated by placeChangedFiles, bounded. No
// git, ever — see the file comment.
func changedParam(r *http.Request) []string {
	raw := r.URL.Query().Get("changed")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(c rune) bool {
		return c == ',' || c == '\n' || c == '\r' || c == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
		if len(out) >= maxChangedPaths {
			break
		}
	}
	return out
}
