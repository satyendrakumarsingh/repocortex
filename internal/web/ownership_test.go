package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/ownership"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestOwnershipWithoutASymbolAsksForOne: this screen routes by what a change
// REACHES, so it cannot answer without knowing what is changing. It is not a
// directory-to-team map and must not pretend to be one.
func TestOwnershipWithoutASymbolAsksForOne(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who", &got)

	if got.Status != BlastNoQuery {
		t.Errorf("status = %q, want %q", got.Status, BlastNoQuery)
	}
	if got.Help == nil || got.Help.Title == "" {
		t.Fatal("no guidance for a query-less request")
	}
	if len(got.People) != 0 {
		t.Error("people were named for no symbol at all")
	}
}

// TestOwnershipResolutionOutcomes covers the three ways a symbol query ends.
func TestOwnershipResolutionOutcomes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		query      string
		wantStatus BlastStatus
		wantTitle  string
	}{
		{name: "a symbol nothing matches", query: "?symbol=nosuchthing",
			wantStatus: BlastNotFound, wantTitle: "No symbol named"},
		{name: "a symbol that resolves", query: "?symbol=hub", wantStatus: BlastOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := fixture(t)

			var got OwnershipResponse
			pillarGet(t, srv.handleOwnership, "/api/who"+tc.query, &got)

			if got.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (guidance %+v)", got.Status, tc.wantStatus, got.Help)
			}
			if tc.wantTitle != "" {
				if got.Help == nil || !strings.Contains(got.Help.Title, tc.wantTitle) {
					t.Errorf("guidance = %+v, want a title containing %q", got.Help, tc.wantTitle)
				}
			}
			if tc.wantStatus != BlastOK {
				return
			}
			if got.Target == nil {
				t.Fatal("an ok response names no target")
			}
			if got.Reach.Symbols == 0 {
				t.Error("reach.symbols is 0 for a symbol with recorded callers")
			}
			if len(got.Caveats) == 0 {
				t.Error("no caveats: a ranked list of names without them becomes an accusation")
			}
		})
	}
}

// TestOwnershipImpactedRowsCarryTierAndEvidence.
//
// The routing is only as good as the radius it routes over, so the radius is on
// this screen with its own path-minimum confidence and its own file:line.
func TestOwnershipImpactedRowsCarryTierAndEvidence(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who?symbol=hub", &got)

	if len(got.Impacted) == 0 {
		t.Fatal("no impacted rows for a symbol with three callers above the floor")
	}
	var withEvidence int
	for _, im := range got.Impacted {
		if !im.Confidence.Valid() {
			t.Errorf("%s carries confidence %q, which is not a tier", im.Name, im.Confidence)
		}
		for _, e := range im.Evidence {
			if e.FilePath != "" && e.Range.Start.Line > 0 {
				withEvidence++
			}
		}
	}
	if withEvidence == 0 {
		t.Error("not one impacted row carries a file:line; without it an edge is an assertion")
	}

	// The default floor hides the `low` caller, and hiding it silently is the
	// one thing this package forbids.
	if got.Filters.MinConfidence != string(model.ConfidenceMedium) {
		t.Errorf("floor = %q, want medium", got.Filters.MinConfidence)
	}
	if got.Omissions.HiddenByFloor() == 0 {
		t.Errorf("the floor hid the `low` caller and said nothing: %+v", got.Omissions.Omitted)
	}
}

// TestOwnershipWithoutGitHistorySaysSoRatherThanNamingNobody.
//
// "Nobody has touched this code" and "no history was read" produce the same
// empty list and are opposite facts.
func TestOwnershipWithoutGitHistorySaysSoRatherThanNamingNobody(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t) // config.Config{} — no RepoRoot, so no worktree

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who?symbol=hub", &got)

	if got.History.Read {
		t.Fatal("history.read is true with no repository root wired")
	}
	if got.History.Note == "" || !strings.Contains(got.History.Note, "No history was read") {
		t.Errorf("history note = %q, want it to say no history was read", got.History.Note)
	}
	if got.PeopleFound != 0 {
		t.Errorf("people_found = %d with no history", got.PeopleFound)
	}
	var noted bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "No git history was read") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("the missing history is not recorded as an omission: %+v", got.Omissions.Omitted)
	}
	if got.Next.Label == "" {
		t.Error("the screen ends with no next action")
	}
	// The radius is still the index's, which is the half that does not need git.
	if got.Reach.Symbols == 0 {
		t.Error("the blast radius was dropped along with the history; only the routing half depends on git")
	}
}

