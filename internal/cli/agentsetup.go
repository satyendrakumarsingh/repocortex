package cli

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// ---------------------------------------------------------------- guidance
//
// A PERFECT INDEX THE AGENT NEVER QUERIES IS WORTH NOTHING.
//
// RepoCortex's MCP surface is deliberately small — see profileTools in
// internal/mcp/server.go, which argues that every exposed schema is injected
// into the agent's context on every request, so a wide surface spends the very
// context this product exists to save. That argument cuts both ways: a narrow
// surface only pays off if the agent knows WHEN to reach for the few tools
// there are. Tool descriptions can say what a tool does; they cannot say
// "before you edit a symbol, do this first", because they are only read once
// the agent has already decided to look at the tool list.
//
// This block is where that lives, and it is text rather than a tool on purpose:
// it costs a fixed handful of lines in project memory, once, instead of another
// schema on every request. Nothing here adds surface — every rule below names a
// tool that already exists.
//
// It is SHORT and DECISION-SHAPED because an agent skims. Each line is a
// trigger followed by an action, and each is grounded in behaviour the server
// actually has:
//
//   - get_task_context's own description opens "CALL THIS FIRST".
//   - impact_analysis and find_tests appear only in the 'standard' and 'full'
//     profiles, so the blast-radius rule names find_callers as the fallback
//     rather than an unavailable tool.
//   - the confidence vocabulary is find_references' own: exact | high | medium
//     | low.
//   - the empty-result rule is relationshipQuery's LOWER BOUND note.
//   - "COVERAGE TOO LOW" and "COVERAGE UNKNOWN" are the literal strings emitted
//     by coverage.caveat in internal/mcp/tools.go.
//   - "No index has been built" is the guide() text from server.go's noRun, and
//     the isError policy there depends on the agent treating it as a state of
//     the world rather than a broken toolset. The rule says so explicitly.

// agentGuidanceOpen and agentGuidanceClose delimit the block wherever it is
// pasted.
//
// HTML comments because the destination is Markdown — CLAUDE.md, AGENTS.md, a
// skill body — and a marker that renders as visible junk gets deleted by the
// first person who tidies the file. They also give --check something exact to
// look for, so "is the guidance wired up?" is answered by a substring match
// rather than by guessing at prose.
const (
	agentGuidanceOpen  = "<!-- cortex:guidance -->"
	agentGuidanceClose = "<!-- /cortex:guidance -->"
)

// agentGuidance is the canonical block. It is duplicated verbatim in
// .claude-plugin/skills/cortex/SKILL.md, and TestAgentGuidanceMatchesSkill
// fails if the two ever drift.
const agentGuidance = agentGuidanceOpen + `
## RepoCortex

RepoCortex serves a resolved index of this repository over MCP. For questions
about code that already exists, prefer it to grep and to opening files.

- Before changing code, call ` + "`get_task_context`" + ` with the task in plain English.
  It returns the files, symbols, tests and tables the change touches, found by
  following the call graph. One call instead of a dozen exploratory ones.
- Before editing a symbol, ask for its blast radius: ` + "`impact_analysis`" + ` where the
  profile exposes it, otherwise ` + "`find_callers`" + `. Read it before the edit, not
  after a test fails.
- Before changing behaviour, ` + "`find_tests`" + ` says what to run and which signal
  found each test. A proven call edge outranks a matching filename.
- Every result carries a confidence. ` + "`exact`" + ` means a compiler proved it;
  ` + "`high`" + `, ` + "`medium`" + ` and ` + "`low`" + ` were inferred and may be wrong. Treat anything
  below ` + "`exact`" + ` as a lead to verify, never as a fact to act on.
- An empty relationship result means UNRESOLVED, not absent. "No callers" is not
  proof a symbol is unused; do not delete or narrow one on that evidence.
- A ` + "`COVERAGE TOO LOW`" + ` or ` + "`COVERAGE UNKNOWN`" + ` note outranks the list beneath it.
  Run ` + "`cortex resolve`" + ` and ask again rather than reporting that list.
- "No index has been built" is a missing index, not a broken tool. Run
  ` + "`cortex index`" + `, then ` + "`cortex resolve`" + `, and ask again.
- ` + "`find_symbol`" + ` matches whole names only; use ` + "`search_code`" + ` for partial ones.
  Generated, oversized and excluded files are not indexed, so absence from the
  index is not absence from the repository.
- The index is a snapshot, not your working tree. Read files you have just
  edited directly, or keep the index current with ` + "`cortex watch`" + `.
` + agentGuidanceClose + "\n"

