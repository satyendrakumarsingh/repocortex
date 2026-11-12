package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Keeping the index current is a scheduling problem, and there are exactly two
// honest ways to solve it: watch the filesystem (`repocortex watch`, a process
// that must be running) or hook the moment the tree reaches a state worth
// indexing (a commit). The second needs no daemon and no editor integration,
// which is why it exists: a portal reading a three-day-old index is worse than
// one reading none, because it looks current.
//
// A commit hook is a file RepoCortex writes into someone's repository, so the
// rules here are deliberately conservative:
//
//   - never clobber a hook that is already there; append inside markers;
//   - be idempotent — installing twice leaves one block, not two;
//   - remove exactly what was added and nothing else;
//   - never fail the commit. A re-index that errors must not cost work.
const (
	hookBegin = "# >>> repocortex post-commit (managed) >>>"
	hookEnd   = "# <<< repocortex post-commit (managed) <<<"

	// postCommitName is the only hook RepoCortex writes. post-commit runs after
	// the commit object exists, so the index it builds names a revision that a
	// reader can actually check out — which pre-commit cannot promise.
	postCommitName = "post-commit"
)

// hookBody is the managed block. It re-indexes and re-resolves in the
// background and swallows every failure, because a hook that can fail a commit
// is a hook people delete.
func hookBody(binary string) string {
	var b strings.Builder
	b.WriteString(hookBegin + "\n")
	b.WriteString("# Re-indexes this repository after every commit so the graph, the portal\n")
	b.WriteString("# and any connected agent describe the commit you just made.\n")
	b.WriteString("# Runs detached and never fails the commit: a stale index is a problem,\n")
	b.WriteString("# a blocked commit is a worse one.\n")
	b.WriteString("if command -v " + shellQuote(binary) + " >/dev/null 2>&1; then\n")
	b.WriteString("  (" + shellQuote(binary) + " index --quiet >/dev/null 2>&1 && \\\n")
	b.WriteString("   " + shellQuote(binary) + " resolve --quiet >/dev/null 2>&1) &\n")
	b.WriteString("fi\n")
	b.WriteString(hookEnd + "\n")
	return b.String()
}

// shellQuote renders s safe to paste into a POSIX shell script. Paths with
// spaces are ordinary on macOS, and an unquoted one silently produces a hook
// that does nothing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func newHooksCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Keep the index current by re-indexing on every commit",
		Long: "Installs a Git post-commit hook that re-indexes this repository after each\n" +
			"commit, so the portal and any connected agent describe the code you just\n" +
			"wrote rather than the code you wrote on Tuesday.\n\n" +
			"The hook is written inside marked delimiters. An existing post-commit hook is\n" +
			"appended to, never replaced, and 'hooks uninstall' removes exactly the marked\n" +
			"block. The hook runs detached and cannot fail a commit.",
	}
	cmd.AddCommand(newHooksInstallCmd(g), newHooksUninstallCmd(g), newHooksStatusCmd(g))
	return cmd
}

func newHooksInstallCmd(g *Global) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the post-commit hook into this repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := g.Config().RepoRoot
			if binary == "" {
				binary = resolveOwnBinary()
			}
			st, err := InstallCommitHook(root, binary)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case st.AlreadyCurrent:
				fmt.Fprintf(out, "Already installed — %s\n", st.Path)
			case st.Appended:
				fmt.Fprintf(out, "Appended RepoCortex's block to your existing hook\n  %s\n", st.Path)
			default:
				fmt.Fprintf(out, "Installed post-commit hook\n  %s\n", st.Path)
			}
			fmt.Fprintf(out, "  Runs: %s index && %s resolve  (detached, never fails a commit)\n", binary, binary)
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "",
		"path to the repocortex binary the hook should call (default: this executable)")
	return cmd
}

func newHooksUninstallCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the post-commit hook block RepoCortex wrote",
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := UninstallCommitHook(g.Config().RepoRoot)
			if err != nil {
				return err
			}
			if !removed {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove — no RepoCortex block in this repository's post-commit hook.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed RepoCortex's block. Anything else in that hook is untouched.")
			return nil
		},
	}
}

func newHooksStatusCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the post-commit hook is installed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := CommitHookStatus(g.Config().RepoRoot)
			out := cmd.OutOrStdout()
			if st.Installed {
				fmt.Fprintf(out, "post-commit: installed  (%s)\n", st.Path)
				fmt.Fprintln(out, "The index is rebuilt after every commit on this machine.")
				return nil
			}
			fmt.Fprintln(out, "post-commit: not installed")
			fmt.Fprintln(out, "The index is only as current as the last 'repocortex index'.")
			fmt.Fprintln(out, "Install it with: repocortex hooks install")
			return nil
		},
	}
}

// HookState reports what an install actually did. Callers print it; the portal
// renders it.
type HookState struct {
	Path           string `json:"path"`
	Installed      bool   `json:"installed"`
	Appended       bool   `json:"appended,omitempty"`
	AlreadyCurrent bool   `json:"already_current,omitempty"`
}

