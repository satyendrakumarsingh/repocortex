package cli

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/rename"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/edit"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/internal/verify"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Exit codes. 2 always means "nothing was written".
const (
	renameExitVerifyFail = 1
	renameExitRefused    = 2
	renameExitUnverified = 3
)

// renameIncomplete is printed with every rename: what RepoCortex cannot see, and
// what the user has to do about it.
const renameIncomplete = "RepoCortex renames the definition and the references it resolved, nothing else, " +
	"and it never compiles your code. A reference it could not bind is not listed and is not renamed; " +
	"neither are interface implementations or overrides that have no edge to this symbol. " +
	"After an apply, verify checks the change against the pre-rename index, and an apply that " +
	"leaves any occurrence of the old name exits 3, never 0. " +
	"Every line of an indexed file that still names the old identifier is listed under LEFTOVER " +
	"OCCURRENCES (JSON: leftovers): review each by hand, then build and test."

type renameOptions struct {
	Old, New, Path string
	Floor          model.Confidence
	Apply          bool
	MaxSites       int
	ContractOut    string
}

// renameReport is the --json shape and the source of the text rendering.
type renameReport struct {
	Old       string           `json:"old"`
	New       string           `json:"new"`
	Target    rename.Target    `json:"target"`
	Floor     model.Confidence `json:"floor"`
	FloorNote string           `json:"floor_note,omitempty"`
	Sites     []rename.Site    `json:"sites"`
	Refusals  []renameRefusal  `json:"refusals"`
	Applied   bool             `json:"applied"`
	Files     []string         `json:"files_changed,omitempty"`
	Contract  string           `json:"contract,omitempty"`
	Verify    *verify.Report   `json:"verify,omitempty"`
	Complete  bool             `json:"complete"`
	Warning   string           `json:"warning"`

	// LeftoverScan: lines that still name the old identifier. In a dry run,
	// the sites themselves are discounted; after an apply, nothing is.
	rename.LeftoverScan

	// notePrinted: the text renderer already printed FloorNote, ahead of the
	// write, and must not repeat it.
	notePrinted bool
}

type renameRefusal struct {
	Path       string `json:"path,omitempty"`
	Line       int    `json:"line,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Reason     string `json:"reason"`
}

func (r renameRefusal) String() string {
	loc := rename.TerminalSafe(r.Path)
	if r.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, r.Line)
	}
	switch {
	case loc == "":
		return r.Reason
	case r.Confidence != "":
		return fmt.Sprintf("%-7s %s  %s", r.Confidence, loc, r.Reason)
	default:
		return loc + "  " + r.Reason
	}
}

func newRenameCmd(g *Global) *cobra.Command {
	var (
		o     renameOptions
		floor string
	)
	cmd := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a symbol, editing only sites RepoCortex can defend",
		Long: `Renames a symbol at its definition and at every reference RepoCortex resolved.

A dry run by default: it prints every site, its confidence, and whether the
rename could be applied. Nothing is written without --apply.

--apply writes only if EVERY site passes:
  - confidence is at or above the floor: exact (a compiler-proven edge) by
    default; --floor high also accepts an explicit import plus an
    unambiguous receiver type, measured ` + rename.HighPrecision + ` precise
  - the file is byte-for-byte what was indexed (otherwise re-index)
  - the old name occurs on the site's line exactly once, or the recorded
    column lands on it exactly
  - the file is inside the project root
  - every reference was fetched, and the new name is not already defined in
    a file being edited
If any site fails, nothing is written, every failing site is listed, and the
exit code is 2. Each file is written atomically; if one write fails, files
already written are restored.

After applying, a task contract for the rename is written (default
.cortex/rename-contract.json) and 'cortex verify' runs on the changed
files — against the index as it was BEFORE the rename.

Exit codes: 0 would apply / applied and verify passed or advised; 1 error or
verify failed; 2 refused, nothing written; 3 applied, but verify was
unverified or the old name still appears (LEFTOVER OCCURRENCES).

The rename is not complete. RepoCortex never compiles your code: references it
could not bind, and interface implementations or overrides with no edge to
the symbol, are not listed and not renamed. Search for the old name, then
build and test.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := parseRenameFloor(floor)
			if err != nil {
				return err
			}
			o.Old, o.New, o.Floor = args[0], args[1], f
			return runRename(cmd, g, o)
		},
	}
	cmd.Flags().StringVar(&floor, "floor", string(rename.DefaultFloor),
		"weakest confidence an edit may rest on: exact, or high (measured "+rename.HighPrecision+" precise)")
	cmd.Flags().StringVar(&o.Path, "path", "", "substring of the defining file's path, when several definitions share the name")
	cmd.Flags().BoolVar(&o.Apply, "apply", false, "write the edits (default is a dry run)")
	cmd.Flags().StringVar(&o.ContractOut, "contract-out", "", "where to write the rename's task contract (default .cortex/rename-contract.json)")
	cmd.Flags().IntVar(&o.MaxSites, "max-sites", 0, "refuse renames with more references than this (0 = default 2000)")
	return cmd
}

