package parse

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A fallback for definitions the grammar lost: line shape, inside ERROR spans.
//
// WHY. A tree-sitter grammar that meets text it cannot read wraps that text in
// an ERROR node, and the definition query matches nothing in it. One construct
// the grammar does not know can put the rest of a file inside one ERROR, so the
// file keeps a fraction of its definitions. The masks (cmacro.go, mask_*.go)
// and the per-language recovery patterns repair the constructs someone has
// diagnosed; this pass is for what is left.
//
// WHAT. Only the text inside the outermost ERROR nodes of the FINAL tree is
// read, so a mask that recovered a file wins and the pass sees only what is
// still unreadable after it. A line there is a definition when, after its
// indentation and the language's own modifiers, it starts with one of the
// language's definition keywords followed by a name and a character that may
// follow a declared name (`sub name {`, `public func name(`). The keyword must
// be a token the parser lexed outside any comment or string: the token is
// looked up in the tree, and a comment or string node above it rejects the
// line.
//
// WHICH LANGUAGES. Only those in lineShapeRules, and a language is listed only
// when both hold:
//
//   - its keywords make a definition line unambiguous. Markup, data and
//     configuration languages have no definition keyword and are never
//     listed; nor are languages whose functions have no keyword (C, C++, Java,
//     C#, Dart, Erlang), where a line shape would be a guess at a declarator;
//   - measured on real code, it recovered definitions the grammar lost, at
//     least 95% of them correct. Perl (whole files of `sub` lost to one
//     construct the grammar cannot read) and Swift (declarations after a
//     region the grammar gives up on; see fixtures/langs/swift/KNOWN_GAPS.md)
//     are. Python, Ruby, Go, Rust, JavaScript, TypeScript, Java, Kotlin, C#,
//     PHP, Lua and Bash were measured too and lost nothing a keyword line
//     would find, so the pass is not run for them: it would add risk and no
//     definition.
//
// HOW A RECOVERED DEFINITION IS MARKED. With the vocabulary the index already
// has:
//
//   - its Confidence is medium, the value model.Symbol documents for a Tier-1
//     extraction that is less than certain; every symbol a query found is
//     high. symbols.confidence is shown wherever a symbol is;
//   - the file's parse status stays partial, as describeParse set it, and its
//     parse error says how many definitions were recovered this way, so a
//     reader asking why the file is partial sees that its definitions there
//     were read by line shape and not by the grammar.
//
// RANGES. A recovered definition's end decides which calls it is the caller
// of, so it is conservative. It ends at the first of: the line before the next
// recovered definition; the line before the next definition the grammar found
// at the same or a lower indentation; the end of its ERROR span; and, for a
// language with braces, the line where its braces close if they close on the
// line it starts on, or else the first later line that starts with `}` at its
// own indentation. A call past that end is attributed to whatever encloses it
// or to the file, never to a definition that may not contain it.
//
// NO DUPLICATES. A candidate is dropped when the grammar found any definition
// starting on its line, or one with the same name that starts on its line or
// above it and either reaches it or starts at most three lines above it. A
// stack of attributes moves a declaration's start up by as many lines as it
// has (`@available` four times over a struct, in Apple's SDK), and the range
// then covers the keyword's line.

// shapeRule is one language's definition line shape.
type shapeRule struct {
	// keywords maps a definition keyword to the tag kind it declares.
	keywords map[string]string

	// modifiers may precede the keyword, in any order and any number. A word
	// that is both (Swift `class func`) is a modifier when another keyword or
	// modifier follows it.
	modifiers map[string]bool

	// attributes accepts `@name` and `@name(...)` before the keyword (Swift).
	attributes bool

	// follow lists the characters that may come right after the name, spaces
	// skipped; eolOK accepts the end of the line there too.
	follow string
	eolOK  bool

	// nameExtra are characters a name may contain besides letters, digits and
	// underscores (`:` in Perl's `Foo::bar`).
	nameExtra string

	// braces says the language delimits a body with { }, which bounds a
	// recovered range (see RANGES above).
	braces bool

	// needsBody names the keywords whose line is rejected when it ends in `;`
	// with no `{`: a forward declaration (Perl `sub name;`, `sub name ($$);`)
	// declares a name defined elsewhere, and an index that holds it twice
	// binds calls to the wrong one. Perl's `package Name;` is a definition.
	needsBody map[string]bool

	// dead marks the lines that are never code whatever the parser made of
	// them (Perl's POD and the text after __END__), for a language whose
	// grammar can lose track of them inside an ERROR.
	dead func(lines [][]byte) []bool
}

