// Package arch is the declared architecture: which paths belong to which
// component, and which dependencies between components are permitted.
//
// # It is a RULE MODEL, not a checker
//
// Nothing here reads the graph, opens a file, or produces a violation. It
// answers two questions and stops: "which component owns this path?" and "is a
// dependency from this component to that one allowed?". The capability that
// walks edges lives in internal/capability/architecture and calls in here once
// per file and once per crossing.
//
// Keeping the model this small is what makes the check trustworthy. Component
// membership is a pure function of a path, so the same repository always
// produces the same assignment, and a rule's verdict never depends on which
// edges happened to be visited first.
//
// # What this deliberately does not do
//
// It does NOT infer components. A model RepoCortex guessed would be a description
// of the code's current shape, and checking code against a description of itself
// finds nothing by construction.
//
// It does NOT compile patterns to regular expressions. This model is read from
// the repository's own .cortex.yml, which is untrusted; a regex from an
// untrusted file is a catastrophic-backtracking denial of service waiting to
// happen. The matcher below is a hand-written dynamic program that cannot
// backtrack, over inputs bounded in both length and depth.
//
// It does NOT make rules transitive, and does not compute layer orderings or
// cycles. "A may not use C" is not derived from "A may not use B" and "B uses
// C": a rule that fires on a relationship nobody wrote is a rule people learn to
// ignore. Every verdict here traces to a rule a person typed.
//
// It does NOT know about languages, symbols or imports. A component is a set of
// PATHS, which is the one definition that works for a Java service, a Go
// module and a TypeScript front end at the same time.
package arch

import (
	"fmt"
	"strings"
	"sync"
)

// Component is a named part of the system, defined by path patterns.
type Component struct {
	Name    string   `yaml:"name"    json:"name"`
	Include []string `yaml:"include" json:"include"` // glob patterns, repo-relative, forward slashes
	Exclude []string `yaml:"exclude" json:"exclude"`
}

// Rule constrains dependencies out of one or more components.
type Rule struct {
	ID          string   `yaml:"id"          json:"id"`
	Description string   `yaml:"description" json:"description"`
	From        []string `yaml:"from"        json:"from"`       // component names
	AllowedTo   []string `yaml:"allowed_to"  json:"allowed_to"` // component names; empty = no allow-list
	Forbidden   []string `yaml:"forbidden"   json:"forbidden"`  // component names
	Severity    string   `yaml:"severity"    json:"severity"`   // high | medium | low
}

// Model is a whole declared architecture.
//
// IT HOLDS NO MUTABLE STATE, and that is a deliberate constraint rather than an
// omission. A Model is copied by value into config.Config and again into
// architecture.Check, and it is read from many goroutines while the graph is
// walked. A memo cache field would be silently shared or silently duplicated
// depending on which copy a caller happened to hold, and a mutex field would
// make every enclosing struct uncopyable — config.Config.Validate has a value
// receiver, so a lock in here would break the build of a package this one is
// not allowed to know about. Callers that need memoisation take a Resolver.
type Model struct {
	Components []Component `yaml:"components" json:"components"`
	Rules      []Rule      `yaml:"rules"      json:"rules"`

	// Data and DataRules are the sensitive-data half of the model: which
	// tables hold regulated data, and which components may reach them. They
	// live in data.go, and they are separate blocks rather than fields on
	// Component and Rule because a classification is not a dependency —
	// nothing about `pci` constrains an import, and the reachability question
	// is answered by walking a path rather than by judging one crossing.
	Data      []DataClass `yaml:"data"       json:"data"`
	DataRules []DataRule  `yaml:"data_rules" json:"data_rules"`
}

// Severity levels. Spelled lowercase because reports rank and group by this
// string, and "High" landing in a different bucket from "high" is the kind of
// bug that makes a CI gate quietly stop failing.
const (
	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"
)

