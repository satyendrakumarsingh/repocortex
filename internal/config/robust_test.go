package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func loadWithRepoFile(t *testing.T, content string) (Config, error) {
	t.Helper()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeFile(t, filepath.Join(root, RepoConfigName), content)
	return Load(root)
}

func TestEmptyRepoConfigMeansDefaults(t *testing.T) {
	for name, content := range map[string]string{
		"empty":        "",
		"newline":      "\n",
		"comment only": "# just a comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadWithRepoFile(t, content)
			if err != nil {
				t.Fatalf("%q must mean defaults: %v", content, err)
			}
			if cfg.MaxGraphDepth != Defaults().MaxGraphDepth {
				t.Errorf("defaults not applied")
			}
		})
	}
}

func TestWrongTypeNamesKeyNotGoType(t *testing.T) {
	_, err := loadWithRepoFile(t, "index: 5\n")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "config.") || strings.Contains(msg, "unmarshal") {
		t.Errorf("message leaks Go types: %s", msg)
	}
	if !strings.Contains(msg, "number") || !strings.Contains(msg, "mapping") {
		t.Errorf("message should say what was found and wanted: %s", msg)
	}
}

func TestBadExcludeGlobRejected(t *testing.T) {
	_, err := loadWithRepoFile(t, "index:\n  exclude: [\"[\"]\n")
	if err == nil || !strings.Contains(err.Error(), "index.exclude") {
		t.Fatalf("want index.exclude glob error, got %v", err)
	}
}
