package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// run executes the CLI in-process against an isolated project root.
func run(t *testing.T, projectRoot string, args ...string) (stdout string, err error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	full := append([]string{"--project", projectRoot, "--quiet"}, args...)
	root.SetArgs(full)
	err = root.ExecuteContext(context.Background())
	return out.String(), err
}

// isolate gives each test its own project root and user-config home so tests
// never read the developer's real ~/.config/repocortex.
func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	return t.TempDir()
}

func TestInitCreatesIndex(t *testing.T) {
	root := isolate(t)

	out, err := run(t, root, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	dbPath := filepath.Join(root, IndexDir, IndexFile)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("index not created: %v", err)
	}

	// The registry maps the whole codebase; committing it would be a leak.
	ignore, err := os.ReadFile(filepath.Join(root, IndexDir, ".gitignore"))
	if err != nil {
		t.Fatalf("index dir must self-ignore: %v", err)
	}
	if strings.TrimSpace(string(ignore)) != "*" {
		t.Errorf(".repocortex/.gitignore = %q, want \"*\"", ignore)
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "init"); err == nil {
		t.Fatal("second init should refuse without --force")
	}
	if _, err := run(t, root, "init", "--force"); err != nil {
		t.Fatalf("init --force should succeed: %v", err)
	}
}

func TestStatusJSONIsMachineReadable(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status --json emitted non-JSON on stdout: %v\n%s", err, out)
	}
	if got["index_status"] != "empty" {
		t.Errorf("index_status = %v, want empty", got["index_status"])
	}
	if got["schema_version"].(float64) < 1 {
		t.Errorf("schema_version = %v, want >= 1", got["schema_version"])
	}
}

func TestStatusOnMissingIndex(t *testing.T) {
	root := isolate(t)
	out, err := run(t, root, "status")
	if err != nil {
		t.Fatalf("status on missing index should not error: %v", err)
	}
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("expected ABSENT, got:\n%s", out)
	}
}

func TestDoctorPassesOnFreshIndex(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "doctor", "--verify-index")
	if err != nil {
		t.Fatalf("doctor failed on a fresh index: %v\n%s", err, out)
	}
	for _, want := range []string{"fts5", "dangling edges", "unproven exact edges"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorDetectsDanglingEdges proves the invariant gate can actually fail.
//
// A check that has only ever been observed passing is not evidence of
// anything. This injects a real violation and requires doctor to catch it.
func TestDoctorDetectsDanglingEdges(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, ctx)

	// Foreign keys would normally block this, so drop the constraint's effect
	// for one statement to simulate corruption arriving another way (a bad
	// migration, a restored backup, a bug in a future writer).
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 9999, 'CALLS', 'high', 'source_code')`); err != nil {
		t.Fatalf("seed dangling edge: %v", err)
	}
	db.Close()

	out, err := run(t, root, "doctor", "--verify-index")
	if err == nil {
		t.Fatalf("doctor did not fail on a dangling edge:\n%s", out)
	}
	if !strings.Contains(out, "dangling edges") || !strings.Contains(out, "FAIL") {
		t.Errorf("expected a dangling-edge FAIL, got:\n%s", out)
	}
	t.Logf("gate correctly failed: %v", err)
}

// TestDoctorDetectsUnprovenExactEdge guards the product's central promise: an
// edge may not claim compiler certainty unless a compiler produced it.
func TestDoctorDetectsUnprovenExactEdge(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(root, IndexDir, IndexFile)})
	if err != nil {
		t.Fatal(err)
	}
	seed(t, db, ctx)

	// Layer 1: the storage CHECK must reject the corrupt write outright.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 2, 'CALLS', 'exact', 'inferred')`); err == nil {
		t.Fatal("storage accepted confidence=exact with source=inferred")
	}

	// A legitimate exact edge must still be insertable...
	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 2, 'CALLS', 'exact', 'compiler')`); err != nil {
		t.Fatalf("legitimate compiler-backed exact edge rejected: %v", err)
	}
	// ...and must not be downgradable into a false claim afterwards.
	updateErr := func() error {
		_, err := db.ExecContext(ctx,
			`UPDATE edges SET source = 'inferred' WHERE confidence = 'exact'`)
		return err
	}()
	if updateErr == nil {
		// The CHECK let it through, so layer 2 (the doctor gate) must catch it.
		db.Close()
		out, err := run(t, root, "doctor", "--verify-index")
		if err == nil {
			t.Fatalf("neither the CHECK nor doctor flagged an unproven exact edge:\n%s", out)
		}
		if !strings.Contains(out, "unproven exact edges") {
			t.Errorf("expected an unproven-exact-edge failure, got:\n%s", out)
		}
		t.Logf("doctor gate caught it: %v", err)
		return
	}
	db.Close()
	t.Logf("storage CHECK rejected the downgrade outright (stronger than the doctor gate): %v", updateErr)
}

func TestVersionJSON(t *testing.T) {
	root := isolate(t)
	out, err := run(t, root, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("version --json invalid: %v\n%s", err, out)
	}
	for _, k := range []string{"version", "commit", "build_time", "go_os", "go_arch"} {
		if v[k] == "" {
			t.Errorf("version --json missing %q", k)
		}
	}
}

func TestUnknownProjectRootIsRejected(t *testing.T) {
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	if _, err := run(t, filepath.Join(t.TempDir(), "does-not-exist"), "status"); err == nil {
		t.Fatal("expected an error for a nonexistent project root")
	}
}

func seed(t *testing.T, db *store.DB, ctx context.Context) {
	t.Helper()
	for _, s := range []string{
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`,
		`INSERT INTO files(id, index_run_id, path, content_hash) VALUES(1,1,'a.go','h')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		 VALUES(1,1,1,'function','A','pkg.A','high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		 VALUES(2,1,1,'function','B','pkg.B','high','source_code')`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}
