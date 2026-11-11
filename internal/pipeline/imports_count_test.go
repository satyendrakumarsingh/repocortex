package pipeline

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDistinctImportsCountsFilesPerPath: the count is how many files import a
// path, not how many import statements there are. A file that imports the same
// package twice counts once.
func TestDistinctImportsCountsFilesPerPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/one.go":   "package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n",
		"a/two.go":   "package a\n\nimport (\n\t\"fmt\"\n)\n\nvar _ = fmt.Sprint\n",
		"b/three.go": "package b\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n",
	})

	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))
	got, err := reg.DistinctImports(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got["fmt"] != 3 {
		t.Errorf("fmt is imported by 3 files, counted %d", got["fmt"])
	}
	if got["os"] != 1 {
		t.Errorf("os is imported by 1 file, counted %d", got["os"])
	}
	if _, ok := got["net/http"]; ok {
		t.Error("a path nothing imports appeared in the count")
	}
}
