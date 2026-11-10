package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scipimport"

	"github.com/spf13/cobra"
)

func newSCIPCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scip",
		Short: "Work with compiler-backed SCIP indexes",
		Long: `SCIP is the format compiler-backed indexers emit — scip-go, scip-java,
scip-typescript, scip-python, scip-clang.

It is the only input RepoCortex accepts as proof. A tree-sitter parse can tell
you that ` + "`svc.updateStatus()`" + ` is a call; it cannot tell you WHICH updateStatus,
because that needs a type for svc, which needs the build graph. So heuristic
resolution tops out at 'high' confidence, and 'exact' is reserved for facts a
typechecker established.`,
	}
	cmd.AddCommand(newSCIPImportCmd(g))
	return cmd
}

func newSCIPImportCmd(g *Global) *cobra.Command {
	var (
		pathPrefix string
		maxBytes   int64
	)

	cmd := &cobra.Command{
		Use:   "import <index.scip>",
		Short: "Import a pre-built SCIP index as compiler-proven relationships",
		Long: `Reads a .scip index and writes its relationships into the current index.

Use this when a compiler-backed index already exists — most often produced by
CI, which has the toolchain and the build cache that a developer's machine may
not. 'cortex indexers install' provisions and runs those tools locally; this
is the path for an index you already have.

WHAT IT DOES TO THE GRAPH. Relationships the compiler proved are written with
confidence 'exact' and source 'compiler'. Nothing is ever downgraded, so
importing a stale index cannot weaken the graph, and importing the same index
twice is a no-op.

CALLS COME FROM A JOIN, NOT FROM SCIP. A SCIP index records that one symbol
references another; it never records that the reference is a call, and RepoCortex
will not stamp a guess about that with 'exact'. So a compiler reference becomes
a CALLS edge only where RepoCortex's OWN parser independently recorded a call
expression at that same file and line, the compiler resolved that line to
exactly one target, and that target is named as one of the calls the parser
recorded there. Every part of that is observed; nothing is inferred. Everything
else lands as REFERENCES — including a call line the compiler resolved to
several targets, and one whose target is merely NEAR a call rather than being
it. The import reports how many were declined on each ground.

Where a compiler fact describes a relationship the resolver already found, at
the same kind and line, the weaker edge is upgraded in place rather than
duplicated. Run 'cortex resolve' before importing: the join needs the
parser's references on disk, and without them nothing can be promoted.

WHAT IT DOES NOT DO. It never adds symbols. Definitions the compiler found that
RepoCortex has not indexed are reported, not invented — writing symbols into a
published index is what the two-phase swap exists to prevent. Expect that count
to be large and do not be alarmed by it: a compiler sees local variables,
struct fields and generated declarations that a tags-based extractor never
emits, so on a real Go repository most of the index's symbols legitimately have
no counterpart. Only a very low match RATE suggests the index and the working
tree have diverged, and the command says so when it does.

PATHS ARE THE USUAL FAILURE. An index built in a container records paths like
/build/repo/src/Main.java. RepoCortex reconciles those against its own file list
and reports how many documents matched. If that count is zero, pass --path-prefix
with the repo-relative directory the index's documents hang off.

Index first, resolve second, import third:

  cortex index
  cortex resolve
  cortex scip import build/index.scip`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			indexPath := args[0]

			info, err := os.Stat(indexPath)
			if err != nil {
				return fmt.Errorf("read SCIP index %q: %w", indexPath, err)
			}
			if info.IsDir() {
				return fmt.Errorf("%q is a directory; pass the .scip file itself", indexPath)
			}
			if info.Size() == 0 {
				return fmt.Errorf("%q is empty; the indexer probably failed — check its output", indexPath)
			}

			cfg := g.Config()
			dbPath := g.DBPath()
			if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("no index at %s — run 'cortex init' first", dbPath)
			}

			// Read-write: this mutates the committed run in place, which is the
			// same posture `cortex watch` takes for incremental updates.
			db, err := openWritable(ctx, g, dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			reg := registry.New(db)
			run, err := reg.CurrentRun(ctx)
			if errors.Is(err, registry.ErrNoCommittedRun) {
				// Rewritten rather than passed through. The registry's own
				// message describes the store's state accurately and tells the
				// reader nothing about what to do next, which is the whole
				// question when a command refuses to run.
				return errors.New("no committed index — run 'cortex index' first, " +
					"then import")
			}
			if err != nil {
				return err
			}

			start := time.Now()
			imp := scipimport.NewImporter(reg, run.ID, scip.Options{
				RepoRoot:   cfg.RepoRoot,
				PathPrefix: pathPrefix,
				MaxBytes:   maxBytes,
			})
			res, err := imp.Import(ctx, indexPath)
			if err != nil {
				return err
			}
			elapsed := time.Since(start)

			g.Log().Info("scip import complete",
				"tool", res.Ingest.Tool.String(),
				"documents_matched", res.Ingest.DocumentsMatched,
				"symbols_matched", res.SymbolsMatched,
				"edges_new", res.Edges.Inserted,
				"edges_upgraded", res.Edges.Upgraded,
				"calls_promoted", res.Promote.Promoted,
				"duration_ms", elapsed.Milliseconds())

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), scipImportReport{
					IndexFile:        filepath.Base(indexPath),
					Tool:             res.Ingest.Tool.String(),
					IndexRevision:    run.ID,
					Documents:        res.Ingest.Documents,
					DocumentsMatched: res.Ingest.DocumentsMatched,
					SymbolsMatched:   res.SymbolsMatched,
					SymbolsUnmatched: res.SymbolsUnmatched,
					EdgesNew:         res.Edges.Inserted,
					EdgesUpgraded:    res.Edges.Upgraded,
					EdgesUnchanged:   res.Edges.Unchanged,
					CallsPromoted:    res.Promote.Promoted,
					CallLines:        res.Promote.CallLines,
					CallLinesAmbig:   res.Promote.Ambiguous,
					CallLinesRenamed: res.Promote.NameMismatch,
					CallLinesRecvr:   res.Promote.Receivers,
					ParsedCallSites:  res.ParsedCallSites,
					Unresolved:       res.RelationsUnresolved,
					FileAnchored:     res.FileAnchored,
					DurationMS:       elapsed.Milliseconds(),
				})
			}

			out := cmd.OutOrStdout()
			fmt.Fprint(out, res.Summary())
			fmt.Fprintf(out, "duration: %v\n", elapsed.Round(time.Millisecond))
			renderImportGuidance(out, res)
			return nil
		},
	}

	cmd.Flags().StringVar(&pathPrefix, "path-prefix", "",
		"repo-relative directory the index's documents hang off, when the index "+
			"records absolute build-container paths")
	cmd.Flags().Int64Var(&maxBytes, "max-bytes", 0,
		"cap the index size in bytes (0 = default, negative = no cap)")
	return cmd
}

