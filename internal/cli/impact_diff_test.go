package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// Tests for `repocortex impact --base/--head`.
//
// They drive the whole stack — a real repository, a real index, a real
// traversal — for the reason who_test.go gives: everything that can go wrong on
// this path is a WIRING defect. A diff read in the repository's path space and
// looked up in the index's, a report computed from a run built at some other
// commit, or a seed set drawn from the post-rename name alone all produce a
// plausible answer that is simply about different code, and none of them is
// visible against stubs.

func execImpact(t *testing.T, root string, jsonOut bool, args ...string) (string, error) {
	t.Helper()
	g := &Global{cfg: mustConfig(t, root), JSON: jsonOut}
	cmd := newImpactCmd(g)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// impactRepo is a small go-git repository whose commits are made one at a time,
// so a test can decide which of them the index is built at.
type impactRepo struct {
	root string
	dir  string
	wt   *gogit.Worktree
	seq  int
}

func newImpactRepo(t *testing.T) *impactRepo {
	t.Helper()
	root := isolate(t)

	// go-git reports the RESOLVED worktree root; on macOS t.TempDir() lives
	// under a symlink, so initialising on the unresolved path would make the
	// command's index-root rebasing arithmetic answer against a fiction.
	dir := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		dir = resolved
	}
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init repository: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	return &impactRepo{root: root, dir: dir, wt: wt}
}

func (r *impactRepo) write(t *testing.T, rel, body string) {
	t.Helper()
	full := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *impactRepo) commit(t *testing.T, message string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := r.wt.Add(p); err != nil {
			t.Fatalf("stage %s: %v", p, err)
		}
	}
	r.seq++
	sig := &object.Signature{
		Name: "Test", Email: "test@example.com",
		When: time.Unix(1700000000+int64(r.seq)*86400, 0).UTC(),
	}
	if _, err := r.wt.Commit(message, &gogit.CommitOptions{
		Author: sig, Committer: sig, AllowEmptyCommits: true,
	}); err != nil {
		t.Fatalf("commit %q: %v", message, err)
	}
}

func (r *impactRepo) index(t *testing.T) {
	t.Helper()
	if out, err := run(t, r.root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, r.root, "index"); err != nil {
		t.Fatalf("index: %v\n%s", err, out)
	}
	if out, err := run(t, r.root, "resolve"); err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
}

const repoGo = `package svc

type Repo struct{}

func (r *Repo) Save(id string) error { return nil }
`

const paymentGo = `package svc

type PaymentService struct{ repo *Repo }

func (s *PaymentService) Settle(id string) error { return s.repo.Save(id) }

func (s *PaymentService) Refund(id string) error { return s.repo.Save(id) }
`

// impactFixture: a repository indexed at HEAD~1, whose HEAD commit changes
// svc/repo.go — the file every other symbol depends on.
func impactFixture(t *testing.T) *impactRepo {
	t.Helper()
	r := newImpactRepo(t)
	r.write(t, "svc/repo.go", repoGo)
	r.write(t, "svc/payment.go", paymentGo)
	r.commit(t, "add the service", "svc/repo.go", "svc/payment.go")
	r.index(t)

	r.write(t, "svc/repo.go", repoGo+"\nfunc (r *Repo) Delete(id string) error { return nil }\n")
	r.commit(t, "add Delete", "svc/repo.go")
	return r
}

// The command must reach impact.AnalyzeDiff: seeds drawn from the git diff, and
// the dependants of those seeds reported as at risk.
func TestImpactDiffSeedsFromTheGitDiff(t *testing.T) {
	r := impactFixture(t)

	out, err := execImpact(t, r.root, false, "--base", "HEAD~1", "--head", "HEAD")
	if err != nil {
		t.Fatalf("impact: %v\n%s", err, out)
	}
	if !strings.Contains(out, "svc/repo.go") {
		t.Errorf("the changed file is not named in the report:\n%s", out)
	}
	if !strings.Contains(out, "Save") {
		t.Errorf("no seed drawn from the changed file; the diff never reached the analyser:\n%s", out)
	}
	if !strings.Contains(out, "Settle") && !strings.Contains(out, "Refund") {
		t.Errorf("nothing downstream of the change was reported at risk:\n%s", out)
	}
	// The one claim that must survive every empty result.
	if !strings.Contains(strings.ToLower(out), "lower bound") {
		t.Errorf("the report does not state that it is a lower bound:\n%s", out)
	}
}

