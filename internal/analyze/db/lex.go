package db

// A tiny token scanner, shared by the three ORM extractors.
//
// # Why a lexer and not a pattern match
//
// The SQL side of this package has no regexes because it splits statements
// structurally, and the ORM side is held to the same standard for the same
// reason. `Delete` inside a comment, `"@Entity"` inside a string, and
// `__tablename__` in a docstring are all things a pattern match reports and a
// scanner does not. Comments and string bodies are the two places source most
// reliably contains text that looks exactly like the thing being searched for,
// so the scanner is the cheapest way to stop looking at them.
//
// It is NOT a parser for Go, Java or Python. It knows comments, string
// literals, identifiers and punctuation, and that is the whole of it. What the
// extractors build on top is a call-chain walk (see chainsIn), which is the one
// structure all three ecosystems' APIs actually share: a receiver, a run of
// selectors, and calls with argument lists.

type tokKind uint8

const (
	tokIdent tokKind = iota
	tokString
	tokPunct
	tokNumber
)

// token is one lexical item.
type token struct {
	kind tokKind
	// text is the identifier, the single punctuation character, or a string
	// literal's CONTENTS with its delimiters removed. Escapes are left as
	// written: a table name containing a backslash escape does not exist, and
	// unescaping would be work with no consumer.
	text string
	// line is 1-based, and is the line the token STARTS on. Attribution uses
	// it, so a multi-line raw string reports where it opened, which is where a
	// reader would point.
	line int
	// col is the 0-based byte offset from the start of the line. Only Python
	// needs it — its block structure is indentation — but carrying it for every
	// language costs nothing and keeps one token type.
	col int
}

// maxORMBytes bounds a file the ORM extractors will read, on the same argument
// as maxScanBytes on the SQL side: past this the file is generated, and the
// entity declarations a person wrote are not in its second megabyte.
const maxORMBytes = 2 << 20

// maxORMTokens bounds one file's token stream. Reached only by generated or
// adversarial input; the cap is what keeps a malformed file from turning
// indexing into an allocation storm.
const maxORMTokens = 400 << 10

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// lexBraced scans Go and Java. The two share every lexical feature that matters
// here — `//` and `/* */` comments, double-quoted strings with backslash
// escapes — and differ only in their multi-line string spelling, so both are
// accepted: Go's backticks and Java's `"""` text blocks.
func lexBraced(src string) []token {
	toks := make([]token, 0, len(src)/8+8)
	line, lineStart := 1, 0

	add := func(k tokKind, text string, at int) {
		if len(toks) >= maxORMTokens {
			return
		}
		toks = append(toks, token{kind: k, text: text, line: line, col: at - lineStart})
	}

	for i := 0; i < len(src); i++ {
		if len(toks) >= maxORMTokens {
			break
		}
		c := src[i]
		switch {
		case c == '\n':
			line++
			lineStart = i + 1
		case c == ' ' || c == '\t' || c == '\r':
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i++; i < len(src) && src[i] != '\n'; i++ {
			}
			i-- // let the loop see the newline and count it
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := i + 2
			for ; j+1 < len(src); j++ {
				if src[j] == '\n' {
					line++
					lineStart = j + 1
				}
				if src[j] == '*' && src[j+1] == '/' {
					j++
					break
				}
			}
			i = j
		case c == '"' && i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"':
			// Java text block.
			start := i + 3
			end := indexTripleQuote(src, start, '"')
			if end < 0 {
				return toks
			}
			add(tokString, src[start:end], i)
			line, lineStart = advance(src, i, end, line, lineStart)
			i = end + 2
		case c == '"' || c == '\'':
			quote := c
			start := i + 1
			j := start
			for ; j < len(src); j++ {
				if src[j] == '\\' {
					j++
					continue
				}
				if src[j] == '\n' || src[j] == quote {
					break
				}
			}
			if j < len(src) && src[j] == quote {
				add(tokString, src[start:j], i)
				i = j
			} else {
				// Unterminated. An apostrophe in a comment reaches here only if
				// the comment scan missed, so treat it as punctuation and keep
				// going rather than swallowing the rest of the file.
				add(tokPunct, string(c), i)
			}
		case c == '`':
			start := i + 1
			j := start
			for ; j < len(src) && src[j] != '`'; j++ {
			}
			if j >= len(src) {
				return toks
			}
			add(tokString, src[start:j], i)
			line, lineStart = advance(src, i, j, line, lineStart)
			i = j
		case isIdentStart(c):
			j := i
			for ; j < len(src) && isIdentPart(src[j]); j++ {
			}
			add(tokIdent, src[i:j], i)
			i = j - 1
		case isDigitByte(c):
			j := i
			for ; j < len(src) && (isIdentPart(src[j]) || src[j] == '.'); j++ {
			}
			add(tokNumber, src[i:j], i)
			i = j - 1
		default:
			add(tokPunct, string(c), i)
		}
	}
	return toks
}

