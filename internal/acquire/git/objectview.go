package git

import (
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// A history walk reads thousands of small objects. go-git charges a file open
// for every one of them, and that — not the diffing — is the whole cost.
//
// # The measurement
//
// `GET /api/who` took 10.4 s on a 767-commit repository while `git log -n 200
// --name-only` answered the same question from the shell in 43 ms. The commit
// walk itself was 98 ms; filling in the paths took the other 10.2 s. A CPU
// profile put 81% of the samples under
// storage/filesystem.(*ObjectStorage).packfile, below it billy's Open and below
// that syscall.syscall.
//
// The reason is one line in go-git's ObjectStorage.getFromPackfile: it opens
// the packfile, and only THEN does decodeObjectAt consult the object cache. So
// a cache hit still pays an open, a scanner construction and a close of a 35 MB
// pack — about 0.34 ms — and the 200-commit walk performs 30,798 object reads.
// 30,798 × 0.34 ms is the ten seconds. The cache was working perfectly and
// saving nothing that mattered.
//
// go-git already has the fix as a storage option: Options.MaxOpenDescriptors
// keeps a bounded ring of packfiles open across reads. Measured on the same
// repository and the same 200 commits — identical object-read count (30,798),
// identical change set (1,652 paths), identical output:
//
//	default storage       10.35 s
//	MaxOpenDescriptors=1   0.15 s
//	KeepDescriptors=true   0.14 s
//
// # Why this is a view rather than an option on Open
//
// A kept descriptor must be released, and Open's callers are not all shaped for
// that: internal/web opens a repository per HTTP request, and one leaked
// packfile descriptor per request on the Ownership screen is a slower failure
// than the one being fixed. Repo therefore keeps the descriptor-free storage it
// has always had, and the walks that read objects in bulk borrow a pooled view
// for their own duration and close it on the way out. The descriptors live
// exactly as long as the walk that needs them.
//
// It also keeps Repo's concurrency contract intact. MaxOpenDescriptors makes
// the object storer mutate a map on every read; a view owned by one walk is
// never shared, so two concurrent walks cannot observe each other's.

// packDescriptors is how many packfiles one walk keeps open at a time.
//
// Bounded rather than unbounded (go-git's KeepDescriptors) because the count is
// then a property of this file rather than of the repository it is pointed at:
// a repository that has never been repacked can carry hundreds of packs, and
// "as many descriptors as there are packs" is not a bound. Eight covers the
// pack count of a repository that has seen ordinary maintenance; beyond that
// the ring evicts and reopens, which is the behaviour before this change and
// costs the same as it always did.
const packDescriptors = 8

// objectView is the same repository seen through an object store that keeps its
// packfile descriptors open. Close it when the walk is done.
type objectView struct {
	repo *gogit.Repository

	// store is nil when the view is the caller's own repository, which happens
	// when the storer is not filesystem-backed and so has no packfile to hold
	// open. close is then a no-op and the walk is exactly as fast as before.
	store *filesystem.Storage
}

// objectView opens a pooled view of r.
//
// Returns ErrNotRepository for a directory that is not under git control and
// for one go-git declined to open fully — the degraded state Open records. The
// object store is precisely what is missing there, and every caller of this
// already treats ErrNotRepository as "no history", which is the truth.
func (r *Repo) objectView() (*objectView, error) {
	if !r.isRepo || r.repo == nil {
		return nil, ErrNotRepository
	}

	// The filesystem is taken from the storage go-git built rather than rebuilt
	// from r.root: resolving a .git that is a FILE (linked worktrees), and the
	// commondir indirection behind it, is logic that lives in unexported go-git
	// functions. Reusing the result cannot drift from them.
	fsStore, ok := r.repo.Storer.(*filesystem.Storage)
	if !ok {
		return &objectView{repo: r.repo}, nil
	}

	store := filesystem.NewStorageWithOptions(
		fsStore.Filesystem(), cache.NewObjectLRUDefault(),
		filesystem.Options{MaxOpenDescriptors: packDescriptors},
	)

	// A bare repository has no worktree, and that is not an error here: every
	// walk in this package reads objects and refs, never working files.
	var (
		repo *gogit.Repository
		err  error
	)
	if wt, werr := r.repo.Worktree(); werr == nil {
		repo, err = gogit.Open(store, wt.Filesystem)
	} else {
		repo, err = gogit.Open(store, nil)
	}
	if err != nil {
		if cerr := store.Close(); cerr != nil {
			return nil, fmt.Errorf("git: open a pooled object view of %s: %w (and closing it failed: %w)",
				r.root, err, cerr)
		}
		return nil, fmt.Errorf("git: open a pooled object view of %s: %w", r.root, err)
	}
	return &objectView{repo: repo, store: store}, nil
}

// close releases the packfile descriptors the view held open.
func (v *objectView) close() error {
	if v == nil || v.store == nil {
		return nil
	}
	if err := v.store.Close(); err != nil {
		return fmt.Errorf("git: release pooled packfile descriptors: %w", err)
	}
	return nil
}
