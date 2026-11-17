package parse

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
)

// # Call shapes — what stands before the dot, for every language
//
// A tags query reports that `New` is called on line 40. It does not report
// that the source wrote `errors.New(...)`, and a resolver that is not told
// binds the call to the package's own `New`, because the name matches and
// nothing says otherwise. Hand-checked on eight public projects
// (docs/verification/2026-11-12) that was the largest source of wrong
// high-confidence edges in every language measured: about 170 standard-library
// calls bound to gin's own functions, 1,025 Java local variables bound to a
// helper method of the same name, 193 Rust delegations bound to themselves.
//
// This file states the shape of every call, in every language that has call
// references, without a query per language. Three facts make that possible.
//
// THE TAG CARRIES BOTH RANGES. A reference tag holds the range of the whole
// call expression and the range of the name it captured. Everything between
// the start of the call and the start of the name is the receiver, as written.
// So `reader.peek()` with the name `peek` has the receiver text `reader.`, and
// the same expression with the name `reader` — which the inferred tags queries
// also emit, because their patterns match any identifier under a call node —
// has no text before it and a `.` after it. The first is a call on `reader`.
// The second is not a call at all: it is the receiver, tagged. It is dropped,
// and it used to be a third of all Java "call" references.
//
// THE RECEIVER IS CLASSIFIED BY ITS TEXT. An identifier is a name; identifiers
// joined by a member operator are a path; `this` and `self` are the enclosing
// instance; anything containing a call, an index or an operator is an
// expression with no name. None of that needs to know which grammar produced
// the text, so a grammar added later gets it without a line written for it.
//
// A DECLARED TYPE IS A FIELD CALLED `type`. Tree-sitter grammars name the type
// of a parameter, a variable or a field `type:` and the thing declared `name:`,
// `pattern:` or `declarator:`. One walk over the tree, reading those fields,
// yields what the source states about a variable's type in Go, Java, C#,
// TypeScript, Rust and C++ alike — and where a grammar spells it differently
// the variable simply stays a name, which is what it was before.
//
// The walk also qualifies definitions by what encloses them — `Context.Set`,
// `outer.<locals>.inner` — which is what pyscope.go does for Python. That is
// the precondition for binding `self.f()` to the enclosing type's `f` rather
// than to whichever `f` the file happens to contain.
//
// # What it refuses to infer
//
// The same things pyscope.go refuses, for the same reason. A variable has a
// type only where a declaration WRITES one, or constructs one outright
// (`x := &T{}`, `var q = new Queue()`), and only where the function binds that
// name once: a second declaration of the name anywhere in the function
// withdraws the claim. No return types, no flow, no generics.

// shapeMaxDepth bounds the walk's recursion, for pyMaxDepth's reason: the
// depth of a syntax tree follows the source's nesting, and the source is not
// ours. Past the bound a subtree's declarations are not read and its calls
// keep the shape their text gives them.
const shapeMaxDepth = 400

// shapeMaxReceiver bounds the receiver text that is examined at all. A longer
// one is a call chain spread over several lines, and is an expression.
const shapeMaxReceiver = 240

// callShape is what one call reference was made on.
type callShape struct {
	kind     string
	receiver string

	// notCall is true for a tagged identifier that is the receiver or a
	// qualifier of a call rather than its callee.
	notCall bool

	// indirect is true for a bare call whose callee is a local value — a
	// parameter, a variable — and so names no definition. See
	// IndirectCallSuffix.
	indirect bool
}

// shapeFacts is everything the walk reports about one file.
type shapeFacts struct {
	// calls is keyed by the start byte of the name the tags query captured.
	calls map[uint32]callShape

	// quals holds the qualified name of each definition that is nested in
	// something, keyed the same way. A top-level definition is absent.
	quals map[uint32]string
}

type shScopeKind uint8

const (
	shFile shScopeKind = iota
	shClass
	shFunc
)

// shBinding is what a scope knows about one name: the type its one
// declaration states, or "" when it states none or the name is declared more
// than once with types that differ.
type shBinding struct {
	typ string
}

type shScope struct {
	parent int
	kind   shScopeKind

	// name is the class's or function's own name; "" for an anonymous
	// function, which is transparent to qualification.
	name string

	// nameStart is the start byte of the node that names this scope, so a
	// definition can tell the scope it IS from the scope it is IN.
	nameStart uint32
	hasName   bool

	// owner is the type a function belongs to when the syntax states it
	// outside the type's body: a Go method's receiver type.
	owner string

	decls map[string]shBinding
}

type shWalker struct {
	lang     *ts.Language
	src      []byte
	language string

	scopes []shScope

	// want holds the name positions the tags query reported; scopeAt records
	// the scope each one was met in.
	want    map[uint32]struct{}
	scopeAt map[uint32]int

	// declared holds the name positions already bound, so a declaration and
	// the declarator nested in it bind the name once.
	declared map[uint32]struct{}

	// fields maps a class name to the declared types of its fields, so
	// `this.reader.peek()` can be read as a call on the reader's type.
	fields map[string]map[string]shBinding

	// flags caches what a node type's NAME says about it. The name is read
	// once per distinct node type in the grammar, not once per node.
	flags map[ts.Symbol]shFlags
}

type shFlags uint8

const (
	shMaybeClass shFlags = 1 << iota
	shMaybeFunc
	shMaybeDecl
	shIsField
)

// implicitThisLanguages are the languages in which a bare name inside a class
// may denote a member of that class: `reader.peek()` can be a call on the
// field `reader`. Elsewhere a field is reached only through `this` or `self`,
// and a bare name is a local, a parameter or something at file level.
var implicitThisLanguages = map[string]bool{
	"java": true, "c_sharp": true, "cpp": true, "kotlin": true, "scala": true,
	"swift": true, "dart": true, "groovy": true, "apex": true,
}

// callsNeverReachVariables are the languages in which `f(x)` cannot invoke a
// local variable named f. Java keeps methods and variables in separate
// namespaces — `JsonReader reader = reader(json)` calls the METHOD — and a
// Ruby local is invoked with `.call`, never with parentheses.
var callsNeverReachVariables = map[string]bool{"java": true, "ruby": true}

// callConstructsLanguages are the languages that construct an object by
// calling its type with no keyword: `val q = Queue()`.
var callConstructsLanguages = map[string]bool{
	"kotlin": true, "swift": true, "dart": true, "scala": true,
}

