package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// This file pins the ANSWER, not the implementation.
//
// /api/who took 10.4 s at its default window because internal/acquire/git
// reopened the packfile for every object its walk read. The fix is a storage
// option — see internal/acquire/git/objectview.go — and the whole risk of a fix
// like that is that it quietly reads a different history. This screen names
// people. A colleague who appears, disappears, or moves up the ranking because
// of a descriptor-caching decision is an accusation nobody could audit, and it
// would look exactly like a correct answer.
//
// So: a scripted repository with two known authors, and every number the screen
// reports asserted exactly. TestOwnershipOverRealHistory next door checks the
// same path against THIS repository's real commits, which is the better test of
// realism and cannot assert a fixed answer; this is the one that can.

// whoAuthor is one identity in the scripted history.
type whoAuthor struct{ name, email string }

var (
	ada   = whoAuthor{"Ada Lovelace", "ada@example.com"}
	grace = whoAuthor{"Grace Hopper", "grace@example.com"}
)

// scriptedHistory builds a repository whose commits are fixed in author, order
// and timestamp, and returns its root.
//
// Built with go-git rather than by shelling out to `git`, matching what
// internal/acquire/git's own fixtures do and for the same reason: the product
// refuses to depend on the git binary, so its tests must not either.
func scriptedHistory(t *testing.T) string {
	t.Helper()

	// go-git reports the resolved worktree root, and on macOS t.TempDir() sits
	// under a symlink. Resolving here keeps the path prefix empty, which is
	// what makes commit paths line up with the index's.
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init repository: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("stage %s: %v", rel, err)
		}
	}
	commit := func(a whoAuthor, when int64, message string) {
		t.Helper()
		sig := &object.Signature{Name: a.name, Email: a.email, When: time.Unix(when, 0).UTC()}
		if _, err := wt.Commit(message, &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
			t.Fatalf("commit %q: %v", message, err)
		}
	}

	write("internal/core/alpha.go", "package core\n\nfunc Alpha() {}\n")
	write("internal/core/beta.go", "package core\n\nfunc Beta() {}\n")
	write("internal/core/gamma.go", "package core\n\nfunc Gamma() {}\n")
	commit(ada, 1700000001, "initial import")

	write("internal/core/alpha.go", "package core\n\nfunc Alpha() { /* 2 */ }\n")
	commit(ada, 1700000002, "tighten Alpha")

	write("internal/core/beta.go", "package core\n\nfunc Beta() { /* 3 */ }\n")
	commit(grace, 1700000003, "tighten Beta")

	write("internal/core/alpha.go", "package core\n\nfunc Alpha() { /* 4 */ }\n")
	commit(ada, 1700000004, "tighten Alpha again")

	return dir
}

// scriptedServer indexes three symbols at the scripted repository's three
// paths, with Beta and Gamma both reaching Alpha, so a change to Alpha has a
// blast radius of all three files.
func scriptedServer(t *testing.T, root string) *Server {
	t.Helper()
	deps, _ := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "Alpha", "internal/core/alpha.go").Export(),
			captest.S(2, "Beta", "internal/core/beta.go").Export(),
			captest.S(3, "Gamma", "internal/core/gamma.go").Export(),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceHigh),
			captest.E(3, 1).WithConf(model.ConfidenceHigh),
		},
		captest.Refs{Resolved: 6})
	return newServer(t, realRegistry(t, deps), deps,
		config.Config{ProjectName: "scripted", RepoRoot: root})
}

