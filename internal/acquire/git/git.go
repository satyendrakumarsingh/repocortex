// Package git acquires repository metadata for the RepoCortex indexer.
//
// Three consumers shape everything here:
//
//  1. Provenance — an IndexRun is stamped with the revision it describes, so a
//     stale index can be recognised instead of silently trusted.
//  2. Recency ranking — the context engine boosts code that changed recently,
//     which needs per-file "when was this last touched" data.
//  3. Incremental re-indexing — knowing exactly which paths changed between two
//     revisions, including renames, is what makes re-indexing cheap. A rename
//     reported as delete+add would make the indexer destroy and rebuild every
//     symbol in the file rather than move it.
//
// # Two hard constraints
//
// Pure Go. Everything goes through github.com/go-git/go-git/v5. RepoCortex ships
// CGO_ENABLED=0 binaries, and it must not shell out to a `git` binary: the
// binary may be absent, it may behave differently across versions, and spawning
// subprocesses from a tool that runs over untrusted repositories is an attack
// surface this product declines to open.
//
// Not being a git repository is normal, not an error. RepoCortex indexes plain
// directories, extracted tarballs and vendored trees. [Open] therefore succeeds
// on a non-repository and reports IsRepo=false; callers branch on a boolean,
// never on the text of an error.
//
// # Performance
//
// go-git is O(history) for anything resembling `git log`, and it has no index
// behind path-scoped queries. Every walk here takes an explicit limit, checks
// [context.Context] cancellation between commits, and bounds rename detection.
//
// Measured on a synthetic 500-commit, 200-file repository (see bench_test.go;
// Apple M-series, go-git v5.19.2, ns/op converted to ms):
//
//	Open                          1 ms
//	Info (includes worktree scan) 15 ms
//	ChangedSince HEAD~1..HEAD     17 ms
//	ChangedSince HEAD~200..HEAD   93 ms
//	RecentCommitsMeta(50)         16 ms
//	RecentCommits(50) with paths  733 ms
//	RecentCommits(500) with paths 7.5 s
//	FileLastModified, recent file 3 ms
//	FileLastModified, absent path 635 ms   (full 500-commit walk)
//	go-git Blame, one file        2.0 s    — not exposed; see BenchmarkGoGitBlame
//
// The single number that governs everything is the tree diff: roughly 8–17 ms
// per commit pair on a 200-file tree, and it is CPU-bound in go-git's tree
// decoding rather than I/O-bound — repacking the repository changes it by less
// than 10%. That is why RecentCommitsMeta exists (~50x cheaper than
// RecentCommits) and why callers that need per-commit paths for more than a few
// dozen commits should persist them rather than recompute them.
//
// Blame is deliberately absent. Two seconds for one file on a 500-commit
// repository is not a per-file cost an indexer can pay, and it grows with
// history; the benchmark stays in the tree as the evidence, so the decision can
// be revisited against measurements rather than impressions.
//
// # Paths
//
// Every path returned by this package is repo-relative and forward-slashed on
// every platform, matching model.File.Path. Callers may use returned paths as
// database keys directly.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Sentinel errors. They are values, not message text: callers use errors.Is,
// never string matching. See the package comment on why "not a repository" is
// reported through Info.IsRepo rather than through one of these.
var (
	// ErrNotRepository is returned by the history and diff methods when the
	// Repo was opened on a directory that is not under git control. Info does
	// not return it — it reports IsRepo=false instead, because "is this a
	// repository?" is a question, not a failure.
	ErrNotRepository = errors.New("git: path is not a repository")

	// ErrUnboundedWalk is returned when a commit walk is requested without a
	// positive limit. Refusing is deliberate: an accidental unbounded walk on a
	// kernel-sized repository takes minutes, and a hang is a worse bug than a
	// loud error.
	ErrUnboundedWalk = errors.New("git: commit walk requires a positive limit")

	// ErrNoHistory means the reachable history was exhausted without finding
	// any commit that touched the requested path.
	ErrNoHistory = errors.New("git: no commit in history touched this path")

	// ErrScanLimit means the search stopped at MaxHistoryScan commits without
	// an answer. Distinct from ErrNoHistory on purpose: this one says "we gave
	// up", not "it isn't there", and callers may reasonably treat it as unknown
	// rather than as absent.
	ErrScanLimit = errors.New("git: history scan limit reached without a match")
)

