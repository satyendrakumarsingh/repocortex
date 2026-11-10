package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"

	"github.com/spf13/cobra"
)

// runAgent executes the agent command against an isolated project root.
//
// It builds its own root rather than using the shared run() helper because
// NewRoot does not expose the *Global it owns, and this command needs one that
// PersistentPreRunE has resolved. Replace this with run() once root.go
// registers newAgentCmd.
func runAgent(t *testing.T, projectRoot string, args ...string) (string, error) {
	t.Helper()

	g := &Global{}
	root := &cobra.Command{
		Use:               "cortex",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return g.setup(cmd) },
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "")
	pf.StringVar(&g.Project, "project", "", "")
	root.AddCommand(newAgentCmd(g))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--project", projectRoot, "--quiet", "agent"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func agentRepoFile(t *testing.T, rel string) string {
	t.Helper()
	// Tests run with the package directory as the working directory.
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// ------------------------------------------------------------------ drift

// TestAgentGuidanceNamesOnlyRealTools stops the guidance from telling an agent
// to call something that does not exist.
//
// A rule naming a tool the server never registers is worse than no rule: the
// agent tries it, the call fails, and per the isError policy in
// internal/mcp/server.go a failed call teaches it to stop using the toolset.
func TestAgentGuidanceNamesOnlyRealTools(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	db, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.Migrate(ctx, db.DB, time.Now().Unix); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The widest profile, because the guidance is allowed to mention a tool
	// that only appears on 'standard' as long as it says so — and it does.
	srv, err := devmcp.New(devmcp.Deps{
		Registry: registry.New(db),
		Graph:    graph.New(db),
		Config:   config.Config{MCPProfile: config.MCPFull},
	})
	if err != nil {
		t.Fatalf("build mcp server: %v", err)
	}
	exposed := make(map[string]bool, len(srv.Tools()))
	for _, name := range srv.Tools() {
		exposed[name] = true
	}

	cited := []string{
		"get_task_context", "impact_analysis", "find_callers",
		"find_tests", "find_symbol", "search_code",
	}
	for _, name := range cited {
		if !strings.Contains(agentGuidance, name) {
			t.Errorf("guidance no longer mentions %s; drop it from this list or restore the rule", name)
			continue
		}
		if !exposed[name] {
			t.Errorf("guidance tells the agent to call %q, which no profile registers", name)
		}
	}
}

// TestAgentGuidanceStaysShort guards the one property that makes guidance work.
//
// An agent skims project memory. A block that grows into a page stops being
// read, and the failure is silent — the text is still there, it is simply
// ignored. The ceiling is arbitrary and generous; what matters is that adding a
// rule now costs a decision about which rule to drop.
func TestAgentGuidanceStaysShort(t *testing.T) {
	const maxLines = 40
	if n := strings.Count(agentGuidance, "\n"); n > maxLines {
		t.Errorf("guidance is %d lines, ceiling is %d — cut a rule rather than raising it", n, maxLines)
	}
	if !strings.HasPrefix(agentGuidance, agentGuidanceOpen) {
		t.Error("guidance must open with the marker --check looks for")
	}
	if !strings.HasSuffix(agentGuidance, agentGuidanceClose+"\n") {
		t.Error("guidance must close with its marker and a trailing newline")
	}
}

// ------------------------------------------------------------------ print

func TestAgentPrintsRegistrationAndGuidance(t *testing.T) {
	root := isolate(t)

	out, err := runAgent(t, root)
	if err != nil {
		t.Fatalf("agent: %v\n%s", err, out)
	}
	for _, want := range []string{
		"claude mcp add cortex -- cortex --project " + root + " serve",
		"claude --plugin-dir " + root,
		"stdio",
		agentGuidanceOpen,
		agentGuidanceClose,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s", want, out)
		}
	}
}

func TestAgentGuidanceFlagPrintsBlockAlone(t *testing.T) {
	root := isolate(t)

	out, err := runAgent(t, root, "--guidance")
	if err != nil {
		t.Fatalf("agent --guidance: %v\n%s", err, out)
	}
	// Byte-exact. This output is appended to a user's CLAUDE.md, and a banner
	// or a trailing hint would become permanent litter in it.
	if out != agentGuidance {
		t.Errorf("--guidance must emit the block and nothing else, got:\n%s", out)
	}
}

func TestAgentJSONIsMachineReadable(t *testing.T) {
	root := isolate(t)

	out, err := runAgent(t, root, "--json")
	if err != nil {
		t.Fatalf("agent --json: %v\n%s", err, out)
	}
	var got agentIntegration
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Server.Command != "cortex" || len(got.Server.Args) == 0 {
		t.Errorf("server spec is unusable: %+v", got.Server)
	}
	if got.Guidance != agentGuidance {
		t.Error("JSON guidance differs from the printed block")
	}
}

// ------------------------------------------------------------------ write

func TestAgentWriteCreatesFile(t *testing.T) {
	root := isolate(t)
	target := filepath.Join(root, "nested", "GUIDANCE.md")

	out, err := runAgent(t, root, "--write", target)
	if err != nil {
		t.Fatalf("agent --write: %v\n%s", err, out)
	}
	body, err := os.ReadFile(target) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(body) != agentGuidance {
		t.Error("written file is not the guidance block")
	}
}

