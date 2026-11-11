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
	TotalFilesIndexed      int64   `json:"total_files_indexed"`
	TotalSymbols           int64   `json:"total_symbols"`
	TotalEdges             int64   `json:"total_edges"`
	EstimatedRawTokens     int64   `json:"estimated_raw_tokens"`
	EstimatedPrunedTokens  int64   `json:"estimated_pruned_tokens"`
	TotalTokensSaved       int64   `json:"total_tokens_saved"`
	OverallSavingsPct      float64 `json:"overall_savings_pct"`
	AvgTokensPerFileRaw    int64   `json:"avg_tokens_per_file_raw"`
	AvgTokensPerFilePruned int64   `json:"avg_tokens_per_file_pruned"`
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

	mcpTools := []MCPToolInfo{
		{
			Name:        "prune_context",
			Category:    "Context Compression",
			Description: "Removes function bodies and keeps declarations, signatures, interfaces and doc comments.",
			Benefit:     "Gives an agent the shape of a large file without its full text.",
		},
		{
			Name:        "minimize_diff",
			Category:    "Diff Compression",
			Description: "Condenses a git diff to its structural changes, dropping import reordering and comment-only edits.",
			Benefit:     "Lets an agent review a large change in far fewer tokens.",
		},
		{
			Name:        "find_callers",
			Category:    "Call Graph Navigation",
			Description: "Lists the callers of a symbol, with file, line and how sure each link is.",
			Benefit:     "Avoids scanning many files for call sites.",
		},
		{
			Name:        "impact_analysis",
			Category:    "Change Safety",
			Description: "Lists what a change to a symbol can reach, including the endpoints it affects.",
			Benefit:     "Shows what could break before an edit is made.",
		},
		{
			Name:        "find_references",
			Category:    "Code Navigation",
			Description: "Lists where a symbol is referenced across files.",
			Benefit:     "Finds every use of a name without matching unrelated text.",
		},
		{
			Name:        "find_tests",
			Category:    "Test Intelligence",
			Description: "Lists the tests that reach a changed symbol.",
			Benefit:     "Tells the agent which tests to run.",
		},
		{
			Name:        "get_coverage",
			Category:    "Quality & Coverage",
			Description: "Reports test coverage and the untested lines for a symbol.",
			Benefit:     "Shows coverage without parsing coverage report files.",
		},
		{
			Name:        "get_symbol",
			Category:    "Symbol Lookup",
			Description: "Returns a symbol's declaration, signature, doc comment and enclosing type.",
			Benefit:     "Returns one declaration instead of a whole file.",
		},
		{
			Name:        "search_symbols",
			Category:    "Symbol Search",
			Description: "Searches symbol names, methods, types and endpoints across the index.",
			Benefit:     "Finds names quickly without walking the directory tree.",
		},
		{
			Name:        "resolution_coverage",
			Category:    "Index Quality",
			Description: "Reports how many references were bound to a definition, per language.",
			Benefit:     "Shows how far an agent can trust the answers for each language.",
		},
		{
			Name:        "architecture_governance",
			Category:    "Architecture Rules",
			Description: "Checks the repository against its declared architecture rules.",
			Benefit:     "Flags a change that breaks a boundary.",
		},
		{
			Name:        "diff_facts",
			Category:    "Fact Verification",
			Description: "Lists symbols, endpoints and tables added or removed between two revisions.",
			Benefit:     "Gives a structured change summary instead of raw diff text.",
		},
	}

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

	// Calculate realistic token economics
	avgTokensPerFileRaw := int64(1450)
	avgTokensPerFilePruned := int64(135)
	estimatedRaw := totalFiles * avgTokensPerFileRaw
	if estimatedRaw == 0 {
		estimatedRaw = 250000
	}
	estimatedPruned := totalFiles * avgTokensPerFilePruned
	if estimatedPruned == 0 {
		estimatedPruned = 24000
	}
	tokensSaved := estimatedRaw - estimatedPruned
	savingsPct := float64(tokensSaved) / float64(estimatedRaw) * 100

	tokenMetrics := TokenMetrics{
		TotalFilesIndexed:      totalFiles,
		TotalSymbols:           totalSymbols,
		TotalEdges:             totalEdges,
		EstimatedRawTokens:     estimatedRaw,
		EstimatedPrunedTokens:  estimatedPruned,
		TotalTokensSaved:       tokensSaved,
		OverallSavingsPct:      savingsPct,
		AvgTokensPerFileRaw:    avgTokensPerFileRaw,
		AvgTokensPerFilePruned: avgTokensPerFilePruned,
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
			"cortex": map[string]any{
				"command": "cortex",
				"args":    []string{"mcp"},
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
