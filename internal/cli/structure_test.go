package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"

	"github.com/spf13/cobra"
)

// Tests for the commands that front the full-profile tools, for `repocortex tool`,
// and for the help layout and naming rules.
//
// Like query_test.go these drive the whole stack through the CLI. The property
// most worth protecting here is PARITY: `repocortex tool call X` must print what an
// MCP client receives from X, because that sentence is the feature.

// runWithStdin is run with standard input supplied.
func runWithStdin(t *testing.T, projectRoot, stdin string, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--project", projectRoot, "--quiet"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// ---------------------------------------------------------------- tool

func TestToolListNamesEveryToolWithItsProfileAndCost(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "tool", "list")
	if err != nil {
		t.Fatalf("tool list: %v\n%s", err, out)
	}
	for _, want := range []string{"get_task_context", "analyze_impact", "list_dead_code", "minimal", "standard", "full", "TOKENS"} {
		if !strings.Contains(out, want) {
			t.Errorf("tool list is missing %q:\n%s", want, out)
		}
	}

	raw, err := run(t, root, "--json", "tool", "list", "--profile", "minimal")
	if err != nil {
		t.Fatalf("tool list --json: %v\n%s", err, raw)
	}
	var got struct {
		Profile string `json:"profile"`
		Tools   []struct {
			Name            string `json:"name"`
			Profile         string `json:"profile"`
			EstimatedTokens int    `json:"estimated_tokens"`
		} `json:"tools"`
		Total int `json:"total_estimated_tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("tool list --json is not JSON: %v\n%s", err, raw)
	}
	minimal, _ := devmcp.ProfileTools()
	if len(got.Tools) != len(minimal) {
		t.Errorf("minimal profile lists %d tools, want %d", len(got.Tools), len(minimal))
	}
	sum := 0
	for _, tool := range got.Tools {
		if tool.Profile != "minimal" {
			t.Errorf("%s is labelled %q on the minimal profile", tool.Name, tool.Profile)
		}
		sum += tool.EstimatedTokens
	}
	if sum != got.Total || got.Total == 0 {
		t.Errorf("total %d does not equal the sum of its rows %d", got.Total, sum)
	}
}

func TestToolListRejectsAnUnknownProfile(t *testing.T) {
	root := queryFixture(t)
	if _, err := run(t, root, "tool", "list", "--profile", "everything"); err == nil {
		t.Error("an unknown profile must be rejected")
	}
}

func TestToolDescribeShowsArgumentsRequiredFirst(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "tool", "describe", "trace_call_path")
	if err != nil {
		t.Fatalf("tool describe: %v\n%s", err, out)
	}
	from, depth := strings.Index(out, "from (string, required)"), strings.Index(out, "max_depth (")
	if from < 0 || depth < 0 || from > depth {
		t.Errorf("required arguments should be listed before optional ones:\n%s", out)
	}

}

// The feature in one assertion: the shell gets the answer an MCP client gets.
func TestToolCallPrintsWhatAnMCPClientReceives(t *testing.T) {
	root := queryFixture(t)

	out, err := run(t, root, "tool", "call", "find_callers", "name=Save")
	if err != nil {
		t.Fatalf("tool call: %v\n%s", err, out)
	}
	for _, want := range []string{"=== SYSTEM METADATA ===", "Settle", "Refund", "index_revision"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer is missing %q:\n%s", want, out)
		}
	}

	viaJSON, err := run(t, root, "tool", "call", "find_callers", "--args", `{"name":"Save"}`)
	if err != nil {
		t.Fatalf("tool call --args: %v\n%s", err, viaJSON)
	}
	if stripAge(viaJSON) != stripAge(out) {
		t.Errorf("key=value and --args produced different answers:\n--- pairs\n%s\n--- json\n%s", out, viaJSON)
	}
}

var ageLine = regexp.MustCompile(`(?m)^.*\bago\b.*$`)

// stripAge drops lines stating the index's age, which tick between two runs.
func stripAge(s string) string { return ageLine.ReplaceAllString(s, "") }

func TestToolCallJSONCarriesTextAndStructuredResult(t *testing.T) {
	root := queryFixture(t)
	raw, err := run(t, root, "--json", "tool", "call", "find_callers", "name=Save")
	if err != nil {
		t.Fatalf("tool call --json: %v\n%s", err, raw)
	}
	var got struct {
		Tool    string          `json:"tool"`
		Text    string          `json:"text"`
		Result  json.RawMessage `json:"result"`
		IsError bool            `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if got.Tool != "find_callers" || got.IsError || !strings.Contains(got.Text, "Settle") {
		t.Errorf("unexpected envelope: %+v", got)
	}
	var result struct {
		Summary string `json:"summary"`
		Items   []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.Result, &result); err != nil || len(result.Items) == 0 {
		t.Errorf("structured result is missing or empty: %v\n%s", err, got.Result)
	}
}

// A tool reporting failure must fail the command, so a script can branch on it,
// and must not print a second "error:" under the tool's own explanation.
func TestToolCallExitsNonZeroWhenTheToolReportsFailure(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "tool", "call", "find_callers", "name=")
	if err == nil {
		t.Fatalf("an invalid call must fail the command:\n%s", out)
	}
	var ec *ExitCodeError
	if !asExitCode(err, &ec) || ec.Code != 1 || !ec.Silent {
		t.Errorf("want a silent exit code 1, got %#v", err)
	}
	if !strings.Contains(out, "name must not be empty") {
		t.Errorf("the tool's own explanation should be printed:\n%s", out)
	}
}

