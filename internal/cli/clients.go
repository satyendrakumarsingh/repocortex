package cli

// The coding agents `repocortex install` can register RepoCortex with.
//
// Each client stores its MCP servers in its own file and shape, so a client is
// described by where its user-level config lives and which writer understands
// it. Every writer merges: the file belongs to the user, holds other servers
// and settings, and is never rewritten wholesale. A file a writer cannot parse
// safely (for example JSON with comments) is reported as needing a manual step,
// never overwritten.
//
// Paths and shapes come from each vendor's own documentation; the Docs field
// records where. Clients whose config path is not documented, or that are only
// configured through a UI or their own CLI, are listed in manualClients with the
// exact step instead of a path.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// clientFormat is the shape of a client's config file.
type clientFormat int

const (
	// fmtJSONMap: a JSON object under Key maps server name to an entry.
	fmtJSONMap clientFormat = iota
	// fmtCodexTOML: Codex's config.toml, one [mcp_servers.<name>] table.
	fmtCodexTOML
	// fmtGooseYAML: Goose's config.yaml, one entry under `extensions`.
	fmtGooseYAML
	// fmtContinueYAML: a standalone Continue MCP file owned by RepoCortex.
	fmtContinueYAML
)

// mcpClient is one coding agent and the user-level file RepoCortex writes.
type mcpClient struct {
	Name   string
	Path   string
	Format clientFormat
	// Key is the top-level JSON key for fmtJSONMap (mcpServers, servers, ...).
	Key string
	// Entry builds the value stored under the server name for fmtJSONMap.
	Entry func(agentServerSpec) map[string]any
	// DetectDirs and DetectBins say the client is installed when any one
	// exists. The config file existing always counts.
	DetectDirs []string
	DetectBins []string
	// OnlyIfExists: update this file when it is already there, never create
	// it. Used for paths a client has moved away from.
	OnlyIfExists bool
	Docs         string
}

// manualClient is an agent whose MCP config is set through its own CLI or UI,
// or whose file path is not documented. RepoCortex prints the step.
type manualClient struct {
	Name string
	Step func(agentServerSpec) string
	Docs string
}

// stdioEntry is the common {command, args} entry.
func stdioEntry(s agentServerSpec) map[string]any {
	e := map[string]any{"command": s.Command}
	if len(s.Args) > 0 {
		e["args"] = s.Args
	}
	return e
}

// typedStdioEntry adds "type": "stdio", which VS Code requires.
func typedStdioEntry(s agentServerSpec) map[string]any {
	e := stdioEntry(s)
	e["type"] = "stdio"
	return e
}

// copilotCLIEntry is GitHub Copilot CLI's shape: it needs type "local" and an
// explicit tool allow-list.
func copilotCLIEntry(s agentServerSpec) map[string]any {
	e := stdioEntry(s)
	e["type"] = "local"
	e["tools"] = []string{"*"}
	return e
}

// kiloEntry is Kilo Code's shape: the command and its arguments as one array.
func kiloEntry(s agentServerSpec) map[string]any {
	return map[string]any{
		"type":    "local",
		"command": append([]string{s.Command}, s.Args...),
		"enabled": true,
	}
}

// appData returns %APPDATA% on Windows, falling back to the default location.
func appData(home string) string {
	if v := os.Getenv("APPDATA"); v != "" {
		return v
	}
	return filepath.Join(home, "AppData", "Roaming")
}

// vsCodeUserDir is VS Code's user settings directory.
func vsCodeUserDir(home string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(appData(home), "Code", "User")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Code", "User")
	default:
		return filepath.Join(xdgConfig(home), "Code", "User")
	}
}

// xdgConfig is $XDG_CONFIG_HOME or ~/.config.
func xdgConfig(home string) string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".config")
}

// kimiHome is $KIMI_CODE_HOME or ~/.kimi-code.
func kimiHome(home string) string {
	if v := os.Getenv("KIMI_CODE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".kimi-code")
}

// codexHome is $CODEX_HOME or ~/.codex.
func codexHome(home string) string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".codex")
}

