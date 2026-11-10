// Package verify answers the question the loop never asked: did the change
// that was made honour the contract that was compiled?
//
// # The missing half
//
// internal/compile turns an informal request into an explicit task contract —
// scope, non-scope, constraints, blast radius, tests, each citing evidence.
// Then an agent works. Then, until this package, nothing. The contract was a
// brief nobody ever marked. Everything in this segment of the market is
// PRE-flight: a plan is checked before the work and never after it, which
// catches the tasks that were badly specified and none of the changes that
// ignored a well-specified one.
//
// So this asks four questions and refuses to ask a fifth:
//
//  1. Did the change stay inside the declared scope — did it edit a file the
//     contract explicitly excluded, and if so, what reason did the contract
//     give for excluding it?
//  2. Did it break a declared architecture rule?
//  3. Did it miss a caller the impact analysis flagged?
//  4. Do the tests the contract named still cover what changed?
//
// It is not a linter, a test runner, or a reviewer. Every other judgement about
// a diff already has a tool, and a verifier that also had opinions about naming
// would be switched off along with them.
//
// # Facts and leads
//
// internal/capability/architecture draws the distinction this report is built
// on: a violation on a compiler-proven edge is a FACT, and one inferred from a
// heuristic edge is a LEAD. That vocabulary is adopted rather than replaced,
// and it is what the disposition keys off — CI fails on facts and reports
// leads, because a gate that blocks a merge on a 2%-precision edge gets removed
// within a week and takes the facts with it.
//
// # What this refuses to do
//
// It does not call a model, read the network, or look at a clock, for the same
// reason internal/compile does not: a verdict that changed between two runs of
// the same inputs could not be gated on.
//
// It does not run git. The changed files are an INPUT. A verifier that shelled
// out to `git diff` would work in exactly one situation — a clean checkout of a
// branch — and not in the one that matters, an agent asking "is what I have in
// my working tree acceptable" before it writes anything anywhere.
//
// It does not report a finding it cannot evidence. Every Finding carries the
// evidence and the confidence it rests on, and the exit filter drops anything
// that does not, exactly as the compiler's does.
//
// # Determinism
//
// The same contract and the same changed-file set produce a byte-identical
// report. Every ordering is an explicit total sort and no map is ranged over
// into output.
package verify

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Standing is whether a finding is a fact or a lead.
//
// Deliberately the vocabulary internal/capability/architecture already uses in
// its package documentation rather than a second one: a relationship a compiler
// proved supports a claim that can block a merge, and a relationship inferred
// from a heuristic supports a claim that deserves a reader's attention and
// nothing more. Presenting them identically makes the report either alarmist or
// ignorable, and both end with it being switched off.
type Standing string

const (
	// StandingFact rests on a compiler-proven relationship (confidence
	// `exact`), or on something directly observed — a file that was edited and
	// that the contract said not to edit needs no inference at all.
	StandingFact Standing = "fact"

	// StandingLead rests on a heuristic relationship. Worth reading, never
	// worth failing a build on by itself.
	StandingLead Standing = "lead"
)

// Disposition is the whole report's verdict, and the thing CI gates on.
type Disposition string

const (
	// DispositionPass: every question was asked and none produced a finding.
	DispositionPass Disposition = "pass"

	// DispositionAdvisory: findings exist, all of them leads. The change is not
	// blocked; a human is told what to look at.
	DispositionAdvisory Disposition = "advisory"

	// DispositionFail: at least one finding is a fact.
	DispositionFail Disposition = "fail"

	// DispositionUnverified: there was nothing to check against — an empty
	// change set, or a contract that declared no scope, no exclusions, no blast
	// radius and no tests, with no architecture source supplied.
	//
	// This is NOT a pass, and the distinction is the whole point. "We looked and
	// found nothing wrong" and "there was nothing to look at" are different
	// statements, and a CI gate that treats the second as the first is a gate
	// that silently stops gating the day a contract stops being generated.
	DispositionUnverified Disposition = "unverified"
)

// ExitCode is the process status a CI gate should use.
//
// Advisory exits 0 on purpose: a lead is a thing to read, not a thing to block
// a merge on. Unverified exits 2 rather than 0 or 1 — it is neither a pass nor
// a failure of the change, it is a failure of the setup, and a pipeline should
// be able to tell those apart without parsing the report.
func (d Disposition) ExitCode() int {
	switch d {
	case DispositionFail:
		return 1
	case DispositionUnverified:
		return 2
	default:
		return 0
	}
}

// Kind names which of the four questions produced a finding. There are four,
// and adding a fifth means this package has started doing something else.
type Kind string

const (
	KindOutOfScopeEdit        Kind = "out_of_scope_edit"
	KindArchitectureViolation Kind = "architecture_violation"
	KindMissedCaller          Kind = "missed_caller"
	KindTestCoverageGap       Kind = "test_coverage_gap"
)

