package ownership

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Limits bounds one report.
//
// Zero means "use the default", not "show nothing", for the same reason
// internal/docs makes that choice: a caller building a Limits literal and
// setting one field is the common case, and reading the other three as
// "suppress" turns a partial literal into a silently empty answer. Negative
// values are rejected rather than clamped.
type Limits struct {
	// MaxCommits is how far back from HEAD the history is read.
	//
	// This is the bound with a measured cost behind it, and the cost is linear:
	// internal/acquire/git reads commits WITH their paths at roughly 0.7 ms
	// each on a packed 767-commit repository, so 200 is about 150 ms and 2000
	// would be about a second and a half. A caller who needs deeper history
	// should raise it knowingly.
	//
	// It used to be 50 ms per commit, for a reason that had nothing to do with
	// this package: go-git reopened the packfile for every object the walk
	// read. See internal/acquire/git/objectview.go. The window is a bound on
	// what the report can SEE, and it should be set for that reason rather than
	// because the walk behind it was slow.
	//
	// It is also the report's largest blind spot, which is why Window and the
	// caveats state it rather than leaving the number in a config file.
	MaxCommits int

	// MaxPeople caps the ranked list. The count found is reported separately,
	// so a cap never reads as "that is everybody".
	MaxPeople int

	// MaxCommitsPerPerson and MaxFilesPerPerson cap one person's evidence.
	MaxCommitsPerPerson int
	MaxFilesPerPerson   int

	// MaxUntouchedFiles caps the list of reached files nobody touched.
	MaxUntouchedFiles int
}

// DefaultLimits is what a caller gets when it asks for nothing.
func DefaultLimits() Limits {
	return Limits{
		MaxCommits:          200,
		MaxPeople:           10,
		MaxCommitsPerPerson: 5,
		MaxFilesPerPerson:   10,
		MaxUntouchedFiles:   15,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxCommits == 0 {
		l.MaxCommits = d.MaxCommits
	}
	if l.MaxPeople == 0 {
		l.MaxPeople = d.MaxPeople
	}
	if l.MaxCommitsPerPerson == 0 {
		l.MaxCommitsPerPerson = d.MaxCommitsPerPerson
	}
	if l.MaxFilesPerPerson == 0 {
		l.MaxFilesPerPerson = d.MaxFilesPerPerson
	}
	if l.MaxUntouchedFiles == 0 {
		l.MaxUntouchedFiles = d.MaxUntouchedFiles
	}
	return l
}

// Validate reports every problem at once, because these arrive from a flag set.
func (l Limits) Validate() error {
	var problems []string
	check := func(name string, v int) {
		if v < 0 {
			problems = append(problems, fmt.Sprintf("%s is %d; a negative bound is a typo", name, v))
		}
	}
	check("MaxCommits", l.MaxCommits)
	check("MaxPeople", l.MaxPeople)
	check("MaxCommitsPerPerson", l.MaxCommitsPerPerson)
	check("MaxFilesPerPerson", l.MaxFilesPerPerson)
	check("MaxUntouchedFiles", l.MaxUntouchedFiles)

	switch len(problems) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("ownership: invalid limits: %s", problems[0])
	default:
		msg := fmt.Sprintf("ownership: %d invalid limits:", len(problems))
		for _, p := range problems {
			msg += "\n  - " + p
		}
		return fmt.Errorf("%s", msg)
	}
}

// Analyzer joins a blast radius to a repository's history.
//
// Constructed with functional options per CLAUDE.md §2 and holding no mutable
// state, so one Analyzer is safe to share and two concurrent Analyze calls
// cannot observe each other.
type Analyzer struct {
	hist   History
	limits Limits
}

// Option configures an Analyzer.
type Option func(*Analyzer) error

// WithLimits overrides the bounds. Zero fields keep their defaults.
func WithLimits(l Limits) Option {
	return func(a *Analyzer) error {
		if err := l.Validate(); err != nil {
			return err
		}
		a.limits = l.withDefaults()
		return nil
	}
}

