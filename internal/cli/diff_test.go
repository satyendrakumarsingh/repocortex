package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/diffgraph"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// runDiffCLI executes `repocortex diff` in-process against an isolated project.
//
// The command is assembled here rather than fetched out of NewRoot so the test
// exercises newDiffCmd itself and does not depend on where in the root tree the
// command happens to be registered. The persistent flags are the ones the root
// declares, because the command reads --json through cobra's inherited flag set
// exactly as `docs` and `verify` do.
func runDiffCLI(t *testing.T, projectRoot string, args ...string) (string, error) {
	t.Helper()

	g := &Global{}
	root := &cobra.Command{
		Use:           "repocortex",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return g.setup(cmd)
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON on stdout")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "verbose logging")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "suppress all logging")
	pf.StringVar(&g.Project, "project", "", "project root")
	root.AddCommand(newDiffCmd(g))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--project", projectRoot, "--quiet", "diff"}, args...))

	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// diffSeed describes one run to write into the fixture index.
type diffSeed struct {
	id        int64
	revision  string
	committed bool
	digest    string
}

// seedDiffIndex builds an index holding the given runs, all over IDENTICAL
// content.
//
// Identical content is the point: every assertion below about an empty diff is
// then a statement about the comparison rather than about the fixture. The rows
// are written directly because the pipeline that would otherwise produce them
// needs a real repository, and this test is about ref resolution and
// determinism, not about extraction.
func seedDiffIndex(t *testing.T, root string, runs ...diffSeed) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var stmts []string
	for _, r := range runs {
		committed := 0
		if r.committed {
			committed = 1
		}
		file := r.id * 100
		sym := r.id * 1000

		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO index_runs(id, status, committed, started_unix, git_revision,
			                        accuracy_mode, registry_digest,
			                        files_indexed, symbols_found, edges_found)
			   VALUES(%d,'ready',%d,0,'%s','exact','%s',2,3,2)`,
			r.id, committed, r.revision, r.digest),

			fmt.Sprintf(`INSERT INTO files(id, index_run_id, path, language, content_hash)
			   VALUES(%d,%d,'pkg/api.go','go','h1')`, file+1, r.id),
			fmt.Sprintf(`INSERT INTO files(id, index_run_id, path, language, content_hash)
			   VALUES(%d,%d,'pkg/service.go','go','h2')`, file+2, r.id),

			fmt.Sprintf(`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
			                                 start_line, end_line, confidence, source)
			   VALUES(%d,%d,%d,'function','handleRefund','pkg/api.go:handleRefund',
			          20,30,'high','source_code')`, sym+1, r.id, file+1),
			fmt.Sprintf(`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
			                                 start_line, end_line, confidence, source)
			   VALUES(%d,%d,%d,'method','Refund','pkg/service.go:Refund',
			          5,12,'high','source_code')`, sym+2, r.id, file+2),
			fmt.Sprintf(`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
			                                 start_line, end_line, confidence, source)
			   VALUES(%d,%d,%d,'method','legacyRefund','pkg/service.go:legacyRefund',
			          80,90,'high','source_code')`, sym+3, r.id, file+2),

			fmt.Sprintf(`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                               evidence_file, evidence_line, source)
			   VALUES(%d,%d,%d,'CALLS','high','pkg/api.go',22,'source_code')`,
				r.id, sym+1, sym+2),
			fmt.Sprintf(`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
			                               evidence_file, evidence_line, source)
			   VALUES(%d,%d,%d,'CALLS','high','pkg/api.go',23,'source_code')`,
				r.id, sym+1, sym+3),
		)
	}

	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

// diffFixture creates a project, initialises its index and seeds the runs.
func diffFixture(t *testing.T, runs ...diffSeed) string {
	t.Helper()
	root := isolate(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	seedDiffIndex(t, root, runs...)
	return root
}

// TestDiffRunAgainstItselfIsEmpty is the property the whole command rests on.
//
// If a run does not compare equal to itself then every other output the command
// produces is noise, so this is asserted on both renderings: the human one must
// say so in words, and the JSON one must carry no change entries at all.
func TestDiffRunAgainstItselfIsEmpty(t *testing.T) {
	root := diffFixture(t, diffSeed{id: 1, revision: "8f3c1ab9de", committed: true, digest: "d1"})

	out, err := runDiffCLI(t, root, "--from", "1", "--to", "1")
	if err != nil {
		t.Fatalf("diff: %v\n%s", err, out)
	}
	t.Logf("rendered:\n%s", out)

	if !strings.Contains(out, "no semantic differences") {
		t.Errorf("a run diffed against itself must report no differences, got:\n%s", out)
	}
	for _, section := range []string{"\nSYMBOLS\n", "\nEDGES\n", "\nENDPOINTS\n", "\nTABLES\n"} {
		if strings.Contains(out, section) {
			t.Errorf("self-diff emitted a %q section:\n%s", strings.TrimSpace(section), out)
		}
	}

	jsonOut, err := runDiffCLI(t, root, "--from", "1", "--to", "1", "--json")
	if err != nil {
		t.Fatalf("diff --json: %v\n%s", err, jsonOut)
	}
	var rep diffgraph.Report
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("diff --json emitted non-JSON on stdout: %v\n%s", err, jsonOut)
	}
	if !rep.Empty() {
		t.Errorf("self-diff report is not empty: %d symbols, %d edges, %d endpoints, %d tables",
			len(rep.Symbols), len(rep.Edges), len(rep.Endpoints), len(rep.Tables))
	}
	if rep.Base.RunID != 1 || rep.Head.RunID != 1 {
		t.Errorf("report names runs %d..%d, want 1..1", rep.Base.RunID, rep.Head.RunID)
	}
	// Equal digests mean the two sides hashed identically over content, so any
	// reported difference would be a bug in the comparison. The report says so
	// itself; assert the accusation is absent.
	if !rep.Caveats.DigestsEqual {
		t.Error("caveats should record that the two sides have equal digests")
	}
	for _, note := range rep.Caveats.Notes {
		if strings.Contains(note, "INCONSISTENT") {
			t.Errorf("report accuses itself of being wrong: %s", note)
		}
	}
}

// TestDiffResolvesCommitSHAAcrossTwoRuns covers the resolution the command
// exists for — a ref that is a recorded commit SHA rather than a run id — and
// repeats the determinism claim across two genuinely distinct runs.
func TestDiffResolvesCommitSHAAcrossTwoRuns(t *testing.T) {
	root := diffFixture(t,
		diffSeed{id: 1, revision: "8f3c1ab9de", committed: true, digest: "d1"},
		diffSeed{id: 2, revision: "a19d7e04bc", digest: "d1"},
	)

	out, err := runDiffCLI(t, root, "8f3c1ab..a19d7e0")
	if err != nil {
		t.Fatalf("diff by SHA prefix: %v\n%s", err, out)
	}
	t.Logf("rendered:\n%s", out)

	if !strings.Contains(out, "(runs 1..2)") {
		t.Errorf("SHA prefixes did not resolve to runs 1 and 2:\n%s", out)
	}
	if !strings.Contains(out, "no semantic differences") {
		t.Errorf("two runs over identical content must compare equal, got:\n%s", out)
	}

	// The flag spelling must resolve identically, including the default head.
	viaFlags, err := runDiffCLI(t, root, "--from", "a19d7e0")
	if err != nil {
		t.Fatalf("diff --from with default --to: %v\n%s", err, viaFlags)
	}
	if !strings.Contains(viaFlags, "(runs 2..1)") {
		t.Errorf("--to should default to the committed run (1):\n%s", viaFlags)
	}
}

// TestDiffUnknownRefFails is the other half of the design: a ref nobody indexed
// must fail loudly, name how to produce the missing run, and produce no report.
// Falling back to the committed run would answer a question nobody asked.
func TestDiffUnknownRefFails(t *testing.T) {
	root := diffFixture(t, diffSeed{id: 1, revision: "8f3c1ab9de", committed: true, digest: "d1"})

	cases := []struct {
		name string
		args []string
	}{
		{"unknown sha", []string{"--from", "deadbee", "--to", "1"}},
		{"unknown run id", []string{"--from", "1", "--to", "99"}},
		{"unknown sha in range form", []string{"deadbee..8f3c1ab"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runDiffCLI(t, root, tc.args...)
			if err == nil {
				t.Fatalf("expected a failure for an unindexed ref, got a report:\n%s", out)
			}
			msg := err.Error()
			t.Logf("error: %s", msg)

			for _, want := range []string{
				"no indexed run matches",
				"repocortex index",
				"runs recorded in",
				"run 1",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error message missing %q:\n%s", want, msg)
				}
			}
			if strings.Contains(out, "no semantic differences") || strings.Contains(out, "COULD NOT COMPARE") {
				t.Errorf("a failed ref resolution still rendered a report:\n%s", out)
			}
		})
	}
}

// TestDiffAmbiguousRefFails: a prefix matching two runs has no right answer, and
// picking the lower id would silently diff the wrong revision.
func TestDiffAmbiguousRefFails(t *testing.T) {
	root := diffFixture(t,
		diffSeed{id: 1, revision: "abc1111111", committed: true, digest: "d1"},
		diffSeed{id: 2, revision: "abc2222222", digest: "d1"},
	)

	out, err := runDiffCLI(t, root, "--from", "abc", "--to", "1")
	if err == nil {
		t.Fatalf("an ambiguous ref must not resolve, got:\n%s", out)
	}
	msg := err.Error()
	t.Logf("error: %s", msg)
	for _, want := range []string{"matches 2 indexed runs", "run 1", "run 2", "longer SHA"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ambiguity error missing %q:\n%s", want, msg)
		}
	}
}

// TestDiffRefSpellingErrors covers the argument shapes that have no unambiguous
// reading, which are rejected rather than resolved by convention.
func TestDiffRefSpellingErrors(t *testing.T) {
	root := diffFixture(t, diffSeed{id: 1, revision: "8f3c1ab9de", committed: true, digest: "d1"})

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"range and flags together", []string{"1..1", "--from", "1"}, "not both"},
		{"no base at all", []string{"--to", "1"}, "--from is required"},
		{"not a range", []string{"1"}, "is not a range"},
		{"empty base in range", []string{"..1"}, "--from is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runDiffCLI(t, root, tc.args...)
			if err == nil {
				t.Fatalf("expected a failure, got:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
