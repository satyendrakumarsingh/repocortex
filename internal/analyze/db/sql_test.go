package db

import (
	"strings"
	"testing"
)

// The recogniser's job is to be right about what it claims and honest about
// what it cannot classify. These tests are organised around those two things:
// the statements it must get right, and the inputs it must refuse.

func TestParseClassifiesAccessMode(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		verb   string
		access Access
		tables []string
	}{
		{
			name:   "select",
			sql:    "SELECT id, merchant_id, status FROM transactions WHERE id = ?",
			verb:   "SELECT",
			access: AccessRead,
			tables: []string{"transactions"},
		},
		{
			name:   "select with join reads both",
			sql:    "SELECT t.id FROM transactions t JOIN transaction_events e ON e.transaction_id = t.id",
			verb:   "SELECT",
			access: AccessRead,
			tables: []string{"transactions", "transaction_events"},
		},
		{
			name:   "insert",
			sql:    "INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)",
			verb:   "INSERT",
			access: AccessWrite,
			tables: []string{"transaction_events"},
		},
		{
			name:   "update",
			sql:    "UPDATE transactions SET status = ?, updated_at = now() WHERE id = ?",
			verb:   "UPDATE",
			access: AccessWrite,
			tables: []string{"transactions"},
		},
		{
			name:   "delete",
			sql:    "DELETE FROM transactions WHERE created_at < ?",
			verb:   "DELETE",
			access: AccessWrite,
			tables: []string{"transactions"},
		},
		{
			// The distinction the spec asks for: one statement that provably
			// does both. Reporting this as a plain WRITE would hide that
			// `audit_log` is read, and as a plain READ would hide the write.
			name:   "insert select is read_write",
			sql:    "INSERT INTO archive (id, status) SELECT id, status FROM transactions WHERE created_at < ?",
			verb:   "INSERT",
			access: AccessReadWrite,
			tables: []string{"archive", "transactions"},
		},
		{
			name:   "update from is read_write",
			sql:    "UPDATE transactions SET status = s.status FROM staging s WHERE s.id = transactions.id",
			verb:   "UPDATE",
			access: AccessReadWrite,
			tables: []string{"transactions", "staging"},
		},
		{
			name:   "create table is ddl",
			sql:    "CREATE TABLE transactions (id VARCHAR(64) PRIMARY KEY, status VARCHAR(32) NOT NULL)",
			verb:   "CREATE",
			access: AccessDDL,
			tables: []string{"transactions"},
		},
		{
			name:   "alter table is ddl",
			sql:    "ALTER TABLE transactions ADD COLUMN refunded_at TIMESTAMP",
			verb:   "ALTER",
			access: AccessDDL,
			tables: []string{"transactions"},
		},
		{
			name:   "create index names its table",
			sql:    "CREATE INDEX idx_transactions_merchant ON transactions (merchant_id)",
			verb:   "CREATE",
			access: AccessDDL,
			tables: []string{"transactions"},
		},
		{
			name:   "truncate is a write",
			sql:    "TRUNCATE TABLE transaction_events",
			verb:   "TRUNCATE",
			access: AccessWrite,
			tables: []string{"transaction_events"},
		},
		{
			name:   "schema prefix is stripped",
			sql:    "SELECT * FROM public.transactions WHERE id = ?",
			verb:   "SELECT",
			access: AccessRead,
			tables: []string{"transactions"},
		},
		{
			name:   "quoted identifiers",
			sql:    `SELECT * FROM "transactions" WHERE "id" = ?`,
			verb:   "SELECT",
			access: AccessRead,
			tables: []string{"transactions"},
		},
		{
			name:   "if not exists is skipped",
			sql:    "CREATE TABLE IF NOT EXISTS transactions (id INT)",
			verb:   "CREATE",
			access: AccessDDL,
			tables: []string{"transactions"},
		},
		{
			name:   "cte takes its body's verb",
			sql:    "WITH recent AS (SELECT id FROM transactions) SELECT * FROM recent JOIN transaction_events e ON e.transaction_id = recent.id",
			verb:   "SELECT",
			access: AccessRead,
			tables: []string{"transactions", "recent", "transaction_events"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, ok := Parse(c.sql)
			if !ok {
				t.Fatalf("not recognised as SQL: %s", c.sql)
			}
			if st.Verb != c.verb {
				t.Errorf("verb = %q, want %q", st.Verb, c.verb)
			}
			if st.Access != c.access {
				t.Errorf("access = %q, want %q", st.Access, c.access)
			}
			for _, want := range c.tables {
				if !contains(st.Tables, want) {
					t.Errorf("tables = %v, missing %q", st.Tables, want)
				}
			}
			if len(st.Tables) != len(c.tables) {
				t.Errorf("tables = %v, want exactly %v", st.Tables, c.tables)
			}
		})
	}
}

