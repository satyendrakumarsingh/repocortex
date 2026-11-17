package parse

import "bytes"

// Haxe: metadata before a type, and the short form of an object typedef.
//
// Two constructs at the top of a module make the grammar lose what follows,
// and neither can be repaired by a query (fixtures/langs/haxe/KNOWN_GAPS.md).
//
//   - Metadata before a type declaration (`@:keep class X`,
//     `@:autoBuild(macros.Live.build()) extern interface Live`): the type's
//     own node is not built, so its name is not in the tree.
//   - An object typedef with fields written as in a signature
//     (`typedef Argument = { name : String, ?t : CType };`, or the extension
//     `{> Base, var z : Int }`): the parser does not recover at the end of the
//     declaration, and every declaration after it in the file is lost.
//
//   - A constraint on a type parameter (`class Box<T:Dynamic>`, `function f<T:Foo>()`):
//     the declaration, and with a class the file after it, is lost.
//   - A `case` or `default` clause with nothing in it (`case A(_), B(_):` on
//     its own line, or a `default:` that ends the switch): the parser fails
//     and the rest of the class is read as floating declarations or lost.
//
// The mask overwrites the metadata, a type parameter's constraint, and the inside of the braces of such a
// typedef, with spaces (newlines kept). Metadata of a type and the fields of a
// typedef are not definitions, and an empty clause gets a `;` in the first blank
// after its colon (an empty statement, no change of meaning), so nothing is
// lost; the typedef is read as the
// empty `{}` and stays a type alias. The block form
// (`typedef P = { var x:Int; var y:Int }`) is read properly and is left alone.

var haxeTypeKeywords = []string{"class", "interface", "enum", "abstract", "typedef", "extern", "private", "final"}

// findHaxeConstructs returns the spans, in source order.
func findHaxeConstructs(src []byte) []maskSpan {
	var out []maskSpan
	depth := 0
	lineStart := true
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			i += end + 4
			continue
		case c == '"' || c == '\'':
			i = hxSkipString(src, i)
			lineStart = false
			continue
		case c == '{':
			depth++
		case c == '}':
			depth--
		case c == '\n':
			lineStart = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r':
			i++
			continue
		}
		if c == 'd' && hxWordAt(src, i, "default") || c == 'c' && hxWordAt(src, i, "case") {
			if span, ok := hxEmptyClause(src, i); ok {
				out = append(out, span)
			}
		}
		if c >= 'a' && c <= 'z' && (hxWordAt(src, i, "class") || hxWordAt(src, i, "interface") || hxWordAt(src, i, "typedef") ||
			hxWordAt(src, i, "abstract") || hxWordAt(src, i, "function")) {
			out = append(out, hxConstraints(src, i)...)
		}
		if depth == 0 {
			if c == '@' && lineStart {
				if end, ok := hxMetadataBeforeType(src, i); ok {
					out = append(out, maskSpan{start: i, repl: hxBlank(src[i:end])})
					i = end
					lineStart = false
					continue
				}
			}
			if c == 't' && hxWordAt(src, i, "typedef") {
				if open, closeAt, ok := hxShortObjectTypedef(src, i); ok {
					out = append(out, maskSpan{start: open + 1, repl: hxBlank(src[open+1 : closeAt])})
					i = closeAt + 1
					lineStart = false
					continue
				}
			}
		}
		lineStart = false
		i++
	}
	return out
}

func hxBlank(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c == '\n' || c == '\r' {
			out[i] = c
		} else {
			out[i] = ' '
		}
	}
	return out
}

func hxSkipString(src []byte, i int) int {
	q := src[i]
	for k := i + 1; k < len(src); k++ {
		switch src[k] {
		case '\\':
			k++
		case q:
			return k + 1
		case '\n':
			return k
		}
	}
	return len(src)
}

func hxWordAt(src []byte, i int, w string) bool {
	end := i + len(w)
	if end > len(src) || string(src[i:end]) != w {
		return false
	}
	if i > 0 && gvIdentByte(src[i-1]) {
		return false
	}
	return end == len(src) || !gvIdentByte(src[end])
}

