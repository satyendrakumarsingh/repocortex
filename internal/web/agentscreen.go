package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/prune"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

// MCPToolInfo describes one registered MCP tool and its token-reduction capability.
type MCPToolInfo struct {
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Benefit     string `json:"benefit"`
}

// TokenMetrics describes token savings and efficiency metrics for AI agents.
type TokenMetrics struct {
	TotalFilesIndexed int64 `json:"total_files_indexed"`
	TotalSymbols      int64 `json:"total_symbols"`
	TotalEdges        int64 `json:"total_edges"`
}

// PruneDemoSample holds a concrete before-and-after pruning comparison on a repository file.
type PruneDemoSample struct {
	FilePath        string  `json:"file_path"`
	Language        string  `json:"language"`
	OriginalBytes   int     `json:"original_bytes"`
	PrunedBytes     int     `json:"pruned_bytes"`
	OriginalTokens  int     `json:"original_tokens"`
	PrunedTokens    int     `json:"pruned_tokens"`
	TokensSaved     int     `json:"tokens_saved"`
	SavingsPct      float64 `json:"savings_pct"`
	OriginalExcerpt string  `json:"original_excerpt"`
	PrunedExcerpt   string  `json:"pruned_excerpt"`
}

// QueryBenchmark models token economics for a specific type of AI agent query.
type QueryBenchmark struct {
	QueryIntent  string  `json:"query_intent"`
	ToolUsed     string  `json:"tool_used"`
	RawTokens    int     `json:"raw_tokens"`
	PrunedTokens int     `json:"pruned_tokens"`
	TokensSaved  int     `json:"tokens_saved"`
	SavingsPct   float64 `json:"savings_pct"`
	Explanation  string  `json:"explanation"`
}

// AgentPayload is the comprehensive response for GET /api/agent.
type AgentPayload struct {
	Status          string           `json:"status"`
	ProjectName     string           `json:"project_name"`
	RepoRoot        string           `json:"repo_root"`
	TokenMetrics    TokenMetrics     `json:"token_metrics"`
	MCPTools        []MCPToolInfo    `json:"mcp_tools"`
	QueryBenchmarks []QueryBenchmark `json:"query_benchmarks"`
	Health          IndexHealth      `json:"health"`
	Sample          *PruneDemoSample `json:"sample,omitempty"`
	ConfigSnippet   map[string]any   `json:"config_snippet"`
	Guidance        string           `json:"guidance"`
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusOK, AgentPayload{
			Status:   "unindexed",
			Guidance: meta.Help.Detail,
			Health:   meta.Health,
		})
		return
	}

	projectName := s.deps.Config.ProjectName
	if projectName == "" {
		projectName = "Current Project"
	}
	repoRoot := s.deps.Config.RepoRoot

	mcpTools := mcpCatalogue()

	var totalFiles int64 = 0
	var totalSymbols int64 = 0
	var totalEdges int64 = 0

	if run != nil {
		counts, err := s.deps.Registry.Counts(ctx, run.ID)
		if err == nil {
			totalFiles = counts.Files
			totalSymbols = counts.Symbols
			totalEdges = counts.Edges
		}
	}

	// Only counts the index holds. Token savings are not estimated here: a
	// per-file average applied to the file count is a constant, not a
	// measurement. The sample below is measured on a real file in this index.
	tokenMetrics := TokenMetrics{
		TotalFilesIndexed: totalFiles,
		TotalSymbols:      totalSymbols,
		TotalEdges:        totalEdges,
	}

	// Live prune demonstration on a real repository file
	var sample *PruneDemoSample
	if run != nil {
		files, err := s.deps.Registry.Files(ctx, run.ID)
		if err == nil && len(files) > 0 {
			for _, f := range files {
				ext := strings.ToLower(filepath.Ext(f.Path))
				if ext == ".java" || ext == ".go" || ext == ".py" || ext == ".ts" || ext == ".js" {
					// f.Path comes from the index, not the request, but a
					// repository can hold a symlink that lands outside it.
					// With a root configured only relative paths are read,
					// and the read goes through os.Root so a component swapped
					// for a link after validation still cannot escape.
					var (
						src     []byte
						readErr error
					)
					switch {
					case repoRoot == "":
						src, readErr = os.ReadFile(f.Path) //nolint:gosec // no repo root configured; index-supplied path, as before
					case filepath.IsAbs(f.Path):
						continue
					default:
						src, _, readErr = security.ReadFileContained(repoRoot, f.Path)
					}
					if readErr == nil && len(src) > 200 {
						prunedSrc, pruneErr := prune.PruneFile(f.Path, src)
						if pruneErr == nil {
							origB := len(src)
							prunedB := len(prunedSrc)
							origTok := (origB + 3) / 4
							prunedTok := (prunedB + 3) / 4
							savedTok := origTok - prunedTok
							pct := float64(savedTok) / float64(origTok) * 100

							sample = &PruneDemoSample{
								FilePath:        f.Path,
								Language:        strings.TrimPrefix(ext, "."),
								OriginalBytes:   origB,
								PrunedBytes:     prunedB,
								OriginalTokens:  origTok,
								PrunedTokens:    prunedTok,
								TokensSaved:     savedTok,
								SavingsPct:      pct,
								OriginalExcerpt: limitExcerpt(string(src), 24),
								PrunedExcerpt:   limitExcerpt(string(prunedSrc), 24),
							}
							break
						}
					}
				}
			}
		}
	}

	configSnippet := map[string]any{
		"mcpServers": map[string]any{
			"repocortex": map[string]any{
				"command": "repocortex",
				// The same entry `repocortex install` writes: the server is `serve`,
				// pinned to this repository so the client's working directory
				// cannot change which index it reads.
				"args": []string{"--project", repoRoot, "serve"},
			},
		},
	}

	s.writeJSON(w, http.StatusOK, AgentPayload{
		Status:        "active",
		ProjectName:   projectName,
		RepoRoot:      repoRoot,
		TokenMetrics:  tokenMetrics,
		MCPTools:      mcpTools,
		Health:        meta.Health,
		Sample:        sample,
		ConfigSnippet: configSnippet,
		Guidance:      "Connect RepoCortex to your AI agent (Claude Desktop, Cursor, Gemini, Antigravity) through the Model Context Protocol (MCP).",
	})
}

