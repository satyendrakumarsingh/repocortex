package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// An agent hands analyze_patch the diff it is about to apply. The answer
// names the caller in another package that would stop compiling, found by
// type-checking the patch in memory, and nothing is written.
func TestAnalyzePatchTakesADiffAndTypeChecksGo(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod":     "module example.com/shop\n\ngo 1.22\n",
		"lib/lib.go": "package lib\n\n// Total adds.\nfunc Total(a, b int) int {\n\treturn a + b\n}\n",
		"app/app.go": "package app\n\nimport \"example.com/shop/lib\"\n\nfunc Run() int { return lib.Total(1, 2) }\n",
	})
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	g := &Global{Project: root, Quiet: true}
	cmd := NewRoot()
	cmd.SetArgs([]string{"--project", root, "--quiet", "version"})
	cmd.SetOut(io.Discard)
	if err := g.setup(cmd); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPFull})
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	inv, err := srv.NewInvoker(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inv.Close() }()

	diff := `--- a/lib/lib.go
+++ b/lib/lib.go
@@ -3,4 +3,4 @@
 // Total adds.
-func Total(a, b int) int {
-	return a + b
+func Total(a, b, c int) int {
+	return a + b + c
 }
`
	args, _ := json.Marshal(map[string]string{"diff": diff})
	res, err := inv.Call(ctx, "analyze_patch", args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "app/app.go:5") || !strings.Contains(res.Text, "stops compiling with the patch") ||
		!strings.Contains(res.Text, "not enough arguments") {
		t.Fatalf("the caller that stops compiling is not named:\n%s", res.Text)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "lib", "lib.go")); !strings.Contains(string(b), "func Total(a, b int) int") {
		t.Fatal("the patch was written to disk")
	}

	bad, _ := json.Marshal(map[string]string{"diff": "--- a/lib/lib.go\n+++ b/lib/lib.go\n@@ -1,1 +1,1 @@\n-package nothere\n+package lib\n"})
	res, err = inv.Call(ctx, "analyze_patch", bad)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "does not apply") {
		t.Errorf("a diff that does not apply was not refused:\n%s", res.Text)
	}
}