// kindRank fixes the order findings are reported in: the contract's own
// boundaries first, then the repository's declared rules, then the
// consequences, then the checks. Sorting by severity instead would put a
// heuristic `high` above a compiler-proven scope breach.
func kindRank(k Kind) int {
	switch k {
	case KindOutOfScopeEdit:
		return 0
	case KindArchitectureViolation:
		return 1
	case KindMissedCaller:
		return 2
	case KindTestCoverageGap:
		return 3
	default:
		return 4
	}
}

// Finding is one answer that was not the answer the contract wanted.
type Finding struct {
	Kind Kind `json:"kind"`

	// Summary states the finding in one sentence.
	Summary string `json:"summary"`

	// Path and Symbol locate it. Path is the changed file the finding is about
	// wherever there is one, so a report can be read alongside a diff.
	Path   string `json:"path,omitempty"`
	Symbol string `json:"symbol,omitempty"`

	// Evidence is what makes the finding checkable: a file and line, a rule id,
	// or the contract's own words. A finding without it is an opinion and is
	// dropped at the exit filter.
	Evidence string `json:"evidence"`

	// Confidence is the confidence of the relationship the finding rests on,
	// carried through unchanged from whatever produced it.
	Confidence model.Confidence `json:"confidence,omitempty"`

	// Standing follows from Confidence and is stated separately so a consumer
	// does not have to re-derive the rule.
	Standing Standing `json:"standing"`

	// Severity is high | medium | low. For an architecture violation it is the
	// severity the RULE declared — this package does not re-rank someone else's
	// rule.
	Severity string `json:"severity,omitempty"`

	// Detail carries the source's own words verbatim where there are any: the
	// contract's reason for excluding a file, the rule's description. Never
	// paraphrased — restating an exclusion in this package's words would put a
	// second, unsourced claim next to a checkable one.
	Detail string `json:"detail,omitempty"`
}

// Exclusion is a region of the answer that was deliberately not searched.
//
// Named for the same reason impact.Report.LowConfidenceHidden is: a missed
// caller reported on a `low`-confidence edge is an accusation with a 2%
// precision behind it (docs/accuracy.md), so the floor is right — but a floor
// nobody states is indistinguishable from an analysis that found nothing.
type Exclusion struct {
	Reason string `json:"reason"`
	Count  int    `json:"count,omitempty"`
}

// Checked records what the verification actually covered, so a reader can tell
// a clean report from an empty one.
type Checked struct {
	ChangedFiles    int `json:"changed_files"`
	ScopeEntries    int `json:"scope_entries"`
	ExclusionsKnown int `json:"out_of_scope_entries"`
	BlastSymbols    int `json:"blast_radius_symbols"`
	NamedTests      int `json:"named_tests"`

	// DeclaredChangedFiles is how many of the changed files the contract took
	// ANY position on — named in scope, or named out of scope. The number that
	// decides whether this verification marked the change against a brief or
	// merely ran beside it. Zero means the contract said nothing about anything
	// that was edited.
	DeclaredChangedFiles int `json:"declared_changed_files"`

	// ChangedScopeSymbols is how many scope symbols live in a changed file: the
	// population questions 3 and 4 are asked about.
	ChangedScopeSymbols int `json:"changed_scope_symbols"`

	// AttributedViolations is how many architecture violations were attributed
	// to a changed file, as opposed to found elsewhere in the repository.
	AttributedViolations int `json:"attributed_violations"`

	// ArchitectureConsulted distinguishes "no rule was broken" from "no rule
	// was checked".
	ArchitectureConsulted bool `json:"architecture_consulted"`
	// RulesEvaluated is carried from the architecture report.
	RulesEvaluated int `json:"rules_evaluated,omitempty"`
}

// Question is one of the four, and what it managed to do with THIS change set.
//
// # Why "asked" was not enough
//
// The report already distinguished a question that could not be asked from one
// that found nothing. What it did not distinguish is a question that was asked
// of the repository but engaged none of the changed files — and that is the gap
// a green PASS fell through. A contract with twelve scope entries, none of them
// naming a file that was edited, made the scope check runnable and vacuous, and
// the disposition read "every question was asked and none produced a finding".
//
// So each question now reports three things and they are different:
//
//	Asked     the inputs existed to ask it at all.
//	Answered  it actually engaged this change set — there was something in the
//	          change for it to have an opinion about.
//	Covered   how much, in the unit that question works in.
//
// A verdict of `pass` requires at least one Answered question. Nothing else in
// this package is allowed to derive that from the other fields, because that is
// precisely the derivation that was got wrong.
type Question struct {
	ID string `json:"id"`

	// Question is the question in words, so a report is readable without this
	// package's source beside it.
	Question string `json:"question"`

	Asked    bool `json:"asked"`
	Answered bool `json:"answered"`
	Covered  int  `json:"covered"`
	Findings int  `json:"findings"`

	// Unit names what Covered counts.
	Unit string `json:"unit"`

	// Detail says what happened, in the terms a reader needs to act on.
	Detail string `json:"detail"`
}

// The four question ids, in the fixed order they are reported.
const (
	QuestionScope         = "scope"
	QuestionArchitecture  = "architecture"
	QuestionMissedCallers = "missed_callers"
	QuestionTests         = "tests"
)

