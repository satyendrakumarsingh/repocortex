package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/diffmini"
	"github.com/satyendrakumarsingh/repocortex/internal/prune"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- coverage gate
//
// A RELATIONSHIP ANSWER IS ONLY AS GOOD AS THE RESOLUTION BEHIND IT.
//
// find_callers, find_references, analyze_impact and find_tests all answer the
// same underlying question — what connects to this symbol — and all of them
// read the same graph. When most of a repository's references were never bound
// to a definition, that graph is a minority report, and three callers rendered
// in a tidy list look exactly like three callers rendered from complete data.
// The response has to say which one it is BEFORE the list, not merely append a
// caveat after it.
//
// The philosophy and the mandatory wording live in internal/capability's
// Completeness, which every capability report already embeds. This gate is the
// MCP-side trigger for that same mechanism, not a second one: the caveat text
// it emits is capability.LowerBoundCaveat verbatim, so the two cannot drift.

// The gate itself is in signal.go: coverage, Reliability and OverallSignal.

// Tool argument types.
//
// The SDK infers each tool's JSON Schema from these structs, and the
// `jsonschema` tags become the descriptions an agent reads when deciding which
// tool to call. They are therefore part of the product surface, not comments:
// a vague description produces a wrong tool choice, which costs a whole round
// trip.

type emptyArgs struct{}

type fileOutlineArgs struct {
	Path string `json:"path" jsonschema:"File path to outline (relative to repo root or absolute). Prunes implementation bodies to reduce token size while preserving all types, structs, interfaces, signatures, and docstrings."`
}

type minimizeDiffArgs struct {
	Diff string `json:"diff" jsonschema:"Unified diff text (e.g. git diff output) to compress into a structured, token-efficient representation."`
}

type symbolArgs struct {
	Name string `json:"name" jsonschema:"Symbol name to find, e.g. 'TransactionService' or 'updateStatus'. Exact match on the name, not a substring."`
	Kind string `json:"kind,omitempty" jsonschema:"Optional filter: class, interface, function, method, field, constant, type_alias."`

	minConfidenceArg
}

// symbolRefArgs selects one definition: by name, and by path and line when the
// name is shared.
type symbolRefArgs struct {
	Name string `json:"name" jsonschema:"Symbol name, e.g. 'updateStatus' or 'TransactionService'."`
	Path string `json:"path,omitempty" jsonschema:"File or directory of the definition, to choose between symbols that share the name."`
	lineArg

	minConfidenceArg
}

// relationArgs is symbolRefArgs for a tool that lists the relationships of the
// selected definition, and so can be paged.
type relationArgs struct {
	Name string `json:"name" jsonschema:"Symbol name whose relationships you want."`
	Path string `json:"path,omitempty" jsonschema:"File or directory of the definition, to choose between symbols that share the name."`
	lineArg

	minConfidenceArg
	pageArg
}

// siteArgs is relationArgs for the two tools whose rows are sites in the
// code (find_callers, find_references), which can be narrowed by where the
// site is.
type siteArgs struct {
	relationArgs
	Within string `json:"within,omitempty" jsonschema:"Keep only rows whose call site is in this file or under this directory (e.g. 'src/'). The first line and the verdict then describe those rows alone."`
}

// minConfidenceArg carries the confidence floor every confidence-bearing tool
// offers.
//
// EMBEDDED RATHER THAN REPEATED. The jsonschema tag is the sentence an agent
// reads before it decides what to pass, so it is product surface; Go struct
// tags cannot reference a constant, and a copy per tool would drift until two
// tools described the same parameter differently — which is worse than not
// offering it, because the agent cannot tell which description is true.
//
// The floor is a floor, not a preference: results below it are omitted and the
// omission is reported in the response notes (see floorNote). A filter that
// silently drops evidence would be worse than no filter.
//
// A tool whose items carry NO confidence tier must not embed this. Accepting a
// parameter that changes nothing tells the agent its answer was filtered when
// it was not. minConfidenceSurface in minconfidence_test.go records that
// decision per tool and fails the build when a new tool leaves it unmade.
type minConfidenceArg struct {
	MinConfidence string `json:"min_confidence,omitempty" jsonschema:"Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives."`
}

// hubDegreeLimitArg carries the hub bound offered by the traversal-backed
// tools.
//
// EMBEDDED RATHER THAN REPEATED, for the same reason minConfidenceArg is: the
// jsonschema tag is the sentence an agent reads before deciding what to pass,
// and a copy per tool would drift until two tools described the same parameter
// differently.
//
// A tool whose answer does NOT come from a graph traversal must not embed this.
// The limit refuses to expand THROUGH a wide node, so a tool that answers from
// a single registry query has no expansion to refuse — accepting the parameter
// there would tell an agent its blast radius was bounded when nothing was
// bounded at all. hubPruneSurface in hubprune_mcp_test.go records that decision
// per tool and fails the build when a new tool leaves it unmade.
type hubDegreeLimitArg struct {
	HubDegreeLimit int `json:"hub_degree_limit,omitempty" jsonschema:"Stop the walk from expanding THROUGH any symbol with more than this many edges. Default 0 (off), which is what every result you have seen so far used. Set it (50 is a reasonable start) when a result is enormous because the walk crossed a logger, a util or a base class; pruned symbols are still listed, only their neighbours are not explored, and the response says how many were cut."`
}

// parseHubDegreeLimit validates the wire value.
//
// Zero is not an error: it means the caller expressed no preference, which is
// exactly "no pruning" and is what every existing caller already gets. A
// NEGATIVE value IS an error rather than a silent zero — a caller who wrote -1
// has a bug, and quietly answering the unbounded question would return a wider
// blast radius than was asked for while looking like a success.
func parseHubDegreeLimit(n int) error {
	if n < 0 {
		return fmt.Errorf("hub_degree_limit %d is negative: pass a positive edge count, or omit it to disable pruning", n)
	}
	return nil
}

// parseMinConfidence maps the wire value onto a tier.
//
// An empty value is not an error: it means the caller expressed no preference,
// which is exactly ConfidenceLow, the floor that admits everything. An
// unrecognised value IS an error — silently treating "hgih" as "no threshold"
// would hand an agent low-confidence edges it explicitly tried to exclude.
func parseMinConfidence(raw string) (model.Confidence, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return model.ConfidenceLow, nil
	}
	c := model.Confidence(trimmed)
	if !c.Valid() {
		return "", fmt.Errorf("unknown confidence %q: use exact, high, medium or low", raw)
	}
	return c, nil
}

// parseImpactFloor decodes the floor for analyze_impact alone.
//
// Absent must stay the ZERO value here, not low. impact.AnalyzeWith applies
// impact.DefaultMinConfidence (medium) to an unset floor, so mapping "no
// preference" onto low would widen every existing caller's blast radius and
// weaken the risk score computed from it — precisely what adding an optional
// parameter must never do. The decoding itself is still parseMinConfidence's;
// only the empty case differs, because only the default differs.
func parseImpactFloor(raw string) (model.Confidence, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return parseMinConfidence(raw)
}

