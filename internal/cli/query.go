package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// Query commands: the read side of RepoCortex from a terminal.
//
// These answer the same questions as the MCP tools, against the same registry,
// with the same honesty rules. That duplication is deliberate and worth its
// cost. MCP is a machine protocol spoken over a pipe by an agent; a person
// debugging why an agent got a wrong answer needs to ask the index directly,
// and telling them to write a JSON-RPC client is not an answer. It is also how
// the index gets used in a shell script, a CI check, or a git hook, none of
// which want to run a server.
//
// The rule these share with the MCP layer, and the one most easily lost when a
// query surface is written twice: AN EMPTY RELATIONSHIP RESULT IS AMBIGUOUS. It
// means either that nothing references the symbol, or that RepoCortex could not
// resolve the references that do. Those have opposite consequences — the first
// says a method is safe to delete, the second says nothing at all — so every
// empty result here carries the measured resolution coverage. See emptyCaveat.

// session is an open, read-only view of the committed index.
//
// Read-only at the store level rather than by convention: a query command that
// can write is one bad code path away from mutating an index while `repocortex
// watch` holds it, and SQLite's answer to that is a locked database, not an
// error anyone can act on.
type session struct {
	db    *store.DB
	reg   *registry.Registry
	runID int64

	// stale explains, if it needs explaining, that this index was written by a
	// different build than the one reading it. Empty when there is nothing to
	// say. See staleAgainst.
	stale string
}

func (s *session) close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}

// openQuery opens the committed index for reading.
//
// The two failure modes are separated on purpose. "No index" and "an index with
// nothing committed in it" look identical to a user staring at an empty result,
// and each has a different fix; conflating them costs someone an afternoon.
func openQuery(ctx stdctx.Context, g *Global) (*session, error) {
	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no index at %s — run 'repocortex init' first", dbPath)
	}

	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return nil, err
	}

	var runID int64
	err = db.QueryRowContext(ctx, `SELECT id FROM index_runs WHERE committed = 1`).Scan(&runID)
	if err != nil {
		_ = db.Close()
		if strings.Contains(err.Error(), "no rows") {
			return nil, errors.New("no committed index — run 'repocortex index' first")
		}
		return nil, err
	}

	s := &session{db: db, reg: registry.New(db), runID: runID}

	// Warned once, here, rather than in each command: this is the only place
	// every read path passes through, and a caveat that depends on remembering
	// to print it is one that will be missing from the command where it mattered.
	// It goes to the logger, so it lands on stderr and never contaminates the
	// --json stdout a script is parsing.
	if s.stale = staleAgainst(ctx, s.reg, runID, Version); s.stale != "" {
		g.Log().Warn(s.stale)
	}
	return s, nil
}

// staleAgainst reports, in one sentence, that this index was written by a
// different build than the one now reading it — or "" when it was not.
//
// An index is data PLUS the resolution rules that produced it, and this project
// has already changed those rules twice: one guard broke every chained Go call
// and another converted 362 ambiguous references into confident wrong answers.
// An index written either side of such a change holds different edges for
// identical source, every query against it succeeds, and the results look
// exactly as trustworthy as current ones. There is no symptom to notice, which
// is why the index has to say so itself.
//
// A run with no stamp predates the column. That is UNKNOWN provenance, not
// known-staleness, and it is reported differently because the advice differs:
// one is "re-index to be sure", the other is "these results came from other
// rules". Neither is an error — a stale index still answers, and refusing to
// answer would be a worse trade than answering with a caveat.
func staleAgainst(ctx stdctx.Context, reg *registry.Registry, runID int64, running string) string {
	run, err := reg.Run(ctx, runID)
	if err != nil || run == nil {
		// Never let a provenance check break a query. The caveat is a courtesy;
		// the answer is the product.
		return ""
	}
	switch {
	case run.ProducerVersion == "":
		return "This index does not record which build wrote it, so whether its " +
			"resolution rules match this binary is unknown. Re-index to be sure."
	case run.ProducerVersion != running:
		return fmt.Sprintf("This index was written by repocortex %s and you are running %s. "+
			"Resolution rules change between builds, so these results may come from "+
			"different rules than this binary would produce. Re-index to refresh.",
			run.ProducerVersion, running)
	default:
		return ""
	}
}