// Bounds on what can be matched.
//
// These exist because the model comes from an untrusted file. They are set well
// above anything a person writes and well below anything that costs real time:
// POSIX PATH_MAX is 4096, so a longer path is not a file in a repository, and a
// 512-byte glob is not a pattern someone typed. The SEGMENT caps matter more
// than the byte caps — matching is O(pattern segments x path segments), so
// capping both caps the work per call at roughly 8k cell updates no matter what
// the config says.
//
// Input beyond a bound is reported as "no match" rather than as an error.
// ComponentOf has nowhere to put an error, and a path nobody can classify is
// exactly what an unowned path already means.
const (
	// maxComponents and maxPatternsPerComponent bound the model itself.
	//
	// ComponentOf walks every component's every pattern for every FILE, so the
	// cost is components x patterns x files — and all three come from
	// .cortex.yml, which arrives with the repository and is untrusted.
	// Bounding the pattern LENGTH, as the next two constants do, does nothing
	// about a model declaring hundreds of thousands of them.
	//
	// A real architecture has tens of components. One declaring more than this
	// is generated or hostile, and saying so is more useful than grinding
	// through it.
	maxComponents           = 512
	maxPatternsPerComponent = 256

	maxPatternBytes    = 512
	maxPathBytes       = 4096
	maxPatternSegments = 64
	maxPathSegments    = 128
)

