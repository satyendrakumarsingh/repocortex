package git

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestOpenNonRepositoryIsNotAnError(t *testing.T) {
	t.Parallel()

	// The core promise: RepoCortex works on plain directories. If this ever
	// regresses, `repocortex index` on an extracted tarball starts failing.
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on a non-repository returned an error: %v", err)
	}
	if r.IsRepo() {
		t.Fatal("IsRepo() = true for a directory with no .git")
	}

	info, err := r.Info(context.Background())
	if err != nil {
		t.Fatalf("Info on a non-repository returned an error: %v", err)
	}
	if info.IsRepo {
		t.Fatalf("Info.IsRepo = true, want false (info=%+v)", info)
	}
	if info.HeadSHA != "" || info.Branch != "" || info.Dirty {
		t.Fatalf("non-repository Info should be zero apart from Root, got %+v", info)
	}

	// The history methods do report an error, but a typed one.
	if _, err := r.ChangedSince(context.Background(), "a", "b"); !errors.Is(err, ErrNotRepository) {
		t.Errorf("ChangedSince error = %v, want ErrNotRepository", err)
	}
	if _, err := r.RecentCommits(context.Background(), 10); !errors.Is(err, ErrNotRepository) {
		t.Errorf("RecentCommits error = %v, want ErrNotRepository", err)
	}
	if _, err := r.Status(context.Background()); !errors.Is(err, ErrNotRepository) {
		t.Errorf("Status error = %v, want ErrNotRepository", err)
	}
	if _, err := r.FileLastModified(context.Background(), "x.go"); !errors.Is(err, ErrNotRepository) {
		t.Errorf("FileLastModified error = %v, want ErrNotRepository", err)
	}
}

func TestInfoHeadAndBranch(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", "package a\n")
	want := f.commit("first")

	info, err := f.open().Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.IsRepo {
		t.Fatal("IsRepo = false for a real repository")
	}
	if info.HeadSHA != want.String() {
		t.Errorf("HeadSHA = %q, want %q", info.HeadSHA, want.String())
	}
	if len(info.HeadSHA) != 40 {
		t.Errorf("HeadSHA is %d chars, want the full 40-char revision", len(info.HeadSHA))
	}
	if info.Branch != fixtureBranch {
		t.Errorf("Branch = %q, want %q", info.Branch, fixtureBranch)
	}
	if info.Detached {
		t.Error("Detached = true on a branch checkout")
	}
	if info.Root != f.dir {
		t.Errorf("Root = %q, want %q", info.Root, f.dir)
	}
}

func TestHeadSHAMatchesInfo(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", "package a\n")
	want := f.commit("first")
	r := f.open()

	sha, err := r.HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if sha != want.String() {
		t.Errorf("HeadSHA = %q, want %q", sha, want.String())
	}

	// Must agree with Info's HeadSHA — HeadSHA is a cheaper path to the same
	// value, not a different one.
	info, err := r.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if sha != info.HeadSHA {
		t.Errorf("HeadSHA() = %q, Info().HeadSHA = %q — must match", sha, info.HeadSHA)
	}
}

func TestHeadSHANonRepositoryIsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open on a non-repository returned an error: %v", err)
	}

	sha, err := r.HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA on a non-repository returned an error: %v", err)
	}
	if sha != "" {
		t.Errorf("HeadSHA = %q, want empty for a non-repository", sha)
	}
}

func TestHeadSHAEmptyRepositoryIsEmpty(t *testing.T) {
	t.Parallel()

	// `repocortex init` on a freshly `git init`ed project hits exactly this.
	f := newFixture(t)
	r := f.open()

	sha, err := r.HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA on an empty repository: %v", err)
	}
	if sha != "" {
		t.Errorf("HeadSHA = %q, want empty for a repository with no commits", sha)
	}
}

func TestHeadSHARespectsCancellation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", "package a\n")
	f.commit("first")
	r := f.open()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.HeadSHA(ctx); err == nil {
		t.Fatal("HeadSHA with a cancelled context returned no error")
	}
}