// absenceDiagnosis turns "nothing found" into a diagnosis of THIS name.
//
// emptyCaveat above reports the repository's overall resolution coverage, which
// says how much to trust an empty answer in general. This says what actually
// happened to this particular name, which is the question the reader has.
//
// The outcome of every reference has been on disk since migration 0002 and was
// read back only as a percentage. So an empty result could mean the call does
// not exist, or the file never parsed, or the reference was seen and could not
// be bound — three situations with different remedies and one indistinguishable
// rendering. Every tool in this category shares that flaw; none of them can tell
// you which of the three you are looking at.
//
// Reports only the references that did NOT bind. A resolved reference is already
// visible as an edge, so listing it here would pad a diagnostic with rows that
// are not the problem.
func absenceDiagnosis(ctx stdctx.Context, s *session, name string) string {
	rows, err := s.reg.ResolutionsForName(ctx, s.runID, name, 0)
	if err != nil || len(rows) == 0 {
		// No recorded reference to this name anywhere. That is itself the
		// answer — the name was never even seen — but emptyCaveat has already
		// framed it, and guessing further would be worse than stopping.
		return ""
	}

	var unbound []registry.ReferenceResolution
	for _, r := range rows {
		if r.Resolution != registry.ResolutionResolved {
			unbound = append(unbound, r)
		}
	}
	if len(unbound) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nWHY: %d reference(s) to %q were seen and not bound.\n",
		len(unbound), name)
	for _, r := range unbound {
		recv := ""
		if r.Receiver != "" {
			recv = fmt.Sprintf(", receiver %q", r.Receiver)
		}
		fmt.Fprintf(&b, "  %s:%d  %s%s\n", r.Path, r.Line, r.Resolution, recv)
	}
	fmt.Fprintf(&b, "\n  %q means the reference was found and RepoCortex could not decide "+
		"which definition it meant; %q means no definition of that name is in this index — "+
		"usually a library, but it can also be code this index did not cover. "+
		"Either way the call may well exist — this is a blind spot, not an absence.\n",
		registry.ResolutionAmbiguous, registry.ResolutionUnresolved)
	return b.String()
}

// emptyCaveat explains an empty relationship result in terms of measured
// coverage rather than leaving the reader to assume completeness.
//
// Returns "" when the graph has coverage worth reporting and the emptiness is
// therefore probably real — silence is correct there, and a caveat printed on
// every query is a caveat nobody reads.
func emptyCaveat(ctx stdctx.Context, s *session) string {
	rs, err := s.reg.ResolutionStats(ctx, s.runID)
	if err != nil {
		return "Resolution coverage is unknown, so an empty result cannot be read as proof of absence."
	}
	switch {
	case rs.Total == 0:
		return "No references were extracted from this index at all, so there is nothing to " +
			"search. Run 'repocortex index' first."

	case rs.Pending == rs.Total:
		// The condition used to be `rs.Total == 0`, which is only true when no
		// reference exists. Before `resolve` runs, Total is large and every row
		// is PENDING — so this branch never fired, and the message below
		// printed instead: "0% of 16973 references are bound (0 point outside,
		// 0 ambiguous)". Arithmetically impossible, and it offered no remedy
		// for the one situation with an obvious one.
		return fmt.Sprintf(
			"%d reference(s) were extracted but none have been resolved yet — run "+
				"'repocortex resolve' to build the call graph. Until then this query has "+
				"nothing to search.", rs.Total)

	case rs.Pending > 0:
		// Partially resolved: an incremental index added references after the
		// last resolve. Reporting the rate over the whole set would understate
		// coverage of the part that WAS resolved, and hiding the pending rows
		// would overstate it.
		return bindingCaveat(rs) + fmt.Sprintf(" A further %d reference(s) have not been "+
			"resolved since the last index — re-run 'repocortex resolve'.", rs.Pending)
	}

	return bindingCaveat(rs)
}