// analyzeShapes walks one tree and reports the shape of each call and the
// qualified name of each nested definition. Nil for a nil tree; a walk that
// panics on a malformed tree returns what it had, which may be nothing.
func (p *genericParser) analyzeShapes(tree *ts.Tree, src []byte, tags []ts.Tag) (facts *shapeFacts) {
	if tree == nil || len(tags) == 0 {
		return nil
	}
	facts = &shapeFacts{
		calls: make(map[uint32]callShape, len(tags)),
		quals: make(map[uint32]string, 16),
	}
	defer func() {
		// The walk reads a tree built from untrusted input; contain it exactly
		// as tagging and the import query are contained. The textual shapes
		// below do not depend on the walk, so they are computed regardless.
		_ = recover()
	}()

	w := &shWalker{
		lang:     p.lang,
		src:      src,
		language: p.entry.Name,
		want:     make(map[uint32]struct{}, len(tags)),
		scopeAt:  make(map[uint32]int, len(tags)),
		declared: make(map[uint32]struct{}, 64),
		fields:   make(map[string]map[string]shBinding, 8),
		flags:    make(map[ts.Symbol]shFlags, 64),
	}
	for _, tag := range tags {
		w.want[tag.NameRange.StartByte] = struct{}{}
	}
	w.scopes = append(w.scopes, shScope{parent: -1, kind: shFile})

	// The textual half first: it cannot fail on a strange tree, and a call
	// whose scope the walk never reaches still has the shape its text gives.
	for _, tag := range tags {
		if !isCallTag(tag.Kind) {
			continue
		}
		facts.calls[tag.NameRange.StartByte] = w.textualShape(tag)
	}

	w.visit(tree.RootNode(), 0, 0, "", 0, false)

	for _, tag := range tags {
		at := tag.NameRange.StartByte
		switch {
		case isCallTag(tag.Kind):
			facts.calls[at] = w.typedShape(facts.calls[at], w.scopeOf(at), tag.Name)
		case strings.HasPrefix(tag.Kind, "definition"):
			if q := w.qualifiedName(tag, w.scopeOf(at)); q != "" && q != tag.Name {
				facts.quals[at] = q
			}
		}
	}
	w.adoptOrphans(tags, facts)
	return facts
}

// membersOnlyLanguages are the languages in which every method and field is
// declared inside a type.
var membersOnlyLanguages = map[string]bool{"java": true, "c_sharp": true}

// adoptOrphans gives a member that the tree shows outside any type the type
// declared before it.
//
// In Java and C# a method cannot stand outside a type, so one that does is a
// parse the grammar could not finish: a C# class body interrupted by `#if`
// directives is closed early in the tree, and the methods after the directive
// are left as siblings of their own class. On Newtonsoft.Json that was 671 of
// 4,545 methods, each of which then looked like a free function — reachable by
// a bare name from anywhere and owned by nothing, so `this.SetToken()` could
// not find it. The class a stray member follows in the source is the class it
// belongs to in every case but a nested type's remainder, and a member
// attributed to the outer type is a far smaller error than one attributed to
// none.
func (w *shWalker) adoptOrphans(tags []ts.Tag, facts *shapeFacts) {
	if !membersOnlyLanguages[w.language] {
		return
	}
	// Nearest preceding type, by position. Tags arrive grouped by pattern, not
	// in source order, so the types are collected first.
	type typeAt struct {
		at   uint32
		qual string
	}
	var types []typeAt
	for _, tag := range tags {
		switch kindFromTag(tag.Kind) {
		case model.KindClass, model.KindInterface:
			if !strings.HasPrefix(tag.Kind, "definition") {
				continue
			}
			at := tag.NameRange.StartByte
			qual := tag.Name
			if q, ok := facts.quals[at]; ok {
				qual = q
			}
			types = append(types, typeAt{at: at, qual: qual})
		}
	}
	if len(types) == 0 {
		return
	}
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition") {
			continue
		}
		switch kindFromTag(tag.Kind) {
		case model.KindMethod, model.KindField, model.KindFunction:
		default:
			continue
		}
		at := tag.NameRange.StartByte
		if _, owned := facts.quals[at]; owned {
			continue
		}
		// types is in tag order; take the one that starts latest before at.
		owner, best := "", uint32(0)
		for _, t := range types {
			if t.at < at && t.at >= best {
				best, owner = t.at, t.qual
			}
		}
		if owner != "" {
			facts.quals[at] = owner + "." + tag.Name
		}
	}
}

// isCallTag reports whether a tag is a call reference.
func isCallTag(kind string) bool {
	return strings.HasPrefix(kind, "reference") && strings.Contains(kind, "call")
}

func (w *shWalker) scopeOf(at uint32) int {
	if s, ok := w.scopeAt[at]; ok {
		return s
	}
	return 0
}

// --- the textual shape ------------------------------------------------------

// memberOperators are what a language writes between a receiver and a member,
// longest first so `?.` is not read as `.` after a `?`.
var memberOperators = []string{"::", "?.", "&.", "!.", "->", "."}

// textualShape classifies a call from the source text around its name.
func (w *shWalker) textualShape(tag ts.Tag) callShape {
	start, nameStart, nameEnd, end := tag.Range.StartByte, tag.NameRange.StartByte, tag.NameRange.EndByte, tag.Range.EndByte
	if nameStart < start || nameEnd > end || int(end) > len(w.src) || nameEnd < nameStart {
		return callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	}
	if w.language == "dart" && start == nameStart && end == nameEnd {
		// Dart's calls are tagged by name alone, because the grammar spreads one
		// call over sibling nodes with none that holds the receiver. See
		// dartCallShape.
		return dartCallShape(w.src, int(nameStart))
	}
	pre := strings.TrimSpace(string(w.src[start:nameStart]))
	post := w.src[nameEnd:end]

	// A name followed by a member operator is a qualifier of the call, not its
	// callee: `reader` in `reader.peek()`, `messages` in `messages::errored()`.
	if followedByMember(post) {
		return callShape{notCall: true}
	}

	// A Lisp writes the call's own parenthesis before the callee.
	pre = strings.TrimLeft(pre, "( \t\r\n")
	if pre == "" {
		return callShape{kind: model.ReceiverBare}
	}

	op := ""
	for _, candidate := range memberOperators {
		if strings.HasSuffix(pre, candidate) {
			op = candidate
			break
		}
	}
	if w.language == "objc" && strings.HasPrefix(pre, "[") && !strings.Contains(pre, "(") {
		// A message send: the call node starts at the bracket and the receiver is
		// everything up to the selector. `[[Item alloc] init]` is the construction
		// of an Item, which states the receiver's type.
		return objcReceiver(strings.TrimSpace(pre[1:]))
	}
	if op == "" && w.language == "lua" && strings.HasSuffix(pre, ":") {
		// `obj:method(x)` passes obj as self. A lone colon is not in
		// memberOperators because in every other language it is a type
		// annotation or a label, not a way to reach a member.
		op = ":"
	}
	if op == "" || len(pre) > shapeMaxReceiver {
		return callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	}
	return classifyReceiver(w.language, strings.TrimSpace(pre[:len(pre)-len(op)]), op)
}