// Validate reports every problem with a model at once.
//
// Every problem, not the first, because these are read from a config file a
// person is editing: reporting one typo per run turns fixing five of them into
// five edit-run cycles. The problems are ordered by where they appear in the
// file so the list reads like the document.
//
// An empty model is valid. Architecture governance is opt-in, and a repository
// that declares nothing must not fail to load.
func (m Model) Validate() error {
	if len(m.Components) == 0 && len(m.Rules) == 0 && len(m.Data) == 0 && len(m.DataRules) == 0 {
		return nil
	}

	var problems []string
	if len(m.Components) > maxComponents {
		problems = append(problems, fmt.Sprintf(
			"%d components declared, over the %d limit; component matching runs for every "+
				"file in the repository, so a model this large costs more than it can explain",
			len(m.Components), maxComponents))
	}
	// known holds only NON-EMPTY names. An unnamed component must not become a
	// component every rule can reference by writing `from: [""]`.
	known := make(map[string]bool, len(m.Components))

	for i, c := range m.Components {
		label := fmt.Sprintf("component #%d", i+1)
		switch {
		case c.Name == "":
			problems = append(problems, label+": name is empty; a component nothing can name cannot appear in a rule")
		case known[c.Name]:
			problems = append(problems, fmt.Sprintf("%s: duplicate component name %q; a rule naming it would silently mean one of the two", label, c.Name))
		default:
			known[c.Name] = true
			label = fmt.Sprintf("component %q", c.Name)
		}

		if len(c.Include) == 0 {
			problems = append(problems, label+": no include patterns; it matches no file, so every rule naming it is inert")
		}
		problems = appendPatternProblems(problems, label, "include", c.Include)
		problems = appendPatternProblems(problems, label, "exclude", c.Exclude)
	}

	// classes holds only NON-EMPTY classification names, for the same reason
	// `known` above holds only non-empty component names: an unnamed data class
	// must not become a classification every rule can reference by writing
	// `classification: [""]`.
	classes := make(map[string]bool, len(m.Data))
	problems = appendDataClassProblems(problems, classes, m.Data)

	// Rule IDs are optional — a rule with no ID is still enforced — so only
	// non-empty IDs are checked for duplicates. Duplicates among them matter
	// because the ID is what a report prints and what a person suppresses by.
	seenID := make(map[string]bool, len(m.Rules))
	for i, r := range m.Rules {
		label := fmt.Sprintf("rule #%d", i+1)
		if r.ID != "" {
			label = fmt.Sprintf("rule %q", r.ID)
			if seenID[r.ID] {
				problems = append(problems, fmt.Sprintf("%s: duplicate rule id; a violation report could not say which rule it came from", label))
			}
			seenID[r.ID] = true
		}

		if len(r.From) == 0 {
			problems = append(problems, label+": no `from` components; a rule that constrains nothing is almost always a rule whose author meant it to constrain something")
		}
		problems = appendUnknownProblems(problems, known, label, "from", r.From)
		problems = appendUnknownProblems(problems, known, label, "allowed_to", r.AllowedTo)
		problems = appendUnknownProblems(problems, known, label, "forbidden", r.Forbidden)

		switch r.Severity {
		case "", SeverityHigh, SeverityMedium, SeverityLow:
		default:
			problems = append(problems, fmt.Sprintf("%s: severity %q: want high, medium or low", label, r.Severity))
		}
	}

	problems = appendDataRuleProblems(problems, known, classes, seenID, m.DataRules)

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// appendPatternProblems rejects patterns the matcher would silently never match.
//
// Silently is the operative word: an over-long or empty pattern is not a
// matching error, it is a component that quietly owns no files, and the rules
// naming it then pass forever. Failing to load is the honest outcome.
func appendPatternProblems(problems []string, label, field string, patterns []string) []string {
	if len(patterns) > maxPatternsPerComponent {
		return append(problems, fmt.Sprintf(
			"%s: %d %s patterns, over the %d limit; every one is walked for every file",
			label, len(patterns), field, maxPatternsPerComponent))
	}
	for j, p := range patterns {
		switch {
		case strings.TrimSpace(p) == "":
			problems = append(problems, fmt.Sprintf("%s: %s pattern #%d is empty", label, field, j+1))
		case len(p) > maxPatternBytes:
			problems = append(problems, fmt.Sprintf("%s: %s pattern #%d is %d bytes, over the %d-byte limit, and would never match", label, field, j+1, len(p), maxPatternBytes))
		default:
			if _, ok := splitPattern(p, make([]string, 0, maxPatternSegments)); !ok {
				problems = append(problems, fmt.Sprintf("%s: %s pattern #%d has more than %d path segments and would never match", label, field, j+1, maxPatternSegments))
			}
		}
	}
	return problems
}

// appendUnknownProblems catches the typo this whole validation exists for: a
// rule naming a component that does not exist matches nothing, and a rule that
// matches nothing looks exactly like a rule nobody is breaking.
func appendUnknownProblems(problems []string, known map[string]bool, label, field string, names []string) []string {
	for _, n := range names {
		if !known[n] {
			problems = append(problems, fmt.Sprintf("%s: %s references unknown component %q", label, field, n))
		}
	}
	return problems
}

// ValidationError carries every problem found in one model.
//
// The problems are kept as a slice as well as a message so a caller can render
// them one per line in a terminal or as a list in JSON, rather than having to
// split a message apart.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d problems:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// ComponentOf returns the component owning a repo-relative path, or "" if none.
//
// # Most specific wins
//
// A path is routinely matched by several components — `internal/**` and
// `internal/store/**` both own `internal/store/db.go` — and the narrower
// declaration is the one the author meant. Specificity is measured as the
// number of LITERAL characters in the matching pattern, not its length: raw
// length would rank `**/**/**/**` above `api/`, because wildcards are long and
// mean nothing. Ties fall back to the longer pattern, then to the
// alphabetically first component name.
//
// # Determinism
//
// The result depends only on the path and the set of components — never on
// declaration order, and never on map iteration, of which there is none here.
// Two components that tie resolve to the same answer on every run and in every
// process. This matters because the assignment feeds a report that lands in CI:
// a component that changes identity between runs turns "did this get worse?"
// into noise.
//
// There is no cache. See the note on Model; callers that resolve the same path
// repeatedly should hold a Resolver.
func (m *Model) ComponentOf(path string) string {
	if path == "" || len(path) > maxPathBytes {
		return ""
	}
	var pathBuf [maxPathSegments]string
	segs, ok := splitPath(path, pathBuf[:0])
	if !ok || len(segs) == 0 {
		return ""
	}

	best := ""
	bestLiterals, bestLen := -1, -1
	for i := range m.Components {
		c := &m.Components[i]
		literals, length, matched := c.match(segs)
		if !matched {
			continue
		}
		if literals > bestLiterals ||
			(literals == bestLiterals && length > bestLen) ||
			(literals == bestLiterals && length == bestLen && c.Name < best) {
			best, bestLiterals, bestLen = c.Name, literals, length
		}
	}
	return best
}

// match reports whether a component owns an already-split path, and how
// specifically.
//
// Exclude is checked FIRST and wins outright. An exclude exists to carve a hole
// in an include — `src/**` minus `src/generated/**` — and a hole that an
// include can win back is not a hole.
func (c *Component) match(segs []string) (literals, length int, matched bool) {
	for _, ex := range c.Exclude {
		if matchSegs(ex, segs) {
			return 0, 0, false
		}
	}
	literals, length = -1, -1
	for _, in := range c.Include {
		if !matchSegs(in, segs) {
			continue
		}
		l := literalCount(in)
		if l > literals || (l == literals && len(in) > length) {
			literals, length, matched = l, len(in), true
		}
	}
	return literals, length, matched
}

// literalCount is how many characters of a pattern are not wildcards, which is
// the specificity measure ComponentOf ranks by. `internal/store/**` scores 15
// and `**` scores 0, which is the ordering a reader expects.
func literalCount(pattern string) int {
	n := 0
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '*' && pattern[i] != '?' {
			n++
		}
	}
	return n
}

