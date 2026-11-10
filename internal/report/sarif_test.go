package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
)

func TestBuildSARIF(t *testing.T) {
	findings := []Finding{
		{RuleID: "cortex/b", RuleDescription: "B", Severity: "low", Message: "m3", Path: "z.go", Line: 9},
		{RuleID: "cortex/a", RuleDescription: "A", Severity: "high", Message: "m1", Path: "a.go", Line: 3, Confidence: "exact"},
		{RuleID: "cortex/a", RuleDescription: "A", Severity: "medium", Message: "m2"},
	}
	log := BuildSARIF("1.2.3", findings)

	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("want one SARIF 2.1.0 run, got version %q runs %d", log.Version, len(log.Runs))
	}
	run := log.Runs[0]
	if run.Tool.Driver.Name != "RepoCortex" || run.Tool.Driver.Version != "1.2.3" {
		t.Errorf("driver = %+v", run.Tool.Driver)
	}
	if got := len(run.Tool.Driver.Rules); got != 2 {
		t.Errorf("rules deduplicated: want 2, got %d", got)
	}
	tests := []struct {
		i     int
		rule  string
		level string
		loc   bool
	}{
		{0, "cortex/a", "error", true}, // locationless m2 is omitted from results
		{1, "cortex/b", "note", true},
	}
	for _, tc := range tests {
		r := run.Results[tc.i]
		if r.RuleID != tc.rule || r.Level != tc.level || (len(r.Locations) > 0) != tc.loc {
			t.Errorf("result %d = %+v, want rule %s level %s loc %v", tc.i, r, tc.rule, tc.level, tc.loc)
		}
	}
	if run.Results[0].Properties["confidence"] != "exact" {
		t.Errorf("confidence not carried: %+v", run.Results[1].Properties)
	}
	if len(run.Results) != 2 {
		t.Errorf("want 2 located results, got %d", len(run.Results))
	}
	if findings[0].RuleID != "cortex/b" {
		t.Error("BuildSARIF reordered its input slice")
	}

	a, _ := json.Marshal(BuildSARIF("1.2.3", findings))
	b, _ := json.Marshal(BuildSARIF("1.2.3", findings))
	if string(a) != string(b) {
		t.Error("SARIF output is not deterministic")
	}
}

