package edit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	afs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
)

const (
	storeSrc = "package app\n\nfunc (r *Repo) Save() {}\n"
	svcSrc   = "package app\n\nfunc Run(r *Repo) { r.Save() }\n"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func editFor(rel, body string, lines map[int][]int) FileEdit {
	return FileEdit{Path: rel, WantHash: afs.HashBytes([]byte(body)), Old: "Save", New: "Store", Lines: lines}
}

func noTempFiles(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && (strings.HasPrefix(d.Name(), ".cortex-edit-") || d.Name() == lockName) {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
}

func TestPrepareAndCommitRewriteOnlyTheSites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "store.go", storeSrc)
	writeFile(t, root, "svc/run.go", svcSrc)

	ed := New(root)
	prepared, problems, err := ed.Prepare(ctx, []FileEdit{
		editFor("svc/run.go", svcSrc, map[int][]int{3: {20}}), // expression-start column, as tree-sitter records it
		editFor("store.go", storeSrc, map[int][]int{3: {-1}}),
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("Prepare: err=%v problems=%+v", err, problems)
	}
	if len(prepared) != 2 || prepared[0].Path != "store.go" || prepared[0].Replaced != 1 {
		t.Fatalf("prepared = %+v, want two files sorted by path", prepared)
	}
	if readFile(t, root, "store.go") != storeSrc {
		t.Fatal("Prepare wrote to disk")
	}

	if err := ed.Commit(ctx, prepared); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := readFile(t, root, "store.go"); got != "package app\n\nfunc (r *Repo) Store() {}\n" {
		t.Errorf("store.go = %q", got)
	}
	if got := readFile(t, root, "svc/run.go"); got != "package app\n\nfunc Run(r *Repo) { r.Store() }\n" {
		t.Errorf("svc/run.go = %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(root, "store.go")); err != nil || fi.Mode().Perm() != 0o640 {
			t.Errorf("mode not preserved: %v %v", fi.Mode(), err)
		}
	}
	noTempFiles(t, root)
}

func TestPrepareRefusesWhatItCannotDoSafely(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "store.go", storeSrc)
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte(storeSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		edit    FileEdit
		opts    []Option
		wantErr error
		reason  string
	}{
		{name: "stale hash", edit: FileEdit{Path: "store.go", WantHash: "deadbeef", Old: "Save", New: "Store", Lines: map[int][]int{3: {-1}}},
			wantErr: ErrStale, reason: "changed since it was indexed"},
		{name: "dot-dot escape", edit: editFor("../outside.go", storeSrc, map[int][]int{3: {-1}}), reason: "inside the project root"},
		{name: "absolute path", edit: editFor(outside, storeSrc, map[int][]int{3: {-1}}), reason: "inside the project root"},
		{name: "missing file", edit: editFor("gone.go", storeSrc, map[int][]int{3: {-1}}), reason: "inside the project root"},
		{name: "identifier not on the line", edit: editFor("store.go", storeSrc, map[int][]int{1: {-1}}), wantErr: ErrNotFound},
		{name: "over the size limit", edit: editFor("store.go", storeSrc, map[int][]int{3: {-1}}), opts: []Option{WithMaxFileBytes(8)}, reason: "byte limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prepared, problems, err := New(root, tc.opts...).Prepare(ctx, []FileEdit{tc.edit})
			if err != nil {
				t.Fatalf("Prepare returned an error instead of a problem: %v", err)
			}
			if len(prepared) != 0 || len(problems) != 1 {
				t.Fatalf("prepared=%d problems=%+v, want 0 and 1", len(prepared), problems)
			}
			if tc.wantErr != nil && !errors.Is(problems[0].Err, tc.wantErr) {
				t.Errorf("problem err = %v, want %v", problems[0].Err, tc.wantErr)
			}
			if tc.reason != "" && !strings.Contains(problems[0].Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", problems[0].Reason, tc.reason)
			}
			if readFile(t, root, "store.go") != storeSrc {
				t.Error("a refused edit touched the file")
			}
		})
	}
}

func TestPrepareRefusesASymlinkOutOfTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs elevated privileges on Windows")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte(storeSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	_, problems, err := New(root).Prepare(context.Background(),
		[]FileEdit{editFor("link.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil || len(problems) != 1 {
		t.Fatalf("err=%v problems=%+v, want one problem", err, problems)
	}
	if b, _ := os.ReadFile(outside); string(b) != storeSrc {
		t.Error("a file outside the root was modified")
	}
}

