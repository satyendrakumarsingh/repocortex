package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/autorun"
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

// A hook's pass has no terminal, so its outcome must reach `status`: a failed
// pass used to be visible only in a log file nobody reads, while the index
// quietly stopped following the code.
func TestStatusReportsTheLastAutoIndexPass(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod": "module example.com/ai\n\ngo 1.22\n",
		"a.go":   "package ai\n\nfunc A() int { return 1 }\n",
	})
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, root, "status"); err != nil || strings.Contains(out, "Last auto-index") {
		t.Fatalf("before any automatic pass: err %v\n%s", err, out)
	}
	if out, err := run(t, root, "auto-index"); err != nil {
		t.Fatalf("auto-index: %v\n%s", err, out)
	}
	out, err := run(t, root, "--json", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var st struct {
		Last *autorun.Run `json:"last_auto_index"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if st.Last == nil || !st.Last.OK || st.Last.At == "" {
		t.Fatalf("last_auto_index = %+v, want an ok pass", st.Last)
	}

	if err := autorun.Write(filepath.Join(root, IndexDir), time.Now(), time.Second, errors.New("resolve: database is locked")); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, root, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FAILED: resolve: database is locked") {
		t.Errorf("a failed pass is not reported:\n%s", out)
	}
}