// floorNote is the one sentence a filtered response says about its own filter.
//
// Shared, because a short answer that does not admit it was filtered is read
// as "nothing further exists" — and two tools phrasing this separately would
// end with one of them not saying it at all.
//
// Gated on dropped, not merely on minConf being set. A floor that admits
// everything it was handed (dropped == 0) has filtered nothing, and "lower-
// confidence results exist but were omitted" is false in that case — provably
// so at find_symbol when the only match already meets the floor, and
// self-contradictory at get_task_context, which used to append "0 item(s)
// were dropped by this floor" to the very sentence claiming an omission.
// Saying nothing when dropped is 0 is the chosen behaviour over saying
// something true-but-vacuous ("the floor was applied and excluded nothing"):
// every other Response note here exists to change what the reader does next,
// and a floor that changed nothing has no action to prompt.
//
// dropped is a caller-supplied exact count, not a boolean, so the sentence
// can name how many were left out instead of merely asserting that some were.
func floorNote(minConf model.Confidence, dropped int) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"Filtered to confidence %q and above: %d lower-confidence result(s) were omitted; re-run with min_confidence=\"low\" to see them.",
		minConf, dropped)
}

// weakConfidenceNote is the warning advisory note attached when results include
// medium- or low-confidence edges/results, which have high false positive rates in empirical
// benchmarks (up to 94.3% on medium, up to 100% on low). It informs agents to pass min_confidence='high'.
func weakConfidenceNote(weakCount int) string {
	if weakCount <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"Note: %d result(s) are medium or low confidence (heuristic inference with higher error rates). Pass min_confidence='high' to restrict to compiler-verified and high-precision edges.",
		weakCount)
}

// countWeakConfidence returns the count of items whose confidence tier is medium or low.
func countWeakConfidence(items []Item) int {
	weak := 0
	for _, it := range items {
		if it.Confidence == string(model.ConfidenceMedium) || it.Confidence == string(model.ConfidenceLow) {
			weak++
		}
	}
	return weak
}

func isWeakConfidenceNote(n string) bool {
	return strings.Contains(n, "medium or low confidence (heuristic inference with higher error rates)")
}

// attachWeakConfidenceNote ensures the response carries an up-to-date advisory note
// if any medium- or low-confidence items are present in r.Items.
func (r *Response) attachWeakConfidenceNote() {
	if r == nil {
		return
	}
	var filtered []string
	for _, n := range r.Notes {
		if !isWeakConfidenceNote(n) {
			filtered = append(filtered, n)
		}
	}
	if note := weakConfidenceNote(countWeakConfidence(r.Items)); note != "" {
		filtered = append(filtered, note)
	}
	r.Notes = filtered
}

// atLeastConfidence keeps the definitions a caller can defend at minConf.
//
// A symbol whose recorded confidence is unrecognised or empty ranks 0 and is
// therefore dropped by ANY explicit floor. That is the honest reading: the
// caller asked for facts RepoCortex can defend to a stated tier, and an ungraded
// row is not one.
func atLeastConfidence(syms []registry.SymbolHit, minConf model.Confidence) ([]registry.SymbolHit, int) {
	if minConf == "" || minConf == model.ConfidenceLow {
		return syms, 0
	}
	kept := make([]registry.SymbolHit, 0, len(syms))
	for _, sym := range syms {
		if model.Confidence(sym.Confidence).AtLeast(minConf) {
			kept = append(kept, sym)
		}
	}
	return kept, len(syms) - len(kept)
}

// belowFloorOnly is the recoverable "the name exists, but not at this tier"
// answer.
//
// Distinct from missingSymbol on purpose: "no such symbol" and "no symbol you
// would accept" lead an agent to opposite next steps, and answering both with
// the first would send it looking for a spelling mistake that is not there.
func (s *Server) belowFloorOnly(ctx context.Context, req *sdk.CallToolRequest, runID int64, status, name string, minConf model.Confidence, dropped int) (*sdk.CallToolResult, *Response, error) {
	return s.guide(ctx, req, runID, status,
		fmt.Sprintf("No definition of %q at confidence %q or above.", name, minConf),
		fmt.Sprintf("%d definition(s) of this name are indexed below that floor. The symbol exists; "+
			"RepoCortex simply cannot defend it at the tier you asked for. Re-run with a lower "+
			"min_confidence to see them.", dropped))
}

