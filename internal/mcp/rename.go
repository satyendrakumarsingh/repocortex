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
// needs to finish building its own response (set Summary, Verdict, reply).
// relationshipQuery consumes Target, RunID and Status; planRename discards
// them — a struct lets it do that by construction instead of a run of blank
// identifiers at the call site.
type refLookup struct {
	Items  []Item
	Resp   *Response
	Target registry.SymbolHit
	RunID  int64
	Status string
}

// referencesFor resolves a symbol by name and fetches everything that points
// at it, filtered to minConf and, if kinds is non-nil, restricted to those
// edge kinds. It is the ONE reference-lookup implementation shared by
// relationshipQuery (find_references / find_callers) and planRename — neither
// duplicates the registry/graph calls the other makes.
//
// The kind predicate is pushed into the registry query (EdgeFilter.Kinds)
// rather than applied to the results afterward, for the same reason
// relationshipQuery originally did: s.limits.MaxItems is a LIMIT on the SQL
// query, and filtering in Go after that LIMIT would let a flood of
// same-or-higher-confidence edges of an excluded kind crowd a wanted edge out
// of the capped fetch before a Go-side filter ever saw it.
//
// When the run or the symbol cannot be resolved, or the lookup itself fails,
// shortCircuit is the fully-formed result the caller should return
// immediately, unchanged, as its own (result, response, error) — the crafted
// result IS the answer, including for a missing index, where it carries the
// "Run 'cortex index'" guidance rather than a bare protocol error. That is
// why referencesFor never returns an error itself: the go-sdk's
// ToolHandlerFor wrapper discards a handler's result whenever the handler
// also returns a non-nil error, substituting its own bare errRes.SetError(err)
// — which would silently strip this framing on every call site. shortCircuit
// is nil only on the success path, where the caller still has work to do
// (set Summary, Verdict, and reply) before handing the response back.
func (s *Server) referencesFor(
	ctx context.Context,
	req *sdk.CallToolRequest,
	name, path string,
	minConf model.Confidence,
	kinds []string,
) (refLookup, *sdk.CallToolResult) {
	runID, status, rerr := s.currentRun(ctx)
	if rerr != nil {
		sc, r, _ := s.noRun(ctx, req, rerr)
		return refLookup{Resp: r, RunID: runID, Status: status}, sc
	}

	syms, serr := s.deps.Registry.FindSymbolsByName(ctx, runID, name, "", 5)
	if serr != nil {
		sc, r, _ := failFrom(serr, "symbol lookup failed: %v", serr)
		return refLookup{Resp: r, RunID: runID, Status: status}, sc
	}
	if len(syms) == 0 {
		sc, r, _ := s.missingSymbol(ctx, req, runID, status, name)
		return refLookup{Resp: r, RunID: runID, Status: status}, sc
	}

	target := syms[0]
	for _, sym := range syms {
		if path != "" && strings.Contains(sym.FilePath, path) {
			target = sym
			break
		}
	}

	filter := registry.EdgeFilter{MinConfidence: minConf, Kinds: kinds}
	incoming, ierr := s.deps.Registry.IncomingEdges(ctx, runID, target.ID, s.limits.MaxItems, filter)
	if ierr != nil {
		sc, r, _ := failFrom(ierr, "relationship lookup failed: %v", ierr)
		return refLookup{Resp: r, Target: target, RunID: runID, Status: status}, sc
	}

	resp := &Response{IndexRevision: runID, IndexStatus: status}
	for _, e := range incoming {
		resp.Items = append(resp.Items, Item{
			Kind:       strings.ToLower(e.Kind),
			Name:       e.SrcName,
			Path:       e.EvidenceFile,
			Line:       e.EvidenceLine,
			Confidence: e.Confidence,
			Source:     e.Source,
			Why:        e.Detail,
		})
	}

	// min_confidence is a floor, not a preference: results below it are
	// omitted, and the omission is reported here so an empty or short result
	// is never mistaken for "nothing further exists". The sentence itself is
	// floorNote's, shared with every other tool that takes a floor.
	//
	// dropped has to be an exact COUNT(*), not len(incoming) short of the
	// floor: incoming is already capped at s.limits.MaxItems, so subtracting
	// its length from an unfiltered total would blame the floor for rows the
	// LIMIT excluded on its own. Two indexed counts — matching the kind
	// filter with and without the confidence floor — isolate what the floor
	// alone removed, the same pattern find_callers already uses for notCalls
	// below.
	var dropped int
	if minConf != "" && minConf != model.ConfidenceLow {
		total, terr := s.deps.Registry.IncomingEdgeCount(ctx, runID, target.ID,
			registry.EdgeFilter{Kinds: kinds}, nil)
		passing, perr := s.deps.Registry.IncomingEdgeCount(ctx, runID, target.ID, filter, nil)
		if terr == nil && perr == nil {
			dropped = total - passing
		}
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	// The registry LIMIT is spent entirely on rows that pass both filters, so
	// hitting it exactly means real results may still have been left out.
	if len(incoming) == s.limits.MaxItems {
		resp.markTruncated(fmt.Sprintf(
			"the item cap (%d) was reached after filtering; more results may exist beyond it — "+
				"narrow the query (a path, or a stricter min_confidence) for a complete answer",
			s.limits.MaxItems))
	}

	rs, rserr := s.deps.Registry.ResolutionStats(ctx, runID)

	// THE GATE COMES FIRST, AHEAD OF EVERY OTHER NOTE. See relationshipQuery's
	// original comment: a verdict on whether the answer can be trusted at all
	// outranks the detail notes below it.
	if rserr == nil {
		if gate := coverageOf(rs, target.FilePath).caveat("relationship lookup"); gate != nil {
			resp.Notes = append(gate, resp.Notes...)
		}
	}

	if rserr == nil && rs.Total > 0 {
		if len(resp.Items) == 0 {
			resp.Notes = append(resp.Notes, fmt.Sprintf(
				"No results found — but this is a LOWER BOUND, not proof there are none. "+
					"%d of %d references in this repository are unresolved or ambiguous, "+
					"so a real reference may exist that RepoCortex could not bind. Do not treat this "+
					"as evidence the symbol is unused.",
				rs.Unresolved+rs.Ambiguous, rs.Total))
		} else if rs.Unresolved+rs.Ambiguous > 0 {
			resp.Notes = append(resp.Notes, fmt.Sprintf(
				"%d references repository-wide are unresolved or ambiguous, so this list may be incomplete.",
				rs.Unresolved+rs.Ambiguous))
		}
	}

	return refLookup{Items: resp.Items, Resp: resp, Target: target, RunID: runID, Status: status}, nil
}

type renameArgs struct {
	Name    string `json:"name" jsonschema:"Symbol to rename."`
	NewName string `json:"new_name" jsonschema:"The replacement identifier. Used only to render the plan; nothing is written."`
	Path    string `json:"path,omitempty" jsonschema:"Optional file path to disambiguate when several symbols share a name."`

	// A floor here narrows the REVIEW half of the plan. The apply half is
	// already 'exact' by construction, so min_confidence=exact returns the
	// proven edits alone — the plan an agent can act on without reading a
	// single line by hand.
	minConfidenceArg
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
	lookup, shortCircuit := s.referencesFor(ctx, req, args.Name, args.Path, minConf, nil)
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
		"Rename %q to %q: %d proven site(s) to change, %d to review by hand. Each entry is a file and a line; locate the identifier on it. The plan is not complete.",
		args.Name, args.NewName, len(plan.Apply), len(plan.Review))
	// Verdict is NOT set here. Server.reply grades every response once, after
	// bounding — see Response.Verdict.
	resp.Notes = append(resp.Notes, plan.Warning)
	resp.RenamePlan = &plan
	return s.reply(ctx, req, resp)
}
