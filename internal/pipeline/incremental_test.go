package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// harness holds a fully wired index over a temp repo.
type harness struct {
	t      *testing.T
	root   string
	db     *store.DB
	reg    *registry.Registry
	walker *fs.Walker
	runID  int64
}

// harnessBuild is the producer identity every fixture in this file indexes and
// diffs under, so the build-crossing rule stays satisfied and the tests exercise
// the content-hash rule they were written for.
const harnessBuild = "test-build"

func newHarness(t *testing.T, files map[string]string) *harness {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	writeTree(t, root, files)

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}

	// Stamped with harnessBuild, and every Diff call passes the same value, so
	// the harness exercises the CONTENT-hash rule rather than the build-crossing
	// one. An unstamped run is deliberately treated as a mismatch (see
	// diffAgainst), so leaving this empty would make every incremental test
	// re-parse everything and quietly stop testing what it was written to test.
	reg := registry.New(db, registry.WithProducerVersion(harnessBuild))
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	walker, err := fs.NewWalker(root, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}

	sink := NewRegistrySink(w)
	if _, err := NewRunner(walker, Options{}).Run(ctx, sink); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	return &harness{t: t, root: root, db: db, reg: reg, walker: walker, runID: run.ID}
}

func (h *harness) write(rel, body string) {
	h.t.Helper()
	writeTree(h.t, h.root, map[string]string{rel: body})
}