func asExitCode(err error, target **ExitCodeError) bool {
	ec, ok := err.(*ExitCodeError) //nolint:errorlint // the command returns this type directly
	if ok {
		*target = ec
	}
	return ok
}

// "Nothing found" is an answer. It must not fail the command.
func TestToolCallNothingFoundIsNotAFailure(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "tool", "call", "find_symbol", "name=NoSuchSymbolAnywhere")
	if err != nil {
		t.Fatalf("an empty answer must exit 0: %v\n%s", err, out)
	}
}

func TestToolCallUnknownToolSuggestsTheRightOne(t *testing.T) {
	root := queryFixture(t)
	_, err := run(t, root, "tool", "call", "callers", "name=Save")
	if err == nil || !strings.Contains(err.Error(), "find_callers") {
		t.Errorf("an unknown tool should suggest near matches, got %v", err)
	}
	// A tool outside the chosen profile is unknown on that profile.
	_, err = run(t, root, "tool", "call", "list_dead_code", "--profile", "minimal")
	if err == nil || !strings.Contains(err.Error(), `"minimal" profile`) {
		t.Errorf("a full-only tool must not be callable on minimal, got %v", err)
	}
}

func TestToolCallReadsArgumentsFromFilesAndStdin(t *testing.T) {
	root := queryFixture(t)
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"

	fromStdin, err := runWithStdin(t, root, diff, "tool", "call", "minimize_diff", "diff=@-")
	if err != nil {
		t.Fatalf("diff=@- : %v\n%s", err, fromStdin)
	}
	if !strings.Contains(fromStdin, "x.go") {
		t.Errorf("the diff from stdin did not reach the tool:\n%s", fromStdin)
	}

	path := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(path, []byte(`{"name":"Save"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := run(t, root, "tool", "call", "find_callers", "--args-file", path)
	if err != nil || !strings.Contains(fromFile, "Settle") {
		t.Errorf("--args-file did not reach the tool: %v\n%s", err, fromFile)
	}

	if _, err := run(t, root, "tool", "call", "find_callers", "--args", `{"name":"Save"}`, "--args-file", path); err == nil {
		t.Error("--args and --args-file together must be rejected")
	}
}

func TestParseToolArgTypes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		key  string
		want any
	}{
		{"name=ParseConfig", "name", "ParseConfig"},
		{"limit=10", "limit", int64(10)},
		{"include_exported=true", "include_exported", true},
		{"include_tests=false", "include_tests", false},
		{"path=", "path", ""},
		{"name=a=b", "name", "a=b"},     // only the first = splits
		{"name=007x", "name", "007x"},   // not a number
		{"version=1.5", "version", 1.5}, // a float
		{"query=3.x", "query", "3.x"},   // not a float
		{`tags:=["a","b"]`, "tags", []any{"a", "b"}},
		{"limit:=5", "limit", float64(5)},
	} {
		k, v, err := parseToolArg(tc.in, strings.NewReader(""))
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if k != tc.key || !jsonEqual(v, tc.want) {
			t.Errorf("%q = (%q, %#v), want (%q, %#v)", tc.in, k, v, tc.key, tc.want)
		}
	}
	for _, bad := range []string{"noequals", "=value", "Bad-Key=1", "key:=not json"} {
		if _, _, err := parseToolArg(bad, strings.NewReader("")); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestToolCallWritesTheAuditLog(t *testing.T) {
	root := queryFixture(t)
	logPath := filepath.Join(t.TempDir(), "audit", "calls.jsonl")

	if out, err := run(t, root, "tool", "call", "find_callers", "name=Save", "--audit-log", logPath); err != nil {
		t.Fatalf("tool call: %v\n%s", err, out)
	}
	data, err := os.ReadFile(logPath) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatalf("the audit log was not written: %v", err)
	}
	var ev devmcp.AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(data), &ev); err != nil {
		t.Fatalf("audit line is not JSON: %v\n%s", err, data)
	}
	if ev.Tool != "find_callers" || ev.Transport != "cli" || ev.Outcome != "ok" {
		t.Errorf("audit event = %+v", ev)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no owner/group/other bits: Go reports 0666 for any writable
	// file there, and access is governed by the directory's ACL instead.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 {
		t.Errorf("audit log is readable beyond its owner: %o", perm)
	}
}

func TestAuditLogEnvironmentVariableIsHonoured(t *testing.T) {
	root := queryFixture(t)
	logPath := filepath.Join(t.TempDir(), "env-audit.jsonl")
	t.Setenv(auditLogEnv, logPath)

	if out, err := run(t, root, "tool", "call", "get_project_overview"); err != nil {
		t.Fatalf("tool call: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(logPath); err != nil || !bytes.Contains(data, []byte("get_project_overview")) { //nolint:gosec // test path
		t.Errorf("%s did not turn the audit log on: %v\n%s", auditLogEnv, err, data)
	}
}

// ---------------------------------------------------------------- dead-code

func TestDeadCodeListsOnlyLikelyByDefault(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"app/app.go": `package app

func Run() { used() }

func used() {}

// orphan is never called and never named anywhere else.
func orphan() int {
	return 1
}

// passedByValue is never called, but it is handed to register below.
func passedByValue() {}

func setup() { register(passedByValue) }

func register(f func()) { f() }
`,
		"app/main.go": `package app

func init() { setup() }
`,
	})
	indexProject(t, root)

	out, err := run(t, root, "dead-code")
	if err != nil {
		t.Fatalf("dead-code: %v\n%s", err, out)
	}
	if !strings.Contains(out, "orphan") {
		t.Errorf("the uncalled, unmentioned function should be listed:\n%s", out)
	}
	if strings.Contains(out, "likely     function  passedByValue") || rowFor(out, "passedByValue") != "" {
		t.Errorf("a function passed as a value must not be listed as likely unused:\n%s", out)
	}
	if rowFor(out, "used") != "" {
		t.Errorf("a called function was listed:\n%s", out)
	}
	if !strings.Contains(out, "not proven unused") {
		t.Errorf("the warning is missing:\n%s", out)
	}

	all, err := run(t, root, "dead-code", "--all")
	if err != nil {
		t.Fatalf("dead-code --all: %v\n%s", err, all)
	}
	row := rowFor(all, "passedByValue")
	if !strings.HasPrefix(row, "possible") || !strings.Contains(row, "name appears elsewhere") {
		t.Errorf("--all should list it as possible, with the reason:\n%s", all)
	}
}

// rowFor returns the table row whose SYMBOL column is exactly name, or "".
func rowFor(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == name && (fields[0] == "likely" || fields[0] == "possible") {
			return line
		}
	}
	return ""
}

func TestDeadCodeJSONIsTheReport(t *testing.T) {
	root := queryFixture(t)
	raw, err := run(t, root, "--json", "dead-code", "--all")
	if err != nil {
		t.Fatalf("dead-code --json: %v\n%s", err, raw)
	}
	var rep struct {
		Candidates []struct {
			Name      string   `json:"name"`
			Certainty string   `json:"certainty"`
			Reasons   []string `json:"reasons"`
		} `json:"candidates"`
		Completeness struct {
			LowerBound bool     `json:"lower_bound"`
			Caveats    []string `json:"caveats"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if !rep.Completeness.LowerBound || len(rep.Completeness.Caveats) == 0 {
		t.Errorf("the JSON report must carry its completeness block: %+v", rep.Completeness)
	}
}

// ---------------------------------------------------------------- hotspots

func TestHotspotsRanksFromGitHistory(t *testing.T) {
	root := whoFixture(t)
	out, err := run(t, root, "hotspots")
	if err != nil {
		t.Fatalf("hotspots: %v\n%s", err, out)
	}
	// payment.go was committed twice, the other two files once each.
	lines := strings.Split(out, "\n")
	first := ""
	for _, l := range lines {
		if strings.Contains(l, "svc/") {
			first = l
			break
		}
	}
	if !strings.Contains(first, "svc/payment.go") || !strings.HasPrefix(strings.TrimSpace(first), "100") {
		t.Errorf("svc/payment.go should rank first with score 100:\n%s", out)
	}
	if !strings.Contains(out, "4 commit(s)") {
		t.Errorf("the header should say how much history was read:\n%s", out)
	}
}

func TestHotspotsWithoutARepositoryExplainsItself(t *testing.T) {
	root := queryFixture(t) // indexed, but not a git repository
	out, err := run(t, root, "hotspots")
	if err != nil {
		t.Fatalf("no history is an answer, not an error: %v\n%s", err, out)
	}
	// The message is wrapped to the terminal width and names the temporary
	// directory, whose length differs by platform, so the phrase can be split
	// across a line break. Compare with the wrapping removed.
	flat := strings.Join(strings.Fields(out), " ")
	if !strings.Contains(flat, "No hotspots to rank") || !strings.Contains(flat, "not a git repository") {
		t.Errorf("the output should say why nothing was ranked:\n%s", out)
	}
}

// ---------------------------------------------------------------- analyze_change

// A reviewer's question arrives as a range of commits, not as a symbol. The
// tool must turn the range into the symbols it touched and what they reach.
func TestAnalyzeChangeReadsACommitRange(t *testing.T) {
	root := whoFixture(t) // four commits; the last touches svc/unrelated.go only
	out, err := run(t, root, "tool", "call", "analyze_change", "base=HEAD~1")
	if err != nil {
		t.Fatalf("analyze_change: %v\n%s", err, out)
	}
	for _, want := range []string{"HEAD~1..HEAD changes", "in 1 file(s)", "- changed ", "svc/unrelated.go", "Risk"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer is missing %q:\n%s", want, out)
		}
	}
	// The payment service was not touched by that commit and must not be
	// reported as changed.
	if strings.Contains(out, "- changed Settle") || strings.Contains(out, "- changed Save") {
		t.Errorf("a symbol outside the range is listed as changed:\n%s", out)
	}

	// A range that covers the payment commits reports its symbols.
	wide, err := run(t, root, "tool", "call", "analyze_change", "base=HEAD~3", "head=HEAD~1")
	if err != nil {
		t.Fatalf("analyze_change over three commits: %v\n%s", err, wide)
	}
	if !strings.Contains(wide, "svc/payment.go") {
		t.Errorf("the wider range should include the payment service:\n%s", wide)
	}
}

func TestAnalyzeChangeExplainsWhatItCannotDo(t *testing.T) {
	// No repository: an answer saying so, not a failure.
	plain := queryFixture(t)
	out, err := run(t, plain, "tool", "call", "analyze_change", "base=main")
	if err != nil {
		t.Fatalf("no repository is a state of the world, not a malfunction: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Errorf("the answer should say why:\n%s", out)
	}

	// An unknown revision: guidance naming it.
	repo := whoFixture(t)
	out, err = run(t, repo, "tool", "call", "analyze_change", "base=no-such-branch")
	if err != nil {
		t.Fatalf("an unknown revision should be guidance: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no-such-branch") || !strings.Contains(out, "could not be read") {
		t.Errorf("the answer should name the revision it could not read:\n%s", out)
	}

	// A missing base is the caller's mistake and fails the command.
	if _, err := run(t, repo, "tool", "call", "analyze_change"); err == nil {
		t.Error("a call with no base must be rejected")
	}
}

// An agent choosing a reviewer needs a name. It must never be handed an
// address: the answer goes into a model's context.
func TestFindOwnersNamesPeopleAndNeverTheirAddresses(t *testing.T) {
	root := whoFixture(t) // Sam, Robin and Ash, each with an example.com address
	raw, err := run(t, root, "--json", "tool", "call", "find_owners", "symbol=Save")
	if err != nil {
		t.Fatalf("find_owners: %v\n%s", err, raw)
	}
	if !strings.Contains(raw, "Robin Vega") || !strings.Contains(raw, "Sam Okafor") {
		t.Errorf("the people who changed the reached code should be named:\n%s", raw)
	}
	if strings.Contains(raw, "example.com") || strings.Contains(raw, "@") {
		t.Errorf("an email address reached the tool answer:\n%s", raw)
	}
	if !strings.Contains(raw, "not who owns it") {
		t.Errorf("the answer should say what it does not establish:\n%s", raw)
	}

	plain := queryFixture(t) // not a repository
	out, err := run(t, plain, "tool", "call", "find_owners", "symbol=Save")
	if err != nil || !strings.Contains(out, "No commit history") {
		t.Errorf("no history should be guidance, not a failure: %v\n%s", err, out)
	}
}

// ---------------------------------------------------------------- path, implementations, tests

func TestPathShowsEachHopWithEvidence(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "path", "Settle", "Save")
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	for _, want := range []string{"Route 1 (1 hop(s))", "Settle", "-calls->", "Save", "svc/payment.go:"} {
		if !strings.Contains(out, want) {
			t.Errorf("path output is missing %q:\n%s", want, out)
		}
	}

	none, err := run(t, root, "path", "Save", "Settle")
	if err != nil {
		t.Fatalf("no route is an answer, not an error: %v\n%s", err, none)
	}
	if !strings.Contains(none, "No route found") || !strings.Contains(none, "not proof") {
		t.Errorf("an empty route must say it proves nothing:\n%s", none)
	}

	if _, err := run(t, root, "path", "Settle", "Save", "--max-depth", "0"); err == nil {
		t.Error("--max-depth 0 must be rejected")
	}
	if _, err := run(t, root, "path", "Settle"); err == nil {
		t.Error("path needs two symbols")
	}
}

func TestImplementationsPrintsBothDirections(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "implementations", "Repo")
	if err != nil {
		t.Fatalf("implementations: %v\n%s", err, out)
	}
	for _, want := range []string{"Implemented or extended by (0)", "Implements or extends (0)", "none declared"} {
		if !strings.Contains(out, want) {
			t.Errorf("implementations output is missing %q:\n%s", want, out)
		}
	}
	if _, err := run(t, root, "implementations", "Repo", "--min-confidence", "certain"); err == nil {
		t.Error("an unknown confidence must be rejected")
	}
}

func TestTestsCommandReportsNoTestsAsAFinding(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "tests", "Save")
	if err != nil {
		t.Fatalf("tests: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No tests found for Save") {
		t.Errorf("the fixture has no tests; the command should say so:\n%s", out)
	}
	if _, err := run(t, root, "tests", "NoSuchSymbolAnywhere"); err == nil {
		t.Error("an unknown symbol must be an error naming it")
	}
}

// ---------------------------------------------------------------- help layout and naming

func TestEveryCommandHasAGroup(t *testing.T) {
	root := NewRoot()
	placed, ungrouped := commandGroupOf(), ungroupedCommands()

	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
		if ungrouped[c.Name()] {
			continue
		}
		if c.GroupID == "" {
			t.Errorf("command %q is in no help group. Place it in commandGroupOf (internal/cli/groups.go), "+
				"or in ungroupedCommands if it is housekeeping.", c.Name())
		}
	}
	for name := range placed {
		if !have[name] {
			t.Errorf("commandGroupOf places %q, which is not a command", name)
		}
	}
	valid := map[string]bool{}
	for _, g := range commandGroups() {
		valid[g.ID] = true
	}
	for name, id := range placed {
		if !valid[id] {
			t.Errorf("%q is placed in group %q, which is not defined", name, id)
		}
	}
}

