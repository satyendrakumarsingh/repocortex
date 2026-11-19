package parse

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	ts "github.com/odvcencio/gotreesitter"
)

// A guarded macro mask for C, C++ and Objective-C.
//
// WHY. Tree-sitter parses the text as written and nothing runs the
// preprocessor. A declaration written behind a macro the grammar has never
// heard of is therefore not a declaration to it:
//
//	class LEVELDB_EXPORT Comparator { ... };   // the class is named LEVELDB_EXPORT
//	JK_STATIC_INLINE void jk_foo(int a) { }    // an error, and the function is lost
//	static REDIS_NO_SANITIZE("bounds") int f(void) { }
//
// Every public class of google/leveldb is written the first way, so none of
// them were symbols and every subclass bound to an unrelated type of the same
// name.
//
// WHAT. The source is scanned as text (comments, strings and preprocessor
// lines skipped) for the three places such a macro is written, and each
// candidate is overwritten with spaces of the SAME BYTE LENGTH, newlines kept,
// so no byte offset and no line number moves. The masked text is parsed and,
// if it is measurably better, replaces the plain parse.
//
// THE GUARD is what makes a wrong candidate harmless, and it must not be
// weakened. An upper-case identifier is often a type (`FILE *open(void)`,
// `BOOL WINAPI Foo(void)`, `DWORD x;`), an enum value or a macro call
// statement, and masking one of those would delete real text. So:
//
//   - the plain source is parsed first and a file whose plain parse shows no
//     trouble where a candidate sits costs nothing more and is returned as it
//     is;
//   - only then is the masked text parsed, once, and it is kept only if it has
//     fewer error nodes, no fewer definitions, no definition named after a
//     masked token, and every definition the plain parse had (other than one
//     named after a masked token) is still there;
//   - otherwise the plain result stands, byte for byte.
//
// A file is never parsed more than twice by this mechanism.

// maskedLanguages are the grammars the mask applies to.
var maskedLanguages = map[string]bool{"c": true, "cpp": true, "objc": true}

// macroCandidate is a span of the source that may be a macro the grammar does
// not know. name is the macro's identifier; the span also covers a
// parenthesised argument list when there is one.
type macroCandidate struct {
	start, end int
	name       string

	// expect is the byte offset of the name the declaration behind the macro
	// should define, when the text says it defines one (a class with a body, a
	// function with one), and -1 when it does not say. A plain parse that has
	// no definition named there is in trouble even if it reports no error:
	// `class EXPORT Foo { };` can parse as a function called Foo whose return
	// type is a class, without a single error node.
	expect int

	// repl, when not nil, replaces the span instead of blanking it. It is
	// exactly as long as the span (see cmacro_c.go: a function written as a
	// macro call is rewritten so that the grammar sees a definition).
	repl []byte
}

type ctokKind uint8

const (
	ctIdent ctokKind = iota
	ctPunct
	ctScope // "::"
	ctOther // number, string or character literal
	ctDir   // a whole preprocessor directive
)

type ctok struct {
	kind       ctokKind
	start, end int
	c          byte // the character, for a ctPunct
}

func (t ctok) text(src []byte) string { return string(src[t.start:t.end]) }

