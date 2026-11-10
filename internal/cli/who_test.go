package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Tests for `cortex who`.
//
// They drive the whole stack — index, resolve, blast radius, git history — for
// the reason query_test.go gives: the defects that matter in a command like this
// are wiring defects. A history whose paths are in the wrong space, a report
// read from the wrong run, or a ranking computed over an empty reached set all
// produce a plausible-looking answer that is simply wrong, and none of them is
// visible when the registry and the repository are stubs.

// runWho executes the command against a project root. It is built directly
// rather than through NewRoot because registration lives in root.go.
func execWho(t *testing.T, root string, jsonOut bool, args ...string) (string, error) {
	t.Helper()
	g := &Global{cfg: mustConfig(t, root), JSON: jsonOut}
	cmd := newWhoCmd(g)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// whoFixture is queryFixture plus a real history: three identities, one of whom
// has only ever touched code outside the blast radius.
//
// Built with go-git rather than by shelling out to `git`, for the reason
// internal/acquire/git's fixtures give: RepoCortex ships without a dependency on
// the git binary, and a test suite that needs one is testing a different program
// than the one we ship.
func whoFixture(t *testing.T) string {
	t.Helper()
	root := queryFixture(t)

	// go-git reports the resolved worktree root; on macOS t.TempDir() lives
	// under a symlink, so initialise on the resolved path or the command's own
	// path-prefix arithmetic is being tested against a fiction.
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

	// Fixed timestamps a day apart, so the ordering assertions below do not
	// depend on how fast the machine is.
	commit := func(seq int, name, email, message string, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if _, err := wt.Add(p); err != nil {
				t.Fatalf("stage %s: %v", p, err)
			}
		}
		sig := &object.Signature{
			Name: name, Email: email,
			When: time.Unix(1700000000+int64(seq)*86400, 0).UTC(),
		}
		if _, err := wt.Commit(message, &gogit.CommitOptions{
			Author: sig, Committer: sig, AllowEmptyCommits: true,
		}); err != nil {
			t.Fatalf("commit %q: %v", message, err)
		}
	}

	commit(1, "Sam Okafor", "sam@example.com", "add the repository", "svc/repo.go")
	commit(2, "Robin Vega", "robin@example.com", "add the payment service", "svc/payment.go")

	// A real second edit to the same file, and committed under the same address
	// written differently — one human, two spellings, which must not become two
	// rows in the ranking.
	payment := filepath.Join(dir, "svc", "payment.go")
	body, err := os.ReadFile(payment) //nolint:gosec // test fixture inside t.TempDir
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(payment, append(body, "\n// settle twice, refund once\n"...), 0o600); err != nil {
		t.Fatalf("edit fixture: %v", err)
	}
	commit(3, "Robin Vega", "Robin@Example.com", "note the refund path", "svc/payment.go")

	// Ash has touched only code the blast radius does not reach.
	commit(4, "Ash Berg", "ash@example.com", "unrelated marketing report", "svc/unrelated.go")
	return root
}

// whoReport is the JSON shape the command emits.
type whoReport struct {
	IndexRevision int64  `json:"index_revision"`
	HistoryNote   string `json:"history_note"`
	ReviewContext struct {
		Reach struct {
			Symbols      int `json:"symbols"`
			Attributable int `json:"attributable"`
			Files        int `json:"files"`
			Direct       int `json:"direct"`
		} `json:"reach"`
		PeopleFound int `json:"people_found"`
		People      []struct {
			Name         string   `json:"name"`
			Email        string   `json:"email"`
			Identity     string   `json:"identity"`
			Symbols      int      `json:"symbols"`
			Share        float64  `json:"share"`
			FilesTouched int      `json:"files_touched"`
			Files        []string `json:"files"`
			CommitCount  int      `json:"commit_count"`
			Commits      []struct {
				SHA     string   `json:"sha"`
				Subject string   `json:"subject"`
				Files   []string `json:"files"`
			} `json:"commits"`
			Basis string `json:"basis"`
		} `json:"people"`
		Window struct {
			CommitsRead int  `json:"commits_read"`
			Exhausted   bool `json:"exhausted"`
		} `json:"window"`
		Caveats []string `json:"caveats"`
	} `json:"review_context"`
}

func whoJSON(t *testing.T, root string, args ...string) whoReport {
	t.Helper()
	out, err := execWho(t, root, true, args...)
	if err != nil {
		t.Fatalf("who: %v\n%s", err, out)
	}
	var rep whoReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("who --json emitted non-JSON: %v\n%s", err, out)
	}
	return rep
}

