// Package bench measures whether the context engine retrieved the right code.
//
// # Why a benchmark and not more unit tests
//
// internal/context is a ranking system, and its unit tests assert that each
// individual signal does what its comment claims. None of them can answer the
// question a user actually has: given a real task against a real repository,
// did the pack contain the code an engineer would have opened? That question
// has no closed form, so it is answered the way retrieval quality has always
// been answered — against hand-written cases in which a human wrote down what
// the answer must contain, and what it must not.
//
// Without this package, every claim about retrieval quality is an assertion.
//
// # What this deliberately does not do
//
// It does not run an AI model. An LLM judge would make the score depend on a
// model version and a sampling temperature, and the regression this most needs
// to catch — a pack that quietly stopped containing the payment controller — is
// exactly the one a generous judge forgives.
//
// It does not measure whether a TASK WAS COMPLETED. Whether an agent handed a
// good pack then went on to write correct code is a property of the agent and
// the model, not of retrieval. Folding the two together makes a retrieval
// regression indistinguishable from a bad day at the model provider, which
// destroys the only signal this package exists to produce.
//
// It measures one thing: whether the CONTEXT contained what a human said it
// should, and whether it stayed inside its budget while doing so.
//
// # Determinism
//
// No clocks, no network, no map iteration reaching output. A benchmark whose
// report reorders between runs cannot be diffed, and a score you cannot diff
// cannot demonstrate a regression — which is the whole job.
package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

