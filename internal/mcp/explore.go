package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type exploreArgs struct {
	Name string `json:"name" jsonschema:"Symbol name to explore."`
	Path string `json:"path,omitempty" jsonschema:"File path to disambiguate symbol."`
	Line int    `json:"line,omitempty" jsonschema:"1-based line number for precise symbol selection."`
	minConfidenceArg
	Limit int `json:"limit,omitempty" jsonschema:"Maximum callers/callees to return (default 10)."`
}

func (s *Server) registerExplore(allowed map[string]bool) {
	descs := toolDescriptions()
	desc, ok := descs["explore_symbol"]
	if !ok {
		desc = "Deep-dive on a symbol in one call: returns its definition with verbatim line-numbered source excerpt, upstream callers (with confidence tiers: 'exact' vs 'heuristic'), downstream callees, blast-radius impact summary, and linked HTTP routes. An empty callers result is ambiguous — confirm with search_code before assuming a symbol is dead."
	}
	addTool(s, allowed, &sdk.Tool{
		Name:        "explore_symbol",
		Description: desc,
	}, s.exploreTool)
}

func (s *Server) exploreTool(ctx context.Context, req *sdk.CallToolRequest, args exploreArgs) (*sdk.CallToolResult, *Response, error) {
	return s.exploreSymbol(ctx, req, args)
}

func (s *Server) exploreSymbol(ctx context.Context, req *sdk.CallToolRequest, args exploreArgs) (*sdk.CallToolResult, *Response, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name to explore.",
			"name must not be empty")
	}

	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	q := symbolQuery{
		Tool:       "explore_symbol",
		Param:      "name",
		PathParam:  "path",
		LineParam:  "line",
		Name:       name,
		Path:       args.Path,
		Line:       args.Line,
		Prefer:     callableKinds,
		PreferWord: "functions and methods",
		PreferNoun: "function or method",
		Tolerate:   typeKinds,
	}

	sel, sc, scResp := s.selectSymbol(ctx, req, q)
	if sc != nil {
		return sc, scResp, nil
	}
	runID, status, target := sel.RunID, sel.Status, sel.Target

	// 1. Definition excerpt.
	excerpt := s.symbolExcerpt(ctx, runID, target.SymbolHit)

	// 2. Upstream callers.
	callerFilter := registry.EdgeFilter{MinConfidence: minConf, Kinds: []string{string(model.EdgeCalls)}}
	callerEdges, callerTotal, _ := s.deps.Registry.EdgePage(ctx, runID, target.ID, true, limit, 0, callerFilter)

	// 3. Downstream callees.
	calleeFilter := registry.EdgeFilter{MinConfidence: minConf, Kinds: []string{string(model.EdgeCalls)}}
	calleeEdges, calleeTotal, _ := s.deps.Registry.EdgePage(ctx, runID, target.ID, false, limit, 0, calleeFilter)

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("Explored %s (%s) at %s:%d: %d caller(s), %d callee(s) recorded.",
			target.Name, target.Kind, target.FilePath, target.StartLine, callerTotal, calleeTotal),
	}

	// Attach target definition item first.
	resp.Items = append(resp.Items, Item{
		Kind:       target.Kind,
		Name:       target.Name,
		FQN:        target.FQN,
		Path:       target.FilePath,
		Line:       target.StartLine,
		Confidence: target.Confidence,
		Source:     target.Source,
		Why:        "definition",
		Excerpt:    excerpt,
	})

	// Upstream callers.
	for _, e := range callerEdges {
		resp.Items = append(resp.Items, Item{
			Kind:       "caller",
			Name:       sourceName(e),
			Path:       e.EvidenceFile,
			Line:       e.EvidenceLine,
			Confidence: e.Confidence,
			Source:     e.Source,
			Why:        fmt.Sprintf("calls %s (%s)", target.Name, e.Confidence),
		})
	}

	// Downstream callees.
	for _, e := range calleeEdges {
		resp.Items = append(resp.Items, Item{
			Kind:       "callee",
			Name:       e.DstName,
			Path:       e.EvidenceFile,
			Line:       e.EvidenceLine,
			Confidence: e.Confidence,
			Source:     e.Source,
			Why:        fmt.Sprintf("called by %s (%s)", target.Name, e.Confidence),
		})
	}

	// 4. Impact / blast-radius summary if graph is present.
	if s.deps.Graph != nil {
		rep, err := impact.AnalyzeTarget(ctx, s.capDeps, runID, capability.TargetFromCandidate(target.Name, target),
			impact.Options{MinConfidence: minConf, HubDegreeLimit: 50, CheckValueUse: false})
		if err == nil {
			_, reach := impactItems(rep)
			resp.Items = append(resp.Items, Item{
				Kind: "impact",
				Name: reach.summary(),
				Path: target.FilePath,
				Line: target.StartLine,
				Why:  "transitive impact",
			})
		}
	}

	// 5. Associated HTTP route if applicable.
	if endpoints, err := s.deps.Registry.EndpointReport(ctx, runID); err == nil {
		for _, ep := range endpoints {
			if ep.FilePath == target.FilePath && (ep.Handler == target.Name || ep.HandlerID == target.ID || (ep.Line >= target.StartLine && ep.Line <= target.EndLine)) {
				resp.Items = append(resp.Items, Item{
					Kind: "endpoint",
					Name: ep.Signature,
					Path: ep.FilePath,
					Line: ep.Line,
					Why:  fmt.Sprintf("HTTP endpoint handled by %s", target.Name),
				})
				break
			}
		}
	}

	// Apply coverage gate.
	s.gate(ctx, resp, sel, "explore", aboutCallers)

	if callerTotal == 0 {
		resp.Notes = append(resp.Notes,
			"No callers found in index. Note that an empty caller result is ambiguous: unindexed callers, dynamic dispatch, or reflection may exist.")
	}
	if note := s.markedNote(ctx, runID, target.SymbolHit); note != "" {
		resp.Notes = append(resp.Notes, note)
	}

	return s.reply(ctx, req, resp)
}