// MaxHistoryScan bounds path-scoped history searches. Without it,
// FileLastModified on a path that was never committed walks the entire
// history — the exact O(history) trap this package exists to avoid.
const MaxHistoryScan = 4096

// renameLimit caps rename detection at N deletions x N additions comparisons
// per diff. Rename detection is quadratic in (deleted x added) files, so a
// commit that moves a whole directory can otherwise dominate the run. Beyond
// this bound go-git reports the moves as delete+add, which degrades incremental
// indexing to a rebuild of those files rather than breaking it.
const renameLimit = 500

// Info is the summary of a repository's current state. It is cheap to describe
// and moderately expensive to compute: Dirty requires a full worktree status
// walk. See BenchmarkInfo.
type Info struct {
	// IsRepo is false when the opened path is not under git control. All other
	// fields are then zero. This is a supported, non-error state.
	IsRepo bool `json:"is_repo"`

	// Root is the absolute path of the worktree root (or of the opened
	// directory when IsRepo is false). Returned paths are relative to it.
	Root string `json:"root"`

	// HeadSHA is the full 40-character revision of HEAD, or "" in a repository
	// with no commits yet.
	HeadSHA string `json:"head_sha"`

	// Branch is the current branch name, or "" when HEAD is detached. A
	// freshly-initialised repository with no commits still reports its branch,
	// because HEAD is a symbolic ref before it is a hash.
	Branch string `json:"branch"`

	// Detached reports that HEAD points straight at a commit. Distinguishing
	// this from an empty repository matters: both have Branch=="", but only one
	// has a HeadSHA.
	Detached bool `json:"detached"`

	// Dirty reports uncommitted changes anywhere in the worktree or index.
	// An index stamped with a dirty revision does not describe that revision,
	// so this must be recorded alongside HeadSHA, not inferred from it.
	Dirty bool `json:"dirty"`
}

// Repo is a handle on a repository (or on a directory that turned out not to be
// one). It is safe for concurrent reads; go-git's object storer is read-only
// here and none of these methods mutate the repository.
type Repo struct {
	root   string
	isRepo bool
	repo   *gogit.Repository
	gitDir string

	// degraded records why the go-git repository could not be opened, when the
	// path IS a repository. HEAD is then read from .git directly and history
	// features are unavailable. Empty means fully open.
	degraded string
}

// Degraded reports why full git support is unavailable, or "" when it is not.
//
// Callers should surface this. A repository RepoCortex cannot fully open still
// indexes — the code is on disk either way — but it loses history, and losing a
// feature silently is how a user ends up trusting an answer the tool never
// computed.
func (r *Repo) Degraded() string { return r.degraded }

// Open opens the repository containing path.
//
// A directory that is not under git control is not an error: the returned Repo
// reports IsRepo()==false and Info returns IsRepo=false with a nil error. Only
// genuine failures (unreadable path, corrupt repository) produce an error.
//
// Parent directories are searched for .git, matching what the git CLI does, so
// opening a subdirectory of a checkout yields the enclosing repository. Use
// Root to learn which repository was actually found.
func Open(path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("git: resolve %q: %w", path, err)
	}

	repo, err := gogit.PlainOpenWithOptions(abs, &gogit.PlainOpenOptions{
		DetectDotGit: true,
		// Linked worktrees (`git worktree add`) store .git as a file pointing
		// at a commondir. Without this they open but cannot resolve refs.
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryNotExists) {
			return &Repo{root: abs}, nil
		}
		// go-git refuses repositories using extensions it does not implement.
		// `extensions.worktreeConfig`, which git itself enables the first time
		// you run `git worktree add`, is the common one, and it made
		// `cortex index` record no revision at all on a repository whose
		// HEAD was perfectly readable — silently, because the caller treated
		// any error as "not a repository".
		//
		// The revision is provenance: it answers "which commit does this index
		// describe". It is worth reading from the files directly rather than
		// discarding because a library declined the container.
		if gitDir, ok := gitDirFor(abs); ok {
			return &Repo{root: abs, isRepo: true, degraded: err.Error(), gitDir: gitDir}, nil
		}
		return nil, fmt.Errorf("git: open repository at %q: %w", abs, err)
	}

	// Prefer the worktree root over the requested path: everything this package
	// returns is relative to the repository root, and callers need to be able
	// to rejoin those paths onto something real.
	root := abs
	if wt, werr := repo.Worktree(); werr == nil {
		root = wt.Filesystem.Root()
	}

	return &Repo{root: root, isRepo: true, repo: repo}, nil
}

