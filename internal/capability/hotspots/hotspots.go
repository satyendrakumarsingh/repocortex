// Package hotspots ranks files by how often they change against how much code
// they hold, and reports which files change together without a static edge
// between them.
//
// # What the two numbers mean
//
// A file that is large and never touched costs nothing. A file that is touched
// constantly and is three lines long costs nothing either. The files that hurt
// are the ones in both sets: every change to them is expensive, and there are
// many changes. That product is the hotspot score. It is a ranking device, not
// a measurement of quality, and the report prints both inputs beside it so a
// reader can disagree with the weighting.
//
// "How much code" is the file's size in bytes. It is a proxy, and a deliberate
// one: a real complexity metric needs a per-language control-flow model, and
// size is the proxy that correlates with it and that the index already holds
// for every one of its languages. The field is called Size, never Complexity.
//
// # Change coupling
//
// Two files that keep appearing in the same commits depend on each other
// whether or not one imports the other. When they do not, the dependency is
// invisible to every static tool, this one included; the call graph says a
// change to A cannot reach B, and the history says it reaches B four times out
// of five. That gap is what Coupling reports, and it is why the result belongs
// beside impact analysis rather than in a metrics dashboard.
//
// # Bounds
//
// History is read through a commit limit, never unbounded, and commits that
// touch more than MaxCommitFiles are excluded from coupling: a formatting
// sweep or a dependency bump couples every file to every other and says
// nothing about any of them. Excluded commits are counted in the report.
package hotspots

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// Commit is one commit reduced to what ranking needs. Paths must already be in
// the index's path space; see internal/capability/ownership.Commit, whose
// reasoning for restating the type at the consumer applies here unchanged.
type Commit struct {
	SHA   string
	When  int64 // author time, Unix seconds
	Paths []string
}

// History is the bounded commit source. An implementation that cannot honour
// limit must return an error rather than read everything.
type History interface {
	Commits(ctx context.Context, limit int) ([]Commit, error)
}

// Structure is the per-file read this package needs from the index.
type Structure interface {
	FileStructureFor(ctx context.Context, runID int64, paths []string) (map[string]registry.FileStructure, error)
}

var _ Structure = (*registry.Registry)(nil)

// Options bounds a ranking.
type Options struct {
	// Limit is the most hotspots to return. Zero means DefaultLimit.
	Limit int
	// CommitWindow is how many recent commits to read. Zero means
	// DefaultCommitWindow.
	CommitWindow int
	// MaxCommitFiles excludes larger commits from coupling. Zero means
	// DefaultMaxCommitFiles.
	MaxCommitFiles int
	// MinCoChanges is the fewest shared commits that count as coupling. Zero
	// means DefaultMinCoChanges.
	MinCoChanges int
	// PathPrefix restricts the ranking to files under it.
	PathPrefix string
	// IncludeTests keeps test files in the ranking. Off by default: tests
	// change with the code they cover, so they rank high for a reason that is
	// not a finding.
	IncludeTests bool
	// IncludeNonCode keeps files in which the index found no definition:
	// bundled assets, generated documents, lock files, data. Off by default.
	// They are large and regenerated often, so by size x churn they outrank
	// every source file while being nothing a reader can refactor.
	IncludeNonCode bool
}

const (
	DefaultLimit          = 20
	DefaultCommitWindow   = 500
	DefaultMaxCommitFiles = 30
	DefaultMinCoChanges   = 3
	// couplingPerFile is how many partners are listed for one hotspot.
	couplingPerFile = 3
)

// Partner is a file that changes together with a hotspot.
type Partner struct {
	Path string `json:"path"`
	// Together is the number of commits in the window touching both files.
	Together int `json:"commits_together"`
	// Share is Together over the hotspot's own commit count: of the times the
	// hotspot changed, how often this file changed with it.
	Share float64 `json:"share"`
}

