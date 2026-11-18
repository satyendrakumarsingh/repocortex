package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
)

// builtProject returns a project with a built index.
func builtProject(t *testing.T) (root, dbPath string) {
	t.Helper()
	root = seedMixedRepo(t)
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root, filepath.Join(root, IndexDir, IndexFile)
}

// A second writer must wait for the lock and then say who holds it. Before the
// fix `index` and `resolve` did not take reindex.lock at all, so a second
// `index` ran on top of a live one and reclaimed its rows as "abandoned".
func TestIndexAndResolveRefuseWhileAnotherWriterHoldsTheLock(t *testing.T) {
	root, dbPath := builtProject(t)
	old := indexLockWait
	indexLockWait = 200 * time.Millisecond
	t.Cleanup(func() { indexLockWait = old })

	lockPath := filepath.Join(filepath.Dir(dbPath), "reindex.lock")
	lock, err := fs.TryLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	release := func() {
		if held {
			_ = lock.Unlock()
			held = false
		}
	}
	t.Cleanup(release)

	// An unfinished run, as a live indexer in another process has mid-build.
	// It must survive: only a writer holding the lock may call it abandoned.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO index_runs(status, committed, started_unix, accuracy_mode) VALUES('indexing', 0, 1, 'fast')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	for _, args := range [][]string{{"index"}, {"index", "--full"}, {"resolve"}} {
		out, err := run(t, root, args...)
		if err == nil {
			t.Fatalf("%v succeeded while another process held the write lock:\n%s", args, out)
		}
		if !strings.Contains(err.Error(), "another index is running") || !strings.Contains(err.Error(), "reindex.lock") {
			t.Errorf("%v: error %q does not say another index is running and where its lock is", args, err)
		}
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM index_runs WHERE committed = 0`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if n != 1 {
		t.Errorf("the other writer's unfinished run was reclaimed while it held the lock: %d left, want 1", n)
	}

	release()
	if out, err := run(t, root, "index"); err != nil {
		t.Fatalf("index after the lock was released: %v\n%s", err, out)
	}
}

// Two indexes started together run one after the other; neither corrupts the
// index and both report success.
func TestConcurrentIndexesAreSerialised(t *testing.T) {
	root, _ := builtProject(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, args := range [][]string{{"index", "--full"}, {"index", "--full"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = run(t, root, args...)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent index %d: %v", i, err)
		}
	}
	out, err := run(t, root, "doctor", "--verify-index")
	if err != nil {
		t.Fatalf("doctor after concurrent indexes: %v\n%s", err, out)
	}
}

func TestNewerSchemaIsRefusedByEveryCommand(t *testing.T) {
	root, dbPath := builtProject(t)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version,name,checksum,applied_unix) VALUES(99,'future','abc',0)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	for _, args := range [][]string{
		{"index"}, {"index", "--full"}, {"resolve"}, {"migrate"}, {"symbol", "Clean"},
	} {
		out, err := run(t, root, args...)
		if err == nil {
			t.Errorf("%v accepted an index from a newer build:\n%s", args, out)
			continue
		}
		for _, want := range []string{"newer repocortex", "v99", "init --force"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: error %q lacks %q", args, err, want)
			}
		}
		if strings.Contains(err.Error(), "to upgrade it") {
			t.Errorf("%v: error still tells the user to run a command that cannot upgrade it: %q", args, err)
		}
	}

	out, _ := run(t, root, "status")
	if strings.Contains(out, "OUTDATED") || !strings.Contains(out, "newer repocortex") {
		t.Errorf("status misdescribes an index from a newer build:\n%s", out)
	}
	if out, err := run(t, root, "doctor"); err == nil || !strings.Contains(out, "newer repocortex") {
		t.Errorf("doctor should fail on an index from a newer build: err=%v\n%s", err, out)
	}

	// Nothing was written.
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var latest int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil || latest != 99 {
		t.Errorf("schema_migrations max = %d, %v; want it left at 99", latest, err)
	}
}

func TestCorruptIndexIsNamedAndDoctorFails(t *testing.T) {
	cases := map[string]func(t *testing.T, dbPath string){
		"garbage": func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(strings.Repeat("this is not sqlite ", 500)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"zero bytes": func(t *testing.T, p string) {
			if err := os.Truncate(p, 0); err != nil {
				t.Fatal(err)
			}
		},
		"truncated": func(t *testing.T, p string) {
			if err := os.Truncate(p, 1000); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			root, dbPath := builtProject(t)
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
			damage(t, dbPath)

			for _, args := range [][]string{{"symbol", "Clean"}, {"status"}, {"index"}, {"resolve"}} {
				out, err := run(t, root, args...)
				if err == nil {
					t.Errorf("%v succeeded on a corrupt index:\n%s", args, out)
					continue
				}
				for _, want := range []string{"corrupt", "init --force"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%v: error %q lacks %q", args, err, want)
					}
				}
				if strings.Contains(err.Error(), "schema is v0") {
					t.Errorf("%v: %q calls an empty file an old schema", args, err)
				}
			}

			out, err := run(t, root, "doctor")
			if err == nil {
				t.Errorf("doctor exited 0 on a corrupt index:\n%s", out)
			}
			if !strings.Contains(out, "FAIL") || strings.Contains(out, "[ok  ]  index") {
				t.Errorf("doctor does not report the index as failed:\n%s", out)
			}
		})
	}
}

func TestReadOnlyIndexCanBeQueried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	root, dbPath := builtProject(t)
	dir := filepath.Dir(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(dbPath, 0o644)
	})

	for _, args := range [][]string{{"symbol", "Clean"}, {"status"}} {
		out, err := run(t, root, args...)
		if err != nil {
			t.Errorf("%v on a read-only index: %v\n%s", args, err, out)
		}
	}
	out, _ := run(t, root, "symbol", "Clean")
	if !strings.Contains(out, "Clean") {
		t.Errorf("symbol found nothing in a read-only index:\n%s", out)
	}
}

var (
	indexFilesLine = regexp.MustCompile(`Files:\s+(\d+) indexed, (\d+) skipped \((\d+) discovered\)`)
	doctorFilesRow = regexp.MustCompile(`(\d+) files indexed, (\d+) skipped \((\d+) discovered\)`)
)

// doctor and index describe the same run, so their totals must agree. doctor
// used to print every discovered file as the total, skipped ones included.
func TestDoctorParseTotalsAgreeWithIndex(t *testing.T) {
	root := seedMixedRepo(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	idx, err := run(t, root, "index")
	if err != nil {
		t.Fatalf("index: %v\n%s", err, idx)
	}
	doc, err := run(t, root, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, doc)
	}
	a := indexFilesLine.FindStringSubmatch(idx)
	b := doctorFilesRow.FindStringSubmatch(oneLine(doc))
	if a == nil || b == nil {
		t.Fatalf("cannot find the totals.\nindex:\n%s\ndoctor:\n%s", idx, doc)
	}
	if strings.Join(a[1:], "/") != strings.Join(b[1:], "/") {
		t.Errorf("index says %v indexed/skipped/discovered, doctor says %v", a[1:], b[1:])
	}
}
