package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"

	"github.com/spf13/cobra"
)

// errToolReportedFailure is returned when a tool answered with its failure
// flag set, so `repocortex tool call` exits non-zero and a script can branch on it.
var errToolReportedFailure = errors.New("the tool reported a failure")

// newToolCmd is the shell front end to the agent tools.
//
// The other commands in this package are written for a person at a terminal:
// tables, colour-free prose, flags per question. This one is written for an
// AGENT that has a shell and nothing else (Aider, a CI job, a model given only
// a command runner). It exposes exactly the tools an MCP client sees, by the
// same names, taking the same arguments and returning the same text, because
// it calls them through the same in-process path the REST endpoint uses.
//
// It defaults to the `full` profile, unlike `repocortex serve`. A profile exists to
// keep tool schemas out of a model's context, and a shell pays nothing for a
// tool it does not invoke.
func newToolCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tool",
		Short: "Call the agent tools from the shell, exactly as an MCP client would",
		Long: `Lists, describes and calls the tools RepoCortex serves to AI agents.

Every tool an MCP client can call is callable here by the same name, with the
same arguments, returning the same text:

    repocortex tool list
    repocortex tool describe find_callers
    repocortex tool call find_callers name=ParseConfig min_confidence=high
    repocortex tool call get_task_context --args '{"task":"add retry to the webhook sender"}'

Use it from an agent that has a shell but no MCP client, from a script, or to
see precisely what an agent was shown. The same tools are served over MCP by
'repocortex serve' and over plain HTTP by 'repocortex serve --http' at /api/v1.`,
	}
	cmd.AddCommand(newToolListCmd(g), newToolDescribeCmd(g), newToolCallCmd(g))
	return cmd
}

// toolProfileFlag adds --profile and returns a reader for it.
func toolProfileFlag(cmd *cobra.Command) *string {
	var profile string
	cmd.Flags().StringVar(&profile, "profile", string(config.MCPFull),
		"tool profile to use: minimal, standard or full")
	return &profile
}

func newToolListCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the tools a profile exposes and what each costs an agent's context",
		Long: `Lists every tool the chosen profile exposes.

The TOKENS column is what the tool's name, description and argument schema cost
an agent on EVERY request while the tool is exposed, estimated as bytes / 4. It
is why 'repocortex serve' defaults to the minimal profile, and it is the number to
look at before choosing a larger one.`,
		Args: cobra.NoArgs,
	}
	profile := toolProfileFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPProfile(*profile)})
		if err != nil {
			return err
		}
		defer closeServer()
		inv, err := srv.NewInvoker(ctx, "cli")
		if err != nil {
			return err
		}
		defer func() { _ = inv.Close() }()

		tools, err := inv.Tools(ctx)
		if err != nil {
			return err
		}
		minimal, standard := devmcp.ProfileTools()
		tier := toolTiers(minimal, standard)

		total := 0
		for _, t := range tools {
			total += t.EstimatedTokens
		}
		if g.JSON {
			type row struct {
				devmcp.ToolInfo
				Profile string `json:"profile"`
			}
			rows := make([]row, 0, len(tools))
			for _, t := range tools {
				rows = append(rows, row{ToolInfo: t, Profile: tier(t.Name)})
			}
			return emitJSON(cmd.OutOrStdout(), map[string]any{
				"profile":                *profile,
				"tools":                  rows,
				"total_estimated_tokens": total,
				"mcp_version":            devmcp.Version,
			})
		}

		out := cmd.OutOrStdout()
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "TOOL\tPROFILE\tTOKENS\tWHAT IT RETURNS")
		for _, t := range tools {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", t.Name, tier(t.Name), t.EstimatedTokens, summaryOf(t.Description, 78))
		}
		_ = tw.Flush()
		fmt.Fprintf(out, "\n%d tool(s) on the %q profile, about %d tokens of schema per request.\n",
			len(tools), *profile, total)
		fmt.Fprintln(out, "Call one with: repocortex tool call <tool> key=value ...")
		return nil
	}
	return cmd
}

// toolTiers returns a function naming the smallest profile that exposes a tool.
func toolTiers(minimal, standard []string) func(string) string {
	in := map[string]string{}
	for _, n := range standard {
		in[n] = string(config.MCPStandard)
	}
	for _, n := range minimal {
		in[n] = string(config.MCPMinimal)
	}
	return func(name string) string {
		if p, ok := in[name]; ok {
			return p
		}
		return string(config.MCPFull)
	}
}

func newToolDescribeCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe <tool>",
		Short: "Show a tool's description and arguments",
		Args:  cobra.ExactArgs(1),
	}
	profile := toolProfileFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPProfile(*profile)})
		if err != nil {
			return err
		}
		defer closeServer()
		inv, err := srv.NewInvoker(ctx, "cli")
		if err != nil {
			return err
		}
		defer func() { _ = inv.Close() }()

		tools, err := inv.Tools(ctx)
		if err != nil {
			return err
		}
		want := args[0]
		for _, t := range tools {
			if t.Name != want {
				continue
			}
			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), t)
			}
			return renderToolDescription(cmd.OutOrStdout(), t)
		}
		return unknownToolError(args[0], *profile, tools)
	}
	return cmd
}

func renderToolDescription(out io.Writer, t devmcp.ToolInfo) error {
	fmt.Fprintf(out, "%s\n", t.Name)
	fmt.Fprintf(out, "\n%s\n", wrapText(t.Description, 88))

	params, err := schemaParams(t.InputSchema)
	if err != nil {
		return err
	}
	if len(params) == 0 {
		fmt.Fprintln(out, "\nTakes no arguments.")
	} else {
		fmt.Fprintln(out, "\nArguments:")
		for _, p := range params {
			req := "optional"
			if p.required {
				req = "required"
			}
			fmt.Fprintf(out, "  %s (%s, %s)\n", p.name, p.typ, req)
			if p.description != "" {
				fmt.Fprintf(out, "%s\n", indent(wrapText(p.description, 80), "      "))
			}
		}
	}
	fmt.Fprintf(out, "\nSchema cost: about %d tokens per request while exposed.\n", t.EstimatedTokens)
	return nil
}

type schemaParam struct {
	name, typ, description string
	required               bool
}