// toolDescriptions is the single source for every registered tool's
// description.
//
// These are PROMPTS, not documentation. The model reads them before choosing a
// tool and will not read anything else. Each one states: when to prefer this
// tool over grep, what the result does and does not prove, and what to do
// next. The tests in descriptions_test.go assert those properties hold, and
// assert that every tool this package can register has an entry here — a
// tool with no entry ships with an empty description, which steers the model
// less than a bad one would.
func toolDescriptions() map[string]string {
	return map[string]string{
		"get_project_overview": "Call this FIRST, before any other RepoCortex tool. Reports what the " +
			"project is, how much of it is indexed, which languages it uses, and how much of the " +
			"code RepoCortex could resolve. The resolution figure is the important one: it tells you " +
			"whether relationship answers for this repo are trustworthy or mostly inferred. Do not " +
			"guess at coverage — this tool states it.",

		"search_code": "Full-text search across indexed source. Prefer this over your own grep: it " +
			"searches the index and returns the paths of the matching files, best match first, at a " +
			"fraction of the context. Use it when you know roughly what you are looking for but not " +
			"where it lives, and to confirm any relationship result that came back empty, incomplete " +
			"or low-confidence.",

		"find_symbol": "Locate a symbol's definition by name. Faster and more precise than searching " +
			"when you already know the identifier. Every definition of the name is listed with its " +
			"file and line; when there are several, pass one row's `path` and `line` to the other " +
			"tools to choose it. If this returns nothing, the symbol may still exist under a different " +
			"name or in an unindexed file — fall back to search_code rather than concluding it is absent.",

		"get_symbol": "Get a symbol's definition with a bounded source excerpt and its immediate " +
			"relationships. Call this after find_symbol instead of reading the whole file. The excerpt " +
			"is bounded and may be truncated; the response says so explicitly when it is, so check " +
			"that flag rather than assuming you have the full body.",

		"explore_symbol": "Deep-dive on a symbol in a single call. Returns its definition with a " +
			"bounded verbatim source excerpt, upstream callers (each carrying a confidence tier: " +
			"'exact' means a compiler proved the binding, 'high', 'medium', 'low' are inferred), " +
			"downstream callees, blast-radius impact reach, and linked HTTP routes. An empty callers " +
			"result is ambiguous and does not prove absence of callers; confirm with search_code.",

		"find_references": "Find where a symbol is used. Every result carries a confidence tier: " +
			"'exact' means a compiler proved the binding, 'high' and 'medium' and 'low' mean RepoCortex " +
			"inferred it and may be wrong. Pass min_confidence='high' when a wrong result costs more " +
			"than a missing one. CRITICAL: an EMPTY result is ambiguous — it may mean nothing " +
			"references this symbol, or that RepoCortex could not resolve the references that exist. " +
			"Never conclude a symbol is unused from an empty result; confirm with search_code.",

		"find_callers": "Find the functions and methods that call a symbol — the callers, not merely " +
			"the files mentioning it. Use before changing a signature or behaviour. Every result " +
			"carries a confidence tier, from 'exact' (a compiler proved the binding) down through " +
			"'high', 'medium' and 'low' (RepoCortex inferred it and may be wrong); pass " +
			"min_confidence='high' to see only bindings RepoCortex can defend. CRITICAL: an EMPTY " +
			"result is ambiguous and unresolved calls are reported separately, so absence of callers " +
			"here is NOT evidence the symbol is dead. Confirm with search_code before deleting " +
			"anything.",

		"plan_rename": "Plan a symbol rename. Returns the edit SITES — a file and a line each, with " +
			"no column — split into 'apply' (confidence 'exact': a compiler proved the binding) and " +
			"'review' (RepoCortex inferred the binding and may be wrong). You must locate the " +
			"identifier on each line yourself; a line may contain it more than once or as part of a " +
			"longer name. WRITES NOTHING. The plan is never " +
			"complete: references RepoCortex could not resolve are absent and are not reported as " +
			"absent, so after applying it, run search_code for the old name to find bindings that " +
			"were missed entirely.",

		"get_task_context": "CALL THIS FIRST when you are about to change code, before exploring the " +
			"repository by hand. Give it the task in plain English and it returns the minimum set of " +
			"files, symbols, tests and database tables the change actually touches, found by walking " +
			"the call graph rather than by keyword matching. Every included item states why it is " +
			"there, and notable near-misses are listed with the reason they were excluded. Using this " +
			"instead of grepping the repository is the point of this server; prefer it over " +
			"search_code and find_symbol when you are starting a change rather than looking up one " +
			"known symbol.",

		"analyze_impact": "Before changing a symbol, find out what depends on it. Separates DIRECT " +
			"callers (a proven call edge) from INDIRECT reach and from dispatch rows the runtime MAY " +
			"execute but no call site proves, and names what it CANNOT see — dynamic dispatch, " +
			"unresolved references, traversal limits. The risk level is 'unknown', never 'low', when " +
			"the callers could not be determined, and the first line says so. The result is a LOWER " +
			"BOUND: treat an empty or short result as 'nothing found', never as proof that changing " +
			"the symbol is safe.",

		"analyze_patch": "Dry-run a change before writing it: pass a unified diff (one or more files), or one " +
			"file's path and full new content. Lists the callers that removed or re-signed definitions " +
			"would break, and for Go type-checks the patched packages and their importers in memory and " +
			"names every line that would stop compiling. Nothing is written.",

		"find_tests": "Find the tests covering a symbol, and which signal found each one — a proven " +
			"call edge is far stronger evidence than a matching filename, and the response names which " +
			"you got. Use before changing behaviour, to know what to run. A symbol reported as " +
			"untested is a positive finding, not silence; when the calls to the symbol could not be " +
			"bound the answer says UNKNOWN instead, and means it.",

		"list_api_endpoints": "List the HTTP endpoints this service exposes, with the handler that " +
			"serves each one and where it is declared. Routes are recognised from the framework's own " +
			"syntax — a Spring annotation, a Go router registration, a FastAPI or Flask decorator, an " +
			"Express member call — never from a string that merely looks like a path, which is what " +
			"keeps a log message mentioning 'GET /users' out of the result. Pass method to filter to " +
			"one HTTP verb. NOT covered: OpenAPI documents, GraphQL schemas, gRPC service definitions " +
			"and API-gateway configuration — a service defined entirely in those reports no endpoints " +
			"here, which is a coverage limit, not a finding about the code. Prefer this over grepping " +
			"route decorators by hand.",

		"list_db_tables": "Report which code reads and which code writes each database table, with " +
			"per-table read/write counts. Pass table to look up one table by name. READ and WRITE are " +
			"counted separately: 'what touches this table' is a weaker question than 'what writes it', " +
			"and the second is the one to check before changing a column. A table no migration here " +
			"declares is still reported — that is the normal shape of a service reading a database " +
			"another service owns. Some statements are assembled at run time; the response says how " +
			"many, because whether that exact statement executes as written cannot be established by " +
			"a static read. Prefer this over grepping for SQL string literals or ORM calls: those are " +
			"exactly the queries this scan may also miss, and this tool says so instead of reporting a " +
			"false all-clear.",

		"get_resolution_coverage": "Ask this when a find_callers, find_references or analyze_impact " +
			"result came back shorter than you expected, BEFORE concluding anything from it. It " +
			"reports how many of this index's references were actually bound to a definition, and " +
			"names the two populations excluded from that rate: the ambiguous ones, where several " +
			"definitions matched and RepoCortex refused to guess, and the ones that matched nothing " +
			"here at all, which are mostly library calls. A low figure means every relationship " +
			"answer from this index is a LOWER BOUND drawn from a minority of the evidence — an " +
			"empty result then tells you about the index, not about the code — and the response " +
			"names the command that raises it. Takes no arguments.",

		"get_config_key": "Look up one configuration key by name: where it is declared and how many " +
			"places read it, joined into a single mismatch check. The two findings worth acting on are " +
			"a key READ but nowhere DECLARED — the way a service fails at start-up — and a key " +
			"declared and never read, which is either dead configuration or a lookup this scan cannot " +
			"see. VALUES ARE NEVER READ: no field in the response can hold one, so a wrong redaction " +
			"rule cannot leak one either; a key whose name looks like a credential is labelled " +
			"sensitive by name alone. Declarations are read from .env, YAML, JSON, TOML and " +
			".properties files — Kubernetes, Helm, Terraform and CI definitions are not, so a key set " +
			"only there reports as undeclared even though it works in production, which is a coverage " +
			"limit, not a finding. Prefer this over grepping for the key string across the repository.",

		"get_file_outline": "Get a token-efficient structural outline of a source file by pruning " +
			"implementation bodies while preserving all imports, types, interfaces, structs, function " +
			"signatures, and docstrings. Use this instead of reading an entire large file (reducing " +
			"10,000 tokens to 300 tokens) when you only need to inspect declarations, API contracts, " +
			"or symbol signatures.",

		"minimize_diff": "Compress unified git diffs into structured, token-efficient representations. " +
			"Strips redundant index hashes and file timestamps, extracts enclosing symbol headers, and " +
			"collapses unchanged context runs to minimize context token usage. Use this when analyzing " +
			"pull requests, patches, or git diffs to preserve context window space.",

		"plan_change": "Generate an ordered change plan for a symbol: the direct callers the index " +
			"bound, a risk level, the test files found to cover it, and the steps in order. Use it " +
			"before modifying or refactoring existing code. The callers are the ones RepoCortex could " +
			"bind: when they could not be determined the risk is 'unknown' and the first line says so.",

		"list_blindspots": "Audit the repository for code intelligence blindspots and hidden risks. " +
			"Detects unresolved external and internal calls, untested critical functions with high fan-in, " +
			"unmapped HTTP endpoints, orphan database tables, missing config declarations, and unparsed files. " +
			"Use this to audit codebase health and find coverage gaps.",

		"analyze_change": "Find out what a range of commits puts at risk: the symbols the change touched, " +
			"everything those can reach, and the tests among them. Use it to review a branch or a pull " +
			"request before merging, or to check your own work before you push — pass the base branch " +
			"and, optionally, the head. Changed symbols are listed first, then the tests that reach them " +
			"(run those first), then the other code within reach, each with its confidence tier. It " +
			"reads the index as built: if the index is older than the head of the range, symbols added " +
			"since are missing and the response says so. To ask about ONE symbol, use analyze_impact. " +
			"Needs a git repository.",

		"find_owners": "Find who has recent context on the code a change would touch: the people who " +
			"have lately changed the symbols a change to this one can reach, ranked by how much of " +
			"that code they have worked on. Use it to suggest a reviewer for a pull request, or to " +
			"find who to ask before changing unfamiliar code. It reports who CHANGED the code " +
			"recently, read from git history, and not who owns it or must approve it: someone in " +
			"CODEOWNERS may be absent. Names only; email addresses are never returned. Needs a git " +
			"repository.",

		"list_knowledge_risks": "List the directories where recent work is concentrated in the fewest " +
			"people, most concentrated first. For each it gives the bus factor (the smallest number of " +
			"people whose commits make up at least half of the recent commits there), the share held by " +
			"the largest contributor and how long ago they last changed it. Use it to find code that one " +
			"person carries before planning a handover, a review rota or a risky change, or pass 'path' " +
			"to ask about one part of the repository. It counts who CHANGED the code in the commits " +
			"read, not who understands it. People are the author names git recorded, never merged and " +
			"never with an email address. A shallow clone or a squash-merge workflow distorts the " +
			"figures, and directories with too few commits are counted but not measured: an empty list " +
			"means nothing could be measured, not that knowledge is well spread. Needs a git repository.",
		"get_session_stats": "Report what this session has used so far: the calls and failures per tool, " +
			"the size of the answers in bytes and estimated tokens, the time spent answering, and " +
			"what the exposed tool definitions cost on every request. Use it when asked what " +
			"RepoCortex cost a session. It reports usage and never a saving: token figures are " +
			"estimates (bytes / 4), and nothing here compares against working without RepoCortex. " +
			"Counts cover the current session only and leave out this call.",

		"find_callees": "List what a symbol calls: the definitions inside this repository that its body " +
			"invokes, each with the file and line of the call and how the binding was established " +
			"(exact, high, medium or low). Use it to read a function's dependencies without opening " +
			"the file, or to walk down from an entry point. It lists only calls the index could bind " +
			"to a definition HERE: calls into libraries and the standard library never appear, so an " +
			"empty or short result means 'nothing bound inside this repository', not 'calls nothing'. " +
			"For the opposite direction use find_callers.",

		"find_implementations": "List a type's hierarchy in both directions: what implements or extends " +
			"it, directly or through other subtypes, and what it implements or extends itself. Use it before changing an interface, an " +
			"abstract method or a base class, when the callers of the declaration are not the whole " +
			"story because the work happens in the types built on it. Each row carries its confidence " +
			"tier; exact means a compiler index proved the relationship. Only DECLARED relationships " +
			"are visible from syntax, so a Go type that satisfies an interface without naming it is " +
			"absent until a compiler index is imported — an empty result means none declared, not " +
			"none exist.",

		"trace_call_path": "Find how one symbol reaches another: the shortest chains of calls and " +
			"references from 'from' to 'to', hop by hop, each hop with its file, line and confidence. " +
			"Use it to answer 'how does this request end up writing that table', to confirm a suspected " +
			"dependency before a refactor, or to explain a blast radius entry you did not expect. Only " +
			"the SHORTEST routes are returned, at most three. No route found is not proof of " +
			"independence: one unresolved call or interface dispatch along the way breaks the chain, " +
			"and the response says so.",

		"list_dead_code": "List definitions that nothing in the index calls or references, graded by " +
			"how far that can be trusted. 'likely' means the definition is known to be unexported, its " +
			"file parsed cleanly, no unbound reference anywhere is spelled like it and its name appears " +
			"nowhere else in the source text; 'possible' means one of those could not be ruled out, and " +
			"the row says which. Export status is known for Go only, so in other languages nothing is " +
			"graded better than 'possible'. Only 'likely' is returned unless you ask. Use it to find cleanup candidates " +
			"or to check whether code you are about to maintain is reachable at all. NEVER delete on " +
			"this evidence alone: reflection, dependency injection, framework registration and other " +
			"repositories all reach code without a reference this index can bind, so search for the " +
			"name first. Tests, entry points and, by default, exported definitions are left out; a " +
			"route handler or event consumer is never 'likely'. Pass include_test_only to also get, " +
			"as a separate section that is never 'likely', definitions whose only callers are tests.",

		"list_hotspots": "Rank files by how often they change against how much code they hold, from " +
			"recent git history, and list for each the files that are usually committed with it. Use " +
			"it to decide where a refactor pays back, to see where review attention should go, and — " +
			"through 'changes with' — to find files coupled to the one you are editing even though no " +
			"call or import connects them, which is a dependency analyze_impact cannot see. The score " +
			"is relative to this repository and this commit window; it orders the list and is not a " +
			"quality measurement. Needs a git repository.",

		"list_communities": "Group the repository's files into communities: sets of files whose calls and " +
			"references run between each other more than they run outwards. Use it to see how an " +
			"unfamiliar repository divides before reading it, or to find the files that belong with " +
			"the one you are about to change. Each community comes with a label, a stable id, its " +
			"size, how many relationships stay inside it against how many cross its boundary, and its " +
			"most-referenced files; pass path to partition one part of a large repository. A community " +
			"describes how the indexed code refers to itself today and is NOT a declared architecture. " +
			"Only relationships the index bound are counted, so files that work together can be " +
			"reported apart, or in no community at all, when what connects them is a reference the " +
			"index could not bind; the response says how many those are. No communities found is not " +
			"proof the files are unrelated.",

		"search_graph": "Execute a guarded, read-only SQL query against the repository's code graph " +
			"(tables: symbols, edges, files, references, http_routes, data_access). Enables custom " +
			"topological filtering and multi-hop queries without multi-roundtrip MCP calls.",

		"repocortex": "Compact front-door code intelligence interface. Executes targeted code intelligence actions " +
			"(context, search, symbol, callers, callees, references, impact, tests, diff, overview, query) through a single unified schema, " +
			"saving over 25 KB of agent token context. Use action='context' for task-oriented context packing, 'search' for regex/code " +
			"search, 'symbol' for definitions, 'callers'/'callees'/'references' for graph traversal, 'impact'/'tests' for blast-radius analysis, and 'query' for declarative graph queries.",
	}
}

