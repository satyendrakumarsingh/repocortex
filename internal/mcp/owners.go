package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/ownership"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// find_owners answers "who should look at this change": the people who have
// recently worked on the code a change to a symbol can reach.
//
// It was `repocortex owners` only. An agent preparing a pull request had no way to
// suggest a reviewer from evidence, and guessed from file headers or did not
// suggest one.
//
// NAMES, NEVER ADDRESSES. The capability reads author names and emails from the
// local git history, and the terminal command prints both to the person who
// owns that history. A tool answer goes into a model's context and, over the
// REST endpoint, to whatever is calling it. An email address is personal data
// with no use to an agent choosing a reviewer, so it is not put in the answer:
// no field of an Item carries one.

type ownersArgs struct {
	Symbol string `json:"symbol" jsonschema:"The symbol you are changing, e.g. 'updateStatus' or 'TransactionService'."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Most people to list. Default 5."`
}

const defaultOwners = 5

func (s *Server) registerOwners(allowed map[string]bool) {
	if s.deps.Graph == nil {
		return
	}
	addTool(s, allowed, &sdk.Tool{
		Name:        "find_owners",
		Description: toolDescriptions()["find_owners"],
	}, s.findOwners)
}

func (s *Server) findOwners(ctx context.Context, req *sdk.CallToolRequest, args ownersArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Symbol) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty symbol name, then call find_owners again.",
			"symbol must not be empty")
	}
	if args.Limit < 0 {
		return failWith("invalid_argument", "Pass limit as a positive number, or omit it.",
			"limit must not be negative")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}
	if s.deps.Owners == nil {
		return s.guide(ctx, req, runID, status,
			"No commit history is available to this server, so nobody can be named.",
			"This reads git history. The project is not a git repository, or its history could not be opened.")
	}

	blast, err := impact.AnalyzeWith(ctx, s.capDeps, runID, args.Symbol, impact.Options{})
	if err != nil {
		if errors.Is(err, capability.ErrTargetNotFound) {
			return s.missingSymbol(ctx, req, runID, status, args.Symbol)
		}
		return failFrom(err, "blast radius of %s: %v", args.Symbol, err)
	}

	lim := ownership.DefaultLimits()
	lim.MaxPeople = defaultOwners
	if args.Limit > 0 {
		lim.MaxPeople = min(args.Limit, s.limits.MaxItems)
	}
	an, err := ownership.New(s.deps.Owners, ownership.WithLimits(lim))
	if err != nil {
		return failFrom(err, "find owners: %v", err)
	}
	rep, err := an.Analyze(ctx, blast)
	if err != nil {
		return failFrom(err, "find owners: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%d person(s) have recently changed code that a change to %s reaches (%d symbol(s) in %d file(s)), from the last %d commit(s).",
			len(rep.People), args.Symbol, rep.Reach.Symbols, rep.Reach.Files, rep.Window.CommitsRead),
	}
	for _, p := range rep.People {
		why := fmt.Sprintf("changed %d of the reached symbol(s) (%.0f%%) across %d file(s), in %d commit(s)",
			p.Symbols, 100*p.Share, p.FilesTouched, p.CommitCount)
		if p.LastTouchUnix > 0 {
			why += ", most recently " + time.Unix(p.LastTouchUnix, 0).UTC().Format("2006-01-02")
		}
		// The first few files, so the agent can say what each person knows.
		path := ""
		if len(p.Files) > 0 {
			path = p.Files[0]
		}
		resp.Items = append(resp.Items, Item{Kind: "person", Name: p.Name, Path: path, Why: why})
	}

	resp.Notes = append(resp.Notes,
		"This is who has recently CHANGED the code, not who owns it or who must approve. "+
			"Someone who wrote it years ago and someone listed in CODEOWNERS may both be absent. "+
			"Email addresses are deliberately left out of this answer.")
	resp.Notes = append(resp.Notes, rep.Caveats...)
	if !rep.Window.Exhausted {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"Only the last %d commit(s) were read; earlier history exists and was not.", rep.Window.CommitsRead))
	}
	return s.reply(ctx, req, resp)
}
