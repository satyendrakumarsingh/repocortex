# Coding agents

RepoCortex is an MCP server that speaks stdio: `cortex --project <repo> serve`.
Any agent that can launch a local MCP server can use it. This page lists every
agent we know of, how RepoCortex reaches it, and what is missing for the few that
cannot use it yet.

| Section | What is in it |
|---|---|
| [Configured automatically](#configured-automatically) | 30 agents that `cortex install` writes the config for |
| [Set up by hand](#set-up-by-hand) | Agents configured through their own command or settings screen |
| [Agents without local MCP](#agents-without-local-mcp) | Aider, Plandex, ChatGPT, Android Studio, and how each is served |
| [Guidance files](#guidance-files) | What `--rules` writes so the agent reaches for the graph first |
| [How `cortex install` edits config files](#how-cortex-install-edits-config-files) | The merge rules that keep your other servers safe |
| [Adding an agent](#adding-an-agent) | How to add support for one we have missed |

## Configured automatically

```bash
cortex install --list      # every supported agent, which are detected here, and the file each uses
cortex install --dry-run   # what would change, without writing
cortex install --rules     # write the configs and the guidance files
```

An agent is detected when its config file, its config directory or its command
exists. `--create` writes configs for agents that are not detected. Paths below
are for macOS and Linux; `~/.config` follows `XDG_CONFIG_HOME`, and Windows uses
`%APPDATA%` where the agent does.

| Agent | Kind | Config file | Format |
|---|---|---|---|
| [Amp](https://ampcode.com/docs/customize/mcp) | CLI, editor | `~/.config/amp/settings.json` | JSON, key `amp.mcpServers` |
| [Antigravity](https://antigravity.google/docs/mcp) | Desktop | `~/.gemini/config/mcp_config.json` (and the earlier `~/.gemini/antigravity/mcp_config.json` if present) | JSON, `mcpServers` |
| [Augment (Auggie)](https://docs.augmentcode.com/cli/integrations) | CLI | `~/.augment/settings.json` | JSON, `mcpServers` |
| [Claude Code](https://code.claude.com/docs/en/mcp) | CLI | `~/.claude.json` | JSON, `mcpServers` |
| [Claude Desktop](https://modelcontextprotocol.io/docs/develop/connect-local-servers) | Desktop | `claude_desktop_config.json` in the app's support folder | JSON, `mcpServers` |
| [Cline CLI](https://docs.cline.bot/mcp/configuring-mcp-servers) | CLI | `~/.cline/data/settings/cline_mcp_settings.json` | JSON, `mcpServers` |
| [CodeBuddy CLI](https://www.workbuddy.ai/docs/cli/mcp) | CLI | `~/.codebuddy/mcp.json` | JSON, `mcpServers` |
| [Codebuff](https://www.codebuff.com/docs/tips/mcp-servers) | CLI | `~/.agents/mcp.json` | JSON, `mcpServers` |
| [Codex CLI](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) | CLI, desktop, IDE | `~/.codex/config.toml` (follows `CODEX_HOME`) | TOML, `[mcp_servers.cortex]` |
| [Continue](https://docs.continue.dev/customize/deep-dives/mcp) | IDE, CLI | `.continue/mcpServers/cortex.yaml` in the project | YAML block file |
| [Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers) | CLI | `~/.copilot/mcp-config.json` | JSON, `mcpServers`, `type: local` |
| [Cursor](https://cursor.com/docs/context/mcp) | Desktop, CLI | `~/.cursor/mcp.json` | JSON, `mcpServers` |
| [Devin Desktop](https://docs.devin.ai/windsurf/plugins/cascade/mcp.md) | Desktop | `~/.config/devin/mcp_config.json` | JSON, `mcpServers` |
| [Factory Droid](https://docs.factory.com/cli/configuration/mcp) | CLI | `~/.factory/mcp.json` | JSON, `mcpServers` |
| [Gemini CLI](https://geminicli.com/docs/tools/mcp-server/) | CLI | `~/.gemini/settings.json` | JSON, `mcpServers` |
| [Goose](https://goose-docs.ai/docs/guides/config-files) | CLI, desktop | `~/.config/goose/config.yaml` | YAML, `extensions.cortex` |
| [Junie](https://junie.jetbrains.com/docs/junie-cli-mcp-configuration.html) | CLI, JetBrains IDEs | `~/.junie/mcp/mcp.json` | JSON, `mcpServers` |
| [Kilo Code](https://kilo.ai/docs/features/mcp/using-mcp-in-kilo-code) | CLI, VS Code | `~/.config/kilo/kilo.json` | JSON, key `mcp`, `type: local` |
| [Kimi Code CLI](https://www.kimi.com/code/docs/en/kimi-code-cli/customization/mcp.html) | CLI | `~/.kimi-code/mcp.json` (follows `KIMI_CODE_HOME`) | JSON, `mcpServers` |
| [Kiro](https://kiro.dev/docs/mcp/configuration/) | Desktop, CLI | `~/.kiro/settings/mcp.json` | JSON, `mcpServers` |
| [LM Studio](https://lmstudio.ai/docs/app/mcp) | Desktop | `~/.lmstudio/mcp.json` | JSON, `mcpServers` |
| [mcphub.nvim](https://ravitemer.github.io/mcphub.nvim/mcp/servers_json.html) | Neovim (avante, CodeCompanion) | `~/.config/mcphub/servers.json` | JSON, `mcpServers` |
| [Qoder CLI](https://docs.qoder.com/cli/mcp-servers) | CLI | `~/.qoder/settings.json` | JSON, `mcpServers` |
| [Qwen Code](https://qwenlm.github.io/qwen-code-docs/en/users/features/mcp/) | CLI | `~/.qwen/settings.json` | JSON, `mcpServers` |
| [Tabnine CLI](https://docs.tabnine.com/main/getting-started/tabnine-cli/features/mcp-server-config) | CLI | `~/.tabnine/agent/settings.json` | JSON, `mcpServers` |
| [Visual Studio](https://learn.microsoft.com/en-us/visualstudio/ide/mcp-servers) | Windows IDE | `%USERPROFILE%\.mcp.json` | JSON, key `servers` |
| [VS Code (Copilot agent mode)](https://code.visualstudio.com/docs/agents/reference/mcp-configuration) | Editor | `Code/User/mcp.json` in the user settings folder | JSON, key `servers`, `type: stdio` |
| [Warp](https://docs.warp.dev/agent-platform/warp-agents/agent-context/mcp) | Terminal | `~/.warp/.mcp.json` | JSON, `mcpServers` |
| [Windsurf and Windsurf Next](https://docs.devin.ai/windsurf/plugins/cascade/mcp.md) | Desktop | `~/.codeium/windsurf/mcp_config.json`, `~/.codeium/windsurf-next/mcp_config.json` | JSON, `mcpServers` |
| [Zed](https://zed.dev/docs/ai/mcp) | Editor (macOS, Linux) | `~/.config/zed/settings.json` | JSON, key `context_servers` |

Restart the agent after `cortex install` so it reloads its MCP servers.

## Set up by hand

These agents keep MCP servers in a settings screen, a database or a per-project
file, or add them through their own command. `cortex install --list` prints each
step with your project path already filled in.

| Agent | Step |
|---|---|
| [Amazon Q Developer CLI](https://docs.aws.amazon.com/amazonq/latest/qdeveloper-ug/command-line-mcp-config-CLI.html) | `q mcp add --name cortex --command cortex --args '["--project","/path/to/repo","serve"]'` |
| [Cline (VS Code extension)](https://docs.cline.bot/mcp/configuring-mcp-servers) | MCP Servers, Configure, then add the `cortex` entry under `mcpServers` |
| [Crush](https://github.com/charmbracelet/crush/blob/main/docs/config/README.md) | `crush mcp add cortex --type stdio --command cortex --args --project --args /path/to/repo --args serve` |
| [Eclipse (Copilot)](https://github.blog/changelog/2025-05-16-agent-mode-and-mcp-support-for-copilot-in-jetbrains-eclipse-and-xcode-now-in-public-preview/) | Preferences, GitHub Copilot, MCP, then paste the `servers` entry |
| [Jan](https://www.jan.ai/docs/agent/cli) | `jan cli mcp add cortex --command cortex --arg --project --arg /path/to/repo --arg serve --active` |
| [JetBrains AI Assistant](https://www.jetbrains.com/help/ai-assistant/mcp.html) | Settings, Tools, AI Assistant, Model Context Protocol, then add as JSON |
| [OpenCode](https://opencode.ai/docs/mcp-servers/) | `opencode mcp add cortex --global -- cortex --project /path/to/repo serve` |
| [Qoder, Lingma, CodeBuddy and Trae IDEs](https://docs.qoder.com/user-guide/chat/model-context-protocol) | Settings, MCP, Add, then paste the `mcpServers` entry |
| [Raycast, Msty, Cherry Studio, 5ire](https://manual.raycast.com/ai/model-context-protocol) | Add a stdio server: command `cortex`, arguments `--project /path/to/repo serve` |
| [Roo Code](https://roocodeinc.github.io/Roo-Code/features/mcp/using-mcp-in-roo) | Add the `cortex` entry to `.roo/mcp.json` in the project |
| Any other MCP client | Add the JSON below to its configuration |

```json
{
  "mcpServers": {
    "cortex": { "command": "cortex", "args": ["--project", "/absolute/path/to/repo", "serve"] }
  }
}
```

## Agents without local MCP

| Agent | What it supports | How RepoCortex serves it today | What would add full support |
|---|---|---|---|
| [Aider](https://aider.chat/docs/usage/conventions.html) | No MCP. Reads a conventions file and runs shell commands. | `cortex install --rules` writes a `CONVENTIONS.md` block that tells Aider to run `cortex context`, `cortex callers` and `cortex impact`. Load it with `aider --read CONVENTIONS.md`. | Nothing in RepoCortex. Aider gains MCP or keeps using the CLI. |
| [Plandex](https://docs.plandex.ai/) | No MCP. Loads files and command output into context. | Load command output: `cortex context "<task>" > ctx.md`, then `plandex load ctx.md`. | Nothing in RepoCortex. |
| [ChatGPT (developer mode)](https://platform.openai.com/docs/guides/developer-mode) | Remote MCP over HTTPS only. | Not reachable: `cortex serve` speaks stdio. | A Streamable HTTP transport in `cortex serve` (see [Roadmap](../ROADMAP.md)), exposed through a tunnel you control. |
| [Android Studio (Gemini)](https://developer.android.com/studio/gemini/add-mcp-server) | Remote MCP over HTTP only. | Not reachable: `cortex serve` speaks stdio. | The same Streamable HTTP transport, bound to `127.0.0.1` with a token. |

Every agent can also use the CLI directly. `cortex context`, `cortex callers`,
`cortex impact` and `cortex tests` print plain text an agent can read from any
shell tool.

## Guidance files

An MCP server alone does not make an agent use it; the agent also needs to know
*when* to ask. `cortex install --rules` adds a short, marked block:

| File | When it is written |
|---|---|
| `AGENTS.md` | Always; created if missing. Read by Codex, Copilot, Cursor, Amp, Jules, Factory, Kilo, OpenCode, Zed, Warp, Devin and others. |
| `CLAUDE.md`, `GEMINI.md`, `QWEN.md`, `CODEBUDDY.md`, `WARP.md` | Updated only if the file already exists |
| `.cursorrules`, `.windsurfrules`, `.clinerules`, `.roorules`, `.github/copilot-instructions.md` | Updated only if the file already exists |
| `CONVENTIONS.md` | For Aider: created when `aider` is installed, otherwise updated if it exists. Holds CLI commands, not MCP tools. |

Running it again replaces the block in place; nothing else in the file changes.

## How `cortex install` edits config files

- It adds or replaces one entry named `cortex` and keeps every other server and
  setting in the file.
- Each agent gets the shape its own documentation specifies. The JSON key,
  `type` field and argument layout differ between agents, and the table above
  records which one each uses.
- JSON is parsed before it is written. A file with comments (JSONC) or an
  unexpected shape is left untouched and reported as `manual`, with the entry to
  paste.
- Codex TOML is edited as text: only the `[mcp_servers.cortex]` table is
  replaced or appended, so comments and ordering elsewhere survive.
- Goose YAML is edited through a node tree, which keeps comments and the other
  extensions.
- A second run with no changes reports `unchanged` and writes nothing.

## Adding an agent

An agent that reads a JSON file with an `mcpServers` map needs one entry in
`mcpClients` in [`internal/cli/clients.go`](../../internal/cli/clients.go): its
name, file path, key, how to detect it, and a link to its MCP documentation.
`TestEverySupportedClientHasDocs` fails if the link is missing. Agents with
another format reuse an existing writer (`fmtCodexTOML`, `fmtGooseYAML`,
`fmtContinueYAML`) or add one with a test that proves it keeps the rest of the
file. An agent configured only through its UI goes in `manualClients` with the
exact step to follow.
