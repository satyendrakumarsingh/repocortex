// Package edit rewrites identifiers in source files under one project root.
//
// It is the only package in RepoCortex that writes to a user's source tree, so
// it is built to refuse: an identifier it cannot find, or cannot tell apart
// from a same-named neighbour on the same line, is an error, never a guess.
package edit

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Errors a caller branches on.
var (
	// ErrNotFound: the line does not contain the identifier. The file no longer
	// says what the index says it does.
	ErrNotFound = errors.New("edit: identifier not found on the line")

	// ErrAmbiguous: the identifier occurs more than once on the line and no
	// recorded column singles out which occurrence the site is.
	ErrAmbiguous = errors.New("edit: identifier occurs more than once on the line and no column singles out the site")

	// ErrStale: the file's bytes are not the ones that were indexed or planned.
	ErrStale = errors.New("edit: file changed since it was indexed")

	// ErrOverlap: two replacements claim the same bytes.
	ErrOverlap = errors.New("edit: replacements overlap")
)

// identRune reports whether r can continue an identifier in the languages this
// project parses (Go, Java, TypeScript, Python): letters, digits, '_' and '$',
// plus the Unicode classes those languages' identifier grammars admit after
// the first character — non-spacing and spacing combining marks (Mn, Mc),
// connector punctuation (Pc), letter numbers (Nl) — and the zero-width
// non-joiner and joiner that JavaScript allows. Erring towards "part of the
// identifier" is the safe direction: it can only turn a match into
// ErrNotFound, never widen one.
func identRune(r rune) bool {
	return r == '_' || r == '$' || r == '\u200c' || r == '\u200d' ||
		unicode.IsLetter(r) || unicode.IsDigit(r) ||
		unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc, unicode.Nl)
}

// lineSpan returns the byte range of 1-indexed line n, without its "\n" or
// "\r\n" terminator, scanning from the top of src. For one lookup only; a
// caller locating many lines in one file uses Source.
func lineSpan(src []byte, n int) (int, int, bool) {
	if n < 1 {
		return 0, 0, false
	}
	start := 0
	for i := 1; i < n; i++ {
		j := bytes.IndexByte(src[start:], '\n')
		if j < 0 {
			return 0, 0, false
		}
		start += j + 1
	}
	return start, lineEnd(src, start), true
}

// lineEnd returns the end of the line starting at start, excluding its
// terminator.
func lineEnd(src []byte, start int) int {
	end := len(src)
	if j := bytes.IndexByte(src[start:], '\n'); j >= 0 {
		end = start + j
	}
	if end > start && src[end-1] == '\r' {
		end--
	}
	return end
}

// Occurrences returns the offsets, relative to line, at which name occurs as
// a whole identifier — the rule every edit uses, so a caller looking for what
// a rename left behind counts exactly what a rename would have matched.
func Occurrences(line []byte, name string) []int {
	if name == "" {
		return nil
	}
	return occurrences(line, name)
}

// occurrences returns the offsets, relative to line, at which name occurs as a
// whole identifier.
func occurrences(line []byte, name string) []int {
	nb := []byte(name)
	var out []int
	for i := 0; i+len(nb) <= len(line); {
		j := bytes.Index(line[i:], nb)
		if j < 0 {
			break
		}
		at := i + j
		if wholeWord(line, at, len(nb)) {
			out = append(out, at)
		}
		i = at + 1
	}
	return out
}

func wholeWord(line []byte, at, n int) bool {
	if at > 0 {
		if r, _ := utf8.DecodeLastRune(line[:at]); identRune(r) {
			return false
		}
	}
	if end := at + n; end < len(line) {
		if r, _ := utf8.DecodeRune(line[end:]); identRune(r) {
			return false
		}
	}
	return true
}

// utf16Offsets converts byte offsets within line (ascending) to UTF-16 code
// unit offsets. Invalid UTF-8 counts one unit per byte, as U+FFFD would.
func utf16Offsets(line []byte, byteOffs []int) []int {
	out := make([]int, len(byteOffs))
	units, pos := 0, 0
	for i, target := range byteOffs {
		for pos < target {
			r, size := utf8.DecodeRune(line[pos:])
			if n := utf16.RuneLen(r); n > 0 {
				units += n
			} else {
				units++
			}
			pos += size
		}
		out[i] = units
	}
	return out
}

// Source is one file's bytes with its line starts indexed once, so locating
// every site in a file costs O(file size + total line length) rather than a
// scan from the top for each line.
type Source struct {
	src    []byte
	starts []int // starts[i] is the byte offset of line i+1
}