// scanCTokens splits src into tokens, dropping comments and keeping each
// preprocessor directive as one token, so a `#define` body is never read as
// code.
func scanCTokens(src []byte) []ctok {
	toks := make([]ctok, 0, len(src)/5)
	n := len(src)
	lineStart := true
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '\n':
			lineStart = true
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				if src[i] == '\\' && i+1 < n {
					i++
				}
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i < n && (src[i] != '*' || i+1 >= n || src[i+1] != '/') {
				i++
			}
			i += 2
			if i > n {
				i = n
			}
		case c == '#' && lineStart:
			start := i
			for i < n && src[i] != '\n' {
				if src[i] == '\\' && i+1 < n {
					i++
					if src[i] == '\r' && i+1 < n {
						i++
					}
				} else if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					i += 2
					for i < n && (src[i] != '*' || i+1 >= n || src[i+1] != '/') {
						i++
					}
					i++
				}
				i++
			}
			if i > n {
				i = n
			}
			toks = append(toks, ctok{kind: ctDir, start: start, end: i})
		case c == '"' || c == '\'':
			start := i
			i = skipQuoted(src, i)
			toks = append(toks, ctok{kind: ctOther, start: start, end: i})
			lineStart = false
		case isIdentStart(c):
			start := i
			for i < n && isIdentByte(src[i]) {
				i++
			}
			// A string prefix (L"x", u8"x", R"(x)") is part of the literal.
			if i < n && (src[i] == '"' || src[i] == '\'') && i-start <= 2 {
				switch string(src[start:i]) {
				case "L", "u", "U", "u8":
					i = skipQuoted(src, i)
					toks = append(toks, ctok{kind: ctOther, start: start, end: i})
					lineStart = false
					continue
				case "R", "LR", "uR", "UR", "u8R":
					if src[i] == '"' {
						i = skipRawString(src, i)
						toks = append(toks, ctok{kind: ctOther, start: start, end: i})
						lineStart = false
						continue
					}
				}
			}
			toks = append(toks, ctok{kind: ctIdent, start: start, end: i})
			lineStart = false
		case c >= '0' && c <= '9':
			start := i
			for i < n && (isIdentByte(src[i]) || src[i] == '.' || src[i] == '\'') {
				i++
			}
			toks = append(toks, ctok{kind: ctOther, start: start, end: i})
			lineStart = false
		case c == ':' && i+1 < n && src[i+1] == ':':
			toks = append(toks, ctok{kind: ctScope, start: i, end: i + 2, c: ':'})
			i += 2
			lineStart = false
		default:
			toks = append(toks, ctok{kind: ctPunct, start: i, end: i + 1, c: c})
			i++
			lineStart = false
		}
	}
	return toks
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentByte(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// skipQuoted returns the index after the string or character literal that
// opens at i. An unterminated literal ends at the end of its line.
func skipQuoted(src []byte, i int) int {
	q := src[i]
	i++
	for i < len(src) && src[i] != '\n' {
		if src[i] == '\\' && i+1 < len(src) {
			i += 2
			continue
		}
		if src[i] == q {
			return i + 1
		}
		i++
	}
	return i
}

// skipRawString returns the index after the raw string literal whose opening
// quote is at i: R"delim( ... )delim".
func skipRawString(src []byte, i int) int {
	j := i + 1
	for j < len(src) && src[j] != '(' && src[j] != '\n' && j-i <= 17 {
		j++
	}
	if j >= len(src) || src[j] != '(' {
		return skipQuoted(src, i)
	}
	closer := ")" + string(src[i+1:j]) + "\""
	if k := strings.Index(string(src[j:]), closer); k >= 0 {
		return j + k + len(closer)
	}
	return len(src)
}

// isMacroName reports whether an identifier is written the way a macro is:
// upper case, digits and underscores, at least two characters, at least one
// letter. `FILE`, `BOOL` and `DWORD` match, which is why the guard exists.
func isMacroName(s string) bool {
	if len(s) < 2 {
		return false
	}
	letters := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			letters++
		case c == '_' || c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return letters > 0 && (s[0] < '0' || s[0] > '9')
}

// cStorage are the words that may precede an attribute macro in a declaration.
var cStorage = map[string]bool{
	"static": true, "inline": true, "extern": true, "virtual": true, "constexpr": true,
	"friend": true, "const": true, "volatile": true, "explicit": true, "_Noreturn": true,
}

// notDeclaration are words that start a statement, not a declaration.
var notDeclaration = map[string]bool{
	"return": true, "else": true, "goto": true, "case": true, "default": true, "new": true,
	"delete": true, "throw": true, "sizeof": true, "do": true, "while": true, "if": true,
	"for": true, "switch": true, "using": true, "typedef": true, "break": true, "continue": true,
	"final": true, "override": true, "alignas": true, "operator": true,
}

// findMacroCandidates returns the spans that look like an unknown macro in a
// declaration, in source order and without overlap. It reads text only.
func findMacroCandidates(src []byte, language string) []macroCandidate {
	if !maskedLanguages[language] {
		return nil
	}
	toks := scanCTokens(src)
	var out []macroCandidate
	add := func(c macroCandidate) {
		if len(out) > 0 && c.start < out[len(out)-1].end {
			return
		}
		out = append(out, c)
	}
	s := &candScan{src: src, toks: toks, cpp: language != "c", objc: language == "objc"}
	for i := range toks {
		t := toks[i]
		if t.kind != ctIdent {
			continue
		}
		switch w := t.text(src); w {
		case "class", "struct", "union", "enum":
			for _, c := range s.typeKeyword(i, w) {
				add(c)
			}
		}
		if s.atDeclStart(i) {
			for _, c := range s.declMacros(i) {
				add(c)
			}
		}
		for _, c := range s.trailingMacros(i) {
			add(c)
		}
	}
	if language == "c" {
		out = append(out, findCCandidates(src, toks)...)
	}
	sort.Slice(out, func(a, b int) bool { return preferRewrite(out[a], out[b]) })
	kept := out[:0]
	for _, c := range out {
		if len(kept) > 0 && c.start < kept[len(kept)-1].end {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

type candScan struct {
	src  []byte
	toks []ctok
	cpp  bool
	objc bool
}

func (s *candScan) word(i int) string {
	if i < 0 || i >= len(s.toks) || s.toks[i].kind != ctIdent {
		return ""
	}
	return s.toks[i].text(s.src)
}

func (s *candScan) punct(i int, c byte) bool {
	return i >= 0 && i < len(s.toks) && s.toks[i].kind == ctPunct && s.toks[i].c == c
}

// skipParens returns the index after the parenthesised list that opens at i,
// or -1 when it does not close within a sane distance.
func (s *candScan) skipParens(i int) int {
	depth := 0
	for j := i; j < len(s.toks) && j < i+400; j++ {
		t := s.toks[j]
		if t.kind == ctDir {
			return -1
		}
		if t.kind != ctPunct {
			continue
		}
		switch t.c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j + 1
			}
		case ';', '{', '}':
			return -1
		}
	}
	return -1
}

// macroAt returns the macro written at token i, with its argument list when it
// has one, and the index of the token after it.
func (s *candScan) macroAt(i int) (macroCandidate, int, bool) {
	w := s.word(i)
	if !isMacroName(w) {
		return macroCandidate{expect: -1}, i, false
	}
	t := s.toks[i]
	c := macroCandidate{start: t.start, end: t.end, name: w, expect: -1}
	next := i + 1
	if s.punct(next, '(') {
		after := s.skipParens(next)
		if after < 0 {
			return macroCandidate{expect: -1}, i, false
		}
		c.end = s.toks[after-1].end
		next = after
	}
	return c, next, true
}

// typeKeyword handles `class MACRO Name`, `struct MACRO(args) Name`,
// `enum class MACRO Name` and `class Name MACRO {`.
func (s *candScan) typeKeyword(i int, kw string) []macroCandidate {
	j := i + 1
	if kw == "enum" {
		if w := s.word(j); w == "class" || w == "struct" {
			j++
		}
	}
	var found []macroCandidate
	for {
		c, next, ok := s.macroAt(j)
		if !ok || s.word(next) == "" {
			break
		}
		// MACRO followed by a word: the macro, and then the name or a second
		// macro.
		found = append(found, c)
		j = next
	}
	name := s.word(j)
	if name == "" || notDeclaration[name] || cStorage[name] {
		return nil
	}
	after := j + 1
	if len(found) == 0 {
		// `class Name MACRO {`: the macro after the name.
		c, next, ok := s.macroAt(after)
		if ok && !isMacroName(name) && (s.punct(next, '{') || s.punct(next, ':') && !s.punct(next+1, ':')) {
			return []macroCandidate{c}
		}
		return nil
	}
	expect := -1
	switch {
	case s.punct(after, '{'), s.punct(after, ':') && !s.punct(after+1, ':'), s.word(after) == "final":
		expect = s.toks[j].start
	case s.punct(after, ';') && s.cpp && (kw == "class" || kw == "struct"):
	default:
		return nil
	}
	for i := range found {
		found[i].expect = expect
	}
	return found
}

// atDeclStart reports whether token i can begin a declaration: it is the first
// token, or follows `;`, `{`, `}`, a preprocessor line or an access label.
func (s *candScan) atDeclStart(i int) bool {
	if i == 0 {
		return true
	}
	p := s.toks[i-1]
	if s.objc && s.word(i-1) == "end" && s.punct(i-2, '@') {
		// `@end` closes an Objective-C container and a declaration may follow.
		return true
	}
	switch p.kind {
	case ctDir:
		return true
	case ctPunct:
		switch p.c {
		case ';', '{', '}':
			return true
		case ':':
			// `public:` and `private:`, not the second half of `a ? b : c`.
			w := s.word(i - 2)
			return w == "public" || w == "private" || w == "protected"
		}
	}
	return false
}

// declMacros handles a macro in front of a function declaration or definition:
//
//	MACRO void f(        MACRO(type) f(        static MACRO("x") int f(
//
// and a macro standing alone on its line in front of a directive or an
// Objective-C declaration (`G_BEGIN_DECLS`, `NS_ASSUME_NONNULL_BEGIN`).
func (s *candScan) declMacros(i int) []macroCandidate {
	j := i
	for cStorage[s.word(j)] {
		j++
	}
	var found []macroCandidate
	hasParen := false
	lastFrom := j
	for {
		c, next, ok := s.macroAt(j)
		if !ok {
			break
		}
		if next-1 > j && s.toks[next-1].kind == ctPunct && s.toks[next-1].c == ')' {
			hasParen = true
		}
		// A macro CALL statement is not a candidate: a word must follow, or
		// the macro must stand alone on its line in front of a directive.
		if s.word(next) == "" && !s.aloneOnLine(j, next) {
			break
		}
		found = append(found, c)
		lastFrom = j
		j = next
	}
	if len(found) == 0 && s.word(j) == "" {
		return nil
	}
	if len(found) > 0 && s.word(j) == "" {
		// The run ended on a macro alone on its line, in front of a directive,
		// an Objective-C `@` declaration or a closing brace.
		if s.aloneOnLine(lastFrom, j) {
			if name := s.objcDeclName(j); name >= 0 {
				for i := range found {
					found[i].expect = name
				}
			}
			return found
		}
		return nil
	}
	// A lone macro on a line of its own in front of the next declaration
	// (`FMT_BEGIN_NAMESPACE`, `G_BEGIN_DECLS`): it takes no arguments, nothing
	// else is on its line, and the next token starts a later one.
	if len(found) == 1 && !hasParen && j < len(s.toks) && bytes.IndexByte(s.src[s.toks[j-1].end:s.toks[j].start], '\n') >= 0 &&
		(lastFrom == 0 || bytes.IndexByte(s.src[s.toks[lastFrom-1].end:s.toks[lastFrom].start], '\n') >= 0) {
		return found
	}
	if notDeclaration[s.word(j)] {
		return nil
	}
	// What follows must be a function declarator: words, pointers, references,
	// scopes and template arguments, then `(`.
	words := 0
	k := j
loop:
	for k < len(s.toks) {
		t := s.toks[k]
		switch {
		case t.kind == ctIdent:
			// A macro in the middle of the head, between the return type and
			// the name (`void *JEMALLOC_ATTR(malloc) je_malloc(`): one that is
			// followed by more of the head, never the name in front of `(`.
			// and never the first word, which is a type (`DFA* Prog::Get(`).
			if c, next, ok := s.macroAt(k); ok && words > 0 && (s.word(next) != "" || s.punct(next, '*') || s.punct(next, '&')) {
				found = append(found, c)
				if next-1 > k {
					hasParen = true
				}
				k = next
				continue
			}
			words++
			k++
		case t.kind == ctScope:
			k++
		case t.kind == ctPunct && (t.c == '*' || t.c == '&' || t.c == '~'):
			k++
		case t.kind == ctPunct && t.c == '<':
			depth := 0
			for ; k < len(s.toks) && k < j+200; k++ {
				tk := s.toks[k]
				if tk.kind == ctPunct && tk.c == '<' {
					depth++
				} else if tk.kind == ctPunct && tk.c == '>' {
					depth--
					if depth == 0 {
						k++
						break
					}
				} else if tk.kind == ctPunct && (tk.c == ';' || tk.c == '{' || tk.c == '}') || tk.kind == ctDir {
					return nil
				}
			}
		case t.kind == ctPunct && t.c == '(':
			break loop
		default:
			return nil
		}
	}
	if k >= len(s.toks) || !s.punct(k, '(') {
		return nil
	}
	// `MACRO name(`: one word is the return type's absence; that is a call or
	// a constructor-like macro use, unless the macro took arguments of its own
	// (`MACRO(type) name(`).
	if len(found) == 0 || words < 2 && !hasParen {
		return nil
	}
	if name := k - 1; s.toks[name].kind == ctIdent && s.definesBody(k) {
		for i := range found {
			found[i].expect = s.toks[name].start
		}
	}
	return found
}

// trailingKeywords may stand between a parameter list and an annotation macro.
var trailingKeywords = map[string]bool{
	"const": true, "noexcept": true, "override": true, "final": true, "volatile": true,
}

// controlWords are followed by a parenthesised condition, not a parameter list.
var controlWords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true,
	"sizeof": true, "defined": true, "alignof": true, "decltype": true, "typeof": true,
}

// trailingMacros handles an annotation written after the parameter list:
//
//	void Compact() LOCKS_EXCLUDED(mutex_) {      int f(int a) NORETURN;
//
// Left alone the grammar reads `LOCKS_EXCLUDED(mutex_)` as the function's name.
// The list it follows must belong to a function: an identifier in front of its
// opening parenthesis that is not a control word and is not written as a macro.
func (s *candScan) trailingMacros(i int) []macroCandidate {
	if s.toks[i].kind != ctIdent || i == 0 {
		return nil
	}
	j := i - 1
	for trailingKeywords[s.word(j)] {
		j--
	}
	if !s.punct(j, ')') {
		return nil
	}
	open := j
	for depth := 0; open >= 0 && open > j-400; open-- {
		if s.toks[open].kind == ctDir {
			return nil
		}
		if s.punct(open, ')') {
			depth++
		} else if s.punct(open, '(') {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if open <= 0 || !s.punct(open, '(') {
		return nil
	}
	name := s.word(open - 1)
	if name == "" || controlWords[name] || notDeclaration[name] || isMacroName(name) {
		return nil
	}
	var found []macroCandidate
	k := i
	for {
		c, next, ok := s.macroAt(k)
		if !ok {
			break
		}
		found = append(found, c)
		k = next
	}
	if len(found) == 0 {
		return nil
	}
	switch {
	case s.punct(k, '{'):
		for n := range found {
			found[n].expect = s.toks[open-1].start
		}
	case s.punct(k, ';'):
	case s.punct(k, ':') && !s.punct(k+1, ':') && s.cpp:
		// A constructor's member initialiser list follows the annotation:
		//   MutexLock(Mutex& m) ACQUIRE(m) : ml_(m.native_handle()) {}
	default:
		return nil
	}
	return found
}

// definesBody reports whether the parameter list that opens at token i is
// followed by a function body rather than `;`.
func (s *candScan) definesBody(i int) bool {
	after := s.skipParens(i)
	if after < 0 {
		return false
	}
	for j := after; j < len(s.toks) && j < after+40; j++ {
		t := s.toks[j]
		if t.kind == ctPunct {
			switch t.c {
			case '{':
				return true
			case ';', '=', ',', '}':
				return false
			}
		}
		if t.kind == ctDir {
			return false
		}
	}
	return false
}

// objcDeclName returns the byte offset of the name an Objective-C container
// declaration at token i defines, or -1 when token i does not start one or it
// defines no name that a symbol would be recorded under.
//
//	@interface Name : Super      @protocol Name <Base>      @implementation Name
//	@interface Name (Category)   defines Category
//
// It lets a macro that stands alone on its line in front of such a declaration
// (NS_ASSUME_NONNULL_BEGIN) say what the plain parse should have found: the
// grammar reads the macro as a type and the "@" after it as an error, and
// recovers by losing the whole declaration without an error node touching the
// macro itself. A plain parse that has no definition at that offset is in
// trouble, and the guard decides whether the masked one is better.
func (s *candScan) objcDeclName(i int) int {
	if !s.objc || !s.punct(i, '@') {
		return -1
	}
	switch s.word(i + 1) {
	case "interface", "implementation", "protocol":
	default:
		return -1
	}
	name := i + 2
	if s.word(name) == "" {
		return -1
	}
	switch {
	case s.punct(name+1, '('):
		// A category is recorded under its own name; an extension, `()`, has none.
		if s.word(name+2) != "" {
			return s.toks[name+2].start
		}
		return -1
	case s.punct(name+1, ';'):
		// `@protocol Name;` is a forward declaration.
		return -1
	}
	return s.toks[name].start
}

// aloneOnLine reports whether the macro occupying tokens [from, to) has
// nothing in front of it and nothing behind it on its lines, and the token
// after it is a directive, `@` or `}`; in Objective-C the end of the file
// counts too.
func (s *candScan) aloneOnLine(from, to int) bool {
	if s.objc && to == len(s.toks) && to > from {
		// NS_ASSUME_NONNULL_END as the last thing in an Objective-C header.
		return from == 0 || bytes.IndexByte(s.src[s.toks[from-1].end:s.toks[from].start], '\n') >= 0
	}
	if to >= len(s.toks) || to <= from {
		return false
	}
	next := s.toks[to]
	if next.kind != ctDir && (next.kind != ctPunct || next.c != '@' && next.c != '}') {
		return false
	}
	if bytes.IndexByte(s.src[s.toks[to-1].end:next.start], '\n') < 0 {
		return false
	}
	return from == 0 || bytes.IndexByte(s.src[s.toks[from-1].end:s.toks[from].start], '\n') >= 0
}

// maskCandidates returns a copy of src with each candidate overwritten by
// spaces. Newlines are kept and no byte is added or removed, so every offset
// and line number is unchanged.
func maskCandidates(src []byte, cands []macroCandidate) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	for _, c := range cands {
		if len(c.repl) == c.end-c.start && c.end <= len(out) {
			copy(out[c.start:c.end], c.repl)
			continue
		}
		for i := c.start; i < c.end && i < len(out); i++ {
			if out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
			}
		}
	}
	return out
}

// maskRetryErrorLimit is, for a language whose only retry is a spanMask, the
// plain parse's error count from which the retry is not made.
//
// MEASURED on 65 Swift interface files (the macOS SDK's, 1 KB to 250 KB): the
// retry ran for 8 files and cost 15.7 s of 66 s of parsing, 24%. The five
// files with 100 or more error nodes (181 to 2,014) took 15.5 s of it (parse
// time is superlinear in Swift) and the retry bought nothing: the accepted
// ones went from 885 to 883, 208 to 207 and 189 to 186 errors, a rejected one
// from 181 to 190, and every file was partial before and after. The three
// below 100 errors cost 0.2 s and fixed a third to a half of theirs (72 to 32,
// 55 to 33). C and C++ are not in the table: their macro mask turns hundreds
// of errors into one (491 to 1, 193 to 1, 119 to 0 in PostgreSQL), so the
// count says nothing there.
var maskRetryErrorLimit = map[string]int{"swift": 100}

// maskRetry is one retry's outcome, for maskRetryObserver.
type maskRetry struct {
	Language     string
	SourceBytes  int
	PlainErrors  int
	MaskedErrors int
	Took         time.Duration
	Outcome      string
	Early        bool
}

// maskRetryObserver, when set (tests, measurements), is told how every masked
// retry ended.
var maskRetryObserver func(maskRetry)

// maskedParse is the result of the guarded second parse.
type maskedParse struct {
	by   *genericParser
	tree *ts.Tree
	stop stopInfo
	src  []byte
	tags []ts.Tag
	// implementers belongs to tags: positions in one are positions in the
	// other, so a retry that replaces the tags replaces it too.
	implementers map[uint32]string
	// count is how many macro tokens were overwritten, branches how many
	// conditional branches were blanked.
	count    int
	branches int
	// constructs is how many spans of a language's own construct were
	// overwritten (see spanMask).
	constructs int
	// lost is how many definitions of the plain parse were in blanked
	// branches and have no counterpart in the masked parse: given up by
	// design.
	lost int
}

// unclean reports whether the masked parse still has a syntax error or was
// itself stopped early: a reading that is better than a parse cut short, and
// not as good as one that read the file.
func (m *maskedParse) unclean() bool {
	if m.stop.early {
		return true
	}
	root := m.tree.RootNode()
	return root != nil && root.HasError()
}

// retryMasked is the guarded second parse. plain is the parse already done;
// it returns nil when the plain result stands, which is the answer for every
// file with no candidate and for every file whose plain parse is clean where
// the candidates are.
//
// It parses at most once, on the parser that produced the plain tree, under
// the same work limits and cancellation as any parse: the caller must look at
// ctx afterwards, since a cancelled parse returns nil here.
func (p *genericParser) retryMasked(ctx context.Context, tree *ts.Tree, stop stopInfo, src []byte, tags []ts.Tag) *maskedParse {
	return p.retryMaskedWithin(ctx, tree, stop, src, tags, p.cfg.budgetMicros(p.entry.Name, len(src)))
}

// retryMaskedWithin is retryMasked with the wall-clock allowance of the
// retry's parse given in microseconds.
//
// THE TWO MASKS ARE ONE RETRY. The macro mask (this file) and the conditional
// mask (cond.go) are decided independently, each by its own trouble test on
// the plain tree, and whichever are due are applied together to one masked
// text that is parsed once. That keeps the bound: a file is parsed at most
// twice by the two mechanisms in total, however many of them it needs. They do
// not interfere: the conditional mask only blanks text (a macro candidate
// inside a blanked branch is already spaces), and the macro mask only blanks
// tokens. The cost is that the guard judges the two together: if one of them
// would have been refused alone, it can pull the other down with it. The
// alternative, a retry each, would parse a file three times and would still
// not let the second mask see the first one's result.
//
// A plain parse that stopped early (a work limit, the clock, or the parser
// giving up) is retried too, by the same rules: a conditional group that cuts
// a declaration is the usual reason the parser gives up, and the masked text
// either reads further or it does not. Only cancellation is never retried.
func (p *genericParser) retryMaskedWithin(ctx context.Context, tree *ts.Tree, stop stopInfo, src []byte, tags []ts.Tag, micros uint64) *maskedParse {
	lang := p.entry.Name
	macroOK, condOK := maskedLanguages[lang], condLanguages[lang]
	sm := spanMasks[lang]
	if !macroOK && !condOK && sm.find == nil || ctx.Err() != nil || stop.early && stop.reason == ts.ParseStopCancelled {
		return nil
	}
	root := tree.RootNode()

	// The macro mask: candidates by text, and trouble where one sits. An
	// early stop leaves the rest of the file unread, so a candidate there
	// cannot be called clean.
	var cands []macroCandidate
	names := map[string]bool{}
	macroDue := false
	if macroOK {
		cands = findMacroCandidates(src, lang)
		for _, c := range cands {
			names[c.name] = true
		}
		macroDue = len(cands) > 0 && (stop.early || troubleAtCandidates(root, cands, tags, names))
	}

	// The conditional mask: groups by text, and error nodes on or next to
	// their directives. Only a tree with errors, or one that stopped, is
	// scanned.
	var plan *condPlan
	if condOK && root != nil && (stop.early || root.HasError()) {
		if dirs := scanCondDirectives(src, lang == "c_sharp"); len(dirs) > 0 {
			errs := leafErrorSpans(root)
			if stop.early {
				// Everything from a little before the stop to the end was
				// not read; groups there may be the reason.
				errs = append(errs, condSpan{max(0, stop.parsedBytes-4096), len(src)})
			}
			plan = planCondMask(src, buildCondGroups(dirs), errs)
		}
	}
	condDue := !plan.empty()

	// A language's own construct (see spanMask): spans by text, and
	// trouble on or after the first one. Only a tree with errors, or one that
	// stopped, is looked at.
	var spans []maskSpan
	if sm.find != nil && root != nil && (stop.early || root.HasError() || sm.silent) {
		spans = sm.find(src)
		if len(spans) == 0 || !stop.early && !sm.silent && !troubleFrom(root, spans[0].start) {
			spans = nil
		}
	}
	if !macroDue && !condDue && len(spans) == 0 {
		return nil
	}
	if limit, ok := maskRetryErrorLimit[lang]; ok && !stop.early && len(spans) > 0 && !macroDue && !condDue {
		// A retry that only blanks a construct the grammar misreads cannot
		// repair a file whose errors are elsewhere; see maskRetryErrorLimit.
		if e, m, _ := countSyntaxErrors(root); e+m >= limit {
			return nil
		}
	}

	masked := src
	if condDue {
		masked = plan.apply(masked)
	}
	if len(spans) > 0 {
		masked = applyMaskSpans(masked, spans)
	}
	macros := 0
	if macroDue {
		masked = maskCandidates(masked, cands)
		for _, c := range cands {
			if !plan.inBlanked(c.start) {
				macros++
			}
		}
	} else {
		names = map[string]bool{}
	}

	began := time.Now()
	tree2, stop2, err := p.parseOnceWithin(ctx, masked, micros)
	stop2 = stop2.settled(len(masked))
	if err != nil || tree2 == nil || ctx.Err() != nil || stop2.early && !stop.early {
		if maskRetryObserver != nil {
			maskRetryObserver(maskRetry{Language: lang, SourceBytes: len(src), Took: time.Since(began), Outcome: "unparsed"})
		}
		return nil
	}
	tags2, implementers2, err := p.tagTree(tree2, masked)
	if err != nil {
		return nil
	}
	var mc *maskContext
	if condDue || stop.early || len(spans) > 0 || macroDue && lang == "c" {
		// C's macro mask is judged with the same extra rules as the
		// conditional one: a kernel file whose plain parse is one error node
		// that swallowed most of it is not compared with a masked parse by
		// the number of error nodes alone (see cmacro_c.go).
		// A plain parse that stopped early is judged by how far each parse
		// got, whichever mask was applied; see maskGuard.
		mc = &maskContext{
			plan: plan, plainErrs: nearErrorSpans(src, root), language: lang,
			plainEarly: stop.early, maskedEarly: stop2.early,
			plainParsed: stop.parsedBytes, maskedParsed: stop2.parsedBytes,
		}
		for _, sp := range spans {
			if sp.drop {
				mc.dropped = append(mc.dropped, condSpan{sp.start, sp.start + len(sp.repl)})
			}
		}
		if len(spans) > 0 && sm.swallows && !stop.early {
			// The plain parse reached the end of the file, but by reading
			// everything after the first construct as part of it: that is a
			// parse which stopped there, whatever the parser reported.
			mc.plainEarly, mc.swallowed, mc.plainParsed = true, true, spans[0].start
		}
	}
	accepted := maskGuard(root, tags, tree2.RootNode(), tags2, names, mc)
	if maskRetryObserver != nil {
		pe, pm, _ := countSyntaxErrors(root)
		me, mm, _ := countSyntaxErrors(tree2.RootNode())
		o := "rejected"
		if accepted {
			o = "accepted"
		}
		maskRetryObserver(maskRetry{Language: lang, SourceBytes: len(src), PlainErrors: pe + pm, MaskedErrors: me + mm,
			Took: time.Since(began), Outcome: o, Early: stop.early})
	}
	if !accepted {
		// The masked tree lost: hand its arena back now, not at the next collection.
		tree2.Release()
		return nil
	}
	branches := 0
	if condDue {
		branches = plan.branches
	}
	lost := 0
	if mc != nil {
		lost = mc.givenUp
	}
	return &maskedParse{by: p, tree: tree2, stop: stop2, src: masked, tags: tags2, implementers: implementers2, count: macros, branches: branches, constructs: len(spans), lost: lost}
}

// maskSpan is one stretch of source a language's own finder wants overwritten:
// repl replaces the bytes [start, start+len(repl)). It must be exactly as long
// as the text it replaces and keep its newlines, so that every offset and line
// number is unchanged; applyMaskSpans skips a span that does not fit the text.
//
// drop marks text whose definitions are not real: the plain parse reads the
// local signatures of a where block as top-level functions, and the masked
// parse rightly does not have them. The guard does not hold the mask to
// account for a definition that starts inside such a span, and a mask that
// removed one is a better reading even when nothing was gained.
type maskSpan struct {
	start int
	repl  []byte
	drop  bool
}

// spanMask is a language's own construct that the grammar cannot read and that
// costs the file after it.
//
// find reads the source by text and returns the spans to overwrite
// (mask_fsharp.go, mask_groovy.go). It must leave comments and strings alone,
// return spans in source order that do not overlap, and return nil for a file
// with nothing to mask.
//
// swallows is set for a construct that, misread, does not stop the parser but
// turns the text after it into part of itself (a doc comment that is never
// closed): the plain parse then reaches the end of the file with an error
// count that only describes the text before the construct, so the guard
// judges it as a parse that stopped at the first span.
//
// silent is set for a construct the grammar misreads without an error node
// (a right-hand side that takes in the lines after it): find is then asked
// for spans whether or not the plain tree has errors, so it must return
// nothing for a file the mask would not change, or every file of the language
// is parsed twice.
type spanMask struct {
	find     func(src []byte) []maskSpan
	swallows bool
	silent   bool
}

// spanMasks ride on the retry above and share its guard, so a file is still
// parsed at most twice by these mechanisms in total, and the masked result
// replaces the plain one only if maskGuard accepts it. A language with a
// spanMask has no macro or conditional mask, so this is its only retry.
var spanMasks = map[string]spanMask{
	"fsharp": {find: findFSharpConstraintCalls},
	"groovy": {find: findGroovyMasks, swallows: true},
	"swift":  {find: findSwiftTypeAttributes},
	"make":   {find: findMakeEmptySubstitutions},
	"haxe":   {find: findHaxeConstructs},
	"agda":   {find: findAgdaConstructs, silent: true},
}

// applyMaskSpans returns a copy of src with the spans overwritten.
func applyMaskSpans(src []byte, spans []maskSpan) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	for _, s := range spans {
		if s.start < 0 || s.start+len(s.repl) > len(out) {
			continue
		}
		copy(out[s.start:], s.repl)
	}
	return out
}