// bindingCaveat states resolution coverage without the two claims the old
// wording made and could not support.
//
// IT NO LONGER DIVIDES BY EVERY REFERENCE. A reference that matched no
// definition in this index cannot be bound by any improvement to binding — only
// by indexing more code — so counting it as a failure turns a description of
// how much library code a project calls into what reads as a defect rate. The
// rate here is over the references that name something this index actually
// defines: bound, plus too ambiguous to choose between. Both populations are
// printed, so nothing is excluded that the reader cannot see.
//
// IT NO LONGER CALLS UNRESOLVED REFERENCES EXTERNAL. The old text said they
// "point outside this repository". The resolver records 'unresolved' when the
// name matched zero candidates HERE, which usually means a library and
// sometimes means a definition this index failed to cover — and those are
// different problems with different owners. Measured on an 8,500-file
// repository, importing compiler indexes for most of it bound 119 of 324,342
// unresolved references to in-repository symbols: enough to justify "mostly",
// not enough to justify "outside".
//
// The compiler-settled count is named when there is one, because "bound by a
// typechecker" and "bound by name matching plus an import heuristic" are
// different claims to somebody deciding whether to trust an empty result.
func bindingCaveat(rs registry.ResolutionStats) string {
	var by string
	if rs.ByCompiler > 0 {
		by = fmt.Sprintf(", %d of them by an imported compiler index", rs.ByCompiler)
	}
	return fmt.Sprintf(
		"%.0f%% of the %d reference(s) that name something this index defines are bound to a "+
			"definition%s; %d remain too ambiguous to choose between. A further %d reference(s) "+
			"matched no definition here at all — mostly calls into libraries, which an index of "+
			"this repository cannot bind. An empty result is a lower bound, not proof that "+
			"nothing uses this symbol.",
		rs.BindingRate()*100, rs.Bindable(), by, rs.Ambiguous, rs.Unresolved)
}

// resolveTarget picks the symbol a relationship query is about.
//
// Ambiguity is reported, never silently resolved. Three methods named `handle`
// in three services are the normal case in real code, and answering for
// whichever one sorted first — while looking exactly like a definite answer —
// is how a person ends up deleting the wrong one.
func resolveTarget(ctx stdctx.Context, s *session, name, path string) (registry.SymbolHit, []registry.SymbolHit, error) {
	syms, err := s.reg.FindSymbolsByName(ctx, s.runID, name, "", 50)
	if err != nil {
		return registry.SymbolHit{}, nil, fmt.Errorf("symbol lookup: %w", err)
	}
	if len(syms) == 0 {
		return registry.SymbolHit{}, nil, fmt.Errorf(
			"no symbol named %q in the index; names must match exactly — use 'repocortex search' for partial matches", name)
	}

	if path != "" {
		var kept []registry.SymbolHit
		for _, sym := range syms {
			if strings.Contains(sym.FilePath, path) {
				kept = append(kept, sym)
			}
		}
		if len(kept) == 0 {
			return registry.SymbolHit{}, nil, fmt.Errorf(
				"no symbol named %q in a file matching %q; %d definition(s) exist elsewhere", name, path, len(syms))
		}
		syms = kept
	}
	return syms[0], syms, nil
}

// ---------------------------------------------------------------- search