// mcpClients lists every client RepoCortex can configure by writing a file,
// for this operating system. root is the repository, for project-scoped files.
func mcpClients(home, root string) []mcpClient {
	win := runtime.GOOS == "windows"
	cfg := xdgConfig(home)

	devinDir := filepath.Join(cfg, "devin")
	gooseFile := filepath.Join(cfg, "goose", "config.yaml")
	if win {
		devinDir = filepath.Join(appData(home), "devin")
		gooseFile = filepath.Join(appData(home), "Block", "goose", "config", "config.yaml")
	}

	clients := []mcpClient{
		{Name: "Amp", Path: filepath.Join(cfg, "amp", "settings.json"), Format: fmtJSONMap, Key: "amp.mcpServers", Entry: stdioEntry,
			DetectBins: []string{"amp"}, Docs: "https://ampcode.com/docs/customize/mcp"},
		{Name: "Antigravity", Path: filepath.Join(home, ".gemini", "config", "mcp_config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".gemini", "config"), filepath.Join(home, ".gemini", "antigravity")}, Docs: "https://antigravity.google/docs/mcp"},
		{Name: "Antigravity (earlier path)", Path: filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			OnlyIfExists: true, Docs: "https://antigravity.google/docs/mcp"},
		{Name: "Augment (Auggie)", Path: filepath.Join(home, ".augment", "settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectBins: []string{"auggie"}, Docs: "https://docs.augmentcode.com/cli/integrations"},
		{Name: "Claude Code", Path: filepath.Join(home, ".claude.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".claude")}, DetectBins: []string{"claude"}, Docs: "https://code.claude.com/docs/en/mcp"},
		{Name: "Claude Desktop", Path: claudeDesktopPath(home), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			Docs: "https://modelcontextprotocol.io/docs/develop/connect-local-servers"},
		{Name: "Cline CLI", Path: filepath.Join(home, ".cline", "data", "settings", "cline_mcp_settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".cline")}, DetectBins: []string{"cline"}, Docs: "https://docs.cline.bot/mcp/configuring-mcp-servers"},
		{Name: "Codex CLI", Path: filepath.Join(codexHome(home), "config.toml"), Format: fmtCodexTOML,
			DetectDirs: []string{codexHome(home)}, DetectBins: []string{"codex"}, Docs: "https://learn.chatgpt.com/docs/extend/mcp?surface=cli"},
		{Name: "Continue (this project)", Path: filepath.Join(root, ".continue", "mcpServers", "repocortex.yaml"), Format: fmtContinueYAML,
			DetectDirs: []string{filepath.Join(root, ".continue"), filepath.Join(home, ".continue")}, Docs: "https://docs.continue.dev/customize/deep-dives/mcp"},
		{Name: "Codebuff", Path: filepath.Join(home, ".agents", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectBins: []string{"codebuff"}, Docs: "https://www.codebuff.com/docs/tips/mcp-servers"},
		{Name: "CodeBuddy CLI", Path: filepath.Join(home, ".codebuddy", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".codebuddy")}, DetectBins: []string{"codebuddy"}, Docs: "https://www.workbuddy.ai/docs/cli/mcp"},
		{Name: "Copilot CLI", Path: filepath.Join(home, ".copilot", "mcp-config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: copilotCLIEntry,
			DetectDirs: []string{filepath.Join(home, ".copilot")}, DetectBins: []string{"copilot"}, Docs: "https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers"},
		{Name: "Cursor", Path: filepath.Join(home, ".cursor", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			Docs: "https://cursor.com/docs/context/mcp"},
		{Name: "Devin Desktop", Path: filepath.Join(devinDir, "mcp_config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectBins: []string{"devin"}, Docs: "https://docs.devin.ai/windsurf/plugins/cascade/mcp.md"},
		{Name: "Factory Droid", Path: filepath.Join(home, ".factory", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".factory")}, DetectBins: []string{"droid"}, Docs: "https://docs.factory.com/cli/configuration/mcp"},
		{Name: "Gemini CLI", Path: filepath.Join(home, ".gemini", "settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectBins: []string{"gemini"}, Docs: "https://geminicli.com/docs/tools/mcp-server/"},
		{Name: "Goose", Path: gooseFile, Format: fmtGooseYAML,
			DetectBins: []string{"goose"}, Docs: "https://goose-docs.ai/docs/guides/config-files"},
		{Name: "Junie (JetBrains)", Path: filepath.Join(home, ".junie", "mcp", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".junie")}, Docs: "https://junie.jetbrains.com/docs/junie-cli-mcp-configuration.html"},
		{Name: "Kilo Code", Path: filepath.Join(cfg, "kilo", "kilo.json"), Format: fmtJSONMap, Key: "mcp", Entry: kiloEntry,
			DetectBins: []string{"kilo"}, Docs: "https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code"},
		{Name: "Kimi Code CLI", Path: filepath.Join(kimiHome(home), "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{kimiHome(home)}, Docs: "https://www.kimi.com/code/docs/en/kimi-code-cli/customization/mcp.html"},
		{Name: "Kiro", Path: filepath.Join(home, ".kiro", "settings", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".kiro")}, DetectBins: []string{"kiro-cli"}, Docs: "https://kiro.dev/docs/mcp/configuration/"},
		{Name: "LM Studio", Path: filepath.Join(home, ".lmstudio", "mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".lmstudio")}, Docs: "https://lmstudio.ai/docs/app/mcp"},
		{Name: "mcphub.nvim (Neovim: avante, CodeCompanion)", Path: filepath.Join(cfg, "mcphub", "servers.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			Docs: "https://ravitemer.github.io/mcphub.nvim/mcp/servers_json.html"},
		{Name: "Qoder CLI", Path: filepath.Join(home, ".qoder", "settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".qoder")}, DetectBins: []string{"qoder"}, Docs: "https://docs.qoder.com/cli/mcp-servers"},
		{Name: "Qwen Code", Path: filepath.Join(home, ".qwen", "settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".qwen")}, DetectBins: []string{"qwen"}, Docs: "https://qwenlm.github.io/qwen-code-docs/en/users/features/mcp/"},
		{Name: "Tabnine CLI", Path: filepath.Join(home, ".tabnine", "agent", "settings.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".tabnine", "agent")}, DetectBins: []string{"tabnine"}, Docs: "https://docs.tabnine.com/main/getting-started/tabnine-cli/features/mcp-server-config"},
		{Name: "VS Code (Copilot)", Path: filepath.Join(vsCodeUserDir(home), "mcp.json"), Format: fmtJSONMap, Key: "servers", Entry: typedStdioEntry,
			Docs: "https://code.visualstudio.com/docs/agents/reference/mcp-configuration"},
		{Name: "Warp", Path: filepath.Join(home, ".warp", ".mcp.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			DetectDirs: []string{filepath.Join(home, ".warp")}, Docs: "https://docs.warp.dev/agent-platform/warp-agents/agent-context/mcp"},
		{Name: "Windsurf", Path: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			Docs: "https://docs.devin.ai/windsurf/plugins/cascade/mcp.md"},
		{Name: "Windsurf Next", Path: filepath.Join(home, ".codeium", "windsurf-next", "mcp_config.json"), Format: fmtJSONMap, Key: "mcpServers", Entry: stdioEntry,
			Docs: "https://docs.devin.ai/windsurf/plugins/cascade/mcp.md"},
	}
	if !win {
		// Zed documents ~/.config/zed/settings.json; its Windows path is not
		// documented, so it is left to the manual step there.
		clients = append(clients, mcpClient{Name: "Zed", Path: filepath.Join(home, ".config", "zed", "settings.json"), Format: fmtJSONMap, Key: "context_servers", Entry: stdioEntry,
			Docs: "https://zed.dev/docs/ai/mcp"})
	}
	if win {
		clients = append(clients, mcpClient{Name: "Visual Studio", Path: filepath.Join(home, ".mcp.json"), Format: fmtJSONMap, Key: "servers", Entry: typedStdioEntry,
			DetectDirs: []string{filepath.Join(appData(home), "Microsoft", "VisualStudio")}, Docs: "https://learn.microsoft.com/en-us/visualstudio/ide/mcp-servers"})
	}
	return clients
}

// manualClients are configured through their own CLI or UI. Their steps are
// printed by `repocortex agent` and `repocortex install --list`.
func manualClients() []manualClient {
	args := func(s agentServerSpec) string { return shellJoin(append([]string{s.Command}, s.Args...)) }
	return []manualClient{
		{Name: "Amazon Q Developer CLI", Docs: "https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/command-line-mcp-config-CLI.html",
			Step: func(s agentServerSpec) string {
				return fmt.Sprintf("q mcp add --name %s --command %s --args %s", shellWord(s.Name), shellWord(s.Command), shellWord(jsonArray(s.Args)))
			}},
		{Name: "Cline (VS Code extension)", Docs: "https://docs.cline.bot/mcp/configuring-mcp-servers",
			Step: func(s agentServerSpec) string {
				return "MCP Servers > Configure, then add to mcpServers: " + string(mustJSON(map[string]any{s.Name: stdioEntry(s)}))
			}},
		{Name: "JetBrains AI Assistant", Docs: "https://www.jetbrains.com/help/ai-assistant/mcp.html",
			Step: func(s agentServerSpec) string {
				return "Settings > Tools > AI Assistant > Model Context Protocol, add as JSON: " + string(mustJSON(map[string]any{"mcpServers": map[string]any{s.Name: stdioEntry(s)}}))
			}},
		{Name: "Crush", Docs: "https://github.com/charmbracelet/crush/blob/main/docs/config/README.md",
			Step: func(s agentServerSpec) string {
				parts := []string{"crush mcp add", shellWord(s.Name), "--type stdio --command", shellWord(s.Command)}
				for _, a := range s.Args {
					parts = append(parts, "--args", shellWord(a))
				}
				return strings.Join(parts, " ")
			}},
		{Name: "Jan", Docs: "https://www.jan.ai/docs/agent/cli",
			Step: func(s agentServerSpec) string {
				parts := []string{"jan cli mcp add", shellWord(s.Name), "--command", shellWord(s.Command)}
				for _, a := range s.Args {
					parts = append(parts, "--arg", shellWord(a))
				}
				return strings.Join(append(parts, "--active"), " ")
			}},
		{Name: "Eclipse (Copilot)", Docs: "https://github.blog/changelog/2025-05-16-agent-mode-and-mcp-support-for-copilot-in-jetbrains-eclipse-and-xcode-now-in-public-preview/",
			Step: func(s agentServerSpec) string {
				return "Preferences > GitHub Copilot > MCP, paste: " + string(mustJSON(map[string]any{"servers": map[string]any{s.Name: typedStdioEntry(s)}}))
			}},
		{Name: "OpenCode", Docs: "https://opencode.ai/docs/mcp-servers/",
			Step: func(s agentServerSpec) string {
				return fmt.Sprintf("opencode mcp add %s --global -- %s", shellWord(s.Name), args(s))
			}},
		{Name: "Qoder, Lingma, CodeBuddy and Trae IDEs", Docs: "https://docs.qoder.com/user-guide/chat/model-context-protocol",
			Step: func(s agentServerSpec) string {
				return "Settings > MCP > Add (manual / raw JSON), paste: " + string(mustJSON(map[string]any{"mcpServers": map[string]any{s.Name: stdioEntry(s)}}))
			}},
		{Name: "Raycast, Msty, Cherry Studio, 5ire", Docs: "https://manual.raycast.com/ai/model-context-protocol",
			Step: func(s agentServerSpec) string {
				return "add a stdio server in the app's MCP settings: command " + shellWord(s.Command) + ", arguments " + shellJoin(s.Args)
			}},
		{Name: "Roo Code", Docs: "https://roocodeinc.github.io/Roo-Code/features/mcp/using-mcp-in-roo",
			Step: func(s agentServerSpec) string {
				return "add to .roo/mcp.json in the project: " + string(mustJSON(map[string]any{"mcpServers": map[string]any{s.Name: stdioEntry(s)}}))
			}},
	}
}

func jsonArray(v []string) string { return string(mustJSON(v)) }

// shellWord makes s one word for a POSIX shell. Words made only of safe
// characters are returned as they are, so typical commands stay readable;
// anything else is wrapped in single quotes with embedded quotes escaped.
func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./:=@%+,", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin quotes each word and joins them with spaces.
func shellJoin(words []string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = shellWord(w)
	}
	return strings.Join(q, " ")
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// clientInstalled reports whether the client appears to be on this machine.
func clientInstalled(c mcpClient) bool {
	if _, err := os.Stat(c.Path); err == nil {
		return true
	}
	if c.OnlyIfExists {
		return false
	}
	dirs := c.DetectDirs
	if len(dirs) == 0 && len(c.DetectBins) == 0 {
		dirs = []string{filepath.Dir(c.Path)}
	}
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return true
		}
	}
	for _, b := range c.DetectBins {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
	}
	return false
}

// mergeClientConfig returns the client's config with RepoCortex registered,
// leaving every other server and setting as it was.
func mergeClientConfig(c mcpClient, existing []byte, spec agentServerSpec) ([]byte, error) {
	switch c.Format {
	case fmtJSONMap:
		return mergeJSONKey(existing, c.Key, spec.Name, c.Entry(spec))
	case fmtCodexTOML:
		return mergeCodexTOML(existing, spec), nil
	case fmtGooseYAML:
		return mergeGooseYAML(existing, spec)
	case fmtContinueYAML:
		return continueYAML(spec)
	}
	return nil, fmt.Errorf("unknown config format")
}

// mergeJSONKey inserts entry under doc[key][name], preserving every other key.
func mergeJSONKey(existing []byte, key, name string, entry map[string]any) ([]byte, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("existing config is not plain JSON (it may contain comments); refusing to rewrite it: %w", err)
		}
	}
	servers, ok := doc[key].(map[string]any)
	if !ok {
		if _, present := doc[key]; present {
			return nil, fmt.Errorf("existing %q is not an object; refusing to rewrite it", key)
		}
		servers = map[string]any{}
	}
	servers[name] = entry
	doc[key] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return append(out, '\n'), nil
}

// mergeCodexTOML writes a [mcp_servers.<name>] table. The rest of the file is
// kept byte for byte: an existing table of the same name is replaced up to the
// next table header, otherwise the table is appended.
func mergeCodexTOML(existing []byte, spec agentServerSpec) []byte {
	quoted := make([]string, len(spec.Args))
	for i, a := range spec.Args {
		quoted[i] = strconv.Quote(a)
	}
	header := "[mcp_servers." + spec.Name + "]"
	block := header + "\n" +
		"command = " + strconv.Quote(spec.Command) + "\n" +
		"args = [" + strings.Join(quoted, ", ") + "]\n"

	lines := strings.SplitAfter(string(existing), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	if start < 0 {
		body := string(existing)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if body != "" {
			body += "\n"
		}
		return []byte(body + block)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	rest := strings.Join(lines[end:], "")
	if rest != "" {
		rest = "\n" + rest
	}
	return []byte(strings.Join(lines[:start], "") + block + rest)
}

// mergeGooseYAML sets extensions.<name> in Goose's config.yaml. The document is
// edited as a YAML node tree, so other keys and comments survive.
func mergeGooseYAML(existing []byte, spec agentServerSpec) ([]byte, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := yaml.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("existing config is not valid YAML; refusing to rewrite it: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("existing config is not a YAML mapping; refusing to rewrite it")
	}
	ext := mappingValue(root, "extensions")
	if ext == nil {
		ext = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content, yamlScalar("extensions"), ext)
	}
	if ext.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("existing extensions is not a mapping; refusing to rewrite it")
	}
	argNodes := make([]*yaml.Node, len(spec.Args))
	for i, a := range spec.Args {
		argNodes[i] = yamlScalar(a)
	}
	entry := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		yamlScalar("type"), yamlScalar("stdio"),
		yamlScalar("name"), yamlScalar(spec.Name),
		yamlScalar("cmd"), yamlScalar(spec.Command),
		yamlScalar("args"), {Kind: yaml.SequenceNode, Content: argNodes},
		yamlScalar("enabled"), {Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
		yamlScalar("timeout"), {Kind: yaml.ScalarNode, Tag: "!!int", Value: "300"},
	}}
	setMappingValue(ext, spec.Name, entry)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// continueYAML is a standalone Continue MCP file. RepoCortex owns this file,
// so it is written whole.
func continueYAML(spec agentServerSpec) ([]byte, error) {
	doc := map[string]any{
		"name":    "RepoCortex",
		"version": "0.0.1",
		"schema":  "v1",
		"mcpServers": []map[string]any{{
			"name":    spec.Name,
			"type":    "stdio",
			"command": spec.Command,
			"args":    spec.Args,
		}},
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

func yamlScalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMappingValue replaces or appends key in a mapping node.
func setMappingValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, yamlScalar(key), v)
}
