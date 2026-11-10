package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
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
// find_callers, find_references, impact_analysis and find_tests all answer the
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

// coverageFloor is the binding rate below which a relationship answer stops
// being an answer.
//
// It is capability.CoverageFloor, not a number of its own. This gate used to
// carry its own 0.5 against ResolutionStats.Rate while resolution_coverage
// used 0.6 against BindingRate, so the two tools reported different coverage
// for one index in one session — see capability.CoverageFloor for the whole
// argument and for why BindingRate is the published rate.
//
// The number is a judgement rather than a measurement, which is exactly why
// the response states the RATE and its denominator as well as the verdict: a
// reader who would draw the line elsewhere can see the input and decide for
// himself.
const coverageFloor = capability.CoverageFloor

// coverage is what the server knows about how far to trust the graph for a
// relationship question about one symbol.
type coverage struct {
	// Language is the target's language, named because coverage is uneven
	// across them: a repository can resolve Go almost completely and Kotlin
	// barely at all, and "22% of references are bound" is only actionable when
	// the reader knows which half of his repository it describes.
	Language string

	// Rate is registry.ResolutionStats.BindingRate: resolved over the
	// references that name something this index defines. See
	// capability.CoverageFloor for why this rate and not Rate().
	Rate float64

	// Bindable is Rate's denominator, and Unresolved and Ambiguous are what
	// that denominator leaves out. All three are carried so the caveat can
	// state them beside the percentage, which is the condition
	// ResolutionStats.Bindable places on excluding them.
	Bindable   int64
	Unresolved int64
	Ambiguous  int64
	Total      int64

	// Known is false when the run holds no resolution outcomes to judge — no
	// references at all, or none resolved yet. Distinct from a LOW rate: one
	// says RepoCortex looked and found little, the other says it never looked,
	// and only the second is fixed by running 'cortex resolve'.
	Known    bool
	Reliable bool
}

func coverageOf(rs registry.ResolutionStats, filePath string) coverage {
	c := coverage{
		Language:   languageOf(filePath),
		Rate:       rs.BindingRate(),
		Bindable:   rs.Bindable(),
		Unresolved: rs.Unresolved,
		Ambiguous:  rs.Ambiguous,
		Total:      rs.Total,
	}
	c.Known = rs.Total > 0 && rs.Pending < rs.Total
	c.Reliable = c.Known && c.Rate >= coverageFloor
	return c
}

// scope names the code the rate describes, for the caveat sentence.
func (c coverage) scope() string {
	if c.Language == "" {
		return "this repository"
	}
	return c.Language
}

// caveat is what a low-coverage answer must carry, ahead of its own results.
//
// Empty when coverage clears the floor — silence is the reward for a
// well-resolved index, and paying tokens to say "coverage is fine" on every
// call would be the same waste this server exists to remove.
func (c coverage) caveat(subject string) []string {
	if c.Reliable {
		return nil
	}
	var head string
	switch {
	case c.Total == 0:
		head = fmt.Sprintf("COVERAGE UNKNOWN — %s for %s cannot be judged: this index recorded "+
			"no reference resolution at all.", subject, c.scope())
	case !c.Known:
		head = fmt.Sprintf("COVERAGE UNKNOWN — %s for %s cannot be judged: all %d references are "+
			"still pending, so the call graph has not been built. Run 'cortex resolve'.",
			subject, c.scope(), c.Total)
	default:
		// The unresolved and ambiguous counts are printed BESIDE the rate,
		// never instead of it: the rate's denominator excludes references
		// that matched no definition here, and ResolutionStats.Bindable
		// permits that exclusion only on condition they are still reported.
		head = fmt.Sprintf("COVERAGE TOO LOW — %s for %s is unreliable here: %.0f%% of the %d "+
			"references that name something this index defines are bound to a definition "+
			"(%d ambiguous; a further %d matched no definition here at all, out of %d "+
			"references in total). Run 'cortex resolve' to raise it.",
			subject, c.scope(), 100*c.Rate, c.Bindable, c.Ambiguous, c.Unresolved, c.Total)
	}
	return []string{
		head + " Do not present what follows as an answer to the question asked.",
		capability.LowerBoundCaveat,
	}
}