// Report is one verification.
type Report struct {
	Intent      string      `json:"intent,omitempty"`
	Disposition Disposition `json:"disposition"`

	Findings []Finding `json:"findings"`

	// Excluded is what the confidence floor and the inputs kept out of the
	// search, sized where it can be sized.
	Excluded []Exclusion `json:"excluded,omitempty"`

	// Unknowns are the parts of the four questions that could not be answered
	// from the inputs given. Never suppressed, for the same reason
	// compile.Contract.Unknowns is not.
	Unknowns []string `json:"unknowns,omitempty"`

	Checked Checked `json:"checked"`

	// Questions is the per-question account of what this run engaged. See the
	// Question type: it is what makes `unverified` derivable rather than
	// guessed at, and it is in the fixed order scope, architecture, missed
	// callers, tests.
	Questions []Question `json:"questions"`

	// changedTargets is checkMissedCallers' working figure — how many analysed
	// scope symbols sat in a changed file. Unexported: it is an input to the
	// question summary, not a second public count of the same thing as
	// Checked.ChangedScopeSymbols.
	changedTargets int
}

// Answered reports whether ANY of the four questions engaged the change set.
//
// The one predicate `pass` depends on. A run where nothing was answered checked
// nothing about this change, whatever else it did.
func (r Report) Answered() bool {
	for _, q := range r.Questions {
		if q.Answered {
			return true
		}
	}
	return false
}

// Facts and Leads count the findings by standing.
func (r Report) Facts() int { return r.countStanding(StandingFact) }

// Leads counts findings that rest on a heuristic relationship.
func (r Report) Leads() int { return r.countStanding(StandingLead) }

func (r Report) countStanding(s Standing) int {
	n := 0
	for _, f := range r.Findings {
		if f.Standing == s {
			n++
		}
	}
	return n
}

// Option configures a Verifier. Functional options per CLAUDE.md §2: the two
// things a caller might supply are independent, and a config struct would make
// the zero value ambiguous between "no architecture declared" and "do not
// check".
type Option func(*Verifier)

// WithArchitecture supplies the post-change architecture check and the run to
// check it against. Without it, question 2 is not asked and the report says so
// rather than reporting no violations.
func WithArchitecture(src ArchitectureSource, runID int64) Option {
	return func(v *Verifier) {
		v.arch = src
		v.runID = runID
	}
}

// WithMinConfidence raises the confidence floor this verification acts on.
//
// It can only RAISE it. The contract already carries the floor its impact
// analysis ran with, and a verifier that lowered it would be reporting missed
// callers across edges the analysis itself declined to walk — findings the
// contract's own evidence does not support.
func WithMinConfidence(c model.Confidence) Option {
	return func(v *Verifier) { v.minConfidence = c }
}

// Verifier checks changes against contracts. All state is here and injected;
// there is no package-level anything (CLAUDE.md §4.2).
type Verifier struct {
	arch          ArchitectureSource
	runID         int64
	minConfidence model.Confidence
}

// New returns a Verifier. The zero-option Verifier is useful: it answers
// questions 1, 3 and 4 from the contract alone, which is what an agent running
// before it has re-indexed can actually do.
func New(opts ...Option) *Verifier {
	v := &Verifier{}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Verify checks a set of changed files against the contract that authorised
// them.
//
// changed is repo-relative paths, in any order and with any duplication; it is
// normalised here so that two callers who disagree about "./" produce the same
// report. Returns an error only when a supplied port fails — every other
// outcome is a Report, including the one where nothing could be checked.
func (v *Verifier) Verify(ctx context.Context, c compile.Contract, changed []string) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("verify: %w", err)
	}

	files := normalisePaths(changed)
	changedSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		changedSet[f] = struct{}{}
	}

	rep := Report{
		Intent: c.Intent,
		Checked: Checked{
			ChangedFiles:    len(files),
			ScopeEntries:    len(c.Scope),
			ExclusionsKnown: len(c.OutOfScope),
			BlastSymbols:    len(c.BlastRadius),
			NamedTests:      len(c.Tests),
		},
	}

	var findings []Finding
	findings = append(findings, v.checkScope(c, changedSet, &rep)...)

	archFindings, err := v.checkArchitecture(ctx, changedSet, &rep)
	if err != nil {
		return Report{}, err
	}
	findings = append(findings, archFindings...)
	findings = append(findings, v.checkMissedCallers(c, changedSet, &rep)...)
	findings = append(findings, v.checkTests(c, changedSet, &rep)...)

	rep.Findings = filterAndSort(findings)
	rep.Questions = summariseQuestions(rep, c)
	rep.Disposition = disposition(rep, len(files))
	if rep.Disposition == DispositionUnverified && len(files) > 0 && !rep.Answered() {
		rep.Unknowns = append(rep.Unknowns,
			"None of the four questions engaged this change set, so nothing about this change was "+
				"verified. This is reported as `unverified` (exit 2) rather than `pass` (exit 0): "+
				"\"we looked and found nothing\" and \"there was nothing to look at\" are different "+
				"statements, and a gate that treats the second as the first stops gating the day a "+
				"contract stops matching the work.")
	}
	return rep, nil
}

