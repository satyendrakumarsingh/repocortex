package pipeline_test

// Incremental indexing must produce the graph a fresh index would produce.
//
// This is not a performance test and not a smoke test. `repocortex diff` reports
// what changed about the code's FACTS between two runs. If the incremental path
// and the full path disagree, diff reports a change that the source never made,
// and there is no way for a user to tell that apart from a real one. The whole
// command rests on this equality holding.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/cli"
)

// graphFingerprint reduces a run's graph to one hash, and returns the rendered
// lines alongside it so a failure can print a readable diff instead of two
// opaque hashes. One pass, one query set — the lines ARE what was hashed.
//
// Symbols and edges are read in a fully specified order and rendered as text,
// because row ids are allocation-order artefacts: a fresh index and an
// incremental index legitimately assign different ids to the same symbol.
// Comparing ids would fail on a correct implementation. Comparing the
// (path, name, kind) identity and the (from, to, kind, confidence) shape of
// every edge is what "the same graph" actually means.
//
// Both queries scope to the current committed run
// (index_runs.committed = 1): the incremental path updates a run in place
// rather than creating a new one, but a fresh index's very first row is
// stamped 'indexing' before commit, and older, superseded runs may still
// physically exist in the file. Only the committed run is "the graph".
func graphFingerprint(t *testing.T, db *sql.DB) (hash string, lines []string) {
	t.Helper()

	symRows, err := db.Query(`
		SELECT f.path, s.name, s.kind, s.start_line
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.index_run_id = (SELECT id FROM index_runs WHERE committed = 1)
		ORDER BY f.path, s.name, s.kind, s.start_line`)
	if err != nil {
		t.Fatalf("query symbols: %v", err)
	}
	defer func() { _ = symRows.Close() }()
	for symRows.Next() {
		var path, name, kind string
		var line int
		if err := symRows.Scan(&path, &name, &kind, &line); err != nil {
			t.Fatalf("scan symbol: %v", err)
		}
		lines = append(lines, fmt.Sprintf("SYM\t%s\t%s\t%s\t%d", path, name, kind, line))
	}
	if err := symRows.Err(); err != nil {
		t.Fatalf("iterate symbols: %v", err)
	}

	edgeRows, err := db.Query(`
		SELECT fa.path, sa.name, fb.path, sb.name, e.kind, e.confidence
		FROM edges e
		JOIN symbols sa ON sa.id = e.src_id
		JOIN files   fa ON fa.id = sa.file_id
		JOIN symbols sb ON sb.id = e.dst_id
		JOIN files   fb ON fb.id = sb.file_id
		WHERE e.index_run_id = (SELECT id FROM index_runs WHERE committed = 1)
		ORDER BY fa.path, sa.name, fb.path, sb.name, e.kind, e.confidence`)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	defer func() { _ = edgeRows.Close() }()
	for edgeRows.Next() {
		var fromPath, fromName, toPath, toName, kind, conf string
		if err := edgeRows.Scan(&fromPath, &fromName, &toPath, &toName, &kind, &conf); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		lines = append(lines, fmt.Sprintf("EDGE\t%s#%s\t%s#%s\t%s\t%s",
			fromPath, fromName, toPath, toName, kind, conf))
	}
	if err := edgeRows.Err(); err != nil {
		t.Fatalf("iterate edges: %v", err)
	}

	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), lines
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

const parityGoMod = "module parityfixture\n\ngo 1.25\n"

const parityAlphaV1 = `package alpha

func Alpha() int { return Beta() }

func Beta() int { return 1 }
`

const parityAlphaV2 = `package alpha

func Alpha() int { return Beta() + Gamma() }

func Beta() int { return 1 }

func Gamma() int { return 2 }
`

