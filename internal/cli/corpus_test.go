package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusRoot copies fixtures/corpus into a temporary directory.
//
// A copy, not the fixture in place: indexing writes .repocortex/, and a gate that
// mutates the repository it is run from is a gate people learn to skip.
func corpusRoot(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("corpus fixture missing at %s: %v", src, err)
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
	return dst
}

func corpusDB(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, ".repocortex", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func scalar(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestCorpusGate is the regression gate over fixtures/corpus.
//
// Every construct in that fixture broke RepoCortex on a real repository. The
// gate exists because all of them were invisible until real code arrived, and
// none of them would have been predicted: a Vue envelope, a Python decorator,
// a Tailwind class that parses as SQL, a TypeScript line declaring two things
// with one name, a Go blank identifier, a config key inside a comment.
//
// It asserts two different kinds of thing, and the distinction is the point:
//
//   - SHAPES, below — that this specific construct still produces the right
//     answer. These catch the return of a known bug.
//   - INVARIANTS, via doctor — properties that must hold for ANY input. These
//     are the only part that can catch a bug nobody has seen, which is the
//     class every one of today's defects belonged to.
func TestCorpusGate(t *testing.T) {
	root := corpusRoot(t)
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v on the corpus: %v\n%s", args, err, out)
		}
	}
	db := corpusDB(t, root)

	t.Run("no file defeated the indexer", func(t *testing.T) {
		// The guarantee that matters most for code nobody has tested against:
		// a file RepoCortex cannot handle costs that file, never the run.
		if n := scalar(t, db, `SELECT COUNT(*) FROM files WHERE parse_status = 'failed'`); n != 0 {
			var path, reason string
			_ = db.QueryRow(`SELECT path, COALESCE(parse_error,'') FROM files
			                  WHERE parse_status = 'failed' LIMIT 1`).Scan(&path, &reason)
			t.Errorf("%d file(s) could not be persisted, e.g. %s: %s", n, path, reason)
		}
	})

	t.Run("every source file was indexed", func(t *testing.T) {
		for _, want := range []string{
			"frontend/Component.vue", "frontend/spec.ts",
			"backend/py/handlers.py", "backend/go/service.go", "db/schema.sql",
		} {
			if n := scalar(t, db, `SELECT COUNT(*) FROM files WHERE path = '`+want+
				`' AND parse_status = 'ok'`); n != 1 {
				t.Errorf("%s was not indexed", want)
			}
		}
	})

	t.Run("vue script block yields symbols with true line numbers", func(t *testing.T) {
		// A .vue file contributed nothing at all until the SFC path existed.
		if n := scalar(t, db, `SELECT COUNT(*) FROM symbols s JOIN files f ON f.id = s.file_id
		                        WHERE f.path = 'frontend/Component.vue'`); n == 0 {
			t.Fatal("the component's script block produced no symbols")
		}
		// The offset is the part that can be confidently wrong: a location
		// that lands on the wrong line is worse than an honest gap.
		var line int
		if err := db.QueryRow(`SELECT s.start_line FROM symbols s JOIN files f ON f.id = s.file_id
		                        WHERE f.path = 'frontend/Component.vue' AND s.name = 'loadBranches'`).
			Scan(&line); err != nil {
			t.Fatalf("loadBranches not found in the component: %v", err)
		}
		src, err := os.ReadFile(filepath.Join(root, "frontend", "Component.vue")) //nolint:gosec // fixture copy
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		if line < 1 || line > len(lines) {
			t.Fatalf("loadBranches reported at line %d, outside a %d-line file", line, len(lines))
		}
		if !strings.Contains(lines[line-1], "loadBranches") {
			t.Errorf("loadBranches reported at line %d, which reads %q — the script "+
				"block offset is wrong", line, strings.TrimSpace(lines[line-1]))
		}
	})

	t.Run("css classes are not tables", func(t *testing.T) {
		for _, notATable := range []string{"flex-1", "font-medium"} {
			if n := scalar(t, db, `SELECT COUNT(*) FROM table_accesses WHERE table_name = '`+
				notATable+`'`); n != 0 {
				t.Errorf("a Tailwind class %q was recorded as a database table", notATable)
			}
		}
		// And the real one survived, so the fix is not simply rejecting everything.
		if n := scalar(t, db, `SELECT COUNT(*) FROM table_accesses WHERE table_name = 'branches'`); n == 0 {
			t.Error("the real table `branches` was lost along with the false ones")
		}
	})

	t.Run("a comment is not a config read", func(t *testing.T) {
		if n := scalar(t, db, `SELECT COUNT(*) FROM config_usages WHERE key_name = '...'`); n != 0 {
			t.Error(`a key named "..." was read out of a comment`)
		}
		if n := scalar(t, db, `SELECT COUNT(*) FROM config_usages WHERE key_name = 'GO_REAL_KEY'`); n == 0 {
			t.Error("the real env read was lost along with the commented one")
		}
	})

	t.Run("determinism", func(t *testing.T) {
		// Two independent indexes of identical input must agree on content.
		// Nothing else here would notice a map iteration or a goroutine race.
		first := indexDigest(t, root)
		second := corpusRoot(t)
		for _, args := range [][]string{{"init"}, {"index"}} {
			if out, err := run(t, second, args...); err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
		}
		if got := indexDigest(t, second); got != first {
			t.Errorf("two indexes of the same corpus disagree:\n  %s\n  %s", first, got)
		}
	})
}

// indexDigest returns the registry digest for a corpus root.
func indexDigest(t *testing.T, root string) string {
	t.Helper()
	out, err := run(t, root, "index", "--full", "--digest", "--json")
	if err != nil {
		t.Fatalf("digest: %v\n%s", err, out)
	}
	const key = `"registry_digest": "`
	i := strings.Index(out, key)
	if i < 0 {
		t.Fatalf("no registry_digest in:\n%s", out)
	}
	rest := out[i+len(key):]
	return rest[:strings.Index(rest, `"`)]
}

// TestCorpusInvariantsHold runs the same structural checks `repocortex doctor`
// offers a user, over the corpus.
//
// These are the checks that can catch a defect nobody has seen. A user hitting
// an unknown construct on their own repository runs the same command and gets
// the same answer, without having to report anything or wait for us — which
// matters, because most people will not report it. They will just stop
// trusting the tool.
func TestCorpusInvariantsHold(t *testing.T) {
	root := corpusRoot(t)
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	out, err := run(t, root, "doctor", "--verify-index")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[FAIL") {
			t.Errorf("invariant violated on the corpus: %s", strings.TrimSpace(line))
		}
	}
	// The gate is worthless if doctor silently stopped checking things.
	for _, invariant := range []string{
		"dangling edges", "orphaned symbols", "unproven exact edges",
		"duplicate declarations", "unreferenceable symbols",
		"implausible table names", "implausible config keys",
	} {
		if !strings.Contains(out, invariant) {
			t.Errorf("doctor no longer runs the %q check", invariant)
		}
	}
}
