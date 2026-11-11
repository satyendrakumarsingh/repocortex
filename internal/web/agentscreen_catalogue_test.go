package web

import (
	"slices"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/mcp"
)

// TestAgentCatalogueMatchesTheMCPServer: the agent screen once listed tools the
// server never registered (prune_context, get_coverage, search_symbols, ...).
// The screen must name exactly the server's tools, with the default profile's
// seven first and in the same order.
func TestAgentCatalogueMatchesTheMCPServer(t *testing.T) {
	minimal, standard := mcp.ProfileTools()

	var names []string
	for _, tool := range mcpCatalogue() {
		names = append(names, tool.Name)
	}

	if len(names) < len(minimal) || !slices.Equal(names[:len(minimal)], minimal) {
		t.Errorf("the catalogue does not open with the minimal profile's tools in order:\n got  %v\n want %v", names, minimal)
	}
	got, want := slices.Clone(names), slices.Clone(standard)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the catalogue names a different set of tools from the server:\n got  %v\n want %v", got, want)
	}
}
