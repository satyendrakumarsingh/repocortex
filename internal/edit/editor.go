package edit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	afs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

// DefaultMaxFileBytes bounds one file the editor reads. A source file larger
// than this is almost certainly generated, and a rename has no business
// rewriting it unattended.
const DefaultMaxFileBytes int64 = 8 << 20

// tempPrefix names every temporary file the editor creates, so a test (or a
// user cleaning up after a crash) can find them.
const tempPrefix = ".cortex-edit-"

// FileEdit is every replacement of Old by New in one file.
type FileEdit struct {
	// Path is relative to the project root, forward slashes, as the index
	// records it.
	Path string
	// WantHash is model.File.ContentHash for this path in the indexed run.
	WantHash string
	Old, New string
	// Lines maps a 1-indexed line to the evidence column hints of every site
	// on it (-1 = none). See Locate.
	Lines map[int][]int
}

// Problem is a site or file that cannot be edited safely. Problems are data,
// not errors: the caller lists every one and writes nothing.
type Problem struct {
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"` // 0 for a whole-file problem
	Reason string `json:"reason"`
	Err    error  `json:"-"`
}

// Prepared is one file's new content, computed and not yet written.
type Prepared struct {
	Path     string
	Replaced int

	rel    string // OS-native, cleaned, relative to the root
	before []byte
	after  []byte
	mode   fs.FileMode
}

// Option configures an Editor.
type Option func(*Editor)

// WithMaxFileBytes overrides DefaultMaxFileBytes. Values <= 0 are ignored.
func WithMaxFileBytes(n int64) Option {
	return func(e *Editor) {
		if n > 0 {
			e.maxBytes = n
		}
	}
}

// errTooLarge marks a file over the read limit. Commit's re-check turns it
// into ErrStale: a file larger than both the old and the new text cannot hold
// either.
var errTooLarge = errors.New("edit: file too large")

// faultStage names a point inside a single file replacement at which a test
// can inject a failure.
type faultStage string

const (
	// stageChecked: a path has passed the component check and is about to be
	// opened for reading (in Prepare and in every re-check).
	stageChecked faultStage = "checked"
	// stageTempWritten: the temporary file holds the new bytes, synced and
	// closed; nothing has been re-checked or renamed yet.
	stageTempWritten faultStage = "temp-written"
	// stageBeforeRename: the target has just been re-checked; the next call is
	// the rename.
	stageBeforeRename faultStage = "before-rename"
)

// Editor rewrites files under one root and nowhere else.
//
// Every filesystem operation goes through an os.Root opened on the project
// root, so no path component — including one swapped for a symlink between a
// check and a use — can lead the editor outside it. On top of that the editor
// refuses any path with a symlinked component at all, inside the root or not:
// a rename edits the file the index saw, and a link is a second name for some
// other file.
type Editor struct {
	root     string
	maxBytes int64

	// fault, when set (tests only), is called at each faultStage of every
	// replacement — rollbacks included — and a non-nil return fails that
	// replacement at that point.
	fault func(stage faultStage, rel string) error
}

// New returns an Editor confined to root.
func New(root string, opts ...Option) *Editor {
	e := &Editor{root: root, maxBytes: DefaultMaxFileBytes}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Prepare reads each file inside the root, checks it against the hash the
// index recorded, locates every site and computes the new bytes. It writes
// nothing. A file with any problem contributes no Prepared entry. The error is
// non-nil only when ctx is done or the root itself cannot be opened.
func (e *Editor) Prepare(ctx context.Context, edits []FileEdit) ([]Prepared, []Problem, error) {
	root, err := os.OpenRoot(e.root)
	if err != nil {
		return nil, nil, fmt.Errorf("edit: open project root %s: %w", e.root, err)
	}
	defer func() { _ = root.Close() }()

	var (
		out      []Prepared
		problems []Problem
		seen     = make(map[string]bool, len(edits))
	)
	for _, fe := range edits {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("edit: prepare: %w", err)
		}
		key := filepath.Clean(filepath.FromSlash(fe.Path))
		if seen[key] {
			problems = append(problems, Problem{Path: fe.Path,
				Reason: "listed more than once; all of a file's sites must be in one edit"})
			continue
		}
		seen[key] = true
		p, probs, err := e.prepareOne(ctx, root, fe)
		if err != nil {
			return nil, nil, err
		}
		if len(probs) > 0 {
			problems = append(problems, probs...)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Path != problems[j].Path {
			return problems[i].Path < problems[j].Path
		}
		return problems[i].Line < problems[j].Line
	})
	return out, problems, nil
}

