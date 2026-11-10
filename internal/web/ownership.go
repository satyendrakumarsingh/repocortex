package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/ownership"
)

// Who has CONTEXT on the code a change reaches — routed by reachability, from
// git history, not from a CODEOWNERS file.
//
// # Why this is not CODEOWNERS
//
// CODEOWNERS routes by the directory a file sits in. That is the right answer
// to "who administers this path" and the wrong answer to "who has seen the code
// this change can break": a two-line edit to a shared decoder reaches thirty
// call sites in six packages, and the person listed for the decoder's directory
// may never have touched one of them. This screen computes the blast radius
// first and asks history about THAT set.
//
// # CONTEXT, not ownership, in every field name and every sentence
//
// Git records that somebody changed a file. Not that they understand it, own
// it, still work here, or introduced a defect. Attribution is per file rather
// than per line, the history window is bounded, and a git identity is an
// address rather than a person. capability/ownership assembles those caveats
// itself and they travel to the client verbatim, because a ranked list of
// colleagues' names read without them becomes an accusation the first time
// something breaks.
//
// # Nothing here is ranked in this package
//
// The radius is capability/impact and the ranking is capability/ownership,
// reached through their own types so every honesty field survives the boundary.
// What this file adds is the BUS FACTOR — reached files exactly one identity in
// the window has touched — which is a projection of the ranking rather than a
// second opinion about it, and the adapter that puts git history behind
// ownership's port.

// ownership bounds. The commit walk is still the expensive half of this screen,
// but it is no longer the reason it was slow. This request took 10.4 s at its
// default window against 9 ms for the same blast radius on /api/blast, and the
// difference was not the tree diffing: go-git opened and closed the packfile
// for every one of the 30,798 objects the walk read. internal/acquire/git now
// holds those descriptors open for the duration of one walk — see
// objectview.go — which puts 200 commits at ~150 ms and 400 at ~300 ms.
//
// What remains is linear in the window, so the window is still a bound with a
// cost behind it, and ownershipMaxCommits is still what stops a caller asking
// for the whole of a large history on an interactive screen.
const (
	ownershipMaxPeopleRanked = 100
	ownershipListedPeople    = 25
	ownershipFilesPerPerson  = 200
	ownershipUntouchedFiles  = 50
	ownershipBusFactorFiles  = 50
	ownershipMaxCommits      = 2000
	ownershipMaxImpacted     = 100
)

// OwnershipFilters echoes the bounds actually applied.
type OwnershipFilters struct {
	MinConfidence string `json:"min_confidence"`
	MaxDepth      int    `json:"max_depth"`
	MaxCommits    int    `json:"max_commits"`
	MaxPeople     int    `json:"max_people"`
}

// HistoryNote states whether history was read at all, and why not when it was
// not.
//
// Its own field because "nobody has touched this code" and "no history was
// read" produce the same empty list and are opposite facts.
type HistoryNote struct {
	Read bool   `json:"read"`
	Root string `json:"repository_root,omitempty"`

	// Prefix is the project root relative to the worktree root, empty when they
	// are the same directory. A service inside a monorepo makes them different,
	// and a commit path that has not been rebased onto the project root matches
	// nothing — every person then ranks at zero and the report looks like a
	// repository nobody has ever touched.
	Prefix string `json:"path_prefix,omitempty"`

	Note string `json:"note,omitempty"`
}

// BusFactorFile is one reached file exactly one identity in the window has
// touched.
type BusFactorFile struct {
	Path string `json:"path"`

	// Name and Identity are the only person who has touched it inside the
	// window. Not the only person who ever has.
	Name     string `json:"name"`
	Identity string `json:"identity"`

	Commits       int   `json:"commits"`
	LastTouchUnix int64 `json:"last_touch_unix"`
}

// BusFactor is the concentration signal: how much of the blast radius rests on
// one person's context.
//
// WINDOW-SCOPED, and the field names say so. A file one person touched in the
// last 200 commits is not a file one person has ever touched, and the two are
// different claims about a team.
type BusFactor struct {
	// ReachedFiles is the denominator: files the blast radius lands in.
	ReachedFiles int `json:"reached_files"`

	// TouchedFiles is those anybody in the window touched. Untouched is the
	// remainder, and it is NOT a bus factor of one — it is a bus factor of
	// zero, which is worse and is counted separately for that reason.
	TouchedFiles   int `json:"touched_files"`
	UntouchedFiles int `json:"untouched_files"`

	// SingleContributor is how many touched files exactly one identity touched.
	SingleContributor int `json:"single_contributor_files"`

	// Files is the bounded listing of those.
	Files []BusFactorFile `json:"files"`

	// Complete is false when the ranking was capped before this was computed,
	// which makes SingleContributor an OVERSTATEMENT: a second contributor
	// outside the cap would have disqualified a file counted here.
	Complete bool `json:"complete"`

	// Detail is the sentence a reader needs before quoting the number.
	Detail string `json:"detail"`
}