// IsRepo reports whether the opened path is under git control.
func (r *Repo) IsRepo() bool { return r.isRepo }

// Root is the absolute worktree root, or the opened directory when !IsRepo.
func (r *Repo) Root() string { return r.root }

// Info reports HEAD, branch and cleanliness.
//
// It never returns ErrNotRepository; a non-repository yields Info{IsRepo:false}
// and a nil error, because callers must be able to keep working on plain
// directories without special-casing an error.
func (r *Repo) Info(ctx context.Context) (Info, error) {
	if !r.isRepo {
		return Info{Root: r.root}, nil
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}

	info := Info{IsRepo: true, Root: r.root}

	if r.repo == nil {
		// Degraded: HEAD read from the files, nothing else available. Dirty
		// stays false rather than being guessed — an unknown reported as
		// "clean" is a claim, and this package does not make claims it has not
		// checked.
		info.HeadSHA = headSHAFromFiles(r.gitDir)
		return info, nil
	}

	head, err := r.repo.Head()
	switch {
	case err == nil:
		info.HeadSHA = head.Hash().String()
		if head.Name().IsBranch() {
			info.Branch = head.Name().Short()
		} else {
			info.Detached = true
		}

	case errors.Is(err, plumbing.ErrReferenceNotFound):
		// A repository with no commits. This is the normal state during
		// `cortex init` on a freshly created project, so it must not be
		// treated as breakage. HEAD is still a symbolic ref at this point, so
		// the branch name is recoverable even though the hash is not.
		if sym, serr := r.repo.Reference(plumbing.HEAD, false); serr == nil && sym.Target().IsBranch() {
			info.Branch = sym.Target().Short()
		}

	default:
		return Info{}, fmt.Errorf("git: read HEAD: %w", err)
	}

	dirty, err := r.isDirty(ctx)
	if err != nil {
		return Info{}, err
	}
	info.Dirty = dirty

	return info, nil
}

// HeadSHA resolves HEAD without the worktree status scan Info performs.
//
// Info's cost is dominated by Dirty, which requires a full worktree walk
// (~15ms on a synthetic 200-file repo — see the package benchmark comment).
// Callers that only need the commit HEAD points at, such as a live
// freshness comparison run on every request, should use this instead: it
// costs one ref read, not a tree walk.
//
// Returns "" (not an error) for a non-repository or a repository with no
// commits yet — the same "unknown, not a failure" contract Info uses.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	if !r.isRepo {
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if r.repo == nil {
		// Degraded: read HEAD from the files directly, same as Info does.
		return headSHAFromFiles(r.gitDir), nil
	}
	head, err := r.repo.Head()
	switch {
	case err == nil:
		return head.Hash().String(), nil
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		// A repository with no commits yet.
		return "", nil
	default:
		return "", fmt.Errorf("git: read HEAD: %w", err)
	}
}

// isDirty reports whether the worktree or index differs from HEAD.
func (r *Repo) isDirty(ctx context.Context) (bool, error) {
	st, err := r.rawStatus(ctx)
	if err != nil {
		return false, err
	}
	return !st.IsClean(), nil
}