// troubleFrom reports whether the plain tree has an error or missing node that
// ends at or after pos: where the parser gave up is on or beyond the first
// span of a construct it is known to misread.
func troubleFrom(root *ts.Node, pos int) bool {
	found := false
	walkErrors(root, func(n *ts.Node) bool {
		if int(n.EndByte()) >= pos {
			found = true
			return false
		}
		return true
	})
	return found
}

// errorSpans lists the byte ranges of the error and missing nodes of a tree.
func errorSpans(root *ts.Node) []condSpan {
	var out []condSpan
	walkErrors(root, func(n *ts.Node) bool {
		out = append(out, condSpan{int(n.StartByte()), int(n.EndByte())})
		return true
	})
	return out
}

// leafErrorSpans lists the byte ranges of the error and missing nodes that
// have no error inside them: where the parser actually gave up, as opposed to
// the nodes that merely enclose that place. The conditional mask judges groups
// by these, so a chain of twelve nested error nodes that all start at line 1
// does not make every directive of the file look guilty.
func leafErrorSpans(root *ts.Node) []condSpan {
	var out []condSpan
	walkErrors(root, func(n *ts.Node) bool {
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c != nil && (c.IsError() || c.IsMissing() || c.HasError()) {
				return true
			}
		}
		out = append(out, condSpan{int(n.StartByte()), int(n.EndByte())})
		return true
	})
	return out
}

