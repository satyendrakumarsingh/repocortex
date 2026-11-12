package db

import "testing"

// TestQualifiedNameKeepsItsTableAndDropsKeywords pins both halves of the
// unqualifiedIdent guard, which fuzzing forced into existence.
//
// isIdentifier runs on the whole token, so "A.FROM" passes it — "A.FROM" is
// not a keyword — and unqualify then discards the prefix and yields "from".
// `repocortex db` reported a table called `from`, which nothing downstream can
// distinguish from a real one.
//
// The guard must not overcorrect: stripping a schema prefix is the entire
// reason unqualify exists, because `public.transactions` and `transactions`
// are one table and splitting them would split the graph.
func TestQualifiedNameKeepsItsTableAndDropsKeywords(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"SELECT id FROM public.transactions WHERE x = ?", "transactions"},
		{"SELECT id FROM mydb.public.orders WHERE x = ?", "orders"},
		{"INSERT INTO app.payments (id, amount) VALUES (?, ?)", "payments"},
		{"UPDATE public.accounts SET balance = ? WHERE id = ?", "accounts"},
	} {
		st, ok := Parse(tc.sql)
		if !ok || len(st.Tables) == 0 {
			t.Errorf("%q: no table parsed — the keyword guard is rejecting real names", tc.sql)
			continue
		}
		if st.Tables[0] != tc.want {
			t.Errorf("%q: table = %q, want %q", tc.sql, st.Tables[0], tc.want)
		}
	}

	if st, ok := Parse("SELECT FROM A.FROM"); ok {
		for _, tbl := range st.Tables {
			if sqlKeywords[upperASCII(tbl)] {
				t.Errorf("keyword %q reported as a table from %q", tbl, "SELECT FROM A.FROM")
			}
		}
	}
}

// TestInvalidUTF8DoesNotPanic pins the crash a hostile or merely corrupt .sql
// file could cause.
//
// strings.ToUpper replaces each invalid UTF-8 byte with a three-byte U+FFFD,
// so the uppercased string is LONGER than its input. insertColumns found an
// offset in it and applied that offset to the original, and a repository
// containing one malformed file crashed `repocortex index` with
// "slice bounds out of range [19:18]".
func TestInvalidUTF8DoesNotPanic(t *testing.T) {
	for _, s := range []string{
		"INSERT \xe4\xe4\xe4\xe4 INTO 0",
		"INSERT INTO \xff\xfe\xfd (a, b) VALUES (1, 2)",
		"SELECT \xc3\x28 FROM \xe2\x82 WHERE x = 1",
		"CREATE TABLE \xf0\x28\x8c\x28 (id INT)",
	} {
		// The assertion is that this returns at all.
		_, _ = Parse(s)
		_ = LooksLikeSQL(s)
	}
}

// TestCSSClassListIsNotATruncate pins false positives found by running this
// analyzer over a real Vue + Tailwind application.
//
// Tailwind's `truncate` utility begins an enormous number of class lists, and
// TRUNCATE is a SQL verb. Two independent weaknesses let that through:
// LooksLikeSQL accepted a bare "TRUNCATE " prefix although its own comment
// said TRUNCATE TABLE, and isIdentifier validated only the FIRST character of
// a name, so `flex-1` looked like a table.
//
// `repocortex db` consequently reported tables named `flex-1`, `font-medium` and
// `block` on a repository that has none of them. An invented table is the
// failure this package's own documentation calls worse than a missed one.
func TestCSSClassListIsNotATruncate(t *testing.T) {
	for _, s := range []string{
		"truncate flex-1",
		"truncate font-medium",
		"truncate block",
		"w-3.5 h-3.5 text-slate-400 shrink-0",
		"text-fs-caption truncate",
	} {
		if LooksLikeSQL(s) {
			st, _ := Parse(s)
			t.Errorf("%q was recognised as SQL (tables %v); it is a CSS class list", s, st.Tables)
		}
	}
}

// TestRealTruncateIsStillRecognised is the other half: tightening the gate must
// not silently delete the statement it was guarding.
func TestRealTruncateIsStillRecognised(t *testing.T) {
	const stmt = "TRUNCATE TABLE transactions"
	if !LooksLikeSQL(stmt) {
		t.Fatalf("%q is no longer recognised as SQL", stmt)
	}
	st, ok := Parse(stmt)
	if !ok || len(st.Tables) == 0 || st.Tables[0] != "transactions" {
		t.Errorf("Parse(%q) = %+v, want table transactions", stmt, st)
	}
	if st.Access != AccessWrite {
		t.Errorf("access = %q, want WRITE", st.Access)
	}
}

// TestIdentifierRejectsNonIdentifierCharacters pins the character rule
// directly, since it is what most of the above rests on.
func TestIdentifierRejectsNonIdentifierCharacters(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"transactions", true},
		{"public.transactions", true}, // qualification arrives unstripped
		{"my_table", true},
		{"t1", true},
		{"flex-1", false},      // the reported case
		{"font-medium", false}, // and its neighbour
		{"a b", false},
		{"", false},
		{"SELECT", false}, // keyword, as before
	} {
		if got := isIdentifier(tc.in); got != tc.want {
			t.Errorf("isIdentifier(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