func TestInfoDirtyAndClean(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", "package a\n")
	f.commit("first")
	r := f.open()

	info, err := r.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Dirty {
		t.Fatal("Dirty = true immediately after committing everything")
	}

	// An unstaged edit must flip it: an index stamped with a clean revision
	// that was actually built from edited files is a silent correctness bug.
	f.writeOnly("a.go", "package a\n\nfunc F() {}\n")
	info, err = r.Info(context.Background())
	if err != nil {
		t.Fatalf("Info after edit: %v", err)
	}
	if !info.Dirty {
		t.Fatal("Dirty = false after an unstaged edit")
	}

	// So must a brand-new untracked file.
	f2 := newFixture(t)
	f2.write("a.go", "package a\n")
	f2.commit("first")
	f2.writeOnly("new.go", "package a\n")
	info2, err := f2.open().Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info2.Dirty {
		t.Fatal("Dirty = false with an untracked file present")
	}
}

func TestEmptyRepositoryDoesNotCrash(t *testing.T) {
	t.Parallel()

	// `repocortex init` on a freshly `git init`ed project hits exactly this.
	f := newFixture(t)
	r := f.open()
	ctx := context.Background()

	info, err := r.Info(ctx)
	if err != nil {
		t.Fatalf("Info on an empty repository: %v", err)
	}
	if !info.IsRepo {
		t.Error("IsRepo = false for an initialised repository with no commits")
	}
	if info.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty for a repository with no commits", info.HeadSHA)
	}
	if info.Branch != fixtureBranch {
		t.Errorf("Branch = %q, want %q — HEAD is a symbolic ref before it is a hash",
			info.Branch, fixtureBranch)
	}
	if info.Detached {
		t.Error("Detached = true on an empty repository; it is on a branch, just an unborn one")
	}

	commits, err := r.RecentCommits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCommits on an empty repository: %v", err)
	}
	if len(commits) != 0 {
		t.Errorf("RecentCommits returned %d commits from an empty repository", len(commits))
	}

	if _, err := r.FileLastModified(ctx, "a.go"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("FileLastModified error = %v, want ErrNoHistory", err)
	}

	if _, err := r.Status(ctx); err != nil {
		t.Fatalf("Status on an empty repository: %v", err)
	}
}

func TestChangedSinceDetectsEveryKind(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("keep.go", filler(1))
	f.write("edit.go", filler(2))
	f.write("gone.go", filler(3))
	f.write("old/name.go", filler(4))
	base := f.commit("base")

	f.write("added.go", filler(5))
	f.write("edit.go", filler(2)+"// appended\n")
	f.remove("gone.go")
	f.rename("old/name.go", "new/name.go")
	head := f.commit("everything")

	changes, err := f.open().ChangedSince(context.Background(), base.String(), head.String())
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}

	got := make(map[string]Change, len(changes))
	for _, c := range changes {
		got[c.Path] = c
	}

	want := []Change{
		{Path: "added.go", Kind: ChangeAdded},
		{Path: "edit.go", Kind: ChangeModified},
		{Path: "gone.go", Kind: ChangeDeleted},
		{Path: "new/name.go", OldPath: "old/name.go", Kind: ChangeRenamed},
	}
	for _, w := range want {
		g, ok := got[w.Path]
		if !ok {
			t.Errorf("no change reported for %q (got %+v)", w.Path, changes)
			continue
		}
		if g != w {
			t.Errorf("change for %q = %+v, want %+v", w.Path, g, w)
		}
	}
	if _, ok := got["keep.go"]; ok {
		t.Error("unchanged file keep.go was reported as changed")
	}
	if len(changes) != len(want) {
		t.Errorf("got %d changes, want %d: %+v", len(changes), len(want), changes)
	}

	// The rename must not additionally surface as a delete+add pair, or the
	// indexer would drop the symbols it was supposed to move.
	if _, ok := got["old/name.go"]; ok {
		t.Error("renamed source path also reported as its own change")
	}
}

func TestChangedSinceEmptyFromMeansEverythingAdded(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.write("pkg/b.go", filler(2))
	f.commit("first")

	// This is the shape of a first-ever index run.
	changes, err := f.open().ChangedSince(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ChangedSince(\"\", \"\"): %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2: %+v", len(changes), changes)
	}
	for _, c := range changes {
		if c.Kind != ChangeAdded {
			t.Errorf("%s: kind = %q, want added", c.Path, c.Kind)
		}
		if strings.Contains(c.Path, "\\") {
			t.Errorf("path %q contains a backslash", c.Path)
		}
	}
	if changes[0].Path != "a.go" || changes[1].Path != "pkg/b.go" {
		t.Errorf("results not sorted by path: %+v", changes)
	}
}

