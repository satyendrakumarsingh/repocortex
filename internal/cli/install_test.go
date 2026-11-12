package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMergeMCPConfigAddsServerToEmptyFile(t *testing.T) {
	spec := agentServerSpec{Name: "repocortex", Command: "repocortex", Args: []string{"serve", "--project", "/repo"}}

	out, err := mergeMCPConfig(nil, "repocortex", spec)
	if err != nil {
		t.Fatalf("mergeMCPConfig: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing from %s", out)
	}
	if _, ok := servers["repocortex"]; !ok {
		t.Fatalf("repocortex entry missing from %s", out)
	}
}

func TestMergeMCPConfigPreservesUnrelatedServers(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other":{"command":"other-server"}},"theme":"dark"}`)
	spec := agentServerSpec{Name: "repocortex", Command: "repocortex", Args: []string{"serve"}}

	out, err := mergeMCPConfig(existing, "repocortex", spec)
	if err != nil {
		t.Fatalf("mergeMCPConfig: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got["theme"] != "dark" {
		t.Errorf("unrelated top-level key was dropped: %s", out)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("unrelated server was dropped: %s", out)
	}
	if _, ok := servers["repocortex"]; !ok {
		t.Errorf("repocortex was not added: %s", out)
	}
}

func TestMergeMCPConfigRejectsMalformedJSON(t *testing.T) {
	spec := agentServerSpec{Name: "repocortex", Command: "repocortex"}

	if _, err := mergeMCPConfig([]byte("{not json"), "repocortex", spec); err == nil {
		t.Fatal("expected an error on malformed existing config, got nil")
	}
}

func TestClaudeDesktopPath(t *testing.T) {
	fakeHome := filepath.Join(string(filepath.Separator), "fake", "home")
	got := claudeDesktopPath(fakeHome)
	if got == "" {
		t.Fatal("claudeDesktopPath returned empty string")
	}
	switch runtime.GOOS {
	case "windows":
		if !strings.Contains(got, "Claude") || !strings.HasSuffix(got, "claude_desktop_config.json") {
			t.Errorf("unexpected windows path: %s", got)
		}
	case "darwin":
		want := filepath.Join(fakeHome, "Library", "Application Support", "Claude", "claude_desktop_config.json")
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	default:
		if !strings.Contains(got, "Claude") || !strings.HasSuffix(got, "claude_desktop_config.json") {
			t.Errorf("unexpected unix path: %s", got)
		}
	}
}

func TestInjectAgentGuidance(t *testing.T) {
	root := t.TempDir()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	// Creates AGENTS.md, which most agents read, when no file exists
	if err := injectAgentGuidance(cmd, root, false); err != nil {
		t.Fatalf("injectAgentGuidance: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read created AGENTS.md: %v", err)
	}
	if !strings.Contains(string(content), agentGuidanceOpen) {
		t.Fatalf("guidance marker missing from AGENTS.md:\n%s", content)
	}
	// It does not scatter a file per agent into the project.
	for _, name := range []string{".cursorrules", "CLAUDE.md", "GEMINI.md"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Errorf("%s was created; only files that already exist are updated", name)
		}
	}

	// Re-running does not duplicate
	out.Reset()
	if err := injectAgentGuidance(cmd, root, false); err != nil {
		t.Fatalf("re-inject guidance: %v", err)
	}
	if !strings.Contains(out.String(), "already present") {
		t.Errorf("expected 'already present', got %s", out.String())
	}
}
