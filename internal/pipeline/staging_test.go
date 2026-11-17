package pipeline

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
)

// A run that dies after staging must not leave its files in the user's tree,
// and the cleanup must never remove anything the user wrote or changed.
func TestRecoverStagedRemovesOnlyUnchangedFilesOfADeadRun(t *testing.T) {
	root := t.TempDir()
	ledgerDir := filepath.Join(root, ".repocortex", "scip")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ours := write("web/App.vue.ts", "export default {}\n")
	edited := write("web/Edited.vue.ts", "export default {}\n")
	pyright := write("svc/pyrightconfig.json", pythonRootConfig)
	tsconfig := write("web/tsconfig.json", "{}\n")
	if err := writeStagingLedger(ledgerDir, []string{ours, edited, pyright}, []string{tsconfig}); err != nil {
		t.Fatal(err)
	}
	// The user changed one of the files after the run died.
	write("web/Edited.vue.ts", "export const mine = 1\n")

	removed, leftover := recoverStaged(ledgerDir, root)
	if removed != 2 {
		t.Errorf("removed %d files, want 2 (the unchanged mirror and pyrightconfig)", removed)
	}
	for _, p := range []string{ours, pyright} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived recovery", p)
		}
	}
	if _, err := os.Stat(edited); err != nil {
		t.Errorf("a file the user changed was removed: %v", err)
	}
	if !reflect.DeepEqual(leftover, []string{tsconfig}) {
		t.Errorf("leftover = %v, want the inferred tsconfig reported, not deleted", leftover)
	}
	if _, err := os.Stat(tsconfig); err != nil {
		t.Errorf("an inferred tsconfig was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, stagingLedger)); !os.IsNotExist(err) {
		t.Error("ledger not cleared after recovery")
	}
	// Nothing to do on a clean directory.
	if n, l := recoverStaged(ledgerDir, root); n != 0 || l != nil {
		t.Errorf("second recovery = %d, %v; want nothing", n, l)
	}
}

// A mirror is never written over a file the user has at that path.
func TestStageVueMirrorsDoesNotOverwrite(t *testing.T) {
	root := t.TempDir()
	vue := filepath.Join(root, "App.vue")
	if err := os.WriteFile(vue, []byte("<script lang=\"ts\">\nexport default {}\n</script>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mine := vue + ".ts"
	if err := os.WriteFile(mine, []byte("// mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staged := stageVueMirrors(root, []indexer.Project{{Language: indexer.LangTypeScript, Dir: "."}})
	if len(staged) != 0 {
		t.Errorf("staged %v over an existing file", staged)
	}
	if raw, _ := os.ReadFile(mine); string(raw) != "// mine\n" {
		t.Errorf("existing file overwritten: %q", raw)
	}
}

// The ledger lives in the repository and is not trusted: a path outside the
// repository is never removed, whatever its digest.
func TestRecoverStagedIgnoresPathsOutsideTheRepository(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(root, ".repocortex", "scip")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeStagingLedger(ledgerDir, []string{outside}, nil); err != nil {
		t.Fatal(err)
	}
	if removed, _ := recoverStaged(ledgerDir, root); removed != 0 {
		t.Errorf("removed %d files outside the repository", removed)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the repository was deleted: %v", err)
	}
}

// A symlinked folder inside the repository must not let a ledger entry that
// looks contained reach a file outside it, and a symlink at a staged path is
// never followed.
func TestRecoverStagedDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "keep.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "staged.vue.ts")); err != nil {
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(root, ".repocortex", "scip")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Both entries carry the victim's real digest, as a crafted ledger would.
	d, _ := fileDigest(victim)
	raw := `[{"path":"` + filepath.Join(root, "link", "keep.txt") + `","sha256":"` + d + `"},` +
		`{"path":"` + filepath.Join(root, "staged.vue.ts") + `","sha256":"` + d + `"}]`
	if err := os.WriteFile(filepath.Join(ledgerDir, stagingLedger), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, _ := recoverStaged(ledgerDir, root); removed != 0 {
		t.Errorf("removed %d entries reached through symlinks", removed)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("a file outside the repository was deleted through a symlink: %v", err)
	}
}