func kinds(pairs ...string) map[string]string {
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = "definition." + pairs[i+1]
	}
	return m
}

// lineShapeRules holds the languages the fallback runs for. See WHICH
// LANGUAGES above for what qualifies one, and fallback_test.go for the
// measurement that did.
var lineShapeRules = map[string]shapeRule{
	// Perl: `sub name` (with a prototype, an attribute or a body after it) and
	// `package Name`. Kinds follow fixtures/langs/perl/expected.tsv.
	"perl": {
		keywords:  kinds("sub", "function", "package", "package"),
		follow:    "{(:;",
		eolOK:     true,
		nameExtra: ":",
		braces:    true,
		needsBody: keywordSet("sub"),
		dead:      perlDeadLines,
	},
	// Swift: functions and the type declarations the grammar records as class,
	// struct, enum, actor (class) and protocol (interface). Not extension: it
	// names a type defined elsewhere, often qualified, and a guessed name is
	// worse than none. Not init, deinit or subscript: they have no name.
	"swift": {
		keywords: kinds("func", "method", "class", "class", "struct", "class", "enum", "class",
			"actor", "class", "protocol", "interface"),
		modifiers: keywordSet("public private fileprivate internal open package final static class " +
			"override mutating nonmutating convenience required indirect nonisolated dynamic " +
			"prefix postfix infix optional distributed"),
		attributes: true,
		follow:     "(<{:",
		eolOK:      true,
		braces:     true,
	},
}

// errorSpan is one outermost ERROR node of the final tree, by byte offset.
type errorSpan struct{ start, end int }