// prepareOne computes one file's edit. Problems are returned as data; the
// error is non-nil only when ctx is done.
func (e *Editor) prepareOne(ctx context.Context, root *os.Root, fe FileEdit) (Prepared, []Problem, error) {
	fail := func(reason string, err error) (Prepared, []Problem, error) {
		return Prepared{}, []Problem{{Path: fe.Path, Reason: reason, Err: err}}, nil
	}
	if fe.Old == "" || fe.New == "" || fe.Old == fe.New {
		return fail(fmt.Sprintf("cannot rename %q to %q", fe.Old, fe.New), nil)
	}
	rel := filepath.Clean(filepath.FromSlash(fe.Path))
	if err := e.contain(rel); err != nil {
		return fail("cannot be read inside the project root: "+err.Error(), err)
	}
	src, mode, err := e.readRegular(root, rel, e.maxBytes)
	if err != nil {
		return fail("cannot be read inside the project root: "+err.Error(), err)
	}
	if afs.HashBytes(src) != fe.WantHash {
		return fail("changed since it was indexed; run 'cortex index' and try again", ErrStale)
	}
	// A read-only file is a statement by its owner (generated, vendored,
	// checked out read-only). Replacing it by rename would ignore that, and
	// on Windows the rename fails outright.
	if mode&0o200 == 0 {
		return fail("file is read-only; make it writable first if it should be renamed", nil)
	}

	lines := make([]int, 0, len(fe.Lines))
	for ln := range fe.Lines {
		lines = append(lines, ln)
	}
	sort.Ints(lines)

	var (
		offsets []int
		probs   []Problem
		text    = NewSource(src)
	)
	for _, ln := range lines {
		if err := ctx.Err(); err != nil {
			return Prepared{}, nil, fmt.Errorf("edit: prepare %s: %w", fe.Path, err)
		}
		at, lerr := text.Locate(ln, fe.Old, fe.Lines[ln])
		if lerr != nil {
			probs = append(probs, Problem{Path: fe.Path, Line: ln, Reason: lerr.Error(), Err: lerr})
			continue
		}
		offsets = append(offsets, at...)
	}
	if len(probs) > 0 {
		return Prepared{}, probs, nil
	}
	if len(offsets) == 0 {
		return fail("no sites listed for this file", nil)
	}
	after, err := Splice(src, fe.Old, fe.New, offsets)
	if err != nil {
		return fail(err.Error(), err)
	}
	// The limit bounds what is written as well as what is read, so every
	// file Commit writes is one a rollback can read back.
	if int64(len(after)) > e.maxBytes {
		return fail(fmt.Sprintf("the edit would make it %d bytes, over the %d-byte limit", len(after), e.maxBytes), nil)
	}
	return Prepared{Path: fe.Path, Replaced: len(offsets), rel: rel, before: src, after: after, mode: mode}, nil, nil
}

// contain runs the textual and resolved containment check of
// security.ContainedPath (relative, no "..", no NUL, no symlinked final
// component, resolves under the root) on rel, the cleaned OS-native relative
// path every os.Root call uses. It is independent of the os.Root confinement:
// either alone refuses an escape.
func (e *Editor) contain(rel string) error {
	if _, err := security.ContainedPath(e.root, rel); err != nil {
		return err
	}
	return nil
}

// checkComponents refuses rel unless every directory on the way to it is a
// real directory and rel itself is a regular file — no symlinks, junctions or
// other reparse points anywhere. It returns the final component's Lstat.
func checkComponents(root *os.Root, rel string) (fs.FileInfo, error) {
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		fi, err := root.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", name, err)
		}
		last := i == len(parts)-1
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("%s is a symlink; symlinked paths are never edited", name)
		case last && !fi.Mode().IsRegular():
			return nil, fmt.Errorf("%s is not a regular file", name)
		case !last && fi.Mode().Type() != fs.ModeDir:
			return nil, fmt.Errorf("%s is not a plain directory", name)
		}
		if last {
			return fi, nil
		}
	}
	return nil, fmt.Errorf("empty path")
}