// hooksDir resolves where Git expects hooks for this repository, honouring
// core.hooksPath. A repository that has moved its hooks elsewhere would
// otherwise get a file Git never runs — installed, reported as working, and
// silently inert.
func hooksDir(root string) (string, error) {
	if out, err := exec.Command("git", "-C", root, "config", "--get", "core.hooksPath").Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if filepath.IsAbs(p) {
				return p, nil
			}
			return filepath.Join(root, p), nil
		}
	}
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", fmt.Errorf("hooks: %s is not a Git repository: %w", root, err)
	}
	if !info.IsDir() {
		// A worktree or submodule: .git is a file pointing at the real dir.
		data, readErr := os.ReadFile(gitDir)
		if readErr != nil {
			return "", fmt.Errorf("hooks: read %s: %w", gitDir, readErr)
		}
		line := strings.TrimSpace(string(data))
		p := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if p == "" {
			return "", fmt.Errorf("hooks: %s does not name a git directory", gitDir)
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		return filepath.Join(p, "hooks"), nil
	}
	return filepath.Join(gitDir, "hooks"), nil
}

// InstallCommitHook writes (or updates) the managed block.
func InstallCommitHook(root, binary string) (HookState, error) {
	dir, err := hooksDir(root)
	if err != nil {
		return HookState{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return HookState{}, fmt.Errorf("hooks: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, postCommitName)
	block := hookBody(binary)

	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		content := "#!/bin/sh\n" + block
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			return HookState{}, fmt.Errorf("hooks: write %s: %w", path, err)
		}
		return HookState{Path: path, Installed: true}, nil
	case err != nil:
		return HookState{}, fmt.Errorf("hooks: read %s: %w", path, err)
	}

	text := string(existing)
	if stripped, had := stripBlock(text); had {
		if stripped+block == text || strings.Contains(text, block) {
			return HookState{Path: path, Installed: true, AlreadyCurrent: true}, nil
		}
		// A block from an older version: replace it rather than stack a second.
		if err := writeHook(path, ensureTrailingNewline(stripped)+block); err != nil {
			return HookState{}, err
		}
		return HookState{Path: path, Installed: true, Appended: true}, nil
	}
	if err := writeHook(path, ensureTrailingNewline(text)+block); err != nil {
		return HookState{}, err
	}
	return HookState{Path: path, Installed: true, Appended: true}, nil
}

func writeHook(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return fmt.Errorf("hooks: write %s: %w", path, err)
	}
	// WriteFile does not chmod an existing file, and a hook Git cannot execute
	// is a hook that silently never runs.
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("hooks: chmod %s: %w", path, err)
	}
	return nil
}

func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// stripBlock removes the managed block and reports whether one was present.
func stripBlock(text string) (string, bool) {
	start := strings.Index(text, hookBegin)
	if start < 0 {
		return text, false
	}
	end := strings.Index(text[start:], hookEnd)
	if end < 0 {
		// Truncated block — treat everything from the marker on as ours, which
		// is the only reading that leaves the file valid.
		return strings.TrimRight(text[:start], "\n") + "\n", true
	}
	end += start + len(hookEnd)
	for end < len(text) && text[end] == '\n' {
		end++
	}
	return text[:start] + text[end:], true
}

// UninstallCommitHook removes the managed block, leaving any other hook content
// exactly as it was. Reports whether anything was removed.
func UninstallCommitHook(root string) (bool, error) {
	dir, err := hooksDir(root)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, postCommitName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hooks: read %s: %w", path, err)
	}
	stripped, had := stripBlock(string(data))
	if !had {
		return false, nil
	}
	// If nothing but the shebang survives, the file was ours alone.
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stripped), "#!/bin/sh")) == "" {
		if err := os.Remove(path); err != nil {
			return false, fmt.Errorf("hooks: remove %s: %w", path, err)
		}
		return true, nil
	}
	if err := writeHook(path, stripped); err != nil {
		return false, err
	}
	return true, nil
}

// CommitHookStatus reports installation without touching anything. It never
// returns an error: "not installed" is the right answer for a directory that is
// not a repository, and a status call must not fail a portal request.
func CommitHookStatus(root string) HookState {
	dir, err := hooksDir(root)
	if err != nil {
		return HookState{}
	}
	path := filepath.Join(dir, postCommitName)
	data, err := os.ReadFile(path)
	if err != nil {
		return HookState{Path: path}
	}
	return HookState{Path: path, Installed: strings.Contains(string(data), hookBegin)}
}

// resolveOwnBinary names the executable to call from the hook. An absolute path
// is preferred: a hook runs under Git's environment, whose PATH is not the
// interactive shell's, and "repocortex: command not found" in a background job is
// invisible.
func resolveOwnBinary() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	return "repocortex"
}