// TestOwnershipOverRealHistory is the populated case.
//
// It indexes symbols at paths that really exist in THIS repository and points
// the portal at this worktree, so the ranking below comes from real commits.
// Skipped rather than faked when the checkout has no history: a test that
// invented commits would prove only that the fake agreed with the assertion.
func TestOwnershipOverRealHistory(t *testing.T) {
	t.Parallel()

	root := findWorktreeRoot(t)
	if root == "" {
		t.Skip("not running inside a git worktree; there is no history to route by")
	}

	touched := webFilesTouchedWithin(t, root, ownershipDerivationWindow)
	if len(touched) == 0 {
		t.Skipf("no internal/web/*.go file was touched in the last %d commits of this worktree; "+
			"there is no attribution left to assert over", ownershipDerivationWindow)
	}
	// probe picks the i-th derived path, repeating the last one when git named
	// fewer than three files. A repeated path narrows the radius; it never
	// makes the radius point at history that is not in the window, which is the
	// only property the assertions below depend on.
	probe := func(i int) string {
		if i < len(touched) {
			return touched[i]
		}
		return touched[len(touched)-1]
	}

	deps, _ := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "portalHub", probe(0)).Export(),
			captest.S(2, "portalBlast", probe(1)).Export(),
			captest.S(3, "portalGraph", probe(2)).Export(),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceHigh),
			captest.E(3, 1).WithConf(model.ConfidenceHigh),
		},
		captest.Refs{Resolved: 10})
	srv := newServer(t, realRegistry(t, deps), deps,
		config.Config{ProjectName: "cortex", RepoRoot: root})

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who?symbol=portalHub&commits=40", &got)

	if got.Status != BlastOK {
		t.Fatalf("status = %q: %+v", got.Status, got.Help)
	}
	if !got.History.Read {
		t.Skipf("history could not be read: %s", got.History.Note)
	}
	if got.Window.CommitsRead == 0 {
		t.Skip("this worktree has no commits in the window")
	}
	if got.Filters.MaxCommits != 40 {
		t.Errorf("max_commits = %d, want the requested 40", got.Filters.MaxCommits)
	}
	if got.Window.Requested != 40 {
		t.Errorf("window.requested = %d, want 40", got.Window.Requested)
	}
	if got.PeopleFound == 0 {
		t.Fatalf("git reports %v changed inside the last %d commits, yet the ownership machinery "+
			"attributed those commits to nobody", touched, ownershipDerivationWindow)
	}
	for _, p := range got.People {
		if p.Basis == "" {
			t.Errorf("%s is ranked with no basis sentence", p.Name)
		}
		if p.Share > 1.0001 {
			t.Errorf("%s has share %v, which is above the whole radius", p.Name, p.Share)
		}
		if len(p.Commits) == 0 {
			t.Errorf("%s is ranked with no commits as evidence, which makes the row an assertion", p.Name)
		}
		for _, c := range p.Commits {
			if c.SHA == "" || len(c.Files) == 0 {
				t.Errorf("%s cites commit %q with no files", p.Name, c.SHA)
			}
		}
	}

	// The bus factor is the signal this screen adds, and it must be scoped.
	if got.BusFactor.ReachedFiles == 0 {
		t.Errorf("bus_factor.reached_files is 0 over a radius covering %v", touched)
	}
	if got.BusFactor.Detail == "" || !strings.Contains(got.BusFactor.Detail, "WITHIN THE WINDOW") {
		t.Errorf("bus factor detail does not scope itself to the window: %q", got.BusFactor.Detail)
	}
}

