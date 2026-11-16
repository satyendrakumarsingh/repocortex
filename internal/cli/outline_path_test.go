package cli

import (
	"strings"
	"testing"
)

// Every other command reads a relative path from the project root that
// --project names. outline read it from the current directory instead, so
// `repocortex --project ~/repo outline src/a.go` failed with "no such file"
// unless the shell happened to be in ~/repo.
func TestOutlineResolvesRelativePathsFromTheProjectRoot(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"pkg/a.go": "package pkg\n\n// Add adds.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n",
	})
	out, err := run(t, root, "outline", "pkg/a.go")
	if err != nil {
		t.Fatalf("outline with a project-relative path: %v\n%s", err, out)
	}
	if !strings.Contains(out, "func Add(a, b int) int") || strings.Contains(out, "return a + b") {
		t.Errorf("outline output kept the body or lost the signature:\n%s", out)
	}
}
