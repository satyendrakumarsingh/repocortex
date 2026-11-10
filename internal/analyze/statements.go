package analyze

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/db"
)

// Finding SQL in a file means two different jobs.
//
// A .sql file IS SQL: split it on statement boundaries and analyse each one. A
// source file CONTAINS SQL, inside string literals, mixed with every other
// string the program uses — log messages, error text, format templates — and
// picking the queries out of that is where a naive scan invents tables.
//
// Both paths end at db.Parse. What differs is how a candidate is found and how
// hard it has to work to be believed.

// maxScanBytes bounds how much of one file is scanned for SQL. A file larger
// than this is generated or a data dump; the queries a person wrote are not in
// its third megabyte, and scanning it would cost more than the answer is worth.
const maxScanBytes = 4 << 20

// statementsIn returns every SQL statement found in a file, with the line it
// starts on.
func statementsIn(path string, src []byte) []located {
	if len(src) == 0 || len(src) > maxScanBytes {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(path), ".sql") {
		return sqlFileStatements(string(src))
	}

	// A third job, and the one that covers most real data access: a file may
	// reach the database through an ORM and contain no SQL text at all. GORM,
	// JPA and SQLAlchemy accesses are recovered by db.ORMStatements and staged
	// through exactly the same path as embedded SQL — same attribution to the
	// enclosing symbol, same table binding in the second pass, same unbound
	// record when the table is declared somewhere this repository does not own.
	//
	// Raw SQL first: a file can legitimately do both, and a query written by
	// hand states its table directly, where an ORM access usually states it by
	// convention.
	out := embeddedStatements(string(src))
	for _, o := range db.ORMStatements(path, src) {
		out = append(out, located{Statement: o.Statement, Line: o.Line})
	}
	return out
}

// sqlFileStatements splits a .sql file on top-level semicolons.
//
// Quote and comment state is tracked so a semicolon inside a string literal or
// a comment does not split a statement in half — which would leave two
// fragments, neither of which parses, and silently lose the table.
func sqlFileStatements(src string) []located {
	var out []located
	var cur strings.Builder
	line, startLine := 1, 1

	var inSingle, inDouble, inLine, inBlock bool
	flush := func() {
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		if text == "" {
			return
		}
		if st, ok := db.Parse(text); ok {
			out = append(out, located{Statement: st, Line: startLine})
		}
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			line++
		}

		switch {
		case inLine:
			cur.WriteByte(c)
			if c == '\n' {
				inLine = false
			}
			continue
		case inBlock:
			cur.WriteByte(c)
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				cur.WriteByte('/')
				i++
				inBlock = false
			}
			continue
		case inSingle:
			cur.WriteByte(c)
			if c == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
					continue
				}
				inSingle = false
			}
			continue
		case inDouble:
			cur.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			inLine = true
			cur.WriteString("--")
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			cur.WriteString("/*")
			i++
		case c == '\'':
			inSingle = true
			cur.WriteByte(c)
		case c == '"':
			inDouble = true
			cur.WriteByte(c)
		case c == ';':
			flush()
			startLine = line
		default:
			if cur.Len() == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
				// Leading whitespace: keep startLine tracking the first real
				// character, so a statement is reported at the line a reader
				// would point to rather than at the blank line before it.
				startLine = line
				continue
			}
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// embeddedStatements finds SQL inside string literals in source.
//
// Every literal is a candidate and almost none are queries, so db.LooksLikeSQL
// runs first as the cheap, strict gate; only what survives is parsed. That
// ordering matters on a real repository: a 27k-line tree holds tens of
// thousands of literals and a handful of queries.
//
// Multi-line literals are handled because that is how readable SQL is written
// in every language that has them — Go's backticks, Java 15+ text blocks,
// Python's triple quotes.
func embeddedStatements(src string) []located {
	var out []located
	seen := make(map[string]struct{})

	for _, lit := range stringLiterals(src) {
		text := strings.TrimSpace(lit.text)
		if !db.LooksLikeSQL(text) {
			continue
		}
		st, ok := db.Parse(text)
		if !ok {
			continue
		}
		// One query written once but concatenated across branches can appear
		// twice with identical text; counting it twice would inflate a usage
		// report without adding a fact.
		key := st.Verb + "|" + strings.Join(st.Tables, ",")
		if _, dup := seen[key+"|"+itoa(lit.line)]; dup {
			continue
		}
		seen[key+"|"+itoa(lit.line)] = struct{}{}
		out = append(out, located{Statement: st, Line: lit.line})
	}
	return out
}

type literal struct {
	text string
	line int
}

// stringLiterals extracts quoted strings from source, tracking line numbers.
//
// Language-agnostic on purpose. A per-language tokeniser would be more precise,
// but the imprecision here is bounded and harmless: at worst a comment
// containing a quoted SQL statement is treated as a literal, and a comment
// documenting a query is describing a query the file really does run. The
// alternative — six tokenisers maintained against six grammars — buys accuracy
// nobody would notice.
func stringLiterals(src string) []literal {
	var out []literal
	line := 1

	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			line++
			continue
		}
		if c != '"' && c != '\'' && c != '`' {
			continue
		}

		// Java text blocks and Python triple quotes: three identical quotes
		// open a literal that runs until the matching three.
		if i+2 < len(src) && src[i+1] == c && src[i+2] == c {
			start := i + 3
			end := indexTriple(src, start, c)
			if end < 0 {
				return out
			}
			out = append(out, literal{text: src[start:end], line: line})
			line += strings.Count(src[i:end], "\n")
			i = end + 2
			continue
		}

		quote := c
		start := i + 1
		startLine := line
		j := start
		for ; j < len(src); j++ {
			if src[j] == '\\' {
				j++ // skip the escaped character, whatever it is
				continue
			}
			if src[j] == '\n' {
				line++
				// A single- or double-quoted literal does not span lines in
				// any language here EXCEPT via a backtick, so a newline inside
				// one means the opening quote was an apostrophe in prose.
				if quote != '`' {
					break
				}
				continue
			}
			if src[j] == quote {
				break
			}
		}
		if j < len(src) && src[j] == quote {
			out = append(out, literal{text: src[start:j], line: startLine})
		}
		i = j
	}
	return out
}

func indexTriple(src string, from int, q byte) int {
	for i := from; i+2 < len(src); i++ {
		if src[i] == q && src[i+1] == q && src[i+2] == q {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
