package ownership

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeHistory is the whole cost of the port: one method and a slice. A port a
// fake cannot cheaply implement has stopped being a test aid.
type fakeHistory struct {
	commits []Commit
	limit   int // the limit the analyzer asked for, recorded for assertion
	err     error
}

func (f *fakeHistory) Commits(_ context.Context, limit int) ([]Commit, error) {
	f.limit = limit
	if f.err != nil {
		return nil, f.err
	}
	if limit < len(f.commits) {
		return append([]Commit(nil), f.commits[:limit]...), nil
	}
	return append([]Commit(nil), f.commits...), nil
}

// shuffled returns the same history in a different order, which is the test
// that determinism belongs to this package rather than to its caller.
func (f *fakeHistory) shuffled(seed int64) *fakeHistory {
	rng := rand.New(rand.NewSource(seed))
	out := &fakeHistory{commits: append([]Commit(nil), f.commits...), err: f.err}
	rng.Shuffle(len(out.commits), func(i, j int) {
		out.commits[i], out.commits[j] = out.commits[j], out.commits[i]
	})
	for i := range out.commits {
		p := append([]string(nil), out.commits[i].Paths...)
		rng.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
		out.commits[i].Paths = p
	}
	return out
}

// ------------------------------------------------------------------ fixtures

func symbolAt(id int64, name, path string) capability.Symbol {
	return capability.Symbol{ID: id, Name: name, FQN: "pkg." + name, Kind: "function", FilePath: path}
}

// blastRadius is a change reaching nine symbols across four files.
//
// The shape is the one the ranking has to get right: one file holds most of the
// reached symbols, so a person who touched only that file must outrank a person
// who touched three files holding one symbol each. Ranking by files touched, or
// by commit count, gets that backwards.
func blastRadius() impact.Report {
	return impact.Report{
		Target: capability.Target{
			Query: "Decode", SymbolID: 1, Name: "Decode", FQN: "pkg.Decode",
			Kind: "function", FilePath: "internal/codec/decode.go", Line: 12,
		},
		Direct: []impact.Impacted{
			{Symbol: symbolAt(2, "A", "internal/api/handler.go"), Depth: 1, Confidence: model.ConfidenceHigh},
			{Symbol: symbolAt(3, "B", "internal/api/handler.go"), Depth: 1, Confidence: model.ConfidenceHigh},
			{Symbol: symbolAt(4, "C", "internal/api/handler.go"), Depth: 1, Confidence: model.ConfidenceHigh},
			{Symbol: symbolAt(5, "D", "internal/api/handler.go"), Depth: 1, Confidence: model.ConfidenceHigh},
		},
		Indirect: []impact.Impacted{
			{Symbol: symbolAt(6, "E", "internal/worker/run.go"), Depth: 2, Confidence: model.ConfidenceMedium},
			{Symbol: symbolAt(7, "F", "internal/worker/run.go"), Depth: 2, Confidence: model.ConfidenceMedium},
		},
		Dispatch: []impact.Dispatched{
			{Impacted: impact.Impacted{Symbol: symbolAt(8, "G", "internal/plugin/impl.go"), Depth: 2},
				Reason: impact.ReasonImplementsTarget},
		},
		// Untouched by any commit in the fixture history: the reached file
		// nobody has context on.
		ConfidenceFloor: model.ConfidenceMedium,
	}
}

// history: three identities, deliberately shaped so that every ranking rule is
// exercised and no two people tie on the first key.
func history() *fakeHistory {
	return &fakeHistory{commits: []Commit{
		{SHA: "c5", AuthorName: "Robin Vega", AuthorEmail: "robin@example.com", When: 500,
			Subject: "tidy handler", Paths: []string{"internal/api/handler.go", "README.md"}},
		{SHA: "c4", AuthorName: "Robin Vega", AuthorEmail: "Robin@Example.com", When: 400,
			Subject: "handler retries", Paths: []string{"internal/api/handler.go"}},
		{SHA: "c3", AuthorName: "Sam Okafor", AuthorEmail: "sam@example.com", When: 300,
			Subject: "decode fixes", Paths: []string{"internal/codec/decode.go", "internal/worker/run.go"}},
		{SHA: "c2", AuthorName: "Sam Okafor", AuthorEmail: "sam@example.com", When: 200,
			Subject: "worker loop", Paths: []string{"internal/worker/run.go"}},
		{SHA: "c1", AuthorName: "Ash Berg", AuthorEmail: "ash@example.com", When: 100,
			Subject: "unrelated docs", Paths: []string{"docs/guide.md"}},
	}}
}

