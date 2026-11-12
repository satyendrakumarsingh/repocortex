package fs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// minimalCfg keeps the config-level filters out of the way so a test observes
// only the behaviour it is about. Tests that care about the default exclusions
// use config.Defaults() explicitly.
func minimalCfg() config.Config {
	return config.Config{Include: []string{"**"}, Exclude: []string{".git/**"}}
}

// writeTree materialises a repo-relative map of files. Directories are created
// as needed, so "a/b/c.go" is enough to get the whole chain.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func walkTree(t *testing.T, root string, cfg config.Config, opts ...Option) []Discovered {
	t.Helper()
	w, err := NewWalker(root, cfg, opts...)
	if err != nil {
		t.Fatalf("NewWalker: %v", err)
	}
	got, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return got
}

// indexedPaths returns the paths that were discovered and not skipped.
func indexedPaths(ds []Discovered) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		if !d.Skipped {
			out = append(out, d.Path)
		}
	}
	return out
}

func lookup(ds []Discovered, path string) (Discovered, bool) {
	for _, d := range ds {
		if d.Path == path {
			return d, true
		}
	}
	return Discovered{}, false
}

func TestWalkGitignoreSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "basic exclusion",
			files: map[string]string{
				".gitignore": "*.log\n",
				"a.go":       "package a",
				"a.log":      "noise",
			},
			want: []string{".gitignore", "a.go"},
		},
		{
			name: "negation re-includes a file",
			files: map[string]string{
				".gitignore": "*.log\n!keep.log\n",
				"keep.log":   "kept",
				"drop.log":   "dropped",
			},
			want: []string{".gitignore", "keep.log"},
		},
		{
			name: "directory-only pattern excludes the directory",
			files: map[string]string{
				".gitignore":  "out/\n",
				"out/gen.go":  "package out",
				"src/main.go": "package main",
			},
			want: []string{".gitignore", "src/main.go"},
		},
		{
			// The trailing slash is the whole rule: "out/" must leave a file
			// named "out" alone.
			name: "directory-only pattern spares a like-named file",
			files: map[string]string{
				".gitignore":  "out/\n",
				"out":         "not a directory",
				"src/main.go": "package main",
			},
			want: []string{".gitignore", "out", "src/main.go"},
		},
		{
			name: "nested gitignore governs only its own subtree",
			files: map[string]string{
				".gitignore":     "*.tmp\n",
				"src/.gitignore": "secret.go\n",
				"src/secret.go":  "package src",
				"src/main.go":    "package main",
				"lib/secret.go":  "package lib",
				"lib/x.tmp":      "tmp",
			},
			want: []string{".gitignore", "lib/secret.go", "src/.gitignore", "src/main.go"},
		},
		{
			name: "nested gitignore may negate a parent rule",
			files: map[string]string{
				".gitignore":      "*.md\n",
				"docs/.gitignore": "!*.md\n",
				"docs/guide.md":   "guide",
				"README.md":       "readme",
			},
			want: []string{".gitignore", "docs/.gitignore", "docs/guide.md"},
		},
		{
			name: "star-form allows re-inclusion under an excluded directory",
			files: map[string]string{
				".gitignore":  "out/*\n!out/keep.go\n",
				"out/keep.go": "package out",
				"out/junk.go": "package out",
			},
			want: []string{".gitignore", "out/keep.go"},
		},
		{
			name: "double star spans directories",
			files: map[string]string{
				".gitignore":     "**/gen/**\n",
				"a/gen/x.go":     "package gen",
				"a/b/gen/y.go":   "package gen",
				"a/b/keep/z.go":  "package keep",
				"gen_notadir.go": "package main",
			},
			want: []string{".gitignore", "a/b/keep/z.go", "gen_notadir.go"},
		},
		{
			name: "anchored pattern applies at the root only",
			files: map[string]string{
				".gitignore":   "/conf.yml\n",
				"conf.yml":     "root",
				"sub/conf.yml": "sub",
			},
			want: []string{".gitignore", "sub/conf.yml"},
		},
		{
			name: "repocortexignore uses the same syntax",
			files: map[string]string{
				".repocortexignore": "*.gen.go\n",
				"a.gen.go":          "package a",
				"a.go":              "package a",
			},
			want: []string{".repocortexignore", "a.go"},
		},
		{
			name: "comments and blank lines are not rules",
			files: map[string]string{
				".gitignore": "# a comment\n\n   \n*.log\n",
				"a.log":      "noise",
				"a.go":       "package a",
			},
			want: []string{".gitignore", "a.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTree(t, root, tc.files)

			got := indexedPaths(walkTree(t, root, minimalCfg()))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("indexed paths\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// TestWalkRefusesSymlinkEscape is the path-traversal guard. A repository can
// commit a symlink pointing anywhere on the host; following it would hash and
// index host data that an agent would then quote back as repository content.
func TestWalkRefusesSymlinkEscape(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	secret := filepath.Join(outside, "secrets.txt")
	if err := os.WriteFile(secret, []byte("SUPER_SECRET_TOKEN"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.go":     "package main",
		"sub/real.go": "package sub",
	})

	// Relative targets are resolved against the link's own directory, so build
	// them with filepath.Rel rather than guessing the number of "..".
	relFromRoot, err := filepath.Rel(root, secret)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	relFromSub, err := filepath.Rel(filepath.Join(root, "sub"), secret)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}

	links := []struct {
		name   string
		target string
	}{
		{"absolute_escape.go", secret},
		{"relative_escape.go", relFromRoot},
		{"sub/parent_escape.go", relFromSub},
		{"etc_passwd.go", filepath.FromSlash("/etc/passwd")},
	}
	for _, l := range links {
		if err := os.Symlink(l.target, filepath.Join(root, filepath.FromSlash(l.name))); err != nil {
			// Windows needs a privilege for symlinks; the guard is still worth
			// testing everywhere it can be.
			t.Skipf("cannot create symlink (%v); skipping on this platform", err)
		}
	}
	// A link that stays inside the root is skipped too, but for a different
	// reason — it is not an escape, just not worth indexing twice.
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "inside_link.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := walkTree(t, root, minimalCfg())

	wantReason := map[string]string{
		"absolute_escape.go":   SkipSymlinkEscape,
		"relative_escape.go":   SkipSymlinkEscape,
		"sub/parent_escape.go": SkipSymlinkEscape,
		"inside_link.go":       SkipSymlink,
	}
	for path, reason := range wantReason {
		d, ok := lookup(got, path)
		if !ok {
			t.Errorf("%s: not reported at all; a refused symlink must still be visible", path)
			continue
		}
		if !d.Skipped {
			t.Errorf("%s: FOLLOWED a symlink out of the repository root", path)
		}
		if d.SkipReason != reason {
			t.Errorf("%s: SkipReason = %q, want %q", path, d.SkipReason, reason)
		}
		if d.ContentHash != "" {
			t.Errorf("%s: hashed a symlink target (%s); nothing outside the root may be read", path, d.ContentHash)
		}
	}

	// /etc/passwd may not exist (Windows), in which case the link is broken
	// rather than escaping — either way it must not be indexed.
	if d, ok := lookup(got, "etc_passwd.go"); ok && !d.Skipped {
		t.Error("etc_passwd.go: followed a symlink to /etc/passwd")
	}

	for _, d := range got {
		if !d.Skipped && strings.Contains(d.Path, "escape") {
			t.Errorf("%s was indexed; symlink containment failed", d.Path)
		}
	}
	if want := []string{"main.go", "sub/real.go"}; !reflect.DeepEqual(indexedPaths(got), want) {
		t.Errorf("indexed %v, want %v", indexedPaths(got), want)
	}
}

func TestWalkSkipsBinaryFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"text.go":       "package main\n",
		"early_nul.bin": "MZ\x00\x00padding",
		// A NUL just inside the 8000-byte window is binary...
		"late_nul.bin": strings.Repeat("a", binarySniffLen-1) + "\x00" + strings.Repeat("b", 100),
		// ...and one just outside it is not, which is git's rule, not ours.
		"past_window.txt": strings.Repeat("a", binarySniffLen) + "\x00tail",
		"utf8.txt":        "héllo wörld — em dash\n",
	})

	got := walkTree(t, root, minimalCfg())

	for _, tc := range []struct {
		path        string
		wantSkipped bool
	}{
		{"early_nul.bin", true},
		{"late_nul.bin", true},
		{"past_window.txt", false},
		{"utf8.txt", false},
		{"text.go", false},
	} {
		d, ok := lookup(got, tc.path)
		if !ok {
			t.Fatalf("%s missing from results", tc.path)
		}
		if d.Skipped != tc.wantSkipped {
			t.Errorf("%s: Skipped = %v, want %v (reason %q)", tc.path, d.Skipped, tc.wantSkipped, d.SkipReason)
		}
		if tc.wantSkipped {
			if d.SkipReason != SkipBinary {
				t.Errorf("%s: SkipReason = %q, want %q", tc.path, d.SkipReason, SkipBinary)
			}
			if d.ContentHash != "" {
				t.Errorf("%s: skipped files must not claim a content hash", tc.path)
			}
		} else if d.ContentHash == "" {
			t.Errorf("%s: indexed file has no content hash", tc.path)
		}
	}
}

func TestWalkSkipsOversizedFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"small.go": "package main\n",
		"big.go":   strings.Repeat("x", 4096),
	})

	got := walkTree(t, root, minimalCfg(), WithMaxFileSize(1024))

	big, ok := lookup(got, "big.go")
	if !ok {
		t.Fatal("big.go missing; oversized files must be reported, not dropped")
	}
	if !big.Skipped || big.SkipReason != SkipTooLarge {
		t.Errorf("big.go: Skipped=%v reason=%q, want true/%q", big.Skipped, big.SkipReason, SkipTooLarge)
	}
	if big.SizeBytes != 4096 {
		t.Errorf("big.go: SizeBytes = %d, want 4096 — size must be recorded even when skipped", big.SizeBytes)
	}
	if big.ContentHash != "" {
		t.Error("big.go: oversized file must not be hashed")
	}

	small, _ := lookup(got, "small.go")
	if small.Skipped {
		t.Errorf("small.go was skipped: %q", small.SkipReason)
	}

	// Default limit is 2 MiB and must not be defeated by a zero or negative
	// override, which would otherwise be an unbounded-read primitive.
	w, err := NewWalker(root, minimalCfg(), WithMaxFileSize(0))
	if err != nil {
		t.Fatalf("NewWalker: %v", err)
	}
	if w.maxFileSize != DefaultMaxFileSize {
		t.Errorf("WithMaxFileSize(0) gave %d, want the %d default", w.maxFileSize, DefaultMaxFileSize)
	}
}