// TestWhoRanksByReachabilityNotDirectory is the claim the command exists to
// make.
//
// Save lives in svc/repo.go; the code a change to it reaches lives mostly in
// svc/payment.go. A directory-based router would name whoever is listed for
// svc/, or for repo.go alone. This must rank the person who has touched the
// REACHED code first, and must not name the person whose commits are entirely
// outside the radius however many of them there are.
func TestWhoRanksByReachability(t *testing.T) {
	root := whoFixture(t)
	rep := whoJSON(t, root, "Save")

	rc := rep.ReviewContext
	if rc.Reach.Direct < 2 {
		t.Fatalf("blast radius has %d direct callers; Settle and Refund both call Save", rc.Reach.Direct)
	}
	if rc.Reach.Files != 2 {
		t.Fatalf("reach spans %d files, want svc/repo.go and svc/payment.go", rc.Reach.Files)
	}
	if rc.Reach.Symbols != rc.Reach.Attributable {
		t.Errorf("reach = %d symbols but only %d attributable; the fixture locates all of them",
			rc.Reach.Symbols, rc.Reach.Attributable)
	}

	if rc.PeopleFound != 2 {
		t.Fatalf("people found = %d, want 2 (Ash has touched nothing in the radius)", rc.PeopleFound)
	}
	first, second := rc.People[0], rc.People[1]
	if first.Name != "Robin Vega" {
		t.Fatalf("ranking = %s then %s; want the person who touched the reached callers first",
			first.Name, second.Name)
	}
	if second.Name != "Sam Okafor" {
		t.Errorf("second = %s, want Sam, who touched the target's own file", second.Name)
	}
	for _, p := range rc.People {
		if p.Name == "Ash Berg" {
			t.Error("Ash's commits touch nothing the change reaches and must not be listed")
		}
	}

	// The proportion, not the raw count, is what ranks them.
	if first.Symbols <= second.Symbols {
		t.Errorf("Robin has %d reached symbols and Sam %d; the ranking key is not the reached count",
			first.Symbols, second.Symbols)
	}
	if first.Share <= 0.5 || first.Share > 1 {
		t.Errorf("Robin's share = %v, want more than half of the radius", first.Share)
	}

	// Two spellings of one address are one identity.
	if first.Identity != "robin@example.com" || first.CommitCount != 2 {
		t.Errorf("Robin = %+v; two differently-cased addresses did not merge into one identity", first)
	}

	// Every name is justified by commits, and those commits cite only the
	// REACHED files they touched.
	for _, p := range rc.People {
		if len(p.Commits) == 0 {
			t.Errorf("%s is ranked with no commits behind it", p.Name)
		}
		for _, c := range p.Commits {
			if c.SHA == "" || len(c.Files) == 0 {
				t.Errorf("%s: commit %+v justifies nothing", p.Name, c)
			}
			for _, f := range c.Files {
				if f == "svc/unrelated.go" {
					t.Errorf("%s: commit %s cites a file outside the blast radius", p.Name, c.SHA)
				}
			}
		}
	}

	if !rc.Window.Exhausted || rc.Window.CommitsRead != 4 {
		t.Errorf("window = %+v, want the whole 4-commit history", rc.Window)
	}
}

