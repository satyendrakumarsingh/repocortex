// Package config_test pins what the configuration surface actually CAUSES.
//
// config_test.go checks that a key parses and lands on the resolved Config.
// That is a different claim from the key doing anything, and the gap between
// the two is how `accuracy.mode`, `security.redact_secrets` and the `context.*`
// limits all came to be documented as settings while being, in varying degree,
// decoration. Two of the three turned out to have grown consumers since the
// documentation was written, which is the same failure in the other direction:
// nothing was pinning the behaviour, so nothing noticed when it arrived.
//
// These tests reach into the consuming packages, so a field going quiet again
// fails here rather than in a changelog. They are an EXTERNAL test package
// because internal/context and internal/indexer both import internal/config;
// importing them from `package config` would be an import cycle.
package config_test

import (
	stdctx "context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
)

func writeRepoConfig(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, config.RepoConfigName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestContextLimitsReachTheAssemblerBudget pins the consumer of the `context.*`
// keys.
//
// They were parsed and validated from M0 with nothing reading them;
// context.BudgetFrom is now the consumer, and every CLI path that assembles a
// pack (`query`, `compile`, `bench`) derives its Budget through it. All four
// values must survive that derivation — a limit that is validated and then
// replaced by a compiled-in default is the same lie as one that is ignored.
func TestContextLimitsReachTheAssemblerBudget(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("CORTEX_CONFIG_HOME", t.TempDir())
	writeRepoConfig(t, repo, `
context:
  max_files: 7
  max_symbols: 11
  max_lines: 13
  max_graph_depth: 2
`)
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}

	b := devctx.BudgetFrom(cfg)
	for _, tc := range []struct {
		key       string
		got, want int
	}{
		{"context.max_files", b.MaxFiles, 7},
		{"context.max_symbols", b.MaxSymbols, 11},
		{"context.max_lines", b.MaxLines, 13},
		{"context.max_graph_depth", b.MaxGraphDepth, 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d — the key is validated but not consulted", tc.key, tc.got, tc.want)
		}
	}
}

// TestContextDefaultsSurviveTheBudget guards the other direction: a repository
// that sets nothing must get config.Defaults, not a second set of numbers
// invented in the context package.
func TestContextDefaultsSurviveTheBudget(t *testing.T) {
	def := config.Defaults()
	b := devctx.BudgetFrom(def)
	if b.MaxFiles != def.MaxFiles || b.MaxSymbols != def.MaxSymbols ||
		b.MaxLines != def.MaxLines || b.MaxGraphDepth != def.MaxGraphDepth {
		t.Fatalf("budget %+v disagrees with config defaults (files=%d symbols=%d lines=%d depth=%d)",
			b, def.MaxFiles, def.MaxSymbols, def.MaxLines, def.MaxGraphDepth)
	}
}

// TestAccuracyModeSelectsWhatRuns pins `accuracy.mode` to a decision rather
// than a label.
//
// `fast` returns before detection: it promises tree-sitter only and seconds to
// index, so it must not pay for a repository walk or a subprocess. `auto` walks
// and reports what it found. Neither runs anything here — planning is pure
// inspection — which is what makes this test cheap enough to keep.
func TestAccuracyModeSelectsWhatRuns(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fast, err := indexer.NewPlan(stdctx.Background(), repo, planConfig(repo, config.AccuracyFast))
	if err != nil {
		t.Fatal(err)
	}
	if len(fast.Projects) != 0 || len(fast.Steps) != 0 {
		t.Errorf("accuracy.mode=fast planned %d project(s) and %d step(s); fast must not detect or run anything",
			len(fast.Projects), len(fast.Steps))
	}
	if !strings.Contains(fast.Note, "fast") {
		t.Errorf("fast plan must say why it is empty, got note %q", fast.Note)
	}

	auto, err := indexer.NewPlan(stdctx.Background(), repo, planConfig(repo, config.AccuracyAuto))
	if err != nil {
		t.Fatal(err)
	}
	if len(auto.Projects) == 0 {
		t.Fatal("accuracy.mode=auto must detect the Go project fast declined to look for")
	}
}

// TestExactModeFailsRatherThanDegrading pins the difference between `exact` and
// `auto` at the point where it matters: a blocked language.
//
// `auto` skipping a language is correct — the run continues with heuristic
// edges, labelled as such. `exact` skipping one would publish a run marked
// exact whose edges were guessed, so it must fail and name what is missing.
func TestExactModeFailsRatherThanDegrading(t *testing.T) {
	blocked := []indexer.Step{{
		Project: indexer.Project{Language: indexer.LangJava, Dir: "services/api"},
		Blocked: true,
		Reason:  "scip-java is not installed",
	}}

	if err := (indexer.Plan{Accuracy: config.AccuracyAuto, Steps: blocked}).Err(); err != nil {
		t.Errorf("auto must tolerate a blocked language, got: %v", err)
	}

	err := (indexer.Plan{Accuracy: config.AccuracyExact, Steps: blocked}).Err()
	if err == nil {
		t.Fatal("accuracy.mode=exact must refuse a plan it cannot carry out; " +
			"a silent downgrade from proven to guessed is the lie confidence exists to prevent")
	}
	if !strings.Contains(err.Error(), "java") {
		t.Errorf("exact-mode failure must name the language, got: %v", err)
	}
}

func planConfig(repo string, mode config.AccuracyMode) config.Config {
	cfg := config.Defaults()
	cfg.RepoRoot = repo
	cfg.Accuracy = mode
	// Nothing may be downloaded by a test.
	cfg.ProvisionPolicy = config.ProvisionNever
	return cfg
}
