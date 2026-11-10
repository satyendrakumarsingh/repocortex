package git

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// The pooled object view is a pure performance change, so every test here is
// about it NOT being anything else. A faster walk that returns a different
// history would be a worse bug than the ten-second one it replaced: the
// Ownership screen names people, and a name that appears or disappears because
// of a descriptor-caching option is an accusation nobody could audit.

// unpooledRecentCommits is the walk exactly as it stood before objectview.go —
// straight off r.repo, one packfile open per object read.
//
// Restated here rather than kept behind a flag in the production walk: a
// production branch that only tests take is a branch nothing in production
// exercises, and the point of this file is to compare against what shipped.
func unpooledRecentCommits(t testing.TB, r *Repo, limit int) []Commit {
	t.Helper()
	iter, err := r.repo.Log(&gogit.LogOptions{Order: gogit.LogOrderCommitterTime})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		t.Fatalf("open commit log: %v", err)
	}
	defer iter.Close()

	out := make([]Commit, 0, limit)
	for len(out) < limit {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("walk commits: %v", err)
		}
		paths, perr := r.commitPaths(context.Background(), c)
		if perr != nil {
			t.Fatalf("commit paths: %v", perr)
		}
		out = append(out, Commit{
			SHA: c.Hash.String(), AuthorName: c.Author.Name, AuthorEmail: c.Author.Email,
			When: c.Author.When.Unix(), Subject: subject(c.Message), Paths: paths,
		})
	}
	return out
}

func sameCommits(t *testing.T, got, want []Commit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d commit(s), want %d", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.SHA != w.SHA || g.AuthorName != w.AuthorName || g.AuthorEmail != w.AuthorEmail ||
			g.When != w.When || g.Subject != w.Subject {
			t.Fatalf("commit %d metadata differs:\n got %+v\nwant %+v", i, g, w)
		}
		if len(g.Paths) != len(w.Paths) {
			t.Fatalf("commit %d (%s) touched %v, want %v", i, w.SHA, g.Paths, w.Paths)
		}
		for j := range w.Paths {
			if g.Paths[j] != w.Paths[j] {
				t.Fatalf("commit %d (%s) touched %v, want %v", i, w.SHA, g.Paths, w.Paths)
			}
		}
	}
}

// TestPooledWalkReadsTheSameHistory over a history holding every shape the walk
// treats specially: a root commit, plain edits, a rename, a deletion, a nested
// directory, and one commit touching two files at once.
func TestPooledWalkReadsTheSameHistory(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.write("pkg/b.go", filler(2))
	f.write("pkg/deep/c.go", filler(3))
	f.commit("initial import")

	f.write("a.go", filler(1)+"// edit\n")
	f.commit("edit a")

	f.rename("pkg/b.go", "pkg/moved/b.go")
	f.commit("move b")

	f.remove("pkg/deep/c.go")
	f.commit("delete c")

	f.write("pkg/moved/b.go", filler(2)+"// edit\n")
	f.write("a.go", filler(1)+"// second edit\n")
	f.commit("edit two files at once")

	r := f.open()

	// The comparison walk runs FIRST, so the pooled walk cannot be credited
	// with a warm page cache the reference walk paid for.
	want := unpooledRecentCommits(t, r, 20)

	got, err := r.RecentCommits(context.Background(), 20)
	if err != nil {
		t.Fatalf("RecentCommits: %v", err)
	}
	sameCommits(t, got, want)

	if len(got) != 5 {
		t.Fatalf("read %d commits, want the 5 this fixture has", len(got))
	}
	// A spot check that the comparison is not two identical wrong answers.
	if len(got[0].Paths) != 2 {
		t.Errorf("newest commit touched %v, want the two files it edited", got[0].Paths)
	}
}