func hxSkipSpace(src []byte, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

// hxMetadataBeforeType reads the run of metadata that starts at src[i] and
// reports where it ends, if a type declaration follows it.
func hxMetadataBeforeType(src []byte, i int) (int, bool) {
	k := i
	for k < len(src) && src[k] == '@' {
		k++
		if k < len(src) && src[k] == ':' {
			k++
		}
		s := k
		for k < len(src) && (gvIdentByte(src[k]) || src[k] == '.') {
			k++
		}
		if k == s {
			return 0, false
		}
		if k < len(src) && src[k] == '(' {
			depth := 0
			for ; k < len(src); k++ {
				if src[k] == '"' || src[k] == '\'' {
					k = hxSkipString(src, k) - 1
				} else if src[k] == '(' {
					depth++
				} else if src[k] == ')' {
					depth--
					if depth == 0 {
						k++
						break
					}
				}
			}
			if depth != 0 {
				return 0, false
			}
		}
		end := k
		k = hxSkipSpace(src, k)
		if k < len(src) && src[k] != '@' {
			for _, w := range haxeTypeKeywords {
				if hxWordAt(src, k, w) {
					return end, true
				}
			}
			return 0, false
		}
		if k >= len(src) {
			return 0, false
		}
		if src[k] == '@' {
			// another one follows; the run ends at the last
			if e2, ok := hxMetadataBeforeType(src, k); ok {
				return e2, true
			}
			return 0, false
		}
	}
	return 0, false
}

// hxShortObjectTypedef reports the braces of `typedef Name = { ... }` when
// the inside holds fields written as in a signature, with no `var` or
// `function`, or starts with the extension marker `>`.
func hxShortObjectTypedef(src []byte, i int) (open, closeAt int, ok bool) {
	k := hxSkipSpace(src, i+len("typedef"))
	s := k
	for k < len(src) && gvIdentByte(src[k]) {
		k++
	}
	if k == s {
		return 0, 0, false
	}
	if k < len(src) && src[k] == '<' { // type parameters
		d := 0
		for ; k < len(src); k++ {
			if src[k] == '<' {
				d++
			} else if src[k] == '>' {
				d--
				if d == 0 {
					k++
					break
				}
			}
		}
	}
	k = hxSkipSpace(src, k)
	if k >= len(src) || src[k] != '=' {
		return 0, 0, false
	}
	k = hxSkipSpace(src, k+1)
	if k >= len(src) || src[k] != '{' {
		return 0, 0, false
	}
	open = k
	depth := 0
	for ; k < len(src); k++ {
		switch src[k] {
		case '"', '\'':
			k = hxSkipString(src, k) - 1
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				closeAt = k
				goto found
			}
		}
	}
	return 0, 0, false
found:
	inner := src[open+1 : closeAt]
	if len(bytes.TrimSpace(inner)) == 0 {
		return 0, 0, false
	}
	for _, w := range []string{"var", "function", "final"} {
		for j := 0; j+len(w) <= len(inner); j++ {
			if hxWordAt(inner, j, w) {
				if bytes.HasPrefix(bytes.TrimSpace(inner), []byte(">")) {
					break
				}
				return 0, 0, false
			}
		}
	}
	return open, closeAt, true
}

// hxEmptyClause reads the `case` or `default` at src[i]. A `default:` that
// ends the switch with nothing in it becomes `case _:;`, the same branch with
// the same length. A `case` with several patterns and nothing in it loses its
// patterns after the first (the span covers the first top-level comma up to
// the colon); the other patterns bind nothing that is a definition.
func hxEmptyClause(src []byte, i int) (maskSpan, bool) {
	isDefault := src[i] == 'd'
	k := i + 4
	if isDefault {
		k = i + len("default")
	}
	depth, comma := 0, -1
	for ; k < len(src); k++ {
		c := src[k]
		if c == '"' || c == '\'' {
			k = hxSkipString(src, k) - 1
			continue
		}
		switch {
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
			if depth < 0 {
				return maskSpan{}, false
			}
		case c == ';' || c == '\n':
			return maskSpan{}, false
		case c == ',' && depth == 0 && comma < 0:
			comma = k
		}
		if c == ':' && depth == 0 && (k+1 >= len(src) || src[k+1] != ':') {
			break
		}
	}
	if k >= len(src) {
		return maskSpan{}, false
	}
	colon := k
	next := -1
	for k++; k < len(src) && next < 0; {
		switch {
		case src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r':
			k++
		case src[k] == '/' && k+1 < len(src) && src[k+1] == '/':
			for k < len(src) && src[k] != '\n' {
				k++
			}
		case src[k] == '/' && k+1 < len(src) && src[k+1] == '*':
			e := bytes.Index(src[k+2:], []byte("*/"))
			if e < 0 {
				return maskSpan{}, false
			}
			k += e + 4
		default:
			next = k
		}
	}
	if next < 0 {
		return maskSpan{}, false
	}
	switch {
	case isDefault && colon == i+len("default") && src[next] == '}':
		return maskSpan{start: i, repl: []byte("case _:;")}, true
	case !isDefault && comma >= 0 && (hxWordAt(src, next, "case") || hxWordAt(src, next, "default") || src[next] == '}'):
		return maskSpan{start: comma, repl: hxBlank(src[comma:colon])}, true
	}
	return maskSpan{}, false
}

// hxConstraints returns a span for each constraint in the type parameters that
// follow the keyword at src[i] and its name: `<T:Dynamic, K:(A, B)>` keeps
// `T` and `K` and blanks what follows each colon, up to the comma or the
// closing bracket at the same level.
func hxConstraints(src []byte, i int) []maskSpan {
	k := i
	for k < len(src) && gvIdentByte(src[k]) {
		k++
	}
	k = hxSkipSpace(src, k)
	s := k
	for k < len(src) && gvIdentByte(src[k]) {
		k++
	}
	if k == s || k >= len(src) || src[k] != '<' {
		return nil
	}
	var out []maskSpan
	depth, from := 0, -1
	for ; k < len(src); k++ {
		c := src[k]
		switch {
		case c == '\n' || c == ';' || c == '{':
			return nil
		case c == '-' && k+1 < len(src) && src[k+1] == '>':
			k++
		case c == '<' || c == '(':
			depth++
		case c == '>' || c == ')':
			depth--
			if depth == 0 {
				if from >= 0 {
					out = append(out, maskSpan{start: from, repl: hxBlank(src[from:k])})
				}
				return out
			}
		case c == ':' && depth == 1 && from < 0:
			from = k
		case c == ',' && depth == 1 && from >= 0:
			out = append(out, maskSpan{start: from, repl: hxBlank(src[from:k])})
			from = -1
		}
	}
	return nil
}
