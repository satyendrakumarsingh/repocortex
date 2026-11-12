package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// graphFingerprint renders everything the incremental path is supposed to
// maintain, in a stable order, so two indexes can be compared as text.
//
// Row counts alone are too weak: losing one edge and gaining another would
// cancel out. The edge triples are what queries actually answer from.
func graphFingerprint(t *testing.T, root string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, ".repocortex", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var b strings.Builder
	for _, q := range []struct{ label, sql string }{
		{"counts", `
			SELECT 'symbols=' || (SELECT COUNT(*) FROM symbols  WHERE index_run_id = r.id)
			    || ' edges='   || (SELECT COUNT(*) FROM edges    WHERE index_run_id = r.id)
			    || ' refs='    || (SELECT COUNT(*) FROM symbol_references WHERE index_run_id = r.id)
			    || ' bound='   || (SELECT COUNT(*) FROM symbol_references
			                        WHERE index_run_id = r.id AND resolved_symbol_id IS NOT NULL)
			    || ' access='  || (SELECT COUNT(*) FROM table_accesses WHERE index_run_id = r.id)
			    || ' cfg='     || (SELECT COUNT(*) FROM config_usages  WHERE index_run_id = r.id)
			  FROM index_runs r WHERE r.committed = 1`},
		{"edges", `
			SELECT e.kind || '|' || sf.path || ':' || s.name
			              || '->' || df.path || ':' || d.name || '|' || e.confidence
			  FROM edges e
			  JOIN symbols s ON s.id = e.src_id JOIN files sf ON sf.id = s.file_id
			  JOIN symbols d ON d.id = e.dst_id JOIN files df ON df.id = d.file_id
			 WHERE e.index_run_id = (SELECT id FROM index_runs WHERE committed = 1)
			 ORDER BY 1`},
	} {
		rows, err := db.Query(q.sql)
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("[" + q.label + "]\n")
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// TestIncrementalConvergesWithFullIndex is the invariant the incremental path
// exists to preserve: after editing files, the index must describe the tree it
// now has, not a partial memory of the tree it used to have.
//
// Stated as "incremental then edits == full index of the final tree", it is one
// assertion that catches an entire class of omission, and it caught three in a
// row — each a thing reindexOne extracted and then failed to write:
//
//   - analysis facts (endpoints, table accesses, config usages), so `repocortex
//     api` lost a controller's routes on an unrelated edit;
//   - references and imports, so a re-indexed file kept zero references,
//     nothing in it could resolve again, and its intra-file call edges bled
//     away one edited file at a time;
//   - resolutions into an edited file, orphaned by the delete cascade and
//     never revisited, which made `repocortex arch` report "No violations" on a
//     repo that had one.
//
// Every one of those was invisible from the outside: an eroded index and a
// correct index differ only in results nobody knows to expect.
func TestIncrementalConvergesWithFullIndex(t *testing.T) {
	inc := isolate(t)
	writeConvergenceFixture(t, inc)

	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, inc, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	// A working session: edit several files, re-indexing after each, the way
	// `repocortex watch` does. One edit was not enough to expose the erosion.
	edits := []string{
		filepath.Join("src", "api", "handler.go"),
		filepath.Join("src", "db", "store.go"),
		filepath.Join("src", "api", "handler.go"),
		filepath.Join("src", "svc", "service.go"),
	}
	for i, rel := range edits {
		p := filepath.Join(inc, rel)
		body, err := os.ReadFile(p) //nolint:gosec // test fixture under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(body, []byte("\n// edit\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"index"}, {"resolve"}} {
			if out, err := run(t, inc, args...); err != nil {
				t.Fatalf("edit %d, %v: %v\n%s", i, args, err, out)
			}
		}
	}

	// Index the identical final tree from scratch in a second directory.
	full := isolate(t)
	writeConvergenceFixture(t, full)
	for _, rel := range edits {
		p := filepath.Join(full, rel)
		body, err := os.ReadFile(p) //nolint:gosec // test fixture under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(body, []byte("\n// edit\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, full, args...); err != nil {
			t.Fatalf("full %v: %v\n%s", args, err, out)
		}
	}

	gotInc, gotFull := graphFingerprint(t, inc), graphFingerprint(t, full)
	if gotInc != gotFull {
		t.Errorf("incremental index diverged from a full index of the same tree\n"+
			"--- incremental ---\n%s\n--- full ---\n%s", gotInc, gotFull)
	}
}

func writeConvergenceFixture(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"go.mod": "module demo\n\ngo 1.25\n",
		".env":   "DATABASE_URL=postgres://localhost/db\nAPI_TIMEOUT_MS=250\n",
		"src/api/handler.go": `package api

import "demo/src/svc"

var client = 1

func Handle() { svc.Process() }

func Ping() { _ = client }
`,
		"src/svc/service.go": `package svc

import "demo/src/db"

func Process() { db.Save() }
`,
		"src/db/store.go": `package db

import "os"

var jdbc = 1

func Save() {
	_ = os.Getenv("DATABASE_URL")
	query("INSERT INTO transactions (id) VALUES (?)")
}

func Load() { query("SELECT id FROM transactions") }

func query(string) { _ = jdbc }
`,
		"schema/V1.sql": "CREATE TABLE transactions (id VARCHAR(64));\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
