package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/diffgraph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// newDiffCmd exposes the semantic graph diff.
//
// The distinction that justifies the command: `git diff` reports changed LINES,
// and a reviewer has to reconstruct the facts from them. This reports the facts
// directly — this endpoint now writes `refunds`, this symbol lost its last
// caller, this CALLS edge fell from `exact` to `high` because the compiler
// stopped proving it. A one-line import that shifts a thousand definitions down
// produces a thousand-line patch and an empty semantic diff, which is the whole
// point.
//
// Why it drives off recorded runs rather than git: this package shells out to
// nothing (the same rule `verify` follows for its change set). A diff that ran
// its own `git checkout` would index a tree the caller never asked for and then
// report the result as if it came from the caller's own index. Both sides are
// therefore runs that ALREADY exist in an index, named by run id or by the
// commit SHA the run recorded, and a ref that matches no run is an error rather
// than a silent fallback to whatever run happened to be committed.
func newDiffCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff [<from>..<to>]",
		Short: "Show what changed about the code's FACTS between two indexed runs",
		Long: `Compares two index runs and reports what changed about the code's facts, not
about its lines: symbols added, removed, moved or orphaned; relationships added,
removed or re-graded; endpoints whose handler changed; tables that gained or lost
a reader or a writer.

Both sides name a run that is already in an index. A ref is either a run id or
the commit SHA the run recorded when it was indexed:

  cortex diff --from 41 --to 42
  cortex diff 8f3c1ab..a19d7e0
  cortex diff --from 8f3c1ab            # --to defaults to the committed run

Nothing here invokes git. A ref that matches no indexed run fails and says so —
it never falls back to the run that happens to be committed, because a diff
against the wrong run is worse than no diff at all. To compare against a commit
this index has never seen, index it first ('git checkout <ref> && cortex
index') and diff the run that produces.

Committing a run retires and deletes the one before it, so one index database
holds one committed run. The two sides of a comparison across commits therefore
normally live in two index files — the working index and a second index built at
the other ref — which is what --from-index and --to-index are for:

  cortex diff --from-index ../release-1.4/.cortex/index.db --from 8f3c1ab --to 42

Determinism is the property the whole feature rests on. Identity is content-based
(path, fqn, kind for a symbol; the two endpoints and the kind for an edge), never
a row id, so a difference means the code changed rather than that a worker pool
interleaved differently. Diffing a run against itself reports nothing.

The report always ends with what it could NOT compare: a degraded run, a file
that did not parse, or two runs indexed in different accuracy modes all mean an
absence may be an indexing gap rather than a deletion. Saying so is the
difference between a diff and a guess.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, g, args)
		},
	}

	cmd.Flags().String("from", "", "base (older) side: a run id, or a commit SHA recorded on an indexed run")
	cmd.Flags().String("to", "", "head (newer) side: a run id or commit SHA (default: the committed run)")
	cmd.Flags().String("from-index", "", "read the base side from this index database (default: this project's)")
	cmd.Flags().String("to-index", "", "read the head side from this index database (default: this project's)")
	cmd.Flags().Int("max-per-section", 0, "cap the changes reported per section (0 = default)")
	cmd.Flags().Int("max-evidence-sites", 0, "cap the evidence sites carried per edge fact (0 = default)")
	return cmd
}

func runDiff(cmd *cobra.Command, g *Global, args []string) error {
	ctx := cmd.Context()

	fromRef, toRef, err := diffRefs(cmd, args)
	if err != nil {
		return err
	}
	fromPath, _ := cmd.Flags().GetString("from-index")
	toPath, _ := cmd.Flags().GetString("to-index")

	cat := &indexCatalogue{g: g, byPath: map[string]*diffIndex{}}
	defer cat.closeAll()

	base, err := resolveSide(ctx, cat, fromPath, fromRef)
	if err != nil {
		return err
	}
	head, err := resolveSide(ctx, cat, toPath, toRef)
	if err != nil {
		return err
	}

	var opts []diffgraph.Option
	if n, _ := cmd.Flags().GetInt("max-per-section"); n > 0 {
		opts = append(opts, diffgraph.WithMaxPerSection(n))
	}
	if n, _ := cmd.Flags().GetInt("max-evidence-sites"); n > 0 {
		opts = append(opts, diffgraph.WithMaxEvidenceSites(n))
	}
	d, err := diffgraph.New(opts...)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}

	rep, err := d.Compare(ctx, base, head)
	if err != nil {
		return fmt.Errorf("diff: compare runs %d..%d: %w", base.RunID, head.RunID, err)
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		return emitJSON(cmd.OutOrStdout(), rep)
	}
	if err := diffgraph.Render(cmd.OutOrStdout(), rep); err != nil {
		return fmt.Errorf("diff: render report: %w", err)
	}
	return nil
}

// diffRefs reads the two refs from either the range argument or the flags.
//
// The two spellings are not allowed to mix. `cortex diff a..b --from c` has
// two plausible readings and no way to tell which one the caller meant, and
// picking one silently is how a tool ends up diffing something nobody asked for.
func diffRefs(cmd *cobra.Command, args []string) (from, to string, err error) {
	from, _ = cmd.Flags().GetString("from")
	to, _ = cmd.Flags().GetString("to")

	if len(args) == 1 {
		if from != "" || to != "" {
			return "", "", errors.New("pass either <from>..<to> or --from/--to, not both")
		}
		a, b, ok := strings.Cut(args[0], "..")
		if !ok {
			return "", "", fmt.Errorf(
				"%q is not a range: write it as <from>..<to>, where each side is a run id "+
					"or a commit SHA recorded on an indexed run", args[0])
		}
		from, to = strings.TrimSpace(a), strings.TrimSpace(b)
	}

	if from = strings.TrimSpace(from); from == "" {
		return "", "", errors.New(
			"--from is required: diff compares two runs, and it will not guess which one is the base")
	}
	return from, strings.TrimSpace(to), nil
}

// resolveSide turns one ref into a diffgraph.Side over a real, existing run.
func resolveSide(ctx stdctx.Context, cat *indexCatalogue, indexPath, ref string) (diffgraph.Side, error) {
	idx, err := cat.get(ctx, indexPath)
	if err != nil {
		return diffgraph.Side{}, err
	}
	runID, err := idx.resolve(ref)
	if err != nil {
		return diffgraph.Side{}, err
	}
	return diffgraph.Side{Source: idx.reg, RunID: runID}, nil
}

// ---------------------------------------------------------------- indexes

// runRef is one row of an index's run catalogue: enough to resolve a ref and to
// tell the caller what was on offer when resolution fails.
type runRef struct {
	id        int64
	revision  string
	status    string
	committed bool
}

// diffIndex is one opened index plus the runs recorded in it.
//
// It holds a *registry.Registry rather than a store handle because that is what
// satisfies diffgraph.IndexSource — the port is declared at the consumer, and
// internal/diffgraph carries the compile-time proof that the registry satisfies
// it, so nothing here has to adapt anything.
type diffIndex struct {
	path      string
	reg       *registry.Registry
	runs      []runRef // ordered by id, so every message derived from it is stable
	committed int64    // 0 when this index has no committed run
	closeFn   func()
}

// indexCatalogue opens each distinct index at most once.
//
// Both sides usually name the same database, and opening it twice would mean
// two connection pools over one SQLite file for no gain.
type indexCatalogue struct {
	g      *Global
	byPath map[string]*diffIndex
	order  []*diffIndex // close order, so a failure late in the command is reproducible
}

func (c *indexCatalogue) get(ctx stdctx.Context, path string) (*diffIndex, error) {
	key := c.g.DBPath()
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("diff: resolve index path %q: %w", path, err)
		}
		key = abs
	}
	if idx, ok := c.byPath[key]; ok {
		return idx, nil
	}

	var (
		idx *diffIndex
		err error
	)
	if key == c.g.DBPath() {
		idx, err = openProjectIndex(ctx, c.g)
	} else {
		idx, err = openForeignIndex(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	c.byPath[key] = idx
	c.order = append(c.order, idx)
	return idx, nil
}

func (c *indexCatalogue) closeAll() {
	for _, idx := range c.order {
		idx.closeFn()
	}
}

// openProjectIndex opens this project's own index through the shared query
// session, so `diff` reports a missing index and an uncommitted one exactly the
// way every other read command does.
func openProjectIndex(ctx stdctx.Context, g *Global) (*diffIndex, error) {
	s, err := openQuery(ctx, g)
	if err != nil {
		return nil, err
	}
	runs, err := listRuns(ctx, s.db)
	if err != nil {
		s.close()
		return nil, err
	}
	return &diffIndex{
		path:      g.DBPath(),
		reg:       s.reg,
		runs:      runs,
		committed: s.runID,
		closeFn:   s.close,
	}, nil
}

// openForeignIndex opens a second index named by --from-index/--to-index.
//
// Read-only, like every query path: the other checkout's index may be in use by
// a `cortex watch` over there, and a diff has no business writing to it.
func openForeignIndex(ctx stdctx.Context, path string) (*diffIndex, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"no index at %s — build one in that checkout first: 'cortex index --project <that checkout>'", path)
	} else if err != nil {
		return nil, fmt.Errorf("diff: open index %s: %w", path, err)
	}

	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("diff: open index %s: %w", path, err)
	}
	runs, err := listRuns(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	var committed int64
	for _, r := range runs {
		if r.committed {
			committed = r.id
		}
	}
	return &diffIndex{
		path:      path,
		reg:       registry.New(db),
		runs:      runs,
		committed: committed,
		closeFn:   func() { _ = db.Close() },
	}, nil
}

// listRuns reads the run catalogue.
//
// Read here rather than through the registry because the registry's read port
// is deliberately single-run — CurrentRun for readers, Run(id) for operational
// tooling — and nothing in it enumerates. The CLI already discovers runs by this
// same query in openQuery and in `status`, and this stays a projection of four
// columns rather than anything that leaks SQL outward.
func listRuns(ctx stdctx.Context, db *store.DB) ([]runRef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(git_revision,''), status, committed
		  FROM index_runs
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("diff: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []runRef
	for rows.Next() {
		var r runRef
		var committed int
		if err := rows.Scan(&r.id, &r.revision, &r.status, &committed); err != nil {
			return nil, fmt.Errorf("diff: read run row: %w", err)
		}
		r.committed = committed == 1
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("diff: list runs: %w", err)
	}
	return out, nil
}

// resolve turns a ref into a run id that exists in this index.
//
// A numeric ref is tried as a run id first and then as a revision, because a
// short SHA can be all digits and refusing "8317294" outright would be a wrong
// answer dressed as a strict one. Every other outcome is an error: an
// unmatched ref must never resolve to "whatever was committed", which is the
// one failure that would make the whole report a lie.
func (idx *diffIndex) resolve(ref string) (int64, error) {
	if ref == "" {
		if idx.committed == 0 {
			return 0, fmt.Errorf(
				"no committed run in %s to use as the default side — run 'cortex index', "+
					"or name both sides explicitly", idx.path)
		}
		return idx.committed, nil
	}

	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		for _, r := range idx.runs {
			if r.id == n {
				return r.id, nil
			}
		}
	}

	var hits []runRef
	want := strings.ToLower(ref)
	for _, r := range idx.runs {
		if r.revision != "" && strings.HasPrefix(strings.ToLower(r.revision), want) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0].id, nil
	case 0:
		return 0, idx.errNoSuchRun(ref)
	default:
		var b strings.Builder
		for i, r := range hits {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "run %d (%s)", r.id, r.revision)
		}
		return 0, fmt.Errorf("%q matches %d indexed runs — %s; name a run id or a longer SHA",
			ref, len(hits), b.String())
	}
}

// errNoSuchRun is the message the design turns on: it says the ref matched no
// indexed run, names how to produce one, and lists what this index does hold,
// so nobody has to guess whether they mistyped a SHA or never indexed it.
func (idx *diffIndex) errNoSuchRun(ref string) error {
	// A numeric ref was read as a run id first, so "git checkout 99" would be
	// nonsense advice; say what was actually tried instead.
	how := fmt.Sprintf("Index that commit and diff the run it produces:\n"+
		"  git checkout %s && cortex index\n", ref)
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		how = "No run carries that id and no recorded commit SHA starts with it. " +
			"Index the commit you meant ('git checkout <sha> && cortex index') " +
			"and diff the run it produces.\n"
	}
	return fmt.Errorf("no indexed run matches %q: nothing in %s was indexed at that ref.\n%s%s",
		ref, idx.path, how, idx.knownRuns())
}

// knownRuns renders the catalogue, in id order so the message is deterministic.
func (idx *diffIndex) knownRuns() string {
	if len(idx.runs) == 0 {
		return fmt.Sprintf("%s records no runs at all — run 'cortex index' first.", idx.path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runs recorded in %s:\n", idx.path)
	for _, r := range idx.runs {
		fmt.Fprintf(&b, "  run %d  rev %s  status %s%s\n",
			r.id, orDash(r.revision), r.status, committedMark(r.committed))
	}
	return b.String()
}

func committedMark(committed bool) string {
	if committed {
		return "  (committed)"
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