// followedByMember reports whether the text after a name begins with a member
// operator, which makes the name a qualifier.
func followedByMember(post []byte) bool {
	i := 0
	for i < len(post) && (post[i] == ' ' || post[i] == '\t' || post[i] == '\r' || post[i] == '\n') {
		i++
	}
	rest := post[i:]
	switch {
	case len(rest) >= 2 && rest[0] == ':' && rest[1] == ':':
		return true
	case len(rest) >= 2 && rest[0] == '-' && rest[1] == '>':
		return true
	case len(rest) >= 2 && (rest[0] == '?' || rest[0] == '&' || rest[0] == '!') && rest[1] == '.':
		// `a?.b()`, `a&.b`, `a!.b()`. Not `a ? .5 : x`, which no call node
		// contains after its own callee.
		return len(rest) < 3 || rest[2] < '0' || rest[2] > '9'
	case len(rest) >= 1 && rest[0] == '.':
		// A range (`a..b`) and a float (`.5`) are not member access.
		return len(rest) < 2 || (rest[1] != '.' && (rest[1] < '0' || rest[1] > '9'))
	}
	return false
}

// selfTokens name the instance or type a call is written in.
var selfTokens = map[string]bool{"this": true, "self": true, "Self": true, "$this": true, "static": true}

// superTokens name the superclass.
var superTokens = map[string]bool{"super": true, "base": true, "parent": true}

// classifyReceiver turns receiver text and the operator that followed it into
// a shape.
func classifyReceiver(language, text, op string) callShape {
	opaque := callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}

	// An object constructed where it is used states its own type:
	// `new GsonBuilder().create()`, `Client{}.Do()`, `Queue.new.push(x)`.
	if typ := constructedReceiver(language, text); typ != "" {
		return callShape{kind: model.ReceiverType, receiver: typ}
	}

	// Type arguments first: they may hold spaces and commas that are not part
	// of the path (`HashMap::<K, V>`).
	text = compactReceiver(stripTypeArguments(text))
	if text == "" {
		return opaque
	}

	// `super().save()` and `super(C, self).save()`: the superclass, spelled as
	// a call.
	if strings.HasPrefix(text, "super(") && strings.HasSuffix(text, ")") &&
		!strings.ContainsAny(text[len("super("):len(text)-1], "()") {
		return callShape{kind: model.ReceiverSuper, receiver: "super"}
	}

	segments, scoped, ok := splitReceiver(text)
	if !ok {
		return opaque
	}
	if language == "php" {
		for i := range segments {
			segments[i] = strings.TrimPrefix(segments[i], "$")
		}
		text = strings.TrimPrefix(text, "$")
	}

	if len(segments) == 1 {
		head := segments[0]
		// In Rust `self::f()` and `super::f()` are module paths, not the
		// instance and not a superclass; `Self::f()` is the enclosing type.
		modulePath := language == "rust" && op == "::" && (head == "self" || head == "super" || head == "crate")
		switch {
		case modulePath:
			return callShape{kind: model.ReceiverScope, receiver: head}
		case selfTokens[head] || (language == "php" && head == "this"):
			return callShape{kind: model.ReceiverSelf, receiver: head}
		case superTokens[head] && (head != "parent" || language == "php") && (head != "base" || language == "c_sharp"):
			return callShape{kind: model.ReceiverSuper, receiver: head}
		case op == "::":
			return callShape{kind: model.ReceiverScope, receiver: head}
		default:
			return callShape{kind: model.ReceiverName, receiver: head}
		}
	}
	if op == "::" || (scoped && !strings.Contains(text, ".")) {
		return callShape{kind: model.ReceiverScope, receiver: text}
	}
	return callShape{kind: model.ReceiverPath, receiver: text}
}

// literalConstructs are the languages in which `T{...}` is a value of type T.
var literalConstructs = map[string]bool{"go": true, "rust": true, "cpp": true}

// constructedReceiver returns the type of a receiver that is nothing but the
// construction of one object, or "". `new Foo().bar().baz()` constructs a Foo
// and then calls on something else, so only a receiver that ENDS with the
// construction qualifies.
func constructedReceiver(language, text string) string {
	t := strings.TrimSpace(text)
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") && balancedEnd(t, 0) == len(t) {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, "&"))
	switch {
	case strings.HasPrefix(t, "new ") && language != "ruby":
		rest := strings.TrimSpace(t[len("new "):])
		name := leadingTypeName(rest)
		if name == "" {
			return ""
		}
		tail := strings.TrimSpace(rest[len(name):])
		if strings.HasPrefix(tail, "<") {
			end := balancedEnd(tail, 0)
			if end < 0 {
				return ""
			}
			tail = strings.TrimSpace(tail[end:])
		}
		if strings.HasPrefix(tail, "(") {
			end := balancedEnd(tail, 0)
			if end < 0 {
				return ""
			}
			tail = strings.TrimSpace(tail[end:])
		}
		// An anonymous subclass body is still a value of the named type.
		if strings.HasPrefix(tail, "{") {
			end := balancedEnd(tail, 0)
			if end < 0 {
				return ""
			}
			tail = strings.TrimSpace(tail[end:])
		}
		if tail != "" {
			return ""
		}
		return normalizeType(name)
	case literalConstructs[language]:
		name := leadingTypeName(t)
		if name == "" || len(name) == len(t) {
			return ""
		}
		tail := strings.TrimSpace(t[len(name):])
		if !strings.HasPrefix(tail, "{") || balancedEnd(tail, 0) != len(tail) {
			return ""
		}
		return normalizeType(name)
	case language == "ruby":
		// `Queue.new`, `Rack::Mutex.new(arg)`.
		name := leadingTypeName(t)
		if !strings.HasSuffix(name, ".new") || name[0] < 'A' || name[0] > 'Z' {
			return ""
		}
		tail := strings.TrimSpace(t[len(name):])
		if tail != "" && (!strings.HasPrefix(tail, "(") || balancedEnd(tail, 0) != len(tail)) {
			return ""
		}
		return strings.TrimSuffix(name, ".new")
	}
	return ""
}

