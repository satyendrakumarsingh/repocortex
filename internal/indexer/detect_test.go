package indexer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestDetectSingleLanguage(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name:  "go module",
			files: map[string]string{"go.mod": "module x\n", "main.go": "package main"},
			want:  []string{"go:."},
		},
		{
			name:  "maven",
			files: map[string]string{"pom.xml": "<project/>"},
			want:  []string{"java:."},
		},
		{
			name:  "gradle kotlin dsl",
			files: map[string]string{"build.gradle.kts": ""},
			want:  []string{"java:."},
		},
		{
			name:  "typescript",
			files: map[string]string{"tsconfig.json": "{}", "package.json": "{}"},
			want:  []string{"typescript:."},
		},
		{
			name:  "python",
			files: map[string]string{"pyproject.toml": "", "requirements.txt": ""},
			want:  []string{"python:."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Detect(context.Background(), writeRepo(t, tc.files))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if !reflect.DeepEqual(projectDirs(got), tc.want) {
				t.Fatalf("got %v, want %v", projectDirs(got), tc.want)
			}
		})
	}
}

// TestDetectMonorepo is the case the plan shape exists for: several projects,
// each with its own sub-root, in one repository.
func TestDetectMonorepo(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go.mod":                     "module x",
		"services/api/pom.xml":       "<project/>",
		"services/web/package.json":  "{}",
		"services/web/tsconfig.json": "{}",
		"tools/scripts/setup.py":     "",
		// Both a Node and a Python project in one directory is genuinely two
		// projects and must produce two steps.
		"tools/mixed/package.json":   "{}",
		"tools/mixed/pyproject.toml": "",
		// Must not be detected: build output and dependencies.
		"node_modules/left-pad/package.json": "{}",
		"services/api/target/pom.xml":        "<project/>",
		"web/dist/package.json":              "{}",
		".git/config":                        "",
	})

	got, err := Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := []string{
		"go:.",
		"java:services/api",
		"typescript:services/web",
		"python:tools/mixed",
		"typescript:tools/mixed",
		"python:tools/scripts",
	}
	if !reflect.DeepEqual(projectDirs(got), want) {
		t.Fatalf("got %v\nwant %v", projectDirs(got), want)
	}

	// Markers are evidence and must be reported, sorted, for `indexers list`.
	for _, p := range got {
		if p.Dir == "services/web" {
			if !reflect.DeepEqual(p.Markers, []string{"package.json", "tsconfig.json"}) {
				t.Fatalf("markers = %v", p.Markers)
			}
		}
	}
}

func TestDetectEmptyRepo(t *testing.T) {
	got, err := Detect(context.Background(), writeRepo(t, map[string]string{"README.md": "hi", "src/lib.rs": ""}))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no projects, got %v", projectDirs(got))
	}
}

func TestDetectIsDeterministic(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"a/go.mod": "module a", "b/package.json": "{}", "c/pom.xml": "", "d/setup.py": "",
	})
	first, err := Detect(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, err := Detect(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(projectDirs(first), projectDirs(next)) {
			t.Fatalf("iteration %d differs: %v vs %v", i, projectDirs(first), projectDirs(next))
		}
	}
}

// TestDetectDoesNotFollowSymlinks guards against a repository committing a
// symlink to somewhere with marker files in it. Walking it would attribute
// another project's — or the machine's — files to this repository.
func TestDetectDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	outside := writeRepo(t, map[string]string{"pom.xml": "<project/>"})
	root := writeRepo(t, map[string]string{"README.md": ""})
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "pom.xml"), filepath.Join(root, "pom.xml")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := Detect(context.Background(), root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("symlinked markers were detected: %v", projectDirs(got))
	}
}

func TestDetectRejectsNonDirectory(t *testing.T) {
	root := writeRepo(t, map[string]string{"go.mod": ""})
	if _, err := Detect(context.Background(), filepath.Join(root, "go.mod")); err == nil {
		t.Fatal("expected an error for a file as repository root")
	}
	if _, err := Detect(context.Background(), filepath.Join(root, "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing repository root")
	}
}

func TestSanitizeRelRefusesEscapes(t *testing.T) {
	for _, bad := range []string{"../etc", "a/../../b", "/abs", "-flag", "a/-flag/b", "a/../b", "./a"} {
		if _, err := sanitizeRel(bad); err == nil {
			t.Errorf("sanitizeRel(%q) accepted an unsafe path", bad)
		}
	}
	for _, ok := range []string{"", ".", "a", "services/api", "a/b/c"} {
		if _, err := sanitizeRel(ok); err != nil {
			t.Errorf("sanitizeRel(%q) = %v, want accepted", ok, err)
		}
	}
}

// TestSlugForIsPathSafe covers the one repository-derived string that becomes a
// file name. The output directory is the only place this package writes, and a
// traversal here would be a write outside it.
func TestSlugForIsPathSafe(t *testing.T) {
	p := Project{Language: LangGo, Dir: "a/../../b c/d$(whoami)"}
	slug := slugFor(p)
	if filepath.Base(slug) != slug {
		t.Fatalf("slug %q is not a single path element", slug)
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			t.Fatalf("slug %q contains %q", slug, r)
		}
	}
}
