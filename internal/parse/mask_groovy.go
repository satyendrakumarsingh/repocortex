package parse

import (
	"bytes"
	"sort"
)

// Groovy: the one-line doc comment.
//
// The grammar's groovy_doc rule reads the text after `/**` up to the end of the
// line as the comment's first line, whether or not a `*/` stands on it, and
// goes on reading lines until the next `*/`. A comment that opens and closes on
// one line (`/** the size */`) is therefore never closed: everything after it
// is comment text, up to the next `*/` or the end of the file. In grails-core's
// DefaultConstrainedProperty.groovy that is 49 of 50 definitions. See
// fixtures/langs/groovy/KNOWN_GAPS.md.
//
// The mask blanks the comment's second `*`, which makes it an ordinary block
// comment (`/* the size */`) that the grammar reads correctly. A doc comment
// carries no definition, so nothing is lost. `/**/` is the one exception: the
// grammar fails on it too, but it is an ordinary empty comment already, and
// blanking its second `*` would leave it unclosed, so it is blanked whole. Multi-line doc comments, which the grammar reads properly,
// are left alone.
//
// Not followed: a GString hole that holds a quote of its own kind
// ("${ "x" }"), which can put the scanner out of step with the string it is in;
// the guard refuses a mask that does not help.

// findGroovyOneLineDocs returns one span for each `/** ... */` that opens and
// closes on one line, outside strings and other comments.
func findGroovyOneLineDocs(src []byte) []maskSpan {
	var out []maskSpan
	prev := byte(0) // last significant byte before i, for slashy strings
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			i = gvLineEnd(src, i)
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			end += i + 4 // one past the closing */
			if i+2 < len(src) && src[i+2] == '*' && bytes.IndexByte(src[i:end], '\n') < 0 {
				if end-i == 4 { // /**/ is an ordinary comment already; unclosing it would swallow the file
					out = append(out, maskSpan{start: i, repl: []byte("    ")})
				} else {
					out = append(out, maskSpan{start: i + 2, repl: []byte(" ")})
				}
			}
			i = end
			continue
		case c == '"' || c == '\'':
			i = gvSkipString(src, i)
			prev = c
			continue
		case c == '$' && i+1 < len(src) && src[i+1] == '/':
			// $/ dollar-slashy /$
			if end := bytes.Index(src[i+2:], []byte("/$")); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(src)
			}
			prev = '"'
			continue
		case c == '/' && gvSlashyMayStart(prev):
			i = gvSkipSlashy(src, i)
			prev = '"'
			continue
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
		i++
	}
	return out
}

func gvLineEnd(src []byte, i int) int {
	for i < len(src) && src[i] != '\n' {
		i++
	}
	return i
}

// gvSkipString returns the end of the string that opens at src[i], a ' or a ":
// single-line, or triple-quoted, with backslash escapes.
func gvSkipString(src []byte, i int) int {
	q := src[i]
	if i+2 < len(src) && src[i+1] == q && src[i+2] == q {
		for k := i + 3; k < len(src); k++ {
			if src[k] == '\\' {
				k++
			} else if src[k] == q && k+2 < len(src) && src[k+1] == q && src[k+2] == q {
				return k + 3
			}
		}
		return len(src)
	}
	for k := i + 1; k < len(src); k++ {
		switch src[k] {
		case '\\':
			k++
		case q:
			return k + 1
		case '\n':
			// A single-quoted string does not span lines: the scanner is out
			// of step, so resume at the line.
			return k
		}
	}
	return len(src)
}

// gvSlashyMayStart reports whether a `/` after the significant byte prev opens
// a slashy string rather than dividing: it does after an operator or an opening
// bracket, where a division would have no left operand.
func gvSlashyMayStart(prev byte) bool {
	switch prev {
	case '=', '(', ',', '[', ':', '~', '{', '!', '&', '|', '?', '+', '-', '*', '<', '>', ';':
		return true
	}
	return false
}

// gvSkipSlashy returns the end of the slashy string that opens at src[i].
func gvSkipSlashy(src []byte, i int) int {
	for k := i + 1; k < len(src); k++ {
		switch src[k] {
		case '\\':
			k++
		case '/':
			return k + 1
		}
	}
	return len(src)
}

