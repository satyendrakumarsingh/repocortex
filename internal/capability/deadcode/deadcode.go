// Package deadcode reports definitions nothing in the index points at, and says
// how far each one can be trusted as actually unused.
//
// # Why this is graded rather than listed
//
// "No caller in the index" and "no caller" are different facts, and a tool that
// prints the first as the second gets a function deleted that a framework was
// calling by name. A static index sees a minority of the ways code is reached:
// reflection, dependency injection, a route table, an interface, a template and
// a test runner all call things without a call expression the parser can bind.
//
// So every row carries a Certainty and the reasons for it, and the rows that
// cannot be judged at all are counted and named instead of being dropped:
//
//   - likely: unexported, in a language whose references the index binds, in a
//     cleanly parsed file, with no unbound reference anywhere spelled like it,
//     and its name occurring nowhere in the indexed code outside its own
//     definition. Every ordinary explanation for a missing caller has been
//     checked and ruled out. It can still be wrong (see Limits), and the
//     report says so.
//   - possible: something on that list could not be ruled out. The row names
//     which, because "exported, so another module may import it" and "an
//     unresolved call spelled like this exists" call for different checks.
//
// Nothing here is ever `exact`. That word is reserved for facts a compiler
// proved, and absence of evidence is not one.
//
// # What is deliberately not reported
//
// Tests, conventional entry points (main, init) and definitions in languages
// the index only parses are skipped: in each case an empty caller list is the
// expected state rather than a finding. Skipped is the count of them, so the
// denominator is visible.
package deadcode

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// Certainty grades one candidate. It is not a model.Confidence: that type
// describes how an EDGE was established, and a dead-code candidate is the
// absence of one.
type Certainty string

const (
	// CertaintyLikely means every ordinary reason for a missing caller was
	// checked and none applies.
	CertaintyLikely Certainty = "likely"
	// CertaintyPossible means at least one such reason could not be ruled out.
	CertaintyPossible Certainty = "possible"
)

// Reason names why a candidate is only possible.
type Reason string

const (
	ReasonExported     Reason = "exported"
	ReasonNameUnbound  Reason = "unbound_reference_with_same_name"
	ReasonOverrides    Reason = "implements_or_extends"
	ReasonPartialParse Reason = "file_partially_parsed"
	ReasonMentioned    Reason = "name_appears_elsewhere"
)

// reasonText is the sentence shown beside each Reason. Kept in one table so
// the CLI, the MCP tool and the HTTP API cannot word the same fact three ways.
var reasonText = map[Reason]string{
	ReasonExported:     "exported, so code outside this index may use it",
	ReasonNameUnbound:  "a reference spelled like it was never bound to a definition and may be a use of it",
	ReasonOverrides:    "implements or extends another declaration, so it may be reached through that type",
	ReasonPartialParse: "its file did not parse cleanly, so uses inside the file may be missing",
	ReasonMentioned:    "its name appears in code outside its own definition, so it may be passed as a value, registered by name or referenced from configuration",
}

// Text returns the sentence for r.
func (r Reason) Text() string { return reasonText[r] }

// reasonLabel is the short form for a table cell; Text is the explanation.
var reasonLabel = map[Reason]string{
	ReasonExported:     "exported",
	ReasonNameUnbound:  "unbound reference with this name",
	ReasonOverrides:    "implements or extends",
	ReasonPartialParse: "file partially parsed",
	ReasonMentioned:    "name appears elsewhere",
}

// Label returns a few words for r, for a column that cannot hold a sentence.
func (r Reason) Label() string { return reasonLabel[r] }

// Reasons lists every reason, in a stable order, for a legend.
func Reasons() []Reason {
	return []Reason{ReasonExported, ReasonNameUnbound, ReasonOverrides, ReasonPartialParse, ReasonMentioned}
}

// Candidate is one definition with no incoming relationship.
type Candidate struct {
	Name      string    `json:"name"`
	FQN       string    `json:"fqn,omitempty"`
	Kind      string    `json:"kind"`
	Path      string    `json:"path"`
	Line      int       `json:"line"`
	EndLine   int       `json:"end_line,omitempty"`
	Language  string    `json:"language,omitempty"`
	Certainty Certainty `json:"certainty"`
	Reasons   []Reason  `json:"reasons,omitempty"`
}

// Lines is the candidate's length in lines, for ranking: a dead 200-line
// function is worth more attention than a dead one-line accessor.
func (c Candidate) Lines() int {
	if c.EndLine < c.Line {
		return 1
	}
	return c.EndLine - c.Line + 1
}

