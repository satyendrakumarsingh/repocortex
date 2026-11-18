// Package db extracts database facts from source: which tables exist, and
// which code reads or writes them.
//
// # What this is, and what it deliberately is not
//
// It is a SQL RECOGNISER, not a SQL parser. It finds the statement verb and the
// tables it names, and stops. It does not build an expression tree, resolve
// aliases into projections, or understand dialect-specific syntax beyond what
// it takes to answer "does this touch `transactions`, and does it write?".
//
// That boundary is deliberate. A real parser for PostgreSQL, MySQL, Oracle and
// SQLite is a multi-year project whose failure mode is silence — an unsupported
// construct makes a statement vanish from the index, and a table that appears in
// no query looks exactly like a table nothing uses. A recogniser degrades
// instead: it reports what it is sure of, marks what it cannot classify as
// AccessUnknown, and never claims a statement was understood when it was not.
//
// # The distinction the spec requires
//
// Spec §22 requires READ, WRITE, READ_WRITE, DDL and UNKNOWN to be
// distinguishable, and they are the whole product here. "Which code touches
// this table" is a mildly useful question; "which code WRITES this table" is
// the one a person asks before changing a column, and answering it with reads
// mixed in is worse than not answering.
//
// # Dynamic SQL
//
// A query assembled at run time from concatenated fragments cannot be resolved
// statically, and pretending otherwise is the exact overclaim this codebase
// exists to avoid. Fragments that are recognisably partial are reported with
// AccessUnknown and low confidence, so the table still appears in the graph with
// an honest label rather than being silently dropped or confidently mislabelled.
package db

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Access is how a statement uses a table. Spec §22.
type Access string

const (
	// AccessRead is a SELECT, or a table named only in a FROM/JOIN.
	AccessRead Access = "READ"
	// AccessWrite is INSERT, UPDATE, DELETE, MERGE, UPSERT, TRUNCATE.
	AccessWrite Access = "WRITE"
	// AccessReadWrite is a single statement that provably does both — an
	// UPDATE ... FROM, or an INSERT ... SELECT.
	AccessReadWrite Access = "READ_WRITE"
	// AccessDDL is CREATE, ALTER, DROP: statements that change the schema
	// rather than the data. Kept separate because migration impact and query
	// impact are different questions.
	AccessDDL Access = "DDL"
	// AccessUnknown is a statement recognised as SQL whose verb or target this
	// recogniser could not determine — most often a fragment concatenated at
	// run time. Reported rather than dropped: a table nobody can classify is
	// still a table something touches.
	AccessUnknown Access = "UNKNOWN"
)

// Statement is one recognised SQL statement.
type Statement struct {
	// Verb is the leading keyword, uppercased ("SELECT", "INSERT").
	Verb string
	// Access is how the statement uses Tables.
	Access Access
	// Tables are the table names the statement names, lowercased and
	// unqualified — a schema prefix is stripped, because `public.transactions`
	// and `transactions` are the same table and splitting them would split the
	// graph.
	Tables []string
	// Columns are column names where the statement declares them: CREATE TABLE
	// definitions and INSERT column lists. Empty elsewhere — inferring the
	// columns a SELECT * touches would be a guess.
	Columns []string
	// Existing is true for a DDL statement that works on a table it does not
	// create: ALTER, DROP and CREATE INDEX. Such a statement USES the table, so
	// the file it sits in is not where the table is declared. Only a CREATE
	// TABLE (and what an ORM entity declares) is.
	Existing bool
	// Certain is false when the statement looked like a run-time fragment
	// (unbalanced quotes or parens, a trailing conjunction, an interpolation
	// marker). Callers downgrade confidence rather than dropping the fact.
	Certain bool
}

// scanState tracks quoting while walking a statement, so a table name inside a
// string literal or a comment is never mistaken for a real one.
type scanState struct {
	inSingle bool
	inDouble bool
	inBack   bool
	inLine   bool
	inBlock  bool
}