// ------------------------------------------------------------------ scope

// checkScope answers question 1.
//
// The match is on the FILE, exactly, because that is what the input is. An
// exclusion that names only a symbol cannot be checked against a list of paths,
// and is reported as an unknown instead of being guessed at from the file it
// probably lives in.
//
// Scope and non-scope are both SYMBOL-level, and one file routinely holds both:
// the context engine ranks each symbol independently, so `store.go` can supply
// two symbols the task needs and three it does not. Reading a file-level edit as
// a violation because SOME symbol in it was excluded would fire on the first
// real change in every repository — a file is out of scope only when nothing in
// it was in scope. This is the check refusing to convert a symbol-level judgment
// into a file-level accusation it cannot support.
func (v *Verifier) checkScope(c compile.Contract, changed map[string]struct{}, rep *Report) []Finding {
	var out []Finding
	symbolOnly := 0
	shadowed := 0

	inScope := make(map[string]struct{}, len(c.Scope))
	for _, s := range c.Scope {
		if p := normalisePath(s.Path); p != "" {
			inScope[p] = struct{}{}
		}
	}

	for _, ex := range c.OutOfScope {
		p := normalisePath(ex.Path)
		if p == "" {
			symbolOnly++
			continue
		}
		if _, ok := changed[p]; !ok {
			continue
		}
		if _, ok := inScope[p]; ok {
			shadowed++
			continue
		}
		reason := strings.TrimSpace(ex.Why)
		out = append(out, Finding{
			Kind:   KindOutOfScopeEdit,
			Symbol: ex.Symbol,
			Path:   p,
			Summary: fmt.Sprintf(
				"%s was edited, and the contract listed it as out of scope.", p),
			// Directly observed: the file is in the change set and in the
			// contract's exclusion list. Nothing was inferred, so this is a
			// fact whatever the index believes about anything.
			Evidence:   fmt.Sprintf("contract out-of-scope entry for %s", excludedRef(ex, p)),
			Confidence: model.ConfidenceExact,
			Standing:   StandingFact,
			Severity:   "high",
			// The contract's own words, verbatim. Rewriting the reason would
			// replace a claim the context engine can defend with one this
			// package cannot.
			Detail: reason,
		})
	}

	if symbolOnly > 0 {
		rep.Unknowns = append(rep.Unknowns, fmt.Sprintf(
			"%d out-of-scope entr(ies) name a symbol but no file, so an edit to them cannot be detected from a list of changed files.",
			symbolOnly))
	}

	// Stated, not swallowed. These are the exclusions this check declined to
	// enforce, and a reader who wants symbol-level enforcement needs to know the
	// count rather than infer it from a clean report.
	if shadowed > 0 {
		rep.Unknowns = append(rep.Unknowns, fmt.Sprintf(
			"%d out-of-scope entr(ies) name a file that ALSO holds an in-scope symbol. The file legitimately had to change, so the edit was not flagged; whether the excluded symbol inside it was touched is not answerable from a list of changed files.",
			shadowed))
	}

	// Files the contract took no position on. Counted, not accused: a contract
	// bounded at twelve scope entries will routinely be silent about a file
	// that legitimately had to change, and turning that silence into a finding
	// would make the report noise on its first real change.
	undeclared := undeclaredFiles(c, changed)
	rep.Checked.DeclaredChangedFiles = len(changed) - undeclared
	if undeclared > 0 {
		rep.Unknowns = append(rep.Unknowns, fmt.Sprintf(
			"%d changed file(s) appear in neither Scope nor Out of scope; the contract took no position on them, so this check has nothing to say about whether they belonged in the change.",
			undeclared))
	}
	return out
}

func undeclaredFiles(c compile.Contract, changed map[string]struct{}) int {
	declared := make(map[string]struct{}, len(c.Scope)+len(c.OutOfScope))
	for _, s := range c.Scope {
		if p := normalisePath(s.Path); p != "" {
			declared[p] = struct{}{}
		}
	}
	for _, ex := range c.OutOfScope {
		if p := normalisePath(ex.Path); p != "" {
			declared[p] = struct{}{}
		}
	}
	n := 0
	// Ranged over a map only to COUNT. No ordering decision depends on it, and
	// the count is the same whatever order it arrives in.
	for f := range changed {
		if _, ok := declared[f]; !ok {
			n++
		}
	}
	return n
}

func excludedRef(ex compile.ScopeItem, p string) string {
	if ex.Symbol != "" {
		return ex.Symbol + " (" + p + ")"
	}
	return p
}

// ----------------------------------------------------------- architecture

