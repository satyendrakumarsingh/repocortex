package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// The API and database query surface — APIMap and DBMind from the spec,
// answered from the shared graph rather than from separate indexes.
//
// Both commands lead with coverage, because both are capable of the same
// misleading silence. An empty endpoint list means either that the service
// exposes nothing or that it uses a framework RepoCortex does not read, and an
// empty table list means either that nothing touches a database or that the
// queries are built somewhere the SQL recogniser cannot see. A reader has to be
// able to tell those apart, so the commands say which frameworks are covered
// and how many statements were classified rather than leaving it implied.

// ---------------------------------------------------------------- api

func newAPICmd(g *Global) *cobra.Command {
	var showUnhandled bool

	cmd := &cobra.Command{
		Use:   "api",
		Short: "List the HTTP endpoints this service exposes",
		Long: `Reports the API surface: every route found in the source, and the
handler behind it.

Routes are discovered from the framework's own spelling — a Spring annotation, a
Go router registration, a FastAPI or Flask decorator, a Django path() in a
urlconf, an Express member call, a NestJS controller decorator — never from
strings that merely look like paths. That is what keeps a log message
mentioning "GET /users" out of the report. Django routes are reported at
medium confidence: the include() mount prefix that precedes a urls.py's own
paths is not visible from the file.

NOT covered: OpenAPI documents, GraphQL schemas, gRPC service definitions and
API-gateway configuration. A service defined entirely in those will report no
endpoints, which is why the supported list is printed alongside the result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			endpoints, err := s.reg.EndpointReport(ctx, s.runID)
			if err != nil {
				return err
			}

			// Endpoints declared in test files are real routes in real code, but
			// they are NOT this service's API surface, and listing them together
			// said RepoCortex had found 5 endpoints on a repository that exposes
			// none. Split rather than dropped: "which routes do the tests stand
			// up" is a fair question, just a different one.
			var live, fromTests []registry.Endpoint
			for _, e := range endpoints {
				if capability.IsTestPath(e.FilePath) {
					fromTests = append(fromTests, e)
					continue
				}
				live = append(live, e)
			}
			endpoints = live

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision":       s.runID,
					"endpoints":            live,
					"test_fixture_routes":  fromTests,
					"supported_frameworks": api.SupportedFrameworks(),
				})
			}

			out := cmd.OutOrStdout()
			if len(endpoints) == 0 && len(fromTests) > 0 {
				fmt.Fprintf(out, "No HTTP endpoints found in application code.\n\n"+
					"%d route(s) are declared in test files. Those are fixtures standing up a "+
					"server for a test, not this service's API surface, so they are not "+
					"reported as endpoints:\n", len(fromTests))
				for _, e := range fromTests {
					fmt.Fprintf(out, "  %s\t%s:%d\n", e.Signature, e.FilePath, e.Line)
				}
				fmt.Fprintf(out, "\nRoute discovery covers %s.\n", frameworkList())
				return nil
			}
			if len(endpoints) == 0 {
				fmt.Fprintln(out, "No HTTP endpoints found.")
				fmt.Fprintf(out, "\nRoute discovery covers %s.\n", frameworkList())
				fmt.Fprintln(out, "A service defined through OpenAPI, GraphQL, gRPC or gateway "+
					"configuration will report nothing here — that is a coverage limit, not a "+
					"finding about your code.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "METHOD & PATH\tHANDLER\tDECLARED AT")
			unhandled := 0
			for _, e := range endpoints {
				handler := e.Handler
				if handler == "" {
					handler = "(not determined)"
					unhandled++
				} else if e.HandlerPath != "" && e.HandlerPath != e.FilePath {
					handler += " (" + e.HandlerPath + ")"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s:%d\n", e.Signature, handler, e.FilePath, e.Line)
			}
			_ = tw.Flush()

			fmt.Fprintf(out, "\n%d endpoint(s). Discovery covers %s.\n",
				len(endpoints), frameworkList())
			if len(fromTests) > 0 {
				fmt.Fprintf(out, "%d further route(s) are declared in test files and are not "+
					"counted above — they are fixtures, not this service's API.\n", len(fromTests))
			}
			if unhandled > 0 {
				fmt.Fprintf(out, "%d endpoint(s) have no handler recorded: the route is real, but "+
					"the framework's syntax did not name the function, and guessing it would send "+
					"you to the wrong place.\n", unhandled)
			}
			_ = showUnhandled
			return nil
		},
	}
	cmd.Flags().BoolVar(&showUnhandled, "all", false, "reserved; endpoints without handlers are always listed")
	_ = cmd.Flags().MarkHidden("all")
	return cmd
}

func frameworkList() string {
	fws := api.SupportedFrameworks()
	names := make([]string, 0, len(fws))
	for _, f := range fws {
		names = append(names, string(f))
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------- db

func newDBCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Report database tables and how the code uses them",
		Long: `Reports every table the repository declares or queries, with the
number of reads and writes against each.

READ, WRITE, READ_WRITE, DDL and UNKNOWN are kept distinct. "What touches this
table" is a mildly useful question; "what WRITES this table" is the one asked
before changing a column, and answering it with reads mixed in is worse than
not answering.

A table nothing here declares is still reported. That is the normal shape of a
service reading a database another service owns, and dropping it would make
"which tables do we touch that we do not own" unanswerable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			usage, err := s.reg.TableUsageReport(ctx, s.runID)
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision": s.runID, "tables": usage,
				})
			}

			out := cmd.OutOrStdout()
			if len(usage) == 0 {
				fmt.Fprintln(out, "No database access found.")
				fmt.Fprintln(out, "\nSQL is recognised in .sql files and in string literals in "+
					"source. Queries built by an ORM's fluent API, or assembled far from where "+
					"they run, are not visible to a static scan — that is a coverage limit, not "+
					"a finding about your code.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "TABLE\tREADS\tWRITES\tDECLARED HERE")
			var external, uncertain int
			for _, u := range usage {
				declared := "yes"
				if !u.Declared {
					declared = "no — not declared in this repository"
					external++
				}
				uncertain += u.Uncertain
				fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", u.Table, u.Readers, u.Writers, declared)
			}
			_ = tw.Flush()

			fmt.Fprintf(out, "\n%d table(s).\n", len(usage))
			if external > 0 {
				fmt.Fprintf(out, "%d are not declared by any migration here — most likely owned "+
					"by another service.\n", external)
			}
			if uncertain > 0 {
				fmt.Fprintf(out, "%d statement(s) were assembled at run time; the table is real, "+
					"but whether that exact statement executes as written is not something a "+
					"static read can establish, so those edges carry low confidence.\n", uncertain)
			}
			return nil
		},
	}
	cmd.AddCommand(newDBTableCmd(g))
	return cmd
}

func newDBTableCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "table <name>",
		Short: "Show what reads and writes one table",
		Long: `Lists the code that reads or writes a single table, with the
evidence line for each.

This is the question asked before changing a column: not "does anything use
this", but "what exactly writes it, and where".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			table := strings.ToLower(strings.TrimSpace(args[0]))
			if table == "" {
				return errors.New("table name must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			usage, err := s.reg.TableUsageReport(ctx, s.runID)
			if err != nil {
				return err
			}
			var target *registry.TableUsage
			for i := range usage {
				if usage[i].Table == table {
					target = &usage[i]
					break
				}
			}
			if target == nil {
				names := make([]string, 0, len(usage))
				for _, u := range usage {
					names = append(names, u.Table)
				}
				if len(names) > 12 {
					names = names[:12]
				}
				return fmt.Errorf("no table %q in the index; known tables include %s",
					table, strings.Join(names, ", "))
			}

			out := cmd.OutOrStdout()
			if !target.Declared {
				if g.JSON {
					return emitJSON(out, map[string]any{"table": table, "declared": false, "accessors": nil})
				}
				fmt.Fprintf(out, "%s — used but not declared in this repository.\n\n", table)
				fmt.Fprintf(out, "%d read(s), %d write(s) recorded. Because no migration here "+
					"declares it, the accesses are not attached to a table node and cannot be "+
					"listed individually.\n", target.Readers, target.Writers)
				return nil
			}

			gr := graph.NewForRun(s.db, s.runID)
			readers, err := gr.Neighbours(ctx, target.SymbolID, graph.Incoming,
				[]model.EdgeKind{model.EdgeReads}, "")
			if err != nil {
				return err
			}
			writers, err := gr.Neighbours(ctx, target.SymbolID, graph.Incoming,
				[]model.EdgeKind{model.EdgeWrites}, "")
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(out, map[string]any{
					"table": table, "declared": true,
					"readers": readers, "writers": writers,
				})
			}

			fmt.Fprintf(out, "%s (declared in this repository)\n\n", table)
			renderAccessors(out, "WRITTEN BY", writers, s)
			renderAccessors(out, "READ BY", readers, s)
			return nil
		},
	}
}

// renderAccessors prints one direction of table access, resolving symbol ids to
// names so the output names functions rather than row ids.
func renderAccessors(out interface{ Write([]byte) (int, error) }, label string,
	ns []graph.Neighbour, s *session) {
	fmt.Fprintf(out, "%s (%d)\n", label, len(ns))
	if len(ns) == 0 {
		fmt.Fprintln(out, "  nothing found")
		fmt.Fprintln(out)
		return
	}
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, n := range ns {
		name := symbolName(s, n.SymbolID)
		fmt.Fprintf(tw, "  %s\t%s:%d\t%s (%s)\n", name,
			n.Evidence.FilePath, n.Evidence.Range.Start.Line,
			n.Confidence, n.Evidence.Detail)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
}

// symbolName resolves one symbol id to a display name, falling back to the id
// so a missing row degrades to a number rather than an empty column.
func symbolName(s *session, id int64) string {
	var name, path string
	err := s.db.QueryRow(`
		SELECT s.name, f.path FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.id = ?`, id).
		Scan(&name, &path)
	if err != nil {
		return fmt.Sprintf("symbol#%d", id)
	}
	return name + " (" + path + ")"
}