// --json is the shape a CI job reads, and it must be the report itself rather
// than a re-derived summary.
func TestImpactDiffJSONCarriesTheReport(t *testing.T) {
	r := impactFixture(t)

	out, err := execImpact(t, r.root, true, "--base", "HEAD~1", "--head", "HEAD")
	if err != nil {
		t.Fatalf("impact --json: %v\n%s", err, out)
	}
	var rep struct {
		BaseRev              string   `json:"base_rev"`
		HeadRev              string   `json:"head_rev"`
		ChangedFiles         []string `json:"changed_files"`
		SeedsFound           int      `json:"seeds_found"`
		RevisionMatchesRange bool     `json:"revision_matches_range"`
		Impacted             []struct {
			Name string `json:"name"`
		} `json:"impacted"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse --json output: %v\n%s", err, out)
	}
	if rep.BaseRev != "HEAD~1" || rep.HeadRev != "HEAD" {
		t.Errorf("revisions not carried through: %+v", rep)
	}
	if len(rep.ChangedFiles) != 1 || rep.ChangedFiles[0] != "svc/repo.go" {
		t.Errorf("changed files = %v, want [svc/repo.go]", rep.ChangedFiles)
	}
	if rep.SeedsFound == 0 {
		t.Error("no seeds found; the changed file's symbols were never located in the index")
	}
	if !rep.RevisionMatchesRange {
		t.Error("the index was built at the base of this range and must be reported as matching it")
	}
	if len(rep.Impacted) == 0 {
		t.Error("nothing reported at risk")
	}
}

// A STALE INDEX is the condition that makes every other number on the report
// describe different code. It must be impossible to read the report without it.
func TestImpactDiffDeclaresAStaleIndex(t *testing.T) {
	r := newImpactRepo(t)
	r.write(t, "svc/repo.go", repoGo)
	r.write(t, "svc/payment.go", paymentGo)
	r.commit(t, "add the service", "svc/repo.go", "svc/payment.go")
	r.index(t)

	// Two commits AFTER the index, so the indexed revision is neither end of
	// the range analysed below.
	r.write(t, "svc/repo.go", repoGo+"\nfunc (r *Repo) Delete(id string) error { return nil }\n")
	r.commit(t, "add Delete", "svc/repo.go")
	r.write(t, "svc/repo.go", repoGo+"\nfunc (r *Repo) Purge(id string) error { return nil }\n")
	r.commit(t, "add Purge", "svc/repo.go")

	out, err := execImpact(t, r.root, false, "--base", "HEAD~1", "--head", "HEAD")
	if err != nil {
		t.Fatalf("impact: %v\n%s", err, out)
	}
	if !strings.Contains(out, "STALE INDEX") {
		t.Errorf("a report computed from a run at neither end of the range must say so:\n%s", out)
	}
}

// --base is not optional and must not default to something plausible: a blast
// radius computed over the wrong range is a wrong answer, not a partial one.
func TestImpactDiffRequiresABase(t *testing.T) {
	r := impactFixture(t)
	out, err := execImpact(t, r.root, false)
	if err == nil {
		t.Fatalf("expected an error without --base, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--base") {
		t.Errorf("the error must name the missing flag, got %v", err)
	}
}

// Tests for the "WHAT THIS DOES NOT SAY" rendering directly, bypassing the
// full index/analyse pipeline: the thing under test is presentation logic in
// writeImpactDiff, not impact.AnalyzeDiff, and a hand-built report exercises
// it in isolation the way a 133-seed real diff would without paying for one.

// Many same-kind unknowns differing only by an embedded count must collapse
// into one line that says plainly that it is an aggregate — this is the exact
// shape a many-seed diff produces: impact.collectUnknowns embeds the frontier
// node count from graph.Result.TruncatedReason in every seed's Detail, so a
// depth-capped walk over N seeds emits N near-identical unexpanded_frontier
// unknowns that differ only in that number.
func TestImpactDiffAggregatesSameKindUnknownsThatDifferOnlyByCount(t *testing.T) {
	var rep impact.DiffReport
	rep.BaseRev, rep.HeadRev = "HEAD~1", "HEAD"
	rep.RevisionMatchesRange = true
	for _, n := range []int{3, 7, 11, 2, 9, 41, 12, 6, 8, 15, 22, 4, 19, 30} {
		rep.Unknown = append(rep.Unknown, impact.Unknown{
			Kind: impact.UnknownUnexpandedFrontier,
			Detail: fmt.Sprintf("The traversal stopped before exhausting the reachable set "+
				"(depth limit 3 reached; %d node(s) at the limit were not expanded). Symbols "+
				"beyond that boundary are affected-status unknown, not unaffected.", n),
			Count:             int64(n),
			CountIsLowerBound: true,
		})
	}

	var buf bytes.Buffer
	writeImpactDiff(&buf, rep, defaultImpactListLimit, "coverage fact stated once")
	out := buf.String()

	if got := strings.Count(out, "NOT VISIBLE (unexpanded_frontier)"); got != 1 {
		t.Fatalf("14 same-kind unknowns differing only by count must collapse to one line, "+
			"got %d occurrences of \"NOT VISIBLE (unexpanded_frontier)\":\n%s", got, out)
	}
	if !strings.Contains(out, "14 occurrences aggregated") {
		t.Errorf("the collapsed line must say plainly that it summarises many, not hide it:\n%s", out)
	}
}

// Two same-kind unknowns whose cause genuinely differs — not merely a
// different count — must NOT be folded together. Collapsing distinct causes
// would trade noise for a false claim that only one thing is unknown.
func TestImpactDiffKeepsGenuinelyDifferentSameKindUnknownsSeparate(t *testing.T) {
	var rep impact.DiffReport
	rep.BaseRev, rep.HeadRev = "HEAD~1", "HEAD"
	rep.RevisionMatchesRange = true
	rep.Unknown = []impact.Unknown{
		{
			Kind: impact.UnknownUnexpandedFrontier,
			Detail: "The traversal stopped before exhausting the reachable set (depth limit 3 " +
				"reached; 42 node(s) at the limit were not expanded). Symbols beyond that " +
				"boundary are affected-status unknown, not unaffected.",
			Count: 42, CountIsLowerBound: true,
		},
		{
			Kind: impact.UnknownUnexpandedFrontier,
			Detail: "The traversal stopped before exhausting the reachable set (node limit " +
				"5000 reached at depth 2; the reachable set is larger than this result). " +
				"Symbols beyond that boundary are affected-status unknown, not unaffected.",
			Count: 5000, CountIsLowerBound: true,
		},
	}

	var buf bytes.Buffer
	writeImpactDiff(&buf, rep, defaultImpactListLimit, "coverage fact stated once")
	out := buf.String()

	if got := strings.Count(out, "NOT VISIBLE (unexpanded_frontier)"); got != 2 {
		t.Fatalf("two unknowns with genuinely different causes must stay two lines, got %d:\n%s", got, out)
	}
	if strings.Contains(out, "aggregated") {
		t.Errorf("distinct causes must not be reported as an aggregate:\n%s", out)
	}
	if !strings.Contains(out, "depth limit 3 reached; 42 node(s)") {
		t.Errorf("the depth-limit cause is missing from the output:\n%s", out)
	}
	if !strings.Contains(out, "node limit 5000 reached at depth 2") {
		t.Errorf("the node-limit cause is missing from the output:\n%s", out)
	}
}

// The resolution-coverage fact must be stated once: `coverage` states it, and
// two more restatements — the unresolved_references Unknown and the
// Completeness.Caveats resolution sentence — say the identical fact in two
// more wordings and must be suppressed rather than printed a second and third
// time.
func TestImpactDiffStatesCoverageFactOnce(t *testing.T) {
	var rep impact.DiffReport
	rep.BaseRev, rep.HeadRev = "HEAD~1", "HEAD"
	rep.RevisionMatchesRange = true
	rep.Unknown = []impact.Unknown{{
		Kind: impact.UnknownUnresolvedReferences,
		Detail: "50 reference(s) in this run were never bound to a definition: 40 matched no " +
			"definition in this index at all, 10 were too ambiguous to choose between, and 0 " +
			"are still pending. Any one of them could be a use of this symbol that does not " +
			"appear above.",
		Count: 50,
	}}
	rep.Completeness = capability.Completeness{
		Caveats: []string{
			"Reference resolution for this index run: 79 of 100 references that name " +
				"something this index defines (79.0%) were bound to a definition. 10 remain " +
				"too ambiguous to choose between and 0 are still pending. A further 40 " +
				"matched no definition here at all.",
		},
	}

	const marker = "COVERAGE-MARKER 79.0%"
	var buf bytes.Buffer
	writeImpactDiff(&buf, rep, defaultImpactListLimit, marker)
	out := buf.String()

	if got := strings.Count(out, marker); got != 1 {
		t.Fatalf("the coverage fact must appear exactly once, got %d occurrences of the marker line:\n%s", got, out)
	}
	if strings.Contains(out, "NOT VISIBLE (unresolved_references)") {
		t.Errorf("the unresolved_references unknown restates the coverage fact and must be suppressed:\n%s", out)
	}
	if strings.Contains(out, "Reference resolution for this index run:") {
		t.Errorf("the Completeness resolution caveat restates the coverage fact and must be suppressed:\n%s", out)
	}
}

// multipliers extracts every "(×N)" a rendered section carries, in order.
func multipliers(section string) []string {
	return regexp.MustCompile(`\(×\d+\)`).FindAllString(section, -1)
}

// TestImpactDiffTruncationSectionsAgree pins the two places one truncation
// field is rendered to the same numbers.
//
// `TRUNCATED:` prints rep.TruncatedReason; `WHAT THIS DOES NOT SAY` prints the
// same field again inside Completeness.Caveats, wrapped in a sentence. When
// the aggregation lived in the renderers, those two wrappings grouped the
// clauses differently and the section printed "(×43)" against the other's
// "(×42)" for the identical fact. A reader has no way to tell which is true,
// and one of them is not.
func TestImpactDiffTruncationSectionsAgree(t *testing.T) {
	// The reason as impact's aggregation now emits it: distinct facts joined by
	// the reserved separator, each stating its own multiplicity once.
	reason := "depth limit 3 reached, and 5 node(s) at the limit were not expanded (×43); " +
		"depth limit 3 reached, and 7 node(s) at the limit were not expanded (×11); " +
		"node limit 1000 reached at depth 2, and the reachable set is larger than this result (×2)"

	var rep impact.DiffReport
	rep.BaseRev, rep.HeadRev = "HEAD~2", "HEAD"
	rep.RevisionMatchesRange = true
	rep.Truncated, rep.TruncatedReason = true, reason
	rep.Completeness = capability.NewCompleteness(
		registry.ResolutionStats{Total: 100, Resolved: 79, Unresolved: 21}, 500, true, reason)

	var buf bytes.Buffer
	writeImpactDiff(&buf, rep, defaultImpactListLimit, "coverage fact stated once")
	out := buf.String()

	i := strings.Index(out, "\nTRUNCATED: ")
	j := strings.Index(out, "WHAT THIS DOES NOT SAY")
	if i < 0 || j < 0 || j < i {
		t.Fatalf("expected both a TRUNCATED: line and a WHAT THIS DOES NOT SAY block:\n%s", out)
	}
	truncSection, caveatSection := out[i:j], out[j:]

	got, want := multipliers(truncSection), multipliers(caveatSection)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the two renderings of one field disagree.\nTRUNCATED: says %v\nWHAT THIS DOES NOT SAY says %v\n\n%s",
			got, want, out)
	}
	if len(got) != 3 {
		t.Fatalf("expected the three aggregated facts to keep their counts, got %v:\n%s", got, out)
	}
}
