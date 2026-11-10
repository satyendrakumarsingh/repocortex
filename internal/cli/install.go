package cli

// `cortex agent` prints the registration a user must paste. This writes it.
//
// The merge is deliberately conservative: an MCP client's config file is the
// user's, not ours. It holds other servers and unrelated settings, and losing
// one because RepoCortex rewrote the file would be a far worse bug than failing
// to install. So the file is parsed as a generic map, only the one key under
// mcpServers is touched, and a file we cannot parse is an error rather than
// something to overwrite.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// mcpTarget is one client's config file and the label shown to the user.
type mcpTarget struct {
	Client string
	Path   string
}

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

// mcpClientTargets lists the config files RepoCortex knows how to write.
//
// Only clients whose config path is documented, stable, and whose MCP
// registration lives under a top-level "mcpServers" object are listed here.
func mcpClientTargets(home string) []mcpTarget {
	return []mcpTarget{
		{Client: "Antigravity", Path: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json")},
		{Client: "Claude Code", Path: filepath.Join(home, ".claude.json")},
		{Client: "Claude Desktop", Path: claudeDesktopPath(home)},
		{Client: "Cursor", Path: filepath.Join(home, ".cursor", "mcp.json")},
		{Client: "Windsurf", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")},
	}
}

// isClientInstalled checks if the target client appears to be installed on the system.
func isClientInstalled(home string, t mcpTarget) bool {
	if _, err := os.Stat(t.Path); err == nil {
		return true
	}
	dir := filepath.Dir(t.Path)
	if t.Client == "Claude Code" {
		if fi, err := os.Stat(filepath.Join(home, ".claude")); err == nil && fi.IsDir() {
			return true
		}
		return false
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return true
	}
	return false
}

// mergeMCPConfig inserts spec under mcpServers[name], preserving every other
// key. A nil or empty `existing` yields a fresh document.
func mergeMCPConfig(existing []byte, name string, spec agentServerSpec) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("existing config is not valid JSON (refusing to overwrite it): %w", err)
		}
	}

	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}

	entry := map[string]any{"command": spec.Command}
	if len(spec.Args) > 0 {
		entry["args"] = spec.Args
	}
	servers[name] = entry
	doc["mcpServers"] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return append(out, '\n'), nil
}

func injectAgentGuidance(cmd *cobra.Command, root string, dryRun bool) error {
	candidates := []string{".cursorrules", "CLAUDE.md", "AGENTS.md"}
	targetFile := ""
	for _, c := range candidates {
		p := filepath.Join(root, c)
		if _, err := os.Stat(p); err == nil {
			targetFile = p
			break
		}
	}
	if targetFile == "" {
		targetFile = filepath.Join(root, ".cursorrules")
	}

	rel, err := filepath.Rel(root, targetFile)
	if err != nil {
		rel = targetFile
	}

	content, err := os.ReadFile(targetFile) //nolint:gosec
	if err == nil {
		if strings.Contains(string(content), agentGuidanceOpen) {
			fmt.Fprintf(cmd.OutOrStdout(), "guidance already present in %s\n", rel)
			return nil
		}
		if dryRun {
			fmt.Fprintf(cmd.OutOrStdout(), "would append guidance to %s\n", rel)
			return nil
		}
		toAppend := "\n\n" + agentGuidance
		f, err := os.OpenFile(targetFile, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec
		if err != nil {
			return fmt.Errorf("append guidance to %s: %w", rel, err)
		}
		defer f.Close()
		if _, err := f.WriteString(toAppend); err != nil {
			return fmt.Errorf("write guidance to %s: %w", rel, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "appended agent guidance to %s\n", rel)
		return nil
	} else if os.IsNotExist(err) {
		if dryRun {
			fmt.Fprintf(cmd.OutOrStdout(), "would create %s with agent guidance\n", rel)
			return nil
		}
		if err := os.WriteFile(targetFile, []byte(agentGuidance), 0o600); err != nil {
			return fmt.Errorf("create %s: %w", rel, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "created %s with agent guidance\n", rel)
		return nil
	}
	return fmt.Errorf("inspect %s: %w", rel, err)
}

func newInstallCmd(g *Global) *cobra.Command {
	var (
		dryRun    bool
		createAll bool
		withRules bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register RepoCortex as an MCP server with detected coding agents",
		Long: `Writes RepoCortex's MCP registration into the config file of every
detected client, preserving every other server and setting already there.

If a supported client's configuration directory exists (e.g. ~/.cursor or
~/.codeium/windsurf), RepoCortex creates or updates its MCP config. Pass --create
to initialize configs for all known clients regardless of detection. Pass --rules
to inject agent behavioral instructions into .cursorrules or CLAUDE.md.

Run "cortex agent" to print the manual registration for any client not covered here.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve home directory: %w", err)
			}
			root := g.Config().RepoRoot
			spec := agentServerSpec{
				Name:    agentMCPName,
				Command: "cortex",
				Args:    []string{"serve", "--project", root},
			}

			targets := mcpClientTargets(home)
			sort.Slice(targets, func(i, j int) bool { return targets[i].Client < targets[j].Client })

			written := 0
			for _, t := range targets {
				dir := filepath.Dir(t.Path)
				installed := isClientInstalled(home, t) || createAll

				existing, err := os.ReadFile(t.Path) // #nosec G304 -- path is from the fixed target list
				if os.IsNotExist(err) {
					if !installed {
						fmt.Fprintf(cmd.OutOrStdout(), "skip  %-16s (client directory not found)\n", t.Client)
						continue
					}
					existing = nil
				} else if err != nil {
					return fmt.Errorf("read %s config: %w", t.Client, err)
				}

				merged, err := mergeMCPConfig(existing, agentMCPName, spec)
				if err != nil {
					return fmt.Errorf("%s: %w", t.Client, err)
				}

				action := "updated"
				if existing == nil {
					action = "created"
				}

				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "would %s %-10s %s\n", action, t.Client, t.Path)
					written++
					continue
				}

				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("create directory for %s: %w", t.Client, err)
				}
				if err := os.WriteFile(t.Path, merged, 0o600); err != nil {
					return fmt.Errorf("write %s config: %w", t.Client, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-7s %-16s %s\n", action, t.Client, t.Path)
				written++
			}

			if withRules {
				if err := injectAgentGuidance(cmd, root, dryRun); err != nil {
					return err
				}
			}

			if written == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "\nNo client config was found. Run 'cortex install --create' to force config creation, or 'cortex agent' to print the manual registration.")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nRestart your agent, then ask it to use RepoCortex.\n")
			if !withRules {
				fmt.Fprintln(cmd.OutOrStdout(), "Tip: Add guidance so your agent reaches for RepoCortex instead of grepping:\n     Run 'cortex install --rules' or 'cortex agent --guidance >> CLAUDE.md'")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be written without writing it")
	cmd.Flags().BoolVar(&createAll, "create", false, "create client config files even if the client directory was not detected")
	cmd.Flags().BoolVar(&withRules, "rules", false, "inject or update agent guidance into .cursorrules, CLAUDE.md, or AGENTS.md")
	return cmd
}