// balancedEnd returns the index just past the bracket that closes the one at
// text[at], or -1 when it is never closed. Quotes are not understood: a
// bracket inside a string literal makes the answer -1 or wrong, and the caller
// then treats the receiver as the expression it is.
func balancedEnd(text string, at int) int {
	if at >= len(text) {
		return -1
	}
	open := text[at]
	var shut byte
	switch open {
	case '(':
		shut = ')'
	case '{':
		shut = '}'
	case '<':
		shut = '>'
	case '[':
		shut = ']'
	default:
		return -1
	}
	depth := 0
	for i := at; i < len(text); i++ {
		switch text[i] {
		case '"', '\'', '`':
			return -1
		case open:
			depth++
		case shut:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// compactReceiver removes the whitespace a chain may be laid out with and
// writes every member operator as `.`, keeping `::` as it is. It returns ""
// for text that holds whitespace anywhere else, which is two tokens and so an
// expression.
func compactReceiver(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			// Whitespace is layout only beside an operator.
			j := i
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\r' || text[j] == '\n') {
				j++
			}
			prevOp := b.Len() > 0 && isOperatorByte(b.String()[b.Len()-1])
			nextOp := j < len(text) && isOperatorByte(text[j])
			if !prevOp && !nextOp {
				return ""
			}
			i = j - 1
		case c == '-' && i+1 < len(text) && text[i+1] == '>':
			b.WriteByte('.')
			i++
		case (c == '?' || c == '&' || c == '!') && i+1 < len(text) && text[i+1] == '.':
			b.WriteByte('.')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isOperatorByte(c byte) bool {
	return c == '.' || c == ':' || c == '>' || c == '-' || c == '?' || c == '&' || c == '!'
}

// stripTypeArguments removes generic arguments from a path: `Vec::<u8>`
// becomes `Vec`, `Foo<T>::Bar` becomes `Foo::Bar`. Text whose brackets do not
// balance is returned unchanged, and then fails to split.
func stripTypeArguments(text string) string {
	if !strings.ContainsRune(text, '<') {
		return text
	}
	var b strings.Builder
	depth := 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '<':
			depth++
			// `Vec::<u8>` writes the scope operator before the arguments.
			if depth == 1 && strings.HasSuffix(b.String(), "::") {
				s := b.String()
				b.Reset()
				b.WriteString(s[:len(s)-2])
			}
		case c == '>':
			depth--
			if depth < 0 {
				return text
			}
		case depth == 0:
			b.WriteByte(c)
		}
	}
	if depth != 0 {
		return text
	}
	return b.String()
}

// splitReceiver splits a receiver into identifiers, reporting whether any were
// joined by the scope operator. False for anything that is not a chain of
// identifiers.
func splitReceiver(text string) (segments []string, scoped, ok bool) {
	start := 0
	for i := 0; i <= len(text); i++ {
		atEnd := i == len(text)
		sepLen := 0
		if !atEnd {
			switch {
			case text[i] == ':' && i+1 < len(text) && text[i+1] == ':':
				sepLen = 2
			case text[i] == '.':
				sepLen = 1
			}
		}
		if !atEnd && sepLen == 0 {
			continue
		}
		seg := text[start:i]
		if !isIdentifier(seg) {
			return nil, false, false
		}
		segments = append(segments, seg)
		if sepLen == 2 {
			scoped = true
		}
		i += sepLen
		start = i
		if sepLen > 0 {
			i--
		}
	}
	return segments, scoped, len(segments) > 0
}

// isIdentifier reports whether text is one identifier, allowing the sigils a
// few languages put on a variable (`$x`, `@x`) and Ruby's `?` and `!`.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c >= 0x80:
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		case c == '$':
		case c == '@':
			if i > 1 || (i == 1 && s[0] != '@') {
				return false
			}
		case c == '?', c == '!':
			if i != len(s)-1 || i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// --- the walk ---------------------------------------------------------------

func (w *shWalker) flagsOf(n *ts.Node) shFlags {
	sym := n.Symbol()
	if f, ok := w.flags[sym]; ok {
		return f
	}
	f := classifyNodeType(w.language, n.Type(w.lang))
	w.flags[sym] = f
	return f
}

// classifyNodeType reads what a node type's name says it might be. It only
// ever narrows the set of nodes whose fields are then examined; the fields
// decide.
func classifyNodeType(language, typ string) shFlags {
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(typ, p) {
				return true
			}
		}
		return false
	}
	var f shFlags
	if language == "dart" && (typ == "declaration" || typ == "formal_parameter" || typ == "initialized_variable_definition") {
		return shMaybeDecl
	}
	if language == "objc" && typ == "method_parameter" {
		// Its name says "method", which reads as a function; it is a declaration.
		return shMaybeDecl
	}
	switch {
	case has("identifier", "expression", "literal", "_body", "heritage", "clause",
		"modifier", "variant", "argument", "statement", "comment", "string"):
		// Never a scope and never a declaration — except the few declarations
		// some grammars call statements, handled below.
		if has("declaration_statement", "for_statement", "foreach_statement", "enhanced_for_statement", "for_in_statement") {
			f |= shMaybeDecl
		}
		if has("lambda_expression", "function_expression", "closure_expression", "arrow_function", "func_literal") {
			f |= shMaybeFunc
		}
		return f
	case typ == "type_spec", typ == "impl_item", typ == "object_declaration",
		typ == "object_definition" && language == "scala",
		typ == "module" && language == "ruby",
		has("class", "struct", "interface", "trait_", "enum", "record", "protocol", "extension") &&
			!has("_type", "constant", "member", "pattern", "_list", "parameter", "field"):
		f |= shMaybeClass
	case has("function", "method", "lambda", "closure", "constructor", "func_literal", "accessor") &&
		!has("call", "invocation", "_type", "signature", "reference", "pointer", "declarator"):
		f |= shMaybeFunc
	}
	if f == 0 && has("declarat", "parameter", "let_", "short_var", "var_spec", "field", "property",
		"range_clause", "binding", "catch", "resource") {
		f |= shMaybeDecl
		if has("field", "property") {
			f |= shIsField
		}
	}
	return f
}