// checkArchitecture answers question 2 by asking the capability that already
// answers it, then keeping only the violations this change is responsible for.
//
// The attribution matters. A repository with a pre-existing violation would
// otherwise fail every verification forever, and a gate that is always red is a
// gate that gets bypassed. A violation is attributed to the change when the
// file the violating relationship lives in is one of the changed files.
func (v *Verifier) checkArchitecture(ctx context.Context, changed map[string]struct{}, rep *Report) ([]Finding, error) {
	if v.arch == nil {
		rep.Unknowns = append(rep.Unknowns,
			"No architecture check was supplied, so no declared rule was evaluated against this change. An empty architecture section here means nobody looked, not that nothing was broken.")
		return nil, nil
	}

	archRep, err := v.arch.Check(ctx, v.runID, v.minConfidence)
	if err != nil {
		if isNoModel(err) {
			rep.Unknowns = append(rep.Unknowns,
				"The repository declares no architecture, so there was no rule for this change to break. Declaring components in .cortex.yml is what makes this question answerable.")
			return nil, nil
		}
		return nil, fmt.Errorf("verify: architecture check for run %d: %w", v.runID, err)
	}
	rep.Checked.ArchitectureConsulted = true
	rep.Checked.RulesEvaluated = archRep.RulesEvaluated

	var out []Finding
	preexisting := 0
	for _, vi := range archRep.Violations {
		p := normalisePath(vi.FilePath)
		if _, ok := changed[p]; !ok {
			preexisting++
			continue
		}
		rep.Checked.AttributedViolations++
		conf := model.Confidence(vi.Confidence)
		out = append(out, Finding{
			Kind:   KindArchitectureViolation,
			Path:   p,
			Symbol: vi.FromName,
			Summary: fmt.Sprintf(
				"%s violates architecture rule %s: %s (%s) %s %s (%s).",
				p, vi.RuleID, vi.FromName, vi.From, verbFor(vi.Kind), vi.ToName, vi.To),
			Evidence: fmt.Sprintf("%s:%d — %s %s %s, %s confidence; rule %s, verdict %s",
				vi.FilePath, vi.Line, vi.FromName, vi.Kind, vi.ToName, vi.Confidence, vi.RuleID, vi.Verdict),
			Confidence: conf,
			Standing:   standingFor(conf),
			// The rule's own severity. Re-ranking it here would let this
			// package overrule the person who wrote the rule.
			Severity: vi.Severity,
			Detail:   vi.Detail,
		})
	}
	if preexisting > 0 {
		rep.Excluded = append(rep.Excluded, Exclusion{
			Reason: "architecture violations in files this change did not touch (pre-existing; not attributed to this change)",
			Count:  preexisting,
		})
	}
	return out, nil
}

func verbFor(kind string) string {
	switch strings.ToLower(kind) {
	case "calls":
		return "calls"
	case "reads":
		return "reads"
	case "writes":
		return "writes"
	case "implements":
		return "implements"
	case "extends":
		return "extends"
	default:
		return "depends on"
	}
}

// isNoModel recognises "no architecture declared", which is not a failure.
//
// A repository that declared no architecture must not fail verification for it:
// there was no rule to break. The distinction between that and a clean check is
// kept in Unknowns rather than collapsed into a pass.
func isNoModel(err error) bool {
	return errors.Is(err, architecture.ErrNoModel)
}

// ---------------------------------------------------------- missed caller

// checkMissedCallers answers question 3.
//
// A blast-radius symbol whose file was not touched is the finding — but only
// when the symbol it was reached FROM was actually changed. Without that
// condition every contract whose scope was not touched would report its whole
// blast radius as missed, which is not a finding about the change at all.
func (v *Verifier) checkMissedCallers(c compile.Contract, changed map[string]struct{}, rep *Report) []Finding {
	if len(c.BlastRadius) == 0 {
		if len(c.Scope) > 0 {
			rep.Unknowns = append(rep.Unknowns,
				"The contract carries no blast radius, so this check cannot say whether a caller was missed. Compile with an impact source to make this question answerable.")
		}
		return nil
	}

	// Which analysed targets actually changed. A target is changed when the
	// scope entry that names it sits in a changed file.
	changedTargets := make(map[string]struct{}, len(c.Scope))
	for _, s := range c.Scope {
		if s.Symbol == "" {
			continue
		}
		if _, ok := changed[normalisePath(s.Path)]; ok {
			changedTargets[s.Symbol] = struct{}{}
		}
	}
	rep.changedTargets = len(changedTargets)
	if len(changedTargets) == 0 {
		rep.Unknowns = append(rep.Unknowns,
			"No file defining a scope symbol was changed, so no caller can be said to have been missed: there is nothing for a caller to have been missed for.")
		return nil
	}

	// The floor. The contract's blast radius was already walked under one, and
	// WithMinConfidence may raise it; nothing here may lower it. Whatever the
	// floor keeps out is stated, because accusing an author of missing a caller
	// on an edge that is wrong 97% of the time (docs/accuracy.md) is worse than
	// saying nothing.
	floor := model.Confidence(c.ImpactFloor)
	if v.minConfidence.Rank() > floor.Rank() {
		floor = v.minConfidence
	}

	var out []Finding
	belowFloor := 0
	inTests := 0
	for _, r := range c.BlastRadius {
		if r.Target != "" {
			if _, ok := changedTargets[r.Target]; !ok {
				continue
			}
		}
		p := normalisePath(r.Path)
		if p == "" {
			continue
		}
		if _, ok := changed[p]; ok {
			continue // the caller's file was touched; nothing was missed
		}
		conf := model.Confidence(r.Confidence)
		if floor.Valid() && !conf.AtLeast(floor) {
			belowFloor++
			continue
		}
		if r.IsTest {
			// A test that references the changed symbol is question 4's
			// business, and reporting it here as well would double-count the
			// same file under two headings.
			inTests++
			continue
		}
		out = append(out, Finding{
			Kind:   KindMissedCaller,
			Path:   p,
			Symbol: r.Symbol,
			Summary: fmt.Sprintf(
				"%s in %s is in the blast radius of %s, and %s was not changed.",
				r.Symbol, p, r.Target, p),
			Evidence:   missedCallerEvidence(r),
			Confidence: conf,
			Standing:   standingFor(conf),
			Severity:   missedCallerSeverity(r, conf),
		})
	}

	if belowFloor > 0 {
		rep.Excluded = append(rep.Excluded, Exclusion{
			Reason: fmt.Sprintf(
				"blast-radius symbols reached across edges weaker than the %q floor this contract was compiled with", string(floor)),
			Count: belowFloor,
		})
	}
	if inTests > 0 {
		rep.Excluded = append(rep.Excluded, Exclusion{
			Reason: "blast-radius symbols that are test code (reported under the test check, not as missed callers)",
			Count:  inTests,
		})
	}
	if c.ImpactHidden > 0 {
		rep.Excluded = append(rep.Excluded, Exclusion{
			Reason: fmt.Sprintf(
				"symbols the impact analysis itself never listed, because every path to them crossed an edge weaker than %q; they were not searched here either",
				c.ImpactFloor),
			Count: c.ImpactHidden,
		})
	}
	return out
}

