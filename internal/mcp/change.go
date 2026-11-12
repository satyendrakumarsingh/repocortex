package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// analyze_change answers the reviewer's question: what does this RANGE of
// commits put at risk?
//
// analyze_impact starts from one symbol the caller names. A pull request does
// not arrive as a symbol; it arrives as a branch. Until this tool an agent
// reviewing a change had to work out which symbols a diff touched and ask
// about each, which is exactly the bookkeeping the index exists to do. The
// capability already existed behind `repocortex impact --base --head`; this is the
// same function reached from the tool surface, so the CLI, MCP, the REST
// endpoint and the portal give one answer.

type changeArgs struct {
	Base string `json:"base" jsonschema:"The revision the change starts from: a branch, tag or commit, e.g. 'main' or 'origin/main'."`
	Head string `json:"head,omitempty" jsonschema:"The revision the change ends at. Default 'HEAD'."`

	MinConfidence string `json:"min_confidence,omitempty" jsonschema:"Lowest confidence to follow: exact, high, medium or low. Default medium. Use 'high' when the result will gate a merge."`
}

// registerChange adds analyze_change. It needs the graph and a git repository;
// without either it does not register, and the server says nothing about a
// tool it cannot run.
func (s *Server) registerChange(allowed map[string]bool) {
	if s.deps.Graph == nil {
		return
	}
	addTool(s, allowed, &sdk.Tool{
		Name:        "analyze_change",
		Description: toolDescriptions()["analyze_change"],
	}, s.analyzeChange)
}

func (s *Server) analyzeChange(ctx context.Context, req *sdk.CallToolRequest, args changeArgs) (*sdk.CallToolResult, *Response, error) {
	base := strings.TrimSpace(args.Base)
	if base == "" {
		return failWith("invalid_argument",
			"Pass 'base' as the branch or commit the change starts from, e.g. base='main'.",
			"base must not be empty")
	}
	head := strings.TrimSpace(args.Head)
	if head == "" {
		head = "HEAD"
	}
	minConf, err := parseImpactFloor(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}
	if s.headRepo == nil || !s.headRepo.IsRepo() {
		return s.guide(ctx, req, runID, status,
			"This project is not a git repository, so there is no commit range to analyse.",
			"analyze_change compares two git revisions. To ask about one symbol instead, call analyze_impact.")
	}

	rep, err := impact.AnalyzeDiff(ctx, s.capDeps, runID, impact.DiffRequest{
		Changes:   s.headRepo,
		BaseRev:   base,
		HeadRev:   head,
		Runs:      s.deps.Registry,
		Revisions: s.headRepo,
		IndexRoot: resolvedRoot(s.deps.Config.RepoRoot),
		Options:   impact.Options{MinConfidence: minConf},
	})
	if err != nil {
		// An unknown revision is the caller's to fix and is the common case:
		// say which one, as guidance rather than as a malfunction.
		if strings.Contains(err.Error(), "resolve") || strings.Contains(err.Error(), "revision") ||
			strings.Contains(err.Error(), "reference not found") {
			return s.guide(ctx, req, runID, status,
				fmt.Sprintf("The range %s..%s could not be read: %v.", base, head, err),
				"Check that both revisions exist in this repository. A remote branch is named origin/<branch>.")
		}
		return failFrom(err, "analyse %s..%s: %v", base, head, err)
	}

	tests := 0
	for _, im := range rep.Impacted {
		if im.IsTest {
			tests++
		}
	}
	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%s..%s changes %d symbol(s) in %d file(s) and can reach %d more, %d of them tests. Risk %s (score %d).",
			base, head, rep.SeedsFound, len(rep.ChangedFiles), rep.TotalReached(), tests, rep.Risk.Level, rep.Risk.Score),
	}

	// Changed symbols first: they are what the reviewer reads the diff for.
	// Among them, the ones other code depends on lead. A changed configuration
	// key or constant that nothing reaches is still listed, after them: a
	// review that opens with seven lines of .env keys buries the function
	// with thirty callers underneath.
	seeds := append([]impact.Seed(nil), rep.Seeds...)
	sort.SliceStable(seeds, func(i, j int) bool {
		ri := len(seeds[i].Report.Direct) + len(seeds[i].Report.Indirect)
		rj := len(seeds[j].Report.Direct) + len(seeds[j].Report.Indirect)
		return ri > rj
	})
	for _, seed := range seeds {
		resp.Items = append(resp.Items, Item{
			Kind: "changed", Name: seed.Name, FQN: seed.FQN, Path: seed.FilePath,
			Why: fmt.Sprintf("%s in this range; %d direct, %d further", seed.ChangeKind,
				len(seed.Report.Direct), len(seed.Report.Indirect)),
		})
	}
	// Then what they reach, tests first, because "what do I run" is the next
	// question and the answer is otherwise buried under ordinary callers.
	for pass := 0; pass < 2; pass++ {
		for _, im := range rep.Impacted {
			if im.IsTest != (pass == 0) {
				continue
			}
			kind := "reached"
			if im.IsTest {
				kind = "test"
			}
			resp.Items = append(resp.Items, Item{
				Kind: kind, Name: im.Name, FQN: im.FQN, Path: im.FilePath,
				Confidence: string(im.Confidence),
				Why:        fmt.Sprintf("%d hop(s) from a changed symbol", im.Depth),
			})
		}
	}

	if gate := coverageOf(rep.Completeness.Resolution, "").caveat("change analysis"); gate != nil {
		resp.Notes = append(resp.Notes, gate...)
	}
	if !rep.RevisionMatchesRange && rep.IndexedRevision != "" {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"The index was built at %.12s, which is not the head of this range. Symbols added or moved "+
				"since then are not in it. Run 'repocortex index' on the head revision for an exact answer.",
			rep.IndexedRevision))
	}
	if n := len(rep.UnindexedFiles); n > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d changed file(s) are not in the index (deleted, generated, or in a language it does not parse), "+
				"so nothing is known about what they reach: %s.", n, joinSome(rep.UnindexedFiles, 4)))
	}
	for _, u := range rep.Unknown {
		resp.Notes = append(resp.Notes, fmt.Sprintf("NOT VISIBLE (%s): %s", u.Kind, u.Detail))
	}
	if rep.Truncated {
		resp.markTruncated(rep.TruncatedReason)
	}
	return s.reply(ctx, req, resp)
}

// resolvedRoot is the project root with symlinks resolved, the form the git
// layer reports its own root in. See canonicalRoot in internal/cli for why the
// two must agree.
func resolvedRoot(root string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		return real
	}
	return root
}

// joinSome joins up to n items and says how many were left out.
func joinSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:n], ", "), len(items)-n)
}