// TestOwnershipAnswerIsPinnedOverAScriptedHistory asserts every figure the
// screen publishes about people.
func TestOwnershipAnswerIsPinnedOverAScriptedHistory(t *testing.T) {
	t.Parallel()

	srv := scriptedServer(t, scriptedHistory(t))

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who?symbol=Alpha&commits=50", &got)

	if got.Status != BlastOK {
		t.Fatalf("status = %q: %+v", got.Status, got.Help)
	}
	if !got.History.Read {
		t.Fatalf("history was not read: %s", got.History.Note)
	}

	// The window. Four commits exist and fifty were asked for, so the walk
	// reached the beginning of history and hid nothing.
	if got.Window.Requested != 50 || got.Window.CommitsRead != 4 || !got.Window.Exhausted {
		t.Errorf("window = %+v, want requested 50, read 4, exhausted", got.Window)
	}

	// The reach every share below is a proportion OF.
	if got.Reach.Symbols != 3 || got.Reach.Attributable != 3 || got.Reach.Files != 3 {
		t.Errorf("reach = %+v, want 3 symbols in 3 files, all attributable", got.Reach)
	}

	// The ranking, exactly. Ada touched all three files across three commits;
	// Grace touched one file in one commit; Ada outranks her on share of the
	// radius, which is the ranking rule this screen exists to apply.
	if got.PeopleFound != 2 {
		t.Fatalf("people_found = %d, want the 2 identities in this history", got.PeopleFound)
	}
	want := []struct {
		name, identity string
		symbols        int
		share          float64
		files          []string
		commits        int
		last           int64
	}{
		{ada.name, ada.email, 3, 1.0,
			[]string{"internal/core/alpha.go", "internal/core/beta.go", "internal/core/gamma.go"}, 3, 1700000004},
		{grace.name, grace.email, 1, 1.0 / 3.0,
			[]string{"internal/core/beta.go"}, 1, 1700000003},
	}
	if len(got.People) != len(want) {
		t.Fatalf("people = %d rows, want %d: %+v", len(got.People), len(want), got.People)
	}
	for i, w := range want {
		p := got.People[i]
		if p.Name != w.name || p.Identity != w.identity {
			t.Errorf("people[%d] is %s <%s>, want %s <%s>", i, p.Name, p.Identity, w.name, w.identity)
		}
		if p.Symbols != w.symbols || p.Share != w.share {
			t.Errorf("people[%d] %s: %d symbols at share %v, want %d at %v",
				i, w.name, p.Symbols, p.Share, w.symbols, w.share)
		}
		if p.CommitCount != w.commits || p.LastTouchUnix != w.last {
			t.Errorf("people[%d] %s: %d commit(s), last %d; want %d, %d",
				i, w.name, p.CommitCount, p.LastTouchUnix, w.commits, w.last)
		}
		if p.FilesTouched != len(w.files) {
			t.Errorf("people[%d] %s: files_touched %d, want %d", i, w.name, p.FilesTouched, len(w.files))
		}
		if len(p.Files) != len(w.files) {
			t.Fatalf("people[%d] %s: files %v, want %v", i, w.name, p.Files, w.files)
		}
		for j := range w.files {
			if p.Files[j] != w.files[j] {
				t.Fatalf("people[%d] %s: files %v, want %v", i, w.name, p.Files, w.files)
			}
		}
	}

	// The bus factor. alpha.go and gamma.go were touched only by Ada; beta.go
	// was touched by both, so it is not a single-contributor file.
	bf := got.BusFactor
	if bf.ReachedFiles != 3 || bf.TouchedFiles != 3 || bf.UntouchedFiles != 0 {
		t.Errorf("bus factor = %d reached / %d touched / %d untouched, want 3/3/0",
			bf.ReachedFiles, bf.TouchedFiles, bf.UntouchedFiles)
	}
	if bf.SingleContributor != 2 || !bf.Complete {
		t.Errorf("bus factor: %d single-contributor file(s), complete=%v; want 2 and true",
			bf.SingleContributor, bf.Complete)
	}
	wantBus := []BusFactorFile{
		{Path: "internal/core/alpha.go", Name: ada.name, Identity: ada.email, Commits: 3, LastTouchUnix: 1700000004},
		{Path: "internal/core/gamma.go", Name: ada.name, Identity: ada.email, Commits: 3, LastTouchUnix: 1700000004},
	}
	if len(bf.Files) != len(wantBus) {
		t.Fatalf("bus factor files = %+v, want %+v", bf.Files, wantBus)
	}
	for i, w := range wantBus {
		if bf.Files[i] != w {
			t.Errorf("bus factor file %d = %+v, want %+v", i, bf.Files[i], w)
		}
	}

	// Nothing older exists, so nothing may claim otherwise.
	if got.Untouched.FileCount != 0 {
		t.Errorf("untouched = %+v, want none", got.Untouched)
	}
	for _, om := range got.Omissions.Omitted {
		if om.Reason == ReasonTraversalBound {
			t.Errorf("an exhausted window reported a truncation: %+v", om)
		}
	}
}

// TestOwnershipStatesTheWindowItCouldNotSeePast.
//
// The remaining cost of this screen is linear in the window, so the window is
// the bound that stays. A ranking read without it is a list of names presented
// as the whole truth: work older than the window is absent, and absent reads
// exactly like never having touched the code. `window` has always carried the
// numbers; this asserts that `omissions` — the block this product treats as the
// place a truncation is stated — carries it too.
func TestOwnershipStatesTheWindowItCouldNotSeePast(t *testing.T) {
	t.Parallel()

	srv := scriptedServer(t, scriptedHistory(t))

	var got OwnershipResponse
	pillarGet(t, srv.handleOwnership, "/api/who?symbol=Alpha&commits=2", &got)

	if got.Window.CommitsRead != 2 || got.Window.Exhausted {
		t.Fatalf("window = %+v, want 2 commits read and not exhausted", got.Window)
	}

	var stated *Omission
	for i, om := range got.Omissions.Omitted {
		if om.Reason == ReasonTraversalBound {
			stated = &got.Omissions.Omitted[i]
			break
		}
	}
	if stated == nil {
		t.Fatalf("a window that stopped 2 commits from HEAD stated no omission: %+v", got.Omissions.Omitted)
	}
	if stated.Remedy == "" {
		t.Error("the omission names no control that widens the window")
	}
	if got.Omissions.Complete {
		t.Error("a truncated history reported a complete answer")
	}

	// And the truncation must not have changed the ranking's shape: the two
	// newest commits are Ada's on alpha.go and Grace's on beta.go.
	if got.PeopleFound != 2 {
		t.Errorf("people_found = %d over the 2-commit window, want 2", got.PeopleFound)
	}
	if got.Untouched.FileCount != 1 {
		t.Errorf("untouched = %d file(s), want gamma.go alone", got.Untouched.FileCount)
	}
}