// TestWalkIsDeterministic protects the downstream determinism gate, which
// compares two index runs of the same tree byte-for-byte. The walk is
// concurrent, so this is the property most likely to regress silently.
func TestWalkIsDeterministic(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := map[string]string{".gitignore": "*.log\n"}
	for i := range 40 {
		files[fmt.Sprintf("pkg%02d/file%02d.go", i%7, i)] = fmt.Sprintf("package pkg%02d\n", i%7)
		files[fmt.Sprintf("pkg%02d/deep/nested/x%02d.py", i%7, i)] = "print(1)\n"
		files[fmt.Sprintf("pkg%02d/noise%02d.log", i%7, i)] = "noise"
	}
	files["big.bin"] = strings.Repeat("q", 8192)
	files["blob.bin"] = "\x00\x01\x02"
	writeTree(t, root, files)

	first := walkTree(t, root, minimalCfg(), WithMaxFileSize(4096))
	if len(first) < 80 {
		t.Fatalf("fixture produced only %d results; the test is not exercising much", len(first))
	}
	for range 8 {
		got := walkTree(t, root, minimalCfg(), WithMaxFileSize(4096))
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("walk results differ between runs\n first: %v\nsecond: %v", first, got)
		}
	}

	// And the ordering is sorted, not merely repeatable — a stable but
	// arbitrary order would still break a diff against another tool's output.
	for i := 1; i < len(first); i++ {
		if first[i-1].Path >= first[i].Path {
			t.Fatalf("results not sorted: %q then %q", first[i-1].Path, first[i].Path)
		}
	}

	// Single-worker and many-worker walks must agree, which is the real
	// statement of concurrency-independence.
	seq := walkTree(t, root, minimalCfg(), WithMaxFileSize(4096), WithWorkers(1))
	par := walkTree(t, root, minimalCfg(), WithMaxFileSize(4096), WithWorkers(runtime.NumCPU()*4))
	if !reflect.DeepEqual(seq, par) {
		t.Error("sequential and parallel walks disagree")
	}
}

func TestWalkRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	files := map[string]string{}
	for i := range 300 {
		files[fmt.Sprintf("d%03d/sub/f.go", i)] = "package d\n"
	}
	writeTree(t, root, files)

	w, err := NewWalker(root, minimalCfg())
	if err != nil {
		t.Fatalf("NewWalker: %v", err)
	}

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := w.Walk(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Walk error = %v, want context.Canceled", err)
		}
		if got != nil {
			t.Errorf("Walk returned %d results alongside an error; partial results must not escape", len(got))
		}
	})

	t.Run("cancelled mid-walk yields an error, never a partial result", func(t *testing.T) {
		// Timing decides whether the walk finishes first, so assert the
		// invariant that holds either way: a nil error implies a complete
		// result. A truncated slice with no error is the bug this guards.
		full := len(walkTree(t, root, minimalCfg()))
		for range 20 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
			got, err := w.Walk(ctx)
			cancel()
			if err == nil && len(got) != full {
				t.Fatalf("Walk succeeded with %d of %d results; cancellation lost data silently", len(got), full)
			}
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				t.Fatalf("Walk error = %v, want a context error", err)
			}
		}
	})

	t.Run("deadline stops the walk promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if _, err := w.Walk(ctx); err == nil {
			t.Fatal("Walk succeeded on a cancelled context")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("cancelled walk took %v; cancellation is not being checked during the walk", d)
		}
	})
}

func TestWalkPathsUseForwardSlashes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/b/c/deep.go":       "package c",
		"top.go":              "package main",
		"x/y/.repocortexkeep": "keep",
	})

	got := walkTree(t, root, minimalCfg())
	want := []string{"a/b/c/deep.go", "top.go", "x/y/.repocortexkeep"}
	if !reflect.DeepEqual(indexedPaths(got), want) {
		t.Fatalf("paths = %v, want %v", indexedPaths(got), want)
	}
	for _, d := range got {
		if strings.ContainsRune(d.Path, '\\') {
			t.Errorf("path %q contains a backslash; paths must be forward-slashed on every OS", d.Path)
		}
		if filepath.IsAbs(d.Path) || strings.HasPrefix(d.Path, "/") {
			t.Errorf("path %q is not repo-relative", d.Path)
		}
	}
}

// TestWalkConfigExcludeCannotBeNegated is the security test for the config
// boundary: .repocortex.yml is attacker-controlled and its exclude entries are
// appended to the defaults, so a repository must not be able to append a
// negation that re-includes .git or node_modules.
func TestWalkConfigExcludeCannotBeNegated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.go":                  "package main",
		".git/config":              "[core]",
		".git/objects/ab/deadbeef": "object",
		"node_modules/pkg/i.js":    "module.exports={}",
		"vendor/dep/dep.go":        "package dep",
		"app.min.js":               "var a=1",
	})

	cfg := config.Defaults()
	// Exactly what a hostile .repocortex.yml produces after config.applyRepo.
	cfg.Exclude = append(cfg.Exclude,
		"!.git/**", "!node_modules/**", "!vendor/**", "!**/*.min.js", "!**")

	got := indexedPaths(walkTree(t, root, cfg))
	if want := []string{"main.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("indexed %v, want %v — a repository re-included a default exclusion", got, want)
	}

	// The same must hold for a .gitignore negation, which is repo-controlled
	// too. Git semantics let a later rule win within the ignore file, but the
	// config exclusions sit above that mechanism entirely.
	writeTree(t, root, map[string]string{".gitignore": "!.git/**\n!node_modules/**\n"})
	got = indexedPaths(walkTree(t, root, config.Defaults()))
	for _, p := range got {
		if strings.HasPrefix(p, ".git/") || strings.HasPrefix(p, "node_modules/") {
			t.Errorf("%s was indexed; a .gitignore negation defeated a config exclusion", p)
		}
	}
}

func TestWalkIncludeFilter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.go":         "package a",
		"b.py":         "print(1)",
		"deep/c.go":    "package deep",
		"deep/d.txt":   "text",
		"deep/e/f.go":  "package e",
		"notes.md":     "# notes",
		"deep/e/g.rst": "rst",
	})

	cfg := minimalCfg()
	cfg.Include = []string{"**/*.go", "*.md"}

	got := indexedPaths(walkTree(t, root, cfg))
	want := []string{"a.go", "deep/c.go", "deep/e/f.go", "notes.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("indexed %v, want %v", got, want)
	}
}