// maskContext carries what the guard needs to judge a conditional mask: where
// the plain parse was unreliable, where text was blanked, and how far each
// parse got when one of them stopped early. A nil context is the macro mask
// alone, whose guard is unchanged.
type maskContext struct {
	plan         *condPlan
	plainErrs    []condSpan
	language     string
	plainEarly   bool
	maskedEarly  bool
	plainParsed  int
	maskedParsed int
	// swallowed marks a plain parse that reached the end of the file but read
	// everything after plainParsed as part of one construct (see spanMask):
	// plainEarly is set with it, and the masked parse must also have gained
	// definitions, since reading further is not a gain when nothing was
	// unread.
	swallowed bool
	// givenUp is set by maskGuard: see maskedParse.lost.
	givenUp int
	// dropped are the byte ranges of spans marked drop (see maskSpan).
	dropped []condSpan
}

// inDropped reports whether byte offset pos lies in a span marked drop.
func (mc *maskContext) inDropped(pos int) bool {
	for _, s := range mc.dropped {
		if pos >= s.start && pos < s.end {
			return true
		}
	}
	return false
}

// nearErrorSpans lists the error and missing nodes of a tree, each widened by
// the line before and the line after. A parser that recovers from an error
// leaves wrong definitions beside it as often as inside it: the parameter
// `b` of a function whose parameter list was cut by an `#if` is recorded as
// a function called `b`, on the line after the error.
func nearErrorSpans(src []byte, root *ts.Node) []condSpan {
	spans := errorSpans(root)
	for i, s := range spans {
		lo, hi := lineBounds(src, s.start, s.end)
		spans[i] = condSpan{lo, hi}
	}
	return spans
}

