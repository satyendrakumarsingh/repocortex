package parse

// Swift: an attribute written on a function type.
//
// `@escaping @Sendable (_ data: Data?) -> Void` is how a parameter or a
// typealias takes a concurrency-safe closure. The grammar reads the attribute
// as one that takes arguments, reads `(_ data: Data?)` as its argument list,
// and fails there; with a second such parameter in the file it gives up on the
// whole declaration, and then on the class around it (Alamofire's
// DownloadRequest.swift: 13 of 41 definitions). See
// fixtures/langs/swift/KNOWN_GAPS.md.
//
// The mask overwrites the attribute's name with spaces when only white space
// separates it from an opening parenthesis, which makes the parenthesis a
// function type of its own. A type's attribute carries no definition, so
// nothing is lost; text of the same length keeps every offset and line.

var swiftTypeAttributes = []string{"@Sendable", "@MainActor"}

// findSwiftTypeAttributes returns one span for each `@Sendable` or
// `@MainActor` that is followed by `(` after white space only, outside
// comments and strings.
func findSwiftTypeAttributes(src []byte) []maskSpan {
	var out []maskSpan
	for i := 0; i < len(src); {
		if next, ok := swSkipTrivia(src, i); ok {
			i = next
			continue
		}
		if src[i] == '@' {
			for _, attr := range swiftTypeAttributes {
				end := i + len(attr)
				if end >= len(src) || string(src[i:end]) != attr {
					continue
				}
				k := end
				for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
					k++
				}
				if k > end && k < len(src) && src[k] == '(' {
					out = append(out, maskSpan{start: i, repl: []byte(blankSpaces(len(attr)))})
					i = end
				}
				break
			}
		}
		i++
	}
	return out
}

func blankSpaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// swSkipTrivia returns the end of the comment or string that starts at
// src[i], if one does: `//`, a nested `/* */`, `"..."`, `"""..."""`, and the
// raw forms `#"..."#`.
func swSkipTrivia(src []byte, i int) (int, bool) {
	c := src[i]
	switch {
	case c == '/' && i+1 < len(src) && src[i+1] == '/':
		for i < len(src) && src[i] != '\n' {
			i++
		}
		return i, true
	case c == '/' && i+1 < len(src) && src[i+1] == '*':
		depth := 0
		for k := i; k+1 < len(src); k++ {
			switch {
			case src[k] == '/' && src[k+1] == '*':
				depth++
				k++
			case src[k] == '*' && src[k+1] == '/':
				depth--
				k++
				if depth == 0 {
					return k + 1, true
				}
			}
		}
		return len(src), true
	case c == '"' || c == '#':
		hashes := 0
		k := i
		for k < len(src) && src[k] == '#' {
			hashes++
			k++
		}
		if k >= len(src) || src[k] != '"' {
			return i, false
		}
		multi := k+2 < len(src) && src[k+1] == '"' && src[k+2] == '"'
		open := 1
		if multi {
			open = 3
		}
		k += open
		for ; k < len(src); k++ {
			if src[k] == '\\' && hashes == 0 {
				k++
				continue
			}
			if src[k] == '\n' && !multi {
				return k, true
			}
			if src[k] != '"' {
				continue
			}
			j := k + 1
			if multi {
				if j+1 >= len(src) || src[j] != '"' || src[j+1] != '"' {
					continue
				}
				j += 2
			}
			h := 0
			for h < hashes && j+h < len(src) && src[j+h] == '#' {
				h++
			}
			if h == hashes {
				return j + h, true
			}
		}
		return len(src), true
	}
	return i, false
}