// OwnershipResponse is the whole review-routing screen.
type OwnershipResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Status BlastStatus `json:"status"`
	Query  string      `json:"query"`

	Help       *Guidance   `json:"guidance,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`

	Filters OwnershipFilters `json:"filters"`

	Target *capability.Target `json:"target,omitempty"`

	// Reach is the blast radius the shares below are a proportion OF, copied
	// from the impact report rather than recomputed so the two cannot disagree.
	Reach ownership.Reach `json:"reach"`

	// Impacted is the bounded sample of reached symbols, each carrying its own
	// path-minimum confidence and its own evidence file:line. It is here so the
	// claim "these files are affected" is checkable on this screen rather than
	// only on the blast one.
	Impacted []impact.Impacted `json:"impacted"`

	// PeopleFound is how many identities touched the reached set; People is the
	// listed ranking. Two fields so a cap never reads as a total.
	PeopleFound int                `json:"people_found"`
	People      []ownership.Person `json:"people"`

	BusFactor BusFactor `json:"bus_factor"`

	Window    ownership.Window    `json:"window"`
	Untouched ownership.Untouched `json:"untouched"`
	History   HistoryNote         `json:"history"`

	// Caveats are ownership's own limitations, as data. Never empty, and a
	// client that renders the names without them is printing a claim the
	// evidence does not support.
	Caveats []string `json:"caveats"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

func (s *Server) handleOwnership(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("symbol"))

	floor := confidenceParam(r, "min_confidence", impact.DefaultMinConfidence)
	depth := intParam(r, "depth", impact.DefaultMaxDepth, 1, 6)
	commits := intParam(r, "commits", ownership.DefaultLimits().MaxCommits, 1, ownershipMaxCommits)

	resp := OwnershipResponse{
		Meta: meta, Legend: Legend(), Query: query,
		Filters: OwnershipFilters{
			MinConfidence: string(floor), MaxDepth: depth,
			MaxCommits: commits, MaxPeople: ownershipMaxPeopleRanked,
		},
		People: []ownership.Person{}, Impacted: []impact.Impacted{},
		BusFactor: BusFactor{Files: []BusFactorFile{}},
		Caveats:   []string{},
	}
	if !ok {
		resp.Status = BlastNoQuery
		resp.Help = meta.Help
		resp.Next = NextAction{Label: "Index this repository first", Command: "cortex index"}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if query == "" {
		resp.Status = BlastNoQuery
		resp.Help = &Guidance{
			Title: "Pick a symbol",
			Detail: "This screen routes a review by what a change REACHES, so it needs to know what " +
				"is changing. It is not a directory-to-team map and cannot answer without one.",
			Steps: []string{
				"Search for a symbol by name in the box above.",
				"A bare name matching several definitions returns the candidates rather than guessing — re-ask with a fully-qualified name.",
			},
		}
		resp.Next = NextAction{
			Label:  "Start from a symbol on the blast-radius screen",
			Detail: "Whatever you would open there is what this screen routes.",
			Screen: "blast",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	rep, err := impact.AnalyzeWith(ctx, s.deps.Caps, run.ID, query, impact.Options{
		MaxDepth: depth, MinConfidence: floor,
	})
	if err != nil {
		var amb *capability.AmbiguousTargetError
		switch {
		case errors.As(err, &amb):
			resp.Status = BlastAmbiguous
			resp.Candidates = candidatesOf(amb.Candidates)
			resp.Help = &Guidance{
				Title: fmt.Sprintf("%q matches %d definitions", query, len(amb.Candidates)),
				Detail: "The portal will not pick one. Routing a review from the blast radius of the " +
					"wrong symbol names the wrong people, which is worse than naming nobody.",
				Steps: []string{"Choose a candidate below, or re-ask with its fully-qualified name."},
			}
			resp.Next = NextAction{
				Label: fmt.Sprintf("Pick one of the %d definitions", len(amb.Candidates)),
				Detail: "Each candidate below carries its file and line. Re-ask with the " +
					"fully-qualified name of the one you are changing.",
				Screen: "blast",
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		case errors.Is(err, capability.ErrTargetNotFound):
			resp.Status = BlastNotFound
			resp.Help = &Guidance{
				Title:  fmt.Sprintf("No symbol named %q in this index", query),
				Detail: "A normal outcome — a typo, or a symbol this index could not extract from its language.",
				Steps: []string{
					"Check the spelling, and try the fully-qualified name.",
					"Check the overview's blind spots: a language with no extracted symbols of that kind will never match.",
				},
			}
			resp.Next = NextAction{
				Label: "Check what this index could extract",
				Detail: "A language whose symbols of that kind were never extracted will never match, " +
					"and an empty answer there is a gap rather than an absence.",
				Screen: "overview#blind-spots",
			}
			s.writeJSON(w, http.StatusOK, resp)
			return
		default:
			s.fail(w, r, err)
			return
		}
	}

	var om Omissions
	resp.Status = BlastOK
	target := rep.Target
	resp.Target = &target

	hist := s.openHistory()
	resp.History = hist.note()

	limits := ownership.Limits{
		MaxCommits:          commits,
		MaxPeople:           ownershipMaxPeopleRanked,
		MaxCommitsPerPerson: ownership.DefaultLimits().MaxCommitsPerPerson,
		MaxFilesPerPerson:   ownershipFilesPerPerson,
		MaxUntouchedFiles:   ownershipUntouchedFiles,
	}
	an, err := ownership.New(hist, ownership.WithLimits(limits))
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: build ownership analyzer: %w", err))
		return
	}
	report, err := an.Analyze(ctx, rep)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: route review for %q: %w", query, err))
		return
	}

	resp.Reach = report.Reach
	resp.Window = report.Window
	resp.Untouched = report.Untouched
	resp.Caveats = report.Caveats
	resp.PeopleFound = report.PeopleFound

	// The bus factor is computed over the RANKED list, before it is shortened
	// for display: shortening first would count a file's second contributor as
	// absent and turn a shared file into a single-contributor one.
	resp.BusFactor = busFactorOf(report)

	people := capped(report.People, ownershipListedPeople, &om, ReasonRowBudget,
		fmt.Sprintf("Identities beyond the first %d (ranked by share of the radius, then recency) "+
			"are not listed. people_found above is the full figure.", ownershipListedPeople),
		"cortex who <symbol> --limit 100")
	if people != nil {
		resp.People = people
	}
	if report.PeopleFound > ownershipMaxPeopleRanked {
		om.Hide(ReasonRowBudget, report.PeopleFound-ownershipMaxPeopleRanked,
			fmt.Sprintf("Identities beyond the first %d were not ranked at all, which makes the "+
				"bus-factor count above an OVERSTATEMENT: a second contributor outside that cap "+
				"would have disqualified a file counted as single-contributor.",
				ownershipMaxPeopleRanked),
			"cortex who <symbol> --limit 500")
	}

	impacted := append(append([]impact.Impacted{}, rep.Direct...), rep.Indirect...)
	resp.Impacted = capped(impacted, ownershipMaxImpacted, &om, ReasonRowBudget,
		fmt.Sprintf("Reached symbols beyond the first %d are not listed. reach.symbols above is "+
			"the full figure and is what every share is computed over — only the sample is "+
			"shortened.", ownershipMaxImpacted),
		"open the blast radius for the full set")

	om.Hide(ReasonBelowConfidenceFloor, rep.LowConfidenceHidden,
		fmt.Sprintf("Symbols that reach %s only through edges weaker than %s. They are excluded "+
			"from the radius, so the files they live in are excluded from the routing and nobody "+
			"is named for them.", target.Name, tierWord(floor)),
		"set the standard of proof to `low`")
	if rep.Truncated {
		om.Note(ReasonTraversalBound,
			"The traversal stopped before exhausting the reachable set: "+rep.TruncatedReason+
				". Files beyond that boundary have nobody named for them because they were never reached.",
			"lower ?depth=")
	}
	// The history window is a truncation like any other, and this block is where
	// this product states truncations. Window already carries the numbers, but a
	// client that renders `omissions` and not `window` would otherwise show a
	// ranking with nothing beside it saying the ranking has an edge — and older
	// work missing from a list of names reads exactly like work that never
	// happened. Stated only when the walk actually stopped short: on a history
	// it exhausted there is nothing older to have missed.
	if report.Window.CommitsRead > 0 && !report.Window.Exhausted {
		om.Note(ReasonTraversalBound,
			fmt.Sprintf("History was read %d commit(s) back from HEAD and no further. Anyone whose "+
				"work on these files is older than that is absent from the ranking, and the "+
				"bus-factor count is computed over the same window — a file counted as "+
				"single-contributor may have had others before it.", report.Window.CommitsRead),
			fmt.Sprintf("raise ?commits= (up to %d) to widen the window", ownershipMaxCommits))
	}
	if n := report.Untouched.FileCount - len(report.Untouched.Files); n > 0 {
		om.Hide(ReasonRowBudget, n,
			"Reached files nobody in the window touched, beyond the listed ones. untouched.file_count is the full figure.",
			"cortex who <symbol> --commits 1000")
	}
	if !hist.available() {
		om.Note(ReasonUnresolvedReferences,
			"No git history was read: "+resp.History.Note+" Every identity count on this screen is "+
				"zero for that reason, not because nobody has touched this code.",
			"run the portal from inside the git worktree")
	}

	resp.Next = ownershipNextAction(resp)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// busFactorOf projects the ranking into the concentration signal.
//
// One pass over the ranked identities' file lists. A file appearing under
// exactly one identity is a single-contributor file WITHIN THE WINDOW, and the
// distinction is carried in Detail rather than left to the field name.
func busFactorOf(rep ownership.Report) BusFactor {
	type holder struct {
		name, identity string
		commits        int
		last           int64
		count          int
	}
	byFile := map[string]*holder{}
	for _, p := range rep.People {
		for _, f := range p.Files {
			h, seen := byFile[f]
			if !seen {
				h = &holder{}
				byFile[f] = h
			}
			h.count++
			if h.count == 1 {
				h.name, h.identity = p.Name, p.Identity
				h.commits, h.last = p.CommitCount, p.LastTouchUnix
			}
		}
	}

	bf := BusFactor{
		ReachedFiles:   rep.Reach.Files,
		TouchedFiles:   len(byFile),
		UntouchedFiles: rep.Untouched.FileCount,
		Files:          []BusFactorFile{},
		Complete:       rep.PeopleFound <= len(rep.People),
	}
	for path, h := range byFile {
		if h.count != 1 {
			continue
		}
		bf.SingleContributor++
		bf.Files = append(bf.Files, BusFactorFile{
			Path: path, Name: h.name, Identity: h.identity,
			Commits: h.commits, LastTouchUnix: h.last,
		})
	}
	sort.Slice(bf.Files, func(i, j int) bool { return bf.Files[i].Path < bf.Files[j].Path })
	if len(bf.Files) > ownershipBusFactorFiles {
		bf.Files = bf.Files[:ownershipBusFactorFiles]
	}

	bf.Detail = fmt.Sprintf(
		"%d of the %d reached file(s) that anybody touched in this window of %d commit(s) were "+
			"touched by exactly ONE identity. That is a bus factor of one WITHIN THE WINDOW — not a "+
			"claim that one person has ever been the only one to touch them. The %d file(s) nobody "+
			"touched are counted separately: those have a bus factor of zero, which is worse.",
		bf.SingleContributor, bf.TouchedFiles, rep.Window.CommitsRead, bf.UntouchedFiles)
	if !bf.Complete {
		bf.Detail += " The ranking was capped before this was computed, so the count is an upper bound."
	}
	return bf
}

func ownershipNextAction(resp OwnershipResponse) NextAction {
	switch {
	case !resp.History.Read:
		return NextAction{
			Label:   "No history was read, so nobody could be named",
			Detail:  "The blast radius above is still the index's. Only the routing half is missing.",
			Command: "git log -1",
		}
	case resp.Untouched.FileCount > 0:
		return NextAction{
			Label: fmt.Sprintf("%d reached file(s) nobody in this window has touched",
				resp.Untouched.FileCount),
			Detail: "This is where a reviewer's attention is worth the most: code the change reaches " +
				"that nobody currently visible has recent context on.",
			Screen: "blast",
		}
	case resp.BusFactor.SingleContributor > 0:
		return NextAction{
			Label: fmt.Sprintf("%d reached file(s) have a single contributor in this window",
				resp.BusFactor.SingleContributor),
			Detail: "Widen the window before treating that as a staffing fact — it is a property of " +
				"the last N commits, not of the repository.",
			Command: "cortex who <symbol> --commits 1000",
		}
	case len(resp.People) == 0:
		return NextAction{
			Label:   "Nobody in the history read has touched any of this",
			Detail:  "Either the window is too narrow or the reached files are genuinely untouched. Widening the window tells you which.",
			Command: "cortex who <symbol> --commits 1000",
		}
	default:
		return NextAction{
			Label:  "Read every row as \"has touched\", never as \"owns\"",
			Detail: "Git records that somebody changed a file, not that they understand it, own it, or still work here.",
			Screen: "blast",
		}
	}
}

// ---------------------------------------------------------------- git history

// gitHistory adapts internal/acquire/git to ownership.History.
//
// The same six-line adapter internal/cli holds, duplicated for the reason
// task.go states about compile's ports: a portal that imported the command
// layer to reach a closure would have inverted the arrow CLAUDE.md §1 is about.
//
// It does one translation and one refusal.
//
// The translation is the PATH SPACE. git reports paths relative to the worktree
// root; the index stores them relative to the project root, and a service inside
// a monorepo makes those different directories. Without the rebase every path
// comparison fails, every person ranks at zero, and the report reads like a
// repository nobody has ever touched — a wrong answer that looks exactly like a
// correct one.
//
// The refusal is that a directory which is not a repository, or one go-git
// declined to open fully, yields NO history rather than an error. Both are
// normal states, the radius is still worth showing without them, and the
// response says that no history was read.
type gitHistory struct {
	repo   *acquiregit.Repo
	root   string
	prefix string
	why    string
}

func (s *Server) openHistory() *gitHistory {
	root := strings.TrimSpace(s.deps.Config.RepoRoot)
	if root == "" {
		return &gitHistory{why: "this portal was started without a repository root, so there is no " +
			"worktree to read history from."}
	}
	repo, err := acquiregit.Open(root)
	if err != nil {
		return &gitHistory{why: err.Error() + "."}
	}
	if !repo.IsRepo() {
		return &gitHistory{why: root + " is not a git repository."}
	}
	if d := repo.Degraded(); d != "" {
		return &gitHistory{why: "this repository opened only in degraded mode (" + d +
			"), which leaves commit history unavailable."}
	}

	h := &gitHistory{repo: repo, root: repo.Root()}
	prefix, err := historyPrefix(repo.Root(), root)
	if err != nil {
		return &gitHistory{why: err.Error() + "."}
	}
	h.prefix = prefix
	return h
}

func (h *gitHistory) available() bool { return h.repo != nil }

func (h *gitHistory) note() HistoryNote {
	if h.repo == nil {
		return HistoryNote{Read: false, Note: "No history was read: " + h.why +
			" The blast radius below is still the index's."}
	}
	n := HistoryNote{Read: true, Root: h.root, Prefix: h.prefix}
	if h.prefix != "" {
		n.Note = fmt.Sprintf("History is read from the enclosing repository at %s and restricted "+
			"to %s/, which is what this index covers.", h.root, h.prefix)
	}
	return n
}

// Commits implements ownership.History.
func (h *gitHistory) Commits(ctx context.Context, limit int) ([]ownership.Commit, error) {
	if h.repo == nil {
		return nil, nil
	}
	cs, err := h.repo.RecentCommits(ctx, limit)
	if err != nil {
		if errors.Is(err, acquiregit.ErrNotRepository) {
			return nil, nil
		}
		return nil, fmt.Errorf("web: read history at %s: %w", h.root, err)
	}
	out := make([]ownership.Commit, 0, len(cs))
	for _, c := range cs {
		out = append(out, ownership.Commit{
			SHA: c.SHA, AuthorName: c.AuthorName, AuthorEmail: c.AuthorEmail,
			When: c.When, Subject: c.Subject,
			Paths: rebaseHistoryPaths(c.Paths, h.prefix),
		})
	}
	return out, nil
}

// historyPrefix is the project root expressed relative to the worktree root.
//
// Both sides are symlink-resolved first: on macOS a temporary directory is a
// symlink (/tmp -> /private/tmp) and go-git returns the resolved form while the
// caller carries the unresolved one, which makes the prefix look like "../../.."
// on exactly the paths a test runs over.
func historyPrefix(worktreeRoot, projectRoot string) (string, error) {
	wt := resolveSymlinks(worktreeRoot)
	pr := resolveSymlinks(projectRoot)

	rel, err := filepath.Rel(wt, pr)
	if err != nil {
		return "", fmt.Errorf("locate project root %s inside worktree %s: %w", projectRoot, worktreeRoot, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("project root %s is outside the worktree %s, so no commit path can "+
			"be matched against this index", projectRoot, worktreeRoot)
	}
	return rel, nil
}

func resolveSymlinks(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// rebaseHistoryPaths drops commit paths outside the project root and strips the
// prefix from the rest, leaving them in the index's path space.
func rebaseHistoryPaths(paths []string, prefix string) []string {
	if prefix == "" {
		out := append([]string(nil), paths...)
		sort.Strings(out)
		return out
	}
	pre := prefix + "/"
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, pre); ok && rest != "" {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}