// parseRenameFloor accepts exact and high only: nothing below high is ever
// edited unattended.
func parseRenameFloor(s string) (model.Confidence, error) {
	switch c := model.Confidence(strings.ToLower(strings.TrimSpace(s))); c {
	case model.ConfidenceExact, model.ConfidenceHigh:
		return c, nil
	default:
		return "", fmt.Errorf("rename: --floor must be exact or high, not %q", s)
	}
}

// floorNote is the warning an opt-in to the high floor carries, printed
// before anything is written.
func floorNote(floor model.Confidence) string {
	if floor != model.ConfidenceHigh {
		return ""
	}
	return fmt.Sprintf("--floor high: `high` bindings measure %s precision against compiler indexes "+
		"(docs/accuracy.md); at 90%%, a rename touching thirty sites gets three wrong. "+
		"Read every high site below, then build and test.", rename.HighPrecision)
}

func newRenameReport(o renameOptions) renameReport {
	return renameReport{
		Old: o.Old, New: o.New, Floor: o.Floor, FloorNote: floorNote(o.Floor),
		Sites: []rename.Site{}, Refusals: []renameRefusal{},
		Complete: false, Warning: renameIncomplete,
	}
}

func runRename(cmd *cobra.Command, g *Global, o renameOptions) error {
	ctx := cmd.Context()
	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	rep := newRenameReport(o)
	plan, err := rename.Collect(ctx, s.reg, s.runID, rename.Request{
		Old: o.Old, New: o.New, Path: o.Path, MaxSites: o.MaxSites,
	})
	var amb *rename.AmbiguousError
	switch {
	case errors.As(err, &amb):
		for _, c := range amb.Candidates {
			rep.Refusals = append(rep.Refusals, renameRefusal{Path: c.Path, Line: c.Line,
				Reason: fmt.Sprintf("one of %d definitions named %s; pass --path to choose", len(amb.Candidates), o.Old)})
		}
		return finishRename(cmd, g, rep, renameExitRefused)
	case err != nil:
		return err
	}
	rep.Target, rep.Sites = plan.Target, plan.Sites
	for _, r := range plan.Refusals(o.Floor) {
		rep.Refusals = append(rep.Refusals, refusalOf(r))
	}

	// The editor checks what the index cannot: each file still holds what was
	// indexed, and each site's line holds the identifier where it should. It
	// runs in a dry run too, so a dry run reports exactly what --apply would do.
	ed := edit.New(g.Config().RepoRoot)
	prepared, problems, err := ed.Prepare(ctx, fileEdits(plan))
	if err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	for _, p := range problems {
		rep.Refusals = append(rep.Refusals, problemRefusal(p))
	}

	if len(rep.Refusals) > 0 {
		return finishRename(cmd, g, rep, renameExitRefused)
	}
	if !o.Apply {
		scan, err := leftovers(ctx, s, g, o.Old, plan.Sites)
		if err != nil {
			return err
		}
		rep.LeftoverScan = scan
		return finishRename(cmd, g, rep, 0)
	}
	return applyRename(ctx, cmd, g, s, o, plan, ed, prepared, rep)
}

func fileEdits(p rename.Plan) []edit.FileEdit {
	files := p.ByFile()
	out := make([]edit.FileEdit, 0, len(files))
	for _, f := range files {
		out = append(out, edit.FileEdit{Path: f.Path, WantHash: f.Hash, Old: p.Old, New: p.New, Lines: f.Lines})
	}
	return out
}

func refusalOf(r rename.Refusal) renameRefusal {
	out := renameRefusal{Reason: r.Reason}
	if r.Site != nil {
		out.Path, out.Line, out.Confidence = r.Site.Path, r.Site.Line, string(r.Site.Confidence)
	}
	return out
}