// Report is the answer to "what looks unused?".
type Report struct {
	RunID      int64       `json:"run_id"`
	Candidates []Candidate `json:"candidates"`

	// Likely and Possible count every candidate found, not just the ones
	// returned, unless Truncated is set — in which case they count what was
	// examined before the scan stopped.
	Likely   int `json:"likely"`
	Possible int `json:"possible"`

	// TextChecked is how many candidates were also checked against the
	// indexed source text for a mention of their name. Every returned `likely`
	// row was. Likely and Possible above include rows graded from the graph
	// alone, so Likely is an upper bound on what a text check of everything
	// would leave.
	TextChecked int `json:"text_checked"`
	// LikelyUnchecked is how many candidates counted in Likely were NOT text
	// checked. Zero means Likely is exact with respect to the text check.
	LikelyUnchecked int `json:"likely_unchecked"`

	// Skipped counts unreferenced definitions left out because an empty caller
	// list is their normal state. SkippedUnjudged is the subset in languages
	// the index cannot bind references for; it is reported separately because
	// it is a coverage limit and the others are not.
	Skipped         int      `json:"skipped"`
	SkippedUnjudged int      `json:"skipped_unjudged_language"`
	UnjudgedLangs   []string `json:"unjudged_languages,omitempty"`

	Completeness capability.Completeness `json:"completeness"`
}

// Limits is what a reader must know before acting on any row. Appended to
// every report's caveats by Find, so no caller can omit it.
const Limits = "A definition with no caller in the index is not proven unused. Reflection, dependency " +
	"injection, framework registration, templates, build scripts and code in other repositories all " +
	"reach code without a reference this index can bind. Search for the name and check the framework's " +
	"conventions before deleting anything listed here."

// Source is the read surface this package needs.
//
// One paging query and one text lookup. The registry satisfies it; a test
// satisfies it with a slice. Declared here, at the consumer, so the storage
// layer does not grow an opinion about what counts as dead.
type Source interface {
	UnreferencedSymbols(ctx context.Context, runID, afterID int64, limit int) ([]registry.UnreferencedSymbol, error)
	// IdentifierMentions reports the lines, outside comments, on which name
	// occurs as a whole identifier. ok is false when the text index cannot
	// answer for this name, which must be read as "unknown", never "not
	// mentioned".
	IdentifierMentions(ctx context.Context, runID int64, name string) (mentions []registry.Mention, ok bool, err error)
}

var _ Source = (*registry.Registry)(nil)

// Options bounds and filters a scan.
type Options struct {
	// Limit is the most candidates to return. Zero means DefaultLimit.
	Limit int

	// MinCertainty drops rows below it. Empty means both tiers.
	MinCertainty Certainty

	// PathPrefix restricts the scan to files under it. Empty means everywhere.
	PathPrefix string

	// IncludeExported keeps exported definitions. Off by default: in a library
	// every public function is "unreferenced", and a report that leads with
	// the whole public API is a report nobody reads twice.
	IncludeExported bool

	// Judgeable reports whether the index binds cross-file references for a
	// language. Nil means every language is judged, which is only right in
	// tests; production callers pass the parser's tier table.
	Judgeable func(language string) bool

	// ScanBudget is the most unreferenced rows to examine. Zero means
	// DefaultScanBudget. It bounds the work when filters reject nearly
	// everything, so a narrow PathPrefix on a huge index still returns.
	ScanBudget int
}

const (
	// DefaultLimit is how many candidates are returned unless asked otherwise.
	DefaultLimit = 50
	// DefaultScanBudget is how many unreferenced rows a scan examines.
	DefaultScanBudget = 200_000
	// pageSize is one registry page. Large enough that a scan is a few dozen
	// statements, small enough that a page is a few hundred kilobytes.
	pageSize = 2_000
)