func (s *Server) registerTools() error {
	allowed := toolFilter(s.deps.Config.MCPProfile, s.deps.AllowTools)
	descs := toolDescriptions()

	addTool(s, allowed, &sdk.Tool{
		Name:        "get_project_overview",
		Description: descs["get_project_overview"],
	}, s.getProjectOverview)

	addTool(s, allowed, &sdk.Tool{
		Name:        "search_code",
		Description: descs["search_code"],
	}, s.searchCode)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_symbol",
		Description: descs["find_symbol"],
	}, s.findSymbol)

	addTool(s, allowed, &sdk.Tool{
		Name:        "get_symbol",
		Description: descs["get_symbol"],
	}, s.getSymbol)

	s.registerExplore(allowed)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_references",
		Description: descs["find_references"],
	}, s.findReferences)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_callers",
		Description: descs["find_callers"],
	}, s.findCallers)

	addTool(s, allowed, &sdk.Tool{
		Name:        "plan_rename",
		Description: descs["plan_rename"],
	}, s.planRename)

	// Endpoint, table and config-key discovery need only the Registry, not
	// the call graph, so they register here rather than in
	// registerCapabilities — an index whose references were never resolved
	// should still answer these, exactly as search_code and find_symbol do.
	addTool(s, allowed, &sdk.Tool{
		Name:        "list_api_endpoints",
		Description: descs["list_api_endpoints"],
	}, s.findAPIEndpoints)

	addTool(s, allowed, &sdk.Tool{
		Name:        "list_db_tables",
		Description: descs["list_db_tables"],
	}, s.findDBAccess)

	addTool(s, allowed, &sdk.Tool{
		Name:        "get_config_key",
		Description: descs["get_config_key"],
	}, s.getConfigKey)

	// Registered here rather than in registerCapabilities on purpose: it needs
	// no graph, and the index whose references were never resolved is exactly
	// the index it exists to describe. See resolutionCoverage.
	addTool(s, allowed, &sdk.Tool{
		Name:        "get_resolution_coverage",
		Description: descs["get_resolution_coverage"],
	}, s.resolutionCoverage)

	addTool(s, allowed, &sdk.Tool{
		Name:        "get_file_outline",
		Description: descs["get_file_outline"],
	}, s.getFileOutline)

	addTool(s, allowed, &sdk.Tool{
		Name:        "minimize_diff",
		Description: descs["minimize_diff"],
	}, s.minimizeDiff)

	return nil
}