func TestIncrementalIndexEqualsFreshIndex(t *testing.T) {
	// Arrange: two identical projects.
	incDir := t.TempDir()
	freshDir := t.TempDir()
	for _, d := range []string{incDir, freshDir} {
		writeFile(t, d, "go.mod", parityGoMod)
		writeFile(t, d, "alpha/alpha.go", parityAlphaV1)
	}

	// Act 1: index the incremental project at v1, then edit to v2 and reindex.
	incDB := indexFixture(t, incDir)
	writeFile(t, incDir, "alpha/alpha.go", parityAlphaV2)
	incDB = reindexFixture(t, incDir, incDB)

	// Act 2: index the fresh project directly at v2.
	writeFile(t, freshDir, "alpha/alpha.go", parityAlphaV2)
	freshDB := indexFixture(t, freshDir)

	// Assert.
	got, gotLines := graphFingerprint(t, incDB)
	want, wantLines := graphFingerprint(t, freshDB)
	if len(wantLines) == 0 {
		t.Fatal("fresh index produced an empty graph — the fingerprint comparison would be vacuous")
	}
	if got != want {
		t.Errorf("incremental index does not match a fresh index\n incremental:\n  %s\n fresh:\n  %s",
			strings.Join(gotLines, "\n  "), strings.Join(wantLines, "\n  "))
	}
}

func TestIncrementalDeleteEqualsFreshIndexWithoutTheFile(t *testing.T) {
	incDir := t.TempDir()
	freshDir := t.TempDir()
	for _, d := range []string{incDir, freshDir} {
		writeFile(t, d, "go.mod", parityGoMod)
		writeFile(t, d, "alpha/alpha.go", parityAlphaV1)
	}
	// The incremental project also gets a second file, which is then deleted.
	writeFile(t, incDir, "alpha/extra.go", "package alpha\n\nfunc Extra() int { return Beta() }\n")

	incDB := indexFixture(t, incDir)
	if err := os.Remove(filepath.Join(incDir, "alpha", "extra.go")); err != nil {
		t.Fatalf("remove extra.go: %v", err)
	}
	incDB = reindexFixture(t, incDir, incDB)

	freshDB := indexFixture(t, freshDir)

	got, gotLines := graphFingerprint(t, incDB)
	want, wantLines := graphFingerprint(t, freshDB)
	if len(wantLines) == 0 {
		t.Fatal("fresh index produced an empty graph — the fingerprint comparison would be vacuous")
	}
	if got != want {
		t.Errorf("incremental delete does not match a fresh index without that file\n incremental:\n  %s\n fresh:\n  %s",
			strings.Join(gotLines, "\n  "), strings.Join(wantLines, "\n  "))
	}
}

// indexFixture indexes dir from scratch and returns an open handle to its
// registry. It must exercise the SAME code path `repocortex index && repocortex
// resolve` uses — a test-only shortcut would be measuring something no user
// runs.
func indexFixture(t *testing.T, dir string) *sql.DB {
	t.Helper()
	return openIndexedProject(t, dir, false /* incremental */)
}

// reindexFixture runs the INCREMENTAL path over a directory that has already
// been indexed, closing and reopening the handle so nothing is cached across
// the boundary.
func reindexFixture(t *testing.T, dir string, prev *sql.DB) *sql.DB {
	t.Helper()
	if prev != nil {
		if err := prev.Close(); err != nil {
			t.Fatalf("close previous handle: %v", err)
		}
	}
	return openIndexedProject(t, dir, true /* incremental */)
}

// openIndexedProject drives the real repocortex CLI (internal/cli.NewRoot) over
// dir — the same entry point `repocortex init/index/resolve` uses — and returns
// an open handle to the resulting registry.
//
// incremental=false runs init, then index, then resolve, against a directory
// with no prior index: this is a fresh, full build.
//
// incremental=true runs index, then resolve, against a directory that already
// has a committed index (see indexFixture / reindexFixture above). `repocortex
// index` is incremental by default whenever a committed run already exists
// (see internal/cli/index.go: tryIncremental) — it is not a separate command,
// so the only difference from the fresh path is whether `init` runs first.
//
// Each call gets its own REPOCORTEX_CONFIG_HOME, mirroring internal/cli's
// isolate() helper, so the CLI never reads or writes the developer's real
// ~/.config/repocortex.
func openIndexedProject(t *testing.T, dir string, incremental bool) *sql.DB {
	t.Helper()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())

	steps := [][]string{{"index"}, {"resolve"}}
	if !incremental {
		steps = append([][]string{{"init"}}, steps...)
	}
	for _, args := range steps {
		root := cli.NewRoot()
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--project", dir, "--quiet"}, args...))
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}

	dbPath := filepath.Join(dir, cli.IndexDir, cli.IndexFile)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
