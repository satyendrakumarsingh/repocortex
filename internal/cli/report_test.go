package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/report"
)

func writeJSONFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReportSARIF(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	m := writeJSONFixture(t, dir, "missed.json",
		`{"blindspots":[{"kind":"orphan_table","severity":"high","target":"t","path":"s.sql","line":2,"description":"d"}]}`)
	a := writeJSONFixture(t, dir, "arch.json",
		`{"architecture_declared":true,"violations":[{"rule_id":"r1","severity":"medium","file_path":"x.go","line":5}],"data_violations":[]}`)
	out := filepath.Join(dir, "out.sarif")

	if _, err := run(t, root, "report", "sarif", "--blindspots", m, "--arch", a, "--out", out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				RuleID string `json:"ruleId"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &log); err != nil {
		t.Fatal(err)
	}
	if log.Version != "2.1.0" || len(log.Runs) != 1 || len(log.Runs[0].Results) != 2 {
		t.Fatalf("unexpected SARIF: %s", data)
	}
	got := map[string]bool{}
	for _, r := range log.Runs[0].Results {
		got[r.RuleID] = true
	}
	for _, want := range []string{"repocortex/blindspot/orphan_table", "repocortex/arch/r1"} {
		if !got[want] {
			t.Errorf("missing rule ID %q in %s", want, data)
		}
	}
}

func TestReportRequiresAnInput(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "report", "sarif"); err == nil {
		t.Error("report sarif with no inputs succeeded")
	}
	if _, err := run(t, root, "report", "pr"); err == nil {
		t.Error("report pr with no inputs succeeded")
	}
}

func TestReportRejectsMalformedJSON(t *testing.T) {
	root := isolate(t)
	bad := writeJSONFixture(t, t.TempDir(), "bad.json", `{"blindspots": [`)
	_, err := run(t, root, "report", "sarif", "--blindspots", bad)
	if err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("malformed input error must name the file, got %v", err)
	}
}

func TestReportPRToStdout(t *testing.T) {
	root := isolate(t)
	m := writeJSONFixture(t, t.TempDir(), "missed.json", `{"blindspots":[]}`)
	out, err := run(t, root, "report", "pr", "--blindspots", m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "<!-- repocortex:pr-report -->") || !strings.Contains(out, "| Blast radius | not run |") {
		t.Errorf("unexpected PR body:\n%s", out)
	}
}

func TestReportSARIFPathPrefix(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	m := writeJSONFixture(t, dir, "missed.json",
		`{"blindspots":[{"kind":"orphan_table","severity":"high","target":"t","path":"s.sql","line":2,"description":"d"}]}`)
	out, err := run(t, root, "report", "sarif", "--blindspots", m, "--path-prefix", "services/api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "services/api/s.sql") {
		t.Errorf("path prefix not applied:\n%s", out)
	}
}

func TestReportArchNotDeclaredLoads(t *testing.T) {
	root := isolate(t)
	a := writeJSONFixture(t, t.TempDir(), "arch.json",
		`{"architecture_declared":false,"violations":[],"crossings":[],"components":[],"note":"n"}`)
	out, err := run(t, root, "report", "pr", "--arch", a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "| Architecture | not run |") {
		t.Errorf("undeclared architecture must render as not run:\n%s", out)
	}
}

func FuzzReportLoad(f *testing.F) {
	f.Add(`{"blindspots":[]}`)
	f.Add(`{"architecture_declared":true,"violations":null}`)
	f.Add(`{`)
	f.Fuzz(func(t *testing.T, body string) {
		dir := t.TempDir()
		p := filepath.Join(dir, "in.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		in := reportInputs{missedPath: p, archPath: p, impactPath: p}
		pr, err := in.load()
		if err != nil {
			return
		}
		_ = report.RenderPR(pr) // must not panic on any decodable input
	})
}

func TestReportRejectsWrongShape(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	arch := writeJSONFixture(t, dir, "arch.json", `{"architecture_declared":true,"violations":[]}`)
	missed := writeJSONFixture(t, dir, "missed.json", `{"blindspots":[]}`)
	for _, tc := range []struct {
		flag, file, name string
	}{
		{"--blindspots", arch, "arch.json"},
		{"--impact", missed, "missed.json"},
		{"--arch", missed, "missed.json"},
	} {
		_, err := run(t, root, "report", "pr", tc.flag, tc.file)
		if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), "does not look like") {
			t.Errorf("%s %s: want shape error naming the file, got %v", tc.flag, tc.name, err)
		}
	}
}

func TestReportPRImpact(t *testing.T) {
	root := isolate(t)
	im := writeJSONFixture(t, t.TempDir(), "impact.json", `{
  "base_rev":"origin/main","head_rev":"HEAD","indexed_revision":"abc123",
  "revision_matches_range":false,"changed_files":["a.go"],"seeds_found":1,"seeds":[],
  "impacted":[{"name":"Handler","file_path":"h.go","depth":1,"confidence":"high","is_test":false}],
  "unknown":[{"kind":"stale_index","detail":"index older than head"}],
  "risk":{"level":"high","score":70,"factors":[],"formula":"f"},
  "completeness":{},"truncated":false}`)
	out, err := run(t, root, "report", "pr", "--impact", im)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Stale index", "### Blast radius", "| Handler | h.go | 1 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestReportDeclaredArch(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	a := writeJSONFixture(t, dir, "arch.json", `{"architecture_declared":true,"components":[],"crossings":[],
 "violations":[{"rule_id":"r1","severity":"high","verdict":"forbidden","from_component":"a","to_component":"b","file_path":"x.go","line":5,"confidence":"high"}],
 "data_paths":[],
 "data_violations":[{"rule_id":"d1","severity":"high","verdict":"forbidden","classification":"pci",
   "path":{"endpoint":"POST /pay","table":"cards","classifications":["pci"],"access":"read","confidence":"high",
   "steps":[{"symbol":"Pay","kind":"calls","confidence":"high","file_path":"pay.go","line":9}]}}]}`)
	out, err := run(t, root, "report", "pr", "--arch", a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "r1") || !strings.Contains(out, "d1") || !strings.Contains(out, "x.go") {
		t.Errorf("violations missing from PR body:\n%s", out)
	}
	sarif, err := run(t, root, "report", "sarif", "--arch", a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repocortex/arch/r1"`, `"repocortex/data/d1"`, "pay.go"} {
		if !strings.Contains(sarif, want) {
			t.Errorf("SARIF missing %s:\n%s", want, sarif)
		}
	}
}
