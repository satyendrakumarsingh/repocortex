package prune

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode"
)

// Options controls how code is pruned.
type Options struct {
	// KeepDocstrings preserves comments and docstrings preceding declarations.
	KeepDocstrings bool
	// Placeholder is the text inserted inside stripped bodies. Default: "/* ... */"
	Placeholder string
}

// DefaultOptions returns the standard pruning configuration.
func DefaultOptions() Options {
	return Options{
		KeepDocstrings: true,
		Placeholder:    "/* ... */",
	}
}

// PruneFile reads source code and strips function and method bodies while
// preserving declarations, type signatures, interfaces, imports, and docstrings.
// This reduces token consumption by 90-95% for non-essential context files.
func PruneFile(path string, src []byte) ([]byte, error) {
	return PruneFileWithOptions(path, src, DefaultOptions())
}

// PruneFileWithOptions strips implementation bodies using the specified options.
func PruneFileWithOptions(path string, src []byte, opts Options) ([]byte, error) {
	if len(src) == 0 {
		return src, nil
	}
	if opts.Placeholder == "" {
		opts.Placeholder = "/* ... */"
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".py":
		return prunePython(src, opts), nil
	default:
		// Default to C-family / braced languages (Go, Java, TS, JS, Rust, C#, C, C++, PHP)
		return pruneBraced(src, opts), nil
	}
}

// pruneBraced strips function and method bodies from braced languages.
func pruneBraced(src []byte, opts Options) []byte {
	var out bytes.Buffer
	out.Grow(len(src) / 4)

	n := len(src)
	i := 0

	var (
		inFuncSignature bool
		parenDepth      int
	)

	// lastWord captures the most recent identifier seen before a brace.
	var wordBuf strings.Builder

	flushWord := func() string {
		w := wordBuf.String()
		wordBuf.Reset()
		return w
	}

	for i < n {
		c := src[i]

		// 1. Line comment
		if c == '/' && i+1 < n && src[i+1] == '/' {
			start := i
			for i < n && src[i] != '\n' {
				i++
			}
			if opts.KeepDocstrings {
				out.Write(src[start:i])
			}
			continue
		}

		// 2. Block comment
		if c == '/' && i+1 < n && src[i+1] == '*' {
			start := i
			i += 2
			for i+1 < n && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			if opts.KeepDocstrings {
				out.Write(src[start:i])
			}
			continue
		}

		// 3. String literals (double quote, single quote, backtick)
		if c == '"' || c == '\'' || c == '`' {
			quote := c
			out.WriteByte(c)
			i++
			for i < n {
				sc := src[i]
				out.WriteByte(sc)
				if sc == '\\' && quote != '`' && i+1 < n {
					i++
					out.WriteByte(src[i])
					i++
					continue
				}
				if sc == quote {
					i++
					break
				}
				i++
			}
			continue
		}

		// 4. Track identifiers to recognize function headers
		if unicode.IsLetter(rune(c)) || c == '_' {
			wordBuf.WriteByte(c)
			out.WriteByte(c)
			i++
			continue
		}

		w := flushWord()
		switch w {
		case "func", "function", "def":
			inFuncSignature = true
		case "struct", "interface", "class", "enum":
			// Type containers should not have their member blocks stripped.
			inFuncSignature = false
		}

		if c == '(' {
			parenDepth++
			out.WriteByte(c)
			i++
			continue
		}
		if c == ')' {
			if parenDepth > 0 {
				parenDepth--
			}
			out.WriteByte(c)
			i++
			continue
		}

		// 5. When we hit an opening brace '{'
		if c == '{' {
			// If we are in a function signature or just closed parameters:
			if inFuncSignature || parenDepth == 0 {
				// Check if this block belongs to a function (has parenthesis earlier on same declaration)
				// Look ahead to find matching closing brace
				end := findMatchingBrace(src, i)
				if end > i && isLikelyFunctionBody(src, i) {
					out.WriteString("{ ")
					out.WriteString(opts.Placeholder)
					out.WriteString(" }")
					i = end + 1
					inFuncSignature = false
					continue
				}
			}
			inFuncSignature = false
			out.WriteByte(c)
			i++
			continue
		}

		if c == ';' || c == '\n' {
			if parenDepth == 0 {
				inFuncSignature = false
			}
		}

		out.WriteByte(c)
		i++
	}

	return out.Bytes()
}