// ---------------------------------------------------------------- overview

func (s *Server) getProjectOverview(ctx context.Context, req *sdk.CallToolRequest, _ emptyArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	counts, err := s.deps.Registry.Counts(ctx, runID)
	if err != nil {
		return failFrom(err, "read index counts: %v", err)
	}
	rs, err := s.deps.Registry.ResolutionStats(ctx, runID)
	if err != nil {
		return failFrom(err, "read resolution stats: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%s — %d files, %d symbols, %d relationships indexed.",
			s.deps.Config.ProjectName, counts.Files, counts.Symbols, counts.Edges),
	}

	if eps, err := s.deps.Registry.EndpointReport(ctx, runID); err == nil && len(eps) > 0 {
		liveCount := 0
		var samples []string
		for _, e := range eps {
			if !capability.IsTestPath(e.FilePath) {
				liveCount++
				if len(samples) < 4 {
					samples = append(samples, e.Signature)
				}
			}
		}
		if liveCount > 0 {
			hint := "Call 'list_api_endpoints' for full catalog."
			if !s.hasTool("list_api_endpoints") && s.hasTool("repocortex") {
				hint = "Call repocortex {action: \"endpoints\"} for full catalog."
			}
			resp.Notes = append(resp.Notes, fmt.Sprintf("Exposed HTTP APIs: %d live endpoints (e.g. %s). %s", liveCount, strings.Join(samples, ", "), hint))
		}
	}

	if tbls, err := s.deps.Registry.TableUsageReport(ctx, runID); err == nil && len(tbls) > 0 {
		var samples []string
		for i, t := range tbls {
			if i < 4 {
				samples = append(samples, t.Table)
			}
		}
		hint := "Call 'list_db_tables' for the access graph."
		if !s.hasTool("list_db_tables") && s.hasTool("repocortex") {
			hint = "Call repocortex {action: \"tables\"} for the access graph."
		}
		resp.Notes = append(resp.Notes, fmt.Sprintf("Database Footprint: %d tables referenced (e.g. %s). %s", len(tbls), strings.Join(samples, ", "), hint))
	}

	guidance := "Agent Guidance: Use 'analyze_impact' to evaluate refactoring impact; use 'find_symbol' to resolve declarations; use 'find_callers' to trace upstream callers."
	if !s.hasTool("analyze_impact") && s.hasTool("repocortex") {
		guidance = "Agent Guidance: Use repocortex {action: 'impact'} to evaluate refactoring impact; use repocortex {action: 'symbol'} to resolve declarations; use repocortex {action: 'callers'} to trace upstream callers."
	}
	resp.Notes = append(resp.Notes, guidance)

	// State the resolution rate plainly. An agent that believes the call graph
	// is complete will trust an empty find_callers result as proof that nothing
	// calls a symbol; telling it the true coverage prevents that mistake.
	// The condition here used to be `rs.Total > 0`, which is true the moment
	// any reference is EXTRACTED — long before any is resolved. So an index
	// that had never been resolved reported "0% of 16973 references are bound,
	// 0 point outside, 0 ambiguous" to the agent: impossible arithmetic, and
	// no hint that a step was missing. Pending is the field that distinguishes
	// "resolved and found little" from "never resolved".
	switch {
	case rs.Total == 0:
		// No reference rows at all. The coverage caveat still has to be stated:
		// an agent that reads an empty find_callers result as proof a symbol is
		// unused will delete it, and "we have no data" is a stronger reason to
		// distrust the result than "we have partial data".
		resp.Notes = append(resp.Notes,
			"No reference data is recorded for this index, so the completeness of the call "+
				"graph is unknown — an empty find_callers result must NOT be read as proof "+
				"that nothing uses a symbol. Run 'repocortex index' followed by "+
				"'repocortex resolve' to build it.")
	case rs.Pending == rs.Total:
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d reference(s) were extracted but none have been resolved — the call graph has "+
				"not been built. find_callers and find_references will return nothing until "+
				"'repocortex resolve' runs.", rs.Total))
	default:
		// Same correction as cli.bindingCaveat, and it matters more here: an
		// agent acts on this number. The denominator is the references that
		// name something this index defines, because one that matched nothing
		// here cannot be bound by better binding; and 'unresolved' is described
		// as what it is rather than asserted to be external.
		by := ""
		if rs.ByCompiler > 0 {
			by = fmt.Sprintf(" %d of those were settled by an imported compiler index.",
				rs.ByCompiler)
		}
		note := fmt.Sprintf(
			"%.0f%% of the %d references that name something this index defines are bound to a "+
				"definition;%s %d are too ambiguous to choose between. A further %d matched no "+
				"definition here at all, mostly calls into libraries. Caller and reference "+
				"results are a lower bound, not a complete set.",
			rs.BindingRate()*100, rs.Bindable(), by, rs.Ambiguous, rs.Unresolved)
		if rs.Pending > 0 {
			note += fmt.Sprintf(" A further %d reference(s) have been extracted since the last "+
				"resolve and are not in the graph yet.", rs.Pending)
		}
		resp.Notes = append(resp.Notes, note)
	}
	// An EMPTY index is a recoverable condition, not a broken server: a run was
	// committed and it holds nothing. Saying so plainly, with the command that
	// fixes it, keeps the agent using RepoCortex for the rest of the session
	// instead of concluding the toolset does not work.
	if counts.Symbols == 0 {
		resp.Notes = append(resp.Notes,
			"This index is committed but EMPTY — no symbols were recorded. Every query will "+
				"return nothing until it is rebuilt: run 'repocortex index' and check that the "+
				"project's languages are supported and its files are not all excluded.")
	}
	if counts.Edges == 0 {
		resp.Notes = append(resp.Notes, "The call graph is empty; relationship queries cannot answer yet.")
	}
	return s.reply(ctx, req, resp)
}

