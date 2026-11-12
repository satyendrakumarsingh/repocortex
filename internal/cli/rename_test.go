package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/rename"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	"github.com/satyendrakumarsingh/repocortex/internal/edit"
	"github.com/satyendrakumarsingh/repocortex/internal/verify"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func renameJSON(t *testing.T, root string, args ...string) (renameReport, int) {
	t.Helper()
	out, err := run(t, root, append([]string{"--json", "rename"}, args...)...)
	var rep renameReport
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
		t.Fatalf("rename --json did not emit a report: %v (err=%v)\n%s", jerr, err, out)
	}
	return rep, exitCodeOf(err)
}

func snapshot(t *testing.T, root string, names ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(root, n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = string(b)
	}
	return out
}

func assertUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for n := range before {
		if before[n] != after[n] {
			t.Errorf("%s was modified", n)
		}
	}
}

// TestRenameDryRunWritesNothing: on seedVerifyRepo every site is high
// (measured; no compiler index), so with --floor high the dry run would apply.
func TestRenameDryRunWritesNothing(t *testing.T) {
	root := seedVerifyRepo(t)
	before := snapshot(t, root, "store.go", "service.go")

	rep, code := renameJSON(t, root, "CreateOrder", "InsertOrder", "--floor", "high")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 for a dry run that would apply; refusals: %+v", code, rep.Refusals)
	}
	if rep.Applied || rep.Complete {
		t.Errorf("applied=%v complete=%v, want false/false", rep.Applied, rep.Complete)
	}
	if rep.Floor != model.ConfidenceHigh || !strings.Contains(rep.FloorNote, rename.HighPrecision) {
		t.Errorf("floor=%s note=%q, want high with the measured precision", rep.Floor, rep.FloorNote)
	}
	var def bool
	for _, s := range rep.Sites {
		if s.Kind == rename.KindDefinition && s.Path == "store.go" {
			def = true
		}
	}
	if !def {
		t.Errorf("the definition is not a site: %+v", rep.Sites)
	}
	assertUnchanged(t, before, snapshot(t, root, "store.go", "service.go"))
}

// TestRenameDefaultFloorIsExact: without --floor, a high site is refused, every
// one is listed, the exit is 2 and nothing is written.
func TestRenameDefaultFloorIsExact(t *testing.T) {
	root := seedVerifyRepo(t)
	before := snapshot(t, root, "store.go", "service.go")

	rep, code := renameJSON(t, root, "CreateOrder", "InsertOrder")
	if code != renameExitRefused {
		t.Fatalf("exit = %d, want %d", code, renameExitRefused)
	}
	if rep.Floor != model.ConfidenceExact || rep.FloorNote != "" {
		t.Errorf("floor=%s note=%q, want exact and no note", rep.Floor, rep.FloorNote)
	}
	refused := map[string]bool{}
	for _, r := range rep.Refusals {
		if strings.Contains(r.Reason, "below exact") {
			refused[r.Path] = true
		}
	}
	if !refused["store.go"] || !refused["service.go"] {
		t.Errorf("refusals %+v, want both sites below exact", rep.Refusals)
	}

	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder")
	if code := exitCodeOf(err); code != renameExitRefused {
		t.Fatalf("text exit = %d, want %d\n%s", code, renameExitRefused, out)
	}
	for _, want := range []string{"REFUSED", "below exact", "--floor high"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	assertUnchanged(t, before, snapshot(t, root, "store.go", "service.go"))
}

// TestRenameStatesItsLimits: the text report names what a rename cannot see.
func TestRenameStatesItsLimits(t *testing.T) {
	root := seedVerifyRepo(t)
	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--floor", "high")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"DRY RUN", rename.HighPrecision, "never compiles", "interface implementations",
		"pre-rename index", "build and test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// staleStore edits store.go after indexing and returns its new content. The
// definition is always a site, so this makes any rename of CreateOrder refuse,
// deterministically.
func staleStore(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "store.go")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(b) + "// edited after indexing\n"
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	return edited
}

// TestRenameRefusesAStaleIndex: the dry run reports exactly the refusal
// --apply would hit, with the same exit code.
func TestRenameRefusesAStaleIndex(t *testing.T) {
	root := seedVerifyRepo(t)
	edited := staleStore(t, root)
	p := filepath.Join(root, "store.go")

	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--floor", "high")
	if code := exitCodeOf(err); code != renameExitRefused {
		t.Fatalf("exit = %d, want %d\n%s", code, renameExitRefused, out)
	}
	for _, want := range []string{"REFUSED", "store.go", "changed since it was indexed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if got, _ := os.ReadFile(p); string(got) != edited {
		t.Error("a refused rename wrote store.go")
	}
}

func TestRenameRejectsBadInput(t *testing.T) {
	root := seedVerifyRepo(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"CreateOrder", "Insert Order"}, want: "not an identifier"},
		{args: []string{"NoSuchThing", "X"}, want: "no definition"},
		{args: []string{"CreateOrder", "X", "--floor", "medium"}, want: "--floor must be exact or high"},
	} {
		out, err := run(t, root, append([]string{"rename"}, tc.args...)...)
		if err == nil || !strings.Contains(err.Error()+out, tc.want) {
			t.Errorf("rename %v: err=%v, want one mentioning %q\n%s", tc.args, err, tc.want, out)
		}
	}
}