// outermostErrors returns the ERROR nodes of the tree that no other ERROR
// contains, in source order. Only subtrees the parser flagged are entered.
func outermostErrors(root *ts.Node) []errorSpan {
	var out []errorSpan
	stack := []*ts.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		if n.IsError() {
			out = append(out, errorSpan{int(n.StartByte()), int(n.EndByte())})
			continue
		}
		if !n.HasError() {
			continue
		}
		for i := n.ChildCount() - 1; i >= 0; i-- {
			if c := n.Child(i); c != nil && (c.HasError() || c.IsError()) {
				stack = append(stack, c)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// shapeCandidate is a definition line found inside an ERROR span, before it
// is checked against what the grammar found.
type shapeCandidate struct {
	name    string
	tagKind string
	line    int // 1-based
	indent  int // bytes of indentation before the line's first word
	col     int // byte column of the keyword
	endLine int // the last line it can extend to (see RANGES)
}

// lineShapeCandidates reads the ERROR spans of the final tree for definition
// lines. Nil for a language without a rule and for a tree with no ERROR.
func (p *genericParser) lineShapeCandidates(tree *ts.Tree, src []byte) []shapeCandidate {
	rule, ok := lineShapeRules[p.entry.Name]
	if !ok || tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root == nil || !root.HasError() {
		return nil
	}
	spans := outermostErrors(root)
	if len(spans) == 0 {
		return nil
	}
	lines := bytes.Split(src, []byte{'\n'})
	starts := make([]int, len(lines)+1)
	for i, l := range lines {
		starts[i+1] = starts[i] + len(l) + 1
	}
	var dead []bool
	if rule.dead != nil {
		dead = rule.dead(lines)
	}
	lineOf := func(off int) int { // 0-based
		return sort.Search(len(lines), func(i int) bool { return starts[i+1] > off })
	}
	var out []shapeCandidate
	for _, sp := range spans {
		sp.end = min(sp.end, len(src))
		if sp.start >= sp.end {
			continue
		}
		last := lineOf(sp.end - 1)
		for i := lineOf(sp.start); i <= last && i < len(lines); i++ {
			if dead != nil && dead[i] {
				continue
			}
			c, ok := matchShape(rule, lines[i])
			if !ok {
				continue
			}
			if off := starts[i] + c.col; off < sp.start || off >= sp.end || !p.lexedAsCode(root, off) {
				continue
			}
			c.line = i + 1
			c.endLine = last + 1
			if rule.braces {
				c.endLine = braceEnd(lines, i, c.indent, c.endLine)
			}
			out = append(out, c)
		}
	}
	return out
}

// matchShape reads one line for the rule's shape.
func matchShape(rule shapeRule, line []byte) (shapeCandidate, bool) {
	le := len(line)
	i := skipSpaces(line, 0, le)
	indent := i
	for {
		if rule.attributes && i < le && line[i] == '@' {
			j := skipAttribute(line, i, le)
			if j == i {
				return shapeCandidate{}, false
			}
			i = skipSpaces(line, j, le)
			continue
		}
		w, j := readIdent(line, i, le, "")
		if w == "" {
			return shapeCandidate{}, false
		}
		tag, isKeyword := rule.keywords[w]
		if isKeyword && (!rule.modifiers[w] || !followedByDeclWord(rule, line, j, le)) {
			k := skipSpaces(line, j, le)
			if k == j {
				return shapeCandidate{}, false // `sub{` or `func(`: no name
			}
			name, n := readIdent(line, k, le, rule.nameExtra)
			if name == "" || rule.keywords[name] != "" || rule.modifiers[name] || shapeReserved[name] {
				return shapeCandidate{}, false
			}
			f := skipSpaces(line, n, le)
			if f == le && !rule.eolOK || f < le && strings.IndexByte(rule.follow, line[f]) < 0 {
				return shapeCandidate{}, false
			}
			if rule.needsBody[w] && isBodyless(line[i:]) {
				return shapeCandidate{}, false
			}
			return shapeCandidate{name: name, tagKind: tag, indent: indent, col: i}, true
		}
		if !rule.modifiers[w] {
			return shapeCandidate{}, false
		}
		i = skipSpaces(line, j, le)
	}
}

// followedByDeclWord reports whether the next word is a keyword or a modifier,
// which makes the word before it a modifier (Swift `class func`).
func followedByDeclWord(rule shapeRule, line []byte, j, le int) bool {
	w, _ := readIdent(line, skipSpaces(line, j, le), le, "")
	_, kw := rule.keywords[w]
	return kw || rule.modifiers[w]
}

// shapeReserved are words that can follow a definition keyword without being
// a name.
var shapeReserved = keywordSet("extends implements of in is as with where if for while return new self super var let func")

// isBodyless reports whether a declaration line ends in `;` before any `{`,
// a trailing `#` comment aside.
func isBodyless(line []byte) bool {
	if bytes.IndexByte(line, '{') >= 0 {
		return false
	}
	if h := bytes.Index(line, []byte(" #")); h >= 0 {
		line = line[:h]
	}
	return bytes.HasSuffix(bytes.TrimRight(line, " \t\r"), []byte{';'})
}

// skipAttribute skips `@name` and `@name(...)` with balanced parentheses on
// the line. It returns i when there is no attribute there.
func skipAttribute(line []byte, i, le int) int {
	w, j := readIdent(line, i+1, le, "")
	if w == "" {
		return i
	}
	if j < le && line[j] == '(' {
		depth := 0
		for ; j < le; j++ {
			switch line[j] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return i
	}
	return j
}

// braceEnd bounds a definition starting on line i (0-based) in a language
// with braces: the line itself when its braces open and close there, else the
// first later line, up to limit (1-based), that starts with `}` at the
// definition's own indentation. The result is 1-based.
func braceEnd(lines [][]byte, i, indent, limit int) int {
	if open := bytes.Count(lines[i], []byte{'{'}); open > 0 && open == bytes.Count(lines[i], []byte{'}'}) {
		return i + 1
	}
	for k := i + 1; k < limit && k < len(lines); k++ {
		l := lines[k]
		j := skipSpaces(l, 0, len(l))
		if j == indent && j < len(l) && l[j] == '}' {
			return k + 1
		}
	}
	return limit
}

func skipSpaces(b []byte, i, le int) int {
	for i < le && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	return i
}

func isShapeNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// readIdent reads an identifier starting at i, also accepting the bytes in
// extra after its first byte. A trailing extra byte (`Foo::` before a space)
// is not part of it.
func readIdent(b []byte, i, le int, extra string) (string, int) {
	if i >= le || !isShapeNameByte(b[i]) || b[i] >= '0' && b[i] <= '9' {
		return "", i
	}
	j := i + 1
	for j < le && (isShapeNameByte(b[j]) || strings.IndexByte(extra, b[j]) >= 0) {
		j++
	}
	for j > i && strings.IndexByte(extra, b[j-1]) >= 0 {
		j--
	}
	return string(b[i:j]), j
}

// lexedAsCode reports whether the token at off is one the parser lexed as
// code: no comment or string node in the tree covers it.
func (p *genericParser) lexedAsCode(root *ts.Node, off int) bool {
	for n := root; n != nil; {
		t := n.Type(p.lang)
		if strings.Contains(t, "comment") || strings.Contains(t, "string") || strings.Contains(t, "heredoc") ||
			strings.Contains(t, "regex") || strings.Contains(t, "pod") || strings.Contains(t, "quote") {
			return false
		}
		var next *ts.Node
		for i := 0; i < n.ChildCount(); i++ {
			if c := n.Child(i); c != nil && int(c.StartByte()) <= off && off < int(c.EndByte()) {
				next = c
				break
			}
		}
		n = next
	}
	return true
}

// perlDeadLines marks POD (a line starting `=word` to the next `=cut`) and
// every line from `__END__` or `__DATA__` on. Inside an ERROR the grammar may
// not have lexed them as the documentation they are, and POD is full of
// example code: `sub increment_my_x {` in PadWalker's documentation.
func perlDeadLines(lines [][]byte) []bool {
	dead := make([]bool, len(lines))
	pod := false
	for i, l := range lines {
		switch {
		case bytes.HasPrefix(l, []byte("__END__")) || bytes.HasPrefix(l, []byte("__DATA__")):
			for k := i; k < len(lines); k++ {
				dead[k] = true
			}
			return dead
		case bytes.HasPrefix(l, []byte("=cut")):
			dead[i] = true
			pod = false
			continue
		case len(l) > 1 && l[0] == '=' && (l[1] >= 'a' && l[1] <= 'z' || l[1] >= 'A' && l[1] <= 'Z'):
			pod = true
		}
		dead[i] = pod
	}
	return dead
}

// addRecovered turns the candidates into symbols: dropped when the grammar
// already found the definition, ranged conservatively, marked medium.
func addRecovered(syms []model.Symbol, cands []shapeCandidate, src []byte) ([]model.Symbol, int) {
	kept := make([]shapeCandidate, 0, len(cands))
	for _, c := range cands {
		dup := false
		for _, s := range syms {
			if s.Range.Start.Line == c.line || s.Name == c.name && s.Range.Start.Line <= c.line &&
				(s.Range.End.Line >= c.line || s.Range.Start.Line >= c.line-3) {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return syms, 0
	}
	widths := lineLengths(src)
	added := make([]model.Symbol, 0, len(kept))
	for i, c := range kept {
		end := c.endLine
		if i+1 < len(kept) && kept[i+1].line-1 < end {
			end = kept[i+1].line - 1
		}
		for _, s := range syms {
			if s.Range.Start.Line > c.line && s.Range.Start.Line-1 < end && s.Range.Start.Col <= c.indent {
				end = s.Range.Start.Line - 1
			}
		}
		end = max(end, c.line)
		kind := kindFromTag(c.tagKind)
		if kind == model.KindMethod && !enclosedByType(syms, added, c) {
			// A function keyword declares a method only inside a type.
			kind = model.KindFunction
		}
		added = append(added, model.Symbol{
			Kind:       kind,
			Name:       c.name,
			Range:      model.Range{Start: model.Position{Line: c.line, Col: c.indent}, End: model.Position{Line: end, Col: widths[end-1]}},
			Confidence: model.ConfidenceMedium,
			Source:     model.SourceSourceCode,
			Exported:   isExported(c.name),
		})
	}
	return dedupeSymbols(append(syms, added...)), len(added)
}

// enclosedByType reports whether a function keyword declares a method: a
// type the grammar found contains its line, or the nearest recovered
// definition above it at a lower indentation is a type.
func enclosedByType(syms, added []model.Symbol, c shapeCandidate) bool {
	if c.indent == 0 {
		return false
	}
	isType := func(k model.EntityKind) bool { return k == model.KindClass || k == model.KindInterface }
	for _, s := range syms {
		if isType(s.Kind) && s.Range.Start.Col < c.indent && s.Range.Start.Line < c.line && s.Range.End.Line >= c.line {
			return true
		}
	}
	for i := len(added) - 1; i >= 0; i-- {
		if added[i].Range.Start.Col < c.indent {
			return isType(added[i].Kind)
		}
	}
	return false
}

func lineLengths(src []byte) []int {
	out := make([]int, 0, bytes.Count(src, []byte{'\n'})+1)
	start := 0
	for i, c := range src {
		if c == '\n' {
			out = append(out, i-start)
			start = i + 1
		}
	}
	return append(out, len(src)-start)
}

// recoveredNote is appended to a partial file's parse error.
func recoveredNote(n int) string {
	return fmt.Sprintf("; %s recovered by line shape inside the unreadable regions (confidence medium)", plural(n, "definition"))
}