var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func TestCommandNamesFollowTheConvention(t *testing.T) {
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		for _, sub := range c.Commands() {
			full := strings.TrimSpace(path + " " + sub.Name())
			if !commandNamePattern.MatchString(sub.Name()) {
				t.Errorf("%q: name %q is not lower-case words joined by hyphens", full, sub.Name())
			}
			// One name per command. An alias is a second spelling to
			// document, complete and keep working, for no reader's benefit.
			if len(sub.Aliases) > 0 {
				t.Errorf("%q has aliases %v; commands have one name", full, sub.Aliases)
			}
			if sub.Short == "" {
				t.Errorf("%q has no one-line description", full)
			} else {
				if strings.HasSuffix(sub.Short, ".") {
					t.Errorf("%q: the one-line description ends in a full stop: %q", full, sub.Short)
				}
				if first := sub.Short[:1]; first != strings.ToUpper(first) {
					t.Errorf("%q: the one-line description should start with a capital: %q", full, sub.Short)
				}
			}
			for _, flag := range localFlagNames(sub) {
				if !commandNamePattern.MatchString(flag) {
					t.Errorf("%q: flag --%s is not lower-case words joined by hyphens", full, flag)
				}
			}
			walk(sub, full)
		}
	}
	walk(NewRoot(), "")
}