// inError reports whether a definition of the plain parse was read inside or
// next to an error node, where the name is as likely to be wrong as right.
func (mc *maskContext) inError(t ts.Tag) bool {
	pos := int(t.NameRange.StartByte)
	for _, e := range mc.plainErrs {
		if e.start <= pos && pos < e.end {
			return true
		}
	}
	return false
}

// maskKeywordNames are words that are never the name of a definition in a
// masked parse. A symbol carrying one is a misreading of a declaration the
// mask cut wrongly, so the guard refuses the mask. The list is wider than
// keywordNames (keywords.go), which is applied to every parse and so holds
// only words that are reserved in every position.
var maskKeywordNames = map[string]map[string]bool{
	"c_sharp": wordSet("abstract as base bool break case catch checked class const continue default delegate do else enum event explicit extern false finally fixed for foreach goto if implicit in interface internal is lock namespace new null object operator out override params private protected public readonly ref return sealed sizeof stackalloc static struct switch this throw true try typeof unchecked unsafe using virtual void volatile while"),
	"c":       cKeywords,
	"cpp":     cKeywords,
	"objc":    cKeywords,
}

var cKeywords = wordSet("auto break case char const continue default do double else enum extern float for goto if inline int long register restrict return short signed sizeof static struct switch typedef union unsigned void volatile while class namespace template typename public private protected virtual new delete this try catch throw using operator")

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(s) {
		out[w] = true
	}
	return out
}