func TestChangedSinceAcceptsSymbolicRevisions(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.commit("first")
	f.write("b.go", filler(2))
	f.commit("second")

	changes, err := f.open().ChangedSince(context.Background(), "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("ChangedSince(HEAD~1, HEAD): %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "b.go" || changes[0].Kind != ChangeAdded {
		t.Fatalf("got %+v, want a single added b.go", changes)
	}
}

func TestChangedSinceRejectsUnknownRevision(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.commit("first")

	if _, err := f.open().ChangedSince(context.Background(), "does-not-exist", "HEAD"); err == nil {
		t.Fatal("ChangedSince accepted a revision that does not exist")
	}
}

func TestRecentCommitsRespectsLimit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	var hashes []string
	for i := 0; i < 10; i++ {
		f.write("a.go", filler(i))
		hashes = append(hashes, f.commit("commit "+string(rune('a'+i))+"\n\nbody text ignored").String())
	}

	r := f.open()
	commits, err := r.RecentCommits(context.Background(), 3)
	if err != nil {
		t.Fatalf("RecentCommits: %v", err)
	}
	if len(commits) != 3 {
		t.Fatalf("got %d commits, want 3", len(commits))
	}

	// Newest first.
	if commits[0].SHA != hashes[len(hashes)-1] {
		t.Errorf("first commit = %s, want the newest %s", commits[0].SHA, hashes[len(hashes)-1])
	}
	for i := 1; i < len(commits); i++ {
		if commits[i].When > commits[i-1].When {
			t.Errorf("commits not newest-first at index %d", i)
		}
	}

	c := commits[0]
	if c.AuthorName != "Test Author" || c.AuthorEmail != "author@example.com" {
		t.Errorf("author = %q <%s>, want the fixture signature", c.AuthorName, c.AuthorEmail)
	}
	if strings.Contains(c.Subject, "\n") || strings.Contains(c.Subject, "body text") {
		t.Errorf("Subject = %q, want only the first line", c.Subject)
	}
	if len(c.Paths) != 1 || c.Paths[0] != "a.go" {
		t.Errorf("Paths = %v, want [a.go]", c.Paths)
	}

	// A limit larger than the history returns the whole history, not an error.
	all, err := r.RecentCommits(context.Background(), 1000)
	if err != nil {
		t.Fatalf("RecentCommits(1000): %v", err)
	}
	if len(all) != 10 {
		t.Errorf("got %d commits, want all 10", len(all))
	}
}

func TestRecentCommitsRefusesUnboundedWalk(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.commit("first")
	r := f.open()

	for _, limit := range []int{0, -1} {
		if _, err := r.RecentCommits(context.Background(), limit); !errors.Is(err, ErrUnboundedWalk) {
			t.Errorf("RecentCommits(%d) error = %v, want ErrUnboundedWalk", limit, err)
		}
	}
}

func TestRecentCommitsPathsCoverRenamesAndDeletes(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("old.go", filler(1))
	f.write("doomed.go", filler(2))
	f.commit("base")
	f.rename("old.go", "new.go")
	f.remove("doomed.go")
	f.commit("shuffle")

	commits, err := f.open().RecentCommits(context.Background(), 1)
	if err != nil {
		t.Fatalf("RecentCommits: %v", err)
	}
	want := []string{"doomed.go", "new.go", "old.go"}
	if len(commits[0].Paths) != len(want) {
		t.Fatalf("Paths = %v, want %v", commits[0].Paths, want)
	}
	for i, w := range want {
		if commits[0].Paths[i] != w {
			t.Fatalf("Paths = %v, want %v", commits[0].Paths, want)
		}
	}

	meta, err := f.open().RecentCommitsMeta(context.Background(), 1)
	if err != nil {
		t.Fatalf("RecentCommitsMeta: %v", err)
	}
	if meta[0].Paths != nil {
		t.Errorf("RecentCommitsMeta filled Paths = %v, want nil", meta[0].Paths)
	}
	if meta[0].SHA != commits[0].SHA {
		t.Error("RecentCommitsMeta and RecentCommits disagree on the newest commit")
	}
}