// TestCommitRollsBackWhenAFileChangesUnderIt: a.go is written, then b.go turns
// out to have been edited since Prepare. b.go must keep the user's edit and
// a.go must be put back, so the tree is never left half-renamed.
func TestCommitRollsBackWhenAFileChangesUnderIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	writeFile(t, root, "b.go", svcSrc)
	ed := New(root)
	prepared, problems, err := ed.Prepare(ctx, []FileEdit{
		editFor("a.go", storeSrc, map[int][]int{3: {-1}}),
		editFor("b.go", svcSrc, map[int][]int{3: {-1}}),
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("Prepare: %v %+v", err, problems)
	}
	userEdit := svcSrc + "// edited by a human\n"
	writeFile(t, root, "b.go", userEdit)

	err = ed.Commit(ctx, prepared)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Commit err = %v, want ErrStale", err)
	}
	if got := readFile(t, root, "a.go"); got != storeSrc {
		t.Errorf("a.go was not rolled back: %q", got)
	}
	if got := readFile(t, root, "b.go"); got != userEdit {
		t.Errorf("b.go lost the user's edit: %q", got)
	}
	noTempFiles(t, root)
}

func TestCommitHonoursCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(context.Background(), []FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ed.Commit(ctx, prepared); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit err = %v, want context.Canceled", err)
	}
	if readFile(t, root, "a.go") != storeSrc {
		t.Error("a cancelled commit wrote")
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs elevated privileges on Windows")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareRefusesSymlinkedPaths: a link is a second name for some other
// file. Out of the root it is an escape; inside it, it is still not the file
// the index saw. Both are refused, for a symlinked directory as well as a
// symlinked file.
func TestPrepareRefusesSymlinkedPaths(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root, outsideDir string) string
	}{
		{name: "directory linked out of the root", setup: func(t *testing.T, root, outsideDir string) string {
			symlinkOrSkip(t, outsideDir, filepath.Join(root, "ext"))
			return "ext/outside.go"
		}},
		{name: "directory linked inside the root", setup: func(t *testing.T, root, _ string) string {
			writeFile(t, root, "real/store.go", storeSrc)
			symlinkOrSkip(t, filepath.Join(root, "real"), filepath.Join(root, "alias"))
			return "alias/store.go"
		}},
		{name: "file linked inside the root", setup: func(t *testing.T, root, _ string) string {
			writeFile(t, root, "real.go", storeSrc)
			symlinkOrSkip(t, "real.go", filepath.Join(root, "alias.go"))
			return "alias.go"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, outsideDir := t.TempDir(), t.TempDir()
			outside := filepath.Join(outsideDir, "outside.go")
			if err := os.WriteFile(outside, []byte(storeSrc), 0o600); err != nil {
				t.Fatal(err)
			}
			rel := tc.setup(t, root, outsideDir)
			prepared, problems, err := New(root).Prepare(context.Background(),
				[]FileEdit{editFor(rel, storeSrc, map[int][]int{3: {-1}})})
			if err != nil || len(prepared) != 0 || len(problems) != 1 {
				t.Fatalf("err=%v prepared=%d problems=%+v, want one problem", err, len(prepared), problems)
			}
			if b, _ := os.ReadFile(outside); string(b) != storeSrc {
				t.Error("a file outside the root was modified")
			}
		})
	}
}

func TestPrepareRefusesADuplicatePathAndBadNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	same := editFor("a.go", storeSrc, map[int][]int{3: {-1}})
	same.New = "Save"
	_, problems, err := New(root).Prepare(context.Background(), []FileEdit{
		editFor("a.go", storeSrc, map[int][]int{3: {-1}}),
		editFor("./a.go", storeSrc, map[int][]int{3: {-1}}),
	})
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0].Reason, "more than once") {
		t.Fatalf("duplicate: err=%v problems=%+v", err, problems)
	}
	_, problems, err = New(root).Prepare(context.Background(), []FileEdit{same})
	if err != nil || len(problems) != 1 {
		t.Fatalf("same name: err=%v problems=%+v", err, problems)
	}
}

