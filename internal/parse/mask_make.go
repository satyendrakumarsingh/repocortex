package parse

// Make: a substitution reference with an empty replacement.
//
// `$(BOARD_DIR:/=)` and `$(OBJ:.c=)` strip a suffix. The grammar has no
// production for a replacement that is empty, and it does not stop at the
// line: it gives up building rule and assignment nodes for the rest of the
// file and leaves a flat run of words. In micropython's `ports/stm32/Makefile`
// that is 34 of 75 definitions, and in two of three files of the check every
// one. makeRecovered (lang_make.go) reads what it can from that run. See
// fixtures/langs/make/KNOWN_GAPS.md.
//
// The mask overwrites the text from the `:` to the `=` with spaces, so the
// reference reads as a plain `$(BOARD_DIR   )`. What it expanded to is not a
// definition, so nothing is lost. Same length, newlines kept.

// findMakeEmptySubstitutions returns one span for each `$(NAME:pattern=)` or
// `${NAME:pattern=}`, outside comments.
func findMakeEmptySubstitutions(src []byte) []maskSpan {
	var out []maskSpan
	for i := 0; i+1 < len(src); i++ {
		switch {
		case src[i] == '#' && (i == 0 || src[i-1] != '\\'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case src[i] == '$' && (src[i+1] == '(' || src[i+1] == '{'):
			if colon, eq, ok := makeEmptySubstitution(src, i); ok {
				out = append(out, maskSpan{start: colon, repl: []byte(blankSpaces(eq - colon + 1))})
			}
		}
	}
	return out
}

// makeEmptySubstitution reports whether the reference that opens at src[i]
// (a `$` before `(` or `{`) is NAME:pattern= with nothing after the `=`, and
// where its `:` and `=` are. The pattern may hold references of its own.
func makeEmptySubstitution(src []byte, i int) (colon, eq int, ok bool) {
	open := src[i+1]
	closer := byte(')')
	if open == '{' {
		closer = '}'
	}
	depth := 1
	colon = -1
	for k := i + 2; k < len(src); k++ {
		c := src[k]
		switch {
		case c == '\n':
			return 0, 0, false
		case c == open:
			depth++
		case c == closer:
			depth--
			if depth > 0 {
				continue
			}
			if colon < 0 || src[k-1] != '=' || k-1 <= colon {
				return 0, 0, false
			}
			return colon, k - 1, true
		case depth == 1 && colon < 0:
			if c == ':' {
				if k == i+2 {
					return 0, 0, false
				}
				colon = k
			} else if !makeNameByte(c) {
				return 0, 0, false // a function call, or text: not NAME:...
			}
		case depth == 1 && c == '=' && src[k+1] != closer:
			return 0, 0, false // a replacement after the pattern
		}
	}
	return 0, 0, false
}

func makeNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-' || c == '/'
}