func TestContextCancellationAbortsWalks(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	for i := 0; i < 20; i++ {
		f.write("a.go", filler(i))
		f.commit("c")
	}
	r := f.open()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.RecentCommits(ctx, 20); !errors.Is(err, context.Canceled) {
		t.Errorf("RecentCommits error = %v, want context.Canceled", err)
	}
	if _, err := r.RecentCommitsMeta(ctx, 20); !errors.Is(err, context.Canceled) {
		t.Errorf("RecentCommitsMeta error = %v, want context.Canceled", err)
	}
	// go-git converts cancellation into its own sentinel; ChangedSince must
	// translate it back or callers cannot tell shutdown from failure.
	if _, err := r.ChangedSince(ctx, "HEAD~5", "HEAD"); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedSince error = %v, want context.Canceled", err)
	}
	if _, err := r.FileLastModified(ctx, "a.go"); !errors.Is(err, context.Canceled) {
		t.Errorf("FileLastModified error = %v, want context.Canceled", err)
	}
	if _, err := r.Info(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Info error = %v, want context.Canceled", err)
	}
}

// countdownCtx reports "not cancelled" for the first n checks and cancelled
// thereafter.
//
// A wall-clock timeout would make this test a race against the machine. This
// makes "the walk re-checks cancellation on every commit, not just at entry"
// a deterministic assertion: with a budget far below the requested limit, a
// loop that only checked at entry would run to completion and return commits.
type countdownCtx struct {
	context.Context
	mu   sync.Mutex
	left int
	done chan struct{}
	once sync.Once
}

func newCountdownCtx(n int) *countdownCtx {
	return &countdownCtx{Context: context.Background(), left: n, done: make(chan struct{})}
}

func (c *countdownCtx) Done() <-chan struct{} { return c.done }