func newSearchCmd(g *Global) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across indexed files",
		Long: `Searches file contents using the index's FTS5 tables.

Identifiers are indexed by their parts, so "callback" matches
handlePaymentCallback and PaymentCallbackController. Search covers indexed
files only: binary, generated and oversized files are recorded in the index but
carry no searchable text, and secrets are redacted before storage.

This searches TEXT. To find a definition by name, use 'repocortex symbol'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.TrimSpace(args[0])
			if query == "" {
				return errors.New("query must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			matches, err := s.reg.SearchFiles(ctx, query, limit)
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"query": query, "index_revision": s.runID, "matches": matches,
				})
			}

			out := cmd.OutOrStdout()
			if len(matches) == 0 {
				fmt.Fprintf(out, "No files match %q.\n", query)
				fmt.Fprintln(out, "\nSearch covers indexed files only; generated and oversized files carry no text.")
				fmt.Fprintln(out, "Try a shorter term — identifiers are indexed by their parts.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			for _, m := range matches {
				fmt.Fprintf(tw, "%s\n", m.Path)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d file(s) matching %q.\n", len(matches), query)
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "maximum files to report")
	return cmd
}

// ---------------------------------------------------------------- symbol

func newSymbolCmd(g *Global) *cobra.Command {
	var kind string
	var limit int

	cmd := &cobra.Command{
		Use:   "symbol <name>",
		Short: "Find definitions by exact name",
		Long: `Looks up symbol definitions by exact name.

Exact, not fuzzy: asking for "TransactionService" returns that type, not the
forty symbols whose names contain it. Use 'repocortex search' for partial matches.