// TestPooledWalkIsRepeatable: the view is opened and closed per walk, so a
// second walk must not be able to observe the first one's storer.
func TestPooledWalkIsRepeatable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.write("a.go", filler(i))
		f.commit("change")
	}
	r := f.open()

	first, err := r.RecentCommits(context.Background(), 10)
	if err != nil {
		t.Fatalf("first walk: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.RecentCommits(context.Background(), 10)
		if err != nil {
			t.Fatalf("walk %d: %v", i+2, err)
		}
		sameCommits(t, again, first)
	}
}

// packedRepoEnv names a repository to read instead of the enclosing worktree.
//
// A working checkout that has never been repacked has no packfile — `git
// count-objects -v` reports `packs: 0` — and everything below then skips. A
// fresh clone is packed, so CI exercises these; a long-lived development
// checkout may not, and this is how its owner points the tests at one that is.
const packedRepoEnv = "CORTEX_TEST_PACKED_REPO"

// packedRepo opens the git worktree this test binary is running inside (or the
// one packedRepoEnv names), and skips unless its objects are actually PACKED.
//
// Nothing in this package can build such a fixture: the fixture helpers commit
// through go-git, which writes loose objects, and go-git has no repack. That
// matters because the whole cost this file is about lives on the packfile path
// — a loose-object repository opens one small file per object and never touches
// a pack — so a test or benchmark run against a fixture would measure the wrong
// half of the storer and pass no matter what.
//
// Read-only: nothing here writes to the repository it is pointed at.
func packedRepo(tb testing.TB) *Repo {
	tb.Helper()

	dir := os.Getenv(packedRepoEnv)
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			tb.Skipf("cannot locate the enclosing worktree: %v", err)
		}
		dir = cwd
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Skip("not running inside a git worktree; there is no packed repository to read")
		}
		dir = parent
	}

	packs, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		tb.Skip("this checkout has no packfile, so the descriptor pool has nothing to hold open")
	}

	r, err := Open(dir)
	if err != nil {
		tb.Skipf("Open(%s): %v", dir, err)
	}
	if !r.isRepo || r.repo == nil {
		tb.Skip("the enclosing worktree did not open fully")
	}
	return r
}

// TestPooledWalkReleasesItsDescriptors is the reason the view is per-walk
// rather than an option on Open.
//
// internal/web opens a repository per HTTP request and never closes it. A
// descriptor kept open for the lifetime of the handle would trade a ten-second
// screen for a portal that runs out of file descriptors after a few hundred
// requests, which is a slower and far less obvious failure.
func TestPooledWalkReleasesItsDescriptors(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("this counts descriptors by their number, which is a POSIX property")
	}
	r := packedRepo(t)
	ctx := context.Background()

	// The garbage collector is stopped for the duration. An *os.File carries a
	// finalizer that closes its descriptor, so a leak here is INVISIBLE under a
	// running collector — a version of this test written without this line
	// passes with objectView.close disabled, which is precisely the bug it
	// exists to catch. Forty short walks allocate little enough to spare.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	// Descriptor numbers are assigned lowest-available, so the number a fresh
	// open receives is a direct reading of how many are already held. Counting
	// /dev/fd would be more literal and is not portable enough to rely on.
	nextFD := func() int {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Skipf("cannot open %s to probe descriptors: %v", os.DevNull, err)
		}
		n := int(f.Fd())
		if err := f.Close(); err != nil {
			t.Fatalf("close probe: %v", err)
		}
		return n
	}

	// One warm-up walk first: the pack index and the object cache are built
	// lazily and their descriptors are not what this test is about.
	if _, err := r.RecentCommits(ctx, 5); err != nil {
		t.Fatalf("warm-up walk: %v", err)
	}
	before := nextFD()
	const walks = 40
	for i := 0; i < walks; i++ {
		if _, err := r.RecentCommits(ctx, 5); err != nil {
			t.Fatalf("walk %d: %v", i, err)
		}
	}
	// A small allowance for descriptors the runtime opens on its own during the
	// loop. Forty leaked packfiles would be unmistakable against it — and are
	// what this test sees when objectView.close is disabled.
	if after := nextFD(); after > before+8 {
		t.Errorf("a fresh descriptor numbered %d before %d walks and %d after; the view is leaking",
			before, walks, after)
	}
}