// TestRenameApplyRefusesAStaleIndexAndWritesNothing: --apply on a refused plan
// exits 2 and leaves every file as it was.
func TestRenameApplyRefusesAStaleIndexAndWritesNothing(t *testing.T) {
	root := seedVerifyRepo(t)
	edited := staleStore(t, root)
	before := snapshot(t, root, "service.go")

	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--floor", "high", "--apply")
	if code := exitCodeOf(err); code != renameExitRefused {
		t.Fatalf("exit = %d, want %d\n%s", code, renameExitRefused, out)
	}
	if got := snapshot(t, root, "store.go", "service.go"); got["store.go"] != edited || got["service.go"] != before["service.go"] {
		t.Errorf("a refused --apply wrote:\n%v", got)
	}
}

// TestRenameApplyAtTheDefaultFloorWritesNothing: high sites are refused by the
// exact floor even with --apply, and the user is told what would change that.
func TestRenameApplyAtTheDefaultFloorWritesNothing(t *testing.T) {
	root := seedVerifyRepo(t)
	before := snapshot(t, root, "store.go", "service.go")

	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--apply")
	if code := exitCodeOf(err); code != renameExitRefused {
		t.Fatalf("exit = %d, want %d\n%s", code, renameExitRefused, out)
	}
	for _, want := range []string{"REFUSED: nothing was written", "below exact", "--floor high"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	assertUnchanged(t, before, snapshot(t, root, "store.go", "service.go"))
	if _, err := os.Stat(filepath.Join(root, IndexDir, "rename-contract.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused rename wrote a contract (stat err = %v)", err)
	}
}

// TestRenameApplyRefusesWhileAnotherEditHoldsTheLock: a held lock is a refusal
// that wrote nothing, so it exits 2 and says how to clear it.
func TestRenameApplyRefusesWhileAnotherEditHoldsTheLock(t *testing.T) {
	root := seedVerifyRepo(t)
	before := snapshot(t, root, "store.go", "service.go")
	if err := os.WriteFile(filepath.Join(root, ".repocortex-edit.lock"), []byte("pid=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--floor", "high", "--apply")
	if code := exitCodeOf(err); code != renameExitRefused {
		t.Fatalf("exit = %d, want %d\n%s", code, renameExitRefused, out)
	}
	for _, want := range []string{"REFUSED: nothing was written", "another edit is in progress"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	assertUnchanged(t, before, snapshot(t, root, "store.go", "service.go"))
}

// TestRenameCommitOutcome: every Commit failure that left the files as they
// were is a refusal (2); only a rollback that could not restore is an error.
func TestRenameCommitOutcome(t *testing.T) {
	unlock := fmt.Errorf("%w .repocortex-edit.lock (delete it by hand): permission denied", edit.ErrLockRelease)
	for _, tc := range []struct {
		name    string
		err     error
		applied bool
		code    int
		want    string
	}{
		{"written", nil, true, 0, ""},
		{"written, lock left behind", errors.Join(nil, unlock), true, 0, "release lock"},
		{"locked", fmt.Errorf("%w (held)", edit.ErrLocked), false, renameExitRefused, "another edit is in progress"},
		{"stale", fmt.Errorf("edit: write a.go: %w: a.go changed", edit.ErrStale), false, renameExitRefused,
			"changed while the rename was being written"},
		{"stale, lock left behind", errors.Join(fmt.Errorf("x: %w", edit.ErrStale), unlock), false, renameExitRefused,
			"release lock"},
		{"io, rolled back", errors.New("edit: write a.go: disk full"), false, renameExitRefused,
			"every file already written was restored"},
		{"rollback incomplete", errors.Join(errors.New("edit: write b.go: disk full"), edit.ErrRollbackIncomplete), false, 1,
			"rollback was incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied, code, msg := commitOutcome(tc.err)
			if applied != tc.applied || code != tc.code || !strings.Contains(msg, tc.want) {
				t.Errorf("commitOutcome = %v %d %q, want %v %d and %q", applied, code, msg, tc.applied, tc.code, tc.want)
			}
		})
	}
}

// TestRenameApplyRewritesProvenSitesAndVerifies: on seedVerifyRepo the
// resolver binds `s.repo.CreateOrder(id)` at high (measured), so with
// --floor high every site passes and the rename must apply, write a contract,
// and verify.
func TestRenameApplyRewritesProvenSitesAndVerifies(t *testing.T) {
	root := seedVerifyRepo(t)

	dry, code := renameJSON(t, root, "CreateOrder", "InsertOrder", "--floor", "high")
	if code != 0 {
		t.Fatalf("fixture premise broken: dry run exit %d, refusals %+v", code, dry.Refusals)
	}
	for _, s := range dry.Sites {
		if !s.Confidence.AtLeast(model.ConfidenceHigh) {
			t.Fatalf("fixture premise broken: %s:%d is %s", s.Path, s.Line, s.Confidence)
		}
	}

	rep, code := renameJSON(t, root, "CreateOrder", "InsertOrder", "--floor", "high", "--apply")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; report: %+v", code, rep)
	}
	if !rep.Applied || len(rep.Files) != 2 {
		t.Fatalf("applied=%v files=%v, want both files", rep.Applied, rep.Files)
	}

	got := snapshot(t, root, "store.go", "service.go")
	if !strings.Contains(got["store.go"], "func (r *Repo) InsertOrder(id string) error") ||
		strings.Contains(got["store.go"], "CreateOrder") {
		t.Errorf("store.go:\n%s", got["store.go"])
	}
	if !strings.Contains(got["service.go"], "return s.repo.InsertOrder(id)") ||
		strings.Contains(got["service.go"], "CreateOrder") {
		t.Errorf("service.go:\n%s", got["service.go"])
	}

	raw, err := os.ReadFile(rep.Contract)
	if err != nil {
		t.Fatalf("contract not written: %v", err)
	}
	var c compile.Contract
	if err := json.Unmarshal(raw, &c); err != nil || c.Intent != "Rename CreateOrder to InsertOrder" {
		t.Fatalf("contract = %+v (%v)", c, err)
	}

	if rep.Total != 0 || len(rep.Items) != 0 || rep.LowerBound || rep.Unreadable != 0 {
		t.Errorf("leftovers on a complete rename: %+v", rep.LeftoverScan)
	}
	if rep.Verify == nil {
		t.Fatal("verify did not run")
	}
	if d := rep.Verify.Disposition; d != verify.DispositionPass && d != verify.DispositionAdvisory {
		t.Errorf("verify disposition = %s, want pass or advisory; findings %+v unknowns %v",
			d, rep.Verify.Findings, rep.Verify.Unknowns)
	}
	for _, pat := range []string{".repocortex-edit-*", ".repocortex-edit.lock"} {
		if matches, _ := filepath.Glob(filepath.Join(root, pat)); len(matches) > 0 {
			t.Errorf("left behind: %v", matches)
		}
	}
}

// TestRenameApplyStatesTheHighFloorBeforeWriting: the opt-in warning comes
// ahead of the result, once.
func TestRenameApplyStatesTheHighFloorBeforeWriting(t *testing.T) {
	root := seedVerifyRepo(t)
	out, err := run(t, root, "rename", "CreateOrder", "InsertOrder", "--floor", "high", "--apply")
	if code := exitCodeOf(err); code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	warn, applied := strings.Index(out, "WARNING --floor high"), strings.Index(out, "APPLIED to 2 file(s)")
	if warn < 0 || applied < 0 || warn > applied || strings.Count(out, rename.HighPrecision+" precision") != 1 {
		t.Errorf("want one high-floor warning ahead of APPLIED:\n%s", out)
	}
	for _, want := range []string{"VERIFY:", "pre-rename index", "build and test"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// fixtureCopy copies fixtures/<name> into a fresh project root, leaving out
// any .repocortex/ a local run left there. Never the fixture in place: rename
// writes source files.
func fixtureCopy(t *testing.T, name string) string {
	t.Helper()
	return fixtureCopyWith(t, name, nil)
}

// fixtureCopyWith is fixtureCopy with a hook that edits the copy before it is
// indexed.
func fixtureCopyWith(t *testing.T, name string, edit func(dst string)) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	dst := isolate(t)
	if err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == IndexDir {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p) //nolint:gosec // a fixture inside this repository
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(dst)
	}
	for _, step := range []string{"init", "index", "resolve"} {
		if out, err := run(t, dst, step); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, out)
		}
	}
	return dst
}