// ---------------------------------------------------------------- search

// ---------------------------------------------------------------- symbols

func (s *Server) findSymbol(ctx context.Context, req *sdk.CallToolRequest, args symbolArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call find_symbol again.",
			"name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	syms, err := s.deps.Registry.FindSymbolsByName(ctx, runID, args.Name, args.Kind, s.limits.MaxItems)
	if err != nil {
		return failFrom(err, "symbol lookup failed: %v", err)
	}
	if len(syms) == 0 {
		// An owner and a member: Guard.AgainstNull, Guard::AgainstNull, Mailer#deliver.
		cands, _, qerr := s.deps.Registry.SymbolCandidatesQualified(ctx, runID, args.Name, s.limits.MaxItems)
		if qerr != nil {
			return failFrom(qerr, "symbol lookup failed: %v", qerr)
		}
		for _, c := range cands {
			if args.Kind == "" || c.Kind == args.Kind {
				syms = append(syms, c.SymbolHit)
			}
		}
	}

	// A definition carries a confidence tier of its own: a compiler-indexed
	// declaration is 'exact', one recovered from a partial parse is not. So the
	// floor selects a real subset here, it does not merely pass through.
	found := len(syms)
	syms, dropped := atLeastConfidence(syms, minConf)
	if found > 0 && len(syms) == 0 {
		return s.belowFloorOnly(ctx, req, runID, status, args.Name, minConf, dropped)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%d definition(s) named %q.", len(syms), args.Name),
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}
	for _, sym := range syms {
		resp.Items = append(resp.Items, Item{
			Kind:       sym.Kind,
			Name:       sym.Name,
			FQN:        sym.FQN,
			Path:       sym.FilePath,
			Line:       sym.StartLine,
			Confidence: sym.Confidence,
			Source:     sym.Source,
		})
	}
	if len(syms) == 0 {
		resp.Notes = append(resp.Notes,
			"Not found. The name must match exactly; use search_code for partial matches.")
	}
	return s.reply(ctx, req, resp)
}

// maxSymbolExcerpts is how many definitions get_symbol shows with source. Each
// costs an excerpt, and a name shared by more than this is one to narrow.
const maxSymbolExcerpts = 5

