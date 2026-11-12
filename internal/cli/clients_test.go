package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var testSpec = agentServerSpec{Name: "repocortex", Command: "repocortex", Args: []string{"--project", "/repo", "serve"}}

// isolateHome points every environment variable the client paths read at a
// temporary home, so a test never touches the real one.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"))
	return home
}

func TestMergeJSONKeyUsesEachClientsKeyAndKeepsTheRest(t *testing.T) {
	cases := []struct {
		key   string
		entry func(agentServerSpec) map[string]any
		check func(t *testing.T, e map[string]any)
	}{
		{"mcpServers", stdioEntry, func(t *testing.T, e map[string]any) {
			if e["command"] != "repocortex" {
				t.Errorf("command = %v", e["command"])
			}
		}},
		{"servers", typedStdioEntry, func(t *testing.T, e map[string]any) {
			if e["type"] != "stdio" {
				t.Errorf("VS Code entry needs type stdio, got %v", e["type"])
			}
		}},
		{"context_servers", stdioEntry, nil},
		{"amp.mcpServers", stdioEntry, nil},
		{"mcp", kiloEntry, func(t *testing.T, e map[string]any) {
			cmd, _ := e["command"].([]any)
			if len(cmd) != 4 || cmd[0] != "repocortex" || cmd[3] != "serve" {
				t.Errorf("Kilo command must be one array of command and args, got %v", e["command"])
			}
		}},
		{"mcpServers", copilotCLIEntry, func(t *testing.T, e map[string]any) {
			if e["type"] != "local" || e["tools"] == nil {
				t.Errorf("Copilot CLI entry needs type local and tools, got %v", e)
			}
		}},
	}
	for _, c := range cases {
		existing := []byte(`{"theme":"dark","` + c.key + `":{"other":{"command":"x"}}}`)
		out, err := mergeJSONKey(existing, c.key, "repocortex", c.entry(testSpec))
		if err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("%s: output is not JSON: %v", c.key, err)
		}
		if doc["theme"] != "dark" {
			t.Errorf("%s: an unrelated setting was dropped", c.key)
		}
		servers := doc[c.key].(map[string]any)
		if _, ok := servers["other"]; !ok {
			t.Errorf("%s: another server was dropped", c.key)
		}
		entry, ok := servers["repocortex"].(map[string]any)
		if !ok {
			t.Fatalf("%s: repocortex entry missing: %s", c.key, out)
		}
		if c.check != nil {
			c.check(t, entry)
		}
		again, err := mergeJSONKey(out, c.key, "repocortex", c.entry(testSpec))
		if err != nil || !bytes.Equal(again, out) {
			t.Errorf("%s: a second run changed the file", c.key)
		}
	}
}

func TestMergeJSONKeyRefusesJSONWithComments(t *testing.T) {
	jsonc := []byte("{\n  // my servers\n  \"context_servers\": {}\n}\n")
	if _, err := mergeJSONKey(jsonc, "context_servers", "repocortex", stdioEntry(testSpec)); err == nil {
		t.Fatal("a file with comments was accepted; it would be rewritten without them")
	}
	if _, err := mergeJSONKey([]byte(`{"mcpServers":[]}`), "mcpServers", "repocortex", stdioEntry(testSpec)); err == nil {
		t.Fatal("a non-object mcpServers was accepted")
	}
}

func TestMergeCodexTOMLAppendsAndReplacesOnlyItsTable(t *testing.T) {
	existing := []byte("model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"other\"\n")
	out := string(mergeCodexTOML(existing, testSpec))
	if !strings.HasPrefix(out, "model = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"other\"\n") {
		t.Fatalf("existing settings changed:\n%s", out)
	}
	want := "[mcp_servers.repocortex]\ncommand = \"repocortex\"\nargs = [\"--project\", \"/repo\", \"serve\"]\n"
	if !strings.Contains(out, want) {
		t.Fatalf("repocortex table missing:\n%s", out)
	}

	// A second run with a new path replaces the table, keeping what follows it.
	withTail := out + "\n[profiles.fast]\nmodel = \"x\"\n"
	moved := testSpec
	moved.Args = []string{"--project", "/elsewhere", "serve"}
	out2 := string(mergeCodexTOML([]byte(withTail), moved))
	if strings.Count(out2, "[mcp_servers.repocortex]") != 1 || !strings.Contains(out2, "/elsewhere") || strings.Contains(out2, "\"/repo\"") {
		t.Fatalf("the repocortex table was not replaced in place:\n%s", out2)
	}
	if !strings.Contains(out2, "[profiles.fast]\nmodel = \"x\"") || !strings.Contains(out2, "[mcp_servers.other]") {
		t.Fatalf("a table after or before repocortex was lost:\n%s", out2)
	}
	if string(mergeCodexTOML([]byte(out2), moved)) != out2 {
		t.Error("a second identical run changed the file")
	}
}

