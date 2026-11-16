package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auto-index is what every managed git hook runs. It builds the index and
// resolve commands itself and calls them directly, so it has to hand them its
// own context: a command that is not executed through cobra has none, and the
// store opened a database with a nil context and panicked (2 of 4 concurrent
// hook runs on a real repository).
func TestAutoIndexRunsIndexAndResolve(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod": "module example.com/ai\n\ngo 1.22\n",
		"a.go":   "package ai\n\nfunc A() int { return B() }\n\nfunc B() int { return 1 }\n",
	})
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, root, "auto-index"); err != nil {
		t.Fatalf("auto-index: %v\n%s", err, out)
	}
	status, err := os.ReadFile(filepath.Join(root, IndexDir, "reindex.status"))
	if err != nil {
		t.Fatalf("auto-index wrote no status file: %v", err)
	}
	if !strings.Contains(string(status), "status=ok") {
		t.Fatalf("auto-index did not complete:\n%s", status)
	}
}