// rawStatus returns go-git's worktree status, or an empty status for a bare
// repository (which has no worktree and therefore cannot be dirty).
//
// go-git's Status takes no context and cannot be cancelled mid-walk; it is
// O(tracked files + worktree entries) and rehashes every file whose stat data
// changed. That makes it the most expensive operation in this package on a
// large checkout — see BenchmarkInfo. Cancellation is checked either side, but
// a caller that needs a hard deadline should run Info on its own goroutine.
func (r *Repo) rawStatus(ctx context.Context) (gogit.Status, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		if errors.Is(err, gogit.ErrIsBareRepository) {
			return gogit.Status{}, nil
		}
		return nil, fmt.Errorf("git: open worktree: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	st, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("git: worktree status: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return st, nil
}

// WorktreeStatus is the set of paths that differ from the committed tree,
// grouped by where the difference lives.
//
// The groups overlap by design: a file staged and then edited again appears in
// both Staged and Modified, which is exactly what git reports and what the
// indexer needs in order to decide what to re-read from disk.
type WorktreeStatus struct {
	// Modified are tracked files whose worktree content differs from the index.
	Modified []string `json:"modified"`
	// Staged are files whose index entry differs from HEAD.
	Staged []string `json:"staged"`
	// Untracked are files present on disk and unknown to git, after applying
	// .gitignore.
	Untracked []string `json:"untracked"`
	// Deleted are tracked files missing from the worktree. Listed separately
	// because the indexer must drop their symbols rather than re-read them.
	Deleted []string `json:"deleted"`
}

// Status reports the current worktree and index state.
//
// All slices are sorted so that callers producing digests or diffs of the
// result get stable output.
func (r *Repo) Status(ctx context.Context) (WorktreeStatus, error) {
	if !r.isRepo {
		return WorktreeStatus{}, ErrNotRepository
	}

	st, err := r.rawStatus(ctx)
	if err != nil {
		return WorktreeStatus{}, err
	}

	var out WorktreeStatus
	for p, fs := range st {
		p = normPath(p)
		if p == "" {
			continue
		}

		switch fs.Worktree {
		case gogit.Untracked:
			out.Untracked = append(out.Untracked, p)
		case gogit.Deleted:
			out.Deleted = append(out.Deleted, p)
		case gogit.Unmodified:
			// nothing in the worktree half
		default:
			out.Modified = append(out.Modified, p)
		}

		// Untracked in the staging half means "not in the index at all", which
		// is the absence of a staged change rather than a staged change.
		if fs.Staging != gogit.Unmodified && fs.Staging != gogit.Untracked {
			out.Staged = append(out.Staged, p)
		}
	}

	sort.Strings(out.Modified)
	sort.Strings(out.Staged)
	sort.Strings(out.Untracked)
	sort.Strings(out.Deleted)
	return out, nil
}

// normPath converts a path from go-git or the host filesystem into RepoCortex's
// canonical form: repo-relative, forward slashes, no leading separator.
//
// go-git's tree entries are already slash-separated, but worktree status keys
// come from the OS filesystem layer, so on Windows they can arrive with
// backslashes. Normalising unconditionally is cheap and removes an entire class
// of "the index has two entries for the same file" bug that only reproduces on
// one platform.
func normPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// gitDirFor locates the git directory for a path, walking upward and following
// the `gitdir:` indirection that a linked worktree's .git FILE uses.
func gitDirFor(start string) (string, bool) {
	dir := start
	for {
		candidate := filepath.Join(dir, ".git")
		fi, err := os.Stat(candidate) //nolint:gosec // G703: probing for .git under the user-chosen project root is the point
		switch {
		case err == nil && fi.IsDir():
			return candidate, true
		case err == nil:
			// A linked worktree stores `.git` as a file containing
			// "gitdir: <path>".
			b, rerr := os.ReadFile(candidate) //nolint:gosec // a .git file in the tree being indexed
			if rerr != nil {
				return "", false
			}
			line := strings.TrimSpace(string(b))
			if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
				return strings.TrimSpace(rest), true
			}
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// headSHAFromFiles resolves HEAD without go-git.
//
// HEAD is either a raw revision (detached) or "ref: refs/heads/<branch>". A
// branch resolves to a loose ref file, or to an entry in packed-refs once git
// has packed it. Anything else yields "" rather than a guess.
func headSHAFromFiles(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD")) //nolint:gosec // inside the repository being indexed
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	ref, isSymbolic := strings.CutPrefix(head, "ref:")
	if !isSymbolic {
		if isHexSHA(head) {
			return head
		}
		return ""
	}
	ref = strings.TrimSpace(ref)

	if b, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil { //nolint:gosec // as above
		if sha := strings.TrimSpace(string(b)); isHexSHA(sha) {
			return sha
		}
	}

	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs")) //nolint:gosec // as above
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(packed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && strings.TrimSpace(name) == ref && isHexSHA(sha) {
			return sha
		}
	}
	return ""
}

// isHexSHA reports whether s is a full 40-character object name.
func isHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
