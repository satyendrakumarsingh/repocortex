package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	c := Defaults()
	if c.Accuracy != AccuracyAuto {
		t.Errorf("default accuracy = %q, want auto", c.Accuracy)
	}
	if c.ProvisionPolicy != ProvisionPrompt {
		t.Errorf("default provision = %q, want prompt — provisioning must never be silent", c.ProvisionPolicy)
	}
	if c.MCPProfile != MCPMinimal {
		t.Errorf("default MCP profile = %q, want minimal", c.MCPProfile)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

// TestRepoCannotSetExecutionSettings is THE security test for this package.
//
// A repository is attacker-controlled the moment you clone it. If a repo could
// set accuracy.mode=exact and provision=auto, then `repocortex index` on a
// hostile repo would download and execute an attacker-named binary.
func TestRepoCannotSetExecutionSettings(t *testing.T) {
	repo := t.TempDir()
	cfgHome := t.TempDir()
	t.Setenv("REPOCORTEX_CONFIG_HOME", cfgHome)

	// A repo config that tries very hard to gain execution.
	writeFile(t, filepath.Join(repo, RepoConfigName), `
project:
  name: totally-legit
accuracy:
  mode: exact
  indexers:
    provision: auto
    runner: native
`)

	cfg, err := Load(repo)
	// Unknown keys are rejected outright, which is the strongest outcome.
	if err != nil {
		if !strings.Contains(err.Error(), "accuracy") && !strings.Contains(err.Error(), "field") {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Logf("repo config rejected outright: %v", err)
		return
	}

	// If it parsed, the execution settings must still be untouched defaults.
	if cfg.Accuracy != AccuracyAuto {
		t.Errorf("SECURITY: repo set accuracy to %q", cfg.Accuracy)
	}
	if cfg.ProvisionPolicy != ProvisionPrompt {
		t.Errorf("SECURITY: repo set provision policy to %q", cfg.ProvisionPolicy)
	}
}

// TestRepoCannotDisableRedaction ensures a hostile repo cannot make RepoCortex
// leak its own secrets to an agent.
//
// The guarantee is now structural rather than conditional. `redact_secrets`
// used to exist and honour only `true`; `false` parsed and was dropped. Since
// redaction is unconditional in pipeline.RegistrySink, neither value ever
// decided anything, so the field is gone and KnownFields(true) turns any
// attempt to express an opinion about redaction into a parse error. A repo that
// says `false` now gets told, instead of being quietly overruled.
func TestRepoCannotDisableRedaction(t *testing.T) {
	for _, body := range []string{
		"security:\n  redact_secrets: false\n",
		"security:\n  redact_secrets: true\n",
		"security: {}\n",
	} {
		repo := t.TempDir()
		t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
		writeFile(t, filepath.Join(repo, RepoConfigName), body)

		_, err := Load(repo)
		if err == nil {
			t.Fatalf("SECURITY: repo config %q was accepted; a redaction key that parses "+
				"reads as a redaction control, and there is no such control", body)
		}
		if !strings.Contains(err.Error(), "security") {
			t.Fatalf("error should name the offending key, got: %v", err)
		}
	}
}

// TestRepoExcludesAreAdditive ensures a repo cannot un-exclude .git or
// node_modules to get RepoCortex to index and expose them.
func TestRepoExcludesAreAdditive(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	writeFile(t, filepath.Join(repo, RepoConfigName), `
index:
  exclude:
    - "docs/**"
`)
	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Exclude, ",")
	for _, must := range []string{".git/**", "node_modules/**", "docs/**"} {
		if !strings.Contains(joined, must) {
			t.Errorf("exclude list lost %q; got %v", must, cfg.Exclude)
		}
	}
}

func TestUserConfigCanSetExecutionSettings(t *testing.T) {
	repo := t.TempDir()
	cfgHome := t.TempDir()
	t.Setenv("REPOCORTEX_CONFIG_HOME", cfgHome)
	writeFile(t, filepath.Join(cfgHome, "config.yml"), `
accuracy:
  mode: exact
  indexers:
    provision: auto
    runner: docker
    languages: [java, go]
`)

	cfg, err := Load(repo)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Accuracy != AccuracyExact {
		t.Errorf("accuracy = %q, want exact", cfg.Accuracy)
	}
	if cfg.ProvisionPolicy != ProvisionAuto {
		t.Errorf("provision = %q, want auto", cfg.ProvisionPolicy)
	}
	if cfg.IndexerRunner != "docker" {
		t.Errorf("runner = %q, want docker", cfg.IndexerRunner)
	}
	if len(cfg.IndexerLanguages) != 2 {
		t.Errorf("indexer languages = %v, want [java go]", cfg.IndexerLanguages)
	}
}

func TestRepoConfigAppliesIndexingSettings(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	writeFile(t, filepath.Join(repo, RepoConfigName), `
project:
  name: payment-platform
languages: [java, go]
capabilities: [code, impact, test]
context:
  max_files: 30
  max_graph_depth: 2
mcp:
  profile: standard
`)
	cfg, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectName != "payment-platform" {
		t.Errorf("project name = %q", cfg.ProjectName)
	}
	if cfg.MaxFiles != 30 || cfg.MaxGraphDepth != 2 {
		t.Errorf("context budget not applied: files=%d depth=%d", cfg.MaxFiles, cfg.MaxGraphDepth)
	}
	if cfg.MCPProfile != MCPStandard {
		t.Errorf("mcp profile = %q, want standard", cfg.MCPProfile)
	}
	if len(cfg.Capabilities) != 3 {
		t.Errorf("capabilities = %v", cfg.Capabilities)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name  string
		mutar func(*Config)
	}{
		{"bad accuracy", func(c *Config) { c.Accuracy = "perfect" }},
		{"bad provision", func(c *Config) { c.ProvisionPolicy = "yolo" }},
		{"bad profile", func(c *Config) { c.MCPProfile = "everything" }},
		{"bad runner", func(c *Config) { c.IndexerRunner = "podman-ish" }},
		{"zero depth", func(c *Config) { c.MaxGraphDepth = 0 }},
		{"unbounded depth", func(c *Config) { c.MaxGraphDepth = 99 }},
		{"zero files", func(c *Config) { c.MaxFiles = 0 }},
	}
	for _, tc := range cases {
		c := Defaults()
		tc.mutar(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func TestLoadWithNoConfigFilesUsesDefaults(t *testing.T) {
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load with no files: %v", err)
	}
	if cfg.Accuracy != AccuracyAuto {
		t.Fatal("missing config files should yield safe defaults")
	}
}

// TestRepoCannotMountTheLocalArtifactCache ensures a cloned repository cannot
// ask for its own build to be handed the developer's ~/.m2.
//
// That mount is the one thing that undoes the container's purpose: a hostile
// build could poison an artifact every other project on the machine then
// compiles against. It is a user decision, so the repository must have no way
// to express it.
func TestRepoCannotMountTheLocalArtifactCache(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	writeFile(t, filepath.Join(repo, RepoConfigName), `
accuracy:
  indexers:
    mount_local_artifacts: true
`)
	cfg, err := Load(repo)
	if err == nil && cfg.IndexerMountLocalArtifacts {
		t.Fatal("SECURITY: repo config mounted the local artifact cache")
	}
}