// lexIndented scans Python: `#` comments, single and triple quotes, and the
// same identifier rules. Indentation is not turned into block tokens — the
// column on each token is enough for the one structure the extractor needs,
// which is "the suite belonging to this class statement".
func lexIndented(src string) []token {
	toks := make([]token, 0, len(src)/8+8)
	line, lineStart := 1, 0

	add := func(k tokKind, text string, at int) {
		if len(toks) >= maxORMTokens {
			return
		}
		toks = append(toks, token{kind: k, text: text, line: line, col: at - lineStart})
	}

	for i := 0; i < len(src); i++ {
		if len(toks) >= maxORMTokens {
			break
		}
		c := src[i]
		switch {
		case c == '\n':
			line++
			lineStart = i + 1
		case c == ' ' || c == '\t' || c == '\r':
		case c == '#':
			for i++; i < len(src) && src[i] != '\n'; i++ {
			}
			i--
		case (c == '"' || c == '\'') && i+2 < len(src) && src[i+1] == c && src[i+2] == c:
			start := i + 3
			end := indexTripleQuote(src, start, c)
			if end < 0 {
				return toks
			}
			add(tokString, src[start:end], i)
			line, lineStart = advance(src, i, end, line, lineStart)
			i = end + 2
		case c == '"' || c == '\'':
			quote := c
			start := i + 1
			j := start
			for ; j < len(src); j++ {
				if src[j] == '\\' {
					j++
					continue
				}
				if src[j] == '\n' || src[j] == quote {
					break
				}
			}
			if j < len(src) && src[j] == quote {
				add(tokString, src[start:j], i)
				i = j
			} else {
				add(tokPunct, string(c), i)
			}
		case isIdentStart(c):
			j := i
			for ; j < len(src) && isIdentPart(src[j]); j++ {
			}
			add(tokIdent, src[i:j], i)
			i = j - 1
		case isDigitByte(c):
			j := i
			for ; j < len(src) && (isIdentPart(src[j]) || src[j] == '.'); j++ {
			}
			add(tokNumber, src[i:j], i)
			i = j - 1
		default:
			add(tokPunct, string(c), i)
		}
	}
	return toks
}

// indexTripleQuote finds the closing run of three identical quotes.
func indexTripleQuote(src string, from int, q byte) int {
	for i := from; i+2 < len(src); i++ {
		if src[i] == '\\' {
			i++
			continue
		}
		if src[i] == q && src[i+1] == q && src[i+2] == q {
			return i
		}
	}
	return -1
}

// advance re-counts lines across a span the scanner jumped over.
func advance(src string, from, to, line, lineStart int) (int, int) {
	for i := from; i < to && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	return line, lineStart
}