var flagUsageName = regexp.MustCompile(`(?m)^\s+(?:-\w, )?--([^\s=]+)`)

// localFlagNames reads a command's flag names from its usage text, which keeps
// this test off a direct dependency on the flag library.
func localFlagNames(c *cobra.Command) []string {
	var names []string
	for _, m := range flagUsageName.FindAllStringSubmatch(c.LocalFlags().FlagUsages(), -1) {
		names = append(names, m[1])
	}
	return names
}

// writeFiles writes a map of repo-relative path to content under root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil { //nolint:gosec // test fixture directory
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// indexProject runs init, index and resolve, as a user would.
func indexProject(t *testing.T, root string) {
	t.Helper()
	for _, step := range []string{"init", "index", "resolve"} {
		if out, err := run(t, root, step); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, out)
		}
	}
}

// Every agent tool should be reachable by a person too. This is the table that
// documents the mapping, and the test that keeps it complete.
func TestEveryAgentToolHasACommand(t *testing.T) {
	commandFor := map[string]string{
		"get_task_context":        "context",
		"get_project_overview":    "status",
		"search_code":             "search",
		"find_symbol":             "symbol",
		"get_symbol":              "symbol",
		"find_references":         "references",
		"find_callers":            "callers",
		"find_callees":            "callees",
		"find_implementations":    "implementations",
		"trace_call_path":         "path",
		"find_tests":              "tests",
		"analyze_impact":          "impact",
		"analyze_change":          "impact",
		"find_owners":             "owners",
		"plan_rename":             "rename",
		"plan_change":             "plan-change",
		"list_api_endpoints":      "api",
		"list_db_tables":          "db",
		"get_config_key":          "config",
		"get_file_outline":        "outline",
		"minimize_diff":           "minimize-diff",
		"get_resolution_coverage": "status",
		"list_blindspots":         "blindspots",
		"list_dead_code":          "dead-code",
		"list_hotspots":           "hotspots",
	}
	root := NewRoot()
	commands := map[string]bool{}
	for _, c := range root.Commands() {
		commands[c.Name()] = true
	}

	minimal, standard := devmcp.ProfileTools()
	_ = minimal
	all := append(append([]string{}, standard...), devmcp.FullOnlyTools()...)
	for _, tool := range all {
		cmd, ok := commandFor[tool]
		if !ok {
			t.Errorf("agent tool %q has no command listed in this table; add one, or add the command", tool)
			continue
		}
		if !commands[cmd] {
			t.Errorf("agent tool %q maps to command %q, which does not exist", tool, cmd)
		}
	}
	if len(commandFor) != len(all) {
		t.Errorf("the table lists %d tools; the server registers %d", len(commandFor), len(all))
	}
}
