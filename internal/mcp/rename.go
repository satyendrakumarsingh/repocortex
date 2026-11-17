package mcp

// plan_rename: the edit set for a rename, split by whether RepoCortex can defend
// each edit.
//
// Why this is not `rename_symbol`. A correct cross-file rename requires
// COMPLETE reference resolution. RepoCortex's confidence tiers are a measured
// admission that it does not have that outside the languages where a SCIP index
// exists. A rename tool that silently missed the medium-confidence references
// would corrupt code in exactly the cases the tiers were built to flag — it
// would be worse than having no rename at all.
//
// So the tool returns a plan and never edits. The agent applies the proven half
// and reviews the rest, which is the honest division of labour and something no
// LSP-backed competitor offers, because an LSP has no vocabulary for "I am
// unsure about this one."

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// RenameEdit is one SITE that would have to change. It is not an edit.
//
// A file and a line, with no column and no span. edges.evidence_col is
// stored — every edge insert writes it (internal/registry/writer.go) and
// neighbour queries already read it back (internal/graph/neighbours.go) —
// but registry.EdgeHit does not project it, which by itself would just be a
// one-column addition to the SELECT in internal/registry/query.go. The real
// blocker is upstream of that: the column values SCIP hands the indexer are
// character offsets in whatever PositionEncoding that document declared —
// UTF-16 code units for the JVM and TypeScript indexers, UTF-8 bytes only for
// Go and Rust ones (internal/resolve/scip/ingest.go) — while
// model.Position.Col is documented as a byte column. A column surfaced here
// today would be silently wrong for exactly the Java and TypeScript
// repositories this tool is most used on. So the line is handed to the agent
// to search instead, and doing that badly (a line may contain the identifier
// several times, or as a substring of a longer name) is how a rename
// corrupts code.
//
// So the honest shape is the one that is actually backed: RepoCortex says WHERE
// to look and how well it can defend the binding, and the agent locates the
// identifier on the line. Anything calling this an edit set that can be
// applied unattended is claiming a span this struct does not have. If the
// indexer ever records evidence columns, add them here and the claim can grow
// to match.
type RenameEdit struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Confidence string `json:"confidence"`

	// Why explains the binding, so a reviewer can judge a non-exact edit
	// without opening the file first.
	Why string `json:"why,omitempty"`
}

// RenamePlan is the partitioned result.
type RenamePlan struct {
	// Apply holds the sites RepoCortex can defend: a compiler proved the
	// binding. It is still a list of SITES, not of applicable edits — each
	// entry names a file and a line and the agent must locate the identifier
	// on that line itself, because no column RepoCortex can defend as a byte
	// offset is available here. See RenameEdit.
	Apply []RenameEdit `json:"apply"`

	// Review holds everything else, in descending confidence. These are real
	// candidates, not noise — they are simply not proven.
	Review []RenameEdit `json:"review"`

	// Complete is always false. It exists as an explicit field rather than an
	// omission so that a client reading the JSON cannot mistake silence for a
	// completeness guarantee.
	Complete bool `json:"complete"`

	// Warning states the incompleteness in prose the model will actually read.
	Warning string `json:"warning"`
}

const renameWarning = "This plan is NOT complete. RepoCortex returns the references it resolved; " +
	"references it could not bind are absent and are not reported as absent. " +
	"Apply the 'apply' list, review the 'review' list by hand, then run search_code " +
	"for the old name to find bindings RepoCortex missed entirely."