// Verdict is the outcome of evaluating one dependency.
type Verdict string

const (
	VerdictAllowed    Verdict = "allowed"     // permitted, or no rule applies
	VerdictForbidden  Verdict = "forbidden"   // explicitly listed in Forbidden
	VerdictNotAllowed Verdict = "not_allowed" // an allow-list exists and this target is not on it
)

// Evaluate decides whether a dependency from one component to another is
// permitted, and returns the rule that decided it (nil when no rule applies).
//
// # Precedence
//
// Forbidden beats everything. A `forbidden` entry is a rule someone wrote to
// stop exactly this dependency, and it must not be cancelled by a broader
// allow-list somewhere else in the file. Every matching rule is examined before
// any allow verdict is returned, so a forbidding rule declared last still wins.
//
// An explicit allow beats not_allowed. Two rules out of the same component each
// listing part of the permitted set are a normal way to write this — a base
// rule plus a team's addition — and reading them as contradictions would make
// the second rule impossible to add.
//
// A dependency inside one component is always allowed: there is no boundary to
// cross. So is a dependency to or from "", which is what ComponentOf returns for
// a path no component claims; judging code the model never described would
// invent a boundary nobody declared.
func (m *Model) Evaluate(from, to string) (Verdict, *Rule) {
	if from == to || from == "" || to == "" {
		return VerdictAllowed, nil
	}

	var allowedBy, gatedBy *Rule
	for i := range m.Rules {
		r := &m.Rules[i]
		if !contains(r.From, from) {
			continue
		}
		if contains(r.Forbidden, to) {
			return VerdictForbidden, r
		}
		if len(r.AllowedTo) == 0 {
			// A rule with no allow-list only forbids. It does not implicitly
			// forbid everything it failed to mention, or writing a single
			// `forbidden` entry would silently cut the component off from the
			// rest of the system.
			continue
		}
		if contains(r.AllowedTo, to) {
			if allowedBy == nil {
				allowedBy = r
			}
			continue
		}
		if gatedBy == nil {
			// Remember the FIRST allow-list that excludes the target, so the
			// reported rule is stable when several do.
			gatedBy = r
		}
	}

	switch {
	case allowedBy != nil:
		return VerdictAllowed, allowedBy
	case gatedBy != nil:
		return VerdictNotAllowed, gatedBy
	}
	return VerdictAllowed, nil
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// SeverityOrDefault is the severity of a rule, defaulting to "medium".
//
// Unset means medium because a rule someone bothered to write is worth
// reporting, and defaulting to high would make the first unannotated rule fail
// a `--fail-on high` build for a reason its author never chose.
//
// Anything unrecognised also becomes medium. Validate rejects those, so this
// branch is reached only by a model that skipped validation — and the value
// flows straight into a report's severity buckets and exit-code ranking, where
// an arbitrary string would silently rank below every real severity and be
// invisible to `--fail-on`.
func (r Rule) SeverityOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(r.Severity)) {
	case SeverityHigh:
		return SeverityHigh
	case SeverityLow:
		return SeverityLow
	}
	return SeverityMedium
}