func missedCallerEvidence(r compile.Reached) string {
	parts := []string{fmt.Sprintf("impact analysis of %s reached %s at depth %d (%s confidence)",
		r.Target, r.Ref(), r.Depth, defaultConfidence(r.Confidence))}
	if r.Evidence != "" {
		parts = append(parts, r.Evidence)
	}
	return strings.Join(parts, "; ")
}

// missedCallerSeverity ranks by how close and how well-proven the relationship
// is, never by how many there are. Forty inferred callers are not worse than
// one proven one; they are forty times as much reading.
func missedCallerSeverity(r compile.Reached, conf model.Confidence) string {
	switch {
	case r.Depth <= 1 && conf == model.ConfidenceExact:
		return "high"
	case r.Depth <= 1:
		return "medium"
	default:
		return "low"
	}
}

// ------------------------------------------------------------------ tests

// checkTests answers question 4.
//
// The claim being checked is the contract's own: it named tests against
// specific symbols. For every scope symbol whose file was changed, either a
// named test covers it or it does not, and both answers are reported the way
// internal/capability/tests reports coverage — with the SIGNAL that found it,
// because a test found by a proven call chain and one found because its name
// looked right are not the same evidence and must not read as though they were.
func (v *Verifier) checkTests(c compile.Contract, changed map[string]struct{}, rep *Report) []Finding {
	// Changed scope symbols, in the contract's own order (already a total sort).
	var changedSymbols []compile.ScopeItem
	for _, s := range c.Scope {
		if s.Symbol == "" || s.Kind == "test" {
			continue
		}
		if _, ok := changed[normalisePath(s.Path)]; ok {
			changedSymbols = append(changedSymbols, s)
		}
	}
	rep.Checked.ChangedScopeSymbols = len(changedSymbols)
	if len(changedSymbols) == 0 {
		return nil
	}
	if len(c.Tests) == 0 {
		rep.Unknowns = append(rep.Unknowns, fmt.Sprintf(
			"The contract names no covering test, so for the %d changed scope symbol(s) there is nothing to check coverage against. That is a gap in the contract, not evidence that the change is untested.",
			len(changedSymbols)))
		return nil
	}

	// Group the named tests by the symbol they were listed against. Built once,
	// read in the deterministic order of changedSymbols; nothing is ranged over
	// this map into output.
	byCovers := make(map[string][]compile.TestRef, len(c.Tests))
	for _, t := range c.Tests {
		byCovers[t.Covers] = append(byCovers[t.Covers], t)
	}

	var out []Finding
	for _, s := range changedSymbols {
		hits := byCovers[s.Symbol]
		if len(hits) == 0 {
			out = append(out, Finding{
				Kind:   KindTestCoverageGap,
				Path:   normalisePath(s.Path),
				Symbol: s.Symbol,
				Summary: fmt.Sprintf(
					"%s changed, and none of the tests this contract named was listed as covering it.",
					s.Ref()),
				Evidence: fmt.Sprintf("contract names %d covering test(s), none against %s",
					len(c.Tests), s.Symbol),
				// A lead, always. Test discovery combines proven call chains
				// with naming conventions, so its ABSENCE is the weakest claim
				// in this package: a test may well exercise this symbol through
				// a path the index never resolved.
				Confidence: model.ConfidenceLow,
				Standing:   StandingLead,
				Severity:   "medium",
			})
			continue
		}
		// A covering test exists. It is still worth a lead when the change did
		// not touch it AND the only evidence it covers anything is its name or
		// its directory.
		if strongestSignalIsWeak(hits) && !anyTestFileChanged(hits, changed) {
			out = append(out, Finding{
				Kind:   KindTestCoverageGap,
				Path:   normalisePath(s.Path),
				Symbol: s.Symbol,
				Summary: fmt.Sprintf(
					"%s changed, and the only test named for it was found by convention rather than by a proven call chain, so it may not exercise this change at all.",
					s.Ref()),
				Evidence:   coveringEvidence(hits),
				Confidence: weakestConfidence(hits),
				Standing:   StandingLead,
				Severity:   "low",
			})
		}
	}
	return out
}