func analyze(t *testing.T, h History, rep impact.Report, opts ...Option) Report {
	t.Helper()
	a, err := New(h, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := a.Analyze(context.Background(), rep)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return out
}

// ------------------------------------------------------------------ the tests

// TestRankingIsByProportionOfTheReachedSet is the claim that separates this from
// CODEOWNERS and from "top committers".
//
// Robin touched one file holding four reached symbols in two commits; Sam
// touched three files holding four reached symbols in two commits. They must
// come out in the order the SYMBOL counts put them, not the file or commit
// counts — and Ash, with a commit touching nothing in the radius, must not
// appear at all.
func TestRankingIsByProportionOfTheReachedSet(t *testing.T) {
	t.Parallel()
	rep := analyze(t, history(), blastRadius())

	if rep.Reach.Symbols != 8 || rep.Reach.Attributable != 8 {
		t.Fatalf("reach = %d symbols (%d attributable), want the 8 the report names including the target",
			rep.Reach.Symbols, rep.Reach.Attributable)
	}
	if rep.Reach.Files != 4 {
		t.Errorf("reach files = %d, want 4", rep.Reach.Files)
	}
	if rep.PeopleFound != 2 {
		t.Fatalf("people = %d, want the 2 who touched the radius (Ash touched only docs)", rep.PeopleFound)
	}

	names := make([]string, 0, len(rep.People))
	for _, p := range rep.People {
		names = append(names, p.Name)
	}
	// Sam: decode.go (1 symbol) + run.go (2) = 3. Robin: handler.go = 4.
	if strings.Join(names, ",") != "Robin Vega,Sam Okafor" {
		t.Fatalf("ranking = %v, want Robin first on 4 reached symbols and Sam second on 3", names)
	}

	robin := rep.People[0]
	if robin.Symbols != 4 {
		t.Errorf("Robin touched %d reached symbols, want 4", robin.Symbols)
	}
	if got := robin.Share; got < 0.49 || got > 0.51 {
		t.Errorf("Robin's share = %v, want 4/8", got)
	}
	if robin.FilesTouched != 1 || robin.CommitCount != 2 {
		t.Errorf("Robin: %d files / %d commits, want 1 and 2", robin.FilesTouched, robin.CommitCount)
	}
	if robin.LastTouchUnix != 500 {
		t.Errorf("Robin's last touch = %d, want the newest commit's author time", robin.LastTouchUnix)
	}
	// The commits are the justification, newest first, and they list only the
	// REACHED files they touched — README.md is not evidence about this change.
	if len(robin.Commits) != 2 || robin.Commits[0].SHA != "c5" {
		t.Fatalf("Robin's commits = %+v, want c5 then c4", robin.Commits)
	}
	if strings.Join(robin.Commits[0].Files, ",") != "internal/api/handler.go" {
		t.Errorf("commit c5 cites %v; only reached files are evidence", robin.Commits[0].Files)
	}

	sam := rep.People[1]
	if sam.Symbols != 3 || sam.FilesTouched != 2 {
		t.Errorf("Sam: %d symbols across %d files, want 3 across 2", sam.Symbols, sam.FilesTouched)
	}
	if !strings.Contains(sam.Basis, "3 of 8 reached symbols") {
		t.Errorf("Sam's basis does not carry its own denominator: %q", sam.Basis)
	}
}

// TestTargetIsPartOfItsOwnBlastRadius. Excluding it would rank a person who has
// worked on the changed symbol and nothing else at zero.
func TestTargetIsPartOfItsOwnBlastRadius(t *testing.T) {
	t.Parallel()
	rep := blastRadius()
	rep.Direct, rep.Indirect, rep.Dispatch = nil, nil, nil

	got := analyze(t, history(), rep)
	if got.Reach.Symbols != 1 {
		t.Fatalf("reach = %d, want the target itself", got.Reach.Symbols)
	}
	if got.PeopleFound != 1 || got.People[0].Name != "Sam Okafor" {
		t.Fatalf("people = %+v, want only the person who touched the target's file", got.People)
	}
	if got.People[0].Share != 1 {
		t.Errorf("share = %v, want 1: they touched the whole radius", got.People[0].Share)
	}
}

// TestUntouchedPartOfTheRadiusIsReported. The reached code nobody has context on
// is the most useful thing in the report, and it is invisible in a list of names.
func TestUntouchedPartOfTheRadiusIsReported(t *testing.T) {
	t.Parallel()
	rep := analyze(t, history(), blastRadius())

	if rep.Untouched.FileCount != 1 || rep.Untouched.Files[0] != "internal/plugin/impl.go" {
		t.Fatalf("untouched = %+v, want the one reached file no commit touched", rep.Untouched)
	}
	if rep.Untouched.Symbols != 1 {
		t.Errorf("untouched symbols = %d, want 1", rep.Untouched.Symbols)
	}
	if !hasCaveatContaining(rep, "touched by nobody in this window") {
		t.Error("the report does not say part of the radius has no context behind it")
	}
}

// TestIdentityIsAnAddressNotAPerson: the same human writing their email with
// different capitalisation must not become two rows, and the report must say
// that the grouping is by address rather than by person.
func TestIdentityIsAnAddressNotAPerson(t *testing.T) {
	t.Parallel()
	rep := analyze(t, history(), blastRadius())

	robin := rep.People[0]
	if robin.Identity != "robin@example.com" {
		t.Errorf("identity = %q, want the lower-cased address", robin.Identity)
	}
	if robin.CommitCount != 2 {
		t.Errorf("Robin's two differently-cased addresses did not merge: %d commits", robin.CommitCount)
	}
	if !hasCaveatContaining(rep, "committing from two addresses appears twice") {
		t.Error("the report does not state that an identity is not a person")
	}

	// A commit with no email at all still ranks, under a key that cannot
	// collide with an address.
	h := &fakeHistory{commits: []Commit{
		{SHA: "x1", AuthorName: "Nobody", When: 10, Paths: []string{"internal/api/handler.go"}},
	}}
	anon := analyze(t, h, blastRadius())
	if len(anon.People) != 1 || anon.People[0].Identity != "name:nobody" {
		t.Fatalf("people = %+v, want one identity keyed by name", anon.People)
	}
}

// TestReportNeverNamesPeopleWithoutItsLimits is the non-negotiable one.
//
// This asserts on the report rather than on a renderer, because the report is
// what an MCP client, a CI job and the CLI all consume: a limitation that lives
// only in the command's output layer is a limitation the other two consumers do
// not get.
func TestReportNeverNamesPeopleWithoutItsLimits(t *testing.T) {
	t.Parallel()
	rep := analyze(t, history(), blastRadius())

	if len(rep.Caveats) == 0 {
		t.Fatal("a report naming people carries no caveats")
	}
	for _, must := range []string{CaveatNotOwnership, CaveatFileLevel, CaveatPronouns} {
		if !hasCaveat(rep, must) {
			t.Errorf("missing mandatory caveat: %q", must)
		}
	}
	joined := strings.Join(rep.Caveats, " ")
	// The framing, positively and negatively.
	for _, want := range []string{"has context on", "never a finding about who is responsible", "\"they\""} {
		if !strings.Contains(joined, want) {
			t.Errorf("caveats do not say %q", want)
		}
	}
	for _, banned := range []string{"responsible for", "blame", "owner of", "at fault"} {
		for _, p := range rep.People {
			if strings.Contains(strings.ToLower(p.Basis), banned) {
				t.Errorf("a person's basis says %q: %s", banned, p.Basis)
			}
		}
	}
	// The window is stated whether or not it bit.
	if !hasCaveatContaining(rep, "history was read") {
		t.Error("the report does not state how much history it saw")
	}
}

// TestWindowIsStatedWhenItBites. "Nobody has touched this" and "nobody has
// touched this lately" render identically in a list of names, so the bound must
// be visible even when the walk returned a full window.
func TestWindowIsStatedWhenItBites(t *testing.T) {
	t.Parallel()
	h := history()
	rep := analyze(t, h, blastRadius(), WithLimits(Limits{MaxCommits: 2}))

	if h.limit != 2 {
		t.Errorf("the analyzer asked history for %d commits, want its own bound of 2", h.limit)
	}
	if rep.Window.CommitsRead != 2 || rep.Window.Exhausted {
		t.Fatalf("window = %+v, want 2 commits and not exhausted", rep.Window)
	}
	if rep.Window.NewestUnix != 500 || rep.Window.OldestUnix != 400 {
		t.Errorf("window spans %d..%d, want 500..400", rep.Window.OldestUnix, rep.Window.NewestUnix)
	}
	if !hasCaveatContaining(rep, "absent reads exactly like never having touched it") {
		t.Error("a truncated window is not disclosed")
	}
	// Sam's work is outside the window and must simply be gone, not guessed at.
	if rep.PeopleFound != 1 {
		t.Errorf("people = %d, want only the identity inside the window", rep.PeopleFound)
	}

	// A full history says so instead, so a reader can tell the two apart.
	full := analyze(t, history(), blastRadius())
	if !full.Window.Exhausted {
		t.Fatal("a history shorter than the bound is not marked exhausted")
	}
	if !hasCaveatContaining(full, "nobody is missing because of the window") {
		t.Error("an exhausted window is not disclosed as such")
	}
}

// TestBlastRadiusHonestyTravels: the impact report's own hedges must reach this
// report, or a share of a narrowed radius reads as a share of the whole one.
func TestBlastRadiusHonestyTravels(t *testing.T) {
	t.Parallel()
	in := blastRadius()
	in.LowConfidenceHidden = 12
	in.Truncated = true
	in.TruncatedReason = "node budget exhausted"
	// A symbol the index located nowhere: it cannot be attributed to anyone and
	// must not silently cap everybody's share.
	in.Indirect = append(in.Indirect, impact.Impacted{Symbol: symbolAt(9, "H", ""), Depth: 3})

	rep := analyze(t, history(), in)

	if rep.Reach.Symbols != 9 || rep.Reach.Attributable != 8 || rep.Reach.Unlocatable != 1 {
		t.Fatalf("reach = %+v, want 9 reached, 8 attributable, 1 unlocatable", rep.Reach)
	}
	if rep.People[0].Share <= 0.49 || rep.People[0].Share > 0.51 {
		t.Errorf("share = %v; the unlocatable symbol was counted in the denominator", rep.People[0].Share)
	}
	if rep.Reach.ConfidenceFloor != model.ConfidenceMedium || rep.Reach.LowConfidenceHidden != 12 {
		t.Errorf("the impact report's confidence fields did not travel: %+v", rep.Reach)
	}
	for _, want := range []string{
		"confidence floor",
		"12 more symbol(s)",
		"node budget exhausted",
		"no file path in the index",
	} {
		if !hasCaveatContaining(rep, want) {
			t.Errorf("caveats do not mention %q", want)
		}
	}
}

// TestDeterminism is the property the registry digest guarantee rests on.
//
// Generating twice proves little — Go's randomised map order would have to be
// unlucky twice. Re-deriving from a SHUFFLED history is the real test: it fails
// the moment any ordering decision here leans on the order commits arrived in.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	base := analyze(t, history(), blastRadius())
	want := fingerprint(base)

	for i := 0; i < 5; i++ {
		if got := fingerprint(analyze(t, history(), blastRadius())); got != want {
			t.Fatalf("two identical runs disagree:\n want %s\n got  %s", want, got)
		}
	}
	for _, seed := range []int64{1, 2, 99} {
		got := fingerprint(analyze(t, history().shuffled(seed), blastRadius()))
		if got != want {
			t.Errorf("output changed when the commits arrived in a different order (seed %d):\n want %s\n got  %s",
				seed, want, got)
		}
	}
}