// problemRefusal turns an editor problem into a refusal, adding what the user
// can do about the ones they can act on.
func problemRefusal(p edit.Problem) renameRefusal {
	reason := strings.TrimPrefix(p.Reason, "edit: ")
	switch {
	case errors.Is(p.Err, edit.ErrAmbiguous):
		reason += "; rename this line by hand"
	case errors.Is(p.Err, edit.ErrNotFound):
		reason += "; the file no longer matches the index, run 'cortex index' and try again"
	}
	return renameRefusal{Path: p.Path, Line: p.Line, Reason: reason}
}

func finishRename(cmd *cobra.Command, g *Global, rep renameReport, code int) error {
	if g.JSON {
		if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
			return err
		}
	} else if err := renderRename(cmd.OutOrStdout(), rep); err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	// Silent: the report is the explanation; the code is the verdict.
	return &ExitCodeError{Code: code, Silent: true, Err: fmt.Errorf("rename: exit %d", code)}
}

// belowExactOnlyHigh reports whether some refusal is a high site refused by
// the exact floor — the case --floor high would change.
func belowExactOnlyHigh(rep renameReport) bool {
	if rep.Floor != model.ConfidenceExact {
		return false
	}
	for _, r := range rep.Refusals {
		if r.Confidence == string(model.ConfidenceHigh) {
			return true
		}
	}
	return false
}

func renderRename(w io.Writer, rep renameReport) error {
	fmt.Fprintf(w, "Rename %s -> %s", rep.Old, rep.New)
	if rep.Target.Path != "" {
		name := rep.Target.FQN
		if name == "" {
			name = rep.Target.Name
		}
		fmt.Fprintf(w, "  (%s, %s:%d)", rename.TerminalSafe(name), rename.TerminalSafe(rep.Target.Path), rep.Target.Line)
	}
	fmt.Fprintln(w)
	if rep.FloorNote != "" && !rep.notePrinted {
		fmt.Fprintf(w, "WARNING %s\n", rep.FloorNote)
	}
	switch {
	case len(rep.Refusals) > 0:
		fmt.Fprintf(w, "REFUSED: nothing was written. Every site must be %s or better, "+
			"unambiguous on its line, and unchanged since indexing.\n", rep.Floor)
		if belowExactOnlyHigh(rep) {
			fmt.Fprintf(w, "  High sites need a compiler-backed index to become exact "+
				"('cortex indexers install', then 'cortex scip import'), or an explicit --floor high "+
				"(measured %s precise).\n", rename.HighPrecision)
		}
	case rep.Applied && !rep.LeftoverScan.Clear():
		fmt.Fprintf(w, "APPLIED to %d file(s), but NOT VERIFIED COMPLETE: the old name is still in the "+
			"files listed under LEFTOVER OCCURRENCES.\n", len(rep.Files))
	case rep.Applied:
		fmt.Fprintf(w, "APPLIED to %d file(s).\n", len(rep.Files))
	default:
		fmt.Fprintln(w, "DRY RUN: nothing was written. Re-run with --apply to make these edits.")
		if !rep.LeftoverScan.Clear() {
			fmt.Fprintln(w, "  The old name also appears outside the sites (LEFTOVER OCCURRENCES); "+
				"--apply would exit 3 unless they are resolved first.")
		}
	}

	if len(rep.Sites) > 0 {
		fmt.Fprintln(w, "\nSITES")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, s := range rep.Sites {
			c := string(s.Confidence)
			if c == "" {
				c = "none"
			}
			fmt.Fprintf(tw, "  %s\t%s:%d\t%s\n", c, rename.TerminalSafe(s.Path), s.Line, s.Kind)
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("render rename: %w", err)
		}
	}
	if len(rep.Refusals) > 0 {
		fmt.Fprintln(w, "\nREFUSED SITES")
		for _, r := range rep.Refusals {
			fmt.Fprintf(w, "  %s\n", r)
		}
	}
	if len(rep.Refusals) == 0 {
		renderLeftovers(w, rep)
	}
	if rep.Verify != nil {
		fmt.Fprintf(w, "\nVERIFY: %s  (contract: %s)\n", strings.ToUpper(string(rep.Verify.Disposition)), rep.Contract)
		for _, f := range rep.Verify.Findings {
			fmt.Fprintf(w, "  [%s] %s\n", strings.ToUpper(string(f.Standing)), f.Summary)
		}
		for _, u := range rep.Verify.Unknowns {
			fmt.Fprintf(w, "  [UNKNOWN] %s\n", u)
		}
		fmt.Fprintln(w, "  Checked against the pre-rename index; run 'cortex index' to refresh it.")
	}
	fmt.Fprintf(w, "\nNOT COMPLETE: %s\n", rep.Warning)
	return nil
}

