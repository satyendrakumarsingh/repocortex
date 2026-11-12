package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestHeadIsReadableWhenGoGitRefusesTheRepository pins a silent feature loss.
//
// go-git rejects a repository whose core.repositoryformatversion declares an
// extension it does not implement. `extensions.worktreeConfig` is the common
// one — git enables it the first time you run `git worktree add` — and on a
// real repository that made `repocortex index` record no revision at all, while
// `git rev-parse HEAD` answered instantly. The caller treated every Open error
// as "not a repository", so the loss was invisible.
//
// The revision is provenance: it answers which commit an index describes.
// Worth reading from the files when a library declines the container.
func TestHeadIsReadableWhenGoGitRefusesTheRepository(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o750); err != nil {
		t.Fatal(err)
	}
	const sha = "a2dbf38767bd469088e94ee97a761b92deddefc8"
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(gitDir, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// An extension go-git does not implement, exactly as `git worktree add`
	// leaves it.
	write("config", "[core]\n\trepositoryformatversion = 1\n[extensions]\n\tworktreeConfig = true\n")
	write("HEAD", "ref: refs/heads/dev\n")
	write("refs/heads/dev", sha+"\n")

	repo, err := Open(root)
	if err != nil {
		t.Fatalf("Open returned an error instead of degrading: %v", err)
	}
	if repo.Degraded() == "" {
		t.Error("Degraded() is empty; the caller cannot tell history is unavailable")
	}
	info, err := repo.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !info.IsRepo {
		t.Fatal("IsRepo is false for a directory that plainly is one")
	}
	if info.HeadSHA != sha {
		t.Errorf("HeadSHA = %q, want %q", info.HeadSHA, sha)
	}
}

// TestHeadResolvesFromPackedRefs covers the same path once git has packed the
// loose ref away, which it does routinely.
func TestHeadResolvesFromPackedRefs(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	const sha = "1111111111111111111111111111111111111111"
	for rel, content := range map[string]string{
		"config":      "[core]\n\trepositoryformatversion = 1\n[extensions]\n\tworktreeConfig = true\n",
		"HEAD":        "ref: refs/heads/main\n",
		"packed-refs": "# pack-refs with: peeled fully-peeled sorted\n" + sha + " refs/heads/main\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, rel), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	info, err := repo.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.HeadSHA != sha {
		t.Errorf("HeadSHA = %q, want %q from packed-refs", info.HeadSHA, sha)
	}
}