// partitionEdits splits edits into the provable and the rest.
//
// Only `exact` reaches Apply. `high` measures between 86.8% and 96.1% precision,
// which is excellent for ranking and unacceptable for an unattended edit: at
// 90%, a rename touching thirty sites corrupts three of them.
func partitionEdits(edits []RenameEdit) RenamePlan {
	plan := RenamePlan{
		Apply:    []RenameEdit{},
		Review:   []RenameEdit{},
		Complete: false,
		Warning:  renameWarning,
	}

	for _, e := range edits {
		if model.Confidence(e.Confidence) == model.ConfidenceExact {
			plan.Apply = append(plan.Apply, e)
			continue
		}
		plan.Review = append(plan.Review, e)
	}

	byPathThenLine := func(s []RenameEdit) {
		sort.SliceStable(s, func(i, j int) bool {
			if s[i].Path != s[j].Path {
				return s[i].Path < s[j].Path
			}
			return s[i].Line < s[j].Line
		})
	}
	byPathThenLine(plan.Apply)

	// Review is ordered by confidence first so a reviewer works down from the
	// most likely, then by path and line for determinism.
	sort.SliceStable(plan.Review, func(i, j int) bool {
		ri := model.Confidence(plan.Review[i].Confidence).Rank()
		rj := model.Confidence(plan.Review[j].Confidence).Rank()
		if ri != rj {
			return ri > rj
		}
		if plan.Review[i].Path != plan.Review[j].Path {
			return plan.Review[i].Path < plan.Review[j].Path
		}
		return plan.Review[i].Line < plan.Review[j].Line
	})

	return plan
}

// refLookup is the success-path result of referencesFor: everything a caller
// needs to finish building its own response (set Summary, reply).
// relationshipQuery consumes Target and RunID; planRename discards them — a
// struct lets it do that by construction instead of a run of blank identifiers
// at the call site.
type refLookup struct {
	Items  []Item
	Resp   *Response
	Target registry.SymbolCandidate
	RunID  int64
	Status string
}

