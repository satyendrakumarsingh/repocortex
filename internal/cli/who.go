package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/ownership"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// newWhoCmd routes a review by REACHABILITY rather than by directory.
//
// The name is short and it is a question, which is the register the other query
// commands use — `cortex callers`, `cortex references`, `cortex context`.
// It also invites exactly one misreading, "who owns this", and that misreading
// is answered in the first line of the command's own output rather than in a
// document nobody opens: the heading says CONTEXT, every row says "has touched",
// and the closing section states what git history can and cannot support. A
// name that dodged the question ('cortex reviewers') would have been quieter
// and would have made the same claim less visibly.
func newWhoCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "who <symbol>",
		Short: "Who has context on the code a change to this symbol reaches",
		Long: `Computes the blast radius of a change to a symbol, then asks git history who
has touched the files that radius lands in — ranked by how much of the radius
they have touched.

This is not CODEOWNERS. CODEOWNERS routes by the directory a file sits in, which
is the right answer to "who administers this path" and the wrong answer to "who
has seen the code this change can break": a two-line edit to a shared decoder
reaches thirty call sites in six packages, and the person listed for the
decoder's directory may never have touched any of them.

  cortex who PaymentService.capture
  cortex who pkg.Decode --depth 2 --commits 500

What this reports is CONTEXT, not ownership. Git records that somebody changed a
file — not that they understand it, own it, still work here, or introduced a
defect. Attribution is per file rather than per line, the history window is
bounded, and a git identity is an address rather than a person. Every one of
those limits is printed with the answer, because a list of names read without
them turns into an accusation the first time something breaks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWho(cmd, g, args[0])
		},
	}

	cmd.Flags().Int("depth", 0, "hops to follow out from the symbol (0 = default)")
	cmd.Flags().String("min-confidence", "", "confidence floor for the blast radius (exact|high|medium|low)")
	cmd.Flags().Int("commits", 0, "how far back from HEAD to read history (0 = default)")
	cmd.Flags().Int("limit", 0, "maximum identities to list (0 = default)")
	return cmd
}

func runWho(cmd *cobra.Command, g *Global, target string) error {
	ctx := cmd.Context()
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("who: symbol must not be empty")
	}

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	var opts impact.Options
	if n, _ := cmd.Flags().GetInt("depth"); n > 0 {
		opts.MaxDepth = n
	}
	if raw, _ := cmd.Flags().GetString("min-confidence"); raw != "" {
		c := model.Confidence(raw)
		if !c.Valid() {
			return fmt.Errorf("--min-confidence %q is not one of exact, high, medium, low", raw)
		}
		opts.MinConfidence = c
	}

	deps := capability.NewDeps(s.reg, graph.NewForRun(s.db, s.runID))
	blast, err := impact.AnalyzeWith(ctx, deps, s.runID, target, opts)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		if errors.As(err, &amb) {
			return whoAmbiguous(cmd.OutOrStdout(), amb)
		}
		if errors.Is(err, capability.ErrTargetNotFound) {
			return fmt.Errorf("no symbol named %q in the index; names must match exactly — "+
				"use 'cortex search' for partial matches", target)
		}
		return fmt.Errorf("who: blast radius of %q: %w", target, err)
	}

	hist := openHistory(g)
	lim := ownership.DefaultLimits()
	if n, _ := cmd.Flags().GetInt("commits"); n > 0 {
		lim.MaxCommits = n
	}
	if n, _ := cmd.Flags().GetInt("limit"); n > 0 {
		lim.MaxPeople = n
	}

	an, err := ownership.New(hist, ownership.WithLimits(lim))
	if err != nil {
		return fmt.Errorf("who: %w", err)
	}
	rep, err := an.Analyze(ctx, blast)
	if err != nil {
		return fmt.Errorf("who: %w", err)
	}

	if g.JSON {
		return emitJSON(cmd.OutOrStdout(), map[string]any{
			"index_revision": s.runID,
			"history_note":   hist.note,
			"review_context": rep,
		})
	}
	writeWho(cmd.OutOrStdout(), rep, hist.note)
	return nil
}

func whoAmbiguous(out io.Writer, amb *capability.AmbiguousTargetError) error {
	fmt.Fprintf(out, "%q matches %d definitions. Re-ask with a fully-qualified name:\n\n",
		amb.Query, len(amb.Candidates))
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for i, c := range amb.Candidates {
		if i == 10 {
			fmt.Fprintf(tw, "…and %d more\n", len(amb.Candidates)-10)
			break
		}
		fmt.Fprintf(tw, "  %s\t%s:%d\n", c.FQN, c.FilePath, c.Line)
	}
	_ = tw.Flush()
	return &ExitCodeError{Code: 1, Silent: true}
}

// ---------------------------------------------------------------- rendering

// writeWho prints the report.
//
// The order is deliberate: what the change reaches, then who has context on it,
// then the part of it nobody has touched, then what the whole answer does not
// say. The last section is not an appendix — it is the only thing standing
// between a ranked list of colleagues' names and a reader who takes it for a
// finding about them.
func writeWho(out io.Writer, rep ownership.Report, historyNote string) {
	t := rep.Target
	name := t.Name
	if name == "" {
		name = t.FQN
	}
	fmt.Fprintf(out, "%s  %s\n", name, location(t.FilePath, t.Line))
	if t.FQN != "" && t.FQN != name {
		fmt.Fprintf(out, "%s\n", t.FQN)
	}

	fmt.Fprintf(out, "\nA change here reaches %s across %s (%d direct, %d indirect, %d through dispatch).\n",
		countOf(rep.Reach.Symbols, "symbol", "symbols"),
		countOf(rep.Reach.Files, "file", "files"),
		rep.Reach.Direct, rep.Reach.Indirect, rep.Reach.Dispatch)

	if historyNote != "" {
		fmt.Fprintf(out, "\n%s\n", historyNote)
	}

	fmt.Fprintf(out, "\nWHO HAS CONTEXT ON THIS CODE\n")
	if len(rep.People) == 0 {
		fmt.Fprintln(out, "\n  Nobody in the history this report read has touched any of it.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "\n  SHARE\tREACHED\tFILES\tCOMMITS\tLAST TOUCH\tHAS TOUCHED IT")
		for _, p := range rep.People {
			fmt.Fprintf(tw, "  %.0f%%\t%d of %d\t%d\t%d\t%s\t%s\n",
				p.Share*100, p.Symbols, rep.Reach.Attributable,
				p.FilesTouched, p.CommitCount, day(p.LastTouchUnix), personLabel(p))
		}
		_ = tw.Flush()

		// The commits are the justification. A ranked name without the commits
		// behind it is an assertion, which is the thing this command must not
		// produce.
		for _, p := range rep.People {
			fmt.Fprintf(out, "\n  %s — %s\n", personLabel(p), p.Basis)
			for _, c := range p.Commits {
				fmt.Fprintf(out, "    %s  %s  %s\n", shortSHA(c.SHA), day(c.When), c.Subject)
				fmt.Fprintf(out, "      %s\n", strings.Join(c.Files, ", "))
			}
			if n := p.CommitCount - len(p.Commits); n > 0 {
				fmt.Fprintf(out, "    …and %d earlier commit(s) touching the same code\n", n)
			}
			if n := p.FilesTouched - len(p.Files); n > 0 {
				fmt.Fprintf(out, "    …and %d more reached file(s) they have touched\n", n)
			}
		}
	}

	if rep.Untouched.FileCount > 0 {
		fmt.Fprintf(out, "\nREACHED CODE NOBODY IN THIS WINDOW HAS TOUCHED (%d of %d files, %d symbols)\n\n",
			rep.Untouched.FileCount, rep.Reach.Files, rep.Untouched.Symbols)
		for _, f := range rep.Untouched.Files {
			fmt.Fprintf(out, "  %s\n", f)
		}
		if n := rep.Untouched.FileCount - len(rep.Untouched.Files); n > 0 {
			fmt.Fprintf(out, "  …and %d more\n", n)
		}
	}

	fmt.Fprintf(out, "\nWHAT THIS DOES NOT SAY\n\n")
	for _, c := range rep.Caveats {
		fmt.Fprintf(out, "  - %s\n", c)
	}
}

// personLabel renders one identity. Name as git recorded it, address alongside,
// because the address is the thing that was actually grouped on.
func personLabel(p ownership.Person) string {
	if p.Email == "" {
		return p.Name
	}
	return p.Name + " <" + p.Email + ">"
}

func location(path string, line int) string {
	if path == "" {
		return "(no file recorded)"
	}
	if line > 0 {
		return fmt.Sprintf("%s:%d", path, line)
	}
	return path
}

// day formats an author timestamp. UTC, and the date only: a review-routing
// report is not improved by minutes, and a local timezone would make the same
// index render differently on two machines.
func day(unix int64) string {
	if unix == 0 {
		return "—"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---------------------------------------------------------------- history

// gitHistory adapts internal/acquire/git to the ownership port.
//
// It exists to do one translation and one refusal.
//
// The translation is the path space. git reports paths relative to the WORKTREE
// root; the index stores them relative to the PROJECT root, and a service inside
// a monorepo makes those different directories. Without the rebase every path
// comparison fails, every person ranks at zero, and the report reads like a
// repository nobody has ever touched — a wrong answer that looks exactly like a
// correct one.
//
// The refusal is that a directory which is not a repository, or one go-git
// declined to open fully, yields no history rather than an error. Both are
// normal states (internal/acquire/git makes the same choice), the command is
// still worth running without them, and the report's own caveats say that no
// history was read.
type gitHistory struct {
	repo   *acquiregit.Repo
	prefix string // project root relative to the worktree root, "" when equal
	note   string // why history is unavailable or partial, "" when it is not
}

func openHistory(g *Global) *gitHistory {
	root := g.Config().RepoRoot
	repo, err := acquiregit.Open(root)
	if err != nil {
		return &gitHistory{note: "No history was read: " + err.Error() +
			". The blast radius below is still the index's."}
	}
	if !repo.IsRepo() {
		return &gitHistory{note: "No history was read: " + root +
			" is not a git repository. The blast radius below is still the index's."}
	}
	if d := repo.Degraded(); d != "" {
		return &gitHistory{note: "No history was read: this repository opened only in degraded mode (" +
			d + "), which leaves commit history unavailable."}
	}

	h := &gitHistory{repo: repo}
	prefix, err := pathPrefix(repo.Root(), root)
	if err != nil {
		h.repo = nil
		h.note = "No history was read: " + err.Error()
		return h
	}
	h.prefix = prefix
	if prefix != "" {
		h.note = fmt.Sprintf("History is read from the enclosing repository at %s and restricted to %s/, "+
			"which is what this index covers.", repo.Root(), prefix)
	}
	return h
}

// Commits implements ownership.History.
func (h *gitHistory) Commits(ctx stdctx.Context, limit int) ([]ownership.Commit, error) {
	if h.repo == nil {
		return nil, nil
	}
	cs, err := h.repo.RecentCommits(ctx, limit)
	if err != nil {
		if errors.Is(err, acquiregit.ErrNotRepository) {
			return nil, nil
		}
		return nil, fmt.Errorf("read history at %s: %w", h.repo.Root(), err)
	}

	out := make([]ownership.Commit, 0, len(cs))
	for _, c := range cs {
		paths := rebasePaths(c.Paths, h.prefix)
		out = append(out, ownership.Commit{
			SHA:         c.SHA,
			AuthorName:  c.AuthorName,
			AuthorEmail: c.AuthorEmail,
			When:        c.When,
			Subject:     c.Subject,
			Paths:       paths,
		})
	}
	return out, nil
}

// pathPrefix is the project root expressed relative to the worktree root.
//
// Both sides are symlink-resolved first. On macOS the temporary directories
// every test uses are symlinks (/tmp -> /private/tmp) and go-git returns the
// resolved form while the CLI carries the unresolved one, so skipping this makes
// the prefix look like "../../..' on exactly the paths the tests run over.
func pathPrefix(worktreeRoot, projectRoot string) (string, error) {
	wt := resolve(worktreeRoot)
	pr := resolve(projectRoot)

	rel, err := filepath.Rel(wt, pr)
	if err != nil {
		return "", fmt.Errorf("locate project root %s inside worktree %s: %w", projectRoot, worktreeRoot, err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("project root %s is outside the worktree %s, so no commit path can be "+
			"matched against this index", projectRoot, worktreeRoot)
	}
	return rel, nil
}

func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// rebasePaths drops commit paths outside the project root and strips the prefix
// from the rest, leaving them in the index's path space. Sorted, because the
// filter can only preserve an order it was given and this package promises one.
func rebasePaths(paths []string, prefix string) []string {
	if prefix == "" {
		out := append([]string(nil), paths...)
		sort.Strings(out)
		return out
	}
	pre := prefix + "/"
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, pre); ok && rest != "" {
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out
}
