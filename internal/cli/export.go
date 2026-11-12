package cli

import (
	"bytes"
	stdctx "context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/export"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// `repocortex export`: the index, in somebody else's format.
//
// Everything else RepoCortex prints is an ANSWER — a blast radius, a caller list,
// a review routing. This command prints the GRAPH, and it does so because the
// useful thing to do with a code graph is frequently not something RepoCortex
// can do: lay it out in Graphviz, open it in Gephi, join it against a coverage
// report in a notebook, diff two revisions of it in git. A tool that can only
// be queried through its own surfaces is a tool whose ceiling is whatever its
// authors thought of.
//
// The whole command is an adapter, in the CLAUDE.md §1 sense: it reads through
// internal/registry, builds the domain-shaped metrics.Graph, and hands it to
// internal/export. internal/export never learns that SQLite exists, which is
// what keeps it testable against a graph literal.

// exportOptions is one `repocortex export` invocation.
//
// The cobra command and the global config are carried as fields rather than as
// parameters so the flag-shaped inputs above stay separable from the plumbing
// below: a test can construct an exportOptions with nothing but a Format and
// assert on the refusal, which is exactly what the format-validation test does.
type exportOptions struct {
	Format        string
	Out           string
	MinConfidence string

	g   *Global
	cmd *cobra.Command
}

func newExportCmd(g *Global) *cobra.Command {
	var opts exportOptions

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write the code graph in an interchange format",
		Long: `Writes the committed index's symbol graph in a format other tools read.

  repocortex export --format dot | dot -Tsvg -o graph.svg
  repocortex export --format graphml --out graph.graphml     # Gephi, yEd, Cytoscape
  repocortex export --format json --out graph.json           # notebooks, jq, diffing
  repocortex export --format csv --out edges.csv             # spreadsheets, pandas
  repocortex export --format svg --out graph.svg             # no graphviz required

Every format carries edge DIRECTION from the edge itself, and every format that
can hold a confidence level does. A format that cannot hold something says so
on stderr before it writes — stderr, not stdout, so a pipeline reading the
export is unaffected by the warning meant for the person who typed the command.

--min-confidence drops facts below a floor. The default is 'low', which keeps
everything the index holds; raising it to 'high' or 'exact' exports only what
RepoCortex is confident about, at the cost of a sparser graph.

Writes to stdout unless --out is given. With --out the file is written beside
its destination and renamed into place, so an interrupted export never replaces
a good artifact with a truncated one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.g = g
			opts.cmd = cmd
			return runExport(opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.Format, "format", "", fmt.Sprintf("output format (%s)", formatList()))
	f.StringVar(&opts.Out, "out", "", "write to this file instead of stdout")
	f.StringVar(&opts.MinConfidence, "min-confidence", string(model.ConfidenceLow),
		"confidence floor for exported facts (exact|high|medium|low)")
	if err := cmd.MarkFlagRequired("format"); err != nil {
		// MarkFlagRequired only fails on a flag that was never registered, two
		// lines above. Panicking here would be a naked panic in production
		// logic; the command is still usable without the annotation.
		g.Log().Warn("export: could not mark --format required", "error", err)
	}
	return cmd
}

func formatList() string {
	fs := export.Formats()
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, string(f))
	}
	return strings.Join(names, "|")
}

// runExport resolves the format, loads the graph, and emits it.
//
// The format is resolved FIRST, before the index is opened. A typo in --format
// should cost nothing and should be answered by naming the formats that exist —
// export.ForFormat's error already does that, so it is returned unchanged
// rather than reworded into a second, divergent list.
func runExport(opts exportOptions) error {
	ex, err := export.ForFormat(export.Format(strings.TrimSpace(opts.Format)))
	if err != nil {
		return err
	}

	floor := model.Confidence(strings.TrimSpace(opts.MinConfidence))
	if opts.MinConfidence == "" {
		floor = model.ConfidenceLow
	}
	if !floor.Valid() {
		return fmt.Errorf("--min-confidence %q is not one of exact, high, medium, low", opts.MinConfidence)
	}

	// --out IS NOT VALIDATED WITH internal/security, AND THAT IS DELIBERATE.
	// ContainedPath and ValidatePath refuse absolute paths and confine writes
	// to the repository root, which is the right rule for a path an AGENT
	// supplied. Here the path was typed by the operator into their own shell,
	// where `repocortex export --out /tmp/graph.dot` is the ordinary case and
	// `> /tmp/graph.dot` would have worked anyway: containment would block a
	// legitimate use and protect nobody. RE-APPLY security.ValidatePath (or
	// ContainedPath, preferred in new code) the moment this reaches MCP, a
	// server handler, or any other caller where the path is not the operator's
	// own keystrokes.
	//
	// Nil-guarded because runExport is reachable from a test with a bare
	// exportOptions, and a panic on a nil *cobra.Command would be a crash in
	// production code caused by nothing but call order.
	ctx := stdctx.Background()
	out, errOut := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if opts.cmd != nil {
		if c := opts.cmd.Context(); c != nil {
			ctx = c
		}
		out, errOut = opts.cmd.OutOrStdout(), opts.cmd.ErrOrStderr()
	}

	s, err := openQuery(ctx, opts.g)
	if err != nil {
		return err
	}
	defer s.close()

	g, st, err := loadExportGraph(ctx, s, floor)
	if err != nil {
		return err
	}

	// Before the document, not after: a reader who pipes stdout onward should
	// see the caveats whether or not the write succeeds.
	reportExportProvenance(errOut, exportProvenanceOf(ctx, s))
	reportExportDeltas(errOut, st, floor)

	return emitExport(ctx, ex, g, out, errOut, opts.Out)
}

// exportStats is the accounting for one export: everything read, and every
// reason something read did not reach the output.
//
// It exists because the command used to print "wrote 8494 nodes and 8431
// edges" over an index holding 8916 symbols and 8469 edges and say nothing
// about the difference. A number with no denominator is not a report, and an
// artifact sold as joinable against a diff report has to be able to say what
// is not in it.
type exportStats struct {
	symbolsRead       int
	symbolsBelowFloor int
	symbolsUnkeyable  int // no FQN and no name: nothing to call the node
	nodesKept         int // metrics.Graph.Order()
	disambiguated     int // keys that collided and were made distinct

	edgesRead       int
	edgesBelowFloor int
	edgesDangling   int // an endpoint was dropped above, so the edge has none
	edgesSelfLoop   int // from == to: metrics.NewGraph will not keep it
	edgesKept       int // len(metrics.Graph.Edges())
}

// edgesDropped is the sum of the named reasons, so a test can assert that
// read minus kept is fully explained rather than approximately explained.
func (st exportStats) edgesDropped() int {
	return st.edgesBelowFloor + st.edgesDangling + st.edgesSelfLoop
}

// loadExportGraph reads the committed run into the shape internal/export takes.
func loadExportGraph(ctx stdctx.Context, s *session, floor model.Confidence) (metrics.Graph, exportStats, error) {
	files, err := s.reg.Files(ctx, s.runID)
	if err != nil {
		return metrics.Graph{}, exportStats{}, fmt.Errorf("export: read files of run %d: %w", s.runID, err)
	}
	paths := make(map[int64]string, len(files))
	for _, f := range files {
		paths[f.ID] = f.Path
	}

	symbols, err := s.reg.Symbols(ctx, s.runID)
	if err != nil {
		return metrics.Graph{}, exportStats{}, fmt.Errorf("export: read symbols of run %d: %w", s.runID, err)
	}
	edges, err := s.reg.Edges(ctx, s.runID)
	if err != nil {
		return metrics.Graph{}, exportStats{}, fmt.Errorf("export: read edges of run %d: %w", s.runID, err)
	}
	return buildExportGraph(ctx, paths, symbols, edges, floor)
}

// buildExportGraph turns rows into the domain graph, and counts what it loses.
//
// Split from the I/O above so the keying rule — the part that was wrong — can
// be tested against a four-symbol fixture instead of a SQLite file.
//
// NODE KEY. The key is the symbol's FQN, falling back to
// model.DefaultFQN(path, name) — "path/to/file.go:Name" — for a symbol whose
// extractor left the FQN empty. That is not a choice made here; it is the
// identity every other surface in RepoCortex already uses. internal/analyze
// builds enclosure spans against it, internal/diffgraph keys symbols by it,
// and internal/export/csv.go's own doc comment states that its From/To derive
// from it. An exported id therefore means the same entity as an id in a diff
// report or a blast radius, which is the only property that makes an export
// worth joining against anything.
//
// AN FQN IS NOT AN IDENTITY, THOUGH, and this is where the rule earns its
// exception. Over this repository's own index 8,916 symbols produce 8,494
// distinct FQNs: 422 symbols share a name with another symbol, and
// metrics.NewGraph keeps the first and silently discards the rest. The loss is
// not even the worst of it — collapsing three String() methods onto one node
// RE-PARENTS all their edges, so the artifact does not merely omit a
// relationship, it asserts one that does not exist. RepoCortex's own schema
// already knows FQN is insufficient: symbols is UNIQUE(index_run_id, fqn,
// file_id, start_line).
//
// So a key is disambiguated ONLY when it collides, and the discriminator
// applied is chosen in the order PATH, then LINE, then ORDINAL:
//
//   - PATH first, as "<key>@<path>", because the commonest collision in this
//     repository's own index is the same FQN in unrelated files — "config:name"
//     at line 1 of five different .github/workflows/*.yml files, "table:edges"
//     at line 1 of three migration files. A "#L1" suffix on those names no
//     file at all; "@path" does, and it is the one property a reader actually
//     wants from a collided id. Path is tried before line for exactly this
//     reason: most collisions this index produces are cross-file, and line
//     alone cannot say which file.
//   - LINE next, as "#L<line>", only for the symbols that STILL collide after
//     "@path" — i.e. two or more symbols sharing both an FQN and a file, such
//     as the three String() methods on different types in one file. Path
//     cannot discriminate those; line can, because RepoCortex's schema is
//     UNIQUE(index_run_id, fqn, file_id, start_line).
//   - ORDINAL last, as "#<n>", only for the residual tie the index does hold a
//     handful of: identical FQN, file, AND line (duplicate parses of the same
//     config key or SQL DDL). Assigned in Registry.Symbols' own row order
//     (path, start_line, fqn — confirmed total for this index), so it is
//     deterministic without depending on any other symbol's key.
//
// "@path" is appended whenever a collision is being disambiguated at all, even
// when the FQN already happens to embed the path (as file-scoped fallback FQNs
// do) — the alternative is parsing the FQN to detect whether it already names
// the file, which is a heuristic this function does not need when uniformly
// appending the path is simpler and never wrong.
//
// Both properties that matter about the scheme hold by construction, not by
// the order symbols happen to load in: each symbol's key depends only on ITS
// OWN base key and ITS OWN path (plus line/ordinal, when its own file-group
// still ties) — never on how many distinct paths the run happens to contain.
// So adding an unrelated file never renames a node that lives elsewhere, which
// is the stability the ordinal-only scheme broke: ordinals used to be assigned
// in path order, so a new workflow file sorting ahead of the existing ones
// renumbered every one of them.
//
// Collision-free keys are byte-identical to what they were, so the
// diffability that made FQN the right key in the first place survives; a
// colliding key changes only when the symbol's own file or line changes,
// which is a change a diff should show anyway.
//
// The floor is applied to symbols AND edges. A symbol below the floor takes
// its edges with it, which is the correct reading of "export only facts at
// least this confident": an edge between two entities RepoCortex is not sure
// exist is not a fact worth more than they are. Every such drop is counted
// into exportStats and reported by reportExportDeltas — nothing leaves this
// function unaccounted for.
func buildExportGraph(
	ctx stdctx.Context,
	paths map[int64]string,
	symbols []model.Symbol,
	edges []model.Edge,
	floor model.Confidence,
) (metrics.Graph, exportStats, error) {
	st := exportStats{symbolsRead: len(symbols), edgesRead: len(edges)}

	// First pass counts base keys, because whether a key needs disambiguating
	// is a property of the whole run, not of the symbol in hand.
	base := make(map[int64]string, len(symbols))
	shared := make(map[string]int, len(symbols))
	keyed := make([]model.Symbol, 0, len(symbols))
	for _, sym := range symbols {
		if !sym.Confidence.AtLeast(floor) {
			st.symbolsBelowFloor++
			continue
		}
		k := exportKey(sym, paths[sym.FileID])
		if k == "" {
			st.symbolsUnkeyable++
			continue
		}
		base[sym.ID] = k
		shared[k]++
		keyed = append(keyed, sym)
	}

	// A disambiguated key must not land on a base key some other symbol will
	// claim later, so the unshared ones are reserved before any is minted.
	taken := make(map[string]bool, len(keyed))
	for k, n := range shared {
		if n == 1 {
			taken[k] = true
		}
	}

	// Second pass, within each colliding base key, counts how many symbols
	// share both that key AND a file. That count decides whether "@path" alone
	// resolves a symbol's collision or whether line is also needed — and it is
	// computed per (base key, path), so it depends on nothing about the run
	// except the symbols that actually share this base key and this file.
	pathTies := make(map[string]map[string]int, len(shared))
	for _, sym := range keyed {
		k := base[sym.ID]
		if shared[k] <= 1 {
			continue
		}
		byPath := pathTies[k]
		if byPath == nil {
			byPath = make(map[string]int)
			pathTies[k] = byPath
		}
		byPath[paths[sym.FileID]]++
	}

	nodes := make([]metrics.Node, 0, len(keyed))
	keyByID := make(map[int64]string, len(keyed))
	for _, sym := range keyed {
		k := base[sym.ID]
		if shared[k] > 1 {
			p := paths[sym.FileID]
			var want string
			if pathTies[k][p] == 1 {
				want = fmt.Sprintf("%s@%s", k, p)
			} else {
				want = fmt.Sprintf("%s@%s#L%d", k, p, sym.Range.Start.Line)
			}
			k = uniqueKey(taken, want)
			st.disambiguated++
		}
		taken[k] = true
		keyByID[sym.ID] = k
		nodes = append(nodes, metrics.Node{Key: k, Kind: sym.Kind})
	}

	me := make([]metrics.Edge, 0, len(edges))
	for _, e := range edges {
		if !e.Confidence.AtLeast(floor) {
			st.edgesBelowFloor++
			continue
		}
		from, okFrom := keyByID[e.SrcID]
		to, okTo := keyByID[e.DstID]
		if !okFrom || !okTo {
			st.edgesDangling++
			continue
		}
		if from == to {
			// metrics.NewGraph drops these and this package may not change
			// that, so they are counted here where the reason is still known:
			// direct recursion, which is a real fact the artifact will lack.
			st.edgesSelfLoop++
			continue
		}
		me = append(me, metrics.Edge{
			From: from, To: to,
			Kind: e.Kind, Confidence: e.Confidence, Weight: 1,
		})
	}

	if err := ctx.Err(); err != nil {
		return metrics.Graph{}, st, fmt.Errorf("export: %w", err)
	}
	g := metrics.NewGraph(nodes, me)
	st.nodesKept = g.Order()
	st.edgesKept = len(g.Edges())
	return g, st, nil
}

// exportKey is the one place the base node-key rule is spelled, so the two
// branches cannot drift apart the way a duplicated fallback would. The
// collision suffix is NOT applied here: whether a key collides is a property
// of the whole run, which only buildExportGraph can see.
func exportKey(s model.Symbol, path string) string {
	if s.FQN != "" {
		return s.FQN
	}
	if s.Name == "" {
		return ""
	}
	return model.DefaultFQN(path, s.Name)
}

// uniqueKey returns want, or want with the lowest "#n" that nobody holds.
//
// The suffix is a last resort for rows the index cannot tell apart at all —
// the same name at the same line of the same file, which happens for config
// keys and SQL DDL parsed by more than one extractor. Dropping them would be
// the defect this whole function exists to remove, so they get a number.
func uniqueKey(taken map[string]bool, want string) string {
	if !taken[want] {
		return want
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s#%d", want, n)
		if !taken[c] {
			return c
		}
	}
}

// reportExportDeltas states, on stderr, everything that did not reach the
// output — and explains an empty output, which is the most ambiguous result of
// all.
//
// This is the rule internal/cli/query.go states in its header and
// internal/mcp/capabilities.go repeats: an empty relationship result means
// either "nothing is there" or "RepoCortex could not see it", and those have
// opposite consequences. A bare `digraph repocortex {}` with exit 0 lets the
// reader pick the wrong one.
//
// errOut, never stdout: `repocortex export --format json > graph.json` is the
// ordinary invocation and a caveat written there is a parse error, not a caveat.
func reportExportDeltas(errOut io.Writer, st exportStats, floor model.Confidence) {
	if lost := st.symbolsRead - st.nodesKept; lost > 0 {
		fmt.Fprintf(errOut, "note: %d of %d symbol(s) are not in this export", lost, st.symbolsRead)
		reasons := make([]string, 0, 3)
		if st.symbolsBelowFloor > 0 {
			reasons = append(reasons, fmt.Sprintf("%d below --min-confidence %s", st.symbolsBelowFloor, floor))
		}
		if st.symbolsUnkeyable > 0 {
			reasons = append(reasons, fmt.Sprintf("%d have neither an FQN nor a name", st.symbolsUnkeyable))
		}
		if unexplained := lost - st.symbolsBelowFloor - st.symbolsUnkeyable; unexplained > 0 {
			// Should be unreachable now that keys are disambiguated. Printed
			// rather than assumed away: an unexplained loss the reader can see
			// is a bug report, an invisible one is the defect being fixed here.
			reasons = append(reasons, fmt.Sprintf("%d for no reason this command can name — please report", unexplained))
		}
		fmt.Fprintf(errOut, " (%s)\n", strings.Join(reasons, ", "))
	}
	if st.disambiguated > 0 {
		fmt.Fprintf(errOut,
			"note: %d symbol(s) share a fully-qualified name with another; their node ids carry "+
				"a '@<path>' (and, if that still ties, '#L<line>') suffix so no symbol is dropped "+
				"and no edge is re-parented\n",
			st.disambiguated)
	}
	if lost := st.edgesRead - st.edgesKept; lost > 0 {
		reasons := make([]string, 0, 3)
		if st.edgesBelowFloor > 0 {
			reasons = append(reasons, fmt.Sprintf("%d below --min-confidence %s", st.edgesBelowFloor, floor))
		}
		if st.edgesDangling > 0 {
			reasons = append(reasons, fmt.Sprintf("%d have an endpoint that is not in this export", st.edgesDangling))
		}
		if st.edgesSelfLoop > 0 {
			reasons = append(reasons, fmt.Sprintf("%d are self-loops (direct recursion), which the graph model does not hold", st.edgesSelfLoop))
		}
		if unexplained := lost - st.edgesDropped(); unexplained > 0 {
			reasons = append(reasons, fmt.Sprintf("%d for no reason this command can name — please report", unexplained))
		}
		fmt.Fprintf(errOut, "note: %d of %d edge(s) are not in this export (%s)\n",
			lost, st.edgesRead, strings.Join(reasons, ", "))
	}
	if st.nodesKept == 0 {
		fmt.Fprintf(errOut, "note: the exported graph is empty — %s\n", emptyExportReason(st, floor))
	}
}

// emptyExportReason names the cause of an empty export, in the terms the
// reader can act on.
//
// The 'exact' case is the one worth spelling out: it is not a strict-but-
// reachable setting on this index. model.Edge.Validate refuses
// confidence=exact unless source=compiler, so exact facts exist only where a
// SCIP index produced by a real typechecker was ingested. Without one the
// floor can never match anything, and "try a lower floor" is the whole remedy.
func emptyExportReason(st exportStats, floor model.Confidence) string {
	switch {
	case st.symbolsRead == 0:
		return "the committed index holds no symbols at all; run 'repocortex index' first"
	case floor == model.ConfidenceExact:
		return fmt.Sprintf(
			"all %d symbol(s) were dropped by --min-confidence exact. 'exact' means a compiler "+
				"proved it, which in RepoCortex means the fact came from an ingested SCIP index; "+
				"this index has none, so no floor of 'exact' can ever match. Re-run with "+
				"--min-confidence high (or low) to export what it does hold",
			st.symbolsRead)
	case st.symbolsBelowFloor > 0:
		return fmt.Sprintf("all %d symbol(s) were dropped by --min-confidence %s; lower the floor",
			st.symbolsRead, floor)
	default:
		return "no symbol in the committed index could be given a node id"
	}
}

// emitExport writes the graph, announcing what the format cannot carry first.
//
// The success line reports Exporter.Counts, not the graph's own totals. For an
// edge-list format those differ, and printing the graph's numbers put "wrote
// 9059 nodes" three lines below a note saying nodes with no edges are omitted
// — two claims about one file, on one stream, one of them false.
//
// The lossy note goes to errOut, never to out. `repocortex export --format csv >
// edges.csv` is the ordinary invocation, and a caveat written into that stream
// is not a caveat — it is a corrupt header row that the reader will discover
// as a parse error a week later.
//
// Buffered to memory before an --out write, on purpose. The exporters stream,
// and streaming straight into the destination file is exactly the case
// writeAtomic exists to prevent: an exporter that fails a third of the way
// through would leave a syntactically valid, silently truncated artifact where
// yesterday's good one was.
func emitExport(ctx stdctx.Context, ex export.Exporter, g metrics.Graph, out, errOut io.Writer, path string) error {
	if note := ex.Lossy(); note != "" {
		fmt.Fprintf(errOut, "note: %s export is lossy — %s\n", ex.Format(), note)
	}

	if strings.TrimSpace(path) == "" {
		if err := ex.Write(ctx, out, g); err != nil {
			return fmt.Errorf("export %s: %w", ex.Format(), err)
		}
		return nil
	}

	var buf bytes.Buffer
	if err := ex.Write(ctx, &buf, g); err != nil {
		return fmt.Errorf("export %s: %w", ex.Format(), err)
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return fmt.Errorf("export %s to %s: %w", ex.Format(), path, err)
	}
	nodes, edges := ex.Counts(g)
	fmt.Fprintf(errOut, "wrote %d nodes and %d edges to %s\n", nodes, edges, path)
	return nil
}

// writeAtomic replaces path with data, or leaves path exactly as it was.
//
// The temp file is created in the DESTINATION DIRECTORY rather than in
// os.TempDir, because os.Rename is only atomic within a filesystem: a temp file
// on a different mount turns the rename into a copy, and a copy is the
// non-atomic write this function exists to avoid.
//
// Sync before rename is the other half. Rename orders the directory entry, not
// the file's data blocks; without the fsync a crash between the two can leave
// the new name pointing at a zero-length file, which is a worse outcome than
// either the old content or no file at all.
func writeAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()

	// Any failure past this point must take the temp file with it: a leaked
	// dotfile beside the artifact is litter the user has no way to attribute.
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	// 0600 from CreateTemp is too strict for an artifact meant to be opened by
	// another tool; 0644 matches what a shell redirection would have produced.
	if err = os.Chmod(tmp, 0o644); err != nil { //nolint:gosec // G302: a document for another tool, not a secret; 0644 matches a shell redirection
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	return nil
}

// exportProvenance is what an export artifact describes: which index run, and
// which commit that run was built at.
type exportProvenance struct {
	runID    int64
	revision string

	// stale is session.stale: the sentence saying this index was written by a
	// different build than the one reading it, or "" when there is nothing to
	// say. See staleAgainst.
	stale string
}

// exportProvenanceOf reads the run row for the provenance line.
//
// A failure to read it is not a failure to export. The artifact is the
// product; the revision is a caveat on it, and refusing to write a graph
// because the run table would not say which commit it came from would be a
// worse trade than writing it with the commit left unstated.
func exportProvenanceOf(ctx stdctx.Context, s *session) exportProvenance {
	p := exportProvenance{runID: s.runID, stale: s.stale}
	if run, err := s.reg.Run(ctx, s.runID); err == nil && run != nil {
		p.revision = run.GitRevision
	}
	return p
}

// reportExportProvenance states what the artifact describes, on stderr.
//
// stderr, not stdout, for the reason emitExport gives about the lossy note:
// `repocortex export --format csv > edges.csv` is the ordinary invocation and a
// provenance header written into that stream is a corrupt first row.
//
// This was the only new surface with no provenance at all. Every MCP response
// carries index_revision and a Provenance caveat; `repocortex impact` refuses to
// answer quietly when the index was built outside the range it was asked
// about. An export outlives its session — it gets committed, attached to a
// ticket, joined against something in a notebook — so it is the artifact that
// can least afford to be silent about which index produced it. The wording
// deliberately reuses those surfaces' vocabulary rather than inventing a third.
func reportExportProvenance(errOut io.Writer, p exportProvenance) {
	at := "no commit recorded"
	if p.revision != "" {
		at = "built at commit " + shortRevision(p.revision)
	}
	fmt.Fprintf(errOut, "index_revision: %d  %s\n", p.runID, at)
	if p.stale != "" {
		fmt.Fprintf(errOut, "note: %s\n", p.stale)
	}
}

// shortRevision abbreviates a SHA to the seven characters git itself shows.
// Anything shorter than that is passed through: a caller-supplied revision
// name is not always a SHA, and truncating "HEAD~1" would be a fabrication.
func shortRevision(rev string) string {
	if len(rev) <= 7 {
		return rev
	}
	return rev[:7]
}