type scipImportReport struct {
	IndexFile        string `json:"index_file"`
	Tool             string `json:"tool"`
	IndexRevision    int64  `json:"index_revision"`
	Documents        int64  `json:"documents"`
	DocumentsMatched int64  `json:"documents_matched"`
	SymbolsMatched   int    `json:"symbols_matched"`
	SymbolsUnmatched int    `json:"symbols_unmatched"`
	EdgesNew         int    `json:"edges_new"`
	EdgesUpgraded    int    `json:"edges_upgraded"`
	EdgesUnchanged   int    `json:"edges_unchanged"`
	CallsPromoted    int    `json:"calls_promoted"`
	CallLines        int    `json:"call_lines_adjudicated"`
	CallLinesAmbig   int    `json:"call_lines_ambiguous"`
	CallLinesRenamed int    `json:"call_lines_name_mismatch"`
	CallLinesRecvr   int    `json:"call_lines_receiver_only"`
	ParsedCallSites  int    `json:"parsed_call_sites"`
	Unresolved       int    `json:"relations_unresolved"`
	FileAnchored     int64  `json:"file_anchored"`
	DurationMS       int64  `json:"duration_ms"`
}

// guidanceSamples bounds how many examples each note prints. Five is enough to
// see a shared prefix — the thing that makes a path problem diagnosable — and
// few enough that the numbers above stay visible. The real index this was
// tuned against reported twenty Go build-cache filenames, each 100 characters
// long, which pushed every useful figure off the screen.
const guidanceSamples = 5

