// Package fs is the acquisition layer's front door: it decides which files in a
// repository are worth indexing and fingerprints them.
//
// Two properties govern the design.
//
//  1. THE REPOSITORY IS UNTRUSTED. Everything this package reads — directory
//     entries, symlink targets, .gitignore rules, .cortex.yml globs — is
//     attacker-controlled the moment someone clones a hostile repo. So symlinks
//     are never followed out of the root, config Exclude patterns cannot be
//     negated away, and ignore-file parsing is fuzzed. Spec §33.
//
//  2. OUTPUT MUST BE BYTE-FOR-BYTE REPRODUCIBLE. A determinism gate downstream
//     compares two index runs of the same tree. The walk is concurrent, so
//     discovery order is nondeterministic by construction; results are
//     therefore sorted by path before they leave Walk. Nothing else in the
//     pipeline is allowed to depend on arrival order.
//
// Note the import alias: this package is named fs, so the standard library's
// io/fs is imported as iofs.
package fs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/config"

	"github.com/odvcencio/gotreesitter/grammars"
)

// DefaultMaxFileSize is the largest file that will be hashed and offered to the
// parsers. Generated bundles and checked-in datasets dominate the tail of every
// real repository and yield nothing useful per byte spent.
const DefaultMaxFileSize int64 = 2 << 20 // 2 MiB

// binarySniffLen is git's own heuristic window: a NUL byte within the first
// 8000 bytes means binary. Matching git matters more than being clever here,
// because users reason about RepoCortex's file set by comparing it to git's.
const binarySniffLen = 8000

// Skip reasons. These are stable identifiers, not prose: they end up in the
// index and in `cortex doctor` output, so changing one is a schema change.
const (
	SkipTooLarge      = "too_large"
	SkipBinary        = "binary"
	SkipSymlink       = "symlink"
	SkipSymlinkEscape = "symlink_escapes_root"
	SkipIrregular     = "irregular"
	SkipUnreadable    = "unreadable"
)

// Discovered is one file found by the walk, with everything needed to decide
// whether it changed since the last run. Field names and JSON tags mirror
// model.File so the registry layer can map across without translation.
type Discovered struct {
	// Path is repo-relative and ALWAYS uses forward slashes, on every OS. The
	// content hash of an index must not differ between a Windows and a Linux
	// run of the same commit.
	Path     string `json:"path"`
	Language string `json:"language"` // "" when unrecognised

	// ContentHash is the hex sha256 of the file's bytes. Empty for skipped
	// files: we deliberately do not read a file we have decided not to index,
	// so claiming a hash for it would be a fabrication.
	ContentHash string `json:"content_hash,omitempty"`

	SizeBytes   int64 `json:"size_bytes"`
	ModTimeUnix int64 `json:"mod_time_unix"`

	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason,omitempty"`
}

// Option adjusts a Walker. The knobs here are operational rather than
// repository-scoped, which is why they are not in config.Config: a hostile
// repo must not be able to raise its own size ceiling or spawn workers.
type Option func(*Walker)

// WithMaxFileSize overrides DefaultMaxFileSize. A value <= 0 restores the
// default rather than disabling the limit, because an unbounded limit is a
// memory-exhaustion primitive.
func WithMaxFileSize(n int64) Option {
	return func(w *Walker) {
		if n <= 0 {
			n = DefaultMaxFileSize
		}
		w.maxFileSize = n
	}
}

// WithWorkers overrides the concurrency bound, which defaults to
// runtime.NumCPU(). Mainly for tests that want a deterministic single-threaded
// walk.
func WithWorkers(n int) Option {
	return func(w *Walker) {
		if n < 1 {
			n = 1
		}
		w.workers = n
	}
}

// Walker discovers indexable files beneath a repository root. It is safe for
// concurrent use and holds no state between calls to Walk.
type Walker struct {
	// root is absolute and fully symlink-resolved. Resolving once, here, is
	// what makes the containment check in withinRoot meaningful: on macOS
	// /tmp is itself a symlink to /private/tmp, so an unresolved root would
	// make every file under it look like an escape.
	root string

	include []pattern
	exclude []pattern

	maxFileSize int64
	workers     int
}

