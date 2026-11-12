package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQueryRunsASelectAgainstTheCommittedRun(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "query",
		"SELECT name FROM symbols WHERE index_run_id = :run AND name IN ('Settle','Refund','Save') ORDER BY name")
	if err != nil {
		t.Fatalf("query: %v\n%s", err, out)
	}
	for _, want := range []string{"NAME", "Refund", "Save", "Settle", "3 row(s)."} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestQueryJSONAndLimit(t *testing.T) {
	root := queryFixture(t)
	raw, err := run(t, root, "--json", "query", "SELECT name FROM symbols ORDER BY name", "--limit", "2")
	if err != nil {
		t.Fatalf("query --json: %v\n%s", err, raw)
	}
	var got struct {
		Columns   []string         `json:"columns"`
		Rows      []map[string]any `json:"rows"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if len(got.Rows) != 2 || !got.Truncated || len(got.Columns) != 1 || got.Columns[0] != "name" {
		t.Errorf("want 2 rows, truncated, one column 'name'; got %+v", got)
	}
}

func TestQuerySchemaListsTheTables(t *testing.T) {
	root := queryFixture(t)
	out, err := run(t, root, "query", "--schema")
	if err != nil {
		t.Fatalf("query --schema: %v\n%s", err, out)
	}
	for _, want := range []string{"symbols", "edges", "files", "index_run_id"} {
		if !strings.Contains(out, want) {
			t.Errorf("schema is missing %q:\n%s", want, out)
		}
	}
}

// The index must come out of every query exactly as it went in.
func TestQueryRefusesAnythingButOneSelect(t *testing.T) {
	root := queryFixture(t)
	for _, bad := range []string{
		"DELETE FROM symbols",
		"UPDATE symbols SET name = 'x'",
		"DROP TABLE edges",
		"INSERT INTO files (path) VALUES ('x')",
		"PRAGMA journal_mode = DELETE",
		"ATTACH DATABASE '/tmp/other.db' AS other",
		"SELECT 1; DELETE FROM symbols",
		"VACUUM",
		"",
	} {
		if out, err := run(t, root, "query", bad); err == nil {
			t.Errorf("%q was accepted:\n%s", bad, out)
		}
	}
	// A write smuggled inside a WITH is stopped by the read-only connection,
	// not by the keyword check.
	if out, err := run(t, root, "query", "WITH x AS (SELECT 1) DELETE FROM symbols"); err == nil {
		t.Errorf("a DELETE inside WITH was accepted:\n%s", out)
	}
	// And the rows are all still there.
	out, err := run(t, root, "query", "SELECT COUNT(*) AS n FROM symbols WHERE name = 'Settle'")
	if err != nil || !strings.Contains(out, "1") {
		t.Errorf("the index changed, or the count failed: %v\n%s", err, out)
	}
}

func TestCheckSelect(t *testing.T) {
	for in, ok := range map[string]bool{
		"SELECT 1":                             true,
		"  select name from symbols ; ":        true, // one trailing semicolon is fine
		"WITH t AS (SELECT 1) SELECT * FROM t": true,
		"SELECT 'a;b' AS s":                    true, // a semicolon inside a string is not a separator
		"SELECT 1; SELECT 2":                   false,
		"EXPLAIN SELECT 1":                     false,
		"pragma table_info(symbols)":           false,
		";":                                    false,
	} {
		if _, err := checkSelect(in); (err == nil) != ok {
			t.Errorf("checkSelect(%q): accepted = %v, want %v (%v)", in, err == nil, ok, err)
		}
	}
}

func TestQueryRejectsBadFlags(t *testing.T) {
	root := queryFixture(t)
	if _, err := run(t, root, "query", "SELECT 1", "--limit", "0"); err == nil {
		t.Error("--limit 0 must be rejected")
	}
	if _, err := run(t, root, "query", "SELECT 1", "--schema"); err == nil {
		t.Error("a query together with --schema must be rejected")
	}
	if _, err := run(t, root, "query"); err == nil {
		t.Error("no query and no --schema must be rejected")
	}
}