// renderImportGuidance turns a partial result into the next thing to try.
//
// A number the reader cannot act on is decoration. Each branch corresponds to a
// specific cause with a specific fix — and the alternative, printing "0 edges"
// and letting the reader guess, is the failure this command is most likely to
// produce.
//
// The thresholds matter as much as the messages. A large unmatched-symbol count
// is NORMAL: a compiler sees local variables, struct fields and generated
// declarations that a tags-based extractor never emits, so on a real Go
// repository three quarters of the index's symbols legitimately have no
// counterpart. Warning about that unconditionally would train the reader to
// ignore the warning that matters.
func renderImportGuidance(w io.Writer, res scipimport.ImportResult) {
	var b strings.Builder

	sample := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		if len(items) > guidanceSamples {
			items = items[:guidanceSamples]
		}
		fmt.Fprintf(&b, "  %s %s\n", label, strings.Join(items, "\n       "))
	}

	// Only worth raising when a meaningful share failed to reconcile. An
	// indexer emitting a handful of documents for its own build cache is
	// routine and is not a problem the reader can or should fix.
	if rate := res.MatchRate(); res.Ingest.DocumentsUnmatched > 0 && rate < 0.95 {
		fmt.Fprintf(&b, "\n%d of %d document(s) matched no indexed file (%.0f%% matched).\n",
			res.Ingest.DocumentsUnmatched, res.Ingest.Documents, rate*100)
		sample("e.g.", res.Ingest.UnmatchedPaths)
		fmt.Fprint(&b, "  If these share a leading directory the repository does not have, "+
			"re-run with --path-prefix.\n")
	}

	// Likewise: flag unmatched definitions only when the match rate is poor
	// enough to suggest the index and the tree have diverged, rather than
	// simply that the compiler sees more than the parser does.
	if res.SymbolsMatched > 0 && res.SymbolsUnmatched > 0 {
		matched := float64(res.SymbolsMatched) / float64(res.SymbolsMatched+res.SymbolsUnmatched)
		if matched < 0.10 {
			fmt.Fprintf(&b, "\nOnly %.0f%% of compiler definitions matched an indexed symbol.\n",
				matched*100)
			sample("e.g.", res.UnmatchedSymbols)
			fmt.Fprint(&b, "  That is low enough to suggest the index and the working tree are at "+
				"different commits; re-run 'cortex index' and import again.\n")
		}
	}

	switch {
	case res.Edges.Inserted == 0 && res.Edges.Upgraded == 0:
		fmt.Fprint(&b, "\nNo relationships changed. The index parsed, but nothing in it was "+
			"attachable to this repository's graph.\n")

	case res.Edges.Upgraded == 0 && res.Edges.Inserted > 0:
		// Explaining a zero the reader is entitled to find suspicious. The
		// mechanism works; it simply does not apply to this pairing, and
		// saying so is better than letting them assume it failed.
		fmt.Fprint(&b, "\nNothing was upgraded from a heuristic edge, which is expected rather "+
			"than a failure: most compiler facts are references the resolver never claimed at "+
			"all, so they arrive as new edges. An upgrade happens only where an existing edge "+
			"agrees on the relationship, the kind and the evidence line.\n")
	}

	// A zero here has two completely different causes and the reader cannot
	// tell them apart from the number alone, so name whichever one applies.
	if res.Promote.Promoted == 0 {
		switch {
		case res.ParsedCallSites == 0:
			fmt.Fprint(&b, "\nNo CALLS edges were produced because this index has no parser "+
				"call sites to join against. Run 'cortex index && cortex resolve' first: "+
				"the compiler supplies what a reference resolves to, the parser supplies "+
				"whether it is a call, and neither half is usable alone.\n")
		case res.Promote.CallLines > 0:
			fmt.Fprintf(&b, "\nNo CALLS edges were produced, though %d parsed call line(s) "+
				"carried a compiler fact — every one resolved to more than one target, so no "+
				"single call target is defined at any of them.\n", res.Promote.CallLines)
		default:
			fmt.Fprint(&b, "\nNo CALLS edges were produced: none of the compiler's references "+
				"landed on a line the parser recorded a call at. If that is surprising, the "+
				"index and the working tree are probably at different commits.\n")
		}
	}

	if b.Len() > 0 {
		_, _ = w.Write([]byte(b.String()))
	}
}
