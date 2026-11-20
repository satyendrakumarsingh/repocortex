package parse

// Agda: constructs the layout-sensitive grammar reads wrongly.
//
// The grammar has no notion of where an equation ends, so three forms cost the
// definitions after them (fixtures/langs/agda/KNOWN_GAPS.md):
//
//   - a right-hand side of three or more atoms (`double n = n +++ n`) takes in
//     the lines after it as more atoms, silently: the tree has no error node,
//     and the signatures that follow are not found;
//   - a `where` block in an equation reads its local signatures as top-level
//     functions and its equations as an error that runs to the end of the file;
//   - `data Vec (A : Set) : Nat → Set where`, a data type with both parameters
//     and an index, turns the whole file into one error node.
//
// An equation defines nothing (Agda requires the type signature, and the
// signature is the definition), so the mask overwrites an equation's whole
// right-hand side, where blocks included, with spaces and puts one `_` at its
// first character so that `double n = _` is still an equation of one atom.
// The parameters of an indexed data type are overwritten too; the name, the
// index and the constructors are kept. Same length, newlines kept.
//
// Only an equation that has a where block or three or more atoms is touched,
// so a file written in the short form the grammar reads (the fixture's
// Shapes.agda) is parsed once.

import "bytes"

// findAgdaConstructs returns the spans, in source order.
func findAgdaConstructs(src []byte) []maskSpan {
	if !bytes.Contains(src, []byte("=")) && !bytes.Contains(src, []byte("data")) {
		return nil
	}
	code := agdaCodeView(src)
	lines := agdaLines(code)
	var out []maskSpan
	for i := 0; i < len(lines); {
		l := lines[i]
		if l.blank {
			i++
			continue
		}
		first := agdaFirstWord(code[l.start:l.end])
		if agdaHeaderKeywords[first] {
			end := i
			if first == "module" || first == "data" || first == "record" {
				// The header runs to the line that holds `where`.
				for end+1 < len(lines) && !agdaHasWhere(code, lines, i, end) &&
					(lines[end+1].blank || lines[end+1].indent > l.indent) {
					end++
				}
			}
			if first == "field" && end == i {
				// `field x : T` on one line: the signature after the keyword.
				if toks := agdaTokens(code, l.start, l.end); len(toks) > 1 {
					if sp, ok := agdaExpressionSpan(src, code, toks[1].start, l.end); ok {
						out = append(out, sp)
					}
				}
			}
			if first == "data" {
				if sp, ok := agdaDataParams(code, lines[i].start, lines[end].end); ok {
					out = append(out, sp)
				}
			}
			i = end + 1
			continue
		}
		end := i
		for end+1 < len(lines) && (lines[end+1].blank || lines[end+1].indent > l.indent) {
			end++
		}
		for lines[end].blank {
			end--
		}
		if !agdaSkipKeywords[first] {
			if sp, ok := agdaExpressionSpan(src, code, l.start, lines[end].end); ok {
				out = append(out, sp)
			}
		}
		i = end + 1
	}
	return out
}

// Items that open a block of items of their own: the items inside are read
// one by one, so the block is never one item's continuation.
var agdaHeaderKeywords = map[string]bool{
	"module": true, "data": true, "record": true, "private": true, "abstract": true,
	"instance": true, "mutual": true, "postulate": true, "field": true, "macro": true,
	"variable": true, "primitive": true, "opaque": true, "interleaved": true,
}

// Items whose `=` is not an equation's.
var agdaSkipKeywords = map[string]bool{
	"open": true, "import": true, "pattern": true, "syntax": true, "infix": true,
	"infixl": true, "infixr": true, "unquoteDecl": true, "unquoteDef": true,
	"constructor": true, "using": true, "hiding": true, "renaming": true,
}

type agdaLine struct {
	start, end int // end is before the newline
	indent     int
	blank      bool
}