// New returns an Analyzer reading history through h.
func New(h History, opts ...Option) (*Analyzer, error) {
	if h == nil {
		return nil, fmt.Errorf("ownership: history port is nil")
	}
	a := &Analyzer{hist: h, limits: DefaultLimits()}
	for i, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("ownership: option %d is nil", i)
		}
		if err := opt(a); err != nil {
			return nil, fmt.Errorf("ownership: apply option %d: %w", i, err)
		}
	}
	return a, nil
}

// ------------------------------------------------------------------ the report

// Reach describes the blast radius the ranking is a proportion OF.
//
// Every field is copied from the impact report rather than recomputed, so the
// two can never disagree about how wide the change is. The confidence floor and
// the hidden count travel with it because a share of a narrowed radius is a
// different number from a share of the whole one, and a reader who cannot see
// which they are looking at cannot calibrate the names below.
type Reach struct {
	// Symbols is every symbol a change to the target can reach, the target
	// included: a change to a symbol reaches the symbol.
	Symbols int `json:"symbols"`

	// Attributable is the subset with a file path in the index — the
	// denominator of every share below. A symbol the index located nowhere
	// cannot be attributed to anybody, and quietly counting it in the
	// denominator would cap every share below 100% for a reason no reader could
	// see.
	Attributable int `json:"attributable"`
	Unlocatable  int `json:"unlocatable,omitempty"`

	Files int `json:"files"`

	Direct   int `json:"direct"`
	Indirect int `json:"indirect"`
	Dispatch int `json:"dispatch"`

	ConfidenceFloor     model.Confidence `json:"confidence_floor,omitempty"`
	LowConfidenceHidden int              `json:"low_confidence_hidden,omitempty"`
	Truncated           bool             `json:"truncated,omitempty"`
	TruncatedReason     string           `json:"truncated_reason,omitempty"`
}

// CommitRef is one commit offered as evidence for a person's context.
type CommitRef struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	When    int64  `json:"when"`
	// Files are the REACHED files this commit touched, sorted. Not every file
	// the commit touched: the rest are not evidence of anything about this
	// change, and listing them would inflate a small commit into a large one.
	Files []string `json:"files"`
}