// TestCommitRollsBackWhenALaterRenameFails: the failure is not staleness but
// the write itself. Every stage of a replacement is exercised, so the
// temp-file cleanup is checked on each error path.
func TestCommitRollsBackWhenALaterRenameFails(t *testing.T) {
	for _, stage := range []faultStage{stageTempWritten, stageBeforeRename} {
		t.Run(string(stage), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			writeFile(t, root, "a.go", storeSrc)
			writeFile(t, root, "b.go", svcSrc)
			ed := New(root)
			prepared, problems, err := ed.Prepare(ctx, []FileEdit{
				editFor("a.go", storeSrc, map[int][]int{3: {-1}}),
				editFor("b.go", svcSrc, map[int][]int{3: {-1}}),
			})
			if err != nil || len(problems) != 0 {
				t.Fatalf("Prepare: %v %+v", err, problems)
			}
			boom := errors.New("disk full")
			var aWritten bool
			ed.fault = func(s faultStage, rel string) error {
				if rel == "b.go" && s == stage {
					aWritten = readFile(t, root, "a.go") != storeSrc
					return boom
				}
				return nil
			}
			err = ed.Commit(ctx, prepared)
			if !errors.Is(err, boom) {
				t.Fatalf("Commit err = %v, want the injected failure", err)
			}
			if !aWritten {
				t.Fatal("a.go was not written before b.go failed; the rollback was not exercised")
			}
			if got := readFile(t, root, "a.go"); got != storeSrc {
				t.Errorf("a.go was not rolled back: %q", got)
			}
			if got := readFile(t, root, "b.go"); got != svcSrc {
				t.Errorf("b.go changed: %q", got)
			}
			noTempFiles(t, root)
		})
	}
}

// TestCommitRollsBackOnARealWriteFailure: no injected fault — b.go's
// directory is read-only, so its temp file cannot be created.
func TestCommitRollsBackOnARealWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions enforced for the current user")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a/a.go", storeSrc)
	writeFile(t, root, "z/b.go", svcSrc)
	ed := New(root)
	prepared, problems, err := ed.Prepare(ctx, []FileEdit{
		editFor("a/a.go", storeSrc, map[int][]int{3: {-1}}),
		editFor("z/b.go", svcSrc, map[int][]int{3: {-1}}),
	})
	if err != nil || len(problems) != 0 {
		t.Fatalf("Prepare: %v %+v", err, problems)
	}
	zdir := filepath.Join(root, "z")
	if err := os.Chmod(zdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(zdir, 0o755) })

	if err := ed.Commit(ctx, prepared); err == nil {
		t.Fatal("Commit succeeded into a read-only directory")
	}
	if got := readFile(t, root, "a/a.go"); got != storeSrc {
		t.Errorf("a/a.go was not rolled back: %q", got)
	}
	if got := readFile(t, root, "z/b.go"); got != svcSrc {
		t.Errorf("z/b.go changed: %q", got)
	}
	noTempFiles(t, root)
}