// visit walks a subtree. scope is the innermost scope; hint carries a name
// for an anonymous function that is the value of a named declaration, so
// `const run = () => {...}` scopes what it defines under `run`.
func (w *shWalker) visit(n *ts.Node, scope, depth int, hint string, hintStart uint32, hasHint bool) {
	if n == nil || depth > shapeMaxDepth {
		return
	}
	if n.NamedChildCount() == 0 {
		if _, wanted := w.want[n.StartByte()]; wanted {
			if _, seen := w.scopeAt[n.StartByte()]; !seen {
				w.scopeAt[n.StartByte()] = scope
			}
		}
		return
	}

	flags := w.flagsOf(n)
	switch {
	case flags&shMaybeClass != 0:
		if name, at, ok := w.classNameOf(n); ok {
			scope = w.push(shScope{parent: scope, kind: shClass, name: name, nameStart: at, hasName: true})
		}
	case flags&shMaybeFunc != 0:
		if w.field(n, "body") != nil {
			s := shScope{parent: scope, kind: shFunc}
			if name := w.field(n, "name"); name != nil && name.NamedChildCount() == 0 {
				s.name, s.nameStart, s.hasName = w.text(name), name.StartByte(), true
			} else if hasHint {
				s.name, s.nameStart, s.hasName = hint, hintStart, true
			}
			if recv := w.field(n, "receiver"); recv != nil {
				s.owner = w.receiverType(recv)
			}
			scope = w.push(s)
		}
	case flags&shMaybeDecl != 0:
		w.declare(n, scope, flags&shIsField != 0)
	}

	// A function that is the value of a named declaration takes that name.
	var valueChild *ts.Node
	childHint, childHintStart, childHasHint := "", uint32(0), false
	if value := w.hintedValue(n, flags); value != nil {
		if name := w.field(n, "name"); name != nil && name.NamedChildCount() == 0 {
			valueChild, childHint, childHintStart, childHasHint = value, w.text(name), name.StartByte(), true
		}
	}

	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || !c.IsNamed() {
			continue
		}
		if c == valueChild {
			w.visit(c, scope, depth+1, childHint, childHintStart, childHasHint)
			continue
		}
		w.visit(c, scope, depth+1, "", 0, false)
	}
}

// hintedValue returns the value of a declaration that names it, where the
// node is a declaration at all. Asked only of declaration-shaped nodes, so the
// field lookup is not paid on every node of the tree.
func (w *shWalker) hintedValue(n *ts.Node, flags shFlags) *ts.Node {
	if flags&shMaybeDecl == 0 {
		return nil
	}
	return w.field(n, "value")
}

func (w *shWalker) push(s shScope) int {
	w.scopes = append(w.scopes, s)
	return len(w.scopes) - 1
}

func (w *shWalker) field(n *ts.Node, name string) *ts.Node {
	if c := n.ChildByFieldName(name, w.lang); c != nil || (w.language != "kotlin" && w.language != "objc") {
		return c
	}
	if w.language == "objc" {
		// An Objective-C method has no `body` field: the braces are a
		// compound_statement child. Without it a method was no scope, and a local
		// declared in it (`Store *store = ...`) was bound in the class's, where a
		// call in another method could not see it and this one could not use it.
		if name == "body" && n.Type(w.lang) == "method_definition" {
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.Type(w.lang) == "compound_statement" {
					return c
				}
			}
		}
		return nil
	}
	return w.kotlinField(n, name)
}

func (w *shWalker) text(n *ts.Node) string { return n.Text(w.src) }

// classNameOf returns the name a class-like node declares, where it has both
// a name and a body. A `struct X` in a parameter list has a name and no body,
// and is a use of the type, not its declaration.
func (w *shWalker) classNameOf(n *ts.Node) (string, uint32, bool) {
	typ := n.Type(w.lang)
	if w.language == "objc" {
		// An @interface, an @implementation and an @protocol have no `name` or
		// `body` field: the first identifier is the type, and for a category
		// (`@implementation Cart (Printing)`) it is the class the category extends,
		// whose members they are.
		switch typ {
		case "class_interface", "class_implementation", "protocol_declaration":
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.IsNamed() && c.Type(w.lang) == "identifier" {
					return w.text(c), c.StartByte(), true
				}
			}
		}
		return "", 0, false
	}
	nameNode := w.field(n, "name")
	switch typ {
	case "impl_item":
		// `impl Trait for Type`: the methods belong to Type.
		nameNode = w.field(n, "type")
	case "type_spec":
		// A Go type's fields are read from here; its methods carry their own
		// receiver.
		if w.field(n, "type") == nil {
			return "", 0, false
		}
	default:
		if w.field(n, "body") == nil {
			return "", 0, false
		}
	}
	if nameNode == nil {
		return "", 0, false
	}
	var name string
	if raw := w.text(nameNode); w.language == "kotlin" && strings.HasPrefix(raw, "`") {
		// A name in backticks is whatever it spells, and is not a type
		// expression for normalizeType to read; the symbol is named without
		// them (see registerNameFix in lang_kotlin.go).
		name = strings.Trim(raw, "`")
	} else {
		name = normalizeType(raw)
	}
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "", 0, false
	}
	return name, nameNode.StartByte(), true
}

// receiverType reads the type out of a Go method's receiver list.
func (w *shWalker) receiverType(recv *ts.Node) string {
	for i := 0; i < recv.ChildCount(); i++ {
		c := recv.Child(i)
		if c == nil || !c.IsNamed() {
			continue
		}
		if t := w.field(c, "type"); t != nil {
			name := normalizeType(w.text(t))
			if j := strings.LastIndexAny(name, ".:"); j >= 0 {
				name = name[j+1:]
			}
			return name
		}
	}
	return ""
}

// shName is one declared identifier and where it was written.
type shName struct {
	text string
	at   uint32
}

