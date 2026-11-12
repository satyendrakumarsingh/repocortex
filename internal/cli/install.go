package cli

// `cortex agent` prints the registration a user must paste. This writes it.
//
// The merge is deliberately conservative: an MCP client's config file is the
// user's, not ours. It holds other servers and unrelated settings, and losing
// one because RepoCortex rewrote the file would be a far worse bug than failing
// to install. So each file is parsed, only RepoCortex's own entry is touched,
// and a file that cannot be parsed safely is reported with the manual step
// rather than overwritten. The clients and their formats live in clients.go.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// claudeDesktopPath resolves the cross-platform path to Claude Desktop's config file.
func claudeDesktopPath(home string) string {
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

// mergeMCPConfig inserts spec under mcpServers[name], preserving every other
// key. A nil or empty `existing` yields a fresh document.
func mergeMCPConfig(existing []byte, name string, spec agentServerSpec) ([]byte, error) {
	return mergeJSONKey(existing, "mcpServers", name, stdioEntry(spec))
}

// guidanceCreate is created when missing: most agents read AGENTS.md (Codex,
// Cursor, Copilot, Devin/Windsurf, Antigravity, Zed, Cline, Roo, Junie, Amp,
// Goose, Augment, Kiro, Warp, OpenCode, Qwen, CodeBuddy).
const guidanceCreate = "AGENTS.md"

// guidanceUpdate are agent-specific instruction files. They are updated when
// the repository already has them and never created, so --rules does not
// scatter a file per agent into the project.
var guidanceUpdate = []string{
	"CLAUDE.md",
	"GEMINI.md",
	"QWEN.md",
	"CODEBUDDY.md",
	"WARP.md",
	".cursorrules",
	".windsurfrules",
	".clinerules",
	".roorules",
	filepath.Join(".github", "copilot-instructions.md"),
}

// cliGuidanceOpen marks the block for agents that cannot use MCP.
const (
	cliGuidanceOpen  = "<!-- cortex:cli-guidance -->"
	cliGuidanceClose = "<!-- /cortex:cli-guidance -->"
)

// cliGuidance is for agents without an MCP client (Aider, for example). They
// can run shell commands, so the same answers come from the CLI.
const cliGuidance = cliGuidanceOpen + `
## RepoCortex (command line)

This repository has a RepoCortex index. Run these commands instead of grepping;
add --json for machine-readable output.

- Before changing code: ` + "`cortex context \"<the task in plain English>\"`" + `
- Who calls a symbol: ` + "`cortex callers <symbol>`" + `
- What a change can break: ` + "`cortex impact --base main --head HEAD`" + `
- Routes, tables and settings: ` + "`cortex api`" + `, ` + "`cortex db`" + `, ` + "`cortex config`" + `

Every relationship says how sure it is: exact means a compiler proved it;
anything else is a lead to check. An empty result means nothing was found, not
that nothing exists. If the index is missing, run ` + "`cortex index`" + ` then ` + "`cortex resolve`" + `.
` + cliGuidanceClose + "\n"

// injectAgentGuidance writes the guidance block into AGENTS.md (created when
// missing) and into every agent-specific instruction file the repository
// already has. Files that already carry the block are left alone.
func injectAgentGuidance(cmd *cobra.Command, root string, dryRun bool) error {
	out := cmd.OutOrStdout()
	if err := ensureBlock(out, root, guidanceCreate, agentGuidance, agentGuidanceOpen, true, dryRun); err != nil {
		return err
	}
	for _, name := range guidanceUpdate {
		if err := ensureBlock(out, root, name, agentGuidance, agentGuidanceOpen, false, dryRun); err != nil {
			return err
		}
	}
	// Aider has no MCP client. It reads CONVENTIONS.md when started with
	// --read, so it gets the command-line block.
	_, aiderErr := exec.LookPath("aider")
	return ensureBlock(out, root, "CONVENTIONS.md", cliGuidance, cliGuidanceOpen, aiderErr == nil, dryRun)
}

// ensureBlock appends block to root/name unless it already contains marker.
// A missing file is created only when create is true.
func ensureBlock(out io.Writer, root, name, block, marker string, create, dryRun bool) error {
	path := filepath.Join(root, name)
	content, err := os.ReadFile(path) //nolint:gosec // a fixed name under the project root
	switch {
	case err == nil:
		if strings.Contains(string(content), marker) {
			_, _ = fmt.Fprintf(out, "guidance already present in %s\n", name)
			return nil
		}
		if dryRun {
			_, _ = fmt.Fprintf(out, "would append guidance to %s\n", name)
			return nil
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a fixed name under the project root
		if err != nil {
			return fmt.Errorf("append guidance to %s: %w", name, err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString("\n\n" + block); err != nil {
			return fmt.Errorf("write guidance to %s: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "appended guidance to %s\n", name)
		return nil
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil
		}
		if dryRun {
			_, _ = fmt.Fprintf(out, "would create %s with guidance\n", name)
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create directory for %s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		_, _ = fmt.Fprintf(out, "created %s with guidance\n", name)
		return nil
	default:
		return fmt.Errorf("inspect %s: %w", name, err)
	}
}

// installResult is what happened to one client.
type installResult struct {
	Client string
	Path   string
	Action string // created, updated, unchanged, skipped, manual
	Note   string
}

// installClients writes RepoCortex into every detected client (every client
// with createAll). It never stops at one client's problem: a file it cannot
// merge is reported as a manual step and the rest still install.
func installClients(home, root string, spec agentServerSpec, createAll, dryRun bool) ([]installResult, error) {
	clients := mcpClients(home, root)
	sort.Slice(clients, func(i, j int) bool { return clients[i].Name < clients[j].Name })

	var results []installResult
	for _, c := range clients {
		if c.OnlyIfExists {
			if _, err := os.Stat(c.Path); err != nil {
				continue
			}
		} else if !createAll && !clientInstalled(c) {
			results = append(results, installResult{Client: c.Name, Path: c.Path, Action: "skipped", Note: "not detected"})
			continue
		}

		existing, err := os.ReadFile(c.Path) // #nosec G304 -- path comes from the fixed client list
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			results = append(results, installResult{Client: c.Name, Path: c.Path, Action: "manual", Note: "could not read: " + err.Error()})
			continue
		}
		merged, err := mergeClientConfig(c, existing, spec)
		if err != nil {
			results = append(results, installResult{Client: c.Name, Path: c.Path, Action: "manual", Note: err.Error() + "; see " + c.Docs})
			continue
		}

		action := "updated"
		switch {
		case existing == nil:
			action = "created"
		case string(existing) == string(merged):
			action = "unchanged"
		}
		if !dryRun && action != "unchanged" {
			if err := os.MkdirAll(filepath.Dir(c.Path), 0o750); err != nil {
				return results, fmt.Errorf("create directory for %s: %w", c.Name, err)
			}
			if err := os.WriteFile(c.Path, merged, 0o600); err != nil { //nolint:gosec // G703: the path comes from the fixed client list, not from input
				return results, fmt.Errorf("write %s config: %w", c.Name, err)
			}
		}
		results = append(results, installResult{Client: c.Name, Path: c.Path, Action: action})
	}
	return results, nil
}

// wouldVerb turns a past-tense action into its dry-run form.
func wouldVerb(action string) string {
	switch action {
	case "created":
		return "would create"
	case "updated":
		return "would update"
	}
	return action
}

func printInstallResults(w io.Writer, results []installResult, dryRun bool) (written int) {
	for _, r := range results {
		switch r.Action {
		case "skipped":
			continue
		case "manual":
			_, _ = fmt.Fprintf(w, "manual  %-28s %s\n        %s\n", r.Client, r.Path, r.Note)
			continue
		}
		verb := r.Action
		if dryRun {
			verb = wouldVerb(r.Action)
		}
		_, _ = fmt.Fprintf(w, "%-13s %-28s %s\n", verb, r.Client, r.Path)
		written++
	}
	return written
}

// printClientList shows every supported client: whether it was detected, the
// file RepoCortex writes, and the agents that need a manual step.
func printClientList(w io.Writer, home, root string, spec agentServerSpec) {
	clients := mcpClients(home, root)
	sort.Slice(clients, func(i, j int) bool { return clients[i].Name < clients[j].Name })
	_, _ = fmt.Fprintln(w, "Configured automatically by `cortex install`:")
	for _, c := range clients {
		if c.OnlyIfExists {
			continue
		}
		state := "not detected"
		if clientInstalled(c) {
			state = "detected"
		}
		_, _ = fmt.Fprintf(w, "  %-44s %-13s %s\n", c.Name, state, c.Path)
	}
	_, _ = fmt.Fprintln(w, "\nSet up by hand (their own command or settings screen):")
	for _, m := range manualClients() {
		_, _ = fmt.Fprintf(w, "  %s\n    %s\n", m.Name, m.Step(spec))
	}
	_, _ = fmt.Fprintln(w, "\nNo MCP client (use the CLI): Aider, Plandex. `cortex install --rules` writes CONVENTIONS.md for Aider.")
	_, _ = fmt.Fprintln(w, "Remote MCP only: ChatGPT, Android Studio. Run `cortex serve --http 127.0.0.1:7788` and see docs/guides/coding-agents.md.")
}

func newInstallCmd(g *Global) *cobra.Command {
	var (
		dryRun    bool
		createAll bool
		withRules bool
		list      bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register RepoCortex as an MCP server with detected coding agents",
		Long: `Writes RepoCortex's MCP registration into the config file of every
detected coding agent, preserving every other server and setting already there.

Supported: Amp, Antigravity, Augment, Claude Code, Claude Desktop, Cline CLI,
Codebuff, CodeBuddy CLI, Codex CLI, Continue, Copilot CLI, Cursor (and its CLI),
Devin Desktop, Factory Droid, Gemini CLI, Goose, Junie, Kilo Code, Kimi Code CLI,
Kiro, LM Studio, mcphub.nvim, Qoder CLI, Qwen Code, Tabnine CLI, VS Code
(Copilot), Visual Studio (Windows), Warp, Windsurf and Zed. Run --list to see
which are detected here and the manual step for the rest.

A client counts as detected when its config file, config directory or command
exists. Pass --create to write every client's file regardless. A file RepoCortex
cannot merge safely (for example JSON with comments) is reported with the manual
step and left untouched.

Pass --rules to add guidance to AGENTS.md and to any agent instruction file the
project already has (CLAUDE.md, GEMINI.md, .cursorrules, ...), so the agent asks
RepoCortex before it searches.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home directory: %w", err)
			}
			root := g.Config().RepoRoot
			spec := agentServerSpec{
				Name:    agentMCPName,
				Command: "cortex",
				Args:    []string{"--project", root, "serve"},
			}
			out := cmd.OutOrStdout()

			if list {
				printClientList(out, home, root, spec)
				return nil
			}

			results, err := installClients(home, root, spec, createAll, dryRun)
			written := printInstallResults(out, results, dryRun)
			if err != nil {
				return err
			}

			if withRules {
				if err := injectAgentGuidance(cmd, root, dryRun); err != nil {
					return err
				}
			}

			if written == 0 {
				_, _ = fmt.Fprintln(out, "\nNo coding agent was detected. Run 'cortex install --list' to see every supported agent, or 'cortex install --create' to write all their configs.")
				return nil
			}
			if dryRun {
				_, _ = fmt.Fprintln(out, "\nNothing was written. Run without --dry-run to apply.")
				return nil
			}
			_, _ = fmt.Fprintln(out, "\nRestart your agent, then ask it to use RepoCortex.")
			if !withRules {
				_, _ = fmt.Fprintln(out, "Tip: add guidance so your agent reaches for RepoCortex instead of grepping: cortex install --rules")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be written without writing it")
	cmd.Flags().BoolVar(&createAll, "create", false, "write every supported client's config, even where the client was not detected")
	cmd.Flags().BoolVar(&withRules, "rules", false, "add agent guidance to AGENTS.md and to existing agent instruction files")
	cmd.Flags().BoolVar(&list, "list", false, "list every supported agent, whether it is detected, and manual steps for the rest")
	return cmd
}