func (h *harness) remove(rel string) {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.root, filepath.FromSlash(rel))); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) rename(from, to string) {
	h.t.Helper()
	dst := filepath.Join(h.root, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.root, filepath.FromSlash(from)), dst); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) sync() IncrementalStats {
	h.t.Helper()
	ctx := context.Background()
	changes, err := Diff(ctx, h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		h.t.Fatal(err)
	}
	stats, err := Incremental(ctx, h.reg, h.runID, h.walker, changes, IncrementalOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	return stats
}

func (h *harness) symbolNames() map[string]string {
	h.t.Helper()
	syms, err := h.reg.Symbols(context.Background(), h.runID)
	if err != nil {
		h.t.Fatal(err)
	}
	files, err := h.reg.Files(context.Background(), h.runID)
	if err != nil {
		h.t.Fatal(err)
	}
	byID := map[int64]string{}
	for _, f := range files {
		byID[f.ID] = f.Path
	}
	out := map[string]string{}
	for _, s := range syms {
		out[s.Name] = byID[s.FileID]
	}
	return out
}

func (h *harness) filePaths() map[string]bool {
	h.t.Helper()
	files, err := h.reg.Files(context.Background(), h.runID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, f := range files {
		out[f.Path] = true
	}
	return out
}

// TestIncrementalDetectsNoChanges: the common case. Running an incremental
// pass over an untouched tree must reindex nothing. If this regresses,
// incremental indexing silently becomes a full re-index.
func TestIncrementalDetectsNoChanges(t *testing.T) {
	h := newHarness(t, map[string]string{
		"a.go": "package p\n\ntype Alpha struct{}\n",
		"b.go": "package p\n\ntype Bravo struct{}\n",
	})

	stats := h.sync()
	if stats.Added+stats.Modified+stats.Deleted != 0 {
		t.Fatalf("clean tree reported work: %+v", stats)
	}
	if stats.Unchanged != 2 {
		t.Errorf("Unchanged = %d, want 2 (%+v)", stats.Unchanged, stats)
	}
}

// TestIncrementalModify is the §44 flow: modify a file, re-index, verify new
// symbols appear AND the old ones are gone.
func TestIncrementalModify(t *testing.T) {
	h := newHarness(t, map[string]string{
		"svc/service.go": "package svc\n\ntype TransactionService struct{}\n\nfunc (s *TransactionService) OldMethod() {}\n",
		"other.go":       "package p\n\ntype Untouched struct{}\n",
	})

	if got := h.symbolNames(); got["OldMethod"] == "" {
		t.Fatalf("setup failed, OldMethod missing: %v", got)
	}

	h.write("svc/service.go",
		"package svc\n\ntype TransactionService struct{}\n\nfunc (s *TransactionService) NewMethod() {}\n")

	stats := h.sync()
	if stats.Modified != 1 {
		t.Errorf("Modified = %d, want 1 (%+v)", stats.Modified, stats)
	}

	syms := h.symbolNames()
	if syms["NewMethod"] == "" {
		t.Error("new symbol NewMethod was not indexed")
	}
	if _, stale := syms["OldMethod"]; stale {
		t.Error("stale symbol OldMethod survived the update")
	}
	if syms["Untouched"] == "" {
		t.Error("an unrelated file lost its symbols during an incremental pass")
	}
}

// TestIncrementalDelete: §44 requires deletion to be covered explicitly. A
// symbol pointing at a file that no longer exists actively misleads an agent.
func TestIncrementalDelete(t *testing.T) {
	h := newHarness(t, map[string]string{
		"keep.go":   "package p\n\ntype Keeper struct{}\n",
		"remove.go": "package p\n\ntype Doomed struct{}\n\nfunc DoomedFunc() {}\n",
	})

	if got := h.symbolNames(); got["Doomed"] == "" {
		t.Fatalf("setup failed: %v", got)
	}

	h.remove("remove.go")
	stats := h.sync()
	if stats.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (%+v)", stats.Deleted, stats)
	}

	syms := h.symbolNames()
	for _, gone := range []string{"Doomed", "DoomedFunc"} {
		if _, still := syms[gone]; still {
			t.Errorf("symbol %q survived its file's deletion", gone)
		}
	}
	if syms["Keeper"] == "" {
		t.Error("unrelated file lost symbols during a delete")
	}
	if h.filePaths()["remove.go"] {
		t.Error("deleted file still present in the registry")
	}
}

// TestIncrementalAdd covers a newly created file.
func TestIncrementalAdd(t *testing.T) {
	h := newHarness(t, map[string]string{"a.go": "package p\n\ntype Alpha struct{}\n"})

	h.write("nested/dir/new.go", "package nested\n\ntype Fresh struct{}\n\nfunc FreshFunc() {}\n")
	stats := h.sync()
	if stats.Added != 1 {
		t.Errorf("Added = %d, want 1 (%+v)", stats.Added, stats)
	}

	syms := h.symbolNames()
	if syms["Fresh"] != "nested/dir/new.go" {
		t.Errorf("Fresh indexed at %q, want nested/dir/new.go", syms["Fresh"])
	}
	if syms["FreshFunc"] == "" {
		t.Error("FreshFunc not indexed")
	}
}

// TestIncrementalRename: §44 requires rename coverage. Content-hash diffing
// sees a rename as delete+add, which is correct — but the OLD path must not
// linger, or searches return a path that no longer exists.
func TestIncrementalRename(t *testing.T) {
	h := newHarness(t, map[string]string{
		"old/path/service.go": "package svc\n\ntype Renamed struct{}\n\nfunc RenamedFunc() {}\n",
	})

	h.rename("old/path/service.go", "new/path/service.go")
	stats := h.sync()

	if stats.Added != 1 || stats.Deleted != 1 {
		t.Errorf("rename should be 1 add + 1 delete, got %+v", stats)
	}

	paths := h.filePaths()
	if paths["old/path/service.go"] {
		t.Error("old path survived the rename")
	}
	if !paths["new/path/service.go"] {
		t.Error("new path was not indexed")
	}

	syms := h.symbolNames()
	if syms["Renamed"] != "new/path/service.go" {
		t.Errorf("Renamed attributed to %q, want new/path/service.go", syms["Renamed"])
	}
}

// TestIncrementalMatchesFullReindex is the strongest correctness check: after a
// sequence of edits, the incremental index must be indistinguishable from
// indexing the same tree from scratch. Any drift means incremental updates are
// slowly corrupting the index.
func TestIncrementalMatchesFullReindex(t *testing.T) {
	ctx := context.Background()

	initial := map[string]string{
		"a.go":      "package p\n\ntype Alpha struct{}\n",
		"b.go":      "package p\n\ntype Bravo struct{}\n\nfunc BravoFunc() {}\n",
		"sub/c.go":  "package sub\n\ntype Charlie struct{}\n",
		"doomed.go": "package p\n\ntype Doomed struct{}\n",
	}
	h := newHarness(t, initial)

	// A representative edit sequence: modify, delete, add, rename.
	h.write("a.go", "package p\n\ntype Alpha struct{}\n\nfunc AlphaAdded() {}\n")
	h.remove("doomed.go")
	h.write("added.go", "package p\n\ntype Added struct{}\n")
	h.rename("sub/c.go", "sub/renamed.go")
	h.sync()

	incDigest, err := h.reg.RegistryDigest(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}

	// Index the final on-disk state from scratch in a fresh database.
	db2, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "full.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := store.Migrate(ctx, db2.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	reg2 := registry.New(db2)
	run2, err := reg2.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := reg2.NewWriter(ctx, run2.ID)
	if err != nil {
		t.Fatal(err)
	}
	walker2, err := fs.NewWalker(h.root, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	sink2 := NewRegistrySink(w2)
	if _, err := NewRunner(walker2, Options{}).Run(ctx, sink2); err != nil {
		t.Fatal(err)
	}
	if err := sink2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg2.CommitRun(ctx, run2.ID); err != nil {
		t.Fatal(err)
	}
	fullDigest, err := reg2.RegistryDigest(ctx, run2.ID)
	if err != nil {
		t.Fatal(err)
	}

	if incDigest != fullDigest {
		t.Fatalf("incremental index diverged from a full re-index:\n incremental=%s\n full       =%s",
			incDigest, fullDigest)
	}
	t.Logf("incremental and full re-index agree: %s", incDigest)
}

func TestDiffClassifiesCorrectly(t *testing.T) {
	h := newHarness(t, map[string]string{
		"same.go": "package p\n\ntype Same struct{}\n",
		"mod.go":  "package p\n\ntype Before struct{}\n",
		"gone.go": "package p\n\ntype Gone struct{}\n",
	})

	h.write("mod.go", "package p\n\ntype After struct{}\n")
	h.remove("gone.go")
	h.write("new.go", "package p\n\ntype New struct{}\n")

	changes, err := Diff(context.Background(), h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]ChangeKind{}
	for _, c := range changes {
		got[c.Path] = c.Kind
	}
	want := map[string]ChangeKind{
		"same.go": ChangeSame,
		"mod.go":  ChangeModified,
		"gone.go": ChangeDeleted,
		"new.go":  ChangeAdded,
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("%s: kind = %q, want %q", path, got[path], kind)
		}
	}
}

// TestDiffIgnoresMtimeOnlyChanges: touching a file without changing content
// must not trigger a re-index. `git checkout` rewrites mtimes wholesale, and an
// mtime-based diff would re-index everything after every branch switch.
func TestDiffIgnoresMtimeOnlyChanges(t *testing.T) {
	h := newHarness(t, map[string]string{"a.go": "package p\n\ntype Alpha struct{}\n"})

	// Rewrite identical content, which updates mtime but not the hash.
	h.write("a.go", "package p\n\ntype Alpha struct{}\n")

	changes, err := Diff(context.Background(), h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Path == "a.go" && c.Kind != ChangeSame {
			t.Fatalf("a.go classified %q after an mtime-only change; diffing is not content-based", c.Kind)
		}
	}
}

// TestADifferentBuildMakesEveryFileDirty pins a correctness rule found the hard
// way.
//
// A fix to the SQL extractor started recognising a query the previous build had
// rejected. Re-indexing with the fixed binary changed nothing: the source file's
// content hash was identical, so the incremental differ marked it unchanged and
// the stale analysis survived. The missing edge stayed missing and the check
// that depended on it kept answering confidently and wrongly.
//
// Content hashing compares the INPUT. What changed was the FUNCTION. So a run
// written by a different build invalidates everything, and an unstamped run
// counts as different because it cannot be shown to match.
func TestADifferentBuildMakesEveryFileDirty(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name             string
		indexedBy, using string
		wantAllDirty     bool
	}{
		{"same build reuses unchanged files", "1.4.2", "1.4.2", false},
		{"newer build re-parses everything", "1.4.2", "1.5.0", true},
		{"older build re-parses everything", "1.5.0", "1.4.2", true},
		{"unstamped run cannot be shown to match", "", "1.4.2", true},
		{"unknown running build cannot be shown to match", "1.4.2", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, map[string]string{
				"a.go": "package p\n\ntype A struct{}\n",
				"b.go": "package p\n\ntype B struct{}\n",
			})
			// Nothing in the tree changes. The ONLY difference is the build.

			changes, err := diffAgainst(ctx, h.reg, h.runID, h.walker, tc.indexedBy, tc.using)
			if err != nil {
				t.Fatalf("diffAgainst: %v", err)
			}
			if len(changes) == 0 {
				t.Fatal("no files discovered; the fixture is wrong, not the code")
			}

			var same, dirty int
			for _, c := range changes {
				switch c.Kind {
				case ChangeSame:
					same++
				case ChangeModified:
					dirty++
				case ChangeAdded, ChangeDeleted:
					t.Fatalf("%s was reported %s; the tree did not change, only the build did",
						c.Path, c.Kind)
				}
			}

			if tc.wantAllDirty && same > 0 {
				t.Errorf("%d file(s) were reused across a build change; a content hash "+
					"proves the source is unchanged, not that this build extracts the same facts",
					same)
			}
			if !tc.wantAllDirty && dirty > 0 {
				t.Errorf("%d file(s) re-parsed on the same build; that is a full re-index "+
					"every run, which defeats incremental indexing", dirty)
			}
		})
	}
}

