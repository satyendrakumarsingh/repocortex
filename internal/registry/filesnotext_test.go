package registry

import (
	"context"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestFilesWithoutText: a file the index recorded but gave no full-text row
// (skipped as too large, binary, redacted) is invisible to SearchFiles. A
// caller that must see every file needs the list of those.
func TestFilesWithoutText(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	withText, err := w.AddFile(ctx, &model.File{Path: "a.go", Language: "go", ContentHash: "h1", ParseStatus: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.IndexFileText(ctx, withText, "a.go", "package a"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"z/Big.java", "b.bin"} {
		if _, err := w.AddFile(ctx, &model.File{Path: p, Language: "java", ContentHash: "h", ParseStatus: "skipped"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := r.FilesWithoutText(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	if fmt.Sprint(paths) != "[b.bin z/Big.java]" {
		t.Errorf("FilesWithoutText = %v, want [b.bin z/Big.java]", paths)
	}
}
