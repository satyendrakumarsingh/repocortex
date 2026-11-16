package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// declineInGoGit turns on the repository extension go-git refuses, exactly as
// `git worktree add` leaves it, so Open falls back to degraded mode.
func declineInGoGit(t *testing.T, root string) {
	t.Helper()
	cfg := filepath.Join(root, ".git", "config")
	raw, err := os.ReadFile(cfg) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n[core]\n\trepositoryformatversion = 1\n[extensions]\n\tworktreeConfig = true\n")...)
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryIsReadWhenGoGitRefusesTheRepository pins the fix for an empty
// history on repositories with linked worktrees: hotspots, bus factor, owners
// and co-change all read RecentCommits, and on such a repository they used to
// report "No history was read".
func TestHistoryIsReadWhenGoGitRefusesTheRepository(t *testing.T) {
	if gitBinary() == "" {
		t.Skip("no git executable on PATH")
	}
	f := newFixture(t)
	f.write("a.go", "package a\n")
	first := f.commit("add a\n\nFIX: keep the zero value usable")
	f.write("b.go", "package a\n")
	f.write("a.go", "package a\n\nvar X = 1\n")
	second := f.commit("touch a, add b")

	declineInGoGit(t, f.dir)
	repo, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Degraded() == "" {
		t.Fatal("expected degraded mode")
	}
	if !repo.HistoryAvailable() || repo.HistorySource() != "git binary" {
		t.Fatalf("HistoryAvailable=%v source=%q, want true / git binary", repo.HistoryAvailable(), repo.HistorySource())
	}

	commits, err := repo.RecentCommits(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentCommits: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
	if commits[0].SHA != second.String() || commits[1].SHA != first.String() {
		t.Errorf("order = %s, %s; want newest first", commits[0].SHA[:7], commits[1].SHA[:7])
	}
	if !reflect.DeepEqual(commits[0].Paths, []string{"a.go", "b.go"}) {
		t.Errorf("paths of the second commit = %v, want [a.go b.go]", commits[0].Paths)
	}
	if !reflect.DeepEqual(commits[1].Paths, []string{"a.go"}) {
		t.Errorf("paths of the first commit = %v, want [a.go]", commits[1].Paths)
	}
	if commits[0].Subject != "touch a, add b" || commits[0].AuthorName == "" || commits[0].When == 0 {
		t.Errorf("metadata not read: %+v", commits[0])
	}
	if len(commits[1].Constraints) != 1 {
		t.Errorf("constraints = %v, want the FIX line from the body", commits[1].Constraints)
	}

	meta, err := repo.RecentCommitsMeta(context.Background(), 10)
	if err != nil || len(meta) != 2 || meta[0].Paths != nil {
		t.Errorf("RecentCommitsMeta = %d commits, err %v, paths %v; want 2, nil, nil", len(meta), err, meta[0].Paths)
	}

	rev, err := repo.FileLastModified(context.Background(), "a.go")
	if err != nil || rev.SHA != second.String() {
		t.Errorf("FileLastModified(a.go) = %s, %v; want %s", rev.SHA, err, second.String()[:7])
	}
	if _, err := repo.FileLastModified(context.Background(), "never.go"); !errors.Is(err, ErrNoHistory) {
		t.Errorf("FileLastModified(never.go) err = %v, want ErrNoHistory", err)
	}
}

// Without a git binary, degraded mode still reports history as unavailable
// rather than pretending.
func TestHistoryUnavailableWithoutGitBinary(t *testing.T) {
	f := newFixture(t)
	f.write("a.go", "package a\n")
	f.commit("add a")
	declineInGoGit(t, f.dir)

	saved := gitBinary
	gitBinary = func() string { return "" }
	t.Cleanup(func() { gitBinary = saved })

	repo, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.HistoryAvailable() {
		t.Error("HistoryAvailable() = true with no git binary and go-git declining the repository")
	}
}
