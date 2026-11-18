package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Hooks installed from a linked worktree go where Git reads them (the shared
// hooks directory), status and uninstall agree, and a commit in the worktree
// re-indexes the worktree, not the main checkout.
func TestHooksInstalledFromLinkedWorktreeRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "main")
	wt := filepath.Join(base, "wt")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "init", "-q")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, main, "add", ".")
	gitIn(t, main, "commit", "-q", "-m", "init")
	gitIn(t, main, "worktree", "add", "-q", wt, "-b", "wt")

	// A stand-in binary that records the project it was asked to index.
	record := filepath.Join(base, "record.txt")
	bin := filepath.Join(base, "fakebin.sh")
	script := "#!/bin/sh\necho \"$@\" >> " + shellQuote(record) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := InstallCommitHook(wt, bin); err != nil {
		t.Fatal(err)
	}

	gitHooks := gitIn(t, wt, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if _, err := os.Stat(filepath.Join(gitHooks, "post-commit")); err != nil {
		t.Fatalf("hook is not where git reads hooks (%s): %v", gitHooks, err)
	}
	if _, err := os.Stat(filepath.Join(main, ".git", "worktrees", "wt", "hooks")); err == nil {
		t.Error("hooks were written into the worktree's private git directory, which git never reads")
	}
	if st := AllHooksStatus(wt); !st.PostCommit.Installed || !st.PostMerge.Installed || !st.PostCheckout.Installed {
		t.Errorf("status disagrees with install: %+v", st)
	}

	// A commit in the worktree runs the hook for the worktree.
	if err := os.WriteFile(filepath.Join(wt, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-q", "-m", "second")
	var got string
	for i := 0; i < 100 && got == ""; i++ {
		if b, err := os.ReadFile(record); err == nil {
			got = string(b)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !strings.Contains(got, "--project "+wt) {
		t.Errorf("the hook did not target the worktree %s; recorded %q", wt, got)
	}

	removed, err := UninstallCommitHook(wt)
	if err != nil || !removed {
		t.Fatalf("uninstall from the worktree: removed=%v err=%v", removed, err)
	}
	if st := AllHooksStatus(main); st.PostCommit.Installed {
		t.Error("hooks still installed after uninstall")
	}
}