// TestTiesAreBrokenTotally. Two identities with identical counts must still have
// a fixed order, or the ranking depends on sort stability and on which one the
// walk happened to see first.
func TestTiesAreBrokenTotally(t *testing.T) {
	t.Parallel()
	// Two people, same file, same symbol count, same timestamp, same commit
	// count — everything equal except the address.
	h := &fakeHistory{commits: []Commit{
		{SHA: "b", AuthorName: "Zoe", AuthorEmail: "zoe@example.com", When: 42,
			Paths: []string{"internal/api/handler.go"}},
		{SHA: "a", AuthorName: "Alex", AuthorEmail: "alex@example.com", When: 42,
			Paths: []string{"internal/api/handler.go"}},
	}}
	first := analyze(t, h, blastRadius())
	if len(first.People) != 2 {
		t.Fatalf("people = %d, want 2", len(first.People))
	}
	if first.People[0].Identity != "alex@example.com" {
		t.Errorf("tie broken to %q; the identity key is the last resort and must decide",
			first.People[0].Identity)
	}
	for _, seed := range []int64{4, 5, 6} {
		got := analyze(t, h.shuffled(seed), blastRadius())
		if got.People[0].Identity != first.People[0].Identity {
			t.Errorf("a tie resolved differently under a different arrival order (seed %d)", seed)
		}
	}
}

