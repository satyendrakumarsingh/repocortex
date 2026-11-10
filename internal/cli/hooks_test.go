package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a directory that looks enough like a Git repository for the
// hook installer, without shelling out to git.
func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatalf("create .git/hooks: %v", err)
	}
	return root
}

func readHook(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".git", "hooks", "post-commit"))
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	return string(data)
}

func TestInstallCommitHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing string // "" means no file
		want     func(t *testing.T, st HookState, body string)
	}{
		{
			name:     "creates the hook when none exists",
			existing: "",
			want: func(t *testing.T, st HookState, body string) {
				if !st.Installed || st.Appended || st.AlreadyCurrent {
					t.Errorf("state = %+v, want a fresh install", st)
				}
				if !strings.HasPrefix(body, "#!/bin/sh\n") {
					t.Errorf("hook lacks a shebang:\n%s", body)
				}
				if !strings.Contains(body, hookBegin) || !strings.Contains(body, hookEnd) {
					t.Error("managed markers missing")
				}
			},
		},
		{
			name:     "appends to an existing hook without destroying it",
			existing: "#!/bin/sh\necho \"someone else's hook\"\n",
			want: func(t *testing.T, st HookState, body string) {
				if !st.Appended {
					t.Errorf("state = %+v, want Appended", st)
				}
				if !strings.Contains(body, "someone else's hook") {
					t.Fatalf("the pre-existing hook was destroyed:\n%s", body)
				}
				if strings.Index(body, "someone else's hook") > strings.Index(body, hookBegin) {
					t.Error("RepoCortex's block was inserted before the existing content")
				}
			},
		},
		{
			name:     "is idempotent — installing twice leaves one block",
			existing: "",
			want:     func(t *testing.T, _ HookState, _ string) { /* checked below */ },
		},
		{
			name:     "replaces a stale managed block rather than stacking a second",
			existing: "#!/bin/sh\nkeep-me\n" + hookBegin + "\nold-and-wrong\n" + hookEnd + "\n",
			want: func(t *testing.T, st HookState, body string) {
				if got := strings.Count(body, hookBegin); got != 1 {
					t.Errorf("hookBegin appears %d times, want 1:\n%s", got, body)
				}
				if strings.Contains(body, "old-and-wrong") {
					t.Error("the stale block survived")
				}
				if !strings.Contains(body, "keep-me") {
					t.Error("unmanaged content was lost")
				}
				_ = st
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newRepo(t)
			path := filepath.Join(root, ".git", "hooks", "post-commit")
			if tc.existing != "" {
				if err := os.WriteFile(path, []byte(tc.existing), 0o755); err != nil {
					t.Fatalf("seed hook: %v", err)
				}
			}
			st, err := InstallCommitHook(root, "/usr/local/bin/cortex")
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			tc.want(t, st, readHook(t, root))

			// Every case must also be idempotent.
			before := readHook(t, root)
			st2, err := InstallCommitHook(root, "/usr/local/bin/cortex")
			if err != nil {
				t.Fatalf("second install: %v", err)
			}
			if after := readHook(t, root); after != before {
				t.Errorf("second install changed the file:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if !st2.AlreadyCurrent {
				t.Errorf("second install state = %+v, want AlreadyCurrent", st2)
			}
		})
	}
}

func TestInstallCommitHookIsExecutable(t *testing.T) {
	t.Parallel()
	root := newRepo(t)
	// A non-executable file already there: Git would silently never run it.
	path := filepath.Join(root, ".git", "hooks", "post-commit")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexisting\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := InstallCommitHook(root, "cortex"); err != nil {
		t.Fatalf("install: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("hook mode = %v, want the execute bit set", info.Mode().Perm())
	}
}

func TestUninstallCommitHook(t *testing.T) {
	t.Parallel()

	t.Run("removes the file when RepoCortex wrote all of it", func(t *testing.T) {
		t.Parallel()
		root := newRepo(t)
		if _, err := InstallCommitHook(root, "cortex"); err != nil {
			t.Fatalf("install: %v", err)
		}
		removed, err := UninstallCommitHook(root)
		if err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		if !removed {
			t.Fatal("removed = false, want true")
		}
		if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "post-commit")); !os.IsNotExist(err) {
			t.Error("a hook containing nothing but our block should be deleted")
		}
	})

	t.Run("keeps another author's hook intact", func(t *testing.T) {
		t.Parallel()
		root := newRepo(t)
		path := filepath.Join(root, ".git", "hooks", "post-commit")
		const theirs = "#!/bin/sh\nmake lint\n"
		if err := os.WriteFile(path, []byte(theirs), 0o755); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := InstallCommitHook(root, "cortex"); err != nil {
			t.Fatalf("install: %v", err)
		}
		if _, err := UninstallCommitHook(root); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		got := readHook(t, root)
		if strings.Contains(got, hookBegin) {
			t.Error("our block survived uninstall")
		}
		if !strings.Contains(got, "make lint") {
			t.Fatalf("their hook was damaged: %q", got)
		}
	})

	t.Run("is a no-op when nothing is installed", func(t *testing.T) {
		t.Parallel()
		root := newRepo(t)
		removed, err := UninstallCommitHook(root)
		if err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		if removed {
			t.Error("removed = true, want false for an absent hook")
		}
	})
}

func TestCommitHookStatus(t *testing.T) {
	t.Parallel()
	root := newRepo(t)

	if st := CommitHookStatus(root); st.Installed {
		t.Error("reported installed before anything was written")
	}
	if _, err := InstallCommitHook(root, "cortex"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if st := CommitHookStatus(root); !st.Installed {
		t.Error("reported not installed after installing")
	}

	// Status must not fail on a directory that is not a repository: the portal
	// calls it on every request and "not installed" is the honest answer.
	if st := CommitHookStatus(t.TempDir()); st.Installed {
		t.Error("a non-repository reported an installed hook")
	}
}

func TestShellQuoteHandlesAwkwardPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"/usr/local/bin/cortex", `'/usr/local/bin/cortex'`},
		{"/Users/a b/cortex", `'/Users/a b/cortex'`},
		{"/tmp/it's/cortex", `'/tmp/it'\''s/cortex'`},
	}
	for _, tc := range tests {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHookBodyCannotFailACommit(t *testing.T) {
	t.Parallel()
	body := hookBody("/usr/local/bin/cortex")
	// The commit must survive a missing binary, a failed index and a slow run.
	if !strings.Contains(body, "command -v") {
		t.Error("hook does not check the binary exists before calling it")
	}
	if !strings.Contains(body, ") &") {
		t.Error("hook does not run detached; a slow index would block every commit")
	}
	if !strings.Contains(body, ">/dev/null 2>&1") {
		t.Error("hook does not swallow output; noise on every commit gets it deleted")
	}
	if strings.Contains(body, "exit 1") {
		t.Error("hook can exit non-zero, which would fail the commit")
	}
}