// Case is one benchmark: a task, and what a correct answer must contain.
type Case struct {
	ID   string `json:"id" yaml:"id"`
	Task string `json:"task" yaml:"task"`

	// Expect are paths or symbol names that MUST appear in the pack. A path
	// ending in "/" is a prefix match; anything else matches a path suffix or
	// an exact symbol name.
	Expect []string `json:"expect" yaml:"expect"`
	// Forbid are things that must NOT appear — the unrelated module a naive
	// keyword search drags in. A benchmark with no forbid list measures recall
	// only, and recall alone is trivially gamed by returning everything.
	Forbid []string `json:"forbid,omitempty" yaml:"forbid,omitempty"`

	// MaxTokens, when set, fails the case if the pack exceeds it. A pack that
	// contains everything is not a good pack.
	MaxTokens int `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
}

// Result is one scored case.
type Result struct {
	CaseID string `json:"case_id"`
	Task   string `json:"task"`

	// Found and Missing partition Case.Expect.
	Found   []string `json:"found"`
	Missing []string `json:"missing"`
	// Violations are Forbid entries that appeared anyway.
	Violations []string `json:"violations,omitempty"`

	// Recall is Found/len(Expect). Precision is measured against Forbid rather
	// than against everything, because a pack legitimately contains supporting
	// context a case does not enumerate — the interface a class implements, the
	// caller one hop up — and no human writing a case will list all of it.
	// Scoring those unlisted items as false positives would mean the highest
	// precision goes to the pack that returned only the enumerated items and
	// nothing else, so the metric would actively reward under-retrieval and
	// punish the engine for being useful. Forbid inverts that: it names the
	// specific wrong answers a naive engine gives, so a violation is a real
	// defect rather than an artefact of an incomplete expectation list.
	Recall float64 `json:"recall"`

	TokenEstimate int  `json:"token_estimate"`
	OverBudget    bool `json:"over_budget"`
	Passed        bool `json:"passed"`
	Items         int  `json:"items"`
}

// Score evaluates one case against the pack the context engine produced.
//
// Score does not re-check that the case is well formed; Validate does that, and
// a caller that skips it gets what it asks for. The one guard here is Recall on
// an empty Expect list, which is reported as 0 rather than as a vacuous 1 so
// that a suite of empty cases reads as zero-recall instead of perfect.
func Score(c Case, pack devctx.Pack) Result {
	res := Result{
		CaseID:  c.ID,
		Task:    c.Task,
		Found:   make([]string, 0, len(c.Expect)),
		Missing: make([]string, 0),
		Items:   len(pack.Items),
	}

	for _, want := range c.Expect {
		if matchesPack(want, pack.Items) {
			res.Found = append(res.Found, want)
			continue
		}
		res.Missing = append(res.Missing, want)
	}
	for _, banned := range c.Forbid {
		// Only Items count. An entry in pack.Excluded is the engine reporting
		// that it CONSIDERED and REJECTED the candidate, which is the behaviour
		// a forbid list is asking for; scoring exclusions as violations would
		// punish the engine precisely for being honest about its boundary.
		if matchesPack(banned, pack.Items) {
			res.Violations = append(res.Violations, banned)
		}
	}

	if len(c.Expect) > 0 {
		res.Recall = float64(len(res.Found)) / float64(len(c.Expect))
	}

	// A hand-written pack in a test never fills TokenEstimate, and defaulting a
	// zero to "0 tokens" would let every MaxTokens case pass without measuring
	// anything — the same vacuous-green failure Validate exists to prevent. So
	// fall back to the pack's own content estimate, which is what the context
	// package budgets against.
	res.TokenEstimate = pack.TokenEstimate
	if res.TokenEstimate == 0 {
		res.TokenEstimate = pack.ContentTokens()
	}
	res.OverBudget = c.MaxTokens > 0 && res.TokenEstimate > c.MaxTokens

	// Sorted because Expect and Forbid come from a hand-edited file, where
	// reordering two lines is a routine edit that must not change the report.
	sort.Strings(res.Found)
	sort.Strings(res.Missing)
	sort.Strings(res.Violations)

	res.Passed = len(res.Missing) == 0 && len(res.Violations) == 0 && !res.OverBudget
	return res
}

// matchesPack reports whether any item in the pack satisfies entry.
func matchesPack(entry string, items []devctx.Item) bool {
	for _, it := range items {
		if matches(entry, it) {
			return true
		}
	}
	return false
}

// matches applies the expectation grammar to one item.
//
// Matching is case-sensitive throughout. Paths and symbols are case-sensitive
// in the index, and a case-insensitive match would let "repo.java" claim a hit
// on "Repo.java" — a green case standing on a file that does not exist.
func matches(entry string, it devctx.Item) bool {
	if entry == "" {
		return false
	}
	// A trailing slash is unambiguously a directory, so it is only ever tested
	// against a path. Letting it also match a symbol name would make
	// "payments/" mean two different things depending on the index contents.
	if strings.HasSuffix(entry, "/") {
		return strings.HasPrefix(it.Path, entry)
	}
	if entry == it.Name {
		return true
	}
	if it.FQN != "" && entry == it.FQN {
		return true
	}
	return pathSuffix(it.Path, entry)
}

// pathSuffix reports whether entry names the tail of path at a segment boundary.
//
// The boundary test is the load-bearing half. Plain suffix matching lets
// "Repo.java" match "svc/OtherRepo.java", so a case passes on a file it never
// asked for — a false green, which is strictly worse than having no benchmark,
// because it is trusted.
func pathSuffix(path, entry string) bool {
	if path == "" || len(entry) > len(path) || !strings.HasSuffix(path, entry) {
		return false
	}
	return len(entry) == len(path) || path[len(path)-len(entry)-1] == '/'
}

// Suite is a set of results.
type Suite struct {
	Results []Result `json:"results"`
	// Aggregates, computed by Summarise.
	Cases      int     `json:"cases"`
	Passed     int     `json:"passed"`
	MeanRecall float64 `json:"mean_recall"`
	// TotalViolations counts forbidden items that appeared across all cases.
	TotalViolations int `json:"total_violations"`
}

// Summarise computes the aggregates. Results are sorted by CaseID first, so a
// suite is reproducible and diffable across runs.
func Summarise(results []Result) Suite {
	// Copied rather than sorted in place: cases are scored concurrently in the
	// harness, and reordering a slice the caller still holds is the kind of
	// aliasing bug that only shows up under -race months later.
	sorted := make([]Result, len(results))
	copy(sorted, results)
	// Stable, so duplicate IDs — which Validate rejects but Score will happily
	// produce if it was never called — keep their input order instead of
	// landing wherever the sort's pivot choice puts them.
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CaseID < sorted[j].CaseID })

	s := Suite{Results: sorted, Cases: len(sorted)}
	var recall float64
	for _, r := range sorted {
		if r.Passed {
			s.Passed++
		}
		s.TotalViolations += len(r.Violations)
		recall += r.Recall
	}
	if s.Cases > 0 {
		// Summed over the sorted slice, so the floating-point rounding order is
		// fixed and the mean is bit-identical across runs.
		s.MeanRecall = recall / float64(s.Cases)
	}
	return s
}

// maxCaseFileBytes bounds a case file. Cases are hand-written by engineers who
// read the code first; a megabyte of them is a generated file or a hostile
// input, and either way the loader should refuse rather than hand the decoder
// something unbounded.
const maxCaseFileBytes = 4 << 20

// maxNestDepth bounds bracket nesting before the decoder ever sees the input.
// A case file nests four levels at most (document, case, list, scalar). The
// guard exists because a deeply nested document drives a recursive-descent
// decoder into the goroutine stack, and a benchmark loader that kills the
// process because someone pasted junk is a denial of service in a tool that
// runs in CI.
const maxNestDepth = 64

// caseFile is the object form: cases under a "cases" key.
type caseFile struct {
	Cases []Case `json:"cases" yaml:"cases"`
}

// LoadCases reads cases from YAML or JSON. Format is sniffed from the content.
//
// Sniffed rather than taken from a file extension, because the cases usually
// arrive as bytes — embedded, piped in, or fetched — and threading a filename
// through purely to pick a decoder invites the caller to lie about it.
//
// Unknown fields are accepted rather than rejected. A mistyped key like
// "expects:" leaves Expect empty, and Validate refuses an empty Expect, so the
// typo surfaces as a precise error there instead of as a decoder complaint that
// names a line but not the consequence.
func LoadCases(src []byte) ([]Case, error) {
	src = trimBOM(src)
	if len(strings.TrimSpace(string(src))) == 0 {
		return nil, errors.New("bench: empty case file")
	}
	if len(src) > maxCaseFileBytes {
		return nil, fmt.Errorf("bench: case file is %d bytes, over the %d byte limit", len(src), maxCaseFileBytes)
	}
	if d := nestDepth(src); d > maxNestDepth {
		return nil, fmt.Errorf("bench: case file nests %d levels deep, over the %d level limit", d, maxNestDepth)
	}

	cases, err := decodeCases(src)
	if err != nil {
		return nil, err
	}
	// Zero cases is a load error, not an empty result. Every path that produces
	// it — an empty list, a "cases:" key with nothing under it, a document that
	// decoded into the wrong shape — leaves a suite that reports 0/0 passed and
	// looks green.
	if len(cases) == 0 {
		return nil, errors.New("bench: case file contains no cases")
	}
	return cases, nil
}

// decodeCases accepts both the bare list and the "cases:" mapping, tried in
// that order because the bare list is what a hand-written file looks like.
//
// The panic guard is not defensive habit. gopkg.in/yaml.v3 has panicked on
// malformed input within living memory (CVE-2022-28948), and encoding/json is
// only bounded against nesting by a limit that moves between releases. A
// truncated case file must produce an error a human can read, not a stack trace
// that takes CI down with it.
func decodeCases(src []byte) (cases []Case, err error) {
	defer func() {
		if r := recover(); r != nil {
			cases, err = nil, fmt.Errorf("bench: malformed case file: %v", r)
		}
	}()

	unmarshal := yaml.Unmarshal
	format := "yaml"
	if looksJSON(src) {
		unmarshal = json.Unmarshal
		format = "json"
	}

	var list []Case
	listErr := unmarshal(src, &list)
	if listErr == nil {
		return list, nil
	}
	var file caseFile
	if fileErr := unmarshal(src, &file); fileErr == nil {
		return file.Cases, nil
	}
	// The list error is reported because the list is the documented shape; the
	// mapping error would point at the same syntax problem one level down.
	return nil, fmt.Errorf("bench: parsing %s cases: %w", format, listErr)
}

// looksJSON sniffs the format from the first significant byte.
//
// YAML is very nearly a superset of JSON, so this exists for the cases where it
// is not: yaml.v3 silently reinterprets some JSON scalars, and a JSON file
// deserves the JSON decoder's error messages.
func looksJSON(src []byte) bool {
	for _, b := range src {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// nestDepth is the deepest run of unclosed brackets in the input.
//
// Brackets inside quoted strings are counted too, which overstates depth by a
// few levels on a task like `fix [urgent] bug`. That inaccuracy is acceptable
// in one direction only, and it is this one: the limit sits far above any real
// case file, so overcounting never rejects valid input, while a proper
// string-aware scan would be a second parser to keep correct.
func nestDepth(src []byte) int {
	depth, max := 0, 0
	for _, b := range src {
		switch b {
		case '{', '[':
			depth++
			if depth > max {
				max = depth
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return max
}

func trimBOM(src []byte) []byte {
	const bom = "\xef\xbb\xbf"
	if strings.HasPrefix(string(src), bom) {
		return src[len(bom):]
	}
	return src
}

// Validate rejects a case that cannot measure anything, reporting every
// problem at once.
//
// The empty Expect list is the one that matters. A case with nothing to expect
// PASSES VACUOUSLY: it contributes a green row, raises the pass count, and
// asserts nothing about the engine. A suite full of them reports 100% passed
// while measuring literally nothing, and because the number is high nobody
// looks — which makes it the most dangerous defect a benchmark can carry, worse
// than a wrong expectation, because a wrong expectation at least fails loudly.
//
// Every problem is reported together because a loader that stops at the first
// one turns fixing a case file into a fix-run-fix loop over a file whose errors
// are almost always the same mistake repeated.
func Validate(cases []Case) error {
	var problems []error
	seen := make(map[string]int, len(cases))

	for i, c := range cases {
		id := strings.TrimSpace(c.ID)
		switch id {
		case "":
			// Without an ID a failure cannot be named in a report or diffed
			// against yesterday's run, so the case is unactionable even when it
			// is correct.
			problems = append(problems, fmt.Errorf("case %d: empty id", i))
		default:
			if first, dup := seen[id]; dup {
				// Duplicate IDs make the sorted report ambiguous and let one
				// case silently mask the other in any by-ID lookup.
				problems = append(problems, fmt.Errorf("case %d: duplicate id %q, first used by case %d", i, id, first))
			} else {
				seen[id] = i
			}
		}
		if strings.TrimSpace(c.Task) == "" {
			problems = append(problems, fmt.Errorf("case %d (%q): empty task", i, c.ID))
		}
		if len(c.Expect) == 0 {
			problems = append(problems, fmt.Errorf("case %d (%q): empty expect list, the case would pass without measuring anything", i, c.ID))
		}
		for j, want := range c.Expect {
			if strings.TrimSpace(want) == "" {
				problems = append(problems, fmt.Errorf("case %d (%q): expect[%d] is blank", i, c.ID, j))
			}
		}
		for j, banned := range c.Forbid {
			if strings.TrimSpace(banned) == "" {
				problems = append(problems, fmt.Errorf("case %d (%q): forbid[%d] is blank", i, c.ID, j))
			}
		}
	}
	return errors.Join(problems...)
}

// Markdown renders a suite as a report.
//
// Failures are repeated in full below the table. The table answers "did it
// regress"; the detail answers "on what", and an engineer who has to re-run the
// suite with a debugger to learn the second will not run the suite.
func (s Suite) Markdown() string {
	var b strings.Builder
	b.WriteString("# AgentBench\n\n")

	if s.Cases == 0 {
		b.WriteString("No cases were scored.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%d cases, %d passed, mean recall %s", s.Cases, s.Passed, formatRecall(s.MeanRecall))
	if s.TotalViolations > 0 {
		fmt.Fprintf(&b, ", %d forbidden item(s) retrieved", s.TotalViolations)
	}
	b.WriteString(".\n\n")

	b.WriteString("| Case | Recall | Items | Tokens | Result |\n")
	b.WriteString("| --- | ---: | ---: | ---: | --- |\n")
	for _, r := range s.Results {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n",
			cell(r.CaseID), formatRecall(r.Recall), r.Items, r.TokenEstimate, verdict(r))
	}

	failed := make([]Result, 0, len(s.Results))
	for _, r := range s.Results {
		if !r.Passed {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return b.String()
	}

	b.WriteString("\n## Failures\n")
	for _, r := range failed {
		fmt.Fprintf(&b, "\n### %s\n\n", cell(r.CaseID))
		fmt.Fprintf(&b, "Task: %s\n\n", cell(r.Task))
		if len(r.Missing) > 0 {
			fmt.Fprintf(&b, "- Not retrieved: %s\n", codeList(r.Missing))
		}
		if len(r.Violations) > 0 {
			fmt.Fprintf(&b, "- Retrieved despite being forbidden: %s\n", codeList(r.Violations))
		}
		if r.OverBudget {
			fmt.Fprintf(&b, "- Over budget: %d tokens\n", r.TokenEstimate)
		}
	}
	return b.String()
}

func verdict(r Result) string {
	if r.Passed {
		return "pass"
	}
	return "FAIL"
}

// formatRecall fixes two decimals so a diff between two runs shows a changed
// score rather than a changed number of digits.
func formatRecall(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "`" + cell(s) + "`"
	}
	return strings.Join(quoted, ", ")
}

// cell makes a value safe inside a Markdown table row. An unescaped pipe in a
// task description splits the row and shifts every column after it, silently
// misreporting the numbers rather than looking broken.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}