// declare records what a declaration states about the names it binds.
func (w *shWalker) declare(n *ts.Node, scope int, isField bool) {
	if w.language == "objc" && n.Type(w.lang) == "method_parameter" {
		w.declareObjcParameter(n, scope)
		return
	}
	if w.language == "dart" {
		w.declareDart(n, scope)
		return
	}
	if w.field(n, "parameters") != nil || w.field(n, "body") != nil {
		// A function prototype or a nested definition, not a variable.
		return
	}
	var names []shName
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || !c.IsNamed() {
			continue
		}
		switch n.FieldNameForChild(i, w.lang) {
		case "name", "pattern", "declarator", "left":
			// The Swift grammar gives a parameter's TYPE the field `name` as well as
			// its name: `item: Item` declares `item` and, read by field alone, `Item`.
			// A local called `Item` made every `Item(...)` in the function an
			// indirect call of a value and bound it to nothing, which is the
			// initialiser of the type.
			if w.language == "swift" && strings.HasSuffix(c.Type(w.lang), "_type") {
				continue
			}
			names = w.declaredNames(c, names, 0)
		}
	}
	typeNode := w.field(n, "type")
	if len(names) == 0 && typeNode != nil {
		// A grammar that puts the declarator beside the type with no field.
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if c != nil && c.IsNamed() && strings.Contains(c.Type(w.lang), "declarator") {
				names = w.declaredNames(c, names, 0)
			}
		}
	}
	if len(names) == 0 {
		return
	}

	typ := ""
	if typeNode != nil {
		typ = normalizeType(w.text(typeNode))
	}
	value := w.valueOf(n)
	if typ == "" && value != nil {
		if w.flagsOf(value)&shMaybeFunc != 0 && value.NamedChildCount() > 0 {
			// `const run = () => {...}` declares a function, which the tags
			// query reports as a definition. It is not a local value.
			return
		}
		typ = w.constructedType(value)
	}

	target := w.bindingScope(scope, isField)
	for _, name := range names {
		if _, done := w.declared[name.at]; done {
			continue
		}
		w.declared[name.at] = struct{}{}
		w.bind(target, name.text, typ)
		if w.scopes[target].kind == shClass && (isField || w.scopes[scope].kind == shClass) {
			w.bindField(w.scopes[target].name, name.text, typ)
		}
		// TypeScript declares a field in a constructor's parameter list:
		// `constructor(private svc: Service)`.
		if typ != "" && w.hasAccessModifier(n) {
			if class := w.enclosingClass(scope); class >= 0 {
				w.bindField(w.scopes[class].name, name.text, typ)
			}
		}
	}
}

func (w *shWalker) hasAccessModifier(n *ts.Node) bool {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.IsNamed() && c.Type(w.lang) == "accessibility_modifier" {
			return true
		}
	}
	return false
}

// declaredNames appends the identifiers a declarator binds.
func (w *shWalker) declaredNames(n *ts.Node, out []shName, depth int) []shName {
	if n == nil || depth > 5 {
		return out
	}
	if n.NamedChildCount() == 0 {
		if s := w.text(n); isIdentifier(s) {
			return append(out, shName{text: strings.TrimLeft(s, "$@"), at: n.StartByte()})
		}
		return out
	}
	if w.field(n, "parameters") != nil {
		// A function declarator: the name declared is a function.
		return out
	}
	for _, f := range []string{"declarator", "name", "pattern"} {
		if c := w.field(n, f); c != nil {
			return w.declaredNames(c, out, depth+1)
		}
	}
	typ := n.Type(w.lang)
	if strings.Contains(typ, "list") || n.NamedChildCount() == 1 {
		// `a, b := f()`, or a wrapper around the one identifier.
		for i := 0; i < n.NamedChildCount(); i++ {
			if c := n.NamedChild(i); c != nil && (c.NamedChildCount() == 0 || n.NamedChildCount() == 1) {
				out = w.declaredNames(c, out, depth+1)
			}
		}
	}
	return out
}

// valueOf returns the expression a declaration initialises its name with.
func (w *shWalker) valueOf(n *ts.Node) *ts.Node {
	if v := w.field(n, "value"); v != nil {
		return v
	}
	if v := w.field(n, "right"); v != nil {
		return v
	}
	// Java and C++ hang the value off the declarator, in a field of its own.
	// C# hangs it there with no field at all, as the declarator's last child.
	d := w.field(n, "declarator")
	for i := 0; d == nil && i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.IsNamed() && strings.Contains(c.Type(w.lang), "declarator") {
			d = c
		}
	}
	if d == nil {
		return nil
	}
	if v := w.field(d, "value"); v != nil {
		return v
	}
	if count := d.NamedChildCount(); count >= 2 {
		if last := d.NamedChild(count - 1); last != nil && last.NamedChildCount() > 0 {
			return last
		}
	}
	return nil
}

// constructedType reads the type out of a value that constructs one outright:
// `&Client{}`, `new Queue()`, `Config { .. }`.
func (w *shWalker) constructedType(value *ts.Node) string {
	text := strings.TrimSpace(w.text(value))
	if len(text) > shapeMaxReceiver {
		text = text[:shapeMaxReceiver]
	}
	// Go's expression_list wraps a single value.
	text = strings.TrimPrefix(text, "&")
	text = strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, "new "):
		return normalizeType(leadingTypeName(strings.TrimSpace(text[len("new "):])))
	case strings.HasPrefix(text, "new("):
		rest := text[len("new("):]
		if i := strings.IndexByte(rest, ')'); i > 0 {
			return normalizeType(rest[:i])
		}
		return ""
	}
	name := leadingTypeName(text)
	if name == "" || len(name) >= len(text) {
		return ""
	}
	rest := strings.TrimLeft(text[len(name):], " \t")
	switch {
	case strings.HasPrefix(rest, "{") && w.language != "javascript" && w.language != "typescript" && w.language != "tsx":
		// A composite or struct literal: `Client{...}`, `Config { .. }`.
		return normalizeType(name)
	case strings.HasPrefix(rest, "(") && callConstructsLanguages[w.language] && name[0] >= 'A' && name[0] <= 'Z':
		// In Swift and Dart `Money.zero()` is a static function or a named
		// constructor, and the value is not a `Money.zero`; only a call that ends
		// in a capitalised name (`Money(...)`, `Outer.Inner(...)`) states the type.
		// It typed `sum` in `var sum = Money.zero()` as a type that does not
		// exist, so `sum.adding(x)` named a type nothing declares and was dropped.
		if (w.language == "swift" || w.language == "dart") && !isCapitalised(name[strings.LastIndexByte(name, '.')+1:]) {
			return ""
		}
		return normalizeType(name)
	}
	return ""
}

// leadingTypeName returns the dotted or scoped identifier text begins with.
func leadingTypeName(text string) string {
	i := 0
	for i < len(text) {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' {
			i++
			continue
		}
		break
	}
	return strings.TrimRight(text[:i], ".:")
}

// bindingScope picks the scope a declaration binds in: a field in its class,
// anything else in the innermost function, or failing one in whatever
// encloses it.
func (w *shWalker) bindingScope(scope int, isField bool) int {
	if isField {
		if class := w.enclosingClass(scope); class >= 0 && w.scopes[scope].kind == shClass {
			return class
		}
	}
	return scope
}

func (w *shWalker) enclosingClass(scope int) int {
	for s := scope; s >= 0; s = w.scopes[s].parent {
		if w.scopes[s].kind == shClass {
			return s
		}
	}
	return -1
}

// bind records one declaration of a name. A second declaration that states a
// different type, or none, withdraws the first.
func (w *shWalker) bind(scope int, name, typ string) {
	s := &w.scopes[scope]
	if s.decls == nil {
		s.decls = make(map[string]shBinding, 8)
	}
	if prev, seen := s.decls[name]; seen && prev.typ != typ {
		s.decls[name] = shBinding{}
		return
	}
	s.decls[name] = shBinding{typ: typ}
}