// extLanguage maps file extensions to the language names RepoCortex indexes.
//
// Deliberately local and deliberately small. It exists to name a language in
// one caveat sentence; wiring the MCP boundary to the parser registry (or to
// the telemetry layer, which keeps its own table for the same reason) to
// answer "what is .kt" would buy a dependency far larger than the question.
// An extension missing here degrades to "this repository", which is accurate.
var extLanguage = map[string]string{
	".go":   "Go",
	".java": "Java",
	".kt":   "Kotlin", ".kts": "Kotlin",
	".ts": "TypeScript", ".tsx": "TypeScript",
	".js": "JavaScript", ".jsx": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript",
	".py": "Python", ".pyi": "Python",
	".rb": "Ruby", ".rs": "Rust", ".cs": "C#",
	".php": "PHP", ".swift": "Swift", ".scala": "Scala",
	".c": "C", ".h": "C", ".cc": "C++", ".cpp": "C++", ".hpp": "C++",
}

func languageOf(filePath string) string {
	return extLanguage[strings.ToLower(path.Ext(filePath))]
}

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

type searchArgs struct {
	Query string `json:"query" jsonschema:"Search terms. Matches identifiers and code text; camelCase identifiers are searchable by their parts, so 'callback' finds handleCallback."`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 20)."`
}

type symbolArgs struct {
	Name string `json:"name" jsonschema:"Symbol name to find, e.g. 'TransactionService' or 'updateStatus'. Exact match on the name, not a substring."`
	Kind string `json:"kind,omitempty" jsonschema:"Optional filter: class, interface, function, method, field, constant, type_alias."`

	minConfidenceArg
}

type symbolRefArgs struct {
	Name string `json:"name" jsonschema:"Symbol name whose references or callers you want."`
	Path string `json:"path,omitempty" jsonschema:"Optional file path to disambiguate when several symbols share a name."`

	minConfidenceArg
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

// parseImpactFloor decodes the floor for impact_analysis alone.
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
			"searches the index, returns file paths and matching lines rather than whole files, and " +
			"costs a fraction of the context. Use it when you know roughly what you are looking for " +
			"but not where it lives, and use it to confirm any relationship result that came back " +
			"empty or low-confidence.",

		"find_symbol": "Locate a symbol's definition by name. Faster and more precise than searching " +
			"when you already know the identifier. If several symbols share the name, pass `path` to " +
			"disambiguate. If this returns nothing, the symbol may still exist under a different name " +
			"or in an unindexed file — fall back to search_code rather than concluding it is absent.",

		"get_symbol": "Get a symbol's definition with a bounded source excerpt and its immediate " +
			"relationships. Call this after find_symbol instead of reading the whole file. The excerpt " +
			"is bounded and may be truncated; the response says so explicitly when it is, so check " +
			"that flag rather than assuming you have the full body.",

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

		"impact_analysis": "Before changing a symbol, find out what depends on it. Separates DIRECT " +
			"callers (a proven call edge) from INDIRECT reach and from dispatch rows the runtime MAY " +
			"execute but no call site proves, and names what it CANNOT see — dynamic dispatch, " +
			"unresolved references, traversal limits. The risk score and its factors are only as good " +
			"as the resolution behind them; the response states the coverage rate ahead of the score " +
			"so you know whether to trust it. The result is a LOWER BOUND: treat an empty or short " +
			"result as 'nothing found', never as proof that changing the symbol is safe.",

		"find_tests": "Find the tests covering a symbol, and which signal found each one — a proven " +
			"call edge is far stronger evidence than a matching filename, and the response names which " +
			"you got. Use before changing behaviour, to know what to run. A symbol reported as " +
			"untested is a positive finding, not silence, but it is bounded by what the index could " +
			"resolve: confirm with search_code before trusting it on a low-resolution repository.",

		"find_api_endpoints": "List the HTTP endpoints this service exposes, with the handler that " +
			"serves each one and where it is declared. Routes are recognised from the framework's own " +
			"syntax — a Spring annotation, a Go router registration, a FastAPI or Flask decorator, an " +
			"Express member call — never from a string that merely looks like a path, which is what " +
			"keeps a log message mentioning 'GET /users' out of the result. Pass method to filter to " +
			"one HTTP verb. NOT covered: OpenAPI documents, GraphQL schemas, gRPC service definitions " +
			"and API-gateway configuration — a service defined entirely in those reports no endpoints " +
			"here, which is a coverage limit, not a finding about the code. Prefer this over grepping " +
			"route decorators by hand.",

		"find_db_access": "Report which code reads and which code writes each database table, with " +
			"per-table read/write counts. Pass table to look up one table by name. READ and WRITE are " +
			"counted separately: 'what touches this table' is a weaker question than 'what writes it', " +
			"and the second is the one to check before changing a column. A table no migration here " +
			"declares is still reported — that is the normal shape of a service reading a database " +
			"another service owns. Some statements are assembled at run time; the response says how " +
			"many, because whether that exact statement executes as written cannot be established by " +
			"a static read. Prefer this over grepping for SQL string literals or ORM calls: those are " +
			"exactly the queries this scan may also miss, and this tool says so instead of reporting a " +
			"false all-clear.",

		"resolution_coverage": "Ask this when a find_callers, find_references or impact_analysis " +
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

		"plan_change": "Generate a deterministic refactoring and change plan for a symbol. " +
			"Identifies all direct callers, assesses risk score, discovers covering test files, and " +
			"produces an ordered execution sequence with pre/post-conditions. Use this before modifying " +
			"or refactoring existing code to ensure regression-free changes.",

		"detect_blindspots": "Audit the repository for code intelligence blindspots and hidden risks. " +
			"Detects unresolved external and internal calls, untested critical functions with high fan-in, " +
			"unmapped HTTP endpoints, orphan database tables, missing config declarations, and unparsed files. " +
			"Use this to audit codebase health and find coverage gaps.",
	}
}

