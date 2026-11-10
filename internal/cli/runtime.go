package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/runtime"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/trace"

	"github.com/spf13/cobra"
)

// The last three surfaces: runtime correlation, task compilation, and the
// retrieval benchmark.
//
// Each answers a question the rest of the tool cannot. `trace` says what
// actually ran, which no static index knows. `compile` turns a one-line request
// into the brief an agent should have been given. `bench` measures whether the
// context engine retrieves the right code, which is the one claim in this
// product that would otherwise rest on assertion.

// ---------------------------------------------------------------- trace

func newTraceCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace <export.json>",
		Short: "Correlate an OpenTelemetry or Jaeger trace export with the index",
		Long: `Reads an exported trace and reports which indexed code it reached.

This answers what static analysis cannot: not what COULD run, but what did. An
endpoint the index knows and no span reached is either dead or untested, and
both are worth knowing before a refactor.

Reads an exported FILE, not a live collector. A service that must be running for
RepoCortex to work is a service RepoCortex has no business requiring, and the export
is the artefact CI already produces.

Correlation is heuristic and every match says how it was made. An explicit
OpenTelemetry code.function attribute names the symbol outright; an http.route
matching an endpoint is strong; a span name matching a symbol is weak, is only
tried when nothing better exists, and only when the name is unique — a span
called "handle" tells you nothing about which of nine handlers ran.

Spans are NOT persisted. Runtime data ages out in hours while an index describes
a commit, and storing them together would produce a graph whose edges are true
at different times.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			path := args[0]

			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("read trace export %q: %w", path, err)
			}
			if info.IsDir() {
				return fmt.Errorf("%q is a directory; pass the exported JSON file", path)
			}
			src, err := os.ReadFile(path) //nolint:gosec // caller-supplied export; treated as untrusted
			if err != nil {
				return err
			}

			spans, format, err := trace.Parse(src, 0)
			if err != nil {
				return fmt.Errorf("parse %q: %w", path, err)
			}
			if len(spans) == 0 {
				return fmt.Errorf("%q parsed as %s but contains no spans", path, format)
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			rep, err := runtime.Correlate(ctx, s.reg, s.runID, spans)
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"format": format, "report": rep,
				})
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s: %d span(s) across %d trace(s), service(s): %s\n\n",
				format, rep.Spans, rep.Traces, strings.Join(rep.Services, ", "))

			if len(rep.Exercised) > 0 {
				fmt.Fprintf(out, "EXERCISED (%d)\n", len(rep.Exercised))
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "  ENTITY\tKIND\tCALLS\tERRORS\tP95\tMATCHED BY")
				for _, o := range rep.Exercised {
					fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\t%s\n",
						o.Name, o.Kind, o.Calls, o.Errors,
						time.Duration(o.P95Nanos).Round(time.Microsecond), o.Match)
				}
				_ = tw.Flush()
				fmt.Fprintln(out)
			}

			if len(rep.ColdEndpoints) > 0 {
				fmt.Fprintf(out, "NEVER REACHED (%d endpoint(s))\n", len(rep.ColdEndpoints))
				for _, o := range rep.ColdEndpoints {
					fmt.Fprintf(out, "  %s\t%s\n", o.Name, o.Path)
				}
				fmt.Fprintln(out, "\n  Cold in THIS export only. An endpoint absent from one trace "+
					"window is not dead code — check the window before concluding anything.")
				fmt.Fprintln(out)
			}

			if len(rep.Unmatched) > 0 {
				fmt.Fprintf(out, "%d span operation(s) matched nothing indexed", len(rep.Unmatched))
				shown := rep.Unmatched
				if len(shown) > 8 {
					shown = shown[:8]
				}
				fmt.Fprintf(out, " (e.g. %s).\n", strings.Join(shown, ", "))
				fmt.Fprintln(out, "That means the correlation is weak here, not that the code is absent: "+
					"most services do not set OpenTelemetry's code.* attributes, and without them a "+
					"span name is all there is to match on.")
			}
			return nil
		},
	}
	return cmd
}

// ---------------------------------------------------------------- compile

func newCompileCmd(g *Global) *cobra.Command {
	var maxScope int

	cmd := &cobra.Command{
		Use:   "compile <intent>",
		Short: "Turn an informal request into an explicit task contract",
		Long: `Compiles a one-line request plus the assembled context into the brief an
AI coding agent should have been given.

The thesis is that most bad agent output comes from an under-specified task
rather than a weak model. "Add idempotency to payment callbacks" leaves the
agent to invent the scope, the constraints and the definition of done; a
contract states them, and states what it does not know.

EVERYTHING IS DERIVED, NOTHING IS INVENTED. Every constraint and risk carries
the file or symbol it came from. A constraint with no evidence is an opinion,
and this refuses to emit one.

Example:

  cortex compile "Add idempotency to payment callback processing"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			intent := strings.TrimSpace(strings.Join(args, " "))
			if intent == "" {
				return errors.New("intent must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			cfg := g.Config()
			gr := graph.NewForRun(s.db, s.runID)
			pack, err := devctx.NewAssembler().Assemble(ctx, s.reg, gr, s.runID, devctx.Request{
				Task:   intent,
				Budget: devctx.BudgetFrom(cfg),
			})
			if err != nil {
				return err
			}

			// Compiled WITH the declared architecture and the blast radius, not
			// from the context pack alone. A contract that cannot say which rule
			// the change must respect, or what the change would reach, is a
			// scope statement rather than a brief — and it is what `cortex
			// verify` later checks the change against.
			contract, err := compile.CompileWith(ctx, intent, pack,
				compile.Options{MaxScope: maxScope}, compilePorts(s, g))
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), contract)
			}
			fmt.Fprint(cmd.OutOrStdout(), contract.Markdown())
			return nil
		},
	}
	cmd.Flags().IntVar(&maxScope, "max-scope", 0, "bound the scope list (0 = default)")
	return cmd
}