func (w *shWalker) bindField(class, name, typ string) {
	if class == "" {
		return
	}
	m := w.fields[class]
	if m == nil {
		m = make(map[string]shBinding, 8)
		w.fields[class] = m
	}
	if prev, seen := m[name]; seen && prev.typ != typ {
		m[name] = shBinding{}
		return
	}
	m[name] = shBinding{typ: typ}
}

// lookup returns the type the nearest declaration of a name states. found is
// true when a declaration was met at all, typed or not: a name declared
// without a type is a value of unknown type, not an undeclared name. local is
// true when that declaration is in a function — a parameter or a variable.
func (w *shWalker) lookup(scope int, name string) (typ string, found, local bool) {
	for s := scope; s >= 0; s = w.scopes[s].parent {
		sc := &w.scopes[s]
		if sc.kind == shClass && !implicitThisLanguages[w.language] {
			continue
		}
		if b, ok := sc.decls[name]; ok {
			return b.typ, true, sc.kind == shFunc
		}
	}
	return "", false, false
}

// typedShape replaces a variable with the type the source states for it.
func (w *shWalker) typedShape(shape callShape, scope int, name string) callShape {
	switch shape.kind {
	case model.ReceiverBare:
		// `callback()` where callback is a parameter names no definition —
		// in a language where a variable can be called at all.
		if _, found, local := w.lookup(scope, name); found && local && !callsNeverReachVariables[w.language] {
			shape.indirect = true
		}
	case model.ReceiverName:
		typ, found, _ := w.lookup(scope, strings.TrimLeft(shape.receiver, "$@"))
		switch {
		case typ != "":
			return callShape{kind: model.ReceiverType, receiver: typ}
		case found:
			// Declared, with no type the source states: a value, which rules
			// out reading the name as an import, a module or a type.
			return callShape{kind: model.ReceiverValue, receiver: shape.receiver}
		}
	case model.ReceiverPath:
		segments := strings.Split(shape.receiver, ".")
		if strings.Contains(shape.receiver, "::") || len(segments) < 2 {
			return shape
		}
		// `this.reader.peek()`, `t.in.Get()`: the head's type, then each
		// field's declared type in turn.
		typ := ""
		if selfTokens[segments[0]] {
			if class := w.enclosingClass(scope); class >= 0 {
				typ = w.scopes[class].name
			} else if owner := w.enclosingOwner(scope); owner != "" {
				typ = owner
			}
		} else {
			typ, _, _ = w.lookup(scope, strings.TrimLeft(segments[0], "$@"))
		}
		for _, field := range segments[1:] {
			if typ == "" {
				return shape
			}
			typ = w.fields[lastTypeSegment(typ)][strings.TrimLeft(field, "$@")].typ
		}
		if typ != "" {
			return callShape{kind: model.ReceiverType, receiver: typ}
		}
	}
	return shape
}

func (w *shWalker) enclosingOwner(scope int) string {
	for s := scope; s >= 0; s = w.scopes[s].parent {
		if w.scopes[s].owner != "" {
			return w.scopes[s].owner
		}
	}
	return ""
}

func lastTypeSegment(typ string) string {
	if i := strings.LastIndexAny(typ, ".:"); i >= 0 {
		return typ[i+1:]
	}
	return typ
}

// qualifiedName returns a definition's name qualified by what encloses it,
// following pyscope.go: a member is `Class.member`, and anything defined in a
// function carries the `<locals>` marker — `build.<locals>.Item`.
func (w *shWalker) qualifiedName(tag ts.Tag, scope int) string {
	name := tag.Name
	at := tag.NameRange.StartByte

	// The scope a definition IS — a class's own name sits inside the class
	// node — is not a scope it is IN.
	if s := &w.scopes[scope]; s.hasName && s.nameStart == at && scope > 0 {
		if s.owner != "" {
			return s.owner + "." + name
		}
		scope = s.parent
	}

	// C++ defines a member outside its class: `void Iter::Seek(...)`.
	if owner := w.writtenOwner(at); owner != "" {
		return owner + "." + name
	}

	var parts []string
	for s := scope; s > 0; s = w.scopes[s].parent {
		sc := &w.scopes[s]
		switch {
		case sc.kind == shClass:
			parts = append(parts, sc.name)
		case sc.name != "":
			parts = append(parts, pyLocals, sc.name)
			if sc.owner != "" {
				parts = append(parts, sc.owner)
			}
		}
	}
	if len(parts) == 0 {
		return name
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteString(parts[i])
		b.WriteByte('.')
	}
	b.WriteString(name)
	return b.String()
}

// writtenOwner returns the identifier a definition's name is qualified by in
// the source itself, `Iter` in `Iter::Seek`, or "".
func (w *shWalker) writtenOwner(at uint32) string {
	if w.language != "cpp" && w.language != "c" {
		return ""
	}
	i := int(at)
	if i < 3 || w.src[i-1] != ':' || w.src[i-2] != ':' {
		return ""
	}
	end := i - 2
	// Skip template arguments: `Foo<T>::bar`.
	if end > 0 && w.src[end-1] == '>' {
		depth := 0
		for end > 0 {
			end--
			if w.src[end] == '>' {
				depth++
			} else if w.src[end] == '<' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
	}
	start := end
	for start > 0 {
		c := w.src[start-1]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			start--
			continue
		}
		break
	}
	owner := string(w.src[start:end])
	if !isIdentifier(owner) {
		return ""
	}
	return owner
}

// transparentWrappers are generic types whose methods are, for a call written
// on a value of them, the methods of what they wrap: a `Box<Foo>` or an
// `Arc<Foo>` dereferences to Foo, and a `unique_ptr<Foo>` is used through `->`.
var transparentWrappers = map[string]bool{
	"Box": true, "Rc": true, "Arc": true,
	"unique_ptr": true, "shared_ptr": true, "std::unique_ptr": true, "std::shared_ptr": true,
}

// unnameableTypes are words a declaration writes where a type goes that name
// no type: inference keywords and the top types.
var unnameableTypes = map[string]bool{
	"var": true, "val": true, "let": true, "auto": true, "def": true, "dynamic": true,
	"any": true, "unknown": true, "object": true, "void": true, "interface": true,
	"func": true, "fn": true, "mut": true, "const": true,
}