// Person is one git identity and the part of the blast radius they have touched.
//
// "Has touched", throughout. Not owns, not is responsible for, not introduced.
// Every field name and every derived sentence here is chosen so that a renderer
// which prints them verbatim still makes only the claim git supports. See the
// package doc.
type Person struct {
	// Name and Email are reproduced exactly as git recorded them, from the most
	// recent commit this identity appears in.
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`

	// Identity is the key rows were grouped by — the lower-cased email, or the
	// lower-cased name when no email was recorded. Exposed because two rows that
	// look like the same person and did not merge are otherwise inexplicable.
	Identity string `json:"identity"`

	// Symbols is how many of Reach.Attributable sit in files this person has
	// touched within the window. Share is that over Reach.Attributable.
	Symbols int     `json:"symbols"`
	Share   float64 `json:"share"`

	// FilesTouched is the total; Files is the capped list, sorted.
	FilesTouched int      `json:"files_touched"`
	Files        []string `json:"files"`

	// CommitCount is the total; Commits is the capped list, newest first.
	CommitCount int         `json:"commit_count"`
	Commits     []CommitRef `json:"commits"`

	// LastTouchUnix is the author time of their newest commit touching the
	// reached set. Author time rather than commit time, matching
	// internal/acquire/git: a rebase must not make untouched work look recent.
	LastTouchUnix int64 `json:"last_touch_unix"`

	// Basis states in one sentence what the numbers above are, so a row lifted
	// out of the report on its own still carries its own denominator.
	Basis string `json:"basis"`
}

// Window is how much history the report actually saw.
//
// Reported on every run, not only when it bites. The window is the difference
// between "nobody has touched this code" and "nobody has touched this code
// lately", and those render identically in a list of names.
type Window struct {
	Requested   int   `json:"requested"`
	CommitsRead int   `json:"commits_read"`
	NewestUnix  int64 `json:"newest_unix,omitempty"`
	OldestUnix  int64 `json:"oldest_unix,omitempty"`

	// Exhausted is true when the walk returned fewer commits than it asked for,
	// which means it reached the beginning of history and the window is not
	// hiding anything.
	Exhausted bool `json:"exhausted"`
}

// Untouched is the part of the blast radius no commit in the window touched.
//
// The most useful thing in the report when it is not empty: it is the code a
// change reaches that nobody currently visible has recent context on, which is
// where a reviewer's attention is worth the most.
type Untouched struct {
	Files     []string `json:"files"`
	FileCount int      `json:"file_count"`
	Symbols   int      `json:"symbols"`
}

// Report is one routing answer.
type Report struct {
	Target capability.Target `json:"target"`
	Reach  Reach             `json:"reach"`

	// PeopleFound is how many identities touched the reached set; People is the
	// capped ranking. Two fields so a cap never reads as a total.
	PeopleFound int      `json:"people_found"`
	People      []Person `json:"people"`

	Window    Window    `json:"window"`
	Untouched Untouched `json:"untouched"`
	Limits    Limits    `json:"limits"`

	// Caveats are the limitations of this answer, as data rather than as
	// documentation. They are assembled by the analyzer, they are never empty,
	// and a renderer that prints the names without them is printing a claim the
	// evidence does not support. See the package doc.
	Caveats []string `json:"caveats"`
}

// ------------------------------------------------------------------ analysis

// Analyze ranks the people who have touched the blast radius in rep.
//
// rep is taken whole rather than as a list of symbols so that the honesty fields
// the impact analysis computed — the confidence floor, what the floor hid,
// whether the traversal was truncated — travel into this report instead of being
// dropped at the boundary. A share of a radius is meaningless without them.
func (a *Analyzer) Analyze(ctx context.Context, rep impact.Report) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("ownership: analyze %s: %w", rep.Target.FQN, err)
	}

	reach := collectReach(rep)
	out := Report{
		Target: rep.Target,
		Limits: a.limits,
		Reach: Reach{
			Symbols:             reach.total,
			Attributable:        reach.attributable,
			Unlocatable:         reach.total - reach.attributable,
			Files:               len(reach.files),
			Direct:              len(rep.Direct),
			Indirect:            len(rep.Indirect),
			Dispatch:            len(rep.Dispatch),
			ConfidenceFloor:     rep.ConfidenceFloor,
			LowConfidenceHidden: rep.LowConfidenceHidden,
			Truncated:           rep.Truncated,
			TruncatedReason:     rep.TruncatedReason,
		},
	}

	commits, err := a.hist.Commits(ctx, a.limits.MaxCommits)
	if err != nil {
		return Report{}, fmt.Errorf("ownership: read the last %d commit(s) of history: %w", a.limits.MaxCommits, err)
	}
	commits = normalizeCommits(commits)
	out.Window = windowOf(commits, a.limits.MaxCommits)

	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("ownership: analyze %s: %w", rep.Target.FQN, err)
	}

	people, touched := a.rank(reach, commits)
	out.PeopleFound = len(people)
	if len(people) > a.limits.MaxPeople {
		people = people[:a.limits.MaxPeople]
	}
	out.People = people
	out.Untouched = a.untouched(reach, touched)
	out.Caveats = out.caveats()
	return out, nil
}

// reachSet is the blast radius indexed by the only key git can be joined on.
type reachSet struct {
	files        []string           // sorted, deduplicated
	symbolsIn    map[string][]int64 // file path -> reached symbol ids, sorted
	total        int
	attributable int
}

// collectReach turns an impact report into "which reached symbols live in which
// files".
//
// The target is included. A change to a symbol reaches the symbol, and excluding
// it would mean a person who has worked on the target itself and nothing else
// ranked at zero — which is the opposite of the truth this report is for.
func collectReach(rep impact.Report) reachSet {
	r := reachSet{symbolsIn: map[string][]int64{}}
	seen := make(map[int64]bool)
	add := func(id int64, path string) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		r.total++
		if path == "" {
			return
		}
		r.attributable++
		if _, known := r.symbolsIn[path]; !known {
			r.files = append(r.files, path)
		}
		r.symbolsIn[path] = append(r.symbolsIn[path], id)
	}

	add(rep.Target.SymbolID, rep.Target.FilePath)
	for _, im := range rep.Direct {
		add(im.ID, im.FilePath)
	}
	for _, im := range rep.Indirect {
		add(im.ID, im.FilePath)
	}
	for _, d := range rep.Dispatch {
		add(d.ID, d.FilePath)
	}

	sort.Strings(r.files)
	for path := range r.symbolsIn {
		ids := r.symbolsIn[path]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return r
}

// normalizeCommits copies, de-duplicates and orders the history.
//
// Ordered here rather than trusted from the port, for the reason internal/docs
// re-sorts its registry rows: the promise is "identical repository, identical
// report", and a promise that depends on an adapter's walk order staying put is
// not a promise. Newest first, SHA breaking the tie — two commits can share an
// author timestamp exactly, and scripted history makes that common rather than
// exotic.
func normalizeCommits(in []Commit) []Commit {
	out := make([]Commit, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, c := range in {
		if c.SHA == "" || seen[c.SHA] {
			continue
		}
		seen[c.SHA] = true
		c.Paths = append([]string(nil), c.Paths...)
		sort.Strings(c.Paths)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].When != out[j].When {
			return out[i].When > out[j].When
		}
		return out[i].SHA < out[j].SHA
	})
	return out
}

func windowOf(commits []Commit, requested int) Window {
	w := Window{Requested: requested, CommitsRead: len(commits), Exhausted: len(commits) < requested}
	if len(commits) > 0 {
		w.NewestUnix = commits[0].When
		w.OldestUnix = commits[len(commits)-1].When
	}
	return w
}

// tally accumulates one identity's touches while the commits are walked.
type tally struct {
	key, name, email string
	files            map[string]bool
	fileOrder        []string
	symbols          int
	commits          []CommitRef
	last             int64
}

// rank walks the ordered commits once and produces the ranking.
//
// Returns the set of reached files somebody touched, so the caller can report
// the complement without walking the history twice.
func (a *Analyzer) rank(r reachSet, commits []Commit) ([]Person, map[string]bool) {
	touched := make(map[string]bool)
	byKey := make(map[string]*tally)
	// order preserves first-seen order so the aggregation itself never depends
	// on map iteration; the result is sorted explicitly below regardless.
	var order []string

	for _, c := range commits {
		var hits []string
		for _, p := range c.Paths { // sorted by normalizeCommits
			if _, reached := r.symbolsIn[p]; !reached {
				continue
			}
			touched[p] = true
			hits = append(hits, p)
		}
		if len(hits) == 0 {
			continue
		}

		key, name := identityOf(c)
		t, known := byKey[key]
		if !known {
			// Commits are newest first, so the first sighting of an identity
			// carries the name they used most recently. Picking any other one
			// means addressing somebody by a name they have since changed.
			t = &tally{key: key, name: name, email: strings.TrimSpace(c.AuthorEmail), files: map[string]bool{}}
			byKey[key] = t
			order = append(order, key)
		}
		for _, p := range hits {
			if t.files[p] {
				continue
			}
			t.files[p] = true
			t.fileOrder = append(t.fileOrder, p)
			t.symbols += len(r.symbolsIn[p])
		}
		t.commits = append(t.commits, CommitRef{SHA: c.SHA, Subject: c.Subject, When: c.When, Files: hits})
		if c.When > t.last {
			t.last = c.When
		}
	}

	people := make([]Person, 0, len(order))
	for _, key := range order {
		t := byKey[key]
		files := append([]string(nil), t.fileOrder...)
		sort.Strings(files)
		p := Person{
			Name:          t.name,
			Email:         t.email,
			Identity:      t.key,
			Symbols:       t.symbols,
			Share:         share(t.symbols, r.attributable),
			FilesTouched:  len(files),
			CommitCount:   len(t.commits),
			LastTouchUnix: t.last,
		}
		if len(files) > a.limits.MaxFilesPerPerson {
			files = files[:a.limits.MaxFilesPerPerson]
		}
		p.Files = files
		cs := t.commits // already newest first: the walk order
		if len(cs) > a.limits.MaxCommitsPerPerson {
			cs = cs[:a.limits.MaxCommitsPerPerson]
		}
		p.Commits = cs
		p.Basis = basis(p, r.attributable)
		people = append(people, p)
	}

	// The ranking rule, and it is total: no two rows can compare equal, so the
	// order cannot depend on sort stability or on the order the map was filled.
	sort.Slice(people, func(i, j int) bool {
		x, y := people[i], people[j]
		// Proportion of the blast radius first — the whole point of routing by
		// reachability rather than by directory.
		if x.Symbols != y.Symbols {
			return x.Symbols > y.Symbols
		}
		// Then who saw it most recently: context decays.
		if x.LastTouchUnix != y.LastTouchUnix {
			return x.LastTouchUnix > y.LastTouchUnix
		}
		if x.CommitCount != y.CommitCount {
			return x.CommitCount > y.CommitCount
		}
		return x.Identity < y.Identity
	})
	return people, touched
}

func (a *Analyzer) untouched(r reachSet, touched map[string]bool) Untouched {
	var u Untouched
	var files []string
	for _, f := range r.files { // sorted
		if touched[f] {
			continue
		}
		files = append(files, f)
		u.Symbols += len(r.symbolsIn[f])
	}
	u.FileCount = len(files)
	if len(files) > a.limits.MaxUntouchedFiles {
		files = files[:a.limits.MaxUntouchedFiles]
	}
	u.Files = files
	return u
}

// identityOf returns the grouping key and the display name for a commit.
//
// The email is the key because it is the field that survives a person changing
// how they write their own name, and it is lower-cased because git does not
// normalise it and "A@Example.com" and "a@example.com" are one person in every
// case that matters. A commit with no email falls back to the name, marked so
// the two key spaces cannot collide.
func identityOf(c Commit) (key, name string) {
	name = strings.TrimSpace(c.AuthorName)
	email := strings.ToLower(strings.TrimSpace(c.AuthorEmail))
	switch {
	case email != "":
		key = email
	case name != "":
		key = "name:" + strings.ToLower(name)
	default:
		key = "(no author recorded)"
		name = "(no author recorded)"
	}
	if name == "" {
		name = email
	}
	return key, name
}

func share(n, of int) float64 {
	if of <= 0 {
		return 0
	}
	return float64(n) / float64(of)
}

func basis(p Person, attributable int) string {
	return fmt.Sprintf("has touched %d of %d reached symbols (%.0f%%), across %s, in %s",
		p.Symbols, attributable, p.Share*100,
		plural(p.FilesTouched, "file", "files"),
		plural(p.CommitCount, "commit", "commits"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ------------------------------------------------------------------ caveats

// The sentences that must accompany any list of names this package produces.
//
// Exported as constants so a renderer, a test, or a downstream consumer can
// assert that a particular limitation was stated rather than matching on prose
// that a later edit might soften.
const (
	// CaveatNotOwnership is the one that must never be dropped.
	CaveatNotOwnership = "Git records who changed a file, not who owns it, understands it, still " +
		"works on it, or introduced a defect in it. Read this as who has context on the code this " +
		"change reaches — a suggestion for who to ask, never a finding about who is responsible."

	// CaveatFileLevel names the resolution of the attribution.
	//
	// It deliberately does not use the word the git subcommand for this is
	// named after. That word is the framing this whole report exists to avoid,
	// and a reader skimming a list of colleagues' names will take it from the
	// caveat regardless of the sentence it sits in.
	CaveatFileLevel = "Attribution is per FILE, not per line: a commit that touched a file is " +
		"credited with every reached symbol in it, so someone who changed one line of a large file " +
		"counts the same as the person who wrote the rest. Line-level attribution means asking git " +
		"who last changed each line, which costs seconds per file on a repository of any age and is " +
		"not affordable in an interactive query."

	// CaveatPronouns states why everybody is "they".
	CaveatPronouns = "Everyone here is referred to as \"they\": a commit records a name and an " +
		"email address, and nothing about the person behind them."
)

// caveats assembles the limitations of THIS report, in a fixed order.
//
// The first three are unconditional. The rest are conditional and quantified,
// because a caveat that fires on every report is a caveat nobody reads, while an
// unstated bound that changed the answer is a lie by omission.
func (r Report) caveats() []string {
	out := []string{CaveatNotOwnership, CaveatFileLevel, CaveatPronouns}

	switch {
	case r.Window.CommitsRead == 0:
		out = append(out, "No commit history was read at all, so no name here rests on anything. "+
			"Either this is not a git repository, or it has no commits yet.")
	case r.Window.Exhausted:
		out = append(out, fmt.Sprintf(
			"The whole history was read (%s from HEAD), so nobody is missing because of the window.",
			plural(r.Window.CommitsRead, "commit", "commits")))
	default:
		out = append(out, fmt.Sprintf(
			"Only the newest %s from HEAD were read. Anyone whose work on this code is older than "+
				"that is absent from this report, and absent reads exactly like never having touched it.",
			plural(r.Window.CommitsRead, "commit", "commits")))
	}

	out = append(out, fmt.Sprintf(
		"%s appear in this report. One person committing from two addresses appears twice; a shared or "+
			"automated account appears as one.",
		plural(r.PeopleFound, "git identity", "git identities")))

	if r.Reach.ConfidenceFloor != "" {
		hidden := ""
		if r.Reach.LowConfidenceHidden > 0 {
			hidden = fmt.Sprintf(" A walk without the floor reaches %d more symbol(s), which nobody "+
				"in this report is credited with.", r.Reach.LowConfidenceHidden)
		}
		out = append(out, fmt.Sprintf(
			"The blast radius was walked with a %s confidence floor, so every share here is a share "+
				"of that narrower radius.%s",
			r.Reach.ConfidenceFloor, hidden))
	}
	if r.Reach.Truncated {
		reason := r.Reach.TruncatedReason
		if reason == "" {
			reason = "a traversal bound was reached"
		}
		out = append(out, "The blast radius itself is truncated ("+reason+
			"), so the denominator of every share here is a lower bound.")
	}
	if r.Reach.Unlocatable > 0 {
		out = append(out, fmt.Sprintf(
			"%d reached symbol(s) have no file path in the index and could not be attributed to "+
				"anyone; they are excluded from every share rather than counted against everyone.",
			r.Reach.Unlocatable))
	}
	if r.PeopleFound > len(r.People) {
		out = append(out, fmt.Sprintf(
			"%d of %d identities are shown; the rest touched less of the reached set.",
			len(r.People), r.PeopleFound))
	}
	if r.Untouched.FileCount > 0 {
		out = append(out, fmt.Sprintf(
			"%s of the blast radius, holding %d reached symbol(s), were touched by nobody in this "+
				"window. That is where the least context exists.",
			plural(r.Untouched.FileCount, "file", "files"), r.Untouched.Symbols))
	}
	return out
}
