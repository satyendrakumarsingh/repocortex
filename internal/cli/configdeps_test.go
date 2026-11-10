package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func m9Fixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		".env": "DATABASE_URL=postgres://localhost/app\n" +
			"DATABASE_PASSWORD=hunter2-super-secret\n" +
			"SERVER_PORT=8080\n" +
			"UNUSED_SETTING=nothing-reads-me\n",
		"go.mod": "module example.com/svc\n\ngo 1.25.0\n\nrequire (\n" +
			"\tgithub.com/spf13/cobra v1.10.2\n" +
			"\tgithub.com/never/imported v0.1.0\n" +
			"\tgolang.org/x/sys v0.30.0 // indirect\n)\n",
		"main.go": `package main

import "github.com/spf13/cobra"

func main() {
	url := os.Getenv("DATABASE_URL")
	port := os.Getenv("SERVER_PORT")
	missing := os.Getenv("NOT_DECLARED_ANYWHERE")
	_ = cobra.Command{}
	_, _, _ = url, port, missing
}`,
	}
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestConfigNeverPrintsAValue is the security test for the whole feature.
//
// Spec §24 requires that secret values are never exposed. The guarantee here is
// structural — nothing upstream reads a value — and this asserts it at the only
// place a user would ever see one.
func TestConfigNeverPrintsAValue(t *testing.T) {
	root := m9Fixture(t)

	for _, args := range [][]string{
		{"config"}, {"config", "--all"}, {"config", "--json"},
		{"search", "DATABASE_PASSWORD"},
	} {
		out, err := run(t, root, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, secret := range []string{"hunter2", "postgres://localhost", "nothing-reads-me"} {
			if strings.Contains(out, secret) {
				t.Errorf("%v leaked a configuration VALUE %q:\n%s", args, secret, out)
			}
		}
	}
}

func TestConfigReportsBothMismatches(t *testing.T) {
	root := m9Fixture(t)

	out, err := run(t, root, "config", "--all")
	if err != nil {
		t.Fatalf("config: %v\n%s", err, out)
	}
	// Read but nothing declares it — the deployment-failure case.
	if !strings.Contains(out, "NOT_DECLARED_ANYWHERE") {
		t.Errorf("a key read by code and declared nowhere was not reported:\n%s", out)
	}
	// Declared but nothing reads it.
	if !strings.Contains(out, "UNUSED_SETTING") {
		t.Errorf("a declared-but-unread key is missing from the inventory:\n%s", out)
	}
	// Sensitivity is decided from the NAME, having read nothing.
	if !strings.Contains(out, "sensitive") {
		t.Errorf("DATABASE_PASSWORD not labelled sensitive:\n%s", out)
	}
}

// TestConfigFilesAreAnalysedWithoutAGrammar pins the bug that made the first
// end-to-end run report zero declared keys: the indexer returned early for a
// file no tree-sitter grammar claims, and .env is exactly such a file.
func TestConfigFilesAreAnalysedWithoutAGrammar(t *testing.T) {
	root := m9Fixture(t)

	out, err := run(t, root, "config", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DATABASE_URL", "SERVER_PORT", "DATABASE_PASSWORD"} {
		if !strings.Contains(out, want) {
			t.Errorf("key %q from .env is missing; grammar-less files are not being analysed:\n%s",
				want, out)
		}
	}
}

func TestDepsFlagsTheUnimported(t *testing.T) {
	root := m9Fixture(t)

	out, err := run(t, root, "deps")
	if err != nil {
		t.Fatalf("deps: %v\n%s", err, out)
	}
	if !strings.Contains(out, "github.com/never/imported") {
		t.Errorf("an unimported dependency was not flagged:\n%s", out)
	}
	// cobra IS imported by main.go and must not be listed as unused.
	if strings.Contains(out, "DECLARED BUT NOT IMPORTED") &&
		strings.Contains(strings.SplitN(out, "Check before removing", 2)[0], "spf13/cobra") {
		t.Errorf("an imported dependency was reported unused:\n%s", out)
	}
	// Go's own // indirect marker already states the fact; repeating it is noise.
	if strings.Contains(strings.SplitN(out, "Check before removing", 2)[0], "golang.org/x/sys") {
		t.Errorf("an explicitly-indirect dependency was flagged unused:\n%s", out)
	}
	// The finding is a prompt, not a verdict.
	if !strings.Contains(out, "Check before removing") {
		t.Errorf("unused dependencies presented without the caveat:\n%s", out)
	}
}

// TestDepsReportsNoVulnerabilityData. Spec §27 forbids inventing it, and an
// embedded advisory list is exactly that.
func TestDepsReportsNoVulnerabilityData(t *testing.T) {
	root := m9Fixture(t)
	out, err := run(t, root, "deps", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"CVE-", "vulnerable", "advisory"} {
		if strings.Contains(out, banned) {
			t.Errorf("the report claims vulnerability information (%q):\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "No vulnerability data") {
		t.Errorf("the absence of vulnerability data is not stated:\n%s", out)
	}
}

func TestConfigAndDepsJSON(t *testing.T) {
	root := m9Fixture(t)
	for _, tc := range []struct {
		args []string
		key  string
	}{
		{[]string{"config", "--json"}, "keys"},
		{[]string{"deps", "--json"}, "dependencies"},
	} {
		out, err := run(t, root, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v emitted non-JSON: %v", tc.args, err)
		}
		if _, ok := got[tc.key]; !ok {
			t.Errorf("%v: JSON missing %q", tc.args, tc.key)
		}
	}
}

func TestConfigAndDepsExplainEmptyResults(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err := run(t, root, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "coverage limit") {
		t.Errorf("an empty config report does not state its limits:\n%s", out)
	}
	out, err = run(t, root, "deps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No dependency manifest found") {
		t.Errorf("an empty deps report is unclear:\n%s", out)
	}
}