// TestIncrementalResolveRestoresEdges pins that an incremental update with
// Resolve: true restores both the edited file's references and its callers'
// incoming edges, rather than leaving references pending and the graph hollowed out.
func TestIncrementalResolveRestoresEdges(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, map[string]string{
		"def.go":    "package p\n\nfunc Target() {}\n",
		"caller.go": "package p\n\nfunc Caller() {\n\tTarget()\n}\n",
	})

	// Initial resolution pass.
	if _, err := Resolve(ctx, h.reg, h.runID, ResolveOptions{}); err != nil {
		t.Fatalf("initial resolve: %v", err)
	}

	edges, err := h.reg.Edges(ctx, h.runID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	if len(edges) == 0 {
		t.Fatalf("expected at least 1 edge initially, got 0")
	}

	// Modify def.go.
	h.write("def.go", "package p\n\nfunc Target() {\n\t// new body\n}\n")

	changes, err := Diff(ctx, h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}

	// Run incremental update with Resolve: true.
	stats, err := Incremental(ctx, h.reg, h.runID, h.walker, changes, IncrementalOptions{Resolve: true})
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if stats.Modified != 1 {
		t.Fatalf("expected 1 modified file, got %d", stats.Modified)
	}

	// Verify the edge between Caller and Target is restored.
	edgesAfter, err := h.reg.Edges(ctx, h.runID)
	if err != nil {
		t.Fatalf("read edges after: %v", err)
	}
	if len(edgesAfter) == 0 {
		t.Fatalf("edges were erased after incremental update and not restored by Resolve")
	}

	// Verify symbol_references has 0 pending references left.
	var pendingCount int
	err = h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM symbol_references WHERE index_run_id = ? AND resolution = 'pending'`,
		h.runID).Scan(&pendingCount)
	if err != nil {
		t.Fatalf("count pending refs: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("expected 0 pending references after incremental resolve, got %d", pendingCount)
	}
}