Every result carries the confidence of the EXTRACTION and its source, so a
compiler-proven definition is distinguishable from a parsed one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("name must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			syms, err := s.reg.FindSymbolsByName(ctx, s.runID, name, kind, limit)
			if err != nil {
				return fmt.Errorf("symbol lookup: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"name": name, "kind": kind, "index_revision": s.runID, "symbols": syms,
				})
			}

			out := cmd.OutOrStdout()
			if len(syms) == 0 {
				fmt.Fprintf(out, "No definition named %q.\n", name)
				if kind != "" {
					fmt.Fprintf(out, "\nThe --kind filter was %q; try without it.\n", kind)
				}
				fmt.Fprintln(out, "\nNames must match exactly. Use 'repocortex search' for partial matches.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "KIND\tNAME\tLOCATION\tCONFIDENCE")
			for _, sym := range syms {
				fmt.Fprintf(tw, "%s\t%s\t%s:%d\t%s (%s)\n",
					sym.Kind, sym.Name, sym.FilePath, sym.StartLine, sym.Confidence, sym.Source)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d definition(s) named %q.\n", len(syms), name)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "restrict to one kind (class, interface, method, function, ...)")
	cmd.Flags().IntVarP(&limit, "limit", "n", 50, "maximum definitions to report")
	return cmd
}

// ---------------------------------------------------- references / callers / callees

// relationshipSpec describes one of the three edge queries, which differ only
// in direction and in which edge kinds count.
//
// One implementation rather than three because the part that matters — the
// empty-result caveat, ambiguity reporting, confidence display — must be
// identical in all of them, and three copies is how one of them quietly stops
// being honest.
type relationshipSpec struct {
	use      string
	short    string
	long     string
	incoming bool
	// kinds restricts the edge kinds counted. Empty means every kind.
	kinds []model.EdgeKind
	// noun names the thing being listed, for output.
	noun string
}

func newReferencesCmd(g *Global) *cobra.Command {
	return newRelationshipCmd(g, relationshipSpec{
		use:   "references <name>",
		short: "Find everything that references a symbol",
		long: `Lists every relationship pointing AT a symbol: calls, imports,
implementations, extensions and plain references.

Broader than 'callers', which counts only call edges.`,
		incoming: true,
		noun:     "reference",
	})
}

func newCallersCmd(g *Global) *cobra.Command {
	return newRelationshipCmd(g, relationshipSpec{
		use:   "callers <name>",
		short: "Find what calls a symbol",
		long: `Lists call relationships pointing AT a symbol.

Narrower than 'references': an import or an implementation is a reference but
not a call.`,
		incoming: true,
		kinds:    []model.EdgeKind{model.EdgeCalls},
		noun:     "caller",
	})
}

func newCalleesCmd(g *Global) *cobra.Command {
	return newRelationshipCmd(g, relationshipSpec{
		use:   "callees <name>",
		short: "Find what a symbol calls",
		long: `Lists call relationships pointing AWAY FROM a symbol — what it
depends on, rather than what depends on it.`,
		incoming: false,
		kinds:    []model.EdgeKind{model.EdgeCalls},
		noun:     "callee",
	})
}

func newRelationshipCmd(g *Global, spec relationshipSpec) *cobra.Command {
	var path string
	var limit int
	var minConfidence string

	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Long: spec.long + `

Every edge carries the confidence that justified it and the file and line that
evidences it. An empty result reports the index's measured resolution coverage,
because "nothing found" and "nothing resolvable" are different facts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("name must not be empty")
			}

			var minConf model.Confidence
			if minConfidence != "" {
				minConf = model.Confidence(minConfidence)
				if !minConf.Valid() {
					return fmt.Errorf("--min-confidence %q: want exact, high, medium or low", minConfidence)
				}
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			target, all, err := resolveTarget(ctx, s, name, path)
			if err != nil {
				return err
			}

			// The kind and confidence filters are pushed into the registry
			// query (EdgeFilter) rather than applied to the result afterward,
			// so --limit's cap is spent on rows that pass the filters instead
			// of on an unfiltered set the filters then shrink — which could
			// otherwise leave a real, higher-confidence edge unreported simply
			// because lower-value rows crowded it out of the capped fetch.
			filter := registry.EdgeFilter{MinConfidence: minConf}
			if len(spec.kinds) > 0 {
				filter.Kinds = make([]string, len(spec.kinds))
				for i, k := range spec.kinds {
					filter.Kinds[i] = string(k)
				}
			}

			var edges []registry.EdgeHit
			if spec.incoming {
				edges, err = s.reg.IncomingEdges(ctx, s.runID, target.ID, limit, filter)
			} else {
				edges, err = s.reg.OutgoingEdges(ctx, s.runID, target.ID, limit, filter)
			}
			if err != nil {
				return fmt.Errorf("relationship lookup: %w", err)
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"name": name, "index_revision": s.runID,
					"target": map[string]any{
						"id": target.ID, "kind": target.Kind,
						"path": target.FilePath, "line": target.StartLine,
					},
					"ambiguous":     len(all) > 1,
					spec.noun + "s": edges,
				})
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s (%s) at %s:%d\n\n",
				target.Name, target.Kind, target.FilePath, target.StartLine)

			if len(edges) == 0 {
				fmt.Fprintf(out, "No %ss found.\n\n%s\n", spec.noun, emptyCaveat(ctx, s))
				fmt.Fprint(out, absenceDiagnosis(ctx, s, name))
				reportAmbiguity(out, all, name)
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "KIND\tSYMBOL\tEVIDENCE\tCONFIDENCE")
			for _, e := range edges {
				other, otherPath := e.SrcName, e.SrcPath
				if !spec.incoming {
					other, otherPath = e.DstName, ""
				}
				if other == "" {
					other = "(unnamed)"
				}
				evidence := fmt.Sprintf("%s:%d", e.EvidenceFile, e.EvidenceLine)
				if e.EvidenceFile == "" {
					evidence = otherPath
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s (%s)\n", e.Kind, other, evidence, e.Confidence, e.Source)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d %s(s).\n", len(edges), spec.noun)
			reportAmbiguity(out, all, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "disambiguate by file path substring")
	cmd.Flags().IntVarP(&limit, "limit", "n", 100, "maximum relationships to report")
	cmd.Flags().StringVar(&minConfidence, "min-confidence", "",
		"drop edges below this confidence (exact, high, medium, low)")
	return cmd
}

// reportAmbiguity names the other definitions sharing the queried name.
//
// Printed after the result, not instead of it: the answer for the chosen symbol
// is still useful, and the reader needs to know it may not be the symbol they
// meant.
func reportAmbiguity(w io.Writer, all []registry.SymbolHit, name string) {
	if len(all) <= 1 {
		return
	}
	fmt.Fprintf(w, "\n%d definitions share the name %q; this answered for the first.\n", len(all), name)
	fmt.Fprintln(w, "Pass --path to choose another:")
	for _, sym := range all {
		fmt.Fprintf(w, "  %s:%d (%s)\n", sym.FilePath, sym.StartLine, sym.Kind)
	}
}

// ---------------------------------------------------------------- context

func newContextCmd(g *Global) *cobra.Command {
	var (
		maxFiles   int
		maxSymbols int
		maxLines   int
		showAll    bool
	)

	cmd := &cobra.Command{
		Use:   "context <task>",
		Short: "Assemble the context an agent needs for a task",
		Long: `Assembles a bounded, ranked context pack for a plain-English task.

This is what 'get_task_context' serves over MCP, printed for a human. It is
the fastest way to see WHY an agent received what it received: every item
carries the reason it earned its tokens, and everything considered and left
out is reported with the reason it was cut.

Example:

  repocortex context "Add idempotency to payment callback processing"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			task := strings.TrimSpace(strings.Join(args, " "))
			if task == "" {
				return errors.New("task must not be empty")
			}

			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			cfg := g.Config()
			budget := devctx.BudgetFrom(cfg)
			if maxFiles > 0 {
				budget.MaxFiles = maxFiles
			}
			if maxSymbols > 0 {
				budget.MaxSymbols = maxSymbols
			}
			if maxLines > 0 {
				budget.MaxLines = maxLines
			}

			gr := graph.NewForRun(s.db, s.runID)
			pack, err := devctx.NewAssembler().Assemble(ctx, s.reg, gr, s.runID, devctx.Request{
				Task:   task,
				Budget: budget,
			})
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), pack)
			}
			renderPack(cmd.OutOrStdout(), pack, showAll)
			return nil
		},
	}
	cmd.Flags().IntVar(&maxFiles, "max-files", 0, "override the file budget")
	cmd.Flags().IntVar(&maxSymbols, "max-symbols", 0, "override the symbol budget")
	cmd.Flags().IntVar(&maxLines, "max-lines", 0, "override the excerpt line budget")
	cmd.Flags().BoolVar(&showAll, "show-excluded", false,
		"list everything considered and left out, with the reason")
	return cmd
}

