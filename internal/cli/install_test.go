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
	spec := agentServerSpec{Name: "cortex", Command: "cortex", Args: []string{"serve", "--project", "/repo"}}

	out, err := mergeMCPConfig(nil, "cortex", spec)
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
	if _, ok := servers["cortex"]; !ok {
		t.Fatalf("cortex entry missing from %s", out)
	}
}

func TestMergeMCPConfigPreservesUnrelatedServers(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other":{"command":"other-server"}},"theme":"dark"}`)
	spec := agentServerSpec{Name: "cortex", Command: "cortex", Args: []string{"serve"}}

	out, err := mergeMCPConfig(existing, "cortex", spec)
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
	if _, ok := servers["cortex"]; !ok {
		t.Errorf("cortex was not added: %s", out)
	}
}

func TestMergeMCPConfigRejectsMalformedJSON(t *testing.T) {
	spec := agentServerSpec{Name: "cortex", Command: "cortex"}

	if _, err := mergeMCPConfig([]byte("{not json"), "cortex", spec); err == nil {
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

func TestIsClientInstalled(t *testing.T) {
	tempHome := t.TempDir()

	// Cursor target
	cursorTarget := mcpTarget{Client: "Cursor", Path: filepath.Join(tempHome, ".cursor", "mcp.json")}

	// Initially neither config nor directory exists
	if isClientInstalled(tempHome, cursorTarget) {
		t.Error("expected client to not be installed before dir exists")
	}

	// Create parent dir
	if err := os.MkdirAll(filepath.Dir(cursorTarget.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isClientInstalled(tempHome, cursorTarget) {
		t.Error("expected client to be detected when parent directory exists")
	}

	// Claude Code target
	claudeTarget := mcpTarget{Client: "Claude Code", Path: filepath.Join(tempHome, ".claude.json")}
	if isClientInstalled(tempHome, claudeTarget) {
		t.Error("expected Claude Code not installed before ~/.claude exists")
	}
	if err := os.MkdirAll(filepath.Join(tempHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isClientInstalled(tempHome, claudeTarget) {
		t.Error("expected Claude Code to be detected when ~/.claude directory exists")
	}
}

func TestInjectAgentGuidance(t *testing.T) {
	root := t.TempDir()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	// Injects into default .cursorrules when no file exists
	if err := injectAgentGuidance(cmd, root, false); err != nil {
		t.Fatalf("injectAgentGuidance: %v", err)
	}
	crPath := filepath.Join(root, ".cursorrules")
	content, err := os.ReadFile(crPath)
	if err != nil {
		t.Fatalf("read created .cursorrules: %v", err)
	}
	if !strings.Contains(string(content), agentGuidanceOpen) {
		t.Fatalf("guidance marker missing from .cursorrules:\n%s", content)
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