// readRegular reads at most limit bytes of rel through root after
// checkComponents, and confirms the handle it opened is the very regular file
// it checked, so a swap between the check and the open is caught rather than
// read. The open never blocks: a FIFO swapped in after the check would
// otherwise hang the editor waiting for a writer (see openForRead).
func (e *Editor) readRegular(root *os.Root, rel string, limit int64) ([]byte, fs.FileMode, error) {
	checked, err := checkComponents(root, rel)
	if err != nil {
		return nil, 0, err
	}
	if err := e.injected(stageChecked, rel); err != nil {
		return nil, 0, err
	}
	f, err := openForRead(root, rel)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(checked, info) {
		return nil, 0, fmt.Errorf("%s was replaced while it was being opened", rel)
	}
	if info.Size() > limit {
		return nil, 0, fmt.Errorf("%w: %s is %d bytes, over the %d-byte limit", errTooLarge, rel, info.Size(), limit)
	}
	src, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", rel, err)
	}
	if int64(len(src)) > limit {
		return nil, 0, fmt.Errorf("%w: %s grew past the %d-byte limit while being read", errTooLarge, rel, limit)
	}
	return src, info.Mode().Perm(), nil
}

// Commit writes every prepared file atomically: a temp file in the same
// directory, fsync, a re-check that the target still holds exactly the bytes
// Prepare read, then a rename over it. If any file fails — stale, unwritable,
// cancelled — nothing more is written and every file already replaced is
// restored, newest first.
//
// Rollback deliberately ignores ctx: a cancelled commit must still put back
// what it changed.
//
// Only one Commit runs per project root at a time; see ErrLocked.
func (e *Editor) Commit(ctx context.Context, prepared []Prepared) (err error) {
	root, err := os.OpenRoot(e.root)
	if err != nil {
		return fmt.Errorf("edit: open project root %s: %w", e.root, err)
	}
	defer func() { _ = root.Close() }()

	if err := lock(root); err != nil {
		return err
	}
	defer func() {
		if uerr := root.Remove(lockName); uerr != nil {
			err = errors.Join(err, fmt.Errorf("%w %s (delete it by hand): %w", ErrLockRelease, lockName, uerr))
		}
	}()

	done := make([]Prepared, 0, len(prepared))
	for _, p := range prepared {
		if err := ctx.Err(); err != nil {
			return e.rollback(root, done, fmt.Errorf("edit: commit: %w", err))
		}
		renamed, err := e.swap(root, p.Path, p.rel, p.before, p.after, p.mode)
		if renamed {
			// The new bytes are in place even though something after the
			// rename failed; it has to be restored like the others.
			done = append(done, p)
		}
		if err != nil {
			return e.rollback(root, done, fmt.Errorf("edit: write %s: %w", p.Path, err))
		}
	}
	return nil
}

// lockName is the per-root commit lock, at the top of the project root so
// every editor of that root agrees on where it is whether or not the index
// directory exists.
const lockName = ".cortex-edit.lock"

// ErrLocked: another Commit holds the project root's lock, or one crashed
// and left it behind. The lock is never stolen — a stale one has to be
// deleted by a person who has checked no edit is running.
var ErrLocked = errors.New("edit: another edit is in progress")

// ErrLockRelease: the lock could not be removed after a Commit. It is joined
// to whatever else Commit returns; on its own it means every file was
// written and only the lock was left behind.
var ErrLockRelease = errors.New("edit: release lock")

// lock creates lockName with O_EXCL, recording who holds it. Two commits on
// one root would otherwise interleave inside each other's re-check-to-rename
// window, and the later rename would silently discard the earlier edit.
func lock(root *os.Root) error {
	f, err := root.OpenFile(lockName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%w (%s: %s); if no cortex process is running, delete %s and retry",
			ErrLocked, lockName, describeLock(root), filepath.Join(root.Name(), lockName))
	}
	if err != nil {
		return fmt.Errorf("edit: take lock %s: %w", lockName, err)
	}
	_, werr := fmt.Fprintf(f, "pid=%d\ncreated=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		if rerr := root.Remove(lockName); rerr != nil {
			werr = errors.Join(werr, rerr)
		}
		return fmt.Errorf("edit: write lock %s: %w", lockName, werr)
	}
	return nil
}

