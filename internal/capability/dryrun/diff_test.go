package dryrun

import (
	"errors"
	"strings"
	"testing"
)

func TestApplyUnifiedDiff(t *testing.T) {
	files := map[string]string{
		"svc/a.go": "package svc\n\n// Total adds.\nfunc Total(a, b int) int {\n\treturn a + b\n}\n\nfunc Other() {}\n",
	}
	read := func(p string) (string, error) {
		if s, ok := files[p]; ok {
			return s, nil
		}
		return "", errors.New("no such file")
	}
	diff := `diff --git a/svc/a.go b/svc/a.go
--- a/svc/a.go
+++ b/svc/a.go
@@ -3,4 +3,4 @@ package svc
 // Total adds.
-func Total(a, b int) int {
-	return a + b
+func Total(a, b, c int) int {
+	return a + b + c
 }
--- /dev/null
+++ b/svc/new.go
@@ -0,0 +1,2 @@
+package svc
+func New() {}
--- a/svc/gone.go
+++ /dev/null
@@ -1,1 +0,0 @@
-package svc
`
	got, err := ApplyUnifiedDiff(diff, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d files: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Content, "func Total(a, b, c int) int {\n\treturn a + b + c\n}\n\nfunc Other() {}\n") {
		t.Errorf("patched content:\n%s", got[0].Content)
	}
	if !got[1].Added || got[1].Path != "svc/new.go" || got[1].Content != "package svc\nfunc New() {}\n" {
		t.Errorf("added file: %+v", got[1])
	}
	if !got[2].Deleted || got[2].Path != "svc/gone.go" {
		t.Errorf("deleted file: %+v", got[2])
	}

	// Context that is not in the file is refused, not guessed.
	bad := "--- a/svc/a.go\n+++ b/svc/a.go\n@@ -4,2 +4,2 @@\n-func Total(x int) int {\n+func Total(y int) int {\n"
	if _, err := ApplyUnifiedDiff(bad, read); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Errorf("a hunk whose context is absent was applied: %v", err)
	}
}
