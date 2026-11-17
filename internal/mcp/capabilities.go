package mcp

import (
	stdctx "context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/dryrun"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/planchange"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// targetArgs names the symbol a capability tool is asked about, with the
// selectors that choose between definitions sharing its name.
type targetArgs struct {
	Symbol string `json:"symbol" jsonschema:"The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'."`
	Path   string `json:"path,omitempty" jsonschema:"File or directory of the definition, to choose between symbols that share the name."`
	lineArg

	minConfidenceArg
	hubDegreeLimitArg
	pageArg
}

// testsArgs is targetArgs with the depth find_tests walks to.
type testsArgs struct {
	targetArgs
	MaxDepth int `json:"max_depth,omitempty" jsonschema:"Most calls between a test and the symbol, 1 to 8. 1 lists the tests that call it directly; 2 adds tests that call a function that calls it. Default 4."`
}

// impactArgs takes its own min_confidence wording, and that is the ONE place
// this parameter is described twice on purpose.
//
// analyze_impact has always walked at impact.DefaultMinConfidence (medium),
// not low, and it must keep doing so when the parameter is absent: dropping
// the floor to low would widen every existing caller's blast radius and weaken
// the risk score computed from it. Embedding minConfidenceArg here would
// advertise "Default low" on a tool whose default is medium — and for a while
// this struct did exactly that with a copied description, so the schema and
// docs/reference/mcp-tools.md said low while the code walked at medium. The
// wording differs exactly where the behaviour differs, and
// TestImpactFloorIsDocumentedAsItBehaves holds the two together; the decoding
// is still parseMinConfidence's, via parseImpactFloor.
type impactArgs struct {
	Symbol string `json:"symbol" jsonschema:"The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'."`
	Path   string `json:"path,omitempty" jsonschema:"File or directory of the definition, to choose between symbols that share the name."`
	lineArg

	MinConfidence string `json:"min_confidence,omitempty" jsonschema:"Lowest confidence to follow: exact, high, medium or low. Default medium, so low relationships (a hint that the name exists, not a defended binding) are left out and counted; pass 'low' to walk them too, or 'high' when the result will gate an automated change."`

	hubDegreeLimitArg
	pageArg
}

// targetQuery is the selection request for a tool that takes `symbol`.
func targetQuery(tool, symbol, path string, line int) symbolQuery {
	return symbolQuery{Tool: tool, Param: "symbol", PathParam: "path", LineParam: "line",
		Name: symbol, Path: path, Line: line}
}

// paginate hands a response the rows from offset on and records the page, so
// Bound trims to the limit and says in offsets what it left out. For a tool
// that holds its whole result in memory; the edge tools page in the store.
func paginate(resp *Response, page *Page, items []Item) {
	page.Total = len(items)
	resp.Page = page
	resp.Items = items[min(page.Offset, len(items)):]
}

// registerCapabilities adds the graph-backed analysis tools.
//
// Like get_task_context these need the graph, so they are registered
// separately: an index whose references were never resolved should still serve
// symbol lookups rather than failing to start.
func (s *Server) registerCapabilities(allowed map[string]bool) {
	if s.deps.Graph == nil {
		return
	}

	descs := toolDescriptions()

	addTool(s, allowed, &sdk.Tool{
		Name:        "analyze_impact",
		Description: descs["analyze_impact"],
	}, s.impactAnalysis)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_tests",
		Description: descs["find_tests"],
	}, s.findTests)

	addTool(s, allowed, &sdk.Tool{
		Name:        "plan_change",
		Description: descs["plan_change"],
	}, s.planChange)

	addTool(s, allowed, &sdk.Tool{
		Name:        "list_blindspots",
		Description: descs["list_blindspots"],
	}, s.detectBlindspots)

	addTool(s, allowed, &sdk.Tool{
		Name:        "analyze_patch",
		Description: descs["analyze_patch"],
	}, s.analyzePatch)
}

type patchAnalysisArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"File path of the file being modified, e.g. 'internal/service.go'. With content; or pass diff instead."`
	Content string `json:"content,omitempty" jsonschema:"The proposed full file content to dry-run speculatively in memory. With path; or pass diff instead."`
	Diff    string `json:"diff,omitempty" jsonschema:"A unified diff (git diff, diff -u) of one or more files, applied in memory to the working tree. Use instead of path and content."`
}

// maxPatchFiles bounds the files one dry run analyses.
const maxPatchFiles = 50

func (s *Server) analyzePatch(ctx stdctx.Context, req *sdk.CallToolRequest, args patchAnalysisArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}
	root := s.deps.Config.RepoRoot

	// Every path a patch names is a path this server will read, parse and
	// type-check, and the caller chose it: a diff header is text. Only files
	// the index holds are read, so `--- a/../../etc/passwd` reads nothing; a
	// new file must still lie inside the repository. Over `serve --http`
	// the caller is anyone holding a token.
	indexed, err := s.indexedPaths(ctx, runID)
	if err != nil {
		return failFrom(err, "dryrun: %v", err)
	}

	var patches []dryrun.FilePatch
	switch {
	case strings.TrimSpace(args.Diff) != "":
		// Applied to the working tree, which is what an agent's diff is made
		// against; the index's copy is the fallback for a file not on disk.
		read := func(p string) (string, error) {
			if !indexed[p] {
				return "", fmt.Errorf("%s is not a file in the index", p)
			}
			if full, ok := insideRoot(root, p); ok {
				if b, err := os.ReadFile(full); err == nil { //nolint:gosec // an indexed file, resolved inside the repository root
					return string(b), nil
				}
			}
			return s.deps.Registry.FileExcerpt(ctx, runID, p, 1, 1<<20)
		}
		patches, err = dryrun.ApplyUnifiedDiff(args.Diff, read)
		if err != nil {
			return failWith("invalid_argument", "Pass a unified diff that applies to the working tree, or path and content.",
				"the diff does not apply: %v", err)
		}
	case strings.TrimSpace(args.Path) != "" && strings.TrimSpace(args.Content) != "":
		patches = []dryrun.FilePatch{{Path: strings.TrimPrefix(args.Path, "./"), Content: args.Content}}
	default:
		return failWith("invalid_argument", "Pass diff, or path and content, then call analyze_patch again.",
			"nothing to dry-run: pass diff, or path and non-empty content")
	}
	for _, p := range patches {
		if p.Added {
			if _, ok := insideRoot(root, p.Path); !ok || !cleanRelative(p.Path) {
				return failWith("invalid_argument", "Name files by their path inside the repository.",
					"%s is outside the repository", p.Path)
			}
			continue
		}
		if !indexed[p.Path] {
			return failWith("invalid_argument", "Name files the index holds, by their repository path; run 'repocortex index' for a new one.",
				"%s is not a file in the index", p.Path)
		}
	}
	if len(patches) > maxPatchFiles {
		return failWith("invalid_argument", fmt.Sprintf("Split the patch: at most %d files are dry-run at once.", maxPatchFiles),
			"%d files in the patch", len(patches))
	}

	resp := &Response{IndexRevision: runID, IndexStatus: status}
	var summaries []string
	changed, broken := 0, 0
	overlay := map[string]string{}
	var goDirs []string
	for _, p := range patches {
		if strings.HasSuffix(p.Path, ".go") && !p.Added && !p.Deleted {
			overlay[p.Path] = p.Content
			goDirs = append(goDirs, path.Dir(p.Path))
		}
		if p.Added {
			summaries = append(summaries, p.Path+": a new file, nothing indexed to compare with")
			continue
		}
		rep, err := dryrun.AnalyzePatch(ctx, s.deps.Registry, s.deps.Graph, runID, p.Path, p.Content)
		if err != nil {
			summaries = append(summaries, fmt.Sprintf("%s: not analysed (%v)", p.Path, err))
			continue
		}
		changed += rep.ChangedCount
		broken += rep.BrokenCount
		if len(patches) > 1 {
			summaries = append(summaries, rep.Summary)
		} else {
			resp.Summary = rep.Summary
		}
		for _, c := range rep.BrokenCallers {
			resp.Items = append(resp.Items, Item{
				Kind: string(model.KindFunction),
				Name: c.CallerName,
				Path: c.CallerPath,
				Line: c.CallerLine,
				Why:  fmt.Sprintf("broken caller: target %s: %s", c.TargetName, c.Reason),
			})
		}
	}
	if len(patches) > 1 {
		resp.Summary = fmt.Sprintf("Patch of %d files: %d definition(s) changed, %d caller(s) broken by name and arity.",
			len(patches), changed, broken)
		resp.Notes = append(resp.Notes, summaries...)
	} else if len(summaries) > 0 {
		resp.Summary = strings.Join(summaries, "; ")
	}

	// Go: what actually stops compiling. The checks above compare
	// definitions by name and arity, for every language; the type checker
	// knows the rest (a changed type, a removed method, an interface no
	// longer satisfied).
	if len(overlay) > 0 && root != "" {
		if errs, note := s.goPatchBreakage(ctx, runID, root, overlay, goDirs); note != "" {
			resp.Notes = append(resp.Notes, note)
		} else {
			for _, e := range errs {
				resp.Items = append(resp.Items, Item{Kind: "compile_error", Path: e.Path, Line: e.Line,
					Name: e.Path, Why: "stops compiling with the patch: " + e.Msg, Confidence: string(model.ConfidenceExact)})
			}
			resp.Notes = append(resp.Notes, fmt.Sprintf("Go: the patched packages and the packages importing them were "+
				"type-checked with and without the patch; %d error(s) are new with it.", len(errs)))
		}
	}
	return s.reply(ctx, req, resp)
}

// indexedPaths is the set of the run's file paths.
func (s *Server) indexedPaths(ctx stdctx.Context, runID int64) (map[string]bool, error) {
	files, err := s.deps.Registry.Files(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(files))
	for _, f := range files {
		out[f.Path] = true
	}
	return out, nil
}

// cleanRelative reports whether p is a relative, slash-separated path that
// stays where it is named: no "..", not absolute, already clean.
func cleanRelative(p string) bool {
	if p == "" || path.IsAbs(p) || filepath.IsAbs(p) || strings.Contains(p, "\\") {
		return false
	}
	c := path.Clean(p)
	return c == p && c != ".." && !strings.HasPrefix(c, "../")
}

// insideRoot resolves a repository path against root, following symlinks,
// and reports whether the result is still under root.
func insideRoot(root, p string) (string, bool) {
	if root == "" || !cleanRelative(p) {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	full := filepath.Join(realRoot, filepath.FromSlash(p))
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		full = resolved
	} else if resolvedDir, derr := filepath.EvalSymlinks(filepath.Dir(full)); derr == nil {
		full = filepath.Join(resolvedDir, filepath.Base(full)) // a new file: its directory must resolve inside
	}
	if full != realRoot && !strings.HasPrefix(full, realRoot+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// goPatchBreakage type-checks a Go patch over the patched packages and the
// files that import them. It returns the new errors, or a note saying why it
// could not run.
func (s *Server) goPatchBreakage(ctx stdctx.Context, runID int64, root string, overlay map[string]string, dirs []string) ([]dryrun.GoBreakage, string) {
	files, err := s.deps.Registry.Files(ctx, runID)
	if err != nil {
		return nil, "Go: not type-checked (" + err.Error() + ")."
	}
	inDir := map[string]bool{}
	var imports []string
	for _, d := range dirs {
		if !inDir[d] {
			inDir[d] = true
			if ip := dryrun.GoImportPath(root, d); ip != "" {
				imports = append(imports, ip)
			}
		}
	}
	importers, err := s.deps.Registry.FilesImporting(ctx, runID, imports)
	if err != nil {
		return nil, "Go: not type-checked (" + err.Error() + ")."
	}
	for _, f := range importers {
		inDir[path.Dir(f)] = true
	}
	var scope []string
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".go") && inDir[path.Dir(f.Path)] {
			scope = append(scope, f.Path)
		}
	}
	errs, err := dryrun.TypeCheckPatch(ctx, root, scope, overlay)
	if err != nil {
		return nil, "Go: not type-checked (" + err.Error() + ")."
	}
	return errs, ""
}

func (s *Server) impactAnalysis(ctx stdctx.Context, req *sdk.CallToolRequest, args impactArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call analyze_impact again.",
			"symbol must not be empty")
	}
	// Zero value when absent, so AnalyzeTarget applies its own default. See
	// parseImpactFloor.
	minConf, err := parseImpactFloor(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	page, err := s.page(args.pageArg, "impacted symbol(s)")
	if err != nil {
		return failWith("invalid_argument", "Pass limit and offset as non-negative numbers, or omit them.", "%v", err)
	}
	q := targetQuery("analyze_impact", args.Symbol, args.Path, args.Line)
	sel, sc, scResp := s.selectSymbol(ctx, req, q)
	if sc != nil {
		return sc, scResp, nil
	}
	runID, status := sel.RunID, sel.Status

	// No floorNote here: the report already names what its floor kept out, as
	// a "NOT VISIBLE (below_confidence_floor)" unknown rendered below. Saying
	// it twice would spend the item budget on a repeat.
	rep, err := impact.AnalyzeTarget(ctx, s.capDeps, runID, capability.TargetFromCandidate(args.Symbol, sel.Target),
		impact.Options{MinConfidence: minConf, HubDegreeLimit: args.HubDegreeLimit, CheckValueUse: true})
	if err != nil {
		// A traversal that errored produced no report, and an agent must not
		// read that as "nothing depends on this symbol".
		return failFrom(err, "impact analysis: %v", err)
	}

	items, reach := impactItems(rep)

	// The gate goes ahead of the risk breakdown, the factors and the unknowns.
	// A risk score computed over a graph RepoCortex could barely resolve is a
	// precise-looking number built on absent evidence, and its precision is
	// exactly what makes it dangerous to read without this sentence first.
	//
	// It is judged from the report's own Binding rather than a second query:
	// the evidence shown must be the evidence the analysis used.
	cov := s.coverageFor(ctx, runID, rep.Target.Language, rep.Target.FilePath, rep.Binding, aboutCallers)
	// The blast radius is built on the same bindings find_callers lists, so
	// the same evidence against them applies to it.
	cov.Misbound = s.boundSites(ctx, runID, sel.Target).conflict(sel.Target)
	cov.Elsewhere = rep.ValueUse
	cov.FloorEmpty = floorEmptyLead(rep)

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%s (%s:%d): %s%s. %s",
			rep.Target.Name, rep.Target.FilePath, rep.Target.Line, reach.summary(), floorClause(rep),
			riskSentence(rep.Risk, cov.saysWhyNothingIsKnown())),
	}
	paginate(resp, page, items)
	s.applyCoverage(resp, cov, "impact")

	// What the dispatch rows mean, and what was folded into a stronger claim.
	// Both are stated once per response rather than once per row: the per-row
	// claim is on the item's own Why, and repeating the section's meaning 40
	// times would spend the budget the items need.
	resp.Notes = append(resp.Notes, reach.notes(rep)...)
	if sites := directSites(rep); sites > len(rep.Direct) {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d direct counts the SYMBOLS that call the target; they make %d call site(s) between them. "+
				"find_callers lists every recorded call site at every confidence, including calls the target makes "+
				"to itself, so its list can be longer than this count.", len(rep.Direct), sites))
	}

	// Show the risk score's inputs. A bare "Risk: HIGH" is unactionable — the
	// factors are what tell a developer whether to slow down and why.
	resp.Notes = append(resp.Notes, s.impactCoverageNotes(ctx, runID, rep)...)
	if rep.Risk.Formula != "" {
		resp.Notes = append(resp.Notes, "Risk = "+rep.Risk.Formula)
	}
	for _, f := range rep.Risk.Factors {
		resp.Notes = append(resp.Notes, fmt.Sprintf("  %s (+%d): %s", f.Name, f.Points, f.Detail))
	}
	if rep.Risk.CertaintyReason != "" {
		resp.Notes = append(resp.Notes,
			fmt.Sprintf("Certainty %s — %s. Certainty is a SEPARATE axis from risk: "+
				"how much could break, and how much to trust that, are different questions.",
				rep.Risk.Certainty, strings.TrimSuffix(rep.Risk.CertaintyReason, ".")))
	}

	// The unknowns are the most important part of an impact report. A caller
	// who cannot see what the analysis missed will treat a short list as a
	// complete one.
	for _, u := range rep.Unknown {
		resp.Notes = append(resp.Notes, fmt.Sprintf("NOT VISIBLE (%s): %s", u.Kind, u.Detail))
	}
	if rep.Truncated {
		resp.markTruncated(rep.TruncatedReason)
	}
	if n := sel.note(q); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	resp.Notes = append(resp.Notes,
		"This is a LOWER BOUND. Static analysis cannot see reflection, dynamic dispatch "+
			"or runtime wiring, and unresolved references are listed above. Do not treat an "+
			"empty result as proof that nothing breaks.")
	return s.reply(ctx, req, resp)
}

// riskSentence renders a risk for a summary line.
//
// THE UNKNOWN CASE PRINTS NO SCORE. The score is the sum of what the index
// bound, and when the callers could not be determined that sum is a number
// about missing evidence: "Risk low (score 20)" was printed for a method with
// 792 references spelled like it, none of them bound. The level is the word a
// reader acts on, so it is the word that changes, and the reason follows it.
//
// The portal reads the word after "Risk" out of this sentence; keep that word
// a single token.
//
// reasonAlreadyGiven is true when the line this sentence sits on has already
// said why nothing is known, in the lead; the reason is then not said twice.
func riskSentence(r impact.Risk, reasonAlreadyGiven bool) string {
	switch {
	case r.Level == impact.LevelUnknown && reasonAlreadyGiven:
		// An unknown level is still cut short when the walk stopped at a bound:
		// "Risk low or higher (... cut at depth 3)" says so on the first line
		// for every other level, and this one used to drop it.
		if r.Partial != "" {
			return fmt.Sprintf("Risk unknown (%s).", r.Partial)
		}
		return "Risk unknown."
	case r.Level == impact.LevelUnknown:
		if r.Partial != "" {
			return fmt.Sprintf("Risk unknown: %s (%s).", strings.TrimSuffix(r.Undetermined, "."), r.Partial)
		}
		return fmt.Sprintf("Risk unknown: %s.", strings.TrimSuffix(r.Undetermined, "."))
	case r.Undetermined != "":
		why := "score " + fmt.Sprint(r.Score) + " counts only the callers that were bound"
		if r.Partial != "" {
			why += "; " + r.Partial
		}
		return fmt.Sprintf("Risk %s or higher (%s).", r.Level, why)
	case r.Partial != "":
		// A floor for a reason of its own: the walk stopped at a bound, or the
		// name occurs where no call is recorded. A bare "low" there is the one
		// word an agent acts on, and it is a lower bound.
		return fmt.Sprintf("Risk %s or higher (score %d; %s).", r.Level, r.Score, r.Partial)
	default:
		return fmt.Sprintf("Risk %s (score %d).", r.Level, r.Score)
	}
}

// floorClause is the part of the first line that says the confidence floor kept
// callers out, with the count and the argument that brings them back. Empty when
// the floor kept nothing out.
//
// The hidden-by-floor count used to sit in a NOT VISIBLE note below the sixth
// line, so a first line reading "6 direct" was read as the number of callers
// while find_callers, which has no floor by default, listed 9 of them.
func floorClause(rep impact.Report) string {
	if rep.LowConfidenceHidden == 0 {
		return ""
	}
	tier := floorTier(rep.ConfidenceFloor)
	count := fmt.Sprintf("%d more symbol(s)", rep.LowConfidenceHidden)
	if floorCountBounded(rep) {
		count = fmt.Sprintf("at least %d more symbol(s)", rep.LowConfidenceHidden)
	}
	direct := ""
	if d := rep.LowConfidenceHiddenDirect; d > 0 && d != rep.LowConfidenceHidden {
		direct = fmt.Sprintf(" (%d of them direct)", d)
	}
	return fmt.Sprintf("; %s %s are excluded%s; pass min_confidence=low", count, tier, direct)
}

// directSites is how many call sites the direct callers make between them. Zero
// when the report carries no evidence for them (too many to load).
func directSites(rep impact.Report) int {
	n := 0
	for _, d := range rep.Direct {
		n += len(d.Evidence)
	}
	return n
}

// floorTier names the confidences a floor excludes.
func floorTier(floor model.Confidence) string {
	switch floor {
	case model.ConfidenceMedium:
		return "at low confidence"
	case model.ConfidenceHigh:
		return "at medium or low confidence"
	}
	return "below " + string(floor) + " confidence"
}

func floorCountBounded(rep impact.Report) bool {
	for _, u := range rep.Unknown {
		if u.Kind == impact.UnknownBelowConfidenceFloor {
			return u.CountIsLowerBound
		}
	}
	return false
}

// floorEmptyLead is the lead sentence for a report that shows no direct caller
// only because the floor kept some out. Empty otherwise.
func floorEmptyLead(rep impact.Report) string {
	if len(rep.Direct) > 0 || rep.LowConfidenceHiddenDirect == 0 {
		return ""
	}
	return fmt.Sprintf("0 direct callers are shown only because %d %s are excluded (pass min_confidence=low).",
		rep.LowConfidenceHiddenDirect, floorTier(rep.ConfidenceFloor))
}

// Item kinds an impact report can carry.
//
// Three kinds because the report makes three different claims. A direct caller
// is a recorded reference to the target; an indirect one is reachable through
// other symbols; a dispatch row is a symbol the runtime MAY execute when
// something names the target, with no call site proven to take that path.
// Flattening them would let a possibility be read as a fact.
const (
	kindDirect   = "direct"
	kindIndirect = "indirect"
	kindDispatch = "dispatch"
)

// reachCounts records what impactItems actually rendered, as opposed to what
// the report contained.
//
// The summary must count the rows an agent can see. A summary that counts the
// report's slices instead would over-report every time a symbol was suppressed
// as a duplicate, which is the same double-count the dedup exists to prevent.
type reachCounts struct {
	direct   int
	indirect int
	dispatch int

	// merged counts dispatch rows suppressed because the symbol was already
	// listed as a caller. Reported rather than dropped silently: the reader
	// should know the stronger claim absorbed a weaker one.
	merged int
}

// impactItems renders a report's reachability sets as response items, strongest
// claim first, each symbol at most once.
//
// Order is load-bearing twice over. It is the order the dedup resolves ties in
// — direct beats indirect beats dispatch, so a symbol that is both keeps the
// stronger claim — and it is the order Response.Bound truncates in, since Bound
// drops trailing items, so a response that cannot carry everything sheds the
// weakest claims first.
func impactItems(rep impact.Report) ([]Item, reachCounts) {
	var (
		items []Item
		n     reachCounts
	)

	// impact.Analyze already excludes callers from the dispatch expansion, so
	// on a report it produced this map finds nothing. It is here because
	// rendering is the last place the guarantee can be checked, and a duplicated
	// row is a double-counted blast radius rather than a cosmetic defect.
	seen := make(map[string]struct{}, rep.TotalReached())
	claim := func(sym capability.Symbol) bool {
		key := symbolKey(sym)
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}

	// Prefer the CALL SITE over the symbol's own declaration line. "Which line
	// calls this" is what a developer about to change a signature needs; the
	// declaration is a click away either way. For a dispatch row the evidence is
	// the recorded subtype edge rather than a call — which is exactly what the
	// row's Why says it is.
	callSite := func(d impact.Impacted) (string, int) {
		if len(d.Evidence) > 0 {
			return d.Evidence[0].FilePath, d.Evidence[0].Range.Start.Line
		}
		return d.FilePath, 0
	}

	for _, d := range rep.Direct {
		if !claim(d.Symbol) {
			continue
		}
		path, line := callSite(d)
		why := "calls the target directly"
		if d.Via == model.EdgeInstantiates {
			// A constructor call is recorded against the type. It is a use of
			// the type, and not a call to a function by that name.
			why = "constructs the target directly"
		}
		items = append(items, Item{
			Kind: kindDirect, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence), Why: why,
		})
		n.direct++
	}
	for _, d := range rep.Indirect {
		if !claim(d.Symbol) {
			continue
		}
		path, line := callSite(d)
		items = append(items, Item{
			Kind: kindIndirect, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence),
			Why:        fmt.Sprintf("reaches the target at depth %d (confidence is the weakest link on that path)", d.Depth),
		})
		n.indirect++
	}
	for _, d := range rep.Dispatch {
		if !claim(d.Symbol) {
			// Already rendered under the stronger caller claim. Counted, so the
			// notes can say the two sets overlapped, never re-listed.
			n.merged++
			continue
		}
		path, line := callSite(d.Impacted)
		items = append(items, Item{
			Kind: kindDispatch, Name: d.Name, Path: path, Line: line,
			Confidence: string(d.Confidence), Why: whyDispatched(d),
		})
		n.dispatch++
	}
	return items, n
}

// whyDispatched states HOW a dispatch row was reached, on the row itself.
//
// It leads with the mechanism, not with the detail, for two reasons. A reader
// scanning the list sees "not a direct call" before anything that could be
// mistaken for one — a dispatch row carries a file:line like every other row,
// and without this sentence it reads as a call site. And Limits.MaxWhyBytes
// clips from the end, so the words that must survive clipping go first.
func whyDispatched(d impact.Dispatched) string {
	var how string
	switch d.Reason {
	case impact.ReasonImplementsTarget:
		how = "it implements or overrides the target, so a call naming the target can execute it at run time"
	case impact.ReasonThroughImplementation:
		// Depth counts from the target, so the subtype hop spends one of it. A
		// row that did not carry a usable depth says so by omitting the number
		// rather than by printing a computed one.
		how = "it was reached beyond an implementation of the target, and every path to it " +
			"crosses a subtype edge"
		if d.Depth > 1 {
			how = fmt.Sprintf("it is %d hop(s) beyond an implementation of the target, and every "+
				"path to it crosses a subtype edge", d.Depth-1)
		}
	default:
		// An unrecognised reason still has to say something true. Naming the raw
		// reason is honest; inventing a sentence for it would not be.
		how = fmt.Sprintf("reason %q", string(d.Reason))
	}
	return "NOT a direct call — reached through interface/virtual dispatch: " + how +
		". No call site is proven to take that path"
}

// summary is the one-line count of what the response carries.
func (n reachCounts) summary() string {
	s := fmt.Sprintf("%d direct, %d indirect", n.direct, n.indirect)
	if n.dispatch > 0 {
		s += fmt.Sprintf(", %d reached by interface dispatch", n.dispatch)
	}
	return s
}

// maxThroughNamed bounds the implementations named in the dispatch note.
//
// Notes are rendered in full and are NOT dropped by Response.Bound, which drops
// items — so an unbounded note eats the budget the results need. Three names
// tell the reader where the expansion hinged; a list of forty would not.
const maxThroughNamed = 3

// notes explains the dispatch section once per response.
func (n reachCounts) notes(rep impact.Report) []string {
	var out []string
	if n.dispatch > 0 {
		note := "Items marked 'dispatch' were reached through an interface or an overridden " +
			"method, NOT by a call to the target."
		if named := namesOf(rep.DispatchThrough, maxThroughNamed); named != "" {
			note += " The expansion went through " + named + "."
		}
		out = append(out, note)
	}
	if n.merged > 0 {
		out = append(out, fmt.Sprintf(
			"%d symbol(s) are reachable by dispatch AND reference the target. Each is listed "+
				"once, under the stronger direct/indirect claim.", n.merged))
	}
	return out
}

// namesOf renders at most max symbol names, saying how many it left out.
func namesOf(syms []capability.Symbol, max int) string {
	if len(syms) == 0 || max <= 0 {
		return ""
	}
	shown := syms
	if len(shown) > max {
		shown = shown[:max]
	}
	names := make([]string, 0, len(shown))
	for _, s := range shown {
		names = append(names, clip(s.Name, 60))
	}
	out := strings.Join(names, ", ")
	if rest := len(syms) - len(shown); rest > 0 {
		out += fmt.Sprintf(" and %d more", rest)
	}
	return out
}

// symbolKey identifies a symbol for deduplication across the report's sets.
//
// The symbol id is the real identity and is what a report from impact.Analyze
// always carries. The fallbacks exist because a key that collapses every
// id-less symbol onto one bucket would silently drop rows, and dropping a row
// is a worse failure than listing one twice.
func symbolKey(s capability.Symbol) string {
	switch {
	case s.ID != 0:
		return fmt.Sprintf("id:%d", s.ID)
	case s.FQN != "":
		return "fqn:" + s.FQN
	default:
		return "sym:" + s.FilePath + "#" + s.Name
	}
}

// testsMissingFrom counts the tests in unfiltered that do not appear in
// filtered at all — the honest measure of what a confidence floor cost a
// find_tests caller.
//
// Deliberately NOT len(unfiltered)-len(filtered): a test can survive the
// floor via a weaker signal (naming, co-location) after the floor removes
// only its graph signal, which changes nothing about which tests the caller
// sees even though the two lengths could still differ by coincidence on an
// unrelated symbol. Presence, keyed by symbolKey, is what floorNote's claim
// is actually about — a result the caller would have seen and now does not.
func testsMissingFrom(unfiltered, filtered []tests.TestHit) int {
	present := make(map[string]bool, len(filtered))
	for _, t := range filtered {
		present[symbolKey(t.Symbol)] = true
	}
	missing := 0
	for _, t := range unfiltered {
		if !present[symbolKey(t.Symbol)] {
			missing++
		}
	}
	return missing
}

func (s *Server) findTests(ctx stdctx.Context, req *sdk.CallToolRequest, args testsArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call find_tests again.",
			"symbol must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	if args.MaxDepth < 0 || args.MaxDepth > 8 {
		return failWith("invalid_argument", "Pass max_depth between 1 and 8, or omit it.",
			"max_depth %d is out of range", args.MaxDepth)
	}
	page, err := s.page(args.pageArg, "test(s)")
	if err != nil {
		return failWith("invalid_argument", "Pass limit and offset as non-negative numbers, or omit them.", "%v", err)
	}
	q := targetQuery("find_tests", args.Symbol, args.Path, args.Line)
	sel, sc, scResp := s.selectSymbol(ctx, req, q)
	if sc != nil {
		return sc, scResp, nil
	}
	runID, status := sel.RunID, sel.Status
	tgt := capability.TargetFromCandidate(args.Symbol, sel.Target)

	deps := s.capDeps
	// Only the floor and the hub bound are set; every other bound stays at
	// Options.withDefaults, which is exactly what FindTests would have applied.
	rep, err := tests.FindTestsForTarget(ctx, deps, runID, tgt, tests.Options{
		MinConfidence:  minConf,
		MaxDepth:       args.MaxDepth,
		HubDegreeLimit: args.HubDegreeLimit,
		CheckValueUse:  true,
	})
	if err != nil {
		return failFrom(err, "test discovery: %v", err)
	}

	// "Untested" is the strongest claim this tool makes, and the one a low
	// resolution rate most easily fabricates: a test that calls the symbol
	// through an unresolved reference is a test RepoCortex cannot see. Coverage
	// is judged before that claim is worded, not after.
	cov := s.coverageFor(ctx, runID, rep.Target.Language, rep.Target.FilePath, rep.Binding, aboutTests)
	cov.Misbound = s.boundSites(ctx, runID, sel.Target).conflict(sel.Target)
	cov.Elsewhere = rep.ValueUse

	resp := &Response{IndexRevision: runID, IndexStatus: status}
	where := fmt.Sprintf("%s (%s:%d)", rep.Target.Name, rep.Target.FilePath, rep.Target.Line)
	runnable, helpers, proven := splitRunnable(rep.Tests)
	switch {
	case rep.Untested && rep.Undetermined != "" && cov.saysWhyNothingIsKnown():
		resp.Summary = fmt.Sprintf("%s: no test found, and whether one reaches it is UNKNOWN.", where)
	case rep.Untested && rep.Undetermined != "":
		// Not "0 tests". The search that would have found a calling test could
		// not see one, so the count is of what was visible, and the first line
		// says which.
		resp.Summary = fmt.Sprintf("%s: no test found, and whether one reaches it is UNKNOWN: %s.",
			where, strings.TrimSuffix(rep.Undetermined, "."))
	case rep.Untested && rep.ValueUse.Any():
		// The same refusal for the same reason: a test that reaches the symbol
		// through a function passed as a value leaves no edge, and the name
		// occurs where one would be.
		resp.Summary = fmt.Sprintf("%s: no test found, and whether one reaches it is NOT KNOWN: a function passed "+
			"as a value is reached without a call edge, and its name occurs where no call is recorded.", where)
	default:
		resp.Summary = fmt.Sprintf("%s: %d test(s), %d proven by a call edge%s.",
			where, len(runnable), proven, depthBreakdown(runnable))
	}
	if helpers > 0 {
		// A helper is not run by itself; the tests that reach the target
		// through it are listed and are what to run. Listing the helpers too
		// made the list three times longer than the tests it named.
		resp.Notes = append(resp.Notes, fmt.Sprintf("%d helper function(s) in test files also reach %s and are "+
			"not listed: a test runner does not run them, and every test that reaches %s through one is listed "+
			"if it is within the depth.", helpers, rep.Target.Name, rep.Target.Name))
	}

	items := make([]Item, 0, len(runnable))
	for _, t := range runnable {
		var sigs []string
		for _, s := range t.Signals {
			sigs = append(sigs, string(s.Signal))
		}
		why := "found by " + strings.Join(sigs, ", ")
		switch {
		case t.Depth == 1:
			why = "calls it directly; " + why
		case t.Depth > 1:
			why = fmt.Sprintf("reaches it through %d calls; %s", t.Depth, why)
		}
		items = append(items, Item{
			Kind: "test", Name: t.Name, Path: t.FilePath,
			Confidence: string(t.Confidence),
			// Naming the signal matters: "found by a proven call edge" and
			// "found because the filename looked right" are different claims,
			// and an agent deciding what to run should know which it has.
			Why: why,
		})
	}
	paginate(resp, page, items)

	s.applyCoverage(resp, cov, "test discovery")

	// Ahead of the "untested" claim below, not after it: a floor is one more
	// reason a symbol can look untested when it is not.
	//
	// dropped can't come from a count already in rep: the floor is applied
	// inside the graph traversal (naming and co-location signals are never
	// confidence-scoped — see tests.go), so a test the floor removed from the
	// graph signal can still be present in rep.Tests via one of those, and rep
	// carries no count of what the floor alone excluded. Re-running the
	// unfiltered discovery and diffing the symbol sets is the only way to
	// answer "did the floor actually remove anything a caller would have seen"
	// from within this package — paid only when a floor was actually
	// requested, not on the default (low) path every other caller takes.
	dropped := 0
	if minConf != "" && minConf != model.ConfidenceLow {
		// The hub bound is carried over: this walk must differ from the one
		// above in exactly one respect, or a test the HUB CUT removed would be
		// counted as removed by the floor.
		unfiltered, uerr := tests.FindTestsForTarget(ctx, deps, runID, tgt,
			tests.Options{HubDegreeLimit: args.HubDegreeLimit})
		if uerr == nil {
			dropped = testsMissingFrom(unfiltered.Tests, rep.Tests)
		}
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	switch {
	case rep.Untested && rep.Undetermined != "":
		resp.Notes = append(resp.Notes,
			"No test was found by a call edge, a naming convention or a sibling test file. The call-edge "+
				"search could not have found one: "+strings.TrimSuffix(rep.Undetermined, ".")+
				". This says nothing about whether the symbol is tested. Search for the name in the test files.")
	case rep.Untested && rep.ValueUse.Any():
		resp.Notes = append(resp.Notes,
			"No test was found by a call edge, a naming convention or a sibling test file. Whether one "+
				"reaches the symbol is not known: "+rep.ValueUse.Detail()+" Do not read this as untested.")
	case rep.Untested:
		resp.Notes = append(resp.Notes,
			"No tests found for this symbol. That is a positive finding — but it is bounded "+
				"by what the index resolved, so confirm before concluding the code is untested.")
	case rep.Undetermined != "":
		resp.Notes = append(resp.Notes,
			"None of the tests listed is proven to call the symbol; they were matched by name or by file. "+
				"Whether a test calls it could not be determined: "+strings.TrimSuffix(rep.Undetermined, ".")+".")
	}
	if n := s.testsCoverageNote(ctx, runID, rep.Target.SymbolID, rep.Target.Name); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	for _, s := range tests.SuggestMissingScenarios(rep.Target, rep) {
		resp.Notes = append(resp.Notes, "Possible gap: "+s.Scenario+" — "+s.Why)
	}
	if rep.Truncated {
		resp.markTruncated(rep.TruncatedReason)
	}
	if n := sel.note(q); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	return s.reply(ctx, req, resp)
}

func (s *Server) planChange(ctx stdctx.Context, req *sdk.CallToolRequest, args targetArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call plan_change again.",
			"symbol must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	page, err := s.page(args.pageArg, "plan row(s)")
	if err != nil {
		return failWith("invalid_argument", "Pass limit and offset as non-negative numbers, or omit them.", "%v", err)
	}
	q := targetQuery("plan_change", args.Symbol, args.Path, args.Line)
	sel, sc, scResp := s.selectSymbol(ctx, req, q)
	if sc != nil {
		return sc, scResp, nil
	}
	runID, status := sel.RunID, sel.Status
	tgt := capability.TargetFromCandidate(args.Symbol, sel.Target)

	planner := planchange.NewPlanner(s.capDeps)
	plan, err := planner.PlanTarget(ctx, runID, tgt, planchange.Options{
		MinConfidence:  minConf,
		HubDegreeLimit: args.HubDegreeLimit,
	})
	if err != nil {
		return failFrom(err, "plan change for %q: %v", args.Symbol, err)
	}

	binding, err := s.capDeps.TargetBinding(ctx, runID, plan.Target)
	if err != nil {
		return failFrom(err, "plan change for %q: %v", args.Symbol, err)
	}
	cov := s.coverageFor(ctx, runID, sel.Target.Language, sel.Target.FilePath, binding, aboutCallers)
	cov.Misbound = s.boundSites(ctx, runID, sel.Target).conflict(sel.Target)
	cov.Elsewhere = plan.ValueUse

	where := fmt.Sprintf("%s (%s:%d)", plan.Target.Name, plan.Target.FilePath, plan.Target.Line)
	resp := &Response{IndexRevision: runID, IndexStatus: status}
	switch {
	case plan.RiskLevel == impact.LevelUnknown && cov.saysWhyNothingIsKnown():
		resp.Summary = fmt.Sprintf("Change plan for %s: Risk UNKNOWN, and the caller steps below are only "+
			"the %d caller(s) that could be bound.", where, plan.DirectCallers)
	case plan.RiskLevel == impact.LevelUnknown:
		// The plan lists the callers that were FOUND. With the callers
		// undetermined, a plan that says "0 direct callers, risk LOW" is a plan
		// to change a signature without updating anything.
		resp.Summary = fmt.Sprintf("Change plan for %s: Risk UNKNOWN, and the caller steps below are only "+
			"the %d caller(s) that could be bound: %s.",
			where, plan.DirectCallers, strings.TrimSuffix(plan.RiskUndetermined, "."))
	case plan.RiskUndetermined != "":
		why := fmt.Sprintf("%d/100 counts only the callers that were bound", plan.RiskScore)
		if plan.RiskPartial != "" {
			why += "; " + plan.RiskPartial
		}
		resp.Summary = fmt.Sprintf("Change plan for %s: Risk %s or higher (%s), %d direct caller(s) found, "+
			"%d total reached.",
			where, strings.ToUpper(plan.RiskLevel), why, plan.DirectCallers, plan.TotalCallers)
	case plan.RiskPartial != "":
		resp.Summary = fmt.Sprintf("Change plan for %s: Risk %s or higher (%d/100; %s), %d direct caller(s) "+
			"found, %d total reached.",
			where, strings.ToUpper(plan.RiskLevel), plan.RiskScore, plan.RiskPartial, plan.DirectCallers, plan.TotalCallers)
	default:
		resp.Summary = fmt.Sprintf("Change plan for %s: Risk %s (%d/100), %d direct caller(s), %d total affected.",
			where, strings.ToUpper(plan.RiskLevel), plan.RiskScore, plan.DirectCallers, plan.TotalCallers)
	}

	// The target row carries NO confidence tier. It used to carry the tier the
	// DEFINITION was extracted at, and Server.reply graded that one row into
	// "verdict: strong ... Act on these" over a plan with no caller in it.
	targetWhy := "target symbol to modify"
	if plan.RiskLevel != impact.LevelUnknown {
		targetWhy += fmt.Sprintf(" (risk score: %d/100, level: %s)", plan.RiskScore, plan.RiskLevel)
	}
	if plan.Target.Confidence != "" {
		targetWhy += fmt.Sprintf("; definition extracted at %s confidence", plan.Target.Confidence)
	}
	items := []Item{{
		Kind: "target",
		Name: plan.Target.Name,
		FQN:  plan.Target.FQN,
		Path: plan.Target.FilePath,
		Line: plan.Target.Line,
		Why:  targetWhy,
	}}
	for _, step := range plan.Steps {
		items = append(items, Item{
			Kind: "plan_step",
			Name: fmt.Sprintf("Step %d: %s", step.Order, step.Kind),
			Path: step.Path,
			Line: step.Line,
			Why:  step.Description,
		})
	}
	paginate(resp, page, items)

	s.applyCoverage(resp, cov, "the change plan")

	if len(plan.AffectedTests) > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf("Covering tests to execute: %s", strings.Join(plan.AffectedTests, ", ")))
	}
	if plan.ValueUse.Any() && len(plan.AffectedTests) == 0 {
		resp.Notes = append(resp.Notes,
			"No covering test is listed, which does not mean none exists: "+plan.ValueUse.Detail())
	}
	if plan.TestsUndetermined != "" {
		resp.Notes = append(resp.Notes,
			"Whether a test calls the target could not be determined, so the test list above is not "+
				"the set to rely on: "+strings.TrimSuffix(plan.TestsUndetermined, ".")+".")
	}
	if n := sel.note(q); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	// What the source itself says not to break, in the plan for changing it.
	if n := s.markedNote(ctx, runID, sel.Target.SymbolHit); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	return s.reply(ctx, req, resp)
}

func (s *Server) detectBlindspots(ctx stdctx.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	detector := missed.NewDetector(s.deps.Registry, s.capDeps)
	rep, err := detector.Detect(ctx, runID, missed.DefaultOptions())
	if err != nil {
		return failFrom(err, "detect blindspots: %v", err)
	}

	summary := fmt.Sprintf("%d code intelligence blindspots detected across repository.", rep.TotalBlindspots)
	if len(rep.Capped) > 0 {
		summary = fmt.Sprintf("At least %d code intelligence blindspots: %d categories stopped at %d findings each, so their counts are lower bounds.",
			rep.TotalBlindspots, len(rep.Capped), missed.DefaultOptions().MaxItemsPerKind)
	}
	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       summary,
	}

	for _, b := range rep.Blindspots {
		resp.Items = append(resp.Items, Item{
			Kind: string(b.Kind),
			Name: b.Target,
			Path: b.Path,
			Line: b.Line,
			Why:  fmt.Sprintf("[%s] %s -> %s", b.Severity, b.Description, b.Remediation),
		})
	}

	if rep.TotalBlindspots == 0 {
		resp.Notes = append(resp.Notes, "No code intelligence blindspots detected. Graph and configuration linkages are healthy.")
	}

	return s.reply(ctx, req, resp)
}

// splitRunnable separates the tests a runner runs from helpers in test files,
// and counts the runnable ones proven by a call path.
func splitRunnable(all []tests.TestHit) (runnable []tests.TestHit, helpers, proven int) {
	for _, t := range all {
		if !t.Runnable {
			helpers++
			continue
		}
		runnable = append(runnable, t)
		if t.Strongest == tests.SignalGraph {
			proven++
		}
	}
	return runnable, helpers, proven
}

// depthBreakdown says how near the tests are: ": 6 call it directly, 24
// through one more call, 3 deeper".
func depthBreakdown(ts []tests.TestHit) string {
	var direct, two, deeper int
	for _, t := range ts {
		switch {
		case t.Depth == 1:
			direct++
		case t.Depth == 2:
			two++
		case t.Depth > 2:
			deeper++
		}
	}
	if direct+two+deeper == 0 {
		return ""
	}
	var parts []string
	if direct > 0 {
		parts = append(parts, fmt.Sprintf("%d call it directly", direct))
	}
	if two > 0 {
		parts = append(parts, fmt.Sprintf("%d through one more call", two))
	}
	if deeper > 0 {
		parts = append(parts, fmt.Sprintf("%d deeper", deeper))
	}
	return ": " + strings.Join(parts, ", ")
}