func agdaLines(code []byte) []agdaLine {
	var out []agdaLine
	for start := 0; start <= len(code); {
		end := bytes.IndexByte(code[start:], '\n')
		if end < 0 {
			end = len(code)
		} else {
			end += start
		}
		l := agdaLine{start: start, end: end, blank: true}
		for k := start; k < end; k++ {
			c := code[k]
			if c == ' ' || c == '\t' || c == '\r' {
				if l.blank {
					l.indent++
				}
				continue
			}
			l.blank = false
			break
		}
		if l.blank {
			l.indent = 0
		}
		out = append(out, l)
		start = end + 1
	}
	return out
}

func agdaFirstWord(line []byte) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r') {
		i++
	}
	j := i
	for j < len(line) && !agdaSpace(line[j]) && !agdaDelim(line[j]) {
		j++
	}
	return string(line[i:j])
}

func agdaSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func agdaDelim(c byte) bool {
	return c == '(' || c == ')' || c == '{' || c == '}' || c == ';' || c == '@'
}

// agdaCodeView returns src with comments and strings overwritten by spaces
// (newlines kept): what is left is code, at the same offsets.
func agdaCodeView(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(a, b int) {
		for k := a; k < b && k < len(out); k++ {
			if out[k] != '\n' && out[k] != '\r' {
				out[k] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '{' && i+1 < len(src) && src[i+1] == '-':
			depth, k := 0, i
			for ; k+1 < len(src); k++ {
				if src[k] == '{' && src[k+1] == '-' {
					depth++
					k++
				} else if src[k] == '-' && src[k+1] == '}' {
					depth--
					k++
					if depth == 0 {
						k++
						break
					}
				}
			}
			if depth > 0 {
				k = len(src)
			}
			blank(i, k)
			i = k
		case c == '-' && i+1 < len(src) && src[i+1] == '-' && agdaLineCommentAt(src, i):
			k := i
			for k < len(src) && src[k] != '\n' {
				k++
			}
			blank(i, k)
			i = k
		case c == '"':
			k := i + 1
			for k < len(src) && src[k] != '"' && src[k] != '\n' {
				if src[k] == '\\' {
					k++
				}
				k++
			}
			if k < len(src) && src[k] == '"' {
				k++
			}
			blank(i, min(k, len(src)))
			i = min(k, len(src))
		default:
			i++
		}
	}
	return out
}

// agdaLineCommentAt reports whether the `--` at src[i] starts a comment: it
// begins a token, and is not the start of an operator such as `-->`.
func agdaLineCommentAt(src []byte, i int) bool {
	if i > 0 && !agdaSpace(src[i-1]) && !agdaDelim(src[i-1]) {
		return false
	}
	if i+2 >= len(src) {
		return true
	}
	n := src[i+2]
	return agdaSpace(n) || n == '-' || n >= 'A' && n <= 'Z' || n >= 'a' && n <= 'z' || n >= '0' && n <= '9' || n >= 0x80
}

type agdaToken struct {
	start, end int
	depth      int
	text       string
}

// agdaTokens splits code[a:b] into tokens: runs of non-space, with the
// brackets that Agda always reads on their own as tokens of their own. depth
// is the nesting of those brackets outside the token.
func agdaTokens(code []byte, a, b int) []agdaToken {
	var out []agdaToken
	depth := 0
	for i := a; i < b; {
		c := code[i]
		switch {
		case agdaSpace(c):
			i++
		case agdaDelim(c):
			if c == ')' || c == '}' {
				depth = max(0, depth-1)
			}
			out = append(out, agdaToken{i, i + 1, depth, string(c)})
			if c == '(' || c == '{' {
				depth++
			}
			i++
		default:
			j := i
			for j < b && !agdaSpace(code[j]) && !agdaDelim(code[j]) {
				j++
			}
			out = append(out, agdaToken{i, j, depth, string(code[i:j])})
			i = j
		}
	}
	return out
}

func agdaHasWhere(code []byte, lines []agdaLine, from, to int) bool {
	for _, t := range agdaTokens(code, lines[from].start, lines[to].end) {
		if t.depth == 0 && t.text == "where" {
			return true
		}
	}
	return false
}

// agdaDataParams returns the span that blanks the parameters of a data header
// that has both parameters and an index type: `data Vec (A : Set) : Nat → Set`.
func agdaDataParams(code []byte, a, b int) (maskSpan, bool) {
	toks := agdaTokens(code, a, b)
	if len(toks) < 4 || toks[0].text != "data" {
		return maskSpan{}, false
	}
	colon := -1
	for k := 2; k < len(toks); k++ {
		if toks[k].depth == 0 && toks[k].text == ":" {
			colon = k
			break
		}
	}
	if colon < 0 {
		return maskSpan{}, false
	}
	first, last := -1, -1
	for k := 2; k < colon; k++ {
		if toks[k].depth == 0 && toks[k].text != "(" && toks[k].text != "{" && toks[k].text != ")" && toks[k].text != "}" {
			return maskSpan{}, false // something that is not a parameter group
		}
		if toks[k].text == "(" || toks[k].text == "{" {
			if first < 0 {
				first = k
			}
		}
		if toks[k].text == ")" || toks[k].text == "}" {
			last = k
		}
	}
	if first < 0 || last < first {
		return maskSpan{}, false
	}
	index := false
	for _, t := range toks[colon+1:] {
		if t.depth == 0 && (t.text == "→" || t.text == "->") {
			index = true
		}
	}
	if !index {
		return maskSpan{}, false
	}
	start, end := toks[first].start, toks[last].end
	return maskSpan{start: start, repl: agdaBlankRange(code, start, end)}, true
}

// agdaBlankRange is spaces for code[a:b], keeping newlines.
func agdaBlankRange(code []byte, a, b int) []byte {
	r := make([]byte, b-a)
	for k := a; k < b; k++ {
		if code[k] == '\n' || code[k] == '\r' {
			r[k-a] = code[k]
		} else {
			r[k-a] = ' '
		}
	}
	return r
}

// agdaExpressionSpan returns the span that blanks the expression of the item
// held in code[a:b] (the right-hand side of an equation, or the type of a
// signature, a constructor or a field) when it is one the grammar misreads:
// a run of three or more atoms, or a where or let. A parenthesised or braced
// group is one atom of the run around it. The original text is kept where it
// is not code (a comment inside is overwritten with the rest).
func agdaExpressionSpan(src, code []byte, a, b int) (maskSpan, bool) {
	toks := agdaTokens(code, a, b)
	at := -1
	for k, t := range toks {
		if t.depth == 0 && (t.text == ":" || t.text == "=") {
			at = k
			break
		}
	}
	if at < 0 || at+1 >= len(toks) || !agdaBroken(toks[at+1:]) {
		return maskSpan{}, false
	}
	start, end := toks[at].end, toks[len(toks)-1].end
	r := agdaBlankRange(src, start, end)
	for k := start; k < end; k++ {
		if !agdaSpace(code[k]) {
			r[k-start] = '_'
			break
		}
	}
	return maskSpan{start: start, repl: r, drop: toks[at].text == "="}, true
}

// agdaBroken reports whether the tokens of an expression hold what the
// grammar cannot read: a where or let, or three atoms in a row at any depth.
func agdaBroken(toks []agdaToken) bool {
	run := 0
	var outer []int
	for _, t := range toks {
		switch t.text {
		case "where", "let":
			return true
		case "(", "{":
			outer = append(outer, run)
			run = 0
		case ")", "}":
			if len(outer) == 0 {
				run = 0
				continue
			}
			run = outer[len(outer)-1] + 1
			outer = outer[:len(outer)-1]
		case ";", "→", "->", ":", "=", "λ", "\\", "∀", "|", "..", "...", "←":
			run = 0
		case "@":
		default:
			run++
		}
		if run >= 3 {
			return true
		}
	}
	return false
}