// TestRenameReportsLeftoversAndNeverPassesWithThem: measured on the payment
// fixture, the resolver binds the controller's call to updateStatus but not
// the two calls in the tests. Applying leaves the project uncompilable while
// verify — which never compiles — passes. The leftovers must be listed, and
// the exit must be 3, never 0.
func TestRenameReportsLeftoversAndNeverPassesWithThem(t *testing.T) {
	root := fixtureCopy(t, "payment-platform")
	want := []string{
		"src/test/java/com/pay/controller/CallbackControllerTest.java:8",
		"src/test/java/com/pay/service/TransactionServiceTest.java:7",
	}

	out, err := run(t, root, "rename", "updateStatus", "applyStatus", "--floor", "high")
	if code := exitCodeOf(err); code != 0 {
		t.Fatalf("dry run exit = %d\n%s", code, out)
	}
	for _, w := range append([]string{"LEFTOVER OCCURRENCES", "--apply would exit 3"}, want...) {
		if !strings.Contains(out, w) {
			t.Errorf("dry run does not mention %q:\n%s", w, out)
		}
	}

	rep, code := renameJSON(t, root, "updateStatus", "applyStatus", "--floor", "high", "--apply")
	if code != renameExitUnverified {
		t.Fatalf("exit = %d, want %d; report %+v", code, renameExitUnverified, rep)
	}
	if !rep.Applied || rep.Verify == nil {
		t.Fatalf("applied=%v verify=%v, want an applied, verified rename", rep.Applied, rep.Verify)
	}
	var got []string
	for _, l := range rep.Items {
		got = append(got, fmt.Sprintf("%s:%d", l.Path, l.Line))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || rep.Total != 2 {
		t.Errorf("leftovers = %v (total %d), want %v", got, rep.Total, want)
	}

	raw, err := os.ReadFile(filepath.Join(root, "src/test/java/com/pay/service/TransactionServiceTest.java"))
	if err != nil || !strings.Contains(string(raw), "service.updateStatus(") {
		t.Fatalf("premise: the unbound test call should be untouched (%v)", err)
	}
}

// TestRenameLeftoversIncludeFilesTheIndexHoldsNoTextFor: a file over the
// full-text size limit is recorded but has no searchable text. Its unbound
// call must still be found, and the apply must exit 3.
func TestRenameLeftoversIncludeFilesTheIndexHoldsNoTextFor(t *testing.T) {
	root := fixtureCopyWith(t, "payment-platform", func(dst string) {
		var b strings.Builder
		b.WriteString("package com.pay.gen; class Big { void f(com.pay.service.TransactionService s){ s.updateStatus(\"a\",\"b\"); } }\n")
		for b.Len() < 3<<20 {
			b.WriteString("// pad\n")
		}
		p := filepath.Join(dst, "src/main/java/com/pay/Big.java")
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	rep, code := renameJSON(t, root, "updateStatus", "applyStatus", "--floor", "high", "--apply")
	if code != renameExitUnverified {
		t.Fatalf("exit = %d, want %d; leftovers %+v", code, renameExitUnverified, rep.LeftoverScan)
	}
	var big bool
	for _, l := range rep.Items {
		if l.Path == "src/main/java/com/pay/Big.java" && l.Line == 1 {
			big = true
		}
	}
	if !big {
		t.Errorf("Big.java:1 is not a leftover: %+v", rep.Items)
	}
}