// Normalise strips comments and collapses whitespace, leaving a single-line
// statement that the keyword scan can walk without special cases.
//
// Comments are removed rather than skipped in place because SQL embedded in
// source frequently carries them ("-- see ticket PAY-4412"), and a `--` inside
// a string literal must NOT start a comment. Tracking quote state here once is
// cheaper and far less error-prone than repeating it at every keyword.
func Normalise(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	var st scanState

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		switch {
		case st.inLine:
			if c == '\n' {
				st.inLine = false
				b.WriteByte(' ')
			}
			continue
		case st.inBlock:
			if c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
				st.inBlock = false
				i++
				b.WriteByte(' ')
			}
			continue
		case st.inSingle:
			b.WriteByte(c)
			if c == '\'' {
				// '' is an escaped quote, not a terminator.
				if i+1 < len(sql) && sql[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				st.inSingle = false
			}
			continue
		case st.inDouble:
			b.WriteByte(c)
			if c == '"' {
				st.inDouble = false
			}
			continue
		case st.inBack:
			b.WriteByte(c)
			if c == '`' {
				st.inBack = false
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			st.inLine = true
			i++
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			st.inBlock = true
			i++
		case c == '\'':
			st.inSingle = true
			b.WriteByte(c)
		case c == '"':
			st.inDouble = true
			b.WriteByte(c)
		case c == '`':
			st.inBack = true
			b.WriteByte(c)
		case c == '\n' || c == '\r' || c == '\t':
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// LooksLikeSQL reports whether a string literal is worth analysing.
//
// THIS IS THE FALSE-POSITIVE GATE, and it is the most important function here.
// Source is full of English that collides with SQL keywords, and a false
// positive does not produce a harmless extra row — it INVENTS A TABLE that does
// not exist and puts it in front of an agent as fact. "drop table names into
// the list" is a sentence a developer writes in a comment; a keyword scan turns
// it into a table called `names`.
//
// So a verb prefix is necessary and nowhere near sufficient. Each verb must
// also carry the structure its grammar requires — the thing prose does not
// accidentally have. "SELECT ... FROM" is still not enough, because "select an
// option from the menu" has it; a real query additionally has a projection
// list, a parameter, or a clause.
//
// Erring toward rejection is the correct bias. A missed query costs one edge
// nobody sees; an invented table costs the reader's trust in every edge.
func LooksLikeSQL(s string) bool {
	t := strings.TrimSpace(s)
	if len(t) < 12 || len(t) > maxStatementBytes {
		return false
	}
	up := strings.ToUpper(Normalise(t))

	switch {
	case strings.HasPrefix(up, "SELECT "), strings.HasPrefix(up, "WITH "):
		// A projection list, a bound parameter, or a clause keyword. Prose has
		// none of these; every real query has at least one.
		//
		// ...except the simplest real query there is. `SELECT token FROM
		// card_tokens` has no comma, no star, no parameter and no clause, and
		// was rejected — while the same statement with a WHERE was accepted.
		// That is the wrong gap to have: an unfiltered single-column SELECT is
		// exactly the shape of a bulk export, which is the query a data-access
		// or compliance report most needs to see. Found while testing sensitive
		// data reachability, where the read edge simply never appeared.
		//
		// selectsFromATable closes it without loosening the prose bias — see
		// its comment for the discriminator.
		return strings.Contains(up, " FROM ") &&
			(hasClause(up) || hasProjection(up) || hasParam(t) || selectsFromATable(up))
	case strings.HasPrefix(up, "INSERT "):
		return strings.Contains(up, " INTO ") &&
			(strings.Contains(up, "VALUES") || strings.Contains(up, "SELECT"))
	case strings.HasPrefix(up, "UPDATE "):
		return strings.Contains(up, " SET ") && strings.Contains(up, "=")
	case strings.HasPrefix(up, "DELETE "):
		return strings.Contains(up, " FROM ") && (hasClause(up) || hasParam(t))
	case strings.HasPrefix(up, "CREATE TABLE"):
		return strings.Contains(up, "(")
	case strings.HasPrefix(up, "CREATE INDEX"), strings.HasPrefix(up, "CREATE UNIQUE INDEX"):
		return strings.Contains(up, " ON ")
	case strings.HasPrefix(up, "ALTER TABLE"):
		return hasAnyWord(up, "ADD", "DROP", "ALTER", "RENAME", "MODIFY", "CHANGE")
	case strings.HasPrefix(up, "DROP TABLE"), strings.HasPrefix(up, "TRUNCATE TABLE"):
		// `DROP TABLE t` and `TRUNCATE TABLE t` are short and structurally
		// rigid: identifiers and a handful of modifiers, nothing else. Prose
		// beginning with the same words ("drop table names into the list")
		// carries words that cannot appear, which is what rejects it.
		//
		// TABLE is required, which this comment always claimed and the code did
		// not: the prefix was a bare "TRUNCATE ". Every CSS class list
		// beginning with Tailwind's `truncate` utility therefore parsed as a
		// truncation of whatever class followed it, and `class="truncate block"`
		// put a table called `block` in the graph. The dialects that allow
		// `TRUNCATE t` without TABLE lose an edge here; that is the trade this
		// package states outright — a missed statement costs one edge nobody
		// sees, an invented table costs trust in all of them.
		return isTerseDDL(up)
	case strings.HasPrefix(up, "MERGE "):
		return strings.Contains(up, " USING ")
	}
	return false
}

func hasClause(up string) bool {
	return hasAnyWord(up, "WHERE", "JOIN", "GROUP", "ORDER", "HAVING", "LIMIT",
		"OFFSET", "UNION", "RETURNING")
}

// hasProjection reports a select list that names more than one thing, or `*`.
// "select an option from the menu" has neither.
func hasProjection(up string) bool {
	from := strings.Index(up, " FROM ")
	if from < 0 {
		return false
	}
	list := up[:from]
	return strings.Contains(list, ",") || strings.Contains(list, "*") ||
		strings.Contains(list, "(")
}

// selectsFromATable admits the simplest real query while still rejecting prose.
//
// The discriminator is not the projection, it is the FROM operand. English
// almost always puts an article or a multi-word noun phrase there — "from the
// menu", "from a list", "from our records" — while SQL puts one identifier:
// `card_tokens`, `public.orders`, `"Users"`. So: exactly one word after FROM,
// it is not an article, and both it and the projection look like identifiers.
//
// "select an option from the menu" fails twice over — "the" is an article, and
// the projection "AN OPTION" is two words. "select the report from yesterday"
// fails on the article. `SELECT token FROM card_tokens` passes.
//
// Still biased toward rejection, as the rest of this file is: a missed query
// costs one edge, an invented table costs the reader's trust in every edge.
// Only applied to a SELECT. The caller also reaches here for `WITH`, whose
// projection is not between "SELECT " and the first " FROM " — a CTE puts a
// whole subquery there — so the slice below would be meaningless even where it
// is in range.
//
// It was also a PANIC. `up[len("SELECT "):i]` assumed i > 7, and both
// "WITH a FROM b" (i=6) and "SELECT FROM t" (i=6) put it below that: slice
// bounds out of range [7:6], crashing `repocortex index` on any repository
// containing either string. Found by another agent running a real index, not by
// the unit tests, which only ever fed it well-formed queries.
//
// The prefix is now established before anything is sliced, rather than assumed.
func selectsFromATable(up string) bool {
	const sel = "SELECT "
	if !strings.HasPrefix(up, sel) {
		return false
	}
	i := strings.Index(up, " FROM ")
	if i < len(sel) {
		// No FROM, or a FROM inside the verb itself — neither is a projection.
		return false
	}
	rest := strings.Fields(up[i+len(" FROM "):])
	if len(rest) != 1 {
		return false
	}
	proj := strings.Fields(up[len(sel):i])
	if len(proj) != 1 {
		return false
	}
	return isIdentifierish(rest[0]) && isIdentifierish(proj[0])
}

// isIdentifierish reports whether a token could name a table or a column.
//
// Letters, digits, underscore, and the separators a qualified or quoted name
// uses. Articles are rejected outright: they are the single strongest signal
// that a "FROM" belongs to a sentence rather than a query.
func isIdentifierish(tok string) bool {
	tok = strings.Trim(tok, `";`+"`")
	if tok == "" {
		return false
	}
	switch tok {
	case "THE", "A", "AN", "THIS", "THAT", "THESE", "THOSE", "MY", "OUR", "YOUR", "IT":
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		alnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !alnum && c != '_' && c != '.' {
			return false
		}
	}
	// A bare number is a value, not a name.
	return strings.IndexFunc(tok, func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_'
	}) >= 0
}

// hasParam reports a bound-parameter marker. Checked against the ORIGINAL text
// because `:id` and `$1` are case-sensitive in a way the uppercased form loses.
func hasParam(s string) bool {
	if strings.Contains(s, "?") {
		return true
	}
	for i := 0; i < len(s)-1; i++ {
		if (s[i] == '$' || s[i] == ':' || s[i] == '@') &&
			(isAlphaNum(s[i+1])) {
			return true
		}
	}
	return false
}

func isAlphaNum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isTerseDDL reports whether every word after the leading `DROP TABLE` /
// `TRUNCATE` is something that grammar actually permits.
func isTerseDDL(up string) bool {
	fields := strings.Fields(strings.TrimSuffix(up, ";"))
	allowed := map[string]bool{
		"DROP": true, "TRUNCATE": true, "TABLE": true, "IF": true,
		"EXISTS": true, "CASCADE": true, "RESTRICT": true, "ONLY": true,
	}
	names := 0
	for _, f := range fields {
		f = strings.TrimSuffix(f, ",")
		if allowed[f] {
			continue
		}
		if !isIdentifier(f) {
			return false
		}
		names++
		if names > 4 {
			// A DROP naming five tables is possible but a sentence is far more
			// likely, and this gate exists to favour the safe reading.
			return false
		}
	}
	return names > 0
}

func hasAnyWord(up string, words ...string) bool {
	fields := strings.Fields(up)
	for _, f := range fields {
		for _, w := range words {
			if f == w {
				return true
			}
		}
	}
	return false
}

// maxStatementBytes bounds one statement. A literal larger than this is not a
// query a person wrote; it is a data blob or a generated fixture, and running a
// scan over megabytes of it is work with no answer at the end.
const maxStatementBytes = 64 << 10

// Parse recognises a single SQL statement.
//
// Returns ok=false when the input is not recognisably SQL, so a caller can tell
// "not a query" from "a query I could not classify" — the second is
// AccessUnknown and is reported.
func Parse(sql string) (Statement, bool) {
	norm := Normalise(sql)
	if norm == "" {
		return Statement{}, false
	}
	toks := tokenise(norm)
	if len(toks) == 0 {
		return Statement{}, false
	}

	st := Statement{Certain: isComplete(norm)}
	verb := strings.ToUpper(toks[0])

	// A CTE is named for its body's verb, not for WITH. Skipping to the first
	// statement keyword after the CTE definitions is enough: the CTE bodies
	// themselves are scanned for tables like any other clause.
	if verb == "WITH" {
		if v, idx := verbAfterCTE(toks); v != "" {
			verb = v
			_ = idx
		}
	}
	st.Verb = verb

	switch verb {
	case "SELECT":
		st.Access = AccessRead
		st.Tables = tablesAfter(toks, "FROM", "JOIN")
	case "INSERT":
		st.Access = AccessWrite
		st.Tables = tablesAfter(toks, "INTO")
		st.Columns = insertColumns(norm)
		// INSERT ... SELECT reads its source tables in the same statement.
		if src := tablesAfter(toks, "FROM", "JOIN"); len(src) > 0 {
			st.Access = AccessReadWrite
			st.Tables = append(st.Tables, src...)
		}
	case "UPDATE":
		st.Access = AccessWrite
		st.Tables = tablesAfter(toks, "UPDATE")
		if src := tablesAfter(toks, "FROM", "JOIN"); len(src) > 0 {
			st.Access = AccessReadWrite
			st.Tables = append(st.Tables, src...)
		}
	case "DELETE":
		st.Access = AccessWrite
		st.Tables = tablesAfter(toks, "FROM")
		if src := tablesAfter(toks, "USING", "JOIN"); len(src) > 0 {
			st.Access = AccessReadWrite
			st.Tables = append(st.Tables, src...)
		}
	case "TRUNCATE":
		st.Access = AccessWrite
		st.Tables = tablesAfter(toks, "TRUNCATE", "TABLE")
	case "MERGE":
		st.Access = AccessReadWrite
		st.Tables = tablesAfter(toks, "INTO", "USING")
	case "CREATE", "ALTER", "DROP":
		st.Access = AccessDDL
		// An index's own name is not a table. `CREATE INDEX idx_x ON t (...)`
		// names the table after ON only — scanning after INDEX too captured
		// `idx_x` and put a table in the graph that no database has.
		if hasKeyword(toks, "INDEX") {
			st.Tables = tablesAfter(toks, "ON")
		} else {
			st.Tables = tablesAfter(toks, "TABLE")
		}
		if verb == "CREATE" && hasKeyword(toks, "TABLE") {
			st.Columns = createTableColumns(norm)
		}
		st.Existing = verb != "CREATE" || hasKeyword(toks, "INDEX")
	default:
		st.Access = AccessUnknown
	}

	st.Tables = dedupeLower(st.Tables)
	if len(st.Tables) == 0 && st.Access != AccessUnknown {
		// Recognised the verb, found nothing to attribute it to. Saying UNKNOWN
		// is the honest outcome; reporting a verb with no table would put a
		// fact in the graph that names nothing.
		st.Access = AccessUnknown
	}
	if len(st.Tables) == 0 {
		return st, false
	}
	return st, true
}

// isComplete reports whether the text looks like a whole statement rather than
// a fragment destined for run-time concatenation.
//
// Three cheap signals, each of which really occurs in real code: unbalanced
// quotes or parens (the literal was cut mid-expression), a trailing conjunction
// (`" AND "` appended in a loop), and an interpolation marker. None proves the
// statement is dynamic; together they are a good enough reason to stop claiming
// certainty, which is all Certain is used for.
func isComplete(norm string) bool {
	if strings.Count(norm, "'")%2 != 0 {
		return false
	}
	if strings.Count(norm, "(") != strings.Count(norm, ")") {
		return false
	}
	up := strings.ToUpper(strings.TrimSpace(norm))
	for _, suffix := range []string{" AND", " OR", " WHERE", " ,", ",", " SET", " FROM", " ="} {
		if strings.HasSuffix(up, suffix) {
			return false
		}
	}
	// Interpolation markers from Go, Python, JS, Java and shell-style
	// templating. `?` and `$1` are ordinary bound parameters and are NOT
	// markers of dynamic assembly — those are exactly how a safe query is
	// written, and treating them as suspicious would flag every correct call
	// site in the repository.
	for _, marker := range []string{"%s", "%d", "%v", "{}", "${", "#{", "{0}", "+ \"", "\" +"} {
		if strings.Contains(norm, marker) {
			return false
		}
	}
	return true
}

// tokenise splits a normalised statement into words and significant
// punctuation, keeping quoted identifiers intact.
func tokenise(norm string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(norm); i++ {
		c := norm[i]
		switch c {
		case '\'':
			// Skip string literals whole: their contents are data, and a word
			// inside one is never a table name.
			flush()
			for i++; i < len(norm); i++ {
				if norm[i] == '\'' {
					if i+1 < len(norm) && norm[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case '"', '`', '[':
			// A quoted identifier. The closing delimiter mirrors the opening
			// one, except for brackets, which are the SQL Server spelling.
			end := c
			if c == '[' {
				end = ']'
			}
			joined := strings.HasSuffix(cur.String(), ".")
			if c == '[' && cur.Len() > 0 && !joined {
				// `ARRAY[1, 2]`, `b[i]`: a subscript, not a name. Its
				// content is its own token, as it always was.
				flush()
				j := i + 1
				for ; j < len(norm) && norm[j] != end; j++ {
					cur.WriteByte(norm[j])
				}
				flush()
				i = j
				break
			}
			if !joined {
				flush()
			}
			j := i + 1
			for j < len(norm) && norm[j] != end {
				j++
			}
			if j > i+1 {
				// The token keeps its quotes, so a name with a space in it
				// ("Line Items"), or one that is a keyword ("order"), stays
				// one token and is still known to be a name. A qualifier on
				// either side is joined to it: "shop"."Line Items".
				cur.WriteString(norm[i:min(j+1, len(norm))])
			}
			i = j
			if i+1 >= len(norm) || norm[i+1] != '.' {
				flush()
			}
		case ' ', ';':
			flush()
		case ',', '(', ')':
			flush()
			out = append(out, string(c))
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// tablesAfter collects the identifier following each of the given keywords.
func tablesAfter(toks []string, keywords ...string) []string {
	want := make(map[string]bool, len(keywords))
	for _, k := range keywords {
		want[k] = true
	}
	var out []string
	for i := 0; i < len(toks)-1; i++ {
		if !want[strings.ToUpper(toks[i])] {
			continue
		}
		name := nextIdentifier(toks, i+1)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// nextIdentifier returns the first token at or after i that can be a table
// name, skipping the noise words that sit between a keyword and its target.
func nextIdentifier(toks []string, i int) string {
	for ; i < len(toks); i++ {
		t := toks[i]
		up := strings.ToUpper(t)
		if skipBeforeTable[up] {
			continue
		}
		if t == "(" || t == ")" || t == "," {
			// A subquery, not a table. Its own FROM is picked up separately by
			// the same scan, so nothing is lost by declining here.
			return ""
		}
		if !isIdentifier(t) {
			return ""
		}
		return unqualifiedIdent(t)
	}
	return ""
}

// skipBeforeTable are words that may sit between a keyword and the table it
// targets. Each is a real construct: `DELETE FROM ONLY t` (Postgres),
// `CREATE TABLE IF NOT EXISTS t`, `INSERT INTO TABLE t` (Hive), and the JOIN
// qualifiers that precede `JOIN`.
var skipBeforeTable = map[string]bool{
	"ONLY": true, "TABLE": true, "IF": true, "NOT": true, "EXISTS": true,
	"INTO": true, "LATERAL": true, "UNIQUE": true, "CONCURRENTLY": true,
}

// isIdentifier rejects tokens that cannot be a table name: SQL keywords, and
// anything not starting with a letter or underscore.
func isIdentifier(t string) bool {
	parts, ok := nameParts(t)
	if !ok {
		return false
	}
	// A bare word is a name unless it is a keyword. A quoted one always is:
	// "order" and [key] are legal names, and the quotes are what say so. A
	// qualified name was never a keyword as a whole.
	if len(parts) == 1 && !parts[0].quoted {
		return !sqlKeywords[strings.ToUpper(t)]
	}
	return true
}

// namePart is one segment of a possibly qualified name.
type namePart struct {
	// text is the segment without its quotes.
	text   string
	quoted bool
}

// nameParts splits a table or column name on the dots that are outside quotes.
// It reports false for anything that is not a name: an empty segment, an
// unterminated quote, or a bare segment holding a character no SQL dialect
// allows without quotes.
//
// EVERY character of a bare segment is checked, not just the first. Checking
// only the first accepted `flex-1` and `font-medium` as table names, because a
// Vue template writes `class="truncate flex-1"` and the scan read that literal
// as `TRUNCATE <table>`. `repocortex db` then listed Tailwind utility classes
// among the repository's tables — an invented table, which the doc comment on
// LooksLikeSQL calls the failure that costs the reader's trust in every other
// row. A hyphen is not an identifier character in any SQL dialect without
// quotes, and inside quotes anything but the closing delimiter is.
func nameParts(t string) ([]namePart, bool) {
	var parts []namePart
	i := 0
	for {
		if i >= len(t) {
			return nil, false
		}
		switch c := t[i]; c {
		case '"', '`', '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := strings.IndexByte(t[i+1:], end)
			if j <= 0 {
				return nil, false
			}
			parts = append(parts, namePart{text: t[i+1 : i+1+j], quoted: true})
			i += j + 2
		default:
			start := i
			for i < len(t) && t[i] != '.' {
				r, size := utf8.DecodeRuneInString(t[i:])
				switch {
				case unicode.IsLetter(r), r == '_':
				case i > start && (unicode.IsDigit(r) || r == '$'):
				default:
					return nil, false
				}
				i += size
			}
			if i == start {
				return nil, false
			}
			parts = append(parts, namePart{text: t[start:i]})
		}
		if i == len(t) {
			return parts, true
		}
		if t[i] != '.' {
			return nil, false
		}
		i++
	}
}

// sqlKeywords are words that must never be mistaken for a table name.
//
// Not a complete SQL keyword list, and not meant to be: it holds the words that
// can actually appear where this scan looks for a table. A missing entry shows
// up as a fabricated table in the graph, which is why the list errs toward
// including anything plausible.
var sqlKeywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "JOIN": true, "INNER": true,
	"LEFT": true, "RIGHT": true, "FULL": true, "OUTER": true, "CROSS": true,
	"ON": true, "AS": true, "AND": true, "OR": true, "NOT": true, "IN": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true,
	"DELETE": true, "CREATE": true, "ALTER": true, "DROP": true, "TABLE": true,
	"INDEX": true, "VIEW": true, "GROUP": true, "ORDER": true, "BY": true,
	"HAVING": true, "LIMIT": true, "OFFSET": true, "UNION": true, "ALL": true,
	"DISTINCT": true, "WITH": true, "CASE": true, "WHEN": true, "THEN": true,
	"ELSE": true, "END": true, "NULL": true, "IS": true, "EXISTS": true,
	"USING": true, "RETURNING": true, "CONFLICT": true, "DO": true,
	"NOTHING": true, "MERGE": true, "TRUNCATE": true, "IF": true, "ONLY": true,
	"PRIMARY": true, "KEY": true, "FOREIGN": true, "REFERENCES": true,
	"CONSTRAINT": true, "DEFAULT": true, "UNIQUE": true, "CHECK": true,
}

// clauseWords are the sqlKeywords that open or join a clause, and so cannot be
// a table even when quoted. The rest of sqlKeywords (order, group, table, key)
// are words real schemas do use as table names, and quoting is how they say so.
var clauseWords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "JOIN": true, "INNER": true,
	"LEFT": true, "RIGHT": true, "FULL": true, "OUTER": true, "CROSS": true,
	"ON": true, "AS": true, "AND": true, "OR": true, "NOT": true, "IN": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true,
	"DELETE": true, "CREATE": true, "ALTER": true, "DROP": true, "BY": true,
	"HAVING": true, "UNION": true, "ALL": true, "DISTINCT": true, "WITH": true,
	"WHEN": true, "THEN": true, "ELSE": true, "END": true, "IS": true,
	"EXISTS": true, "USING": true, "RETURNING": true, "DO": true, "IF": true,
	"ONLY": true, "TRUNCATE": true, "MERGE": true,
}

// unqualifiedIdent is unqualify plus the keyword check applied to the RESULT.
//
// isIdentifier runs on the whole token, so a qualified name passes it whenever
// its prefix is not a keyword — and then unqualify throws that prefix away.
// "SELECT FROM A.FROM" reported a table called `from`: the token "A.FROM" is
// not a keyword, but what survives unqualification is. A keyword indexed as a
// table is worse than no result, because `repocortex db` then reports a table
// that does not exist and nothing downstream can tell it apart from a real one.
func unqualifiedIdent(t string) string {
	name := unqualify(t)
	if name == "" {
		return ""
	}
	if parts, ok := nameParts(t); ok && parts[len(parts)-1].quoted {
		// "order" is a name; the quotes say so. "in" is not one: a word that
		// joins or opens a clause is not a table whatever it is wrapped in
		// (`INSERT INTO"IN"` read the keyword `in` as the table).
		if clauseWords[upperASCII(name)] {
			return ""
		}
		return name
	}
	if sqlKeywords[upperASCII(name)] {
		return ""
	}
	return name
}

// unqualify strips a schema or database prefix and the quotes, and lowercases.
//
// `public.transactions` and `transactions` are the same table; keeping both
// would split every question about it in two. The dots that count are the ones
// outside quotes: `"shop"."Line Items"` is `line items`.
func unqualify(name string) string {
	if parts, ok := nameParts(name); ok {
		return strings.ToLower(parts[len(parts)-1].text)
	}
	if i := strings.LastIndex(name, "."); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	return strings.ToLower(strings.Trim(name, "\"`[]"))
}

// leadingName is the first name in s: a quoted one up to its closing
// delimiter, which may hold spaces, else the first word.
func leadingName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if c := s[0]; c == '"' || c == '`' || c == '[' {
		end := c
		if c == '[' {
			end = ']'
		}
		if j := strings.IndexByte(s[1:], end); j > 0 {
			return s[:j+2]
		}
	}
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func hasKeyword(toks []string, kw string) bool {
	for _, t := range toks {
		if strings.EqualFold(t, kw) {
			return true
		}
	}
	return false
}

// verbAfterCTE finds the statement verb following a WITH clause's definitions.
func verbAfterCTE(toks []string) (string, int) {
	depth := 0
	for i, t := range toks {
		switch t {
		case "(":
			depth++
		case ")":
			depth--
		}
		if depth != 0 || i == 0 {
			continue
		}
		switch strings.ToUpper(t) {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE":
			return strings.ToUpper(t), i
		}
	}
	return "", 0
}

// createTableColumns reads the column names from a CREATE TABLE definition.
//
// Only the first identifier of each top-level comma-separated item, and only
// when it is not a constraint keyword — `PRIMARY KEY (id)` is not a column
// named "primary".
func createTableColumns(norm string) []string {
	open := strings.Index(norm, "(")
	if open < 0 {
		return nil
	}
	body := norm[open+1:]
	if close := matchingParen(body); close >= 0 {
		body = body[:close]
	}

	var cols []string
	depth := 0
	item := strings.Builder{}
	flush := func() {
		name := leadingName(item.String())
		item.Reset()
		if name == "" {
			return
		}
		if constraintKeywords[strings.ToUpper(name)] || !isIdentifier(name) {
			return
		}
		if c := unqualifiedIdent(name); c != "" {
			cols = append(cols, c)
		}
	}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
			item.WriteByte(body[i])
		case ')':
			depth--
			item.WriteByte(body[i])
		case ',':
			if depth == 0 {
				flush()
				continue
			}
			item.WriteByte(body[i])
		default:
			item.WriteByte(body[i])
		}
	}
	flush()
	return cols
}

var constraintKeywords = map[string]bool{
	"PRIMARY": true, "FOREIGN": true, "UNIQUE": true, "CHECK": true,
	"CONSTRAINT": true, "INDEX": true, "KEY": true, "EXCLUDE": true,
}

// insertColumns reads the explicit column list of an INSERT, which is the one
// place a write statement names its columns unambiguously.
// upperASCII uppercases a-z and leaves every other byte alone, so the result
// has exactly the same length and byte offsets as its input.
//
// strings.ToUpper does NOT guarantee that. On invalid UTF-8 it replaces each
// bad byte with U+FFFD, which is three bytes, so the uppercased string grows.
// Any offset found in it and then applied to the original is then wrong, and
// far enough past the end to panic: "INSERT \xe4\xe4\xe4\xe4 INTO 0" produced
// slice bounds out of range [19:18] in insertColumns, which means a repository
// containing one malformed .sql file crashed `repocortex index`.
//
// SQL keywords are ASCII, so restricting the fold to ASCII loses nothing —
// this is the primitive keyword scanning wanted in the first place.
func upperASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

func insertColumns(norm string) []string {
	// upperASCII, not strings.ToUpper: offsets found here are applied to norm.
	up := upperASCII(norm)
	into := strings.Index(up, " INTO ")
	if into < 0 {
		return nil
	}
	open := strings.Index(norm[into:], "(")
	if open < 0 {
		return nil
	}
	open += into
	// The parenthesis must belong to the table, not to a VALUES clause.
	if v := strings.Index(up, " VALUES"); v >= 0 && v < open {
		return nil
	}
	body := norm[open+1:]
	if close := matchingParen(body); close >= 0 {
		body = body[:close]
	}
	var cols []string
	for _, part := range strings.Split(body, ",") {
		name := leadingName(part)
		if name == "" || !isIdentifier(name) {
			continue
		}
		if c := unqualifiedIdent(name); c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// matchingParen returns the index of the ')' closing an already-opened '('.
func matchingParen(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

func dedupeLower(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