// Hotspot is one ranked file.
type Hotspot struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`

	// Commits is how many commits in the window touched the file.
	Commits int `json:"commits"`
	// LastChanged is the author time of the most recent of them.
	LastChanged int64 `json:"last_changed_unix,omitempty"`

	// Size, Symbols and Dependents come from the index.
	Size       int64 `json:"size_bytes"`
	Symbols    int   `json:"symbols"`
	Dependents int   `json:"inbound_relationships"`

	// Score is the normalised product of change frequency and size, 0-100.
	// It orders the list and means nothing across two repositories.
	Score int `json:"score"`

	// ChangesWith are the files most often committed with this one.
	ChangesWith []Partner `json:"changes_with,omitempty"`
}

// Report is the answer to "where does change concentrate?".
type Report struct {
	RunID    int64     `json:"run_id"`
	Hotspots []Hotspot `json:"hotspots"`

	// CommitsRead is how much history the ranking is drawn from, and
	// WindowFull says whether more history exists beyond it.
	CommitsRead int  `json:"commits_read"`
	WindowFull  bool `json:"window_full"`
	// LargeCommitsSkipped counts commits left out of coupling for touching
	// more than MaxCommitFiles files.
	LargeCommitsSkipped int `json:"large_commits_skipped"`
	// FilesChanged is how many distinct indexed files the window touched.
	FilesChanged int `json:"files_changed"`
	// NotIndexed counts changed paths the index does not hold: deleted,
	// renamed or ignored files.
	NotIndexed int `json:"changed_paths_not_indexed"`
	// NonCodeSkipped counts changed files left out because the index found no
	// definition in them.
	NonCodeSkipped int `json:"non_code_files_skipped"`

	Notes []string `json:"notes,omitempty"`
}

// Rank reads the commit window and ranks the files it touched.
func Rank(ctx context.Context, hist History, st Structure, runID int64, opts Options) (*Report, error) {
	if hist == nil || st == nil {
		return nil, fmt.Errorf("hotspots: history and structure are both required")
	}
	limit := orDefault(opts.Limit, DefaultLimit)
	window := orDefault(opts.CommitWindow, DefaultCommitWindow)
	maxFiles := orDefault(opts.MaxCommitFiles, DefaultMaxCommitFiles)
	minTogether := orDefault(opts.MinCoChanges, DefaultMinCoChanges)

	commits, err := hist.Commits(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("hotspots: read history: %w", err)
	}

	rep := &Report{RunID: runID, CommitsRead: len(commits), WindowFull: len(commits) >= window}

	// Pass 1: change frequency per path.
	type churn struct {
		commits int
		last    int64
	}
	freq := map[string]*churn{}
	for _, c := range commits {
		for _, p := range uniq(c.Paths) {
			if !keep(p, opts) {
				continue
			}
			ch := freq[p]
			if ch == nil {
				ch = &churn{}
				freq[p] = ch
			}
			ch.commits++
			if c.When > ch.last {
				ch.last = c.When
			}
		}
	}
	if len(freq) == 0 {
		rep.Notes = append(rep.Notes, emptyNote(len(commits), opts))
		return rep, nil
	}

	paths := make([]string, 0, len(freq))
	for p := range freq {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	structure, err := st.FileStructureFor(ctx, runID, paths)
	if err != nil {
		return nil, fmt.Errorf("hotspots: %w", err)
	}
	rep.NotIndexed = len(paths) - len(structure)

	// Score = commits x size, normalised to the largest product in this
	// report. Normalising to the maximum rather than to fixed bounds is what
	// makes the number honest about being relative.
	var hot []Hotspot
	var maxRaw float64
	raws := map[string]float64{}
	for _, p := range paths {
		fs, ok := structure[p]
		if !ok {
			continue
		}
		if fs.Symbols == 0 && !opts.IncludeNonCode {
			rep.NonCodeSkipped++
			delete(structure, p) // also keeps it out of changes-with
			continue
		}
		ch := freq[p]
		// A size of zero would zero the product and tie every such file at
		// the bottom whatever its churn; one byte keeps the ordering by
		// commits. Real files are never empty and still worth ranking, so
		// this only matters to fixtures that do not record sizes.
		raw := float64(ch.commits) * float64(max(fs.SizeBytes, 1))
		raws[p] = raw
		if raw > maxRaw {
			maxRaw = raw
		}
		hot = append(hot, Hotspot{
			Path: p, Language: fs.Language,
			Commits: ch.commits, LastChanged: ch.last,
			Size: fs.SizeBytes, Symbols: fs.Symbols, Dependents: fs.Dependents,
		})
	}
	rep.FilesChanged = len(hot)
	for i := range hot {
		if maxRaw > 0 {
			hot[i].Score = int(100*raws[hot[i].Path]/maxRaw + 0.5)
		}
	}
	sort.Slice(hot, func(i, j int) bool {
		a, b := hot[i], hot[j]
		if ra, rb := raws[a.Path], raws[b.Path]; ra != rb {
			return ra > rb
		}
		return a.Path < b.Path
	})
	if len(hot) > limit {
		hot = hot[:limit]
	}

	if len(hot) == 0 {
		rep.Notes = append(rep.Notes, emptyNote(len(commits), opts))
	}

	// Pass 2: coupling, for the returned hotspots only.
	wanted := make(map[string]bool, len(hot))
	for _, h := range hot {
		wanted[h.Path] = true
	}
	together, skipped := coChanges(commits, wanted, structure, maxFiles, opts)
	rep.LargeCommitsSkipped = skipped
	for i := range hot {
		hot[i].ChangesWith = partners(together[hot[i].Path], hot[i].Commits, minTogether)
	}

	rep.Hotspots = hot
	if !rep.WindowFull {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"The repository's whole history (%d commits) fits inside the window.", len(commits)))
	}
	if rep.NotIndexed > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d changed path(s) are not in the index (deleted, renamed or ignored since) and are not ranked.",
			rep.NotIndexed))
	}
	if rep.NonCodeSkipped > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d changed file(s) hold no definition the index recognises (assets, generated text, data) "+
				"and are not ranked.", rep.NonCodeSkipped))
	}
	if skipped > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d commit(s) touching more than %d files were left out of the changes-with figures; "+
				"they still count toward each file's commit total.", skipped, maxFiles))
	}
	return rep, nil
}

// coChanges counts, for each wanted file, how often every other indexed file
// was committed with it.
func coChanges(
	commits []Commit,
	wanted map[string]bool,
	indexed map[string]registry.FileStructure,
	maxFiles int,
	opts Options,
) (map[string]map[string]int, int) {
	out := make(map[string]map[string]int, len(wanted))
	skipped := 0
	for _, c := range commits {
		paths := uniq(c.Paths)
		if len(paths) > maxFiles {
			skipped++
			continue
		}
		var anchors, all []string
		for _, p := range paths {
			if _, ok := indexed[p]; !ok || !keep(p, opts) {
				continue
			}
			all = append(all, p)
			if wanted[p] {
				anchors = append(anchors, p)
			}
		}
		for _, a := range anchors {
			m := out[a]
			if m == nil {
				m = map[string]int{}
				out[a] = m
			}
			for _, b := range all {
				if a != b {
					m[b]++
				}
			}
		}
	}
	return out, skipped
}

// partners picks the strongest couplings for one hotspot.
func partners(counts map[string]int, ownCommits, minTogether int) []Partner {
	var ps []Partner
	for p, n := range counts {
		if n < minTogether {
			continue
		}
		share := 0.0
		if ownCommits > 0 {
			share = float64(n) / float64(ownCommits)
		}
		ps = append(ps, Partner{Path: p, Together: n, Share: share})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Together != ps[j].Together {
			return ps[i].Together > ps[j].Together
		}
		return ps[i].Path < ps[j].Path
	})
	if len(ps) > couplingPerFile {
		ps = ps[:couplingPerFile]
	}
	return ps
}

func keep(p string, opts Options) bool {
	if opts.PathPrefix != "" && !strings.HasPrefix(p, opts.PathPrefix) {
		return false
	}
	if !opts.IncludeTests && capability.IsTestPath(p) {
		return false
	}
	return true
}

func emptyNote(commits int, opts Options) string {
	if commits == 0 {
		return "No commit history could be read, so nothing can be ranked. " +
			"Hotspots need a git repository with at least one commit."
	}
	if opts.PathPrefix != "" {
		return fmt.Sprintf("None of the last %d commits touched a file under %q.", commits, opts.PathPrefix)
	}
	return fmt.Sprintf("None of the last %d commits touched a file this ranking keeps.", commits)
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// uniq returns paths without repeats, order preserved. A commit can list one
// path twice (a rename onto itself, a merge), and counting it twice would
// inflate both the churn and the coupling of exactly the files with the
// messiest history.
func uniq(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