// TestBoundsAreAppliedAndDisclosed.
func TestBoundsAreAppliedAndDisclosed(t *testing.T) {
	t.Parallel()
	rep := analyze(t, history(), blastRadius(), WithLimits(Limits{
		MaxPeople: 1, MaxCommitsPerPerson: 1, MaxFilesPerPerson: 1,
	}))

	if len(rep.People) != 1 || rep.PeopleFound != 2 {
		t.Fatalf("people shown/found = %d/%d, want 1 of 2", len(rep.People), rep.PeopleFound)
	}
	if !hasCaveatContaining(rep, "1 of 2 identities are shown") {
		t.Error("a capped ranking does not disclose the cap")
	}
	if n := len(rep.People[0].Commits); n != 1 {
		t.Errorf("commits shown = %d, want the cap of 1", n)
	}
	// The totals must survive the caps: a truncated evidence list must not
	// change the number the ranking was computed from.
	if rep.People[0].CommitCount != 2 {
		t.Errorf("CommitCount = %d; the cap changed the count as well as the list",
			rep.People[0].CommitCount)
	}
}

// TestEmptyHistoryIsAnAnswerNotACrash. A plain directory, or a repository with
// no commits yet, is a normal state — see internal/acquire/git.
func TestEmptyHistoryIsAnAnswerNotACrash(t *testing.T) {
	t.Parallel()
	rep := analyze(t, &fakeHistory{}, blastRadius())

	if len(rep.People) != 0 {
		t.Fatalf("people = %+v from an empty history", rep.People)
	}
	if rep.Untouched.FileCount != rep.Reach.Files {
		t.Errorf("untouched = %d of %d files; with no history nothing is touched",
			rep.Untouched.FileCount, rep.Reach.Files)
	}
	if !hasCaveatContaining(rep, "No commit history was read at all") {
		t.Error("an empty history is not disclosed, so an empty ranking reads as 'nobody has touched this'")
	}
}