// applyRename writes the prepared files, then states and checks what was done.
func applyRename(ctx stdctx.Context, cmd *cobra.Command, g *Global, s *session, o renameOptions,
	plan rename.Plan, ed *edit.Editor, prepared []edit.Prepared, rep renameReport) error {
	// An opt-in to the high floor is stated before anything is written, not
	// only in the report that follows.
	if rep.FloorNote != "" && !g.JSON {
		fmt.Fprintf(cmd.OutOrStdout(), "WARNING %s\n", rep.FloorNote)
		rep.notePrinted = true
	}

	applied, code, msg := commitOutcome(ed.Commit(ctx, prepared))
	switch {
	case !applied && code == renameExitRefused:
		rep.Refusals = append(rep.Refusals, renameRefusal{Reason: msg})
		return finishRename(cmd, g, rep, renameExitRefused)
	case !applied:
		return fmt.Errorf("rename: %s", msg)
	case msg != "":
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", msg)
	}
	rep.Applied = true
	for _, p := range prepared {
		rep.Files = append(rep.Files, p.Path)
	}

	contract, err := renameContract(ctx, s, g, plan, rep.Files)
	if err != nil {
		return fmt.Errorf("rename: applied to %d file(s), but the contract could not be compiled: %w", len(rep.Files), err)
	}
	out := o.ContractOut
	if out == "" {
		out = filepath.Join(g.Config().RepoRoot, IndexDir, "rename-contract.json")
	}
	if err := writeContract(out, contract); err != nil {
		return fmt.Errorf("rename: applied to %d file(s), but the contract could not be written: %w", len(rep.Files), err)
	}
	rep.Contract = out

	vr, err := verify.New(
		verify.WithArchitecture(archSource{reg: s.reg, model: g.Config().Architecture}, s.runID),
	).Verify(ctx, contract, rep.Files)
	if err != nil {
		return fmt.Errorf("rename: applied to %d file(s), but verify could not run: %w", len(rep.Files), err)
	}
	rep.Verify = &vr

	// The edited sites no longer hold the old name, so nothing is discounted:
	// any occurrence left is one this rename did not change.
	scan, err := leftovers(ctx, s, g, o.Old, nil)
	if err != nil {
		return fmt.Errorf("rename: applied to %d file(s), but the scan for leftover occurrences failed: %w",
			len(rep.Files), err)
	}
	rep.LeftoverScan = scan

	code = 0
	switch {
	case vr.Disposition == verify.DispositionFail:
		code = renameExitVerifyFail
	case vr.Disposition == verify.DispositionUnverified, !scan.Clear():
		code = renameExitUnverified
	}
	return finishRename(cmd, g, rep, code)
}

// leftovers scans the indexed files for lines still naming old. Files are
// read now through security.ReadFileContained: contained, regular files only,
// bounded like an edit.
func leftovers(ctx stdctx.Context, s *session, g *Global, old string, exclude []rename.Site) (rename.LeftoverScan, error) {
	root := g.Config().RepoRoot
	read := func(rel string) ([]byte, error) {
		b, _, err := security.ReadFileContained(root, rel, security.WithMaxBytes(edit.DefaultMaxFileBytes))
		return b, err
	}
	scan, err := rename.FindLeftovers(ctx, s.reg, s.runID, read, old, exclude, rename.LeftoverLimits{})
	if err != nil {
		return rename.LeftoverScan{}, fmt.Errorf("rename: %w", err)
	}
	return scan, nil
}

