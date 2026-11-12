package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIncrementalEditDoesNotEraseAnalysisFacts pins silent data loss.
//
// The incremental path writes through ReplaceFile, which deletes and re-inserts
// the file row; ON DELETE CASCADE then removes that file's table_accesses and
// config_usages. Analysis was only ever attached on the FULL path, so touching
// one file erased its endpoints and its table writes — `repocortex api` went from
// one endpoint to zero — and nothing reported it, because an empty result and a
// correct empty result are indistinguishable.
//
// With `repocortex watch` running, the whole analysis layer eroded over a working
// session. Five edits here rather than one, because a bug that only bites on
// the second write would pass a single-edit test.
func TestIncrementalEditDoesNotEraseAnalysisFacts(t *testing.T) {
	root := isolate(t)

	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	controller := filepath.Join(src, "C.java")
	body := `@RestController
@RequestMapping("/payment")
public class C {
    @PostMapping("/callback")
    public void handle(String id) {
        jdbc.update("UPDATE transactions SET status = ? WHERE id = ?", id);
    }
}`
	if err := os.WriteFile(controller, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "V1.sql"),
		[]byte("CREATE TABLE transactions (id VARCHAR(64));"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	before, err := run(t, root, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, "POST /payment/callback") {
		t.Fatalf("setup failed — the endpoint was never indexed:\n%s", before)
	}

	// Edit the file repeatedly, exactly as an editor would while `watch` runs.
	for i := 0; i < 5; i++ {
		body += "\n// touch\n"
		if err := os.WriteFile(controller, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, root, "index"); err != nil {
			t.Fatalf("incremental index %d: %v\n%s", i, err, out)
		}
	}

	after, err := run(t, root, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "POST /payment/callback") {
		t.Errorf("the endpoint was erased by incremental re-indexing:\n%s", after)
	}

	// The table write must survive too — it is staged in a different table and
	// re-attached by a different code path.
	if out, err := run(t, root, "resolve"); err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	db, err := run(t, root, "db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db, "transactions") {
		t.Errorf("the table write was erased by incremental re-indexing:\n%s", db)
	}
}