// TestPortErrorsAreWrapped: CLAUDE.md §4.3.
func TestPortErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("history exploded")
	a, err := New(&fakeHistory{err: sentinel})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.Analyze(context.Background(), blastRadius())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the port's: %v", err)
	}
	if !strings.Contains(err.Error(), "commit(s) of history") {
		t.Errorf("error does not say what failed: %v", err)
	}
}

// TestContextCancellation: CLAUDE.md §4.1.
func TestContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, err := New(history())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Analyze(ctx, blastRadius()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze on a cancelled context returned %v, want context.Canceled", err)
	}
}

// TestConstructorRejectsBadInput covers the option plumbing.
func TestConstructorRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Error("New accepted a nil history port")
	}
	if _, err := New(history(), WithLimits(Limits{MaxCommits: -1})); err == nil {
		t.Error("New accepted a negative bound")
	}
	a, err := New(history(), WithLimits(Limits{MaxPeople: 2}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.limits.MaxCommits != DefaultLimits().MaxCommits {
		t.Errorf("a zero field overrode the default: MaxCommits = %d", a.limits.MaxCommits)
	}
}

// ------------------------------------------------------------------ helpers

func hasCaveat(r Report, want string) bool {
	for _, c := range r.Caveats {
		if c == want {
			return true
		}
	}
	return false
}

func hasCaveatContaining(r Report, want string) bool {
	for _, c := range r.Caveats {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// fingerprint renders the parts of a report whose ORDER is a promise, so a
// determinism failure names the field that moved.
func fingerprint(r Report) string {
	var b strings.Builder
	for _, p := range r.People {
		b.WriteString(p.Identity + "|" + p.Name + "|")
		b.WriteString(plural(p.Symbols, "sym", "syms") + "|")
		b.WriteString(strings.Join(p.Files, ",") + "|")
		for _, c := range p.Commits {
			b.WriteString(c.SHA + ":" + strings.Join(c.Files, "+") + ";")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.Join(r.Untouched.Files, ",") + "\n")
	b.WriteString(strings.Join(r.Caveats, "\n"))
	return b.String()
}