// NewWalker compiles cfg's include/exclude globs and validates root.
func NewWalker(root string, cfg config.Config, opts ...Option) (*Walker, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("walker: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("walker: resolve root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("walker: resolve symlinks in root %q: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("walker: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("walker: root %q is not a directory", resolved)
	}

	w := &Walker{
		root:        filepath.Clean(resolved),
		maxFileSize: DefaultMaxFileSize,
		workers:     runtime.NumCPU(),
	}

	include := cfg.Include
	if len(include) == 0 {
		include = []string{"**"}
	}
	for _, g := range include {
		if p, ok := compilePattern(g, ""); ok {
			w.include = append(w.include, p)
		}
	}
	if len(w.include) == 0 {
		// A config whose include list compiled to nothing would silently index
		// an empty repository. Fail loudly instead.
		return nil, fmt.Errorf("walker: index.include %v contains no usable pattern", cfg.Include)
	}

	for _, g := range cfg.Exclude {
		p, ok := compilePattern(g, "")
		if !ok {
			continue
		}
		// SECURITY: exclusions are additive defaults that a repository must
		// never be able to remove. .cortex.yml is attacker-controlled and its
		// exclude list is appended to the defaults (see config.applyRepo), so a
		// repo could otherwise smuggle in "!.git/**" and have RepoCortex index
		// the object store. This is the enforcement point.
		//
		// Negated entries are discarded rather than coerced into positive ones:
		// rewriting "!**" as "**" would honour the letter of the rule while
		// handing a repository a way to blank its own index. An exclude list
		// has no use for negation in the first place.
		if p.negate {
			continue
		}
		w.exclude = append(w.exclude, p)
	}

	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// Root reports the absolute, symlink-resolved repository root.
func (w *Walker) Root() string { return w.root }

// Walk discovers every indexable file beneath the root.
//
// The returned slice is sorted by Path and includes skipped files, so callers
// can distinguish "not present" from "present but not indexed" — the difference
// between a deleted file and an oversized one.
func (w *Walker) Walk(ctx context.Context) ([]Discovered, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	st := &walkState{w: w, ctx: ctx, done: ctx.Done()}
	// One slot per worker beyond the caller's own goroutine, which does real
	// work rather than blocking on a queue.
	if n := w.workers - 1; n > 0 {
		st.sem = make(chan struct{}, n)
	}

	st.walkDir(w.root, "", ruleSet{})
	st.wg.Wait()

	if err := st.firstErr(); err != nil {
		return nil, err
	}

	slices.SortFunc(st.out, func(a, b Discovered) int { return strings.Compare(a.Path, b.Path) })
	return st.out, nil
}

type walkState struct {
	w    *Walker
	ctx  context.Context
	done <-chan struct{} // ctx.Done(), hoisted so the hot path never calls it

	sem chan struct{}
	wg  sync.WaitGroup

	// mu guards both out and err. Results arrive from every worker, and the
	// first error wins so a cancelled walk reports context.Canceled rather
	// than whichever goroutine noticed last.
	mu  sync.Mutex
	out []Discovered
	err error
}

func (st *walkState) fail(err error) {
	st.mu.Lock()
	if st.err == nil {
		st.err = err
	}
	st.mu.Unlock()
}

func (st *walkState) firstErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

func (st *walkState) emit(d Discovered) {
	st.mu.Lock()
	st.out = append(st.out, d)
	st.mu.Unlock()
}

// cancelled is checked once per directory entry, from every worker. A plain
// ctx.Err() would take the context's mutex on each call and turn cancellation
// checking into the walk's contention point; a non-blocking receive on Done
// does not.
func (st *walkState) cancelled() bool {
	select {
	case <-st.done:
		return true
	default:
		return false
	}
}

// walkDir processes one directory. rel is its repo-relative slash path ("" for
// the root) and rules are the ignore rules inherited from its ancestors.
func (st *walkState) walkDir(dir, rel string, rules ruleSet) {
	if st.cancelled() {
		st.fail(st.ctx.Err())
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		// One unreadable directory must not sink the run, but it must not
		// vanish either: an index that silently omits a subtree looks identical
		// to one where the subtree was deleted. Record it and continue.
		if rel == "" {
			st.fail(fmt.Errorf("read repository root %q: %w", dir, err))
			return
		}
		st.emit(Discovered{Path: rel, Skipped: true, SkipReason: SkipUnreadable})
		return
	}

	rules = loadDirRules(rules, dir, rel)

	for _, entry := range entries {
		if st.cancelled() {
			st.fail(st.ctx.Err())
			return
		}

		name := entry.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		childPath := filepath.Join(dir, name)
		isDir := entry.IsDir()

		// Config exclusions are checked first and cannot be negated; see
		// NewWalker. Checking them before the repo's own ignore rules also
		// means a repo cannot make RepoCortex spend time inside node_modules.
		if st.w.configExcluded(childRel, isDir) {
			continue
		}

		if isDir {
			if rules.prunes(childRel) {
				continue
			}
			st.descend(childPath, childRel, rules)
			continue
		}

		if rules.ignored(childRel, false) {
			continue
		}
		if !st.w.included(childRel) {
			continue
		}

		switch {
		case entry.Type()&iofs.ModeSymlink != 0:
			st.emit(st.symlink(childPath, childRel))
		case entry.Type().IsRegular():
			st.emit(st.w.inspect(childPath, childRel, entry))
		default:
			// Sockets, devices and FIFOs. Opening a FIFO blocks until a writer
			// appears, which would hang the indexer indefinitely on a
			// repository that commits one; never open them.
			st.emit(Discovered{Path: childRel, Skipped: true, SkipReason: SkipIrregular})
		}
	}
}

// descend visits a subdirectory, on a new goroutine when a worker slot is free.
//
// Trying the semaphore non-blockingly and otherwise recursing inline is what
// keeps this deadlock-free: a bounded pool fed from a channel can wedge when
// every worker is blocked publishing more work, whereas inline recursion always
// makes progress.
func (st *walkState) descend(dir, rel string, rules ruleSet) {
	select {
	case st.sem <- struct{}{}:
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			defer func() { <-st.sem }()
			st.walkDir(dir, rel, rules)
		}()
	default:
		st.walkDir(dir, rel, rules)
	}
}

// symlink classifies a symbolic link. Links are never followed.
//
// PATH TRAVERSAL IS IN THE THREAT MODEL. A repository can commit a symlink
// named `config.go` pointing at /etc/passwd or ../../secrets; following it
// would hash and index host data, and an agent would then happily quote it. We
// resolve the target purely to report *why* the link was skipped, and the
// containment test uses the symlink-resolved root so that macOS's /tmp ->
// /private/tmp does not read as an escape.
//
// Links that stay inside the root are skipped too. Following them cannot leak
// data, but it creates cycles and indexes the same content under two paths,
// and git does not follow them either.
func (st *walkState) symlink(full, rel string) Discovered {
	d := Discovered{Path: rel, Skipped: true, SkipReason: SkipSymlink}
	d.Language = languageOf(rel)

	target, err := filepath.EvalSymlinks(full)
	if err != nil {
		// Broken or cyclic link. Unresolvable is not the same as escaping, but
		// both are skipped.
		d.SkipReason = SkipUnreadable
		return d
	}
	if !withinRoot(st.w.root, target) {
		d.SkipReason = SkipSymlinkEscape
	}
	return d
}

// withinRoot reports whether target is root or lies beneath it. Both arguments
// must already be absolute, cleaned and symlink-resolved.
func withinRoot(root, target string) bool {
	target = filepath.Clean(target)
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// inspect stats, sizes, sniffs and hashes a regular file.
func (w *Walker) inspect(full, rel string, entry iofs.DirEntry) Discovered {
	d := Discovered{Path: rel, Language: languageOf(rel)}

	info, err := entry.Info()
	if err != nil {
		d.Skipped, d.SkipReason = true, SkipUnreadable
		return d
	}
	d.SizeBytes = info.Size()
	d.ModTimeUnix = info.ModTime().Unix()

	if d.SizeBytes > w.maxFileSize {
		d.Skipped, d.SkipReason = true, SkipTooLarge
		return d
	}

	sum, binary, err := hashFile(full, w.maxFileSize)
	switch {
	case errors.Is(err, errTooLarge):
		d.Skipped, d.SkipReason = true, SkipTooLarge
	case err != nil:
		d.Skipped, d.SkipReason = true, SkipUnreadable
	case binary:
		d.Skipped, d.SkipReason = true, SkipBinary
	default:
		d.ContentHash = sum
	}
	return d
}

// errTooLarge signals that a file outgrew the limit between stat and read.
var errTooLarge = errors.New("file exceeds size limit")

// hashFile returns the hex sha256 of a file, or reports it as binary.
//
// Size is re-checked while streaming rather than trusted from the earlier
// stat: a file can grow between the two, whether through an ordinary
// concurrent write or a deliberate one, and the stat'd size is not a bound on
// what we are about to buffer.
//
// A binary file stops the read at the sniff window instead of hashing to the
// end. There is nothing to gain from the digest of a file we will not index,
// and repositories that commit hundreds of megabytes of binaries are common.
func hashFile(name string, limit int64) (sum string, binary bool, err error) {
	f, err := os.Open(name) //nolint:gosec // name comes from a walked directory entry
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	buf := make([]byte, 32*1024)
	var total, sniffed int64

	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if sniffed < binarySniffLen {
				window := chunk
				if sniffed+int64(len(window)) > binarySniffLen {
					window = window[:binarySniffLen-sniffed]
				}
				if bytes.IndexByte(window, 0) >= 0 {
					return "", true, nil
				}
				sniffed += int64(len(window))
			}
			total += int64(n)
			if total > limit {
				return "", false, errTooLarge
			}
			h.Write(chunk)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", false, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), false, nil
}

// languageOf delegates to the grammar registry rather than keeping a local
// extension table. It already resolves exact filenames (Dockerfile, Makefile)
// ahead of suffixes, and a second table here would drift from the one the
// parsers actually use.
func languageOf(rel string) string {
	if e := grammars.DetectLanguage(path.Base(rel)); e != nil {
		return e.Name
	}
	return ""
}

// configExcluded reports whether a config Exclude glob covers the entry. For
// directories it also honours the "dir/**" form so the subtree is pruned rather
// than walked and discarded.
func (w *Walker) configExcluded(rel string, isDir bool) bool {
	for i := range w.exclude {
		if isDir {
			if w.exclude[i].coversDir(rel) {
				return true
			}
			continue
		}
		if w.exclude[i].match(rel, false) {
			return true
		}
	}
	return false
}

// included reports whether a file matches at least one Include glob.
//
// Include is only applied to files. A restrictive pattern such as "**/*.go"
// matches no directory, so testing directories against it would prune the
// entire tree at depth one.
func (w *Walker) included(rel string) bool {
	for i := range w.include {
		if w.include[i].match(rel, false) {
			return true
		}
	}
	return false
}