// TestParseNeverInventsTables is the test that matters most.
//
// A fabricated table is worse than a missing one: it appears in the graph as a
// fact, an agent reads it as real, and nothing downstream can tell it was
// guessed. Every input here has previously been the kind of thing a loose
// keyword scan turns into a table.
func TestParseNeverInventsTables(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM (SELECT 1) sub",                    // subquery, not a table
		"SELECT COUNT(*) FROM",                            // truncated
		"UPDATE SET status = 1",                           // no target
		"DELETE FROM WHERE id = 1",                        // keyword where a table should be
		"INSERT INTO VALUES (1,2)",                        // ditto
		"SELECT 'INSERT INTO fake_table VALUES (1)' AS s", // SQL inside a string literal
	} {
		st, ok := Parse(sql)
		if !ok {
			continue // refusing outright is a fine outcome
		}
		for _, tbl := range st.Tables {
			switch tbl {
			case "where", "set", "values", "sub", "fake_table", "select", "from":
				t.Errorf("%q produced a fabricated table %q", sql, tbl)
			}
		}
	}
}

// TestLooksLikeSQLRejectsProse guards the gate in front of everything else.
//
// Source is full of English that collides with SQL keywords. A false positive
// does not add a harmless row — it invents a table and presents it as fact.
func TestLooksLikeSQLRejectsProse(t *testing.T) {
	reject := []string{
		"update failed for transaction",
		"could not delete the record",
		"select an option from the menu",
		"insert coin to continue",
		"drop table names into the list",
		"SELECT",
		"",
		"a short one",
		"deleteUserById",
	}
	for _, s := range reject {
		if LooksLikeSQL(s) {
			t.Errorf("prose accepted as SQL: %q", s)
		}
	}

	accept := []string{
		"SELECT id, status FROM transactions WHERE id = ?",
		"insert into transaction_events (id) values (?)",
		"UPDATE transactions SET status = ? WHERE id = ?",
		"CREATE TABLE transactions (id INT)",
	}
	for _, s := range accept {
		if !LooksLikeSQL(s) {
			t.Errorf("real SQL rejected: %q", s)
		}
	}
}

// TestCertaintyMarksRuntimeFragments. A query assembled at run time cannot be
// resolved statically. It is still reported — the table is real — but the
// caller downgrades confidence, and that only works if Certain is honest.
func TestCertaintyMarksRuntimeFragments(t *testing.T) {
	certain := []string{
		"SELECT id FROM transactions WHERE id = ?",
		"SELECT id FROM transactions WHERE id = $1",  // bound parameter, not interpolation
		"SELECT id FROM transactions WHERE id = :id", // named bind
	}
	for _, s := range certain {
		st, ok := Parse(s)
		if !ok {
			t.Fatalf("not recognised: %s", s)
		}
		if !st.Certain {
			t.Errorf("a parameterised query is not a fragment: %q", s)
		}
	}

	fragments := []string{
		"SELECT id FROM transactions WHERE status = '%s'",
		"SELECT id FROM transactions WHERE id = ${id}",
		"SELECT id FROM transactions WHERE ",
		"SELECT id FROM transactions WHERE a = 1 AND",
	}
	for _, s := range fragments {
		st, ok := Parse(s)
		if !ok {
			continue
		}
		if st.Certain {
			t.Errorf("run-time fragment reported as certain: %q", s)
		}
	}
}

func TestCreateTableColumnsSkipConstraints(t *testing.T) {
	st, ok := Parse(`CREATE TABLE transactions (
		id          VARCHAR(64) PRIMARY KEY,
		merchant_id VARCHAR(64) NOT NULL,
		amount      BIGINT      NOT NULL,
		created_at  TIMESTAMP   NOT NULL DEFAULT now(),
		PRIMARY KEY (id),
		FOREIGN KEY (merchant_id) REFERENCES merchants(id)
	)`)
	if !ok {
		t.Fatal("CREATE TABLE not recognised")
	}
	want := []string{"id", "merchant_id", "amount", "created_at"}
	for _, w := range want {
		if !contains(st.Columns, w) {
			t.Errorf("columns = %v, missing %q", st.Columns, w)
		}
	}
	// `PRIMARY KEY (id)` is a constraint, not a column named "primary".
	for _, bad := range []string{"primary", "foreign", "key"} {
		if contains(st.Columns, bad) {
			t.Errorf("constraint keyword %q captured as a column: %v", bad, st.Columns)
		}
	}
}

func TestInsertColumnList(t *testing.T) {
	st, ok := Parse("INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)")
	if !ok {
		t.Fatal("not recognised")
	}
	for _, w := range []string{"transaction_id", "event_type"} {
		if !contains(st.Columns, w) {
			t.Errorf("columns = %v, missing %q", st.Columns, w)
		}
	}

	// No column list: VALUES must not be mistaken for one.
	st, _ = Parse("INSERT INTO transaction_events VALUES (1, 'X')")
	if len(st.Columns) != 0 {
		t.Errorf("columns = %v, want none when the statement declares no list", st.Columns)
	}
}