func (s *Server) registerTools() error {
	allowed := profileTools(s.deps.Config.MCPProfile)
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
		Name:        "find_api_endpoints",
		Description: descs["find_api_endpoints"],
	}, s.findAPIEndpoints)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_db_access",
		Description: descs["find_db_access"],
	}, s.findDBAccess)

	addTool(s, allowed, &sdk.Tool{
		Name:        "get_config_key",
		Description: descs["get_config_key"],
	}, s.getConfigKey)

	// Registered here rather than in registerCapabilities on purpose: it needs
	// no graph, and the index whose references were never resolved is exactly
	// the index it exists to describe. See resolutionCoverage.
	addTool(s, allowed, &sdk.Tool{
		Name:        "resolution_coverage",
		Description: descs["resolution_coverage"],
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
			resp.Notes = append(resp.Notes, fmt.Sprintf("Exposed HTTP APIs: %d live endpoints (e.g. %s). Call 'find_api_endpoints' for full catalog.", liveCount, strings.Join(samples, ", ")))
		}
	}

	if tbls, err := s.deps.Registry.TableUsageReport(ctx, runID); err == nil && len(tbls) > 0 {
		var samples []string
		for i, t := range tbls {
			if i < 4 {
				samples = append(samples, t.Table)
			}
		}
		resp.Notes = append(resp.Notes, fmt.Sprintf("Database Footprint: %d tables referenced (e.g. %s). Call 'find_db_access' for the access graph.", len(tbls), strings.Join(samples, ", ")))
	}

	resp.Notes = append(resp.Notes, "Agent Guidance: Use 'impact_analysis' to evaluate refactoring impact; use 'find_symbol' to resolve declarations; use 'find_callers' to trace upstream callers.")

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
				"that nothing uses a symbol. Run 'cortex index' followed by "+
				"'cortex resolve' to build it.")
	case rs.Pending == rs.Total:
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d reference(s) were extracted but none have been resolved — the call graph has "+
				"not been built. find_callers and find_references will return nothing until "+
				"'cortex resolve' runs.", rs.Total))
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
				"return nothing until it is rebuilt: run 'cortex index' and check that the "+
				"project's languages are supported and its files are not all excluded.")
	}
	if counts.Edges == 0 {
		resp.Notes = append(resp.Notes, "The call graph is empty; relationship queries cannot answer yet.")
	}
	return s.reply(ctx, req, resp)
}