// referencesFor selects a definition and fetches one page of what points at
// it, filtered to minConf and, if kinds is non-nil, restricted to those edge
// kinds. It is the ONE reference-lookup implementation shared by
// relationshipQuery (find_references / find_callers) and planRename — neither
// duplicates the registry/graph calls the other makes.
//
// The kind predicate is pushed into the registry query (EdgeFilter.Kinds)
// rather than applied to the results afterward: the page is a LIMIT on the SQL
// query, and filtering in Go after that LIMIT would let a flood of
// same-or-higher-confidence edges of an excluded kind crowd a wanted edge out
// of the page before a Go-side filter ever saw it.
//
// When the run or the symbol cannot be resolved, the name is ambiguous, or the
// lookup itself fails, shortCircuit is the fully-formed result the caller
// should return immediately, unchanged — the crafted result IS the answer,
// including for a missing index, where it carries the "Run 'repocortex index'"
// guidance rather than a bare protocol error. That is why referencesFor never
// returns an error itself: the go-sdk's ToolHandlerFor wrapper discards a
// handler's result whenever the handler also returns a non-nil error.
// shortCircuit is nil only on the success path, where the caller still has
// work to do (set Summary and reply) before handing the response back.
func (s *Server) referencesFor(
	ctx context.Context,
	req *sdk.CallToolRequest,
	q symbolQuery,
	minConf model.Confidence,
	kinds []string,
	page *Page,
	checkValueUse bool,
) (refLookup, *sdk.CallToolResult) {
	sel, sc, scResp := s.selectSymbol(ctx, req, q)
	if sc != nil {
		return refLookup{Resp: scResp}, sc
	}
	runID, status, target := sel.RunID, sel.Status, sel.Target

	filter := registry.EdgeFilter{MinConfidence: minConf, Kinds: kinds, Within: q.Within}
	incoming, total, ierr := s.deps.Registry.EdgePage(ctx, runID, target.ID, true, page.Limit, page.Offset, filter)
	if ierr != nil {
		sc, r, _ := failFrom(ierr, "relationship lookup failed: %v", ierr)
		return refLookup{Resp: r, Target: target, RunID: runID, Status: status}, sc
	}
	page.Total = total

	// How each bound call was MADE, read back so a row can show it: the text in
	// front of the name, and what that text is (a package, a type, a value).
	// A call on a value of unknown type is bound on the member name alone, and
	// a note can say so only if this is read.
	sites := s.boundSites(ctx, runID, target)

	resp := &Response{IndexRevision: runID, IndexStatus: status, Page: page}
	for _, e := range incoming {
		resp.Items = append(resp.Items, Item{
			Kind:       strings.ToLower(e.Kind),
			Name:       sourceName(e),
			Path:       e.EvidenceFile,
			Line:       e.EvidenceLine,
			Confidence: e.Confidence,
			Source:     e.Source,
			Why:        joinWhy(sites.writtenAs(e.EvidenceFile, e.EvidenceLine), e.Detail),
		})
	}

	// min_confidence is a floor, not a preference: results below it are
	// omitted, and the omission is reported here so an empty or short result
	// is never mistaken for "nothing further exists". The sentence itself is
	// floorNote's, shared with every other tool that takes a floor.
	//
	// dropped is an exact COUNT(*): the same kind filter without the
	// confidence floor, less what passed with it.
	var dropped int
	if minConf != "" && minConf != model.ConfidenceLow {
		unfiltered, terr := s.deps.Registry.IncomingEdgeCount(ctx, runID, target.ID,
			registry.EdgeFilter{Kinds: kinds, Within: q.Within}, nil)
		if terr == nil {
			dropped = unfiltered - total
		}
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	// What the index did with the references spelled like THIS name. The
	// run-wide figure used to stand in for it ("5748 references repository-wide
	// are unresolved or ambiguous, so this list may be incomplete"), which is
	// true of every symbol in the repository and so says nothing about this
	// one.
	binding, berr := s.capDeps.TargetBinding(ctx, runID, capability.TargetFromCandidate(q.Name, target))
	if berr != nil {
		sc, r, _ := failFrom(berr, "relationship lookup failed: %v", berr)
		return refLookup{Resp: r, Target: target, RunID: runID, Status: status}, sc
	}
	cov := s.coverageFor(ctx, runID, target.Language, target.FilePath, binding, aboutCallers)
	cov.Misbound = sites.conflict(target)
	if checkValueUse {
		// A caller list is short by every use of the function as a value. The
		// target's own name is checked; there is no path to walk here.
		vu, verr := s.capDeps.ValueUse(ctx, runID, capability.TargetFromCandidate(q.Name, target), nil, nil)
		if verr != nil {
			sc, r, _ := failFrom(verr, "relationship lookup failed: %v", verr)
			return refLookup{Resp: r, Target: target, RunID: runID, Status: status}, sc
		}
		cov.Elsewhere = vu
	}

	// THE GATE COMES FIRST, AHEAD OF EVERY OTHER NOTE: whether the answer can
	// be trusted at all outranks the detail below it. Reliability is the same
	// judgement in the form the verdict and the first line are derived from.
	resp.Reliability = cov.reliability("relationship lookup")
	resp.Notes = append(cov.caveat("relationship lookup"), resp.Notes...)

	if note := bindingNote(binding, total); note != "" {
		resp.Notes = append(resp.Notes, note)
	}
	if note := sites.note(target.Name); note != "" {
		resp.Notes = append(resp.Notes, note)
	}
	if note := sel.note(q); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	if checkValueUse { // checkValueUse is true for callersOnly (find_callers)
		if ifNotes := s.interfaceDispatchNotes(ctx, runID, target); len(ifNotes) > 0 {
			resp.Notes = append(resp.Notes, ifNotes...)
		}
	}

	return refLookup{Items: resp.Items, Resp: resp, Target: target, RunID: runID, Status: status}, nil
}

func (s *Server) interfaceDispatchNotes(ctx context.Context, runID int64, target registry.SymbolCandidate) []string {
	var notes []string
	if s.deps.Registry == nil {
		return nil
	}

	// 1. Direct method-level hierarchy: does target implement or extend a supertype method?
	upEdges, _, err := s.deps.Registry.EdgePage(ctx, runID, target.ID, false, 5, 0, registry.EdgeFilter{Kinds: hierarchyKinds})
	if err == nil && len(upEdges) > 0 {
		for _, e := range upEdges {
			if e.DstID > 0 {
				callers, _ := s.deps.Registry.IncomingEdgeCount(ctx, runID, e.DstID, registry.EdgeFilter{Kinds: []string{"CALLS"}}, nil)
				if dstSym, ok, _ := s.deps.Registry.SymbolByID(ctx, runID, e.DstID); ok {
					notes = append(notes, fmt.Sprintf(
						"Interface dispatch: %s implements %s (%s:%d), which has %d recorded caller(s). Upstream callers invoke the contract.",
						target.Name, dstSym.Name, dstSym.FilePath, dstSym.StartLine, callers))
				}
			}
		}
	}

	// 2. Class/Struct owner-level hierarchy: if target has an owner (e.g. class UserRepoImpl),
	// check if Owner implements an interface that defines target.Name.
	if target.Owner != "" {
		if supertypes, serr := s.deps.Registry.Supertypes(ctx, runID, []string{target.Owner}); serr == nil {
			for _, st := range supertypes[target.Owner] {
				cands, _, cerr := s.deps.Registry.SymbolCandidates(ctx, runID, target.Name, 10)
				if cerr != nil {
					continue
				}
				for _, c := range cands {
					if c.Owner == st && c.ID != target.ID {
						callers, _ := s.deps.Registry.IncomingEdgeCount(ctx, runID, c.ID, registry.EdgeFilter{Kinds: []string{"CALLS"}}, nil)
						notes = append(notes, fmt.Sprintf(
							"Interface dispatch: %s is declared on %s (which implements %s). %s.%s (%s:%d) has %d recorded caller(s) through interface/dependency injection dispatch.",
							target.Name, target.Owner, st, st, target.Name, c.FilePath, c.StartLine, callers))
						break
					}
				}
			}
		}
	}

	// 3. Reverse direction: does target define an interface method implemented by concrete types?
	downEdges, downTotal, derr := s.deps.Registry.EdgePage(ctx, runID, target.ID, true, 5, 0, registry.EdgeFilter{Kinds: hierarchyKinds})
	if derr == nil && downTotal > 0 {
		var implNames []string
		for _, e := range downEdges {
			if srcSym, ok, _ := s.deps.Registry.SymbolByID(ctx, runID, e.SrcID); ok {
				implNames = append(implNames, fmt.Sprintf("%s (%s:%d)", srcSym.Name, srcSym.FilePath, srcSym.StartLine))
			}
		}
		notes = append(notes, fmt.Sprintf(
			"Interface contract: %s is implemented by %d concrete type(s) or method(s) (including %s). Calls to this method dispatch to implementations at runtime.",
			target.Name, downTotal, strings.Join(implNames, ", ")))
	}

	return notes
}

// bindingNote states, in one sentence, what the index did with every reference
// spelled like the symbol. listed is how many relationships the answer found.
//
// An empty answer leads with the old warning, because it is the case an agent
// most needs it for, and then says the one thing that warning never could:
// whether references to this name exist at all.
func bindingNote(b capability.TargetBinding, listed int) string {
	if !b.Known {
		if listed == 0 {
			return "No results found — but this is a LOWER BOUND, not proof there are none. " +
				"Do not treat this as evidence the symbol is unused."
		}
		return ""
	}

	var account string
	switch {
	case b.References == 0 && !b.LanguageExtracts && b.LanguageDefinitions > 0:
		// "No reference by this name" is no comfort in a language none of whose
		// references were recorded: it is what every name there looks like.
		account = fmt.Sprintf("No reference spelled %q is recorded in this index, and none is recorded "+
			"from any %s code, so its absence says nothing about whether the symbol is used.",
			b.Name, capability.DisplayLanguage(b.Language))
	case b.References == 0:
		account = fmt.Sprintf("No reference spelled %q was recorded anywhere in this index. That rules out "+
			"an unbound call by this name; it does not rule out a call the parser did not extract, "+
			"reflection, a call through another name, or a file the index skipped.", b.Name)
	default:
		parts := []string{fmt.Sprintf("%d bound to this definition", b.BoundHere)}
		if b.WithoutEdge > 0 {
			parts[0] += fmt.Sprintf(" (%d of them from outside any indexed function, so bound but not listed)", b.WithoutEdge)
		}
		if b.BoundElsewhere > 0 {
			parts = append(parts, fmt.Sprintf("%d bound to other definitions of the name", b.BoundElsewhere))
		}
		if b.ProvenOther > 0 {
			parts = append(parts, fmt.Sprintf("%d shown by the compiler to be uses of something else", b.ProvenOther))
		}
		if b.Ambiguous > 0 {
			parts = append(parts, fmt.Sprintf("%d ambiguous", b.Ambiguous))
		}
		if b.Unresolved > 0 {
			parts = append(parts, fmt.Sprintf("%d unresolved", b.Unresolved))
		}
		if b.Pending > 0 {
			parts = append(parts, fmt.Sprintf("%d not yet resolved", b.Pending))
		}
		account = fmt.Sprintf("References spelled %q in this index: %d. %s.",
			b.Name, b.References, strings.Join(parts, ", "))
	}
	if listed == 0 {
		return "No results found — but this is a LOWER BOUND, not proof there are none. " + account +
			" Do not treat this as evidence the symbol is unused."
	}
	return account
}

// maxBoundSites bounds how many bound references are read to learn how each
// call was made. A symbol with more than this has its first rows annotated and
// the rest not, which the note says.
const maxBoundSites = 2000

// maxQualifiersNamed bounds the qualifiers a note lists.
const maxQualifiersNamed = 5

// boundSiteSet is how the references bound to one definition were made.
type boundSiteSet struct {
	name string
	// at maps "path:line" to the site read there. A site with two references
	// to the name on one line keeps the first.
	at map[string]registry.BoundSite
	// counts is occurrences per spelling, "" for an unqualified call.
	counts map[string]int
	// onValue counts calls made on a value, or on an expression, whose type
	// the source does not state. The resolver binds those on the member name
	// alone, because nothing it can read says which type the receiver is.
	onValue int
	// impossible counts, per receiver, the calls made on an instance, a value,
	// a stated type or an expression. See conflict.
	impossible map[string]int
	// capped is true when the read stopped at maxBoundSites.
	capped bool
}

// anExpression is how a call on an expression with no name is spelled in a
// note. The index stores model.OpaqueReceiver for it, which is not a name.
const anExpression = "an expression"

// spelling is what a note prints for the qualifier of a site: "" for a call
// with none.
func spelling(b registry.BoundSite) string {
	if b.ReceiverKind == model.ReceiverExpr {
		return anExpression
	}
	return b.Qualifier
}

// boundSites reads how the references bound to target were made. A read that
// fails yields an empty set: the rows are still correct without the
// annotation, and it is not worth failing an answer over.
func (s *Server) boundSites(ctx context.Context, runID int64, target registry.SymbolCandidate) boundSiteSet {
	set := boundSiteSet{
		name: target.Name, at: map[string]registry.BoundSite{},
		counts: map[string]int{}, impossible: map[string]int{},
	}
	sites, err := s.deps.Registry.BoundSites(ctx, runID, target.ID, maxBoundSites)
	if err != nil {
		return set
	}
	set.capped = len(sites) == maxBoundSites
	for _, b := range sites {
		key := fmt.Sprintf("%s:%d", b.Path, b.Line)
		if _, seen := set.at[key]; !seen {
			set.at[key] = b
		}
		set.counts[spelling(b)]++
		switch b.ReceiverKind {
		case model.ReceiverValue, model.ReceiverExpr:
			set.onValue++
			set.impossible[spelling(b)]++
		case model.ReceiverSelf, model.ReceiverSuper, model.ReceiverType:
			set.impossible[spelling(b)]++
		}
	}
	return set
}

// writtenAs is "written as errors.New" for a site whose call carries a
// qualifier, and "" for an unqualified call or an unknown site.
func (b boundSiteSet) writtenAs(path string, line int) string {
	site, ok := b.at[fmt.Sprintf("%s:%d", path, line)]
	if !ok {
		return ""
	}
	if site.ReceiverKind == model.ReceiverExpr {
		return "written as a call on " + anExpression
	}
	if site.Qualifier == "" {
		return ""
	}
	return fmt.Sprintf("written as %s.%s", clip(site.Qualifier, 60), b.name)
}

// qualifiers returns the distinct non-empty spellings, most frequent first.
func (b boundSiteSet) qualifiers() []nameCount {
	return sortedCounts(withoutKey(b.counts, ""))
}

func withoutKey(m map[string]int, key string) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// note says how the bound calls were made, when that is worth a sentence: the
// calls are spelled more than one way, or some of them were bound on the
// member name alone. Silent otherwise: one spelling bound through its
// qualifier is nothing to report.
func (b boundSiteSet) note(name string) string {
	quals := b.qualifiers()
	oneSpelling := len(quals) == 0 || len(quals) == 1 && b.counts[""] == 0
	if oneSpelling && b.onValue == 0 {
		return ""
	}
	var parts []string
	if n := b.counts[""]; n > 0 {
		parts = append(parts, fmt.Sprintf("%s ×%d", name, n))
	}
	for i, q := range quals {
		if i == maxQualifiersNamed {
			parts = append(parts, fmt.Sprintf("and %d more spelling(s)", len(quals)-maxQualifiersNamed))
			break
		}
		if q.name == anExpression {
			parts = append(parts, fmt.Sprintf("a call on %s ×%d", anExpression, q.count))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s.%s ×%d", clip(q.name, 60), name, q.count))
	}
	out := "How the calls bound to this definition were made: " + strings.Join(parts, ", ") + "."
	if b.onValue > 0 {
		out += fmt.Sprintf(" %d of them were made on a value or an expression whose type the source does not "+
			"state. Those were bound on the member NAME alone, because no other type in the repository "+
			"declares a member of it; nothing showed that the receiver is this definition's type, so "+
			"they are the rows most likely to be a call to something else.", b.onValue)
	} else {
		out += " A qualified call is bound through what its qualifier names (an imported package, a " +
			"module, a type, or a field of a stated type), so one definition is expected to be spelled " +
			"several ways, such as a package under different import aliases; the reason on each row " +
			"says which rule bound it."
	}
	if b.capped {
		out += fmt.Sprintf(" Only the first %d bound references were read for this.", maxBoundSites)
	}
	return out
}

// conflict reports calls that cannot be calls to the target, for the one case
// where that is provable from the shape of the call alone.
//
// A Go package-level function is reached by its bare name inside its package
// and as `pkg.Name` outside it. It is never reached through an instance, a
// value, a value of a stated type or an expression: `x.New()` calls a method
// of x. A call recorded as made on one of those, and bound to a Go function,
// was bound to the wrong thing.
//
// This is NOT a test of how many different qualifiers the calls carry. It was:
// two package qualifiers on calls to one function meant one of them named
// another package, back when a qualified call was bound by its name. A call is
// now bound through its qualifier, and one package is imported under different
// aliases in different files, so `devmcp.New` and `mcp.New` can both be right.
//
// The current resolver binds a call on a value only to a member, so this does
// not fire on an index it wrote. It is a check on the resolver: it fires if a
// regression, or a rule added later, binds such a call to a free function.
//
// It is deliberately not applied to methods, or to other languages. There the
// receiver is a variable or an expression, different at every correct call
// site, and reading that as a conflict would mark every method unverified.
func (b boundSiteSet) conflict(target registry.SymbolCandidate) string {
	if strings.ToLower(target.Language) != "go" || target.Kind != "function" {
		return ""
	}
	var total int
	for _, n := range b.impossible {
		total += n
	}
	if total == 0 {
		return ""
	}
	recv := sortedCounts(b.impossible)
	names := make([]string, 0, maxQualifiersNamed)
	for i, q := range recv {
		if i == maxQualifiersNamed {
			break
		}
		if q.name == anExpression {
			names = append(names, "a call on "+anExpression)
			continue
		}
		names = append(names, clip(q.name, 60)+"."+b.name)
	}
	return fmt.Sprintf("%d call(s) bound to this Go function were made on a value, an instance or an "+
		"expression (%s), and a Go function cannot be called that way.",
		total, strings.Join(names, ", "))
}

// joinWhy joins the non-empty parts of a row's explanation.
func joinWhy(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "; ")
}

type renameArgs struct {
	Name    string `json:"name" jsonschema:"Symbol to rename."`
	NewName string `json:"new_name" jsonschema:"The replacement identifier. Used only to render the plan; nothing is written."`
	Path    string `json:"path,omitempty" jsonschema:"File or directory of the definition, to choose between symbols that share the name."`
	lineArg

	// A floor here narrows the REVIEW half of the plan. The apply half is
	// already 'exact' by construction, so min_confidence=exact returns the
	// proven edits alone — the plan an agent can act on without reading a
	// single line by hand.
	minConfidenceArg
	pageArg
}

// planRename answers plan_rename. It writes nothing: no file is opened for
// writing anywhere in this function or anything it calls.
func (s *Server) planRename(ctx context.Context, req *sdk.CallToolRequest, args renameArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call plan_rename again.",
			"name must not be empty")
	}
	if strings.TrimSpace(args.NewName) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty new_name, then call plan_rename again.",
			"new_name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}

	// The DEFAULT is still model.ConfidenceLow, decoded from an absent
	// min_confidence: the plan must see EVERY candidate reference unless the
	// caller says otherwise, because filtering by default would hide exactly
	// the edits a human reviewer needs to see. A caller that asks for a floor
	// is asking for the narrower plan on purpose, and referencesFor says so in
	// the notes.
	//
	// kinds is explicitly nil: plan_rename wants every edge kind, not a
	// pushed-down subset, so "no kind filter" is stated rather than left to
	// an empty variadic call.
	page, err := s.page(args.pageArg, "site(s)")
	if err != nil {
		return failWith("invalid_argument", "Pass limit and offset as non-negative numbers, or omit them.", "%v", err)
	}
	q := symbolQuery{Tool: "plan_rename", Param: "name", PathParam: "path", LineParam: "line",
		Name: args.Name, Path: args.Path, Line: args.Line,
		Rest: " new_name=" + quoteArg(args.NewName)}
	lookup, shortCircuit := s.referencesFor(ctx, req, q, minConf, nil, page, false)
	if shortCircuit != nil {
		return shortCircuit, lookup.Resp, nil
	}
	resp := lookup.Resp

	edits := make([]RenameEdit, 0, len(lookup.Items))
	for _, it := range lookup.Items {
		edits = append(edits, RenameEdit{
			Path:       it.Path,
			Line:       it.Line,
			Confidence: it.Confidence,
			Why:        it.Why,
		})
	}

	plan := partitionEdits(edits)
	resp.Summary = fmt.Sprintf(
		"Rename %q (%s:%d) to %q: %d proven site(s) to change, %d to review by hand. Each entry is a file and a line; locate the identifier on it. The plan is not complete.",
		args.Name, lookup.Target.FilePath, lookup.Target.StartLine, args.NewName, len(plan.Apply), len(plan.Review))
	// Verdict is NOT set here. Server.reply grades every response once, after
	// bounding — see Response.Verdict.
	resp.Notes = append(resp.Notes, plan.Warning)
	resp.RenamePlan = &plan
	return s.reply(ctx, req, resp)
}

// sourceName is the name a row gives the symbol a relationship comes from:
// with its owner when the index knows one (Writer.AddEdge), so that the reader
// is not left to work out which AddEdge.
func sourceName(e registry.EdgeHit) string {
	if e.SrcQualified != "" {
		return e.SrcQualified
	}
	return e.SrcName
}