// TestNormaliseHandlesCommentsAndQuotes. A `--` inside a string literal does not
// start a comment; getting that wrong truncates the statement and loses its
// tables silently.
func TestNormaliseHandlesCommentsAndQuotes(t *testing.T) {
	in := `SELECT id -- the primary key
	       FROM transactions /* main table */
	       WHERE note = 'a -- b' AND x = 1`
	got := Normalise(in)
	if strings.Contains(got, "the primary key") || strings.Contains(got, "main table") {
		t.Errorf("comment survived normalisation: %q", got)
	}
	if !strings.Contains(got, "'a -- b'") {
		t.Errorf("a comment marker inside a string literal was stripped: %q", got)
	}

	st, ok := Parse(in)
	if !ok {
		t.Fatalf("statement lost to comment handling: %q", got)
	}
	if !contains(st.Tables, "transactions") {
		t.Errorf("tables = %v", st.Tables)
	}
}

// TestParseIsSafeOnHostileInput. Source arrives from repositories RepoCortex did
// not write, so every input here must terminate and none may panic.
func TestParseIsSafeOnHostileInput(t *testing.T) {
	for _, s := range []string{
		"SELECT " + strings.Repeat("(", 5000),
		"SELECT " + strings.Repeat("'", 5000),
		strings.Repeat("SELECT * FROM a JOIN ", 2000),
		"CREATE TABLE t (" + strings.Repeat("c INT,", 5000) + ")",
		"\x00\x01\x02 SELECT * FROM t",
		strings.Repeat("-", 10000),
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %.40q: %v", s, r)
				}
			}()
			_, _ = Parse(s)
			_ = LooksLikeSQL(s)
		}()
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestTheSimplestRealQueryIsNotMistakenForProse closes a recall gap found while
// testing sensitive-data reachability.
//
// `SELECT token FROM card_tokens` has no comma, no star, no bound parameter and
// no WHERE, so the prose guard rejected it — while the identical statement with
// a WHERE was accepted. An unfiltered single-column SELECT is precisely the
// shape of a bulk export, which is the query a compliance report most needs to
// see, so this was the worst possible thing for the guard to drop.
//
// The rejection cases matter as much as the acceptance cases: the guard exists
// because "select an option from the menu" is a sentence, and widening it until
// prose gets in would cost more than the missing edge did.
func TestTheSimplestRealQueryIsNotMistakenForProse(t *testing.T) {
	queries := []string{
		"SELECT token FROM card_tokens",
		"SELECT id FROM users",
		"SELECT email FROM public.customers",
		`SELECT name FROM "Accounts"`,
	}
	for _, q := range queries {
		if !LooksLikeSQL(q) {
			t.Errorf("real query rejected as prose: %q", q)
			continue
		}
		st, ok := Parse(q)
		if !ok {
			t.Errorf("%q passed the guard but did not parse", q)
			continue
		}
		if st.Access != AccessRead {
			t.Errorf("%q parsed as %v, want a read", q, st.Access)
		}
		if len(st.Tables) == 0 {
			t.Errorf("%q parsed with no table", q)
		}
	}

	prose := []string{
		"select an option from the menu",
		"Select the report from yesterday",
		"select a value from this list",
		"please select your plan from our catalogue",
		"select 1 from 2",
	}
	for _, p := range prose {
		if LooksLikeSQL(p) {
			t.Errorf("prose accepted as SQL: %q — an invented table costs more than a missed edge", p)
		}
	}
}

// TestLooksLikeSQLNeverPanicsOnShortStatements is a regression test for a crash
// I shipped into `repocortex index`.
//
// selectsFromATable sliced up[len("SELECT "):i] on the assumption that the first
// " FROM " came after the verb. "WITH a FROM b" and "SELECT FROM t" both put it
// at index 6, so the slice was [7:6] — out of range, panic, and the whole index
// run died on any repository containing either string in a string literal.
//
// The unit tests missed it because every case they fed in was a well-formed
// query. It was found by running a real index. So this test feeds the shapes a
// real repository actually contains: truncated SQL, prose that starts with a
// verb, and fragments of a concatenated query.
func TestLooksLikeSQLNeverPanicsOnShortStatements(t *testing.T) {
	inputs := []string{
		// The reported crashers: " FROM " lands at an offset below 7, so the
		// low bound of up[7:i] exceeded its high bound.
		"A FROM B", " FROM x", "SEL FROM t", "SELECT FROM t", "WITH a FROM b",

		"", " ", "SELECT", "SELECT ", "SELECT  FROM  t",
		"WITH FROM", "WITH  FROM  ", "FROM", " FROM ",
		"SELECT * FROM", "SELECT a FROM", "select from from from",
		"INSERT", "UPDATE", "DELETE FROM", "CREATE TABLE", "ALTER TABLE",
		"SELECT\n\nFROM\n\nt", "\t SELECT \t FROM \t t \t",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("LooksLikeSQL(%q) panicked: %v", in, r)
				}
			}()
			if LooksLikeSQL(in) {
				// If it claims to be SQL it must also survive parsing, since
				// that is what the caller does next.
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("Parse(%q) panicked after LooksLikeSQL said yes: %v", in, r)
					}
				}()
				_, _ = Parse(in)
			}
		}()
	}
}
