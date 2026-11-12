package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// queryTimeout bounds one ad-hoc query. A cross join typed by mistake must end
// on its own instead of holding the index open until someone notices.
const queryTimeout = 15 * time.Second

// maxQueryRows is the most rows a query may return, whatever --limit says.
const maxQueryRows = 5000

// newQueryCmd is the escape hatch: a question no command answers, asked of the
// index directly.
//
// Other code-graph tools offer a graph query language for this. The index here
// is a SQLite file, and SQL is the query language every reader already knows,
// so the command exposes that and nothing invented.
//
// THREE GUARDS, each independent of the others:
//
//   - the index is opened READ-ONLY at the storage layer, so no statement can
//     write whatever it says;
//   - only one statement is accepted and it must be a SELECT or a WITH, which
//     keeps ATTACH (a way to open another file) and PRAGMA out;
//   - the statement runs inside `SELECT * FROM (...) LIMIT n` under a timeout,
//     so its cost and its output are bounded.
//
// It is a command for people and deliberately not an agent tool. A tool is a
// contract, and exposing the tables to agents would freeze the schema as one.
func newQueryCmd(g *Global) *cobra.Command {
	var (
		limit  int
		schema bool
	)
	cmd := &cobra.Command{
		Use:   "query [sql]",
		Short: "Run a read-only SQL query against the index",
		Long: `Runs one SELECT against the index and prints the rows.

  repocortex query --schema
  repocortex query "SELECT kind, COUNT(*) AS n FROM symbols WHERE index_run_id = :run GROUP BY kind ORDER BY n DESC"
  repocortex query "SELECT s.name, COUNT(*) AS callers
                    FROM edges e JOIN symbols s ON s.id = e.dst_id
                    WHERE e.index_run_id = :run AND e.kind = 'CALLS'
                    GROUP BY s.id ORDER BY callers DESC" --limit 20

:run is the id of the committed index run. Every table holds rows for every
run, so a query without "index_run_id = :run" counts rows twice after a
re-index.

The index is opened read-only, only a single SELECT or WITH statement is
accepted, and the result is capped at --limit rows. The tables are the index's
own storage, not a stable interface: they change between versions. Use the
other commands, or 'repocortex tool call', for anything a script depends on.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := stdctx.WithTimeout(cmd.Context(), queryTimeout)
			defer cancel()

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			var sqlText string
			switch {
			case schema && len(args) > 0:
				return errors.New("pass a query or --schema, not both")
			case schema:
				sqlText = `SELECT m.name AS "table", p.name AS "column", p.type AS "type"
				             FROM sqlite_master m JOIN pragma_table_info(m.name) p
				            WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name NOT LIKE '%_fts%'
				            ORDER BY m.name, p.cid`
				limit = maxQueryRows
			case len(args) == 0:
				return errors.New("pass a SELECT statement, or --schema to list the tables")
			default:
				sqlText, err = checkSelect(args[0])
				if err != nil {
					return err
				}
			}
			if limit < 1 || limit > maxQueryRows {
				return fmt.Errorf("--limit %d: want 1 to %d", limit, maxQueryRows)
			}

			// One extra row is fetched so "exactly limit rows" and "more than
			// limit rows" can be told apart and the cut reported.
			wrapped := "SELECT * FROM (" + sqlText + ") LIMIT " + fmt.Sprint(limit+1)
			var bind []any
			if strings.Contains(sqlText, ":run") {
				bind = append(bind, stdSQLNamed("run", s.runID))
			}
			rows, err := s.db.QueryContext(ctx, wrapped, bind...)
			if err != nil {
				if errors.Is(ctx.Err(), stdctx.DeadlineExceeded) {
					return fmt.Errorf("the query did not finish in %s; narrow it or add a LIMIT", queryTimeout)
				}
				return fmt.Errorf("query: %w", err)
			}
			defer rows.Close()

			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			var out [][]any
			truncated := false
			for rows.Next() {
				if len(out) == limit {
					truncated = true
					break
				}
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				for i, v := range vals {
					if b, ok := v.([]byte); ok {
						vals[i] = string(b)
					}
				}
				out = append(out, vals)
			}
			if err := rows.Err(); err != nil {
				if errors.Is(ctx.Err(), stdctx.DeadlineExceeded) {
					return fmt.Errorf("the query did not finish in %s; narrow it or add a LIMIT", queryTimeout)
				}
				return err
			}

			if g.JSON {
				records := make([]map[string]any, 0, len(out))
				for _, r := range out {
					rec := make(map[string]any, len(cols))
					for i, c := range cols {
						rec[c] = r[i]
					}
					records = append(records, rec)
				}
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision": s.runID, "columns": cols, "rows": records, "truncated": truncated,
				})
			}

			w := cmd.OutOrStdout()
			tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
			for _, r := range out {
				cells := make([]string, len(r))
				for i, v := range r {
					cells[i] = cellText(v)
				}
				fmt.Fprintln(tw, strings.Join(cells, "\t"))
			}
			_ = tw.Flush()
			fmt.Fprintf(w, "\n%d row(s).", len(out))
			if truncated {
				fmt.Fprintf(w, " More exist; this stopped at --limit %d.", limit)
			}
			fmt.Fprintln(w)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 100, "maximum rows to print")
	cmd.Flags().BoolVar(&schema, "schema", false, "list the tables and their columns")
	return cmd
}

// checkSelect accepts one SELECT or WITH statement and returns it without a
// trailing semicolon.
//
// The read-only connection is what actually prevents a write. This check is
// for what read-only does not cover: a second statement, and ATTACH or PRAGMA,
// which read or reconfigure without writing.
func checkSelect(raw string) (string, error) {
	q := strings.TrimSpace(raw)
	q = strings.TrimSpace(strings.TrimSuffix(q, ";"))
	if q == "" {
		return "", errors.New("the query is empty")
	}
	if strings.Contains(stripSQLStrings(q), ";") {
		return "", errors.New("only one statement is accepted; remove the ';'")
	}
	first := strings.ToUpper(strings.Fields(q)[0])
	if first != "SELECT" && first != "WITH" {
		return "", fmt.Errorf("only SELECT (or WITH ... SELECT) is accepted, not %s; the index is read-only", first)
	}
	return q, nil
}

// stripSQLStrings blanks out quoted text, so a ';' inside a string literal is
// not mistaken for a statement separator.
func stripSQLStrings(q string) string {
	var b strings.Builder
	var quote rune
	for _, r := range q {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b.WriteRune(' ')
		case r == '\'' || r == '"' || r == '`':
			quote = r
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cellText(v any) string {
	if v == nil {
		return "NULL"
	}
	s := fmt.Sprint(v)
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	if len(s) > 100 {
		s = s[:99] + "…"
	}
	return s
}
