package mcp

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// ---------------------------------------------------------------- scopes
//
// A SCOPE IS A SERVER WITH FEWER TOOLS, NOT A FILTER IN FRONT OF ONE.
//
// A shared server gives each token a scope: a profile, optionally narrowed to
// an allow-list of tool names. The obvious way to enforce that is one server
// with every tool and a check in front of it. That design has to get three
// things right for ever: hide the tool from tools/list, refuse it in
// tools/call with exactly the error an unknown tool gets, and keep it out of
// everything derived from the list (the instructions, the prompts, the REST
// describe route, the OpenAPI document, a client's cache of the list). Each is
// a place a tool's existence can leak, and the next derived surface somebody
// adds is one more.
//
// So a scope is built the way a profile always was: the tools outside it are
// never registered. A call to one is answered by the SDK's own unknown-tool
// path because, on that server, it IS an unknown tool. Nothing imitates
// anything, and there is no filter to forget.
//
// What the scoped servers share is the index and its caches, so the cost of a
// second scope is a tool table and not a second copy of the symbol index.

// toolFilter is the set of tools a profile exposes, narrowed by allow. Nil
// means no filter. A non-nil result is exact, and may be empty.
func toolFilter(p config.MCPProfile, allow []string) map[string]bool {
	base := profileTools(p)
	if len(allow) == 0 {
		return base
	}
	out := make(map[string]bool, len(allow))
	for _, name := range allow {
		// Intersection: the allow-list removes tools from a profile and can
		// never reach past it.
		if base == nil || base[name] {
			out[name] = true
		}
	}
	return out
}

// Scoped returns a server over the same index that exposes the given profile,
// narrowed to tools when the list is not empty. It shares this server's audit
// log and caches. The profile of the receiver does not limit the result.
//
// Every name in tools must be a tool the profile exposes on this index. A
// misspelt name would otherwise narrow a scope silently, and the operator
// would find out when an agent could not do its job.
func (s *Server) Scoped(profile config.MCPProfile, tools []string) (*Server, error) {
	if !profile.Valid() {
		return nil, fmt.Errorf("mcp: profile %q: want minimal, standard, full or compact (alias frontdoor)", profile)
	}
	deps := s.deps
	deps.Config.MCPProfile = profile
	deps.AllowTools = append([]string(nil), tools...)

	scoped, err := newServer(deps, s)
	if err != nil {
		return nil, err
	}
	for _, name := range tools {
		if !scoped.hasTool(name) {
			return nil, fmt.Errorf("mcp: no tool named %q on the %q profile", name, profile)
		}
	}
	return scoped, nil
}

// ToolsOf lists the tools a profile names, in order. It reads the profile
// tables and needs no index, so a command can check a tool name before any
// server exists. A server registers a tool only when its dependencies are
// present too; Scoped is the check against a real one.
func ToolsOf(p config.MCPProfile) []string {
	minimal, standard := profileLists()
	switch p {
	case config.MCPCompact, config.MCPFrontdoor:
		return []string{"repocortex"}
	case config.MCPStandard:
		return standard
	case config.MCPFull:
		return append(standard, fullOnlyTools()...)
	default:
		return minimal
	}
}