// TestAgentWriteRefusesToClobber is the contract that keeps this command safe
// to run: writing to a user's machine is his decision, and losing a file he
// wrote is not a recoverable one.
func TestAgentWriteRefusesToClobber(t *testing.T) {
	root := isolate(t)
	target := filepath.Join(root, "CLAUDE.md")
	const existing = "# my own notes\n"
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAgent(t, root, "--write", target)
	if err == nil {
		t.Fatalf("--write must refuse an existing file, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), ">>") {
		t.Errorf("the refusal must name both ways forward, got: %v", err)
	}
	body, _ := os.ReadFile(target) //nolint:gosec // test-owned temp path
	if string(body) != existing {
		t.Error("a refused write must leave the file untouched")
	}
}

// TestAgentWriteForceAnnouncesTheLoss — --force may destroy, but never quietly.
func TestAgentWriteForceAnnouncesTheLoss(t *testing.T) {
	root := isolate(t)
	target := filepath.Join(root, "CLAUDE.md")
	if err := os.WriteFile(target, []byte("# my own notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAgent(t, root, "--write", target, "--force")
	if err != nil {
		t.Fatalf("--write --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "replacing") || !strings.Contains(out, "discarded") {
		t.Errorf("--force must say what it destroyed, got:\n%s", out)
	}
	body, _ := os.ReadFile(target) //nolint:gosec // test-owned temp path
	if string(body) != agentGuidance {
		t.Error("--force should have replaced the file")
	}
}

func TestAgentRejectsContradictoryFlags(t *testing.T) {
	root := isolate(t)
	cases := [][]string{
		{"--check", "--guidance"},
		{"--check", "--write", filepath.Join(root, "x.md")},
		{"--guidance", "--write", filepath.Join(root, "x.md")},
		{"--force"},
	}
	for _, args := range cases {
		if out, err := runAgent(t, root, args...); err == nil {
			t.Errorf("agent %v should be rejected, got:\n%s", args, out)
		}
	}
}

// ------------------------------------------------------------------ check

// TestAgentCheckOnBareProjectDoesNotFail encodes the same judgement as the
// isError policy in internal/mcp/server.go: an un-integrated project is the
// normal starting state, and a setup command that exits non-zero on it reads as
// a broken tool rather than as a to-do list.
func TestAgentCheckOnBareProjectDoesNotFail(t *testing.T) {
	root := isolate(t)

	out, err := runAgent(t, root, "--check")
	if err != nil {
		t.Fatalf("--check on an unwired project must not fail: %v\n%s", err, out)
	}
	for _, want := range []string{"index", "mcp registration", "guidance", "plugin manifest"} {
		if !strings.Contains(out, want) {
			t.Errorf("--check never mentions %q\n---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "cortex agent --guidance >> CLAUDE.md") {
		t.Errorf("--check must name the remedy for missing guidance\n---\n%s", out)
	}
}

// TestAgentCheckSeesWiring proves the checks read the real artifacts rather
// than reporting a fixed story.
func TestAgentCheckSeesWiring(t *testing.T) {
	root := isolate(t)

	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"),
		[]byte("# project\n\n"+agentGuidance), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"),
		[]byte(`{"mcpServers":{"cortex":{"command":"cortex","args":["serve"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAgent(t, root, "--check", "--json")
	if err != nil {
		t.Fatalf("--check --json: %v\n%s", err, out)
	}
	var doc struct {
		Checks []check `json:"checks"`
		Failed int     `json:"failed"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if doc.Failed != 0 {
		t.Errorf("nothing here is broken, so failed should be 0: %+v", doc.Checks)
	}

	status := map[string]string{}
	for _, c := range doc.Checks {
		status[c.Name] = c.Status
	}
	if status["guidance"] != "ok" {
		t.Errorf("guidance in CLAUDE.md was not detected: %v", status)
	}
	if status["mcp registration"] != "ok" {
		t.Errorf("cortex in .mcp.json was not detected: %v", status)
	}
	// The index really is absent here, and saying otherwise would make the
	// whole report worthless.
	if status["index"] != "warn" {
		t.Errorf("index status = %q, want warn on a project with no index", status["index"])
	}
}

// TestAgentCheckReportsMalformedRegistration — a .mcp.json that exists and does
// not parse registers nothing while looking like it registers something. That
// is the one wiring state worth complaining about.
func TestAgentCheckReportsMalformedRegistration(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runAgent(t, root, "--check")
	if err != nil {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unreadable") {
		t.Errorf("--check should report an unparseable .mcp.json\n---\n%s", out)
	}
}

// TestAgentCheckSeesIndexAndTools runs against a real initialised index, so the
// index, resolution and tool-surface rows are exercised against the registry
// rather than against the absent-index shortcut.
func TestAgentCheckSeesIndexAndTools(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	out, err := runAgent(t, root, "--check")
	if err != nil {
		t.Fatalf("--check: %v\n%s", err, out)
	}
	// init creates the schema and commits nothing, which is exactly the
	// "initialised but never indexed" state.
	if !strings.Contains(out, "never indexed") {
		t.Errorf("--check should distinguish an empty index from a missing one\n---\n%s", out)
	}
	if !strings.Contains(out, "cortex index") {
		t.Errorf("--check must name the command that fixes it\n---\n%s", out)
	}
}