// ---------------------------------------------------------------- matching

// matchSegs reports whether a glob pattern matches an already-split path.
//
// Semantics, which are gitignore's and not path.Match's:
//
//   - `**` AS A WHOLE SEGMENT spans zero or more segments. Zero is the part
//     that gets forgotten: `.git/**` must match `.git` itself, and a matcher
//     that requires at least one segment leaves the directory it was told to
//     claim unclaimed. This repo shipped that bug once.
//   - `*` matches within one segment and never crosses `/`, so `src/*` does not
//     own `src/a/b.go`.
//   - `?` matches one character inside a segment.
//   - Matching is case-sensitive, because the paths come from the index and two
//     files differing only in case are two files.
func matchSegs(pattern string, segs []string) bool {
	if len(pattern) > maxPatternBytes {
		return false
	}
	var patBuf [maxPatternSegments]string
	pats, ok := splitPattern(pattern, patBuf[:0])
	if !ok {
		return false
	}
	return matchSplit(pats, segs)
}

// matchSplit is the matcher proper: a dynamic program over (pattern segment,
// path segment) pairs.
//
// A DP rather than the obvious recursion, and this is the load-bearing choice.
// Recursive `**` handling backtracks, and a pattern like `**/a/**/a/**/a` over a
// long path costs exponential time — from an untrusted config file, that is a
// hang, not a slow answer. The table below is O(len(pats) x len(segs)), never
// revisits a cell, and both dimensions are capped, so the worst case any
// .cortex.yml can buy is a fixed few thousand boolean writes.
//
// next[j] means "pats[i+1:] matches segs[j:]"; cur[j] is the same for pats[i:].
// Both live in fixed-size arrays so a match allocates nothing on the hot path.
func matchSplit(pats, segs []string) bool {
	n, m := len(pats), len(segs)

	// Fast path: with no `**` the arity is fixed, so a length mismatch is a
	// decision and there is nothing to search. This covers most real patterns
	// and skips the table entirely.
	if !hasDoubleStar(pats) {
		if n != m {
			return false
		}
		for i, seg := range segs {
			if !matchSegment(pats[i], seg) {
				return false
			}
		}
		return true
	}

	var nextBuf, curBuf [maxPathSegments + 1]bool
	next, cur := nextBuf[:m+1], curBuf[:m+1]
	next[m] = true // an exhausted pattern matches an exhausted path

	for i := n - 1; i >= 0; i-- {
		if pats[i] == "**" {
			// Either `**` stops here, or it swallows segs[j] and carries on.
			cur[m] = next[m]
			for j := m - 1; j >= 0; j-- {
				cur[j] = next[j] || cur[j+1]
			}
		} else {
			// A concrete segment must consume exactly one path segment, so
			// there is no cell where the path has run out.
			cur[m] = false
			for j := m - 1; j >= 0; j-- {
				cur[j] = next[j+1] && matchSegment(pats[i], segs[j])
			}
		}
		cur, next = next, cur
	}
	return next[0]
}

func hasDoubleStar(pats []string) bool {
	for _, p := range pats {
		if p == "**" {
			return true
		}
	}
	return false
}

