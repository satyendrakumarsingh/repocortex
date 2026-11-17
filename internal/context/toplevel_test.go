package context

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTopLevelCallerAdmitsItsFile: a script whose top-level code calls what
// the task names is a caller the change must update. Its code is in no
// definition, so the walk reaches the file's synthetic caller, and the pack
// names the file. A registry that cannot list those callers gives the pack it
// gave before, without the file.
func TestTopLevelCallerAdmitsItsFile(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/ops.py", "def deploy_release(env):\n    return env\n")
	// No word of the task is in this file but the call itself, so text alone
	// would not admit it.
	write("scripts/ship.py", "from lib.ops import deploy_release\n\nprint(1)\ndeploy_release(\"prod\")\n")

	reg, g, runID := indexFixture(t, root)
	ctx := context.Background()
	req := Request{Task: "change the signature of deploy_release"}

	pack, err := NewAssembler().Assemble(ctx, reg, g, runID, req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range pack.Items {
		if it.Path == "scripts/ship.py" {
			found = true
			if it.Kind != "file" {
				t.Errorf("the top-level caller is item kind %q, want file", it.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("the script that calls deploy_release at its top level is not in the pack: %+v", pack.Items)
	}

	// The read is optional: a registry without it is served as before.
	pack, err = NewAssembler().Assemble(ctx, struct{ Registry }{reg}, g, runID, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pack.Items {
		if it.Path == "scripts/ship.py" && it.Kind == "file" {
			t.Errorf("a registry with no top-level read still admitted %+v", it)
		}
	}
}