// strongestSignalIsWeak reports that no named test was found by the graph — the
// only signal that is evidence of execution rather than of intent. The signal
// vocabulary is internal/capability/tests', matched by value rather than
// re-declared, because a contract carries whatever string that package wrote.
func strongestSignalIsWeak(hits []compile.TestRef) bool {
	for _, t := range hits {
		if strings.EqualFold(strings.TrimSpace(t.Signal), "graph") {
			return false
		}
		if strings.TrimSpace(t.Signal) == "" {
			// An unlabelled signal is not evidence of weakness. Treated as
			// sufficient rather than guessed at downward, so this check cannot
			// manufacture a finding out of a missing field.
			return false
		}
	}
	return true
}

func anyTestFileChanged(hits []compile.TestRef, changed map[string]struct{}) bool {
	for _, t := range hits {
		if _, ok := changed[normalisePath(t.Path)]; ok {
			return true
		}
	}
	return false
}

func coveringEvidence(hits []compile.TestRef) string {
	refs := make([]string, 0, len(hits))
	for _, t := range hits {
		refs = append(refs, fmt.Sprintf("%s found by %s (%s confidence)",
			t.Ref(), t.Signal, defaultConfidence(t.Confidence)))
	}
	sort.Strings(refs)
	return strings.Join(refs, "; ")
}

func weakestConfidence(hits []compile.TestRef) model.Confidence {
	weakest := model.Confidence("")
	for _, t := range hits {
		c := model.Confidence(t.Confidence)
		if weakest == "" || c.Rank() < weakest.Rank() {
			weakest = c
		}
	}
	return weakest
}

// ------------------------------------------------------------------ exit

// filterAndSort is the single exit filter, and the total order.
//
// Same rule as internal/compile's: a finding that cites nothing is dropped
// here, at one place a future check cannot forget to consult, rather than each
// check being trusted to have remembered.
func filterAndSort(in []Finding) []Finding {
	kept := make([]Finding, 0, len(in))
	for _, f := range in {
		if strings.TrimSpace(f.Summary) == "" || strings.TrimSpace(f.Evidence) == "" {
			continue
		}
		if f.Standing == "" {
			f.Standing = StandingLead
		}
		kept = append(kept, f)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		switch {
		case kindRank(a.Kind) != kindRank(b.Kind):
			return kindRank(a.Kind) < kindRank(b.Kind)
		case a.Standing != b.Standing:
			// Facts before leads within a kind: the reader meets the
			// unarguable half of the report first.
			return a.Standing == StandingFact
		case severityRank(a.Severity) != severityRank(b.Severity):
			return severityRank(a.Severity) > severityRank(b.Severity)
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Symbol != b.Symbol:
			return a.Symbol < b.Symbol
		default:
			return a.Summary < b.Summary
		}
	})
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// standingFor applies the rule this package inherited: a compiler proved it, or
// it is a lead. `high` confidence is deliberately NOT a fact — pkg/model
// defines it as "resolved through an explicit import plus an unambiguous
// receiver type", which is an inference that shadowing and reflection defeat,
// and a build should not go red on an inference.
func standingFor(c model.Confidence) Standing {
	if c == model.ConfidenceExact {
		return StandingFact
	}
	return StandingLead
}

// disposition turns the findings into the verdict CI reads.
//
// # The rule that changed, and why
//
// This used to ask whether the CONTRACT was empty. It is the wrong question: a
// contract can carry twelve scope entries, eight exclusions and a full blast
// radius and still take no position on any file the change actually touched.
// Reproduced live against a real repository: two changed files, `0 in scope ·
// 0 explicitly excluded · 2 undeclared`, and the page rendered a large green
// PASS with exit 0 — beside its own unknowns card saying "the contract took no
// position on them, so this check has nothing to say about whether they
// belonged". Both statements were generated by this package, on the same run,
// and they contradicted each other.
//
// The right question is whether any of the four questions ENGAGED THE CHANGE
// SET, which is what Question.Answered records. A run where none did verified
// nothing about this change, and `unverified` — exit 2, a failure of the setup
// rather than of the change — is the verdict that says so.
//
// Order matters here. Findings are checked first on purpose: a finding is proof
// that something was examined, so a run that produced one is never unverified,
// whatever the coverage arithmetic says about the rest of the change.
func disposition(rep Report, changedFiles int) Disposition {
	if changedFiles == 0 {
		return DispositionUnverified
	}
	for _, f := range rep.Findings {
		if f.Standing == StandingFact {
			return DispositionFail
		}
	}
	if len(rep.Findings) > 0 {
		return DispositionAdvisory
	}
	if !rep.Answered() {
		return DispositionUnverified
	}
	return DispositionPass
}