// TestBusFactorCountsOnlySingleContributorFiles pins the projection itself,
// which the handler test above can only observe indirectly.
func TestBusFactorCountsOnlySingleContributorFiles(t *testing.T) {
	t.Parallel()

	person := func(name string, files ...string) ownership.Person {
		return ownership.Person{Name: name, Identity: name + "@example.com", Files: files,
			CommitCount: len(files), LastTouchUnix: 10}
	}

	for _, tc := range []struct {
		name string
		rep  ownership.Report

		wantSingle   int
		wantTouched  int
		wantComplete bool
	}{
		{
			name: "one shared file and two sole-contributor files",
			rep: ownership.Report{
				Reach:     ownership.Reach{Files: 4},
				Untouched: ownership.Untouched{FileCount: 1},
				People: []ownership.Person{
					person("ana", "a.go", "shared.go"),
					person("ben", "b.go", "shared.go"),
				},
				PeopleFound: 2,
			},
			wantSingle: 2, wantTouched: 3, wantComplete: true,
		},
		{
			name: "nobody touched anything",
			rep: ownership.Report{
				Reach: ownership.Reach{Files: 3}, Untouched: ownership.Untouched{FileCount: 3},
			},
			wantSingle: 0, wantTouched: 0, wantComplete: true,
		},
		{
			name: "a capped ranking makes the count an upper bound",
			rep: ownership.Report{
				Reach:       ownership.Reach{Files: 2},
				People:      []ownership.Person{person("ana", "a.go")},
				PeopleFound: 9,
			},
			wantSingle: 1, wantTouched: 1, wantComplete: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := busFactorOf(tc.rep)

			if got.SingleContributor != tc.wantSingle {
				t.Errorf("single_contributor_files = %d, want %d", got.SingleContributor, tc.wantSingle)
			}
			if got.TouchedFiles != tc.wantTouched {
				t.Errorf("touched_files = %d, want %d", got.TouchedFiles, tc.wantTouched)
			}
			if got.Complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", got.Complete, tc.wantComplete)
			}
			if got.UntouchedFiles != tc.rep.Untouched.FileCount {
				t.Errorf("untouched_files = %d, want %d — a bus factor of ZERO is worse than one "+
					"and must be counted separately", got.UntouchedFiles, tc.rep.Untouched.FileCount)
			}
			if len(got.Files) != tc.wantSingle {
				t.Errorf("listed %d single-contributor file(s), counted %d", len(got.Files), tc.wantSingle)
			}
			if !tc.wantComplete && !strings.Contains(got.Detail, "upper bound") {
				t.Errorf("a capped bus factor does not say so: %q", got.Detail)
			}
		})
	}
}

// TestHistoryPathsAreRebasedOntoTheProjectRoot.
//
// A git path that has not been rebased matches nothing, every person ranks at
// zero, and the report looks like a repository nobody has ever touched — a
// wrong answer that looks exactly like a correct one.
func TestHistoryPathsAreRebasedOntoTheProjectRoot(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prefix string
		in     []string
		want   []string
	}{
		{name: "no prefix passes everything through", prefix: "",
			in: []string{"b.go", "a.go"}, want: []string{"a.go", "b.go"}},
		{name: "a prefix strips and filters", prefix: "services/orders",
			in:   []string{"services/orders/main.go", "services/web/app.tsx", "README.md"},
			want: []string{"main.go"}},
		{name: "a prefix that matches nothing yields nothing", prefix: "services/orders",
			in: []string{"services/web/app.tsx"}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := rebaseHistoryPaths(tc.in, tc.prefix)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestHistoryPrefixRefusesAProjectOutsideTheWorktree.
func TestHistoryPrefixRefusesAProjectOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	elsewhere := t.TempDir()

	if _, err := historyPrefix(worktree, elsewhere); err == nil {
		t.Fatal("a project root outside the worktree was accepted; every path comparison would fail silently")
	}
	prefix, err := historyPrefix(worktree, worktree)
	if err != nil || prefix != "" {
		t.Fatalf("same directory gave (%q, %v), want (\"\", nil)", prefix, err)
	}
	sub := filepath.Join(worktree, "services", "orders")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prefix, err = historyPrefix(worktree, sub)
	if err != nil || prefix != "services/orders" {
		t.Fatalf("nested project gave (%q, %v), want (\"services/orders\", nil)", prefix, err)
	}
}

// ownershipDerivationWindow is how far back TestOwnershipOverRealHistory asks
// git for probe paths, and it is deliberately NARROWER than the 40 commits the
// handler is asked to read.
//
// `git log` walks in commit-date order and the handler walks in committer-time
// order (see internal/acquire/git/history.go). The two agree on nearly every
// repository, but a merge or a rewritten date can reorder commits near the
// boundary. The margin means a path git reports cannot land just outside the
// handler's own window because of that disagreement.
const ownershipDerivationWindow = 30

// webFilesTouchedWithin asks git which internal/web/*.go files real commits
// touched inside the last n commits of the worktree at root, sorted so the
// probe set is deterministic. Returns nil when git cannot be read at all.
//
// WHY the paths are derived rather than written down: an earlier version of
// this test named three files literally. Ordinary work moved on, the 40-commit
// window slid past the last commit that touched them — by exactly one commit —
// and the test started failing on a repository whose ownership machinery was
// perfectly healthy. Its real subject had become the project's commit cadence.
// Deriving the probe paths from the same history the handler reads puts the
// subject back where it belongs: that attribution turns real commits into real
// people. Please do not "simplify" this back into hardcoded paths.
func webFilesTouchedWithin(t *testing.T, root string, n int) []string {
	t.Helper()

	out, err := exec.Command("git", "-C", root, "log",
		"-n", strconv.Itoa(n), "--name-only", "--pretty=format:").Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if !strings.HasPrefix(p, "internal/web/") || !strings.HasSuffix(p, ".go") || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// findWorktreeRoot walks up from the test's directory looking for a .git entry.
func findWorktreeRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
