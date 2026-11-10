package git

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// These benchmarks exist because go-git's cost model is not obvious from its
// API: two calls that look equally cheap can differ by three orders of
// magnitude, because anything path-scoped or history-scoped is a linear walk
// with no index behind it. The numbers decide which operations RepoCortex is
// allowed to call on an indexing hot path, and BenchmarkGoGitBlame is the
// reason blame is not part of this package's API.
//
// Run: go test -bench=. -benchtime=10x ./internal/acquire/git/

const (
	benchCommits = 500
	benchFiles   = 200
	// benchTouched is how many files each commit modifies. Real commits touch a
	// handful of files; a benchmark where every commit rewrites the tree would
	// flatter the tree-diff path by making every diff trivially large and
	// cache-friendly.
	benchTouched = 3
)

var (
	benchDir  string
	benchOnce = sync.OnceValues(buildBenchRepo)
)

// benchFixture returns a shared ~500-commit repository, built once per test
// binary. Rebuilding it per benchmark iteration would measure go-git's write
// path, which is not what any of these benchmarks are about.
func benchFixture(b *testing.B) *Repo {
	b.Helper()
	r, err := benchOnce()
	if err != nil {
		b.Fatalf("build benchmark repository: %v", err)
	}
	return r
}

func buildBenchRepo() (repo *Repo, err error) {
	dir, err := os.MkdirTemp("", "cortex-git-bench-")
	if err != nil {
		return nil, err
	}
	benchDir = dir

	// The fixture helpers report failure through testing.TB.Fatalf, which must
	// not be called from a shared builder that is not owned by any one
	// benchmark. benchStub converts those calls into a panic that is recovered
	// here and returned as an ordinary error, so the benchmarks and the tests
	// build their repositories through exactly the same code path.
	defer func() {
		if p := recover(); p != nil {
			repo, err = nil, fmt.Errorf("build benchmark repository: %v", p)
		}
	}()

	f := initRepo(&benchStub{}, dir)

	for i := 0; i < benchFiles; i++ {
		f.write(benchPath(i), filler(i))
	}
	f.commit("initial import")

	for c := 0; c < benchCommits-1; c++ {
		for t := 0; t < benchTouched; t++ {
			idx := (c*benchTouched + t) % benchFiles
			f.write(benchPath(idx), fmt.Sprintf("%s// revision %d\n", filler(idx), c))
		}
		f.commit(fmt.Sprintf("change %d\n\nbody that the indexer discards", c))
	}

	return Open(dir)
}

func benchPath(i int) string {
	return fmt.Sprintf("pkg/mod%02d/file%03d.go", i%10, i)
}

// benchStub satisfies the parts of testing.TB the fixture helpers use.
type benchStub struct{ testing.TB }

func (s *benchStub) Helper() {}
func (s *benchStub) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

func TestMain(m *testing.M) {
	code := m.Run()
	if benchDir != "" {
		_ = os.RemoveAll(benchDir)
	}
	os.Exit(code)
}

func BenchmarkOpen(b *testing.B) {
	r := benchFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Open(r.Root()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInfo covers HEAD resolution plus the full worktree status walk. The
// status walk dominates and scales with the worktree, not with history.
func BenchmarkInfo(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Info(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHeadSHA covers HEAD resolution alone, without the worktree status
// walk BenchmarkInfo pays for. This is the number that matters for a per-MCP
// request freshness check: it must be cheap enough to run on every tool call.
func BenchmarkHeadSHA(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.HeadSHA(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStatus(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Status(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChangedSinceAdjacent is the incremental re-index case: one commit of
// drift. This is the call RepoCortex makes most often.
func BenchmarkChangedSinceAdjacent(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ChangedSince(ctx, "HEAD~1", "HEAD"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChangedSinceWide is a long-stale index catching up 200 commits.
func BenchmarkChangedSinceWide(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ChangedSince(ctx, "HEAD~200", "HEAD"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChangedSinceFromEmpty is a first-ever index: the whole tree as adds.
func BenchmarkChangedSinceFromEmpty(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ChangedSince(ctx, "", "HEAD"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecentCommitsMeta50(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RecentCommitsMeta(ctx, 50); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecentCommits50 is the same walk plus one tree diff per commit. The
// gap between this and the Meta variant is the whole reason both exist.
func BenchmarkRecentCommits50(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RecentCommits(ctx, 50); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecentCommits500(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RecentCommits(ctx, 500); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFileLastModifiedHot is the good case: the file changed near HEAD, so
// the walk stops almost immediately.
func BenchmarkFileLastModifiedHot(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	path := recentlyTouchedPath()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.FileLastModified(ctx, path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFileLastModifiedMissing is the worst case the bound exists for: a
// path no commit ever touched, forcing a walk of the entire reachable history.
// Compare it against Hot to see how sharply this call's cost depends on luck.
func BenchmarkFileLastModifiedMissing(b *testing.B) {
	r := benchFixture(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.FileLastModified(ctx, "no/such/file.go"); err == nil {
			b.Fatal("expected an error for a path that was never committed")
		}
	}
}

// recentlyTouchedPath returns a file the last few commits modified.
func recentlyTouchedPath() string {
	c := benchCommits - 2
	return benchPath((c * benchTouched) % benchFiles)
}

// BenchmarkGoGitBlame measures go-git's blame directly — this package does not
// expose it. Blame reconstructs the file at every commit that touched it and
// diffs each pair, so it is O(history x file size) with no shortcut.
//
// Measured at 2.0 s for a single file on this 500-commit repository. An indexer
// that blamed even a hundred files would spend minutes, and the cost grows with
// history, so a real repository is worse rather than better. That measurement,
// not a guess, is why there is no Repo.Blame; re-run this before adding one.
func BenchmarkGoGitBlame(b *testing.B) {
	r := benchFixture(b)
	head, err := r.repo.Head()
	if err != nil {
		b.Fatal(err)
	}
	commit, err := r.repo.CommitObject(head.Hash())
	if err != nil {
		b.Fatal(err)
	}
	path := recentlyTouchedPath()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := gogit.Blame(commit, path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPackedRecentCommits200 and BenchmarkPackedRecentCommits200Unpooled
// are the pair that justifies objectview.go, and the only benchmarks here that
// run against a PACKED repository.
//
// The fixture above cannot show this: go-git commits loose objects and has no
// repack, so a walk over it never opens a packfile and the two variants below
// would be indistinguishable. Both skip unless the enclosing checkout is packed
// or CORTEX_TEST_PACKED_REPO names one that is.
//
// Measured on a 767-commit, 499-file repository, 200 commits with paths:
//
//	unpooled (one packfile open per object read)   10.36 s
//	pooled   (one packfile open per walk)           0.14 s
//
// Both perform the same 30,798 object reads and return the same 1,652 changed
// paths. The whole difference is the file open, which is why the fix is a
// storage option and not a change to the traversal.
func BenchmarkPackedRecentCommits200(b *testing.B) {
	r := packedRepo(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.RecentCommits(ctx, 200); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPackedRecentCommits200Unpooled(b *testing.B) {
	r := packedRepo(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		unpooledRecentCommits(b, r, 200)
	}
}