// agentMCPName is the server name RepoCortex registers under.
const agentMCPName = "cortex"

// pluginManifestPath is where a Claude Code plugin manifest lives, relative to
// the plugin root. Verified against the plugin reference documentation.
const pluginManifestPath = ".claude-plugin/plugin.json"

// maxGuidanceScanBytes bounds how much of a candidate memory file --check will
// read looking for the marker.
//
// CLAUDE.md is meant to be short, but --check must not become a way to load an
// arbitrary file into memory because someone left a log at that path. A marker
// that is not in the first quarter-megabyte is not doing its job anyway.
const maxGuidanceScanBytes = 256 << 10

// agentMemoryFiles are the project-memory files a coding agent reads on its
// own, in the order --check reports them.
//
// Both, not one: Claude Code reads CLAUDE.md and several other tools read
// AGENTS.md, and a user who wired up the other one has wired it up. Reporting
// only the file RepoCortex happens to prefer would tell him to do work he has
// already done.
var agentMemoryFiles = []string{"CLAUDE.md", "AGENTS.md"}

// ---------------------------------------------------------------- command

func newAgentCmd(g *Global) *cobra.Command {
	var (
		check    bool
		guidance bool
		write    string
		force    bool
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Print the MCP registration and guidance an AI agent needs",
		Long: `Prints the two things that make RepoCortex usable by an AI coding agent: the
MCP registration, and the guidance that tells the agent when to reach for it.

The second is the one that is usually missing. An agent with the tools wired up
but no guidance still greps, because nothing told it that a blast radius is
available before it edits a symbol, or that an empty caller list means
"unresolved" rather than "unused".

  cortex agent                     print the registration and the guidance
  cortex agent --guidance          print the guidance block alone, to pipe
  cortex agent --check             report what is and is not wired up
  cortex agent --write PATH        write the guidance block to a new file

This command does not touch your configuration. Wiring an agent into your
machine is your decision, so the default is to print and let you place the
output; --write creates a file you name, and refuses to replace one that
already exists unless you pass --force, which says what it replaced.

To add the guidance to project memory you already have, append it:

    cortex agent --guidance >> CLAUDE.md

--check reads only. It never reports a missing piece as a failure — an
un-integrated project is the normal starting state, not a malfunction — so it
exits zero unless something is genuinely broken.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case check && (guidance || write != ""):
				return errors.New("--check only reports; it cannot be combined with --guidance or --write")
			case guidance && write != "":
				return errors.New("--guidance prints and --write writes: choose one")
			case force && write == "":
				return errors.New("--force only means something with --write")
			}

			switch {
			case check:
				return runAgentCheck(cmd, g)
			case guidance:
				return runAgentGuidance(cmd, g)
			case write != "":
				return runAgentWrite(cmd, write, force)
			default:
				return runAgentPrint(cmd, g)
			}
		},
	}

	f := cmd.Flags()
	f.BoolVar(&check, "check", false, "report which parts of the agent integration are wired up")
	f.BoolVar(&guidance, "guidance", false, "print only the guidance block, for appending to CLAUDE.md or AGENTS.md")
	f.StringVar(&write, "write", "", "write the guidance block to this path (refuses to replace an existing file)")
	f.BoolVar(&force, "force", false, "with --write, replace an existing file and say so")
	return cmd
}

// ---------------------------------------------------------------- print

// agentIntegration is the printable form of everything a client needs.
type agentIntegration struct {
	Project string `json:"project"`
	Root    string `json:"root"`

	Server agentServerSpec `json:"mcp_server"`

	// ClaudeCode and PluginDir are the two ways to install the same server.
	// Both are given because they suit different users: one line to try it,
	// a plugin to ship it to a team along with the guidance.
	ClaudeCode string `json:"claude_code_command"`
	PluginDir  string `json:"claude_code_plugin_dir"`

	Profile string   `json:"mcp_profile"`
	Tools   []string `json:"mcp_tools,omitempty"`

	Guidance string `json:"guidance"`
}

// agentServerSpec is the stdio launch RepoCortex needs, in the shape every MCP
// client asks for it.
type agentServerSpec struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func runAgentPrint(cmd *cobra.Command, g *Global) error {
	cfg := g.Config()
	spec := agentServerSpec{
		Name:    agentMCPName,
		Command: "cortex",
		// --project is explicit rather than relying on the client's working
		// directory. An MCP client starts the server from wherever it happens
		// to be, and a server that silently indexes the wrong repository is
		// worse than one that fails to start.
		Args: []string{"--project", cfg.RepoRoot, "serve"},
	}

	integ := agentIntegration{
		Project:    cfg.ProjectName,
		Root:       cfg.RepoRoot,
		Server:     spec,
		ClaudeCode: fmt.Sprintf("claude mcp add %s -- %s %s", spec.Name, spec.Command, strings.Join(spec.Args, " ")),
		PluginDir:  fmt.Sprintf("claude --plugin-dir %s", cfg.RepoRoot),
		Profile:    string(cfg.MCPProfile),
		Tools:      agentToolNames(cmd.Context(), g),
		Guidance:   agentGuidance,
	}

	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), integ)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "RepoCortex agent integration for %q\n\n", integ.Project)

	_, _ = fmt.Fprintf(out, "1. Register the MCP server.\n\n")
	_, _ = fmt.Fprintf(out, "   Claude Code, one line:\n\n     %s\n\n", integ.ClaudeCode)
	_, _ = fmt.Fprintf(out, "   Claude Code, as a plugin — same server, plus the guidance below:\n\n     %s\n\n", integ.PluginDir)
	_, _ = fmt.Fprintf(out, "   Any other MCP client, over stdio:\n\n")
	_, _ = fmt.Fprintf(out, "     command  %s\n", spec.Command)
	_, _ = fmt.Fprintf(out, "     args     %s\n", strings.Join(spec.Args, " "))
	_, _ = fmt.Fprintf(out, "     transport stdio\n\n")

	// State the profile and its tools here rather than in the guidance block.
	// The block is portable text pasted into a memory file that outlives this
	// configuration; a tool list printed into it would be stale the moment
	// mcp.profile changes.
	if len(integ.Tools) > 0 {
		_, _ = fmt.Fprintf(out, "   Profile %q exposes %d tool(s): %s\n\n",
			integ.Profile, len(integ.Tools), strings.Join(integ.Tools, ", "))
	} else {
		_, _ = fmt.Fprintf(out, "   Profile %q. Build the index to see which tools it exposes.\n\n", integ.Profile)
	}

	_, _ = fmt.Fprintf(out, "2. Give the agent the guidance below — this is the part that decides\n")
	_, _ = fmt.Fprintf(out, "   whether it uses the index or greps anyway. Append it to project memory:\n\n")
	_, _ = fmt.Fprintf(out, "     cortex agent --guidance >> CLAUDE.md\n\n")
	_, _ = fmt.Fprintf(out, "3. Confirm it landed:\n\n     cortex agent --check\n\n")
	_, _ = fmt.Fprintf(out, "%s", agentGuidance)
	return nil
}

func runAgentGuidance(cmd *cobra.Command, g *Global) error {
	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), map[string]string{"guidance": agentGuidance})
	}
	// No banner, no trailing advice: this output is meant to be appended to a
	// Markdown file, and anything that is not the block itself becomes litter
	// in someone's CLAUDE.md.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s", agentGuidance)
	return nil
}

// agentToolNames reports the tools the active profile would expose, or nil when
// that cannot be determined.
//
// Best-effort by design: an index that is absent or unreadable is exactly the
// state this command exists to help a user out of, so failing to print the
// registration because the tool list was unavailable would withhold the useful
// half of the answer over the decorative half.
func agentToolNames(ctx stdctx.Context, g *Global) []string {
	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()

	srv, err := devmcp.New(devmcp.Deps{
		Registry: registry.New(db),
		Graph:    graph.New(db),
		Config:   g.Config(),
	})
	if err != nil {
		return nil
	}
	return srv.Tools()
}

// ---------------------------------------------------------------- write

func runAgentWrite(cmd *cobra.Command, path string, force bool) error {
	// NEVER SILENTLY. Overwriting a file the user wrote is a decision only he
	// can make, so the default refusal names the append that is almost always
	// what he meant, and --force announces the loss on stderr before causing
	// it rather than after.
	switch _, err := os.Stat(path); {
	case err == nil && !force:
		return fmt.Errorf("%s already exists and RepoCortex will not overwrite it — "+
			"append instead with 'cortex agent --guidance >> %s', or pass --force to replace it",
			path, path)
	case err == nil:
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"replacing %s — its previous contents are discarded\n", path)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("agent: inspect %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("agent: create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(agentGuidance), 0o600); err != nil {
		return fmt.Errorf("agent: write %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d bytes)\n", path, len(agentGuidance))
	return nil
}

// ---------------------------------------------------------------- check

// runAgentCheck reports the state of the integration.
//
// NOTHING MISSING IS A FAILURE HERE. A project that has not been wired up yet
// is the normal case — it is why the command exists — and renderChecks turns
// "fail" into a non-zero exit, which would make `cortex agent --check` in a
// CI job or a setup script look like a broken tool rather than a to-do list.
// The same reasoning as internal/mcp's isError policy: reserve the alarm for
// something the reader cannot fix by following the instruction. Only a genuinely
// unreadable index is "fail".
func runAgentCheck(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()
	root := g.Config().RepoRoot

	checks := agentIndexChecks(ctx, g)
	wiring, err := agentWiringChecks(ctx, root)
	if err != nil {
		return err
	}
	return renderChecks(cmd, g, append(checks, wiring...), nil)
}

func agentIndexChecks(ctx stdctx.Context, g *Global) []check {
	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return []check{{
			Name:   "index",
			Status: "warn",
			Detail: "absent at " + dbPath,
			Impact: "every tool answers \"no index has been built\", so the agent gets nothing and falls back to grep",
			Remedy: "cortex init && cortex index && cortex resolve",
		}}
	}

	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		// The one genuine malfunction this command can meet: the index is
		// there and cannot be read. Distinct from "absent", because the remedy
		// is different and because a broken store must not be reported as a
		// setup step nobody has got to yet.
		return []check{{
			Name:   "index",
			Status: "fail",
			Detail: err.Error(),
			Impact: "RepoCortex cannot serve this project at all",
			// The store's own error names its fix — a stale schema says
			// "run 'cortex index'". Repeating a generic remedy over the
			// specific one would send the reader past the useful sentence.
			Remedy: "do what the message above says; if it persists, 'cortex doctor --verify-index'",
		}}
	}
	defer func() { _ = db.Close() }()

	reg := registry.New(db)
	run, err := reg.CurrentRun(ctx)
	if err != nil {
		if errors.Is(err, registry.ErrNoCommittedRun) {
			return []check{{
				Name:   "index",
				Status: "warn",
				Detail: "initialised but never indexed",
				Impact: "there is a database and no content in it; every query returns nothing",
				Remedy: "cortex index && cortex resolve",
			}}
		}
		return []check{{
			Name:   "index",
			Status: "fail",
			Detail: err.Error(),
			Impact: "the committed revision could not be read",
			Remedy: "cortex doctor --verify-index",
		}}
	}

	out := make([]check, 0, 3)

	counts, err := reg.Counts(ctx, run.ID)
	switch {
	case err != nil:
		out = append(out, check{
			Name: "index", Status: "fail", Detail: err.Error(),
			Impact: "index contents could not be counted",
			Remedy: "cortex doctor --verify-index",
		})
	case counts.Symbols == 0:
		out = append(out, check{
			Name: "index", Status: "warn",
			Detail: fmt.Sprintf("revision %d is committed and empty", run.ID),
			Impact: "no symbols were recorded, so every query returns nothing",
			Remedy: "cortex index --full, and check that this project's languages are supported",
		})
	default:
		out = append(out, check{
			Name: "index", Status: "ok",
			Detail: fmt.Sprintf("revision %d — %d files, %d symbols, %d relationships",
				run.ID, counts.Files, counts.Symbols, counts.Edges),
		})
	}

	// Resolution is reported separately from indexing because they fail
	// separately and mean different things to an agent. An indexed, unresolved
	// project answers "where is this symbol" perfectly and answers "what calls
	// it" with silence — and silence is the answer most likely to be mistaken
	// for a fact.
	if rs, err := reg.ResolutionStats(ctx, run.ID); err == nil {
		switch {
		case rs.Total == 0:
			out = append(out, check{
				Name: "resolution", Status: "warn",
				Detail: "no references recorded",
				Impact: "the call graph is empty, so callers, impact and tests answer nothing",
				Remedy: "cortex index && cortex resolve",
			})
		case rs.Pending == rs.Total:
			out = append(out, check{
				Name: "resolution", Status: "warn",
				Detail: fmt.Sprintf("%d reference(s) extracted, none resolved", rs.Total),
				Impact: "find_callers and find_references return nothing until the graph is built",
				Remedy: "cortex resolve",
			})
		// BindingRate against capability.CoverageFloor, the same rate and the
		// same floor the MCP tools gate on. This check exists to predict what
		// those tools will say, so measuring it differently made it wrong
		// exactly when it mattered. Unresolved is named in the detail because
		// the rate's denominator excludes it.
		case rs.BindingRate() < capability.CoverageFloor:
			out = append(out, check{
				Name: "resolution", Status: "warn",
				Detail: fmt.Sprintf("%.0f%% of the %d references that name something this index defines are bound (%d matched nothing here)",
					rs.BindingRate()*100, rs.Bindable(), rs.Unresolved),
				Impact: "relationship answers carry a COVERAGE TOO LOW caveat and should not be reported as answers",
				Remedy: "cortex resolve, or import a compiler index with 'cortex scip import'",
			})
		default:
			out = append(out, check{
				Name: "resolution", Status: "ok",
				Detail: fmt.Sprintf("%.0f%% of the %d references that name something this index defines are bound (%d matched nothing here)",
					rs.BindingRate()*100, rs.Bindable(), rs.Unresolved),
			})
		}
	}

	if srv, err := devmcp.New(devmcp.Deps{
		Registry: reg,
		Graph:    graph.New(db),
		Config:   g.Config(),
	}); err == nil {
		tools := srv.Tools()
		c := check{
			Name:   "mcp tools",
			Status: "ok",
			Detail: fmt.Sprintf("profile %q exposes %d: %s",
				g.Config().MCPProfile, len(tools), strings.Join(tools, ", ")),
		}
		// Not a warning. A small surface is the intended configuration, not a
		// deficiency — see profileTools in internal/mcp/server.go. The line
		// exists so a reader who wants the blast-radius tools knows the one
		// setting that adds them, without being told his default is wrong.
		if !slices.Contains(tools, "impact_analysis") {
			c.Impact = "impact_analysis and find_tests are not exposed on this profile; the guidance falls back to find_callers"
			c.Remedy = "set mcp.profile: standard in .cortex.yml (or .cortex.yml) to add them"
		}
		out = append(out, c)
	}
	return out
}

// agentWiringChecks reports the parts of the integration that live in files
// rather than in the index.
//
// Read-only, and it says so: this command's whole contract is that it does not
// modify a user's configuration, and a --check that quietly created what it
// found missing would break that in the one mode a user runs without thinking.
func agentWiringChecks(ctx stdctx.Context, root string) ([]check, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]check, 0, 3)

	// Project-scope MCP registration. User- and local-scope registrations live
	// outside the repository, so a miss here is reported as "not visible from
	// this repository" rather than "not registered" — claiming the latter
	// would be wrong for every user who registered it globally.
	mcpPath := filepath.Join(root, ".mcp.json")
	switch registered, err := mcpJSONNames(mcpPath); {
	case err != nil:
		out = append(out, check{
			Name: "mcp registration", Status: "warn",
			Detail: fmt.Sprintf(".mcp.json present but unreadable: %v", err),
			Impact: "whether this project registers RepoCortex could not be determined",
			Remedy: "check .mcp.json is valid JSON",
		})
	case slices.Contains(registered, "cortex") || slices.Contains(registered, "repocortex"):
		matched := agentMCPName
		for _, name := range []string{"cortex", "repocortex"} {
			if slices.Contains(registered, name) {
				matched = name
				break
			}
		}
		out = append(out, check{
			Name: "mcp registration", Status: "ok",
			Detail: fmt.Sprintf("%q in .mcp.json (project scope)", matched),
		})
	default:
		out = append(out, check{
			Name: "mcp registration", Status: "warn",
			Detail: "not in this repository's .mcp.json",
			Impact: "no project-scope registration; a user- or local-scope one is not visible from here",
			Remedy: "cortex agent, and run the 'claude mcp add' line it prints",
		})
	}

	// Guidance in project memory.
	found, err := guidanceFiles(root)
	if err != nil {
		return nil, err
	}
	if len(found) > 0 {
		out = append(out, check{
			Name: "guidance", Status: "ok",
			Detail: fmt.Sprintf("present in %s", strings.Join(found, ", ")),
		})
	} else {
		out = append(out, check{
			Name: "guidance", Status: "warn",
			Detail: "not found in " + strings.Join(agentMemoryFiles, " or "),
			Impact: "the agent has the tools but no rule telling it when to use them, so it will keep grepping",
			Remedy: "cortex agent --guidance >> CLAUDE.md",
		})
	}

	// Plugin manifest.
	manifest := filepath.Join(root, filepath.FromSlash(pluginManifestPath))
	if _, err := os.Stat(manifest); err == nil {
		out = append(out, check{
			Name: "plugin manifest", Status: "ok",
			Detail: pluginManifestPath + " present — installable with 'claude --plugin-dir " + root + "'",
		})
	} else {
		out = append(out, check{
			Name: "plugin manifest", Status: "warn",
			Detail: pluginManifestPath + " absent",
			Impact: "this repository cannot be installed as a Claude Code plugin; the one-line registration still works",
			Remedy: "not required — see docs/agent-integration.md",
		})
	}
	return out, nil
}

// guidanceFiles reports which project-memory files carry the guidance marker.
func guidanceFiles(root string) ([]string, error) {
	var found []string
	for _, name := range agentMemoryFiles {
		ok, err := fileHasMarker(filepath.Join(root, name), agentGuidanceOpen)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, name)
		}
	}
	return found, nil
}

// fileHasMarker reports whether the first maxGuidanceScanBytes of a file
// contain a marker. A missing file is not an error: not having a CLAUDE.md is
// the ordinary state of most repositories.
func fileHasMarker(path, marker string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // path is root plus a fixed filename
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("agent: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	buf, err := io.ReadAll(io.LimitReader(f, maxGuidanceScanBytes))
	if err != nil {
		return false, fmt.Errorf("agent: read %s: %w", path, err)
	}
	content := string(buf)
	return strings.Contains(content, marker), nil
}

// mcpJSONNames reports the server names declared in an MCP client config.
//
// An absent file yields no names and no error — most repositories have none,
// and that is a state of the world rather than a problem. Only a file that
// exists and cannot be parsed is worth reporting, because that one silently
// registers nothing while looking like it does.
func mcpJSONNames(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is root plus a fixed filename
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.NewDecoder(io.LimitReader(f, maxGuidanceScanBytes)).Decode(&doc); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	return names, nil
}
