package mcp

import "strings"

// ---------------------------------------------------------------- tool names
//
// A TOOL NAME IS READ BY A MODEL ON EVERY REQUEST, BEFORE ITS DESCRIPTION.
//
// The name is the cheapest steer the server has: a model picks between
// `find_callers` and `analyze_impact` from the names alone more often than from
// the paragraphs under them. So the names follow one rule, and a test
// (TestEveryToolNameFollowsTheConvention) enforces it:
//
//	<verb>_<noun phrase>, lower snake case, verb from toolVerbs.
//
// The verbs are the ones the wider tool ecosystem already uses, with the
// meanings it already gives them, so a model that has seen any other server
// arrives knowing what each returns. `get`, `list` and `search` are the
// vocabulary of GitHub's and most vendors' MCP servers; `find` is the language
// servers' word for a relationship lookup (find references, find
// implementations).
//
//	get_      one known thing, by name or path. A lookup.
//	list_     a collection of things of one kind, optionally narrowed by a
//	          filter. Takes no target: list_hotspots, list_api_endpoints.
//	find_     the things related to ONE named symbol. May be empty, and empty
//	          is never proof of absence: find_callers, find_tests.
//	search_   ranked text matches. No relationship is claimed.
//	trace_    an ordered route through the graph from one thing to another.
//	analyze_  a computed judgement (risk, reach) rather than a list of facts.
//	plan_     an ordered set of edits. Never writes; the caller applies it.
//	minimize_ a transformation of text the caller supplies.
//
// THERE ARE NO ALIASES. A tool has one name. Before a first release nobody
// depends on an old one, and a second name for a tool is a second thing for a
// model to choose between and a second schema's worth of documentation to keep
// true. After a release, a rename is a breaking change and is made as one.

// toolVerbs is the closed list of leading verbs. Adding a verb is a design
// decision about a new kind of answer, so it happens here, with a line in the
// comment above, not in passing at a registration site.
var toolVerbs = []string{"get", "list", "find", "search", "trace", "analyze", "plan", "minimize"}

// ToolVerbs returns the permitted leading verbs, for the naming test and for
// documentation that must not restate the list by hand.
func ToolVerbs() []string { return append([]string(nil), toolVerbs...) }

// ValidToolName reports whether name follows the convention above.
func ValidToolName(name string) bool {
	if name == "" || name != strings.ToLower(name) {
		return false
	}
	verb, noun, ok := strings.Cut(name, "_")
	if !ok || noun == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	if strings.Contains(name, "__") || strings.HasSuffix(name, "_") {
		return false
	}
	for _, v := range toolVerbs {
		if verb == v {
			return true
		}
	}
	return false
}

// methodCallTool is the JSON-RPC method the audit middleware acts on.
const methodCallTool = "tools/call"
