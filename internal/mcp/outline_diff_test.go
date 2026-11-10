package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetFileOutlineTool(t *testing.T) {
	// The tool reads only inside the repo root, so the fixture lives there.
	tmpDir := t.TempDir()
	sess := newGraphTestServerAt(t, tmpDir, nil)
	ctx := context.Background()

	testFile := filepath.Join(tmpDir, "example.go")
	content := `package example

// Greeter greets people.
type Greeter interface {
	Greet(name string) string
}

func Hello(name string) string {
	msg := "Hello, " + name
	return msg
}
`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "get_file_outline",
		Arguments: map[string]any{
			"path": testFile,
		},
	})
	if err != nil {
		t.Fatalf("call get_file_outline: %v", err)
	}

	text := resultText(res)
	if !strings.Contains(text, "type Greeter interface") {
		t.Errorf("expected pruned outline to contain 'type Greeter interface', got:\n%s", text)
	}
	if !strings.Contains(text, "func Hello(name string) string {") {
		t.Errorf("expected pruned outline to contain 'func Hello', got:\n%s", text)
	}
	if strings.Contains(text, `msg := "Hello, " + name`) {
		t.Errorf("expected function body to be pruned, got:\n%s", text)
	}
}

func TestMinimizeDiffTool(t *testing.T) {
	sess := newGraphTestServer(t, nil)
	ctx := context.Background()

	rawDiff := `diff --git a/pkg/model/symbol.go b/pkg/model/symbol.go
index 1234567..89abcdef 100644
--- a/pkg/model/symbol.go
+++ b/pkg/model/symbol.go
@@ -10,6 +10,7 @@ type Symbol struct {
  Name string
+ ExtraField string
  Kind Kind
`

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "minimize_diff",
		Arguments: map[string]any{
			"diff": rawDiff,
		},
	})
	if err != nil {
		t.Fatalf("call minimize_diff: %v", err)
	}

	text := resultText(res)
	if !strings.Contains(text, "pkg/model/symbol.go") {
		t.Errorf("expected minimized diff to reference symbol.go, got:\n%s", text)
	}
	if !strings.Contains(text, "+ ExtraField string") {
		t.Errorf("expected minimized diff to contain added line, got:\n%s", text)
	}
	if strings.Contains(text, "index 1234567") {
		t.Errorf("expected index hash metadata to be stripped, got:\n%s", text)
	}
}

// TestGetFileOutlineRejectsEscapes proves an agent-supplied path cannot read
// outside the project root: "../", absolute paths outside it, and symlinks
// that point out of it are all refused, and the outside file's content never
// reaches the response.
func TestGetFileOutlineRejectsEscapes(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "SECRET_OUTSIDE_ROOT"
	outside := filepath.Join(base, "outside.go")
	if err := os.WriteFile(outside, []byte("package x\n\n// "+secret+"\nfunc Leak() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package x\n\nfunc Ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess := newGraphTestServerAt(t, root, nil)
	ctx := context.Background()

	for _, p := range []string{"../outside.go", "a/../../outside.go", outside, "link.go"} {
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{
			Name:      "get_file_outline",
			Arguments: map[string]any{"path": p},
		})
		if err != nil {
			t.Fatalf("path %q: call: %v", p, err)
		}
		text := resultText(res)
		if !res.IsError {
			t.Errorf("path %q: expected an error result, got:\n%s", p, text)
		}
		if strings.Contains(text, secret) {
			t.Errorf("path %q: response leaked content from outside the root:\n%s", p, text)
		}
	}

	// A legitimate relative path inside the root still works.
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "get_file_outline",
		Arguments: map[string]any{"path": "ok.go"},
	})
	if err != nil || res.IsError {
		t.Fatalf("in-root path should succeed: err=%v res=%s", err, resultText(res))
	}
}

// TestGetFileOutlineSuppressesSensitiveFiles pins that credential files are
// never served, in the same words security.RedactFile uses.
func TestGetFileOutlineSuppressesSensitiveFiles(t *testing.T) {
	root := t.TempDir()
	const secret = "TOPSECRETVALUE"
	files := map[string]string{
		".env":                "API_TOKEN=" + secret + "\n",
		"id_rsa":              "-----BEGIN OPENSSH PRIVATE KEY-----\n" + secret + "\n",
		"config/.env.staging": "DB_PASSWORD=" + secret + "\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(".env", filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sess := newGraphTestServerAt(t, root, nil)

	for _, p := range []string{".env", "id_rsa", "config/.env.staging", "notes.txt"} {
		res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
			Name:      "get_file_outline",
			Arguments: map[string]any{"path": p},
		})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		text := resultText(res)
		if strings.Contains(text, secret) {
			t.Errorf("%s: secret leaked:\n%s", p, text)
		}
		if !strings.Contains(text, "suppressed in full") {
			t.Errorf("%s: expected the RedactFile notice, got:\n%s", p, text)
		}
	}
}

// TestGetFileOutlineRejectsSymlinkedDirectory covers a directory component that
// is a symlink to outside the root.
func TestGetFileOutlineRejectsSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "x.go"), []byte("package x\n// SECRET_OUTSIDE_ROOT\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sess := newGraphTestServerAt(t, root, nil)
	res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_file_outline",
		Arguments: map[string]any{"path": "linkdir/x.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(resultText(res), "SECRET_OUTSIDE_ROOT") {
		t.Fatalf("symlinked directory escape not refused:\n%s", resultText(res))
	}
}