// TestWhoFramesContextNotOwnership. This command names colleagues, and the
// difference between "has context on" and "is responsible for" is the difference
// between a routing hint and an accusation. The framing is asserted on the
// COMMAND'S OWN OUTPUT, because that is where a reader meets it.
func TestWhoFramesContextNotOwnership(t *testing.T) {
	root := whoFixture(t)
	out, err := execWho(t, root, false, "Save")
	if err != nil {
		t.Fatalf("who: %v\n%s", err, out)
	}

	for _, want := range []string{
		"WHO HAS CONTEXT ON THIS CODE",
		"WHAT THIS DOES NOT SAY",
		"has touched",
		"not who owns it",
		"never a finding about who is responsible",
		"per FILE, not per line",
		"\"they\"",
		// The address is printed as the newest commit spelled it, not as the
		// lower-cased key it was grouped under: the key is an implementation
		// detail of the grouping, and rewriting somebody's address in the
		// output would misquote the record the report rests on.
		"Robin Vega <Robin@Example.com>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	// The words that would turn the report into a finding about a person. Each
	// is checked as a phrase rather than a substring so the caveats — which
	// must be able to say "not who owns it" — do not trip their own guard.
	lower := strings.ToLower(out)
	for _, banned := range []string{
		"is responsible for", "responsible:", "owner:", "owned by", "blame", "at fault", "introduced by",
	} {
		if strings.Contains(lower, banned) {
			t.Errorf("output contains %q, which git history does not support:\n%s", banned, out)
		}
	}

	// A person's own row states its denominator, so a line lifted out of the
	// report on its own still carries what it is a proportion of.
	if !strings.Contains(out, "reached symbols") {
		t.Errorf("no row states what the share is a share of:\n%s", out)
	}

	// The section order is part of the claim, not a layout preference: the
	// limits must come after the names, where somebody who has just read a
	// colleague's name meets them, and not before, where they are skipped.
	sections := []string{
		"A change here reaches",
		"WHO HAS CONTEXT ON THIS CODE",
		"WHAT THIS DOES NOT SAY",
	}
	at := -1
	for _, s := range sections {
		i := strings.Index(out, s)
		if i <= at {
			t.Fatalf("section %q is missing or out of order:\n%s", s, out)
		}
		at = i
	}

	// Logged so a reviewer can read the artifact this command exists to
	// produce, which is worth as much as the assertions above it.
	t.Logf("cortex who Save:\n%s", out)
}

// TestWhoIsDeterministic is the property the registry digest guarantee rests on.
//
// Byte-for-byte over the whole rendered report, not over the ranking alone: an
// unordered file list or an unordered caveat set is just as much a determinism
// bug as an unordered ranking, and only comparing the bytes catches all three.
func TestWhoIsDeterministic(t *testing.T) {
	root := whoFixture(t)

	first, err := execWho(t, root, false, "Save")
	if err != nil {
		t.Fatalf("who: %v\n%s", err, first)
	}
	for i := 0; i < 4; i++ {
		got, err := execWho(t, root, false, "Save")
		if err != nil {
			t.Fatalf("who: %v", err)
		}
		if got != first {
			t.Fatalf("run %d differs from the first:\n--- want ---\n%s\n--- got ---\n%s", i+2, first, got)
		}
	}

	// And the machine-readable form too, which is what an agent consumes.
	firstJSON, err := execWho(t, root, true, "Save")
	if err != nil {
		t.Fatalf("who --json: %v", err)
	}
	for i := 0; i < 4; i++ {
		got, err := execWho(t, root, true, "Save")
		if err != nil {
			t.Fatalf("who --json: %v", err)
		}
		if got != firstJSON {
			t.Fatalf("JSON run %d differs from the first", i+2)
		}
	}
}

// TestWhoBoundsTheHistoryWindowAndSaysSo. "Nobody has touched this" and "nobody
// has touched this lately" are different answers that render identically in a
// list of names.
func TestWhoBoundsTheHistoryWindow(t *testing.T) {
	root := whoFixture(t)

	// The newest two commits are Ash's (outside the radius) and Robin's second.
	rep := whoJSON(t, root, "Save", "--commits", "2")
	if rep.ReviewContext.Window.CommitsRead != 2 || rep.ReviewContext.Window.Exhausted {
		t.Fatalf("window = %+v, want 2 commits and not exhausted", rep.ReviewContext.Window)
	}
	if rep.ReviewContext.PeopleFound != 1 || rep.ReviewContext.People[0].Name != "Robin Vega" {
		t.Fatalf("people = %+v, want only the identity inside the window", rep.ReviewContext.People)
	}

	out, err := execWho(t, root, false, "Save", "--commits", "2")
	if err != nil {
		t.Fatalf("who: %v\n%s", err, out)
	}
	if !strings.Contains(out, "absent reads exactly like never having touched it") {
		t.Errorf("a truncated history window is not disclosed:\n%s", out)
	}
	// Sam's work is outside the window and must be simply gone, not guessed at.
	if strings.Contains(out, "Sam Okafor") {
		t.Errorf("a person outside the window was reported anyway:\n%s", out)
	}
}

// TestWhoWithoutAGitRepositorySaysSo. Indexing a plain directory, an extracted
// tarball or a vendored tree is a supported state (see internal/acquire/git), and
// an empty ranking there must not read as "nobody has ever touched this code".
func TestWhoWithoutAGitRepositorySaysSo(t *testing.T) {
	root := queryFixture(t) // indexed, never a repository

	out, err := execWho(t, root, false, "Save")
	if err != nil {
		t.Fatalf("who: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Errorf("output does not say why there is no history:\n%s", out)
	}
	if !strings.Contains(out, "No commit history was read at all") {
		t.Errorf("an empty ranking is not disclosed as an absence of evidence:\n%s", out)
	}
	// The blast radius is still real and still reported.
	if !strings.Contains(out, "A change here reaches") {
		t.Errorf("the blast radius was suppressed along with the history:\n%s", out)
	}
	// And the whole reached set is flagged as code nobody visible has context on.
	if !strings.Contains(out, "REACHED CODE NOBODY IN THIS WINDOW HAS TOUCHED") {
		t.Errorf("output does not report the untouched part of the radius:\n%s", out)
	}
}

// TestWhoRejectsBadInput covers the argument and flag handling, including the
// ambiguity path: an answer about the wrong `Save` is not a partial answer.
func TestWhoRejectsBadInput(t *testing.T) {
	root := whoFixture(t)

	if out, err := execWho(t, root, false, "NoSuchSymbolAnywhere"); err == nil {
		t.Errorf("an unknown symbol produced an answer:\n%s", out)
	} else if !strings.Contains(err.Error(), "names must match exactly") {
		t.Errorf("unhelpful error for an unknown symbol: %v", err)
	}

	if _, err := execWho(t, root, false, "Save", "--min-confidence", "bogus"); err == nil {
		t.Error("an unrecognised confidence floor was accepted")
	}

	_ = newWhoCmd(&Global{}) // the constructor must not panic
}