// matchSegment matches one pattern segment against one path segment.
//
// Iterative, with a single remembered star position, so it cannot recurse and
// cannot blow the stack on `*******************a`. `**` appearing inside a
// segment (`a**b`) is treated as an ordinary `*`: spanning directories is a
// property of the whole segment being `**`, not of the characters.
func matchSegment(pat, name string) bool {
	if !strings.ContainsAny(pat, "*?") {
		return pat == name
	}
	p, s := 0, 0
	starPat, starSeg := -1, 0
	for s < len(name) {
		switch {
		case p < len(pat) && (pat[p] == '?' || pat[p] == name[s]):
			p++
			s++
		case p < len(pat) && pat[p] == '*':
			// Remember where the star was and try matching zero characters
			// first; if the rest fails we come back and give it one more.
			starPat, starSeg = p, s
			p++
		case starPat >= 0:
			starSeg++
			p, s = starPat+1, starSeg
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// splitPath splits a repo-relative path into segments, into caller-owned
// storage.
//
// Empty and "." segments are dropped, which normalises `./a`, `/a` and `a//b`
// to the same three-segment-free form. Paths reach this from several places —
// config globs, the file index, a git status — and a leading `./` that changes
// which component a file belongs to would be a bug nobody could see.
//
// Returns ok=false when the path has more segments than the cap. The path is
// then unowned, which is the safe reading: nothing in a repository is 128
// directories deep, and the alternative is letting a config file choose how much
// work each of a million edges costs.
func splitPath(path string, dst []string) ([]string, bool) {
	return split(path, dst, false)
}

// splitPattern splits a glob, additionally collapsing runs of `**`.
//
// `**/**` describes exactly the same set as `**`, so collapsing changes no
// answer — but it turns a pattern of ten thousand `**` segments from something
// that trips the segment cap (and would then match nothing, surprisingly) into
// the one-segment pattern it actually means.
func splitPattern(pattern string, dst []string) ([]string, bool) {
	return split(pattern, dst, true)
}

func split(s string, dst []string, collapseDoubleStar bool) ([]string, bool) {
	for s != "" {
		var seg string
		if i := strings.IndexByte(s, '/'); i >= 0 {
			seg, s = s[:i], s[i+1:]
		} else {
			seg, s = s, ""
		}
		if seg == "" || seg == "." {
			continue
		}
		if collapseDoubleStar && seg == "**" && len(dst) > 0 && dst[len(dst)-1] == "**" {
			continue
		}
		if len(dst) == cap(dst) {
			return nil, false
		}
		dst = append(dst, seg)
	}
	return dst, true
}

// ---------------------------------------------------------------- resolver

// Resolver is a Model plus a per-path memo of ComponentOf.
//
// It exists because ComponentOf is called once per graph edge on repositories
// with hundreds of thousands of them, and the same handful of paths recur
// across nearly all of them. The cache lives HERE rather than in Model because a
// Model is copied by value into config and into each capability, where a cache
// field would be either shared behind the copier's back or silently thrown away.
// A caller that wants memoisation asks for it, and owns its lifetime.
//
// Safe for concurrent use.
type Resolver struct {
	model Model

	mu   sync.RWMutex
	memo map[string]string
}

// NewResolver returns a Resolver over a copy of the model. The copy is shallow —
// the slices are shared — which is safe because nothing here ever writes to
// them, and because a caller mutating a Model while a Resolver caches answers
// from it has a bug the copy would only hide.
func NewResolver(m Model) *Resolver {
	return &Resolver{model: m, memo: make(map[string]string)}
}

// maxMemoEntries bounds the cache.
//
// One entry per distinct path, and a large monorepo has a few hundred thousand
// files; the cap is above that and exists only so a caller feeding generated or
// hostile paths cannot turn a lookup table into unbounded memory. Past the cap
// the resolver keeps answering correctly, just without adding entries.
const maxMemoEntries = 1 << 20

// ComponentOf is Model.ComponentOf, memoised.
func (r *Resolver) ComponentOf(path string) string {
	r.mu.RLock()
	c, ok := r.memo[path]
	r.mu.RUnlock()
	if ok {
		return c
	}

	// Computed outside the write lock: matching is pure, so two goroutines
	// racing on the same path compute the same answer and the duplicated work
	// is cheaper than serialising every miss behind one lock.
	c = r.model.ComponentOf(path)

	r.mu.Lock()
	if len(r.memo) < maxMemoEntries {
		r.memo[path] = c
	}
	r.mu.Unlock()
	return c
}

// Model returns the underlying model, for callers that hold only a Resolver and
// need to evaluate a rule.
func (r *Resolver) Model() *Model { return &r.model }