// schemaParams flattens a tool's JSON Schema into its top-level arguments.
func schemaParams(schema json.RawMessage) ([]schemaParam, error) {
	var s struct {
		Properties map[string]struct {
			Type        any    `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(schema) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, fmt.Errorf("read tool schema: %w", err)
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	out := make([]schemaParam, 0, len(s.Properties))
	for name, p := range s.Properties {
		typ := "value"
		switch v := p.Type.(type) {
		case string:
			typ = v
		case []any:
			// ["null","string"] and the like: name the non-null member.
			for _, m := range v {
				if ms, ok := m.(string); ok && ms != "null" {
					typ = ms
				}
			}
		}
		out = append(out, schemaParam{name: name, typ: typ, description: p.Description, required: required[name]})
	}
	// Required arguments first, then by name: the order a reader needs them.
	sort.Slice(out, func(i, j int) bool {
		if out[i].required != out[j].required {
			return out[i].required
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

func newToolCallCmd(g *Global) *cobra.Command {
	var (
		argsJSON string
		argsFile string
		auditLog string
	)
	cmd := &cobra.Command{
		Use:   "call <tool> [key=value ...]",
		Short: "Call one tool and print its answer",
		Long: `Calls a tool and prints the text an MCP client would receive.

Arguments are given as key=value pairs, as one JSON object, or both (pairs win):

    repocortex tool call find_callers name=ParseConfig
    repocortex tool call list_dead_code path=internal/billing/ include_exported=true limit=10
    repocortex tool call get_task_context --args '{"task":"add retry to the webhook sender"}'
    git diff | repocortex tool call minimize_diff diff=@-

A value of true, false, null or a number is sent as that JSON type; anything
else is sent as a string. Write key:=value to force JSON (key:='["a","b"]') and
key=@path to read a string from a file, with @- for standard input.

With --json the output is {tool, text, result, is_error}, where result is the
structured form of the same answer. The exit status is 1 when the tool reports
a failure and 0 otherwise, including when the answer is "nothing found".`,
		Args: cobra.MinimumNArgs(1),
	}
	profile := toolProfileFlag(cmd)
	cmd.Flags().StringVar(&argsJSON, "args", "", "arguments as one JSON object")
	cmd.Flags().StringVar(&argsFile, "args-file", "", "read the JSON arguments object from this file, or - for standard input")
	cmd.Flags().StringVar(&auditLog, "audit-log", "", "append one JSON line for this call to this file (default: $"+auditLogEnv+")")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		name := args[0]

		if argsJSON != "" && argsFile != "" {
			return errors.New("pass --args or --args-file, not both")
		}
		stdin := cmd.InOrStdin()
		merged := map[string]any{}
		raw := argsJSON
		if argsFile != "" {
			b, err := readArgSource(argsFile, stdin)
			if err != nil {
				return err
			}
			raw = string(b)
		}
		if strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &merged); err != nil {
				return fmt.Errorf("--args must be a JSON object, such as '{\"name\":\"ParseConfig\"}': %w", err)
			}
		}
		for _, pair := range args[1:] {
			k, v, err := parseToolArg(pair, stdin)
			if err != nil {
				return err
			}
			merged[k] = v
		}
		payload, err := json.Marshal(merged)
		if err != nil {
			return err
		}

		srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{
			Profile:   config.MCPProfile(*profile),
			AuditPath: auditLog,
		})
		if err != nil {
			return err
		}
		defer closeServer()
		inv, err := srv.NewInvoker(ctx, "cli")
		if err != nil {
			return err
		}
		defer func() { _ = inv.Close() }()

		res, err := inv.Call(ctx, name, payload)
		if err != nil {
			if errors.Is(err, devmcp.ErrUnknownTool) {
				tools, lerr := inv.Tools(ctx)
				if lerr != nil {
					return err
				}
				return unknownToolError(name, *profile, tools)
			}
			return err
		}

		out := cmd.OutOrStdout()
		if g.JSON {
			if err := emitJSON(out, map[string]any{
				"tool":     name,
				"text":     res.Text,
				"result":   res.Structured,
				"is_error": res.IsError,
			}); err != nil {
				return err
			}
		} else {
			fmt.Fprintln(out, strings.TrimRight(res.Text, "\n"))
		}
		if res.IsError {
			// The tool's own text is the report; an "error:" line under it
			// would read as repocortex malfunctioning.
			return &ExitCodeError{Code: 1, Silent: true, Err: errToolReportedFailure}
		}
		return nil
	}
	return cmd
}

// parseToolArg decodes one key=value or key:=json pair.
func parseToolArg(pair string, stdin io.Reader) (string, any, error) {
	if k, v, ok := strings.Cut(pair, ":="); ok && validArgKey(k) {
		var decoded any
		if err := json.Unmarshal([]byte(v), &decoded); err != nil {
			return "", nil, fmt.Errorf("%s:= expects JSON, got %q: %w", k, v, err)
		}
		return k, decoded, nil
	}
	k, v, ok := strings.Cut(pair, "=")
	if !ok || !validArgKey(k) {
		return "", nil, fmt.Errorf("argument %q: want key=value, or key:=<json>", pair)
	}
	if path, isFile := strings.CutPrefix(v, "@"); isFile {
		b, err := readArgSource(path, stdin)
		if err != nil {
			return "", nil, err
		}
		return k, string(b), nil
	}
	switch v {
	case "true":
		return k, true, nil
	case "false":
		return k, false, nil
	case "null":
		return k, nil, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return k, n, nil
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && strings.ContainsAny(v, ".eE") {
		return k, f, nil
	}
	return k, v, nil
}

func validArgKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// maxArgSourceBytes bounds a file or stdin argument, matching the REST limit.
const maxArgSourceBytes = 1 << 20

func readArgSource(path string, stdin io.Reader) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = stdin
	} else {
		f, err := os.Open(path) //nolint:gosec // G304: a path the user typed on their own command line
		if err != nil {
			return nil, fmt.Errorf("read argument file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxArgSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read argument: %w", err)
	}
	if len(b) > maxArgSourceBytes {
		return nil, fmt.Errorf("argument from %s is over the %d byte limit", path, maxArgSourceBytes)
	}
	return b, nil
}

// unknownToolError names the tools that do exist, closest matches first, so a
// mistyped or mis-remembered name is one retry away from the right one.
func unknownToolError(name, profile string, tools []devmcp.ToolInfo) error {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	var near []string
	for _, n := range names {
		if strings.Contains(n, name) || strings.Contains(name, n) || sharesWord(n, name) {
			near = append(near, n)
		}
	}
	hint := "Run 'repocortex tool list' to see every tool."
	if len(near) > 0 {
		hint = "Did you mean: " + strings.Join(near, ", ") + "?"
	}
	return fmt.Errorf("no tool named %q on the %q profile. %s", name, profile, hint)
}

func sharesWord(a, b string) bool {
	for _, wa := range strings.Split(a, "_") {
		for _, wb := range strings.FieldsFunc(b, func(r rune) bool { return r == '_' || r == '-' }) {
			if len(wa) > 3 && wa == wb {
				return true
			}
		}
	}
	return false
}

// summaryOf is a description's first sentence, cut to width.
func summaryOf(desc string, width int) string {
	s := desc
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if len(s) > width {
		s = strings.TrimRight(s[:width-1], " ,;:") + "…"
	}
	return s
}

func wrapText(s string, width int) string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
