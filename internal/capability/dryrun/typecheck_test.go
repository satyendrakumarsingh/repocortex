package dryrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A signature change is reported as the error it causes in the caller's own
// package, without the patch being written anywhere.
func TestTypeCheckPatchNamesTheCallerThatStopsCompiling(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod":       "module example.com/m\n\ngo 1.22\n",
		"lib/lib.go":   "package lib\n\nfunc Total(a, b int) int { return a + b }\n",
		"app/app.go":   "package app\n\nimport \"example.com/m/lib\"\n\nfunc Run() int { return lib.Total(1, 2) }\n",
		"app/other.go": "package app\n\nfunc broken() int { return undefinedName }\n", // already broken: not the patch's
	})
	scope := []string{"lib/lib.go", "app/app.go", "app/other.go"}
	overlay := map[string]string{"lib/lib.go": "package lib\n\nfunc Total(a, b, c int) int { return a + b + c }\n"}

	got, err := TypeCheckPatch(context.Background(), root, scope, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "app/app.go" || got[0].Line != 5 || !strings.Contains(got[0].Msg, "not enough arguments") {
		t.Fatalf("breakage = %+v; want one error in app/app.go:5 about arguments", got)
	}
	if b, err := TypeCheckPatch(context.Background(), root, scope, map[string]string{"lib/lib.go": "package lib\n\nfunc Total(a, b int) int { return b + a }\n"}); err != nil || len(b) != 0 {
		t.Fatalf("a patch that compiles reported %+v, %v", b, err)
	}
}

func TestGoImportPath(t *testing.T) {
	root := writeModule(t, map[string]string{
		"go.mod":            "module example.com/m\n",
		"svc/go.mod":        "module example.com/svc\n",
		"svc/internal/x.go": "package internal\n",
	})
	cases := map[string]string{".": "example.com/m", "lib": "example.com/m/lib", "svc": "example.com/svc", "svc/internal": "example.com/svc/internal"}
	for dir, want := range cases {
		if got := GoImportPath(root, dir); got != want {
			t.Errorf("GoImportPath(%q) = %q, want %q", dir, got, want)
		}
	}
}
