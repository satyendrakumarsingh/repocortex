package mcp

import (
	stdctx "context"
	"fmt"
	"strings"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// taskContextArgs is the highest-value tool in the product, so its schema text
// gets the most care: this description is what decides whether an agent calls
// it FIRST or falls back to grepping the repository.
type taskContextArgs struct {
	Task string `json:"task" jsonschema:"Describe the change you are about to make, in plain English, as you would to a colleague. For example: 'Add idempotency to payment callback processing'. Full sentences work better than keywords — the task is parsed for intent, not matched literally."`

	MaxFiles int `json:"max_files,omitempty" jsonschema:"Cap on returned items (default 20). Raise only if the default answer is genuinely too narrow."`

	// The floor applies to the PACK, not to the traversal. internal/context
	// deliberately walks every edge and discounts weak ones during ranking
	// (see internal/context/expand.go) so a weakly-supported path can still
	// surface and say so; raising a floor inside the walk would silently
	// shrink the reachable set instead. What a caller asking for compiler-grade
	// context wants is the items it can defend, which is a question about the
	// answer, not about the search.
	minConfidenceArg

	// Unlike the floor above, this one DOES apply to the traversal — it is the
	// only knob here that can. The expansion follows every edge by design, so
	// the thing that makes a pack enormous is not a weak edge, it is one wide
	// node whose neighbourhood is the whole repository.
	hubDegreeLimitArg
}

// registerTaskContext adds get_task_context when the context engine is wired.
//
// It is registered separately from the other tools because it needs the graph,
// which the other tools do not — keeping that dependency out of the base
// registration means the server still starts and answers symbol queries on an
// index that has never been resolved.
func (s *Server) registerTaskContext(allowed map[string]bool) {
	if s.deps.Graph == nil {
		return
	}
	addTool(s, allowed, &sdk.Tool{
		Name:        "get_task_context",
		Description: toolDescriptions()["get_task_context"],
	}, s.getTaskContext)
}

func (s *Server) getTaskContext(ctx stdctx.Context, call *sdk.CallToolRequest, args taskContextArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Task) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty task describing the change you intend to make, then call get_task_context again.",
			"task must not be empty — describe the change you intend to make")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if err := parseHubDegreeLimit(args.HubDegreeLimit); err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, call, err)
	}

	// CLAMPED, not merely defaulted. max_files arrives from the agent, and it
	// bounds the excerpt-read loop inside Assemble — an unclamped 1,000,000
	// would read a million file excerpts of up to 20,000 lines each into
	// memory. The response bounds further down trim the OUTPUT; they do not
	// stop the work.
	//
	// search_code three files away already clamps its limit against exactly
	// this ceiling. Two answers to one threat in one server was the bug.
	req := devctx.Request{Task: args.Task}
	if args.MaxFiles > 0 {
		req.Budget.MaxFiles = min(args.MaxFiles, s.limits.MaxItems)
	}
	// Not clamped, unlike MaxFiles: this value can only make the walk SMALLER,
	// so a pathological figure costs nothing to honour. Budget.withDefaults
	// leaves an unset value at zero, which is "no pruning".
	req.Budget.HubDegreeLimit = args.HubDegreeLimit

	pack, err := s.assembler.Assemble(ctx, s.deps.Registry, s.deps.Graph, runID, req)
	if err != nil {
		return failFrom(err, "assemble context: %v", err)
	}

	// Filtered here rather than inside Assemble, and the token estimate is
	// recomputed from what survives — a pack that reports the unfiltered cost
	// of an answer it did not return would misprice every budget decision the
	// agent makes next.
	dropped := 0
	if minConf != model.ConfidenceLow {
		kept := make([]devctx.Item, 0, len(pack.Items))
		for _, it := range pack.Items {
			// An item with no tier ranks 0 and is dropped by any explicit
			// floor. That is the honest reading: the caller asked for what
			// RepoCortex can defend to a stated tier, and an ungraded item is
			// not that.
			if model.Confidence(it.Confidence).AtLeast(minConf) {
				kept = append(kept, it)
			}
		}
		dropped = len(pack.Items) - len(kept)
		pack.Items = kept
		pack.TokenEstimate = pack.EstimateTokens()
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%d item(s) for %q (~%d tokens).",
			len(pack.Items), pack.Task, pack.TokenEstimate),
		// The summary above quotes a bytes/4 token estimate, not a measured
		// count — see internal/mcp/basis.go.
		Basis: BasisTokenCount,
	}

	// The interpretation goes first. Almost every bad pack comes from a
	// misread task rather than bad ranking, so showing what was understood
	// lets an agent correct course in one turn instead of concluding the
	// index is wrong.
	if pack.Interpretation != "" {
		resp.Notes = append(resp.Notes, "Understood as: "+pack.Interpretation)
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	for _, it := range pack.Items {
		resp.Items = append(resp.Items, Item{
			Kind:       it.Kind,
			Name:       it.Name,
			FQN:        it.FQN,
			Path:       it.Path,
			Line:       it.Line,
			Confidence: it.Confidence,
			Why:        it.Why,
			Excerpt:    it.Excerpt,
		})
	}

	// Exclusions are a trust feature, not noise. An agent that can see WHY a
	// plausible-looking file was left out can judge whether the omission was
	// right, instead of assuming the tool simply missed it.
	for _, ex := range pack.Excluded {
		resp.Notes = append(resp.Notes,
			fmt.Sprintf("Excluded %s: %s", ex.Name, ex.Reason))
	}
	resp.Notes = append(resp.Notes, pack.Notes...)

	if len(pack.Items) == 0 {
		resp.Notes = append(resp.Notes,
			"Nothing matched. Either the task names code that is not in this repository, "+
				"or references have not been resolved yet — run 'cortex resolve'.")
	}
	return s.reply(ctx, call, resp)
}
