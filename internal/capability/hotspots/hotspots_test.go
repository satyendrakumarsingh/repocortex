package hotspots

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

type fakeHistory struct {
	commits  []Commit
	gotLimit int
	err      error
}

func (f *fakeHistory) Commits(_ context.Context, limit int) ([]Commit, error) {
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	if len(f.commits) > limit {
		return f.commits[:limit], nil
	}
	return f.commits, nil
}

type fakeStructure map[string]registry.FileStructure

func (f fakeStructure) FileStructureFor(_ context.Context, _ int64, paths []string) (map[string]registry.FileStructure, error) {
	out := map[string]registry.FileStructure{}
	for _, p := range paths {
		if fs, ok := f[p]; ok {
			out[p] = fs
		}
	}
	return out, nil
}

func file(path string, size int64) registry.FileStructure {
	return registry.FileStructure{Path: path, Language: "go", SizeBytes: size, Symbols: 3}
}

func commit(when int64, paths ...string) Commit {
	return Commit{SHA: fmt.Sprintf("c%d", when), When: when, Paths: paths}
}

func rank(t *testing.T, h History, s Structure, opts Options) *Report {
	t.Helper()
	rep, err := Rank(context.Background(), h, s, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// The score is change frequency times size: a large file touched often outranks
// both a large file touched once and a small file touched constantly.
func TestScoreIsChurnTimesSize(t *testing.T) {
	h := &fakeHistory{commits: []Commit{
		commit(1, "big_hot.go", "tiny_hot.go"),
		commit(2, "big_hot.go", "tiny_hot.go"),
		commit(3, "big_hot.go", "tiny_hot.go"),
		commit(4, "big_cold.go", "tiny_hot.go"),
	}}
	s := fakeStructure{
		"big_hot.go":  file("big_hot.go", 10_000),
		"big_cold.go": file("big_cold.go", 10_000),
		"tiny_hot.go": file("tiny_hot.go", 100),
	}
	rep := rank(t, h, s, Options{})

	if got := paths(rep); strings.Join(got, ",") != "big_hot.go,big_cold.go,tiny_hot.go" {
		t.Errorf("order = %v", got)
	}
	top := rep.Hotspots[0]
	if top.Score != 100 || top.Commits != 3 || top.LastChanged != 3 {
		t.Errorf("top = %+v, want score 100, 3 commits, last changed 3", top)
	}
	if rep.Hotspots[1].Score != 33 {
		t.Errorf("big_cold score = %d, want 33 (one third of the top product)", rep.Hotspots[1].Score)
	}
}

// Files committed together are reported as coupled, with the share of the
// hotspot's own commits they appeared in.
func TestChangesWithReportsCoupledFiles(t *testing.T) {
	h := &fakeHistory{commits: []Commit{
		commit(1, "order.go", "invoice.go"),
		commit(2, "order.go", "invoice.go"),
		commit(3, "order.go", "invoice.go", "rare.go"),
		commit(4, "order.go"),
	}}
	s := fakeStructure{
		"order.go":   file("order.go", 5000),
		"invoice.go": file("invoice.go", 100),
		"rare.go":    file("rare.go", 100),
	}
	rep := rank(t, h, s, Options{})

	order := rep.Hotspots[0]
	if order.Path != "order.go" {
		t.Fatalf("top hotspot = %s", order.Path)
	}
	if len(order.ChangesWith) != 1 {
		t.Fatalf("changes_with = %+v, want just invoice.go (rare.go is under the minimum)", order.ChangesWith)
	}
	p := order.ChangesWith[0]
	if p.Path != "invoice.go" || p.Together != 3 || p.Share != 0.75 {
		t.Errorf("partner = %+v, want invoice.go, 3 together, share 0.75", p)
	}
}

// A sweeping commit couples every file to every other and says nothing about
// any of them. It still counts as a change to each file.
func TestLargeCommitsAreExcludedFromCouplingButNotFromChurn(t *testing.T) {
	sweep := []string{"a.go", "b.go"}
	for i := 0; i < 40; i++ {
		sweep = append(sweep, fmt.Sprintf("gen/f%d.go", i))
	}
	h := &fakeHistory{commits: []Commit{
		commit(1, sweep...), commit(2, sweep...), commit(3, sweep...),
	}}
	s := fakeStructure{"a.go": file("a.go", 1000), "b.go": file("b.go", 900)}

	rep := rank(t, h, s, Options{})
	if rep.LargeCommitsSkipped != 3 {
		t.Errorf("large commits skipped = %d, want 3", rep.LargeCommitsSkipped)
	}
	if rep.Hotspots[0].Commits != 3 {
		t.Errorf("a.go commits = %d, want 3", rep.Hotspots[0].Commits)
	}
	if len(rep.Hotspots[0].ChangesWith) != 0 {
		t.Errorf("a.go changes_with = %+v, want none", rep.Hotspots[0].ChangesWith)
	}
}

func TestTestsAreLeftOutUnlessAsked(t *testing.T) {
	h := &fakeHistory{commits: []Commit{commit(1, "x.go", "x_test.go")}}
	s := fakeStructure{"x.go": file("x.go", 10), "x_test.go": file("x_test.go", 1000)}

	if got := paths(rank(t, h, s, Options{})); len(got) != 1 || got[0] != "x.go" {
		t.Errorf("default = %v, want [x.go]", got)
	}
	if got := paths(rank(t, h, s, Options{IncludeTests: true})); len(got) != 2 {
		t.Errorf("with tests = %v, want both files", got)
	}
}

// A regenerated bundle is large and changes constantly, so by size x churn it
// outranks every source file while being nothing a reader can refactor.
func TestFilesWithoutDefinitionsAreLeftOutUnlessAsked(t *testing.T) {
	bundle := registry.FileStructure{Path: "assets/app.js", SizeBytes: 500_000, Symbols: 0}
	h := &fakeHistory{commits: []Commit{
		commit(1, "assets/app.js", "svc.go"), commit(2, "assets/app.js", "svc.go"), commit(3, "assets/app.js", "svc.go"),
	}}
	s := fakeStructure{"assets/app.js": bundle, "svc.go": file("svc.go", 2000)}

	rep := rank(t, h, s, Options{})
	if got := paths(rep); len(got) != 1 || got[0] != "svc.go" {
		t.Errorf("default = %v, want [svc.go]", got)
	}
	if rep.NonCodeSkipped != 1 {
		t.Errorf("non-code skipped = %d, want 1", rep.NonCodeSkipped)
	}
	if len(rep.Hotspots[0].ChangesWith) != 0 {
		t.Errorf("a skipped file must not appear as a coupling partner: %+v", rep.Hotspots[0].ChangesWith)
	}

	all := rank(t, h, s, Options{IncludeNonCode: true})
	if got := paths(all); len(got) != 2 || got[0] != "assets/app.js" {
		t.Errorf("with non-code = %v, want the bundle first", got)
	}
}

func TestDeletedPathsAreCountedNotRanked(t *testing.T) {
	h := &fakeHistory{commits: []Commit{commit(1, "kept.go", "deleted.go")}}
	rep := rank(t, h, fakeStructure{"kept.go": file("kept.go", 10)}, Options{})

	if rep.NotIndexed != 1 || rep.FilesChanged != 1 {
		t.Errorf("not indexed %d, files changed %d; want 1 and 1", rep.NotIndexed, rep.FilesChanged)
	}
	if len(rep.Hotspots) != 1 {
		t.Errorf("hotspots = %v", paths(rep))
	}
}

func TestLimitWindowAndPrefix(t *testing.T) {
	var commits []Commit
	for i := 0; i < 50; i++ {
		commits = append(commits, commit(int64(i), fmt.Sprintf("svc/f%d.go", i%10), "lib/util.go"))
	}
	s := fakeStructure{"lib/util.go": file("lib/util.go", 100)}
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("svc/f%d.go", i)
		s[p] = file(p, int64(100+i))
	}
	h := &fakeHistory{commits: commits}

	rep := rank(t, h, s, Options{Limit: 3, CommitWindow: 20, PathPrefix: "svc/"})
	if h.gotLimit != 20 {
		t.Errorf("history was asked for %d commits, want the 20 in the window", h.gotLimit)
	}
	if len(rep.Hotspots) != 3 {
		t.Errorf("returned %d hotspots, want 3", len(rep.Hotspots))
	}
	for _, hs := range rep.Hotspots {
		if !strings.HasPrefix(hs.Path, "svc/") {
			t.Errorf("%s is outside the prefix", hs.Path)
		}
	}
	if !rep.WindowFull {
		t.Error("20 commits were read from a longer history: WindowFull should be set")
	}
}

// A path listed twice in one commit is one change.
func TestRepeatedPathInOneCommitCountsOnce(t *testing.T) {
	h := &fakeHistory{commits: []Commit{commit(1, "a.go", "a.go", "a.go")}}
	rep := rank(t, h, fakeStructure{"a.go": file("a.go", 10)}, Options{})
	if rep.Hotspots[0].Commits != 1 {
		t.Errorf("commits = %d, want 1", rep.Hotspots[0].Commits)
	}
}

func TestNoHistoryIsAnAnswerNotAnError(t *testing.T) {
	rep := rank(t, &fakeHistory{}, fakeStructure{}, Options{})
	if len(rep.Hotspots) != 0 || len(rep.Notes) == 0 {
		t.Errorf("report = %+v, want no hotspots and a note saying why", rep)
	}
}

func TestHistoryErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	_, err := Rank(context.Background(), &fakeHistory{err: boom}, fakeStructure{}, 1, Options{})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the history error", err)
	}
	if _, err := Rank(context.Background(), nil, nil, 1, Options{}); err == nil {
		t.Error("nil dependencies must be an error")
	}
}

// The same input must rank the same way every time; map iteration must not
// reach the output.
func TestRankingIsDeterministic(t *testing.T) {
	var commits []Commit
	s := fakeStructure{}
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("f%02d.go", i)
		s[p] = file(p, 100) // identical products, so only the tie-break orders them
		commits = append(commits, commit(int64(i), p, "shared.go"))
	}
	s["shared.go"] = file("shared.go", 100)

	first := strings.Join(paths(rank(t, &fakeHistory{commits: commits}, s, Options{})), ",")
	for i := 0; i < 5; i++ {
		if got := strings.Join(paths(rank(t, &fakeHistory{commits: commits}, s, Options{})), ","); got != first {
			t.Fatalf("run %d ordered differently:\n%s\n%s", i, got, first)
		}
	}
}

func paths(rep *Report) []string {
	out := make([]string, len(rep.Hotspots))
	for i, h := range rep.Hotspots {
		out[i] = h.Path
	}
	return out
}
