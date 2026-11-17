package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
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

	// postCommitName is the primary hook RepoCortex writes.
	postCommitName = "post-commit"
)

var managedHookNames = []string{"post-commit", "post-merge", "post-checkout"}

// hookBody generates the managed block for a specific git hook.
// It executes serialized background auto-index, folding overlapping runs
// and logging to .repocortex/logs/auto-index.log.
func hookBody(binary string, hookName ...string) string {
	name := postCommitName
	if len(hookName) > 0 && hookName[0] != "" {
		name = hookName[0]
	}
	var b strings.Builder
	b.WriteString(hookBegin + "\n")
	b.WriteString("# Re-indexes this repository after commits, merges and branch switches so the graph,\n")
	b.WriteString("# the portal and any connected agent describe the code you just updated.\n")
	b.WriteString("# Runs serialized background auto-index and never fails a git operation.\n")
	if name == "post-checkout" {
		b.WriteString("# Only run on branch switches ($3 == 1), not on single-file checkouts ($3 == 0).\n")
		b.WriteString("if [ \"$3\" = \"1\" ] && command -v " + shellQuote(binary) + " >/dev/null 2>&1; then\n")
	} else {
		b.WriteString("if command -v " + shellQuote(binary) + " >/dev/null 2>&1; then\n")
	}
	b.WriteString("  (" + shellQuote(binary) + " auto-index >/dev/null 2>&1) &\n")
	b.WriteString("fi\n")
	b.WriteString(hookEnd + "\n")
	return b.String()
}

// hooksInWorkTree reports whether the hook directory is part of the work tree,
// where it is committed, rather than inside .git or outside the repository.
func hooksInWorkTree(root string) bool {
	dir, err := hooksDir(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	return first != ".git"
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
		Short: "Keep the index current by re-indexing on commits, merges and branch checkouts",
		Long: "Installs Git hooks (post-commit, post-merge, and post-checkout) that re-index\n" +
			"this repository after each commit, merge, or branch switch, so the portal\n" +
			"and any connected agent describe the code you just updated.\n\n" +
			"Hooks are written inside marked delimiters. Existing hooks are preserved,\n" +
			"and 'hooks uninstall' removes exactly the marked block. The hooks run\n" +
			"serialized in the background and cannot fail a git operation.",
	}
	cmd.AddCommand(newHooksInstallCmd(g), newHooksUninstallCmd(g), newHooksStatusCmd(g))
	return cmd
}

func newHooksInstallCmd(g *Global) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write managed hooks (post-commit, post-merge, post-checkout) into this repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := g.Config().RepoRoot
			if binary == "" {
				binary = resolveOwnBinary(root)
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
				fmt.Fprintf(out, "Appended RepoCortex's block to existing hooks in %s\n", filepath.Dir(st.Path))
			default:
				fmt.Fprintf(out, "Installed managed hooks (post-commit, post-merge, post-checkout)\n  %s\n", filepath.Dir(st.Path))
			}
			fmt.Fprintf(out, "  Runs: %s auto-index (serialized background pass, logs to .repocortex/logs/auto-index.log)\n", binary)
			// A hook that is not a shell script is left untouched; its block
			// went to an auxiliary script that git will not run by itself.
			rep := AllHooksStatus(root)
			for _, h := range []HookState{rep.PostCommit, rep.PostMerge, rep.PostCheckout} {
				if h.NeedsWiring {
					fmt.Fprintf(out, "\n  ACTION NEEDED: %s is not a shell script, so RepoCortex did not edit it.\n"+
						"  Its block is in %s, which git will not run on its own.\n  %s\n",
						filepath.Base(h.Path), h.AuxiliaryPath, wiringHint(h))
				}
			}
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
		Short: "Remove the hook blocks RepoCortex wrote",
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := UninstallCommitHook(g.Config().RepoRoot)
			if err != nil {
				return err
			}
			if !removed {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to remove — no RepoCortex block in this repository's hooks.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed RepoCortex's block. Anything else in those hooks is untouched.")
			return nil
		},
	}
}

func newHooksStatusCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report status of all managed Git hooks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := AllHooksStatus(g.Config().RepoRoot)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "post-commit:   %s\n", formatHookState(rep.PostCommit))
			fmt.Fprintf(out, "post-merge:    %s\n", formatHookState(rep.PostMerge))
			fmt.Fprintf(out, "post-checkout: %s\n", formatHookState(rep.PostCheckout))
			if rep.PostCommit.Installed && rep.PostMerge.Installed && rep.PostCheckout.Installed {
				fmt.Fprintln(out, "The index is automatically kept current on commits, merges and branch switches.")
			} else {
				fmt.Fprintln(out, "Install all managed hooks with: repocortex hooks install")
			}
			return nil
		},
	}
}