// Groovy: a method or constructor with modifiers and no return type.
//
// `private static writeStackToFile(File f, Helper h) {` and
// `private Inventory(String name, int size) {` are read as a declaration of the
// modifiers and the name, then a parenthesised expression for the parameters.
// That reading holds for a bare name (`(a)`) and breaks for a typed parameter
// or for two parameters: the parameters become an ERROR, or a call and a stray
// closure, and the name is lost (see the KNOWN_GAPS.md of the language).
//
// The mask keeps the first parameter's name and overwrites the rest of the
// parameter list with spaces, which is the shape the grammar reads. Parameters
// are not definitions, so nothing is lost. Only a line that opens with
// modifiers (and annotations) directly before `name(` and whose `)` is followed
// by `{` is touched: a call does not look like that, and a method with a return
// type is read properly.

// findGroovySignatures returns one span for each such parameter list outside
// comments and strings.
func findGroovySignatures(src []byte) []maskSpan {
	var out []maskSpan
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			i = gvLineEnd(src, i)
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			i += end + 4
			continue
		case c == '"' || c == '\'':
			i = gvSkipString(src, i)
			continue
		case c == '\n':
			i++
			if span, end, ok := gvSignatureAt(src, i); ok {
				out = append(out, span)
				i = end
			}
			continue
		}
		i++
	}
	return out
}

var gvModifiers = map[string]bool{
	"public": true, "private": true, "protected": true, "static": true,
	"final": true, "synchronized": true, "abstract": true,
}

// gvSignatureAt reads one line that starts at src[i].
func gvSignatureAt(src []byte, i int) (maskSpan, int, bool) {
	k := gvSkipBlank(src, i)
	words := 0
	for {
		s := k
		for k < len(src) && gvIdentByte(src[k]) {
			k++
		}
		if k == s {
			return maskSpan{}, 0, false
		}
		if gvModifiers[string(src[s:k])] {
			words++
			k = gvSkipBlank(src, k)
			continue
		}
		if words == 0 {
			return maskSpan{}, 0, false
		}
		break
	}
	k = gvSkipBlank(src, k)
	if k >= len(src) || src[k] != '(' {
		return maskSpan{}, 0, false
	}
	open := k
	// the parameters: top-level pieces split at commas
	var pieces [][2]int
	depth, start := 0, open+1
	closeAt := -1
scan:
	for k = open; k < len(src); k++ {
		switch src[k] {
		case '\n':
			return maskSpan{}, 0, false
		case '"', '\'':
			k = gvSkipString(src, k) - 1
		case '(', '[', '{', '<':
			depth++
		case ']', '}', '>':
			depth--
		case ')':
			depth--
			if depth == 0 {
				pieces = append(pieces, [2]int{start, k})
				closeAt = k
				break scan
			}
		case ',':
			if depth == 1 {
				pieces = append(pieces, [2]int{start, k})
				start = k + 1
			}
		}
	}
	if closeAt < 0 {
		return maskSpan{}, 0, false
	}
	after := gvSkipBlank(src, closeAt+1)
	if after >= len(src) || src[after] != '{' {
		return maskSpan{}, 0, false
	}
	// the first parameter's name: the last identifier before a default value
	first := src[pieces[0][0]:pieces[0][1]]
	if eq := bytes.IndexByte(first, '='); eq >= 0 {
		first = first[:eq]
	}
	nameEnd := len(first)
	for nameEnd > 0 && (first[nameEnd-1] == ' ' || first[nameEnd-1] == '\t') {
		nameEnd--
	}
	nameStart := nameEnd
	for nameStart > 0 && gvIdentByte(first[nameStart-1]) {
		nameStart--
	}
	if nameStart == nameEnd {
		return maskSpan{}, 0, false
	}
	if len(pieces) == 1 && string(bytes.TrimSpace(src[pieces[0][0]:pieces[0][1]])) == string(first[nameStart:nameEnd]) {
		return maskSpan{}, 0, false // already a bare name, which the grammar reads
	}
	repl := make([]byte, closeAt-open-1)
	for j, b := range src[open+1 : closeAt] {
		if b == '\n' {
			repl[j] = '\n'
		} else {
			repl[j] = ' '
		}
	}
	copy(repl[pieces[0][0]-open-1+nameStart:], first[nameStart:nameEnd])
	return maskSpan{start: open + 1, repl: repl}, closeAt, true
}

func gvSkipBlank(src []byte, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i
}

func gvIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// findGroovyMasks is the finder of the language: the one-line doc comments and
// the signatures, in source order.
func findGroovyMasks(src []byte) []maskSpan {
	docs := findGroovyOneLineDocs(src)
	sigs := findGroovySignatures(src)
	if len(sigs) == 0 {
		return docs
	}
	out := append(docs, sigs...)
	sort.Slice(out, func(a, b int) bool { return out[a].start < out[b].start })
	return out
}
