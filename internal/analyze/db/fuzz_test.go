package db

import (
	"strings"
	"testing"
)

// FuzzParse holds the recogniser to two properties on arbitrary input.
//
// It runs over string literals taken from repositories RepoCortex did not write,
// so "must not panic" is a security property rather than a tidiness one. The
// second property is the one that protects the graph: whatever comes back must
// never name a SQL keyword as a table, because that is what a fabricated table
// looks like.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"SELECT id FROM transactions WHERE id = ?",
		"INSERT INTO t (a,b) VALUES (?,?)",
		"UPDATE t SET a = ? WHERE b = ?",
		"CREATE TABLE t (id INT, PRIMARY KEY (id))",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"SELECT '--' FROM t",
		"",
		"((((",
		"'''''",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		st, ok := Parse(s)
		if !ok {
			return
		}
		for _, tbl := range st.Tables {
			if tbl == "" {
				t.Fatalf("empty table name from %q", s)
			}
			if sqlKeywords[strings.ToUpper(tbl)] {
				t.Fatalf("keyword %q reported as a table from %q", tbl, s)
			}
		}
		for _, col := range st.Columns {
			if col == "" {
				t.Fatalf("empty column name from %q", s)
			}
		}
		// A recognised statement must always carry a classification.
		if st.Access == "" {
			t.Fatalf("recognised %q with no access mode", s)
		}
	})
}