// matchIndex pairs every bracket with its partner, once, so the chain walk
// never rescans. Unmatched brackets get -1, which is the normal state of a
// truncated or malformed file and must not be an error.
func matchIndex(toks []token) []int {
	match := make([]int, len(toks))
	for i := range match {
		match[i] = -1
	}
	var stack []int
	for i, t := range toks {
		if t.kind != tokPunct || len(t.text) != 1 {
			continue
		}
		switch t.text[0] {
		case '(', '[', '{':
			stack = append(stack, i)
		case ')', ']', '}':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if closerFor(toks[open].text[0]) != t.text[0] {
				// Mismatched pair: a `{` closed by a `)`. Leaving both unmatched
				// is right — claiming a span here would hand an extractor a
				// bracket range that spans an arbitrary part of the file.
				continue
			}
			match[open] = i
			match[i] = open
		}
	}
	return match
}

func closerFor(open byte) byte {
	switch open {
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	}
	return 0
}

// callSeg is one link of a call chain: a name, and the arguments it was called
// with when it was called at all.
type callSeg struct {
	name string
	// args are the tokens between the parentheses, exclusive. Nil for a plain
	// selector (`r.db` — `db` is a field, not a call).
	args []token
	// called distinguishes `Find(&u)` from the `db` in `r.db.Find(&u)`.
	called bool
	line   int
}

// chain is `a.b.C(x).D(y)`: the receiver and everything selected off it.
type chain struct {
	segs []callSeg
	// line is where the chain starts, which is where the statement reads.
	line int
}

// maxChainSegs bounds one chain. A hundred links is not code anyone wrote.
const maxChainSegs = 128

// chainsIn walks the token stream and returns every call chain.
//
// A chain starts at an identifier NOT preceded by a dot, which is what keeps
// `Find` in `db.Where(...).Find(...)` from being reported as its own chain.
// Identifiers inside argument lists are not skipped, so a nested call —
// `session.execute(select(User))` — yields both the outer chain and the inner
// one, and the extractor sees the `select(User)` it needs.
func chainsIn(toks []token, match []int) []chain {
	var out []chain
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tokIdent {
			continue
		}
		if i > 0 && isPunct(toks[i-1], ".") {
			continue
		}
		c := chain{line: toks[i].line}
		j := i
		for len(c.segs) < maxChainSegs {
			if j >= len(toks) || toks[j].kind != tokIdent {
				break
			}
			seg := callSeg{name: toks[j].text, line: toks[j].line}
			j++
			if j < len(toks) && isPunct(toks[j], "(") {
				end := match[j]
				if end < 0 {
					break
				}
				seg.args = toks[j+1 : end]
				seg.called = true
				j = end + 1
			}
			c.segs = append(c.segs, seg)
			if j < len(toks) && isPunct(toks[j], ".") {
				j++
				continue
			}
			break
		}
		if len(c.segs) > 0 {
			out = append(out, c)
		}
	}
	return out
}

func isPunct(t token, s string) bool { return t.kind == tokPunct && t.text == s }

// hasCall reports whether the chain calls any of the named methods. Used as the
// framework-spelling gate: `Model`, `Preload` and `Migrator` are GORM's own
// vocabulary in a way that `Delete` is not.
func (c chain) hasCall(names map[string]bool) bool {
	for _, s := range c.segs {
		if s.called && names[s.name] {
			return true
		}
	}
	return false
}

// call returns the first call to the named method.
func (c chain) call(name string) (callSeg, bool) {
	for _, s := range c.segs {
		if s.called && s.name == name {
			return s, true
		}
	}
	return callSeg{}, false
}

// firstString returns the first string literal in an argument list.
func firstString(args []token) (string, bool) {
	for _, t := range args {
		if t.kind == tokString {
			return t.text, true
		}
	}
	return "", false
}

// namedString returns the value of a `name = "..."` argument, which is how
// every annotation and keyword argument in scope here spells a table name.
func namedString(args []token, key string) (string, bool) {
	for i := 0; i+2 < len(args); i++ {
		if args[i].kind == tokIdent && args[i].text == key &&
			isPunct(args[i+1], "=") && args[i+2].kind == tokString {
			return args[i+2].text, true
		}
	}
	return "", false
}
