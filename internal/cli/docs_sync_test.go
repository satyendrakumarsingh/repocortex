package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGateWorkflowMatchesUserGuide: the guide tells users to copy the workflow
// the repository ships. The two had already diverged once, so the guide's YAML
// block must contain the shipped file verbatim.
func TestGateWorkflowMatchesUserGuide(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "repocortex-gate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "USER_GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), string(wf)) {
		t.Error("docs/USER_GUIDE.md does not contain .github/workflows/repocortex-gate.yml verbatim; update the guide's YAML block")
	}
}

// TestGateWorkflowCanFail: a gate whose steps end in `|| true` cannot fail,
// and `impact` takes --base/--head rather than a file argument.
func TestGateWorkflowCanFail(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "repocortex-gate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(wf)
	if strings.Contains(text, "|| true") {
		t.Error("gate workflow swallows failures with '|| true'")
	}
	for _, want := range []string{
		"repocortex resolve", "--fail-on", "impact --base",
		"report sarif", "report pr", "upload-sarif", "repocortex:pr-report",
		"indexers pins", "indexers install --yes --out-dir", "scip import", "actions/cache@v4",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("gate workflow is missing %q", want)
		}
	}
	// A fork PR's token cannot write security events; an unguarded upload
	// would fail the job before the gate runs.
	i := strings.Index(text, "name: Upload SARIF to Code Scanning")
	if i < 0 {
		t.Fatal("gate workflow has no SARIF upload step")
	}
	step := text[i:]
	if j := strings.Index(step[1:], "- name:"); j >= 0 {
		step = step[:j+1]
	}
	if !strings.Contains(step, "github.event.pull_request.head.repo.full_name == github.repository") {
		t.Error("SARIF upload step must be skipped for fork pull requests")
	}
}

// stepBody returns the text of the workflow step whose "- name:" line is name,
// up to the next step.
func stepBody(t *testing.T, text, name string) string {
	t.Helper()
	i := strings.Index(text, "- name: "+name)
	if i < 0 {
		t.Fatalf("gate workflow has no step %q", name)
	}
	step := text[i:]
	if j := strings.Index(step[1:], "- name:"); j >= 0 {
		step = step[:j+1]
	}
	return step
}

// TestGateWorkflowImportsCompilerFactsBeforeImpact: compiler facts only help
// if impact reads them, the stage must be switchable off without editing
// steps, and the gate must never build the repository (that is what
// --allow-repo-build authorises for Java).
func TestGateWorkflowImportsCompilerFactsBeforeImpact(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "repocortex-gate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(wf)

	resolve := strings.Index(text, "./bin/repocortex resolve")
	imported := strings.Index(text, "./bin/repocortex scip import")
	impactAt := strings.Index(text, "impact --base")
	if resolve < 0 || imported < 0 || impactAt < 0 {
		t.Fatalf("missing a step: resolve=%d scip import=%d impact=%d", resolve, imported, impactAt)
	}
	if resolve >= imported || imported >= impactAt {
		t.Errorf("scip import must run after resolve and before impact (resolve=%d import=%d impact=%d)",
			resolve, imported, impactAt)
	}
	if !strings.Contains(text, "REPOCORTEX_ACCURACY: auto") {
		t.Error("REPOCORTEX_ACCURACY must default to auto at the workflow level")
	}
	for _, name := range []string{"Pin Compiler Indexers", "Cache Compiler Indexers", "Import Compiler Facts"} {
		if !strings.Contains(stepBody(t, text, name), "if: env.REPOCORTEX_ACCURACY == 'auto'") {
			t.Errorf("step %q is not guarded by REPOCORTEX_ACCURACY", name)
		}
	}
	if !strings.Contains(stepBody(t, text, "Cache Compiler Indexers"), "hashFiles('repocortex-out/indexer-pins.txt')") {
		t.Error("the indexer cache must be keyed on the pinned versions and digests")
	}
	imp := stepBody(t, text, "Import Compiler Facts")
	for _, want := range []string{
		"::warning::compiler indexers unavailable",
		"::warning::scip import of",
	} {
		if !strings.Contains(imp, want) {
			t.Errorf("Import Compiler Facts must degrade with %q, not fail the gate", want)
		}
	}
	if !strings.Contains(text, "steps.setup-go.outputs.go-version") {
		t.Error("the indexer cache key must include the Go version scip-go is built with")
	}
	if strings.Contains(text, "--allow-repo-build") {
		t.Error("the gate must never run the repository's own build")
	}
}