func TestBuildSARIFEmptyHasResultsArray(t *testing.T) {
	data, err := json.Marshal(BuildSARIF("dev", nil))
	if err != nil {
		t.Fatal(err)
	}
	// Code scanning treats a missing results array as "did not run".
	var v struct {
		Runs []struct {
			Results []any `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.Runs[0].Results == nil {
		t.Errorf("empty findings must serialise as results: [], got %s", data)
	}
}

func TestConverters(t *testing.T) {
	b := FromBlindspots([]missed.Blindspot{{
		Kind: missed.BlindspotOrphanTable, Severity: "high", Target: "payments",
		Path: "db/schema.sql", Line: 12, Description: "table is never read", Remediation: "drop it or map it",
	}})
	if len(b) != 1 || b[0].RuleID != "cortex/blindspot/orphan_table" || b[0].Path != "db/schema.sql" ||
		b[0].Line != 12 || !strings.Contains(b[0].Message, "drop it or map it") {
		t.Errorf("blindspot finding = %+v", b)
	}

	v := FromViolations([]architecture.Violation{{
		RuleID: "no-controller-to-repo", Severity: "high", Verdict: "forbidden",
		From: "web", To: "repo", FromName: "Handler", ToName: "Save",
		Kind: "CALLS", Confidence: "high", FilePath: "web/h.go", Line: 40,
	}, {Severity: "low", FilePath: "x.go"}})
	if v[0].RuleID != "cortex/arch/no-controller-to-repo" || v[0].Confidence != "high" || v[0].Line != 40 {
		t.Errorf("violation finding = %+v", v[0])
	}
	if v[1].RuleID != "cortex/arch/unnamed" {
		t.Errorf("unnamed rule id = %q", v[1].RuleID)
	}

	d := FromDataViolations([]architecture.DataViolation{{
		RuleID: "pci-isolation", Severity: "high", Verdict: "forbidden", Classification: "pci",
		Path: architecture.DataPath{Endpoint: "GET /x", Table: "cards", Steps: []architecture.PathStep{
			{Symbol: "GET /x"}, {Symbol: "load", FilePath: "svc/load.go", Line: 7},
		}},
	}})
	if d[0].RuleID != "cortex/data/pci-isolation" || d[0].Path != "svc/load.go" || d[0].Line != 7 {
		t.Errorf("data violation finding = %+v", d[0])
	}
}

func TestBuildSARIFOmitsLocationless(t *testing.T) {
	log := BuildSARIF("v", []Finding{
		{RuleID: "r", Message: "a", Path: "a.go", Line: 1},
		{RuleID: "r", Message: "b"},
		{RuleID: "r2", RuleDescription: "R2", Message: "c"},
	})
	run := log.Runs[0]
	for _, r := range run.Results {
		if len(r.Locations) < 1 {
			t.Errorf("result without location emitted: %+v", r)
		}
	}
	if len(run.Invocations) != 1 || !run.Invocations[0].ExecutionSuccessful {
		t.Fatalf("invocations = %+v", run.Invocations)
	}
	n := run.Invocations[0].ToolExecutionNotifications
	if len(n) != 1 || n[0].Level != "warning" || !strings.HasPrefix(n[0].Message.Text, "2 findings have no source location") {
		t.Errorf("notifications = %+v", n)
	}
	clean := BuildSARIF("v", []Finding{{RuleID: "r", Path: "a.go"}})
	if len(clean.Runs[0].Invocations) != 0 {
		t.Errorf("no invocations expected when every finding is located")
	}
}

func TestBuildSARIFPathHandling(t *testing.T) {
	tests := []struct {
		name    string
		opts    []SARIFOption
		path    string
		wantURI string
	}{
		{"plain", nil, "a/b.go", "a/b.go"},
		{"prefix", []SARIFOption{WithPathPrefix("services/api")}, "a/b.go", "services/api/a/b.go"},
		{"colon in first segment", nil, "a:b/c.go", "./a:b/c.go"},
		{"space hash percent", nil, "my dir/a#b%c.go", "my%20dir/a%23b%25c.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log := BuildSARIF("v", []Finding{{RuleID: "r", Path: tc.path, Line: 1}}, tc.opts...)
			got := log.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI
			if got != tc.wantURI {
				t.Errorf("uri = %q, want %q", got, tc.wantURI)
			}
		})
	}
}

func TestBuildSARIFTieBreakers(t *testing.T) {
	fs := []Finding{
		{RuleID: "r", Path: "a", Message: "m", Severity: "low", Confidence: "b"},
		{RuleID: "r", Path: "a", Message: "m", Severity: "high", Confidence: "z"},
		{RuleID: "r", Path: "a", Message: "m", Severity: "high", Confidence: "a"},
	}
	rev := []Finding{fs[2], fs[1], fs[0]}
	a, _ := json.Marshal(BuildSARIF("v", fs))
	b, _ := json.Marshal(BuildSARIF("v", rev))
	if string(a) != string(b) {
		t.Error("output depends on input order")
	}
}

func TestConvertersEdgeCases(t *testing.T) {
	b := FromBlindspots([]missed.Blindspot{{Kind: missed.BlindspotOrphanTable, Target: "t", Description: "d"}})
	if b[0].Message != "t: d" {
		t.Errorf("message without remediation = %q", b[0].Message)
	}
	d := FromDataViolations([]architecture.DataViolation{{
		Path: architecture.DataPath{Steps: []architecture.PathStep{{Symbol: "x"}}},
	}})
	if d[0].Path != "" || d[0].Line != 0 || d[0].RuleID != "cortex/data/unnamed" {
		t.Errorf("no-file-hop finding = %+v", d[0])
	}
}
