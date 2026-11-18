package cli

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// patchInvoker indexes files and returns a full-profile MCP invoker.
func patchInvoker(t *testing.T, files map[string]string) (context.Context, func(args map[string]string) string) {
	t.Helper()
	root := isolate(t)
	writeFiles(t, root, files)
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
	t.Cleanup(closeServer)
	inv, err := srv.NewInvoker(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inv.Close() })
	return ctx, func(args map[string]string) string {
		raw, _ := json.Marshal(args)
		res, err := inv.Call(ctx, "analyze_patch", raw)
		if err != nil {
			t.Fatal(err)
		}
		return res.Text
	}
}

// A compile error is never reported beside a SAFE or proven headline.
func TestAnalyzePatchGoArityChangeIsNotSafe(t *testing.T) {
	_, patch := patchInvoker(t, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"a.go":   "package m\n\nfunc Beta(x int) {}\n",
		"b.go":   "package m\n\nfunc Use() {\n\tBeta(1)\n}\n",
	})
	text := patch(map[string]string{"path": "a.go", "content": "package m\n\nfunc Beta(x, y int) {}\n"})
	if !strings.Contains(text, "compile_error") {
		t.Fatalf("setup: the type check found nothing:\n%s", text)
	}
	if strings.Contains(text, "SAFE") || strings.Contains(text, "Safe to act on") {
		t.Errorf("SAFE headline beside a compile error:\n%s", text)
	}
	if !strings.Contains(text, "breaks_build") && !strings.Contains(text, "BREAKS THE BUILD") {
		t.Errorf("verdict does not say the build breaks:\n%s", text)
	}
	if !strings.Contains(text, "parameter count changed") {
		t.Errorf("the arity change was not detected by the definition compare:\n%s", text)
	}
}

func TestAnalyzePatchPythonArityChangeIsDetected(t *testing.T) {
	_, patch := patchInvoker(t, map[string]string{
		"a.py": "def beta(x):\n    return x\n",
		"b.py": "from a import beta\n\n\ndef use():\n    return beta(1)\n",
	})
	text := patch(map[string]string{"path": "a.py", "content": "def beta(x, y, z):\n    return x\n"})
	if strings.Contains(text, "SAFE") {
		t.Errorf("a changed Python signature reads SAFE:\n%s", text)
	}
	if !strings.Contains(text, "signature changed") {
		t.Errorf("signature change not reported:\n%s", text)
	}
}