// troubleAtCandidates reports whether the plain parse shows trouble where a
// candidate sits: an error or missing node overlapping one, or a definition
// named after one.
func troubleAtCandidates(root *ts.Node, cands []macroCandidate, tags []ts.Tag, names map[string]bool) bool {
	for _, t := range tags {
		if len(t.Kind) > len("definition") && t.Kind[:len("definition")] == "definition" && t.Kind != cMacroDefinitionTag && names[t.Name] {
			return true
		}
	}
	defined := make(map[int]bool, len(tags))
	for _, t := range tags {
		if isDefinitionTag(t.Kind) {
			defined[int(t.NameRange.StartByte)] = true
		}
	}
	for _, c := range cands {
		if c.expect >= 0 && !defined[c.expect] {
			return true
		}
	}
	if root == nil || !root.HasError() {
		return false
	}
	found := false
	walkErrors(root, func(n *ts.Node) bool {
		a, b := int(n.StartByte()), int(n.EndByte())
		for _, c := range cands {
			if c.start < b && a < c.end || a == b && a >= c.start && a <= c.end {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// walkErrors visits every error and missing node, entering only subtrees the
// parser flagged. visit returns false to stop.
func walkErrors(root *ts.Node, visit func(*ts.Node) bool) {
	stack := []*ts.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		if (n.IsError() || n.IsMissing()) && !visit(n) {
			return
		}
		if n.IsMissing() || !n.HasError() {
			continue
		}
		for i := n.ChildCount() - 1; i >= 0; i-- {
			if c := n.Child(i); c != nil && (c.HasError() || c.IsError() || c.IsMissing()) {
				stack = append(stack, c)
			}
		}
	}
}

// maskIsBetter is the guard for the macro mask alone; see maskGuard.
func maskIsBetter(plainRoot *ts.Node, plain []ts.Tag, maskedRoot *ts.Node, masked []ts.Tag, names map[string]bool) bool {
	return maskGuard(plainRoot, plain, maskedRoot, masked, names, nil)
}

// maskGuard is the guard. The masked parse replaces the plain one only when
// it is measurably better and no worse: no more error nodes (a missing token
// counts as one) and no fewer definitions, with at least one of the two
// strictly better; no definition named after a masked token; and every
// definition the plain parse had that was not itself named after a masked
// token still there.
//
// For the conditional mask (mc is not nil) the same rule holds with four
// additions, each because that mask removes text:
//
//   - a plain definition that was read inside an error node, or that sits in a
//     branch which was blanked on purpose, is not one the mask can lose: the
//     first is as likely wrong as right, the second is gone by design. Such
//     definitions are neither required to survive nor counted;
//   - no definition may be named after a keyword of the language;
//   - more error nodes than the plain parse are tolerated when the text under
//     error nodes, in bytes, shrank and the definitions strictly grew: one
//     error node that swallowed a file and several small ones are not the
//     same amount of trouble;
//   - a plain parse that stopped early cannot be compared by its error count,
//     which only reflects the part it read; the masked parse must have read
//     further (to the end, or past where the plain one stopped), and every
//     reliable definition must still be there.
func maskGuard(plainRoot *ts.Node, plain []ts.Tag, maskedRoot *ts.Node, masked []ts.Tag, names map[string]bool, mc *maskContext) bool {
	pe, pm, _ := countSyntaxErrors(plainRoot)
	me, mm, _ := countSyntaxErrors(maskedRoot)
	plainErrs, maskedErrs := pe+pm, me+mm
	better, moreDefs := false, false
	switch {
	case mc != nil && mc.plainEarly:
		if mc.maskedEarly && mc.maskedParsed <= mc.plainParsed {
			return false
		}
		better = !mc.swallowed
	default:
		if maskedErrs > plainErrs {
			// More error nodes can still be a better reading, when one big
			// error that swallowed the text became several small ones: the
			// conditional mask accepts that, and only if the text under
			// errors shrank and the definitions grew (checked below).
			if mc == nil || errorBytes(maskedRoot) >= errorBytes(plainRoot) && (mc.language != "c" || !grewMuch(plain, masked)) {
				return false
			}
			moreDefs = true
		}
		better = maskedErrs < plainErrs
	}
	var keywords map[string]bool
	if mc != nil {
		keywords = maskKeywordNames[mc.language]
	}
	have := make(map[string]int, len(masked))
	nMasked := 0
	for _, t := range masked {
		if !isDefinitionTag(t.Kind) {
			continue
		}
		if t.Kind != cMacroDefinitionTag && names[t.Name] || keywords[t.Name] {
			return false
		}
		have[t.Name]++
		nMasked++
	}
	// A plain definition read inside an error node is unreliable only when
	// the masked parse is a much better reading (at most half the error
	// nodes) of a parse that ran to the end: then the names in the damaged
	// regions were as likely wrong as right. Otherwise it is held to account
	// like any other.
	strong := mc != nil && !mc.plainEarly && maskedErrs*2 <= plainErrs
	nPlain, givenUp, droppedN := 0, 0, 0
	offset := map[string]int{}
	for _, t := range plain {
		if !isDefinitionTag(t.Kind) {
			continue
		}
		if mc != nil && mc.inDropped(int(t.NameRange.StartByte)) {
			droppedN++
			continue
		}
		if t.Kind != cMacroDefinitionTag && names[t.Name] {
			// A definition named after a masked token is the macro mistaken
			// for a name; it is not one the mask can lose.
			continue
		}
		if keywords[t.Name] {
			// `int` as the name of a function is the parser's mistake, not a
			// definition the mask can lose.
			continue
		}
		if mc != nil {
			if mc.plan.inBlanked(int(t.NameRange.StartByte)) {
				// Given up on purpose: another build's code. It has to be
				// paid for by what the mask gained, below, and a name that
				// survives elsewhere (the same function in the other
				// branch) is not given up at all. It counts even when an
				// error node held it: a branch that is most of a file is in
				// an error node as often as not.
				// A name offsets as many given-up definitions as the masked
				// parse has definitions of it: twenty-two `TEST` in a branch
				// that was blanked are not saved by one `TEST` outside it.
				if offset[t.Name] < have[t.Name] {
					offset[t.Name]++
				} else {
					givenUp++
				}
				continue
			}
			if strong && mc.inError(t) {
				continue
			}
		}
		nPlain++
		if have[t.Name] == 0 {
			return false
		}
	}
	if mc != nil {
		mc.givenUp = givenUp
	}
	if nMasked < nPlain+givenUp || moreDefs && nMasked <= nPlain {
		return false
	}
	return better || nMasked > nPlain || droppedN > 0
}

// grewMuch reports whether the masked parse has at least half as many
// definitions again as the plain one, and at least five more. A plain parse can
// read most of a file as part of one construct without an error node at all
// (a `__printf(2, 3)` in front of a function leaves the parser reading
// everything after it as one declaration); the masked parse then has more
// error nodes of its own, each small, and many more definitions. For C only:
// the guard's other rules still apply, in particular that no definition the
// plain parse had is lost.
func grewMuch(plain, masked []ts.Tag) bool {
	np, nm := 0, 0
	for _, t := range plain {
		if isDefinitionTag(t.Kind) {
			np++
		}
	}
	for _, t := range masked {
		if isDefinitionTag(t.Kind) {
			nm++
		}
	}
	return nm >= np+5 && nm*2 >= np*3
}

// errorBytes is the number of bytes under the innermost error nodes of a tree:
// how much text the parser actually gave up on. The outer error nodes that
// merely enclose them are not counted; they can span the whole file while
// everything in them was read.
func errorBytes(root *ts.Node) int {
	total := 0
	for _, s := range leafErrorSpans(root) {
		total += s.end - s.start
	}
	return total
}

func isDefinitionTag(kind string) bool { return strings.HasPrefix(kind, "definition") }

// cMacroDefinitionTag is the capture of a function-like `#define` in C. It is a
// definition like any other, except to the guard's test for a definition named
// after a masked token: a file that defines `for_each_css(...)` and uses it as
// a statement is not misreading a macro as a name, it has the macro's own
// definition (see lang_c.go).
const cMacroDefinitionTag = "definition.function.cmacro"

// maskNote is the sentence appended to a parse error when text was masked, so
// a file read through a mask says so. It names what was overwritten: macro
// tokens, and the conditional branches that were blanked (a definition that
// exists only in one of them is not in the index).
func (r *Result) maskNote() string {
	var parts []string
	if r.MacrosMasked > 0 {
		parts = append(parts, plural(r.MacrosMasked, "macro token")+" masked")
	}
	if r.BranchesBlanked > 0 {
		what := "conditional branches"
		if r.BranchesBlanked == 1 {
			what = "conditional branch"
		}
		parts = append(parts, fmt.Sprintf("%d %s blanked (definitions only in %s are not indexed)", r.BranchesBlanked, what, branchPronoun(r.BranchesBlanked)))
	}
	if r.ConstructsMasked > 0 {
		parts = append(parts, plural(r.ConstructsMasked, "construct")+" masked")
	}
	if len(parts) == 0 {
		return ""
	}
	return "; read with " + strings.Join(parts, " and ")
}

func branchPronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