// Find scans runID for unreferenced definitions.
//
// The candidates returned are the top Limit by certainty and then by size, so
// the first row is the one most worth a reader's minute. Because that order is
// not the scan order, the scan cannot stop at Limit rows; it keeps only the
// best Limit seen so far, which is what makes memory a function of Limit and
// not of how much dead code the repository holds.
func Find(ctx context.Context, deps capability.Deps, src Source, runID int64, opts Options) (*Report, error) {
	if src == nil {
		return nil, fmt.Errorf("deadcode: source is nil")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	budget := opts.ScanBudget
	if budget <= 0 {
		budget = DefaultScanBudget
	}

	rep := &Report{RunID: runID}
	unjudged := map[string]bool{}
	// More than limit are kept through the scan because the text check below
	// demotes some of them, and a demoted row must be replaced by the next
	// best rather than leave the answer short.
	keep := newTopN(shortlistSize(limit))

	var after int64
	examined := 0
	truncated := false
scan:
	for {
		page, err := src.UnreferencedSymbols(ctx, runID, after, pageSize)
		if err != nil {
			return nil, fmt.Errorf("deadcode: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, u := range page {
			after = u.ID
			if examined >= budget {
				truncated = true
				break scan
			}
			examined++

			if opts.PathPrefix != "" && !strings.HasPrefix(u.FilePath, opts.PathPrefix) {
				continue
			}
			if isExpectedUncalled(u) {
				rep.Skipped++
				continue
			}
			if opts.Judgeable != nil && !opts.Judgeable(u.Language) {
				rep.Skipped++
				rep.SkippedUnjudged++
				unjudged[u.Language] = true
				continue
			}
			if u.Exported && !opts.IncludeExported {
				rep.Skipped++
				continue
			}

			c := grade(u)
			if c.Certainty == CertaintyLikely {
				rep.Likely++
			} else {
				rep.Possible++
			}
			if opts.MinCertainty == CertaintyLikely && c.Certainty != CertaintyLikely {
				continue
			}
			keep.offer(c)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(page) < pageSize {
			break
		}
	}

	shortlist := keep.sorted()
	graphLikely := rep.Likely
	checked, err := verifyAgainstText(ctx, src, runID, shortlist, limit, rep)
	if err != nil {
		return nil, err
	}
	rep.TextChecked = checked
	rep.LikelyUnchecked = max(graphLikely-checked, 0)
	sort.SliceStable(shortlist, func(i, j int) bool { return better(shortlist[i], shortlist[j]) })
	if opts.MinCertainty == CertaintyLikely {
		kept := shortlist[:0]
		for _, c := range shortlist {
			if c.Certainty == CertaintyLikely {
				kept = append(kept, c)
			}
		}
		shortlist = kept
	}
	if len(shortlist) > limit {
		shortlist = shortlist[:limit]
	}
	rep.Candidates = shortlist
	for lang := range unjudged {
		rep.UnjudgedLangs = append(rep.UnjudgedLangs, lang)
	}
	sort.Strings(rep.UnjudgedLangs)

	stats, err := deps.ResolutionStats(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("deadcode: %w", err)
	}
	reason := ""
	if truncated {
		reason = fmt.Sprintf("the scan stopped after examining %d unreferenced definitions; "+
			"narrow it with a path to cover the rest", budget)
	}
	rep.Completeness = capability.NewCompleteness(stats, examined, truncated, reason)
	rep.Completeness.Caveats = append(rep.Completeness.Caveats, Limits)
	return rep, nil
}

// The text check is bounded twice. The scan keeps the best shortlistSize
// graph-graded candidates, and the check walks them in rank order until it has
// confirmed Limit of them as likely. So a repository where the largest
// unreferenced definitions are all handlers passed by value still gets a full
// page of confirmed rows, and one with a hundred thousand candidates still
// costs at most shortlistSize text lookups.
const (
	shortlistFactor = 40
	shortlistMin    = 400
	shortlistMax    = 4_000
)

func shortlistSize(limit int) int {
	return min(max(limit*shortlistFactor, shortlistMin), shortlistMax)
}

// verifyAgainstText demotes shortlisted `likely` candidates whose name occurs
// in the indexed text somewhere other than at a definition of that name. It
// returns how many candidates it checked.
//
// THE GRAPH ALONE OVERCLAIMS HERE, and this is the measured reason. Run on
// RepoCortex's own source, the graph-only grading listed every MCP handler as
// likely unused: each is passed to a registration call as a method VALUE
// (`addTool(..., s.findCallers)`), which is a use with no call expression and
// so no edge. A function handed to a router, a callback, a table of handlers
// and a name in a configuration file are all the same shape.
//
// The text holds what the graph misses. A name that occurs in code outside its
// own definition is being referred to by something, and the candidate is
// demoted to possible with that reason. It is a demotion and never a removal:
// the mention may be an unrelated symbol that happens to share the name, and
// the reader should still see the row. Comment-only lines are not mentions
// (see registry.IdentifierMentions for why that matters).
//
// shortlist must be in rank order. Candidates past the point where `want`
// likely rows are confirmed are left ungraded by text and are not returned as
// likely: Find cuts the list at Limit, and everything it keeps as likely has
// been checked.
func verifyAgainstText(ctx context.Context, src Source, runID int64, shortlist []Candidate, want int, rep *Report) (int, error) {
	// Where each shortlisted name is DEFINED, so those lines are not read as
	// uses. Several candidates can share a name; each one's own span is a
	// definition for all of them.
	spans := map[string][]span{}
	for _, c := range shortlist {
		spans[c.Name] = append(spans[c.Name], span{path: c.Path, from: c.Line, to: max(c.EndLine, c.Line)})
	}

	// The lookup is per name, so candidates sharing one share the verdict.
	seen := map[string]bool{} // name -> mentioned, or unknown

	checked, confirmed := 0, 0
	for i := range shortlist {
		c := &shortlist[i]
		if c.Certainty != CertaintyLikely {
			continue
		}
		if confirmed >= want {
			// Enough confirmed rows to fill the answer. The rest are not
			// checked, so they must not be presented as checked: they sort
			// after every confirmed row and fall outside Limit.
			break
		}
		mentioned, done := seen[c.Name]
		if !done {
			mentions, ok, err := src.IdentifierMentions(ctx, runID, c.Name)
			if err != nil {
				return checked, fmt.Errorf("deadcode: %w", err)
			}
			// Unknown is treated as mentioned: the claim "likely unused"
			// needs evidence, and "the text index could not look" is not
			// evidence.
			mentioned = !ok || usedOutside(mentions, spans[c.Name])
			seen[c.Name] = mentioned
		}
		checked++
		if mentioned {
			c.Certainty = CertaintyPossible
			c.Reasons = append(c.Reasons, ReasonMentioned)
			rep.Likely--
			rep.Possible++
		} else {
			confirmed++
		}
		if err := ctx.Err(); err != nil {
			return checked, err
		}
	}
	return checked, nil
}

// span is the lines one definition occupies in one file.
type span struct {
	path     string
	from, to int
}

// usedOutside reports whether any mention falls outside every definition span.
//
// A mention INSIDE a definition's own span is the definition itself or the
// function referring to itself (recursion, a log message naming the function),
// and neither is a use by anything else.
func usedOutside(mentions []registry.Mention, defs []span) bool {
	for _, m := range mentions {
		for _, line := range m.Lines {
			inside := false
			for _, d := range defs {
				if d.path == m.Path && line >= d.from && line <= d.to {
					inside = true
					break
				}
			}
			if !inside {
				return true
			}
		}
	}
	return false
}

// isExpectedUncalled reports whether an empty caller list is this symbol's
// normal state, so listing it would be noise rather than a finding.
func isExpectedUncalled(u registry.UnreferencedSymbol) bool {
	if capability.IsTestSymbol(u.Kind, u.Name, u.FilePath) || capability.IsTestPath(u.FilePath) {
		return true
	}
	switch u.Name {
	case "main", "init", "__init__", "__main__", "constructor", "setUp", "tearDown":
		return true
	}
	return false
}

// grade turns a registry row into a candidate with its certainty and reasons.
func grade(u registry.UnreferencedSymbol) Candidate {
	c := Candidate{
		Name: u.Name, FQN: u.FQN, Kind: u.Kind,
		Path: u.FilePath, Line: u.StartLine, EndLine: u.EndLine,
		Language: u.Language,
	}
	if u.Exported {
		c.Reasons = append(c.Reasons, ReasonExported)
	}
	if u.NameUnbound {
		c.Reasons = append(c.Reasons, ReasonNameUnbound)
	}
	if u.Overrides {
		c.Reasons = append(c.Reasons, ReasonOverrides)
	}
	if u.PartialParse {
		c.Reasons = append(c.Reasons, ReasonPartialParse)
	}
	c.Certainty = CertaintyLikely
	if len(c.Reasons) > 0 {
		c.Certainty = CertaintyPossible
	}
	return c
}

// better reports whether a should be listed before b: likely before possible,
// then longer before shorter, then by location so the order is deterministic.
func better(a, b Candidate) bool {
	if a.Certainty != b.Certainty {
		return a.Certainty == CertaintyLikely
	}
	if la, lb := a.Lines(), b.Lines(); la != lb {
		return la > lb
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	return a.Line < b.Line
}

// topN keeps the best n candidates seen. A plain slice with a tracked worst
// element: n is small (tens), so a heap would be more code for no measurable
// gain, and the scan's cost is the SQL, not this.
type topN struct {
	n     int
	items []Candidate
}

func newTopN(n int) *topN { return &topN{n: n, items: make([]Candidate, 0, n)} }

func (t *topN) offer(c Candidate) {
	if len(t.items) < t.n {
		t.items = append(t.items, c)
		return
	}
	worst := 0
	for i := 1; i < len(t.items); i++ {
		if better(t.items[worst], t.items[i]) {
			worst = i
		}
	}
	if better(c, t.items[worst]) {
		t.items[worst] = c
	}
}

func (t *topN) sorted() []Candidate {
	out := append([]Candidate(nil), t.items...)
	sort.Slice(out, func(i, j int) bool { return better(out[i], out[j]) })
	return out
}