func (c *countdownCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.left > 0 {
		c.left--
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

func TestWalkChecksCancellationEveryCommit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	for i := 0; i < 60; i++ {
		f.write("a.go", filler(i))
		f.commit("c")
	}
	r := f.open()

	// Sanity: uncancelled, the same call returns the full 50.
	all, err := r.RecentCommitsMeta(context.Background(), 50)
	if err != nil {
		t.Fatalf("RecentCommitsMeta: %v", err)
	}
	if len(all) != 50 {
		t.Fatalf("got %d commits, want 50 — the assertion below would be vacuous", len(all))
	}

	if _, err := r.RecentCommitsMeta(newCountdownCtx(5), 50); !errors.Is(err, context.Canceled) {
		t.Errorf("RecentCommitsMeta error = %v, want context.Canceled after 5 iterations", err)
	}
	if _, err := r.FileLastModified(newCountdownCtx(3), "never/existed.go"); !errors.Is(err, context.Canceled) {
		t.Errorf("FileLastModified error = %v, want context.Canceled after 3 iterations", err)
	}
}

func TestStatusGroupsPaths(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("tracked.go", filler(1))
	f.write("staged.go", filler(2))
	f.write("removed.go", filler(3))
	f.commit("base")

	f.writeOnly("tracked.go", filler(1)+"// edited\n") // modified, unstaged
	f.write("staged.go", filler(2)+"// staged\n")      // staged
	f.writeOnly("untracked.go", filler(4))             // untracked
	if err := removeFile(f.dir, "removed.go"); err != nil {
		t.Fatalf("delete removed.go: %v", err)
	}

	st, err := f.open().Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	assertContains(t, "Modified", st.Modified, "tracked.go")
	assertContains(t, "Staged", st.Staged, "staged.go")
	assertContains(t, "Untracked", st.Untracked, "untracked.go")
	assertContains(t, "Deleted", st.Deleted, "removed.go")
}

func TestFileLastModified(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a.go", filler(1))
	f.write("stable.go", filler(2))
	first := f.commit("first")

	f.write("a.go", filler(1)+"// change\n")
	second := f.commit("second")

	f.write("c.go", filler(3))
	f.commit("third")

	r := f.open()
	ctx := context.Background()

	rev, err := r.FileLastModified(ctx, "a.go")
	if err != nil {
		t.Fatalf("FileLastModified(a.go): %v", err)
	}
	if rev.SHA != second.String() {
		t.Errorf("a.go last modified in %s, want %s", rev.SHA, second)
	}
	if rev.When <= 0 {
		t.Errorf("When = %d, want a positive Unix timestamp", rev.When)
	}

	// A file touched only by the root commit still resolves.
	rev, err = r.FileLastModified(ctx, "stable.go")
	if err != nil {
		t.Fatalf("FileLastModified(stable.go): %v", err)
	}
	if rev.SHA != first.String() {
		t.Errorf("stable.go last modified in %s, want %s", rev.SHA, first)
	}

	// Nested paths, given with forward slashes on every platform.
	f.write("pkg/sub/d.go", filler(4))
	nested := f.commit("nested")
	rev, err = f.open().FileLastModified(ctx, "pkg/sub/d.go")
	if err != nil {
		t.Fatalf("FileLastModified(pkg/sub/d.go): %v", err)
	}
	if rev.SHA != nested.String() {
		t.Errorf("pkg/sub/d.go last modified in %s, want %s", rev.SHA, nested)
	}

	if _, err := f.open().FileLastModified(ctx, "never/existed.go"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("FileLastModified on an unknown path: err = %v, want ErrNoHistory", err)
	}
}

func TestFileLastModifiedReportsDeletion(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("gone.go", filler(1))
	f.write("other.go", filler(2))
	f.commit("first")
	f.remove("gone.go")
	deleted := f.commit("delete gone.go")
	f.write("other.go", filler(2)+"// more\n")
	f.commit("later")

	rev, err := f.open().FileLastModified(context.Background(), "gone.go")
	if err != nil {
		t.Fatalf("FileLastModified on a deleted path: %v", err)
	}
	if rev.SHA != deleted.String() {
		t.Errorf("got %s, want the deleting commit %s", rev.SHA, deleted)
	}
}

func TestPathsAreRepoRelativeWithForwardSlashes(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("a/b/c/deep.go", filler(1))
	f.commit("first")

	r := f.open()
	ctx := context.Background()

	changes, err := r.ChangedSince(ctx, "", "HEAD")
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	commits, err := r.RecentCommits(ctx, 1)
	if err != nil {
		t.Fatalf("RecentCommits: %v", err)
	}
	f.writeOnly("a/b/untracked.go", filler(2))
	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	var all []string
	for _, c := range changes {
		all = append(all, c.Path)
	}
	all = append(all, commits[0].Paths...)
	all = append(all, st.Untracked...)

	if len(all) == 0 {
		t.Fatal("no paths collected; the assertion below would be vacuous")
	}
	for _, p := range all {
		if strings.Contains(p, "\\") {
			t.Errorf("path %q contains a backslash", p)
		}
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") {
			t.Errorf("path %q is not repo-relative", p)
		}
		if strings.Contains(p, f.dir) {
			t.Errorf("path %q is absolute", p)
		}
	}
	if all[0] != "a/b/c/deep.go" {
		t.Errorf("path = %q, want a/b/c/deep.go", all[0])
	}
}

func TestOpenFromSubdirectoryFindsRepositoryRoot(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.write("pkg/sub/a.go", filler(1))
	f.commit("first")

	r, err := Open(f.dir + "/pkg/sub")
	if err != nil {
		t.Fatalf("Open subdirectory: %v", err)
	}
	if !r.IsRepo() {
		t.Fatal("IsRepo = false when opened from a subdirectory")
	}
	if r.Root() != f.dir {
		t.Errorf("Root = %q, want the repository root %q", r.Root(), f.dir)
	}

	// Paths must stay relative to the root, not to the opened directory.
	changes, err := r.ChangedSince(context.Background(), "", "HEAD")
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "pkg/sub/a.go" {
		t.Errorf("got %+v, want a single pkg/sub/a.go", changes)
	}
}

func TestSubjectTrimsBody(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"subject":                        "subject",
		"subject\n\nbody\nmore":          "subject",
		"  padded  \n\nbody":             "padded",
		"":                               "",
		"\n\nonly a body somehow":        "",
		"trailing whitespace   \n\nbody": "trailing whitespace",
	}
	for in, want := range cases {
		if got := subject(in); got != want {
			t.Errorf("subject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormPath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"a/b.go":   "a/b.go",
		"./a/b.go": "a/b.go",
		"/a/b.go":  "a/b.go",
		"":         "",
	}
	for in, want := range cases {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func assertContains(t *testing.T, group string, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Errorf("%s = %v, want it to contain %q", group, got, want)
}