// TestRollbackDoesNotClobberALaterEdit: if a.go is edited by someone else
// between its rename and the rollback, the rollback leaves it alone and says
// so, rather than overwriting their work with the pre-rename text.
func TestRollbackDoesNotClobberALaterEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	writeFile(t, root, "b.go", svcSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{
		editFor("a.go", storeSrc, map[int][]int{3: {-1}}),
		editFor("b.go", svcSrc, map[int][]int{3: {-1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	human := "// a human got here first\n"
	ed.fault = func(s faultStage, rel string) error {
		if rel == "b.go" && s == stageBeforeRename {
			writeFile(t, root, "a.go", human)
			return errors.New("injected")
		}
		return nil
	}
	err = ed.Commit(ctx, prepared)
	if !errors.Is(err, ErrRollbackIncomplete) || !strings.Contains(err.Error(), "restore a.go") {
		t.Fatalf("Commit err = %v, want an incomplete rollback naming a.go", err)
	}
	if got := readFile(t, root, "a.go"); got != human {
		t.Errorf("rollback clobbered a.go: %q", got)
	}
	noTempFiles(t, root)
}

// TestCommitRefusesATargetSwappedForASymlink: between Prepare and Commit the
// file is replaced by a link to an outside file with identical bytes. The
// content check alone would pass; the component check must not.
func TestCommitRefusesATargetSwappedForASymlink(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte(storeSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "a.go"))

	if err := ed.Commit(ctx, prepared); err == nil {
		t.Fatal("Commit wrote through a symlink")
	}
	if b, _ := os.ReadFile(outside); string(b) != storeSrc {
		t.Error("a file outside the root was modified")
	}
	if fi, err := os.Lstat(filepath.Join(root, "a.go")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link itself was replaced: %v %v", fi, err)
	}
	noTempFiles(t, root)
}

const growSrc = "package app\n\nfunc (r *Repo) Save() {}\n"

func growEdit(rel string) FileEdit {
	return FileEdit{Path: rel, WantHash: afs.HashBytes([]byte(growSrc)), Old: "Save", New: "StoreLonger",
		Lines: map[int][]int{3: {-1}}}
}

// TestPrepareRefusesAnEditThatGrowsPastTheLimit: the limit bounds what the
// editor writes as well as what it reads, so a rollback can always read back
// what Commit wrote.
func TestPrepareRefusesAnEditThatGrowsPastTheLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", growSrc)
	prepared, problems, err := New(root, WithMaxFileBytes(int64(len(growSrc)+2))).
		Prepare(context.Background(), []FileEdit{growEdit("a.go")})
	if err != nil || len(prepared) != 0 || len(problems) != 1 || !strings.Contains(problems[0].Reason, "byte limit") {
		t.Fatalf("err=%v prepared=%d problems=%+v, want one byte-limit problem", err, len(prepared), problems)
	}
}

// TestRollbackRestoresAFileTheEditGrew: the restore re-reads a file that
// holds the NEW bytes, so its read limit must cover them even when they are
// over the editor's limit (here because the limit changed after Prepare).
func TestRollbackRestoresAFileTheEditGrew(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", growSrc)
	writeFile(t, root, "b.go", growSrc)
	ed := New(root)
	prepared, problems, err := ed.Prepare(ctx, []FileEdit{growEdit("a.go"), growEdit("b.go")})
	if err != nil || len(problems) != 0 {
		t.Fatalf("Prepare: %v %+v", err, problems)
	}
	ed.maxBytes = int64(len(growSrc) + 2)
	ed.fault = func(s faultStage, rel string) error {
		if rel == "b.go" && s == stageBeforeRename {
			return errors.New("injected")
		}
		return nil
	}
	err = ed.Commit(ctx, prepared)
	if err == nil || errors.Is(err, ErrRollbackIncomplete) {
		t.Fatalf("Commit err = %v, want the injected failure and a complete rollback", err)
	}
	if got := readFile(t, root, "a.go"); got != growSrc {
		t.Errorf("a.go was not restored: %q", got)
	}
	noTempFiles(t, root)
}

func TestPrepareRefusesAReadOnlyFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	p := filepath.Join(root, "a.go")
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	prepared, problems, err := New(root).Prepare(context.Background(),
		[]FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil || len(prepared) != 0 || len(problems) != 1 || !strings.Contains(problems[0].Reason, "read-only") {
		t.Fatalf("err=%v prepared=%d problems=%+v, want one read-only problem", err, len(prepared), problems)
	}
}

// cancelAfter is a context whose Err turns to Canceled after n calls, to
// cancel in the middle of one file deterministically.
type cancelAfter struct {
	context.Context
	n int
}

func (c *cancelAfter) Err() error {
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	return nil
}

func TestPrepareHonoursCancellationWithinAFile(t *testing.T) {
	root := t.TempDir()
	src, lines := largeSource(5000)
	writeFile(t, root, "big.go", string(src))
	fe := FileEdit{Path: "big.go", WantHash: afs.HashBytes(src), Old: "Save", New: "Store", Lines: lines}
	_, _, err := New(root).Prepare(&cancelAfter{Context: context.Background(), n: 100}, []FileEdit{fe})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare err = %v, want context.Canceled", err)
	}
}

// TestPrepareOfALargeFileIsLinear: 40,000 sites in a 2.4 MB file go through
// Prepare, which must index the file's lines once.
func TestPrepareOfALargeFileIsLinear(t *testing.T) {
	root := t.TempDir()
	src, lines := largeSource(40000)
	writeFile(t, root, "big.go", string(src))
	fe := FileEdit{Path: "big.go", WantHash: afs.HashBytes(src), Old: "Save", New: "Store", Lines: lines}
	start := time.Now()
	prepared, problems, err := New(root).Prepare(context.Background(), []FileEdit{fe})
	if err != nil || len(problems) != 0 || len(prepared) != 1 || prepared[0].Replaced != 40000 {
		t.Fatalf("Prepare: err=%v problems=%d prepared=%+v", err, len(problems), prepared)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Prepare of %d bytes, 40000 sites took %v; want well under 1s", len(src), took)
	}
}

// TestCommitRefusesADirectorySwappedInForTheFile: rename(2) would happily
// fail or, on some systems, succeed over an empty directory; neither is an
// edit of the file the index saw.
func TestCommitRefusesADirectorySwappedInForTheFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "a.go")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ed.Commit(ctx, prepared); err == nil {
		t.Fatal("Commit replaced a directory")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Errorf("a.go is no longer the directory: %v %v", fi, err)
	}
	noTempFiles(t, root)
}