// renderPack prints a context pack for a human.
//
// Interpretation leads, because almost every bad pack comes from a misread task
// rather than from bad ranking, and seeing it first turns a confusing result
// into an obvious one.
func renderPack(w io.Writer, pack devctx.Pack, showExcluded bool) {
	fmt.Fprintf(w, "Task: %s\n", pack.Task)
	fmt.Fprintf(w, "Understood as: %s\n\n", pack.Interpretation)

	if len(pack.Items) == 0 {
		fmt.Fprintln(w, "Nothing matched this task.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "KIND\tNAME\tLOCATION\tWHY")
		for _, it := range pack.Items {
			loc := it.Path
			if it.Line > 0 {
				loc = fmt.Sprintf("%s:%d", it.Path, it.Line)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Kind, it.Name, loc, it.Why)
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(w, "\n%d item(s), roughly %d tokens.\n", len(pack.Items), pack.TokenEstimate)

	for _, n := range pack.Notes {
		fmt.Fprintf(w, "\n%s\n", n)
	}

	if !showExcluded || len(pack.Excluded) == 0 {
		if len(pack.Excluded) > 0 {
			fmt.Fprintf(w, "\n%d candidate(s) were considered and excluded; pass --show-excluded to see why.\n",
				len(pack.Excluded))
		}
		return
	}

	// Sorted so that repeated runs are diffable. The engine's order is by
	// score, which is not stable across index revisions.
	ex := append([]devctx.Exclusion(nil), pack.Excluded...)
	sort.Slice(ex, func(i, j int) bool { return ex[i].Name < ex[j].Name })

	fmt.Fprintf(w, "\nExcluded (%d):\n", len(ex))
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, e := range ex {
		fmt.Fprintf(tw, "  %s\t%s\n", e.Name, e.Reason)
	}
	_ = tw.Flush()
}