func (s *Server) getSymbol(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call get_symbol again.",
			"name must not be empty")
	}
	// get_symbol ADVERTISED min_confidence before it honoured it: symbolRefArgs
	// is shared with find_references, so the schema promised a filter this
	// handler never applied and an agent that misspelled the tier got a silent
	// success. Decoding it here is what makes the advertised schema true.
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	if args.Line < 0 {
		return failWith("invalid_argument", "Pass line as the line a definition starts on, or omit it.",
			"line must not be negative")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}
	name := strings.TrimSpace(args.Name)
	q := symbolQuery{Tool: "get_symbol", Param: "name", PathParam: "path", LineParam: "line",
		Name: name, Path: args.Path, Line: args.Line}

	// Every definition of the name is read, and the path and line are applied
	// to all of them. This used to read the first five and look for the path
	// among those, so a path naming the sixth returned an answer with no
	// definition in it.
	all, total, err := s.deps.Registry.SymbolCandidates(ctx, runID, name, maxSelectRows)
	if err != nil {
		return failFrom(err, "symbol lookup failed: %v", err)
	}
	if total == 0 {
		if all, total, err = s.deps.Registry.SymbolCandidatesByFQN(ctx, runID, name, maxSelectRows); err != nil {
			return failFrom(err, "symbol lookup failed: %v", err)
		}
	}
	qualified := false
	if total == 0 {
		// An owner and a member: Guard.AgainstNull, Guard::AgainstNull, Mailer#deliver.
		if all, total, err = s.deps.Registry.SymbolCandidatesQualified(ctx, runID, name, maxSelectRows); err != nil {
			return failFrom(err, "symbol lookup failed: %v", err)
		}
		qualified = total > 0
	}
	if total == 0 {
		return s.missingSymbol(ctx, req, runID, status, name)
	}
	cands := all
	if want := cleanPathHint(args.Path); want != "" {
		if total > len(all) && !qualified {
			if cands, _, err = s.deps.Registry.SymbolCandidatesIn(ctx, runID, name, want, maxSelectRows); err != nil {
				return failFrom(err, "symbol lookup failed: %v", err)
			}
		}
		if cands = matchPath(cands, want); len(cands) == 0 {
			sc, r := s.disambiguate(ctx, req, runID, status, q,
				fmt.Sprintf("no definition of %q is in a file matching path=%q", name, args.Path), all, total)
			return sc, r, nil
		}
	}
	if args.Line > 0 {
		at := matchLine(cands, args.Line)
		if len(at) == 0 {
			sc, r := s.disambiguate(ctx, req, runID, status, q,
				fmt.Sprintf("no definition of %q starts at or contains line %d", name, args.Line), cands, total)
			return sc, r, nil
		}
		cands = at
	}

	hits := make([]registry.SymbolHit, len(cands))
	for i, c := range cands {
		hits[i] = c.SymbolHit
	}
	syms, dropped := atLeastConfidence(hits, minConf)
	if len(syms) == 0 {
		return s.belowFloorOnly(ctx, req, runID, status, name, minConf, dropped)
	}
	shown := syms
	if len(shown) > maxSymbolExcerpts {
		shown = shown[:maxSymbolExcerpts]
	}

	resp := &Response{IndexRevision: runID, IndexStatus: status}
	if len(syms) == 1 {
		resp.Summary = fmt.Sprintf("%s (%s) at %s:%d", syms[0].Name, syms[0].Kind, syms[0].FilePath, syms[0].StartLine)
	} else {
		// Several definitions are several answers. The summary used to name
		// the first as if it were the symbol.
		resp.Summary = fmt.Sprintf("%d definitions are named %q; %d are shown below, each with its source. "+
			"Pass path and line to see one.", len(syms), name, len(shown))
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}
	for _, sym := range shown {
		resp.Items = append(resp.Items, Item{
			Kind:       sym.Kind,
			Name:       sym.Name,
			FQN:        sym.FQN,
			Path:       sym.FilePath,
			Line:       sym.StartLine,
			Confidence: sym.Confidence,
			Source:     sym.Source,
			Why:        "definition",
			// The tool's own description promises "a bounded source excerpt",
			// and it was returning none — an agent that followed the
			// description read the location, found no source, and opened the
			// file, which is the one outcome this server exists to prevent.
			// Served from the index rather than the filesystem, so what the
			// agent reads is what RepoCortex analysed.
			Excerpt: s.symbolExcerpt(ctx, runID, sym),
		})
	}
	for _, sym := range shown {
		if note := s.markedNote(ctx, runID, sym); note != "" {
			resp.Notes = append(resp.Notes, note)
		}
	}
	if rest := len(syms) - len(shown); rest > 0 {
		resp.markTruncated(fmt.Sprintf("%d more definition(s) named %q are not shown; pass path to narrow, "+
			"or find_symbol to list every one", rest, name))
	}
	return s.reply(ctx, req, resp)
}

// symbolExcerpt reads a symbol's own lines out of the index.
//
// Clamped to the response's excerpt budget rather than to the symbol's true
// span: a 900-line class must not arrive as a 900-line excerpt, and Bound would
// cut it anyway — reading it first would just be work thrown away.
//
// A read that fails returns "", not an error. The definition's location is
// still a correct and useful answer, and turning a missing FTS row into an
// isError would teach the agent the toolset is broken over a condition it can
// neither see nor fix. See the isError policy in server.go.
func (s *Server) symbolExcerpt(ctx context.Context, runID int64, sym registry.SymbolHit) string {
	lines := sym.EndLine - sym.StartLine + 1
	if lines < 1 {
		lines = 1
	}
	if s.limits.MaxExcerptLines > 0 {
		lines = min(lines, s.limits.MaxExcerptLines)
	}
	text, err := s.deps.Registry.FileExcerpt(ctx, runID, sym.FilePath, sym.StartLine, lines)
	if err != nil {
		return ""
	}
	return text
}

// ---------------------------------------------------------------- references

func (s *Server) findReferences(ctx context.Context, req *sdk.CallToolRequest, args siteArgs) (*sdk.CallToolResult, *Response, error) {
	return s.relationshipQuery(ctx, req, args, false)
}

func (s *Server) findCallers(ctx context.Context, req *sdk.CallToolRequest, args siteArgs) (*sdk.CallToolResult, *Response, error) {
	return s.relationshipQuery(ctx, req, args, true)
}