func limitExcerpt(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n// ... [remaining implementation pruned for token brevity]"
}

// mcpCatalogue lists the tools the MCP server registers (internal/mcp), in the
// order the profiles expose them: the default `minimal` profile has the first
// seven, `standard` and `full` have all of them. TestAgentCatalogueMatchesTheMCPServer
// holds this list to the server's own.
func mcpCatalogue() []MCPToolInfo {
	return []MCPToolInfo{
		{
			Name:        "get_task_context",
			Category:    "Task context",
			Description: "Returns the files, symbols, callers and tests relevant to a task in one call.",
			Benefit:     "Replaces a dozen exploratory searches with one answer.",
		},
		{
			Name:        "get_project_overview",
			Category:    "Orientation",
			Description: "Summarises the languages, size, entry points and index coverage of the project.",
			Benefit:     "Gives an agent its bearings before it reads any file.",
		},
		{
			Name:        "search_code",
			Category:    "Search",
			Description: "Searches the indexed code by text, ranked by relevance.",
			Benefit:     "Finds code without walking the directory tree.",
		},
		{
			Name:        "find_symbol",
			Category:    "Lookup",
			Description: "Finds definitions by their name.",
			Benefit:     "Goes straight to a definition instead of grepping for it.",
		},
		{
			Name:        "get_symbol",
			Category:    "Lookup",
			Description: "Returns a symbol's declaration, signature, doc comment and enclosing type.",
			Benefit:     "Returns one declaration instead of a whole file.",
		},
		{
			Name:        "find_references",
			Category:    "Navigation",
			Description: "Lists where a symbol is referenced across files.",
			Benefit:     "Finds every use of a name without matching unrelated text.",
		},
		{
			Name:        "find_callers",
			Category:    "Navigation",
			Description: "Lists the callers of a symbol, with file, line and how sure each link is.",
			Benefit:     "Avoids scanning many files for call sites.",
		},
		{
			Name:        "find_tests",
			Category:    "Change safety",
			Description: "Lists the tests that reach a symbol.",
			Benefit:     "Tells the agent which tests to run.",
		},
		{
			Name:        "analyze_impact",
			Category:    "Change safety",
			Description: "Lists what a change to a symbol can reach, including the endpoints it affects.",
			Benefit:     "Shows what could break before an edit is made.",
		},
		{
			Name:        "get_resolution_coverage",
			Category:    "Index quality",
			Description: "Reports how many references were bound to a definition, per language.",
			Benefit:     "Shows how far an agent can trust the answers for each language.",
		},
		{
			Name:        "plan_rename",
			Category:    "Refactoring",
			Description: "Lists every site a rename would edit, and the ones it cannot defend.",
			Benefit:     "Lets an agent rename across files without missing a site.",
		},
		{
			Name:        "plan_change",
			Category:    "Refactoring",
			Description: "Orders the steps of a change, with the callers to update and the tests that gate it.",
			Benefit:     "Turns a large change into a checked sequence.",
		},
		{
			Name:        "list_api_endpoints",
			Category:    "Cross-stack",
			Description: "Lists HTTP endpoints, their methods and handlers.",
			Benefit:     "Maps a request path to the code that serves it.",
		},
		{
			Name:        "list_db_tables",
			Category:    "Cross-stack",
			Description: "Lists database tables and the code that reads and writes them.",
			Benefit:     "Shows who touches a table before its schema changes.",
		},
		{
			Name:        "get_config_key",
			Category:    "Cross-stack",
			Description: "Reports where a configuration key is declared and read.",
			Benefit:     "Finds every reader of a setting before it is renamed.",
		},
		{
			Name:        "get_file_outline",
			Category:    "Navigation",
			Description: "Returns a file's declarations without their bodies.",
			Benefit:     "Gives the shape of a large file in a fraction of its tokens.",
		},
		{
			Name:        "minimize_diff",
			Category:    "Review",
			Description: "Condenses a diff to its structural changes, dropping import reordering and comment-only edits.",
			Benefit:     "Lets an agent review a large change in far fewer tokens.",
		},
		{
			Name:        "list_blindspots",
			Category:    "Index quality",
			Description: "Lists what the index could not see: unbound calls, unmapped endpoints, unparsed files.",
			Benefit:     "Tells an agent where an empty answer is not proof.",
		},
	}
}