// NewSource indexes src's lines. src is not copied and must not be modified
// while the Source is in use.
func NewSource(src []byte) *Source {
	starts := make([]int, 1, bytes.Count(src, []byte{'\n'})+1)
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &Source{src: src, starts: starts}
}

// Locate is Locate(s's bytes, line, old, cols) without rescanning the file.
func (s *Source) Locate(line int, old string, cols []int) ([]int, error) {
	if old == "" {
		return nil, errors.New("edit: empty identifier")
	}
	// Line n exists when n-1 newlines precede it, exactly as lineSpan counts.
	if line < 1 || line > len(s.starts) {
		return nil, fmt.Errorf("%w: line %d is past the end of the file", ErrNotFound, line)
	}
	start := s.starts[line-1]
	return locateIn(s.src, start, lineEnd(s.src, start), line, old, cols)
}

// Locate finds the absolute byte offsets in src at which old must be replaced
// for the sites recorded on one line.
//
// cols holds the evidence column of every site on the line, -1 where none was
// recorded. Stored columns are unreliable in two ways: tree-sitter edges
// record the start of the call expression rather than the name, and SCIP
// indexers for the JVM and TypeScript count UTF-16 code units where
// tree-sitter counts bytes. So a column is trusted only if old, as a whole
// identifier, starts exactly there under BOTH readings — which is to say the
// line before it is plain ASCII. Otherwise f("é", Save, Save) would let the
// UTF-16 column of the second Save select the first.
//
//   - old occurs once on the line: that occurrence.
//   - more than once: exactly the occurrences the columns select, provided
//     every distinct column selects one. Anything else is ErrAmbiguous: a
//     line holding a.Save() and b.Save() names two methods, and guessing which
//     one is the target is how a rename corrupts code.
//   - not at all: ErrNotFound.
func Locate(src []byte, line int, old string, cols []int) ([]int, error) {
	if old == "" {
		return nil, errors.New("edit: empty identifier")
	}
	start, end, ok := lineSpan(src, line)
	if !ok {
		return nil, fmt.Errorf("%w: line %d is past the end of the file", ErrNotFound, line)
	}
	return locateIn(src, start, end, line, old, cols)
}

func locateIn(src []byte, start, end, line int, old string, cols []int) ([]int, error) {
	text := src[start:end]
	occ := occurrences(text, old)
	switch len(occ) {
	case 0:
		return nil, fmt.Errorf("%w: %q on line %d", ErrNotFound, old, line)
	case 1:
		return []int{start + occ[0]}, nil
	}

	// A column selects an occurrence only where its byte offset and its
	// UTF-16 offset are the same number.
	u16 := utf16Offsets(text, occ)
	agreed := make(map[int]bool, len(occ))
	for i, o := range occ {
		if u16[i] == o {
			agreed[o] = true
		}
	}
	seen := make(map[int]bool, len(cols))
	var out []int
	for _, c := range cols {
		if seen[c] {
			continue
		}
		seen[c] = true
		if !agreed[c] {
			return nil, fmt.Errorf("%w: %q occurs %d times on line %d", ErrAmbiguous, old, len(occ), line)
		}
		out = append(out, start+c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %q occurs %d times on line %d", ErrAmbiguous, old, len(occ), line)
	}
	sort.Ints(out)
	return out, nil
}

// Splice returns a copy of src with len(old) bytes at each offset replaced by
// repl. It sorts a copy of the offsets, re-checks that each one still spells
// old and that none overlap, then builds the output front to back in one
// buffer: O(len(src) + len(offsets)). src is never modified.
func Splice(src []byte, old, repl string, offsets []int) ([]byte, error) {
	if old == "" {
		return nil, errors.New("edit: empty identifier")
	}
	sorted := append([]int(nil), offsets...)
	sort.Ints(sorted)
	for i, at := range sorted {
		if at < 0 || at+len(old) > len(src) {
			return nil, fmt.Errorf("edit: offset %d out of range for %d bytes", at, len(src))
		}
		if i > 0 && sorted[i-1]+len(old) > at {
			return nil, fmt.Errorf("%w: offsets %d and %d", ErrOverlap, sorted[i-1], at)
		}
		if string(src[at:at+len(old)]) != old {
			return nil, fmt.Errorf("edit: bytes at offset %d are not %q", at, old)
		}
	}
	size := len(src) + len(sorted)*(len(repl)-len(old))
	out := make([]byte, 0, size)
	prev := 0
	for _, at := range sorted {
		out = append(out, src[prev:at]...)
		out = append(out, repl...)
		prev = at + len(old)
	}
	out = append(out, src[prev:]...)
	return out, nil
}