// normalizeType reduces the text of a type to the name of the type a method
// call on a value of it would reach: `*Context` and `&mut Context` are
// Context, `List<String>` is List, `h.ResponseWriter` stays qualified. It
// returns "" for anything that names no single type — a slice, a map, a
// function type, a union, an inference keyword.
func normalizeType(text string) string {
	t := strings.TrimSpace(text)
	if i := strings.IndexAny(t, "\r\n"); i >= 0 {
		return ""
	}
	// TypeScript's annotation node includes the colon.
	t = strings.TrimSpace(strings.TrimPrefix(t, ":"))
	for changed := true; changed; {
		changed = false
		for _, prefix := range []string{"*", "&", "mut ", "const ", "final ", "readonly ", "dyn ", "impl ",
			"struct ", "class ", "enum ", "volatile ", "static ", "ref ", "out ", "in ", "?"} {
			if strings.HasPrefix(t, prefix) {
				t = strings.TrimSpace(t[len(prefix):])
				changed = true
			}
		}
		// A Rust lifetime: `&'a Foo`.
		if strings.HasPrefix(t, "'") {
			if i := strings.IndexByte(t, ' '); i > 0 {
				t = strings.TrimSpace(t[i+1:])
				changed = true
			}
		}
	}
	for strings.HasSuffix(t, "*") || strings.HasSuffix(t, "&") || strings.HasSuffix(t, "?") || strings.HasSuffix(t, "!") {
		t = strings.TrimSpace(t[:len(t)-1])
	}
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), " const"))
	if t == "" || t[0] == '[' || t[0] == '(' || t[0] == '{' || strings.HasSuffix(t, "[]") {
		return ""
	}
	if i := strings.IndexByte(t, '<'); i > 0 && strings.HasSuffix(t, ">") {
		outer, inner := t[:i], t[i+1:len(t)-1]
		if transparentWrappers[outer] && !strings.ContainsAny(inner, ",") {
			return normalizeType(inner)
		}
		t = outer
	} else if i := strings.IndexByte(t, '['); i > 0 && strings.HasSuffix(t, "]") && !strings.HasPrefix(t, "map[") {
		// Go and Scala write type arguments in brackets.
		t = t[:i]
	}
	if strings.HasPrefix(t, "map[") || strings.HasPrefix(t, "chan ") || strings.HasPrefix(t, "func(") || strings.HasPrefix(t, "fn(") {
		return ""
	}
	segments, _, ok := splitReceiver(t)
	if !ok || unnameableTypes[t] {
		return ""
	}
	for _, seg := range segments {
		if strings.ContainsAny(seg, "@?!") {
			return ""
		}
	}
	return t
}

func isCapitalised(s string) bool { return s != "" && s[0] >= 'A' && s[0] <= 'Z' }

// objcReceiver classifies the receiver of a message send, written between the
// opening bracket and the first selector part.
func objcReceiver(text string) callShape {
	if inner, ok := strings.CutPrefix(text, "["); ok && strings.HasSuffix(inner, "]") {
		// [Item alloc] and [Item new] state that the value is an Item.
		fields := strings.Fields(strings.TrimSuffix(inner, "]"))
		if len(fields) == 2 && (fields[1] == "alloc" || fields[1] == "new") && isCapitalised(fields[0]) && isIdentifier(fields[0]) {
			return callShape{kind: model.ReceiverType, receiver: fields[0]}
		}
		return callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	}
	return classifyReceiver("objc", text, ".")
}

// declareObjcParameter records a method parameter, `:(Item *)item`. The grammar
// gives it no fields: the type is the type_identifier inside its method_type
// and the name is the identifier after it. `id`, a primitive and a block type
// state no class and leave the name an untyped local.
func (w *shWalker) declareObjcParameter(n *ts.Node, scope int) {
	var name *ts.Node
	typ := ""
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || !c.IsNamed() {
			continue
		}
		switch c.Type(w.lang) {
		case "identifier":
			name = c
		case "method_type":
			for j := 0; j < c.ChildCount(); j++ {
				tn := c.Child(j)
				if tn == nil || tn.Type(w.lang) != "type_name" {
					continue
				}
				for k := 0; k < tn.ChildCount(); k++ {
					if id := tn.Child(k); id != nil && id.Type(w.lang) == "type_identifier" {
						typ = w.text(id)
					}
				}
			}
		}
	}
	if name == nil {
		return
	}
	if _, done := w.declared[name.StartByte()]; done {
		return
	}
	w.declared[name.StartByte()] = struct{}{}
	w.bind(scope, w.text(name), typ)
}

// declareDart records what a Dart declaration states: a field or a top-level
// variable (`final Store store;`), a parameter (`Item item`) and a local
// (`Item y = ...`). The grammar gives none of them a `type` field: the type is
// the type_identifier written before the name, and `var` and `final` with no
// type state none.
func (w *shWalker) declareDart(n *ts.Node, scope int) {
	var names []*ts.Node
	typ, constructed := "", ""
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || !c.IsNamed() {
			continue
		}
		switch c.Type(w.lang) {
		case "type_identifier":
			typ = w.text(c)
		case "identifier":
			switch n.FieldNameForChild(i, w.lang) {
			case "name":
				names = append(names, c)
			case "value":
				// `var cart = Cart(store)`: a capitalised name called at once, with
				// nothing chained after the argument list, constructs that type. The
				// grammar spreads the call over siblings (the name, then a selector
				// holding the arguments), so it is read from them.
				if sel := c.NextSibling(); sel != nil && sel.Type(w.lang) == "selector" && sel.NextSibling() == nil &&
					isCapitalised(w.text(c)) && sel.NamedChildCount() > 0 && sel.NamedChild(0).Type(w.lang) == "argument_part" {
					constructed = w.text(c)
				}
			}
		case "initialized_identifier_list":
			for j := 0; j < c.ChildCount(); j++ {
				if ii := c.Child(j); ii != nil && ii.Type(w.lang) == "initialized_identifier" {
					for k := 0; k < ii.ChildCount(); k++ {
						if id := ii.Child(k); id != nil && id.Type(w.lang) == "identifier" {
							names = append(names, id)
							break
						}
					}
				}
			}
		}
	}
	if typ == "" {
		typ = constructed
	}
	target := w.bindingScope(scope, w.scopes[scope].kind == shClass)
	for _, name := range names {
		if _, done := w.declared[name.StartByte()]; done {
			continue
		}
		w.declared[name.StartByte()] = struct{}{}
		w.bind(target, w.text(name), typ)
		if w.scopes[target].kind == shClass {
			w.bindField(w.scopes[target].name, w.text(name), typ)
		}
	}
}