// ---------------------------------------------------------------- search

func (s *Server) searchCode(ctx context.Context, req *sdk.CallToolRequest, args searchArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Query) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty query, then call search_code again.",
			"query must not be empty")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	limit := args.Limit
	if limit <= 0 || limit > s.limits.MaxItems {
		limit = 20
	}

	matches, err := s.deps.Registry.SearchFiles(ctx, args.Query, limit)
	if err != nil {
		return failFrom(err, "search failed: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%d file(s) matching %q.", len(matches), args.Query),
	}
	for _, m := range matches {
		resp.Items = append(resp.Items, Item{
			Kind: "file",
			Path: m.Path,
			Why:  "full-text match",
		})
	}
	if len(matches) == 0 {
		resp.Notes = append(resp.Notes,
			"No matches. Search covers indexed files only; generated and oversized files are skipped. "+
				"Try a shorter term — identifiers are indexed by their parts.")
	}
	return s.reply(ctx, req, resp)
}

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
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	syms, err := s.deps.Registry.FindSymbolsByName(ctx, runID, args.Name, "", 5)
	if err != nil {
		return failFrom(err, "symbol lookup failed: %v", err)
	}
	if len(syms) == 0 {
		return s.missingSymbol(ctx, req, runID, status, args.Name)
	}
	syms, dropped := atLeastConfidence(syms, minConf)
	if len(syms) == 0 {
		return s.belowFloorOnly(ctx, req, runID, status, args.Name, minConf, dropped)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%s (%s) at %s:%d", syms[0].Name, syms[0].Kind, syms[0].FilePath, syms[0].StartLine),
	}
	if note := floorNote(minConf, dropped); note != "" {
		resp.Notes = append(resp.Notes, note)
	}
	for _, sym := range syms {
		if args.Path != "" && !strings.Contains(sym.FilePath, args.Path) {
			continue
		}
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
	if len(syms) > 1 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d symbols share this name; pass 'path' to disambiguate.", len(syms)))
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

func (s *Server) findReferences(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs) (*sdk.CallToolResult, *Response, error) {
	return s.relationshipQuery(ctx, req, args, false)
}

func (s *Server) findCallers(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs) (*sdk.CallToolResult, *Response, error) {
	return s.relationshipQuery(ctx, req, args, true)
}

// relationshipQuery answers both references and callers.
//
// Shared because the honesty requirement is identical for both, and it is the
// part most easily got wrong: an EMPTY RESULT IS AMBIGUOUS. It may mean nothing
// calls the symbol, or that RepoCortex could not resolve the calls that do. An
// agent told "no callers" will happily delete a method. So an empty result
// always carries the coverage caveat.
func (s *Server) relationshipQuery(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs, callersOnly bool) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call find_callers or find_references again.",
			"name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}

	label := "reference"
	var kinds []string
	if callersOnly {
		label = "caller"
		kinds = []string{"CALLS"}
	}

	// The reference lookup itself — symbol resolution, the confidence/kind
	// filtered registry query, item construction, coverage caveats — is
	// shared with plan_rename via referencesFor (rename.go) so there is one
	// implementation of it, not two.
	lookup, shortCircuit := s.referencesFor(ctx, req, args.Name, args.Path, minConf, kinds)
	if shortCircuit != nil {
		return shortCircuit, lookup.Resp, nil
	}
	items, resp, target, runID := lookup.Items, lookup.Resp, lookup.Target, lookup.RunID

	resp.Summary = fmt.Sprintf("%d %s(s) of %s (%s:%d).",
		len(items), label, target.Name, target.FilePath, target.StartLine)

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
			registry.EdgeFilter{MinConfidence: minConf}, []string{"CALLS"})
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
			resp.Notes = append(resp.Notes, fmt.Sprintf(
				"%d further relationship(s)%s point at this symbol without being calls — imports, "+
					"implementations or plain references. Use find_references to see them.",
				notCalls, filtered))
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