func TestWalkDetectsLanguagesAndHashes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const goSrc = "package main\n\nfunc main() {}\n"
	writeTree(t, root, map[string]string{
		"main.go":    goSrc,
		"Dockerfile": "FROM scratch\n",
		"script.py":  "print(1)\n",
		"mystery.zz": "who knows\n",
	})

	got := walkTree(t, root, minimalCfg())

	// Language names come from the grammar registry, so assert only that the
	// known cases resolve and the unknown one stays empty — pinning exact
	// registry spellings here would make this test a mirror of that package.
	for _, path := range []string{"main.go", "Dockerfile", "script.py"} {
		d, ok := lookup(got, path)
		if !ok {
			t.Fatalf("%s missing", path)
		}
		if d.Language == "" {
			t.Errorf("%s: no language detected", path)
		}
	}
	if d, _ := lookup(got, "mystery.zz"); d.Language != "" {
		t.Errorf("mystery.zz: Language = %q, want empty for an unknown extension", d.Language)
	}

	sum := sha256.Sum256([]byte(goSrc))
	d, _ := lookup(got, "main.go")
	if want := hex.EncodeToString(sum[:]); d.ContentHash != want {
		t.Errorf("main.go: ContentHash = %q, want %q", d.ContentHash, want)
	}
	if d.SizeBytes != int64(len(goSrc)) {
		t.Errorf("main.go: SizeBytes = %d, want %d", d.SizeBytes, len(goSrc))
	}
	if d.ModTimeUnix <= 0 {
		t.Errorf("main.go: ModTimeUnix = %d, want a real timestamp", d.ModTimeUnix)
	}
}

func TestNewWalkerValidation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	file := filepath.Join(root, "f.go")
	if err := os.WriteFile(file, []byte("package f"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		root string
		cfg  config.Config
	}{
		{"empty root", "", minimalCfg()},
		{"blank root", "   ", minimalCfg()},
		{"missing root", filepath.Join(root, "nope"), minimalCfg()},
		{"root is a file", file, minimalCfg()},
		{"include compiles to nothing", root, config.Config{Include: []string{"#comment", ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewWalker(tc.root, tc.cfg); err == nil {
				t.Error("NewWalker succeeded, want an error")
			}
		})
	}

	// An empty Include list means "everything", not "nothing" — the zero
	// config must not silently index an empty repository.
	w, err := NewWalker(root, config.Config{})
	if err != nil {
		t.Fatalf("NewWalker with a zero config: %v", err)
	}
	got, err := w.Walk(context.Background())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(indexedPaths(got)) != 1 {
		t.Errorf("zero config indexed %v, want the single file", indexedPaths(got))
	}
	if !strings.HasPrefix(w.Root(), string(filepath.Separator)) && filepath.VolumeName(w.Root()) == "" {
		t.Errorf("Root() = %q, want an absolute path", w.Root())
	}
}

// TestWalkRootSymlinkResolution guards a real trap: on macOS t.TempDir() sits
// under /var, which is itself a symlink to /private/var. If the root were not
// resolved before the containment check, every file in the repository would
// look like it escaped.
func TestWalkRootSymlinkResolution(t *testing.T) {
	t.Parallel()

	real := t.TempDir()
	writeTree(t, real, map[string]string{"pkg/a.go": "package pkg"})

	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "repo")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink (%v)", err)
	}
	if err := os.Symlink(filepath.Join(real, "pkg", "a.go"), filepath.Join(real, "self.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got := walkTree(t, link, minimalCfg())
	if want := []string{"pkg/a.go"}; !reflect.DeepEqual(indexedPaths(got), want) {
		t.Errorf("indexed %v, want %v", indexedPaths(got), want)
	}
	// The in-root link is reached through the resolved root, so it must read
	// as an ordinary skipped symlink rather than an escape.
	if d, ok := lookup(got, "self.go"); !ok || d.SkipReason != SkipSymlink {
		t.Errorf("self.go: reason = %q, want %q", d.SkipReason, SkipSymlink)
	}
}