// findMatchingBrace finds the closing '}' matching the '{' at startIdx.
func findMatchingBrace(src []byte, startIdx int) int {
	depth := 0
	n := len(src)
	i := startIdx

	for i < n {
		c := src[i]
		if c == '/' && i+1 < n && src[i+1] == '/' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i+1 < n && (src[i] != '*' || src[i+1] != '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote := c
			i++
			for i < n {
				if src[i] == '\\' && quote != '`' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}

		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// isLikelyFunctionBody checks whether the block opening at '{' is a function/method
// body rather than a struct/interface/class/enum definition.
func isLikelyFunctionBody(src []byte, braceIdx int) bool {
	// Walk backwards from brace to find preceding non-space tokens
	j := braceIdx - 1
	for j >= 0 && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\n') {
		j--
	}
	if j < 0 {
		return false
	}

	// Read backwards up to 300 characters or start of line/decl
	start := j - 300
	if start < 0 {
		start = 0
	}
	prefix := string(src[start : j+1])

	// If it contains "struct", "interface", "enum", "class", it's a type block, not a function body.
	// Unless preceded by func/method!
	words := strings.Fields(prefix)
	for idx, w := range words {
		if w == "struct" || w == "interface" || w == "enum" {
			// Verify it's not a parameter type like func f(s struct{})
			if idx > 0 && words[idx-1] == "type" {
				return false
			}
			if idx == 0 {
				return false
			}
		}
	}

	// A function body usually has a closing parenthesis ')' somewhere before the '{'
	return strings.Contains(prefix, ")") || strings.Contains(prefix, "func")
}

// prunePython strips function and method bodies from Python code.
func prunePython(src []byte, opts Options) []byte {
	var out bytes.Buffer
	lines := strings.Split(string(src), "\n")

	inDef := false
	defIndent := 0
	inDocstring := false
	docstringDelim := ""

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Check for docstring continuation
		if inDocstring {
			out.WriteString(line)
			out.WriteByte('\n')
			if strings.Contains(trimmed, docstringDelim) {
				inDocstring = false
				// After closing docstring, emit placeholder
				indentStr := strings.Repeat(" ", defIndent+4)
				out.WriteString(indentStr + "...\n")
			}
			continue
		}

		// Detect function / method definitions
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def ") {
			inDef = true
			defIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			out.WriteString(line)
			out.WriteByte('\n')

			// Check if line ends with ':'
			if !strings.HasSuffix(trimmed, ":") {
				// Multiline def signature
				for i+1 < len(lines) {
					i++
					subLine := lines[i]
					out.WriteString(subLine)
					out.WriteByte('\n')
					if strings.HasSuffix(strings.TrimSpace(subLine), ":") {
						break
					}
				}
			}

			// Check if the next line is a docstring
			if i+1 < len(lines) {
				nextTrimmed := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(nextTrimmed, `"""`) || strings.HasPrefix(nextTrimmed, `'''`) {
					delim := nextTrimmed[:3]
					i++
					out.WriteString(lines[i])
					out.WriteByte('\n')
					// Check if single-line docstring
					rest := nextTrimmed[3:]
					if strings.Contains(rest, delim) {
						// Single line docstring completed
						indentStr := strings.Repeat(" ", defIndent+4)
						out.WriteString(indentStr + "...\n")
					} else {
						inDocstring = true
						docstringDelim = delim
					}
					continue
				}
			}

			// No docstring, emit placeholder
			indentStr := strings.Repeat(" ", defIndent+4)
			out.WriteString(indentStr + "...\n")
			continue
		}

		if inDef {
			if trimmed == "" {
				continue
			}
			currentIndent := len(line) - len(strings.TrimLeft(line, " \t"))
			if currentIndent > defIndent {
				// Inside function body — skip implementation line!
				continue
			}
			// Dropped out of function body
			inDef = false
		}

		out.WriteString(line)
		out.WriteByte('\n')
	}

	return out.Bytes()
}