func formatHookState(st HookState) string {
	switch {
	case st.Installed && st.AuxiliaryPath != "":
		return fmt.Sprintf("installed, via %s called from your hook", filepath.Base(st.AuxiliaryPath))
	case st.Installed:
		return fmt.Sprintf("installed (%s)", st.Path)
	case st.NeedsWiring:
		return fmt.Sprintf("not active: %s was written, but your hook does not call it and git never runs it on its own", st.AuxiliaryPath)
	}
	return "not installed"
}

// HookState reports what an install actually did. Callers print it; the portal
// renders it.
type HookState struct {
	Path          string `json:"path"`
	AuxiliaryPath string `json:"auxiliary_path,omitempty"`
	// NeedsWiring is true when RepoCortex wrote an auxiliary script beside a
	// hook it would not edit (not a shell script) and that hook does not call
	// it yet. Git never runs the auxiliary script by itself, so such a hook is
	// not Installed.
	NeedsWiring    bool `json:"needs_wiring,omitempty"`
	Installed      bool `json:"installed"`
	Appended       bool `json:"appended,omitempty"`
	AlreadyCurrent bool `json:"already_current,omitempty"`
}

// HooksReport reports status across all managed hooks.
type HooksReport struct {
	PostCommit   HookState `json:"post_commit"`
	PostMerge    HookState `json:"post_merge"`
	PostCheckout HookState `json:"post_checkout"`
}

// hooksDir resolves where Git expects hooks for this repository, honouring
// core.hooksPath. A repository that has moved its hooks elsewhere would
// otherwise get a file Git never runs — installed, reported as working, and
// silently inert.
func hooksDir(root string) (string, error) {
	//nolint:gosec // G702: an argument vector, not a shell line; root is the project directory, passed as one argument
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

// posixShells are the interpreters the managed block runs under: it is POSIX
// sh (`if [ ... ]; then`, `command -v`, `&`).
var posixShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true}

// isShellShebang reports whether the first line names a POSIX-compatible shell.
//
// By the interpreter's name, not by whether the line contains "sh": fish,
// pwsh, csh and tcsh all contain it and none of them runs the block, so a
// hook in one of them got a block it could not execute instead of the
// separate script it needs.
func isShellShebang(firstLine string) bool {
	firstLine = strings.TrimSpace(firstLine)
	if !strings.HasPrefix(firstLine, "#!") {
		return true // git runs a hook with no shebang through sh
	}
	fields := strings.Fields(strings.TrimPrefix(firstLine, "#!"))
	if len(fields) == 0 {
		return true
	}
	interp := path.Base(fields[0])
	if interp == "env" {
		interp = ""
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") || strings.Contains(f, "=") {
				continue // env -S, env VAR=value
			}
			interp = path.Base(f)
			break
		}
	}
	return posixShells[strings.ToLower(interp)]
}

// hasTerminalExit reports whether the script ends in a top-level exit statement.
func hasTerminalExit(text string) bool {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "exit ") || trimmed == "exit" {
			return true
		}
		break
	}
	return false
}

// insertBeforeTerminalExit inserts content before the final top-level exit command.
func insertBeforeTerminalExit(text, block string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "exit ") || trimmed == "exit" {
			return strings.Join(lines[:i], "\n") + "\n" + block + "\n" + strings.Join(lines[i:], "\n")
		}
		break
	}
	return ensureTrailingNewline(text) + block
}

