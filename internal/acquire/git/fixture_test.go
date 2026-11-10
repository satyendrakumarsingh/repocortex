package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The fixtures build real repositories with go-git rather than by shelling out
// to `git`. CI runs on machines where the git binary's presence and version are
// not guaranteed, and a test suite that depends on a tool the product itself
// refuses to depend on would be testing a different program than the one we
// ship.

const fixtureBranch = "main"

type fixture struct {
	t    testing.TB
	dir  string
	repo *gogit.Repository
	wt   *gogit.Worktree
	seq  int
}

// newFixture creates an initialised repository with no commits.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	return initRepo(t, t.TempDir())
}

func initRepo(t testing.TB, dir string) *fixture {
	t.Helper()

	// On macOS t.TempDir() sits under /var, a symlink to /private/var. go-git
	// reports the resolved worktree root, so resolve here too or every Root()
	// comparison in these tests fails for a reason that has nothing to do with
	// git.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.NewBranchReferenceName(fixtureBranch),
		},
	})
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	return &fixture{t: t, dir: dir, repo: repo, wt: wt}
}

// write creates or overwrites a file and stages it.
func (f *fixture) write(rel, content string) {
	f.t.Helper()
	abs := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", rel, err)
	}
	if _, err := f.wt.Add(rel); err != nil {
		f.t.Fatalf("stage %s: %v", rel, err)
	}
}

// writeOnly creates a file without staging it, producing an untracked entry.
func (f *fixture) writeOnly(rel, content string) {
	f.t.Helper()
	abs := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", rel, err)
	}
}

// remove deletes a tracked file and stages the deletion.
func (f *fixture) remove(rel string) {
	f.t.Helper()
	if _, err := f.wt.Remove(rel); err != nil {
		f.t.Fatalf("remove %s: %v", rel, err)
	}
}

// rename moves a tracked file and stages both halves. The content is unchanged,
// so the blob hash is identical and rename detection must find it.
func (f *fixture) rename(oldRel, newRel string) {
	f.t.Helper()
	if _, err := f.wt.Move(oldRel, newRel); err != nil {
		f.t.Fatalf("move %s -> %s: %v", oldRel, newRel, err)
	}
}

// commit records the staged changes with a deterministic signature. Timestamps
// increase by one second per commit so ordering assertions are stable and do
// not depend on how fast the test machine is.
func (f *fixture) commit(message string) plumbing.Hash {
	f.t.Helper()
	f.seq++
	sig := &object.Signature{
		Name:  "Test Author",
		Email: "author@example.com",
		When:  time.Unix(1700000000+int64(f.seq), 0).UTC(),
	}
	hash, err := f.wt.Commit(message, &gogit.CommitOptions{
		Author:            sig,
		Committer:         sig,
		AllowEmptyCommits: true,
	})
	if err != nil {
		f.t.Fatalf("commit %q: %v", message, err)
	}
	return hash
}

// open returns a Repo for this fixture.
func (f *fixture) open() *Repo {
	f.t.Helper()
	r, err := Open(f.dir)
	if err != nil {
		f.t.Fatalf("Open(%s): %v", f.dir, err)
	}
	return r
}

// removeFile deletes a file from disk without touching the index, producing a
// worktree deletion rather than a staged one.
func removeFile(dir, rel string) error {
	return os.Remove(filepath.Join(dir, filepath.FromSlash(rel)))
}

// filler produces file content long enough that go-git's rename detection has
// something to compare, and distinct per seed.
func filler(seed int) string {
	var b []byte
	for i := 0; i < 40; i++ {
		b = append(b, fmt.Sprintf("line %d of file %d\n", i, seed)...)
	}
	return string(b)
}