// summariseQuestions is the single place the four questions account for
// themselves. Fixed order, one entry each, always present — a question that
// could not run is a row saying so, never an absent row.
func summariseQuestions(rep Report, c compile.Contract) []Question {
	byKind := map[Kind]int{}
	for _, f := range rep.Findings {
		byKind[f.Kind]++
	}
	ck := rep.Checked

	scope := Question{
		ID: QuestionScope, Unit: "changed files the contract took a position on",
		Question: "Did the change stay inside the declared scope?",
		Asked:    len(c.Scope) > 0 || len(c.OutOfScope) > 0,
		Covered:  ck.DeclaredChangedFiles,
		Findings: byKind[KindOutOfScopeEdit],
	}
	scope.Answered = scope.Asked && scope.Covered > 0
	switch {
	case !scope.Asked:
		scope.Detail = "The contract declared no scope and no exclusions, so there was no boundary for " +
			"the change to stay inside."
	case !scope.Answered:
		scope.Detail = fmt.Sprintf("The contract declared %d scope entr(ies) and %d exclusion(s), and "+
			"none of them names a file in this change. The check ran and had nothing to say: it did not "+
			"find the change acceptable, it found the change unrelated to the brief.",
			ck.ScopeEntries, ck.ExclusionsKnown)
	default:
		scope.Detail = fmt.Sprintf("%d of %d changed file(s) are named by the contract.",
			scope.Covered, ck.ChangedFiles)
	}

	arch := Question{
		ID: QuestionArchitecture, Unit: "violations attributed to a changed file",
		Question: "Did the change break a declared architecture rule?",
		Asked:    ck.ArchitectureConsulted,
		Covered:  ck.AttributedViolations,
		Findings: byKind[KindArchitectureViolation],
	}
	arch.Answered = arch.Asked && arch.Covered > 0
	switch {
	case !arch.Asked:
		arch.Detail = "No architecture check ran, so no declared rule was evaluated. That is not " +
			"'no rule was broken'."
	case !arch.Answered:
		arch.Detail = fmt.Sprintf("%d rule(s) were evaluated over the repository and no violation was "+
			"recorded in a file this change touched. That is a repository-level result: whether the "+
			"changed files carry any indexed relationship at all is not answerable from here, so this "+
			"question is not counted as having verified the change on its own.", ck.RulesEvaluated)
	default:
		arch.Detail = fmt.Sprintf("%d violation(s) were attributed to a changed file.", arch.Covered)
	}

	callers := Question{
		ID: QuestionMissedCallers, Unit: "changed scope symbols with an analysed blast radius",
		Question: "Did the change miss a caller the impact analysis flagged?",
		Asked:    len(c.BlastRadius) > 0,
		Covered:  rep.changedTargets,
		Findings: byKind[KindMissedCaller],
	}
	callers.Answered = callers.Asked && callers.Covered > 0
	switch {
	case !callers.Asked:
		callers.Detail = "The contract carries no blast radius, so this question could not be asked."
	case !callers.Answered:
		callers.Detail = "No file defining a scope symbol was changed, so no caller can be said to have " +
			"been missed: there is nothing for a caller to have been missed for."
	default:
		callers.Detail = fmt.Sprintf("%d changed scope symbol(s) had their callers checked.", callers.Covered)
	}

	tests := Question{
		ID: QuestionTests, Unit: "changed scope symbols",
		Question: "Do the tests the contract named still cover what changed?",
		Asked:    len(c.Tests) > 0,
		Covered:  ck.ChangedScopeSymbols,
		Findings: byKind[KindTestCoverageGap],
	}
	tests.Answered = tests.Asked && tests.Covered > 0
	switch {
	case !tests.Asked:
		tests.Detail = "The contract names no covering test, so there is nothing to check coverage against."
	case !tests.Answered:
		tests.Detail = "No changed file defines a scope symbol, so no named test had anything to cover " +
			"in this change."
	default:
		tests.Detail = fmt.Sprintf("%d changed scope symbol(s) were checked against %d named test(s).",
			tests.Covered, ck.NamedTests)
	}

	return []Question{scope, arch, callers, tests}
}

// ------------------------------------------------------------------ paths

// normalisePaths cleans, de-duplicates and sorts a changed-file list.
//
// Sorted because two callers who list the same files in a different order are
// making the same statement, and a report that differs between them could not
// be diffed across runs — which is the property a CI gate is built on.
func normalisePaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, p := range in {
		n := normalisePath(p)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// normalisePath puts one path in the form the index uses: repo-relative,
// forward slashes, no "./" prefix. A Windows-style separator arrives from CI
// runners often enough to be worth handling here rather than in every caller.
func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "./")
	if p == "." || p == "/" {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

func defaultConfidence(c string) string {
	if strings.TrimSpace(c) == "" {
		return "unstated"
	}
	return c
}