// InstallCommitHook writes (or updates) the managed block in post-commit,
// post-merge (on git pull/merge), and post-checkout (on branch switch).
func InstallCommitHook(root, binary string) (HookState, error) {
	dir, err := hooksDir(root)
	if err != nil {
		return HookState{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return HookState{}, fmt.Errorf("hooks: create %s: %w", dir, err)
	}

	var primary HookState
	for _, name := range managedHookNames {
		block := hookBody(binary, name)
		st, err := installSingleHook(filepath.Join(dir, name), name, block)
		if err != nil {
			return HookState{}, err
		}
		if name == postCommitName {
			primary = st
		}
	}
	return primary, nil
}

func installSingleHook(path, name, block string) (HookState, error) {
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

	// Check shebang: if non-shell (e.g. Python, Node), do NOT append shell syntax.
	// Write an auxiliary hook instead to prevent syntax errors.
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && !isShellShebang(lines[0]) {
		auxPath := path + "-repocortex"
		auxContent := "#!/bin/sh\n" + block
		if err := os.WriteFile(auxPath, []byte(auxContent), 0o755); err != nil {
			return HookState{}, fmt.Errorf("hooks: write auxiliary %s: %w", auxPath, err)
		}
		wired := strings.Contains(text, filepath.Base(auxPath))
		return HookState{Path: path, AuxiliaryPath: auxPath, Installed: wired, NeedsWiring: !wired}, nil
	}

	if stripped, had := stripBlock(text); had {
		if stripped+block == text || strings.Contains(text, block) {
			return HookState{Path: path, Installed: true, AlreadyCurrent: true}, nil
		}
		// A block from an older version: replace it
		newContent := ensureTrailingNewline(stripped) + block
		if hasTerminalExit(stripped) {
			newContent = insertBeforeTerminalExit(stripped, block)
		}
		if err := writeHook(path, newContent); err != nil {
			return HookState{}, err
		}
		return HookState{Path: path, Installed: true, Appended: true}, nil
	}

	newContent := ensureTrailingNewline(text) + block
	if hasTerminalExit(text) {
		newContent = insertBeforeTerminalExit(text, block)
	}
	if err := writeHook(path, newContent); err != nil {
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

// stripBlock removes any managed RepoCortex block and reports whether one was present.
func stripBlock(text string) (string, bool) {
	start := strings.Index(text, hookBegin)
	if start < 0 {
		return text, false
	}
	end := strings.Index(text[start:], hookEnd)
	if end < 0 {
		// Truncated block — treat everything from the marker on as ours.
		return strings.TrimRight(text[:start], "\n") + "\n", true
	}
	end += start + len(hookEnd)
	for end < len(text) && text[end] == '\n' {
		end++
	}
	return text[:start] + text[end:], true
}

// UninstallCommitHook removes the managed block from post-commit, post-merge,
// and post-checkout, leaving any other hook content exactly as it was.
// Reports whether anything was removed.
func UninstallCommitHook(root string) (bool, error) {
	dir, err := hooksDir(root)
	if err != nil {
		return false, err
	}
	anyRemoved := false
	for _, name := range managedHookNames {
		removed, err := uninstallSingleHook(filepath.Join(dir, name))
		if err != nil {
			return false, err
		}
		if removed {
			anyRemoved = true
		}
	}
	return anyRemoved, nil
}

func uninstallSingleHook(path string) (bool, error) {
	// Also clean auxiliary script if present
	_ = os.Remove(path + "-repocortex")

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

// CommitHookStatus reports primary post-commit installation status.
func CommitHookStatus(root string) HookState {
	return checkHook(root, postCommitName)
}

// AllHooksStatus reports status across post-commit, post-merge, and post-checkout.
func AllHooksStatus(root string) HooksReport {
	return HooksReport{
		PostCommit:   checkHook(root, "post-commit"),
		PostMerge:    checkHook(root, "post-merge"),
		PostCheckout: checkHook(root, "post-checkout"),
	}
}

func checkHook(root, name string) HookState {
	dir, err := hooksDir(root)
	if err != nil {
		return HookState{}
	}
	path := filepath.Join(dir, name)
	auxPath := path + "-repocortex"
	hasAux := false
	if auxData, errAux := os.ReadFile(auxPath); errAux == nil && strings.Contains(string(auxData), "# >>> repocortex") { //nolint:gosec // G304: a hook path this package derives
		hasAux = true
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a hook path this package derives
	if err != nil {
		if hasAux {
			// The user's hook is gone, so nothing calls the auxiliary script.
			return HookState{Path: path, AuxiliaryPath: auxPath, NeedsWiring: true}
		}
		return HookState{Path: path}
	}
	str := string(data)
	if strings.Contains(str, "# >>> repocortex") {
		return HookState{Path: path, Installed: true}
	}
	if hasAux {
		// Git runs only the hook named after the event. The auxiliary script
		// counts once the user's hook calls it, and not before.
		wired := strings.Contains(str, filepath.Base(auxPath))
		return HookState{Path: path, AuxiliaryPath: auxPath, Installed: wired, NeedsWiring: !wired}
	}
	return HookState{Path: path}
}

// wiringHint is the line a user adds to a hook RepoCortex would not edit, so
// that the hook calls the auxiliary script.
func wiringHint(st HookState) string {
	return fmt.Sprintf("Add a call to it at the end of %s, passing the hook's arguments through, e.g.\n"+
		"      shell:  %s \"$@\"\n"+
		"      python: subprocess.run([os.path.join(os.path.dirname(__file__), %q)] + sys.argv[1:])",
		st.Path, shellQuote(st.AuxiliaryPath), filepath.Base(st.AuxiliaryPath))
}

// resolveOwnBinary names the executable to call from the hook.
//
// A hook directory in the work tree (core.hooksPath = .husky, .githooks) is
// committed and shared, so it gets the bare name "repocortex": an absolute path
// would leak this machine's layout and point nowhere on anyone else's. Every
// other hook directory, .git/hooks above all, is this machine's own and gets
// the absolute path, so a git client that runs hooks with a minimal PATH (a GUI
// client, a cron job) still finds the binary.
func resolveOwnBinary(root string) string {
	if hooksInWorkTree(root) {
		return "repocortex"
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	return "repocortex"
}
