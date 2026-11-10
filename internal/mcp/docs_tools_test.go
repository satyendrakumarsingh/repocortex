package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUserGuideNamesOnlyRealTools: the guide listed tools the server never
// registered (get_blast_radius, get_event_topology, ...), so an agent following
// it called names that do not exist. Every backticked snake_case name in the
// guide's MCP section, and in the server's own guidance notes, must be a tool.
func TestUserGuideNamesOnlyRealTools(t *testing.T) {
	real := toolDescriptions()

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "USER_GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(raw)
	start := strings.Index(guide, "### Available MCP Tools")
	if start < 0 {
		t.Fatal("USER_GUIDE.md has no 'Available MCP Tools' section")
	}
	section := guide[start:]
	if end := strings.Index(section, "\n---"); end > 0 {
		section = section[:end]
	}

	for _, m := range regexp.MustCompile("`([a-z]+_[a-z_]+)`").FindAllStringSubmatch(section, -1) {
		if _, ok := real[m[1]]; !ok {
			t.Errorf("USER_GUIDE.md names MCP tool %q, which the server does not register", m[1])
		}
	}

	src, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?:Use|Call) '([a-z]+_[a-z_]+)'`).FindAllStringSubmatch(string(src), -1) {
		if _, ok := real[m[1]]; !ok {
			t.Errorf("tools.go guidance points agents at %q, which the server does not register", m[1])
		}
	}
}