// TestADifferentlyCasedPathDoesNotRenameTheEntry: on a case-insensitive
// filesystem STORE.go opens store.go. Whatever the filesystem, the entry the
// user has must keep its name.
func TestADifferentlyCasedPathDoesNotRenameTheEntry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "store.go", storeSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{editFor("STORE.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	if err := ed.Commit(ctx, prepared); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "store.go" {
		t.Fatalf("directory holds %v, want exactly [store.go]", names)
	}
}

// TestASecondCommitFailsFastWhileOneIsInProgress: two editors prepared from
// the same bytes. Without a lock the second commits inside the first's
// re-check-to-rename window and the first then overwrites it — a lost update
// neither reports.
func TestASecondCommitFailsFastWhileOneIsInProgress(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	fe := func(n string) FileEdit {
		f := editFor("a.go", storeSrc, map[int][]int{3: {-1}})
		f.New = n
		return f
	}
	e1, e2 := New(root), New(root)
	p1, _, err := e1.Prepare(ctx, []FileEdit{fe("One")})
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := e2.Prepare(ctx, []FileEdit{fe("Two")})
	if err != nil {
		t.Fatal(err)
	}
	var err2 error
	e1.fault = func(s faultStage, _ string) error {
		if s == stageBeforeRename {
			err2 = e2.Commit(ctx, p2)
		}
		return nil
	}
	if err := e1.Commit(ctx, p1); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if !errors.Is(err2, ErrLocked) || !strings.Contains(err2.Error(), "another edit is in progress") ||
		!strings.Contains(err2.Error(), fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Fatalf("second Commit err = %v, want ErrLocked naming this pid", err2)
	}
	if got := readFile(t, root, "a.go"); got != "package app\n\nfunc (r *Repo) One() {}\n" {
		t.Errorf("a.go = %q", got)
	}
	noTempFiles(t, root)

	// Released: the second editor's plan is now stale, which it must say.
	if err := e2.Commit(ctx, p2); !errors.Is(err, ErrStale) {
		t.Fatalf("second Commit after release: %v, want ErrStale", err)
	}
	noTempFiles(t, root)
}

// TestAStaleLockIsNotStolen: a lock left by a crashed run blocks every
// commit until a person removes it, and the error says how.
func TestAStaleLockIsNotStolen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	stale := "pid=99999999\ncreated=2001-01-01T00:00:00Z\n"
	writeFile(t, root, lockName, stale)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	err = ed.Commit(ctx, prepared)
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "pid=99999999") ||
		!strings.Contains(err.Error(), "delete") {
		t.Fatalf("Commit err = %v, want ErrLocked quoting the lock and saying how to clear it", err)
	}
	if got := readFile(t, root, lockName); got != stale {
		t.Errorf("the lock was touched: %q", got)
	}
	if got := readFile(t, root, "a.go"); got != storeSrc {
		t.Errorf("a.go was written under someone else's lock: %q", got)
	}
}

func TestCommitReleasesTheLockOnFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	ed := New(root)
	prepared, _, err := ed.Prepare(ctx, []FileEdit{editFor("a.go", storeSrc, map[int][]int{3: {-1}})})
	if err != nil {
		t.Fatal(err)
	}
	ed.fault = func(s faultStage, _ string) error {
		if s == stageBeforeRename {
			return errors.New("injected")
		}
		return nil
	}
	if err := ed.Commit(ctx, prepared); err == nil {
		t.Fatal("Commit ignored the injected failure")
	}
	noTempFiles(t, root)
	ed.fault = nil
	if err := ed.Commit(ctx, prepared); err != nil {
		t.Fatalf("Commit after a failed one: %v", err)
	}
	noTempFiles(t, root)
}

// TestOnDiskSpellingKeepsTheEntrysName: where the filesystem ignores case, a
// differently spelled path that opens a file resolves to the spelling the
// directory holds. onDiskSpelling is only ever called with a path that was just
// read, so on a case-sensitive filesystem the differently spelled path is not a
// valid input and there is nothing to assert.
func TestOnDiskSpellingKeepsTheEntrysName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sub/store.go", storeSrc)
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	if got := onDiskSpelling(r, filepath.Join("sub", "store.go")); got != filepath.Join("sub", "store.go") {
		t.Errorf("an exact spelling changed: %q", got)
	}
	if _, err := r.Stat(filepath.Join("SUB", "STORE.GO")); err != nil {
		t.Skip("case-sensitive filesystem: a differently cased path does not open the file")
	}
	if got := onDiskSpelling(r, filepath.Join("SUB", "STORE.GO")); got != filepath.Join("sub", "store.go") {
		t.Errorf("case-insensitive filesystem: got %q, want sub/store.go", got)
	}
}