// commitOutcome classifies what edit.Commit returned. applied reports whether
// the new text is in place; when it is not, code is renameExitRefused if every
// file still holds what it held before (nothing was written, or everything
// written was restored) and 1 if a rollback could not restore a file. msg
// explains either, or, when applied, carries a warning about a lock left
// behind.
func commitOutcome(err error) (applied bool, code int, msg string) {
	if err == nil {
		return true, 0, ""
	}
	// Commit joins a failure to release its lock onto whatever else it
	// returns. Alone, it means the edit itself succeeded.
	rest, lockErr := []error{err}, error(nil)
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		rest = rest[:0]
		for _, e := range j.Unwrap() {
			if errors.Is(e, edit.ErrLockRelease) {
				lockErr = e
				continue
			}
			rest = append(rest, e)
		}
	}
	if len(rest) == 0 {
		return true, 0, fmt.Sprintf("the rename was written, but %v", lockErr)
	}
	cause := errors.Join(rest...)
	switch {
	case errors.Is(cause, edit.ErrRollbackIncomplete):
		msg = fmt.Sprintf("a write failed and the rollback was incomplete; the files named here may hold "+
			"the new name — check them with your version control before anything else: %v", cause)
		code = 1
	case errors.Is(cause, edit.ErrLocked):
		msg = fmt.Sprintf("nothing was written: %v", cause)
		code = renameExitRefused
	case errors.Is(cause, edit.ErrStale):
		msg = fmt.Sprintf("a file changed while the rename was being written; every file already written "+
			"was restored. Run 'cortex index' and try again (%v)", cause)
		code = renameExitRefused
	default:
		msg = fmt.Sprintf("a write failed; every file already written was restored (%v)", cause)
		code = renameExitRefused
	}
	if lockErr != nil {
		msg += fmt.Sprintf("; also, %v", lockErr)
	}
	return false, code, msg
}

// renameContract states what the rename was allowed to touch: the symbol and
// every edited file. Its blast radius is restricted to direct references
// (directReach): a rename changes the name at each reference and nothing
// further, so a caller two hops away is unaffected — and left in, it would make
// verify report a missed caller on every rename.
func renameContract(ctx stdctx.Context, s *session, g *Global, plan rename.Plan, files []string) (compile.Contract, error) {
	symbol := plan.Target.FQN
	if symbol == "" {
		symbol = plan.Target.Name
	}
	items := []devctx.Item{{
		Kind: plan.Target.Kind, Name: symbol, FQN: plan.Target.FQN, Path: plan.Target.Path,
		Line: plan.Target.Line, Relevance: 1, Why: "the symbol being renamed",
	}}
	for _, f := range files {
		items = append(items, devctx.Item{Kind: string(model.KindFile), Name: f, Path: f,
			Relevance: 0.5, Why: "holds a site the rename changed"})
	}
	intent := fmt.Sprintf("Rename %s to %s", plan.Old, plan.New)
	ports := compilePorts(s, g)
	ports.Impact = directReach{inner: ports.Impact}
	return compile.CompileWith(ctx, intent,
		devctx.Pack{Task: intent, Interpretation: intent, Items: items},
		compile.Options{MaxScope: len(items)}, ports)
}

// directReach keeps only depth-1 callers from an impact view. See
// renameContract.
type directReach struct{ inner compile.ImpactView }

func (d directReach) Reach(ctx stdctx.Context, target string) (compile.Reach, error) {
	r, err := d.inner.Reach(ctx, target)
	if err != nil {
		return r, err
	}
	kept := make([]compile.Reached, 0, len(r.Callers))
	for _, c := range r.Callers {
		if c.Depth == 1 {
			kept = append(kept, c)
		}
	}
	r.Callers = kept
	return r, nil
}

func writeContract(path string, c compile.Contract) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode contract: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func renderLeftovers(w io.Writer, rep renameReport) {
	scan := rep.LeftoverScan
	if scan.Clear() {
		return
	}
	total := fmt.Sprintf("%d", scan.Total)
	if scan.LowerBound {
		total = "at least " + total
	}
	if scan.Unsearchable {
		total = "unknown"
	}
	fmt.Fprintf(w, "\nLEFTOVER OCCURRENCES (%s line(s) still name %s; showing %d)\n", total, rep.Old, len(scan.Items))
	fmt.Fprintln(w, "  References RepoCortex could not bind, or comments and strings. Nothing here was changed; "+
		"review each by hand.")
	for _, l := range scan.Items {
		fmt.Fprintf(w, "  %s:%d  %s\n", rename.TerminalSafe(l.Path), l.Line, l.Text)
	}
	if scan.Unreadable > 0 {
		fmt.Fprintf(w, "  %d indexed file(s) that may mention %s were not checked: unreadable now, over the "+
			"%d-byte limit, or a credential file RepoCortex never reads. Check them by hand.\n",
			scan.Unreadable, rep.Old, edit.DefaultMaxFileBytes)
	}
	if scan.Unsearchable {
		fmt.Fprintf(w, "  %s has no letter or digit, so no scan can tell its uses from punctuation: "+
			"every file must be checked by hand.\n", rep.Old)
	}
}