func TestMergeGooseYAMLKeepsOtherExtensionsAndComments(t *testing.T) {
	existing := []byte("# my goose\nGOOSE_PROVIDER: openai\nextensions:\n  developer:\n    enabled: true\n    type: builtin\n")
	out, err := mergeGooseYAML(existing, testSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "# my goose") {
		t.Errorf("a comment was dropped:\n%s", out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["GOOSE_PROVIDER"] != "openai" {
		t.Errorf("an unrelated key was dropped:\n%s", out)
	}
	ext := doc["extensions"].(map[string]any)
	if _, ok := ext["developer"]; !ok {
		t.Errorf("another extension was dropped:\n%s", out)
	}
	c := ext["repocortex"].(map[string]any)
	if c["cmd"] != "repocortex" || c["type"] != "stdio" || c["enabled"] != true {
		t.Errorf("repocortex extension is wrong: %v", c)
	}
	again, err := mergeGooseYAML(out, testSpec)
	if err != nil || !bytes.Equal(again, out) {
		t.Errorf("a second run changed the file:\n%s\n---\n%s", out, again)
	}
	if _, err := mergeGooseYAML([]byte("- a list\n"), testSpec); err == nil {
		t.Error("a config that is not a mapping was accepted")
	}
}

func TestContinueYAMLIsAStandaloneV1File(t *testing.T) {
	out, err := continueYAML(testSpec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema"] != "v1" || doc["name"] == nil || doc["version"] == nil {
		t.Errorf("a standalone Continue file needs name, version and schema v1: %s", out)
	}
	servers := doc["mcpServers"].([]any)
	if s := servers[0].(map[string]any); s["command"] != "repocortex" || s["type"] != "stdio" {
		t.Errorf("server entry is wrong: %v", s)
	}
}

func TestInstallClientsWritesEveryClientAndIsIdempotent(t *testing.T) {
	home := isolateHome(t)
	root := t.TempDir()

	results, err := installClients(home, root, testSpec, true, false)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, r := range results {
		if r.Action == "manual" {
			t.Errorf("%s needed a manual step on a clean home: %s", r.Client, r.Note)
		}
		if r.Action == "created" {
			created++
			if _, err := os.Stat(r.Path); err != nil {
				t.Errorf("%s reported created but %s does not exist", r.Client, r.Path)
			}
			if !strings.HasPrefix(r.Path, home) && !strings.HasPrefix(r.Path, root) {
				t.Errorf("%s wrote outside the test home: %s", r.Client, r.Path)
			}
		}
	}
	if created < 25 {
		t.Errorf("created %d client configs, want every supported client (25 or more)", created)
	}

	again, err := installClients(home, root, testSpec, true, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Action != "unchanged" {
			t.Errorf("second run: %s was %s, want unchanged", r.Client, r.Action)
		}
	}
}

func TestInstallClientsSkipsUndetectedAndSurvivesABadFile(t *testing.T) {
	home := isolateHome(t)
	root := t.TempDir()

	// Cursor is installed with a normal config; Zed has comments RepoCortex
	// must not rewrite.
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	zed := filepath.Join(home, ".config", "zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zed), 0o755); err != nil {
		t.Fatal(err)
	}
	jsonc := "{\n  // theme\n  \"theme\": \"One Dark\"\n}\n"
	if err := os.WriteFile(zed, []byte(jsonc), 0o600); err != nil {
		t.Fatal(err)
	}

	results, err := installClients(home, root, testSpec, false, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]installResult{}
	for _, r := range results {
		byName[r.Client] = r
	}
	if byName["Cursor"].Action != "created" {
		t.Errorf("Cursor = %+v, want created", byName["Cursor"])
	}
	if z, ok := byName["Zed"]; ok {
		if z.Action != "manual" {
			t.Errorf("Zed with comments = %+v, want a manual step", z)
		}
		got, _ := os.ReadFile(zed)
		if string(got) != jsonc {
			t.Error("Zed's commented settings were rewritten")
		}
	}
	if byName["Goose"].Action != "skipped" {
		t.Errorf("an undetected client was written: %+v", byName["Goose"])
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	home := isolateHome(t)
	root := t.TempDir()
	if _, err := installClients(home, root, testSpec, true, true); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a dry run wrote into the home directory: %v", entries)
	}
}

func TestEverySupportedClientHasDocs(t *testing.T) {
	for _, c := range mcpClients("/h", "/r") {
		if c.Docs == "" || c.Path == "" {
			t.Errorf("%s needs a documented path and a docs link", c.Name)
		}
		if c.Format == fmtJSONMap && (c.Key == "" || c.Entry == nil) {
			t.Errorf("%s is a JSON client without a key or entry shape", c.Name)
		}
	}
	for _, m := range manualClients() {
		if m.Docs == "" || m.Step(testSpec) == "" {
			t.Errorf("%s needs a docs link and a manual step", m.Name)
		}
	}
}

func TestRulesUpdateExistingAgentFilesAndWriteCLIGuidanceForAider(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"CLAUDE.md", "GEMINI.md", "CONVENTIONS.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("# notes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := injectAgentGuidance(cmd, root, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		b, _ := os.ReadFile(filepath.Join(root, name))
		if !strings.Contains(string(b), agentGuidanceOpen) {
			t.Errorf("%s has no guidance", name)
		}
	}
	conv, _ := os.ReadFile(filepath.Join(root, "CONVENTIONS.md"))
	if !strings.Contains(string(conv), cliGuidanceOpen) || !strings.HasPrefix(string(conv), "# notes\n") {
		t.Errorf("CONVENTIONS.md should keep its content and gain the command-line block:\n%s", conv)
	}
}

func TestShellWordRoundTripsThroughAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting")
	}
	for _, in := range []string{"plain", "/a b/c", "it's", `{"x":["--project","/o'k"]}`, "", "$HOME `x`"} {
		out, err := exec.Command("sh", "-c", "printf %s "+shellWord(in)).Output()
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("shellWord(%q) came back as %q", in, out)
		}
	}
	if got := shellWord("/usr/bin/repocortex"); got != "/usr/bin/repocortex" {
		t.Errorf("a safe word should stay unquoted, got %s", got)
	}
}

func TestManualStepsQuoteAProjectPathWithSpaces(t *testing.T) {
	spec := agentServerSpec{Name: "repocortex", Command: "repocortex", Args: []string{"--project", "/My Repos/it's here", "serve"}}
	for _, m := range manualClients() {
		step := m.Step(spec)
		if strings.Contains(step, " /My Repos/") {
			t.Errorf("%s: path with spaces is not quoted: %s", m.Name, step)
		}
	}
}