// describeLock returns the holder recorded in an existing lock, on one line,
// or why it could not be read. It reads through the same non-blocking,
// regular-file-only path as source files.
func describeLock(root *os.Root) string {
	fi, err := root.Lstat(lockName)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if !fi.Mode().IsRegular() {
		return "not a regular file"
	}
	f, err := openForRead(root, lockName)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

// swap replaces rel, which must hold exactly expect, with data. renamed
// reports whether the rename happened, which can be true alongside an error
// from the directory sync that follows it.
func (e *Editor) swap(root *os.Root, path, rel string, expect, data []byte, mode fs.FileMode) (renamed bool, err error) {
	if err := e.contain(rel); err != nil {
		return false, err
	}
	tmp, err := writeTemp(root, filepath.Dir(rel), data, mode)
	if err != nil {
		return false, err
	}
	defer func() {
		if err != nil {
			if rerr := root.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temp file %s: %w", tmp, rerr))
			}
		}
	}()
	if err = e.injected(stageTempWritten, rel); err != nil {
		return false, err
	}

	// The re-check sits as close to the rename as it can: everything slow
	// (writing and syncing the temp file) is already done.
	// The limit covers both sides: a rollback re-reads a file holding the
	// new bytes, which may be longer than the old.
	limit := int64(max(len(expect), len(data)))
	cur, _, err := e.readRegular(root, rel, limit)
	if errors.Is(err, errTooLarge) {
		err = fmt.Errorf("%w: %s no longer holds the bytes this edit expected", ErrStale, path)
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("re-read before rename: %w", err)
	}
	if !bytes.Equal(cur, expect) {
		err = fmt.Errorf("%w: %s no longer holds the bytes this edit expected", ErrStale, path)
		return false, err
	}
	if err = e.injected(stageBeforeRename, rel); err != nil {
		return false, err
	}
	if err = root.Rename(tmp, rel); err != nil {
		return false, fmt.Errorf("rename over %s: %w", path, err)
	}
	if serr := syncDir(root, filepath.Dir(rel)); serr != nil {
		return true, serr
	}
	return true, nil
}

func (e *Editor) injected(stage faultStage, rel string) error {
	if e.fault == nil {
		return nil
	}
	return e.fault(stage, rel)
}

// writeTemp creates a fresh file next to the target with O_EXCL (so it can
// never open something an attacker planted under the chosen name), writes and
// syncs data, sets mode on the open handle, and returns its root-relative name.
func writeTemp(root *os.Root, dir string, data []byte, mode fs.FileMode) (name string, err error) {
	var f *os.File
	for attempt := 0; ; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("temp file name: %w", err)
		}
		name = filepath.Join(dir, tempPrefix+hex.EncodeToString(suffix[:]))
		f, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt == 9 {
			return "", fmt.Errorf("create temp file in %s: %w", dir, err)
		}
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			if rerr := root.Remove(name); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temp file %s: %w", name, rerr))
			}
		}
	}()
	if _, err = f.Write(data); err != nil {
		return "", fmt.Errorf("write temp file %s: %w", name, err)
	}
	// Chmod on the handle, not the name: no window in which the name could
	// be pointed somewhere else.
	if err = f.Chmod(mode); err != nil {
		return "", fmt.Errorf("chmod temp file %s: %w", name, err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("sync temp file %s: %w", name, err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close temp file %s: %w", name, err)
	}
	return name, nil
}

// syncDir makes a rename in dir durable. Windows cannot fsync a directory
// handle and its rename is already journalled by NTFS, so it is skipped there.
func syncDir(root *os.Root, dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s for sync: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// ErrRollbackIncomplete: a Commit failed and at least one file it had already
// written could not be restored. It is the one Commit error after which the
// tree may hold some of the new text; the joined errors name each such file.
var ErrRollbackIncomplete = errors.New("edit: rollback incomplete")

// rollback restores every file in done to the bytes Prepare read, newest
// first — but only a file that still holds exactly the bytes Commit wrote. A
// file someone changed since is left alone rather than clobbered. The returned
// error names each file that could not be restored, because that is the one
// thing the user must then fix.
func (e *Editor) rollback(root *os.Root, done []Prepared, cause error) error {
	var failed []error
	for i := len(done) - 1; i >= 0; i-- {
		p := done[i]
		if _, err := e.swap(root, p.Path, p.rel, p.after, p.before, p.mode); err != nil {
			failed = append(failed, fmt.Errorf("restore %s (it may still hold the new text): %w", p.Path, err))
		}
	}
	if len(failed) == 0 {
		return cause
	}
	return errors.Join(append([]error{cause, ErrRollbackIncomplete}, failed...)...)
}