// relationshipQuery answers both references and callers.
//
// Shared because the honesty requirement is identical for both, and it is the
// part most easily got wrong: an EMPTY RESULT IS AMBIGUOUS. It may mean nothing
// calls the symbol, or that RepoCortex could not resolve the calls that do. An
// agent told "no callers" will happily delete a method. So an empty result
// always carries what the index did with the references spelled like the name.
func (s *Server) relationshipQuery(ctx context.Context, req *sdk.CallToolRequest, args siteArgs, callersOnly bool) (*sdk.CallToolResult, *Response, error) {
	tool, label := "find_references", "reference"
	var kinds []string
	q := symbolQuery{Param: "name", PathParam: "path", LineParam: "line",
		Name: args.Name, Path: args.Path, Line: args.Line, Within: args.Within}
	if callersOnly {
		tool, label = "find_callers", "caller"
		kinds = []string{"CALLS"}
		// A caller list is about something callable. A class and its
		// same-named constructor, or a function and a variable holding it,
		// are the usual way a callable name is shared.
		q.Prefer, q.PreferWord, q.PreferNoun = callableKinds, "functions and methods", "function or method"
		// What constructs a class is a caller of it in the sense that matters,
		// and the answer counts the constructions separately.
		q.Tolerate = typeKinds
	}
	q.Tool = tool

	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call "+tool+" again.",
			"name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	page, err := s.page(args.pageArg, label+"(s)")
	if err != nil {
		return failWith("invalid_argument", "Pass limit and offset as non-negative numbers, or omit them.", "%v", err)
	}

	// The reference lookup itself — symbol selection, the confidence/kind
	// filtered registry query, item construction, coverage — is shared with
	// plan_rename via referencesFor (rename.go) so there is one implementation
	// of it, not two.
	lookup, shortCircuit := s.referencesFor(ctx, req, q, minConf, kinds, page, callersOnly)
	if shortCircuit != nil {
		return shortCircuit, lookup.Resp, nil
	}
	resp, target, runID := lookup.Resp, lookup.Target, lookup.RunID

	// The TOTAL, not the number of rows in this answer: "how many callers" is
	// the question, and the first line must answer it even when the rows are a
	// page of them. Which rows are shown is said by the lead and by Page.
	resp.Summary = fmt.Sprintf("%d %s(s) of %s (%s:%d) are recorded.",
		page.Total, label, target.Name, target.FilePath, target.StartLine)
	if callersOnly {
		// page.Total counts call sites. "How many callers" is a count of
		// functions, and a function that calls three times is one of them: the
		// line said "56 caller(s)" of a method that 18 functions call.
		filter := registry.EdgeFilter{MinConfidence: minConf, Kinds: kinds, Within: args.Within}
		if callers, cerr := s.deps.Registry.IncomingSourceCount(ctx, runID, target.ID, filter); cerr == nil && callers < page.Total {
			resp.Summary = fmt.Sprintf("%d %s(s) of %s (%s:%d) are recorded, at %d call sites.",
				callers, label, target.Name, target.FilePath, target.StartLine, page.Total)
		}
	}

	// notCalls is informational only — how many further relationships point at
	// this symbol without being calls — so it is a COUNT(*), not a row fetch:
	// fetching rows at the same s.limits.MaxItems cap used above would make
	// the count itself saturate at that cap, understating an arbitrarily
	// large true total as if it were exact (a 20-edge fixture with
	// MaxItems=3 would report "3 further relationship(s)", wrong by an order
	// of magnitude, stated with the same confidence as an accurate count).
	// COUNT(*) has no such cap.
	if callersOnly {
		notCalls, err := s.deps.Registry.IncomingEdgeCount(ctx, runID, target.ID,
			registry.EdgeFilter{MinConfidence: minConf, Within: args.Within}, []string{"CALLS"})
		if err != nil {
			resp.Notes = append(resp.Notes,
				"could not determine whether other relationship kinds exist")
		} else if notCalls > 0 {
			// When the filter dropped something, say so. "3 callers" alongside
			// a repository where 6 things reference the symbol is accurate but
			// incomplete, and the difference is exactly what a caller wants to
			// know.
			//
			// The count is FILTERED, so it has to say so. IncomingEdgeCount is
			// called with the same min_confidence floor as the fetch above, so
			// at min_confidence="high" a symbol with 3 high and 27 low non-call
			// references reports 3 — a true statement about a filtered set,
			// read as a total. Naming the floor is the whole difference between
			// the two readings.
			filtered := ""
			if minConf != model.ConfidenceLow {
				filtered = fmt.Sprintf(" at confidence %q and above", minConf)
			}
			// A constructor call is recorded against the TYPE, as INSTANTIATES,
			// and this tool reads CALLS only. A class that is only ever
			// constructed therefore has no callers, which is easily read as
			// "nothing uses it". Counted apart so the answer can say it is used.
			constructs, cerr := s.deps.Registry.IncomingEdgeCount(ctx, runID, target.ID,
				registry.EdgeFilter{MinConfidence: minConf, Kinds: []string{"INSTANTIATES"}, Within: args.Within}, nil)
			others := "imports, implementations or plain references"
			if cerr == nil && constructs > 0 {
				others = fmt.Sprintf("%d of them construct it (INSTANTIATES), the rest are imports, "+
					"implementations or plain references", constructs)
				if constructs == notCalls {
					others = fmt.Sprintf("all %d construct it (INSTANTIATES)", constructs)
				}
				resp.Summary += fmt.Sprintf(" %d construct it, which is not a call: find_references lists them.", constructs)
			}
			resp.Notes = append(resp.Notes, fmt.Sprintf(
				"%d further relationship(s)%s point at this symbol without being calls — %s. "+
					"Use find_references to see them.",
				notCalls, filtered, others))
		}
	}

	// Verdict is NOT set here. Server.reply grades every response once, after
	// bounding — see Response.Verdict.
	return s.reply(ctx, req, resp)
}

// ---------------------------------------------------------------- outline & diff

func (s *Server) getFileOutline(ctx context.Context, req *sdk.CallToolRequest, args fileOutlineArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Path) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty path, then call get_file_outline again.",
			"path must not be empty")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	// The path comes from an agent, and an agent's arguments derive from
	// content it read, so it is untrusted: contain it to the repository root
	// (rejects "../", absolute paths outside the root, and symlink escapes).
	// An absolute path inside the root is accepted by rebasing it first, which
	// keeps callers that echo back the paths the index reports working.
	requested := args.Path
	if filepath.IsAbs(requested) && s.deps.Config.RepoRoot != "" {
		if rel, relErr := filepath.Rel(s.deps.Config.RepoRoot, requested); relErr == nil {
			requested = rel
		}
	}
	// ReadFileContained validates, then reads through os.Root so a component
	// swapped for a symlink between check and open cannot escape either.
	src, resolved, err := security.ReadFileContained(s.deps.Config.RepoRoot, requested)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return failFrom(err, "read file %q: %v", args.Path, err)
		}
		return failWith("invalid_argument",
			"Pass a path relative to the project root that stays inside it, then call get_file_outline again.",
			"path %q is not readable: %v", args.Path, err)
	}

	// Credential files are never served, outlined or not: the same policy and
	// notice as security.RedactFile. Both the requested name and the resolved
	// target are checked, so a link named notes.txt pointing at .env is caught.
	if security.IsSensitivePath(requested) || security.IsSensitivePath(resolved) {
		notice, _ := security.RedactFile(requested, string(src))
		if !security.IsSensitivePath(requested) {
			notice, _ = security.RedactFile(filepath.Base(resolved), string(src))
		}
		return s.reply(ctx, req, &Response{
			IndexRevision: runID,
			IndexStatus:   status,
			Summary:       fmt.Sprintf("%s is a credentials file; its contents are never served.", args.Path),
			Items: []Item{{
				Kind:    "file_outline",
				Path:    args.Path,
				Why:     "sensitive path suppressed in full",
				Excerpt: notice,
			}},
		})
	}

	pruned, err := prune.PruneFile(args.Path, src)
	if err != nil {
		return failFrom(err, "prune file %q: %v", args.Path, err)
	}

	origLen := len(src)
	prunedLen := len(pruned)
	var savedPct float64
	if origLen > 0 {
		savedPct = float64(origLen-prunedLen) / float64(origLen) * 100
		if savedPct < 0 {
			savedPct = 0
		}
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("Pruned outline of %s (%d -> %d bytes, %.1f%% reduction).",
			args.Path, origLen, prunedLen, savedPct),
		Items: []Item{
			{
				Kind:    "file_outline",
				Path:    args.Path,
				Why:     "structural AST pruning (bodies stripped, signatures preserved)",
				Excerpt: string(pruned),
			},
		},
	}
	return s.reply(ctx, req, resp)
}

func (s *Server) minimizeDiff(ctx context.Context, req *sdk.CallToolRequest, args minimizeDiffArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Diff) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty diff, then call minimize_diff again.",
			"diff must not be empty")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	rep, err := diffmini.ParseAndCompress(args.Diff)
	if err != nil {
		return failFrom(err, "parse diff: %v", err)
	}

	formatted := rep.FormatText()
	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("Compressed %d file diff(s) (%d -> %d bytes, %.1f%% token reduction, ~%d tokens).",
			len(rep.Files), rep.OriginalBytes, rep.CompressedBytes, rep.SavedTokensPct, rep.EstimatedTokens),
		Items: []Item{
			{
				Kind:    "diff",
				Why:     fmt.Sprintf("minimized diff across %d file(s)", len(rep.Files)),
				Excerpt: formatted,
			},
		},
	}
	return s.reply(ctx, req, resp)
}