// TestObjectViewRefusesARepositoryWithNoObjectStore.
//
// Open records a repository go-git declined to open fully as degraded: IsRepo
// is true and the go-git handle is nil, because HEAD was read from the files
// directly. The object store is exactly what is missing there. Every walk must
// say ErrNotRepository — which callers already render as "no history was read"
// — rather than dereference the handle it does not have.
func TestObjectViewRefusesARepositoryWithNoObjectStore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o750); err != nil {
		t.Fatal(err)
	}
	const sha = "a2dbf38767bd469088e94ee97a761b92deddefc8"
	for rel, content := range map[string]string{
		// An extension go-git does not implement, exactly as `git worktree add`
		// leaves it.
		"config":         "[core]\n\trepositoryformatversion = 1\n[extensions]\n\tworktreeConfig = true\n",
		"HEAD":           "ref: refs/heads/dev\n",
		"refs/heads/dev": sha + "\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	repo, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if repo.Degraded() == "" {
		t.Fatal("this fixture is meant to open in degraded mode and did not")
	}

	ctx := context.Background()
	for name, call := range map[string]func() error{
		"RecentCommits":     func() error { _, err := repo.RecentCommits(ctx, 10); return err },
		"RecentCommitsMeta": func() error { _, err := repo.RecentCommitsMeta(ctx, 10); return err },
		"FileLastModified":  func() error { _, err := repo.FileLastModified(ctx, "a.go"); return err },
		"ChangedSince":      func() error { _, err := repo.ChangedSince(ctx, "", "HEAD"); return err },
	} {
		if err := call(); !errors.Is(err, ErrNotRepository) {
			t.Errorf("%s on a degraded repository returned %v, want ErrNotRepository", name, err)
		}
	}
}

// TestPooledChangedSinceMatchesUnpooled: the incremental indexer's diff runs
// through the same view, and a rename it stopped detecting would make the
// indexer discard every edge pointing into the moved file.
func TestPooledChangedSinceMatchesUnpooled(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.write("pkg/b.go", filler(2))
	f.commit("initial import")
	base := "HEAD"

	f.rename("pkg/b.go", "pkg/renamed.go")
	f.write("pkg/new.go", filler(3))
	f.remove("a.go")
	f.commit("rename, add and delete")

	r := f.open()
	got, err := r.ChangedSince(context.Background(), base+"~1", "HEAD")
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}

	want := map[string]ChangeKind{
		"pkg/renamed.go": ChangeRenamed,
		"pkg/new.go":     ChangeAdded,
		"a.go":           ChangeDeleted,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d change(s) %+v, want %d", len(got), got, len(want))
	}
	for _, c := range got {
		kind, known := want[c.Path]
		if !known || kind != c.Kind {
			t.Errorf("change %+v is not one of %v", c, want)
		}
		if c.Kind == ChangeRenamed && c.OldPath != "pkg/b.go" {
			t.Errorf("rename lost its old path: %+v", c)
		}
	}
}

// TestFileLastModifiedThroughTheView pins the third walk the view carries.
func TestFileLastModifiedThroughTheView(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.write("b.go", filler(2))
	f.commit("initial import")
	f.write("a.go", filler(1)+"// edit\n")
	want := f.commit("edit a")
	f.write("b.go", filler(2)+"// edit\n")
	f.commit("edit b")

	r := f.open()
	rev, err := r.FileLastModified(context.Background(), "a.go")
	if err != nil {
		t.Fatalf("FileLastModified: %v", err)
	}
	if rev.SHA != want.String() {
		t.Errorf("a.go last changed at %s, want %s", rev.SHA, want)
	}
}
