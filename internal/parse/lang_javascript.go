package parse

import (
	"path"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// JavaScript, TypeScript and TSX: the definitions and references the shared
// inferred query does not know. What the index holds, see
// fixtures/langs/javascript, fixtures/langs/typescript and fixtures/langs/tsx.
//
// The three grammars share every node type used below, so one body of patterns
// and one refiner serve all three; lang_typescript.go and lang_tsx.go register
// them, and the JSX patterns belong to the two grammars that have JSX.
//
// # Why a refiner and not only a query
//
// A query says which shape of statement to look at. It cannot say whether
// `app.init = function init() {}` defines `init` OF `app` (because `app` is an
// object this file declares at its top level) or only assigns to a property of
// something unknown (because it is not), and it cannot name a definition after
// the file it is in. The refiner reads the same tree for exactly those facts:
// it drops a tag that is not a definition, names the ones the file names, and
// says which object a method belongs to. Every rule is listed here, once.
//
// # CommonJS, and the limits
//
//	exports.parse = function () {}            function  parse
//	module.exports.parse = (x) => x           function  parse
//	exports.Router = class Router {}          class     Router
//	module.exports = function createApp() {}  function  createApp
//	module.exports = function () {}           function  named after the file
//	module.exports = class {}                 class     named after the file
//	module.exports = { a() {}, b: () => 1 }   method a (the inferred query),
//	                                          function b
//	Foo.prototype.bar = function () {}        method    bar, owned by Foo
//	obj.name = function () {}                 method    name, owned by obj
//
// The first five and the object literal are definitions only when the
// statement is at the top level of the file: directly, or as the value of a
// top-level `var x = ` / chain of assignments (`var app = exports =
// module.exports = {}`). `exports.x = ...` inside a function is an assignment
// to a property of whatever `exports` is there.
//
// `obj.name = function` is a definition only when it is a top-level statement
// AND obj is declared at the top level of the same file (`var obj = {}`,
// `function obj() {}`, `class obj {}`, `const obj = require(...)` is NOT: the
// object belongs to another file). Everything else — `this.x = function`,
// `a.b.c = function`, `obj.x = function` inside a function body, `obj.x =
// someName` — is an assignment and not a definition.
//
// `Foo.prototype.bar = function` is a definition wherever it is written:
// nothing else is spelled that way. Its owner is Foo whether or not Foo is
// declared in the file, and when the file declares `function Foo` the
// declaration is a class (an ES5 constructor is the class of its prototype).
//
// A chain `res.set = res.header = function header() {}` defines both names.
// The name is the PROPERTY, not the function expression's own name: the
// expression's name is local to its body, and `res.type(...)` is how it is
// called.
//
// # The file's name
//
// An anonymous `module.exports = function () {}` is what a caller reaches
// through `require('./x')`, and the caller names it, so the one stable name the
// definition has is the file's: the base name without its extension, with
// `index` replaced by the directory's name, and hyphens or dots turned to
// camel case (`merge-options.js` is `mergeOptions`). A name that is still not
// an identifier gives no symbol.
//
// # Constants and wrapped components
//
// At the top level of a file (and `export` only there):
//
//	const X = () => {}, = function () {}      function  (as before)
//	const X = forwardRef(...), memo(...),     function  a component built by
//	  lazy(...), observer(...),                          a wrapper is called to
//	  defineComponent(...), styled.x`...`,               render, so it is one
//	  styled(Y)`...`, React.memo(...)
//	const X = class {}                        class
//	const Ctx = createContext(...)            constant
//	export const MAX = 5                      constant  (export let/var: variable)
//	const cva = build(); export { cva }       constant  (named in an export clause)
//
// A non-exported `const x = 5` is not a definition. The inferred function
// patterns are untouched, and still find a local `const f = () => {}`.

// scriptDefinitions are the patterns every script grammar shares.
const scriptDefinitions = `
(assignment_expression
  left: (member_expression property: (property_identifier) @name)
  right: [(function_expression) (arrow_function) (class) (assignment_expression)]) @definition.method

(assignment_expression
  left: (member_expression)
  right: (object (pair key: (property_identifier) @name value: [(function_expression) (arrow_function)]) @definition.function))

(program
  (export_statement
    (lexical_declaration
      (variable_declarator name: (identifier) @name)) @definition.constant))
(program
  (export_statement
    (variable_declaration
      (variable_declarator name: (identifier) @name)) @definition.variable))
(program
  (lexical_declaration
    (variable_declarator name: (identifier) @name)) @definition.constant)
(program
  (variable_declaration
    (variable_declarator name: (identifier) @name)) @definition.variable)
`

// scriptNewReferences make `new Foo(...)` a call on Foo. The tagged node is
// the callee, so the shape of the call is read from it and not from the `new`
// keyword in front.
const scriptNewReferences = `
(new_expression constructor: (identifier) @name @reference.call)
(new_expression constructor: (member_expression property: (property_identifier) @name) @reference.call)
`

// jsxReferences make using a component a reference to it. See the decision in
// lang_tsx.go.
const jsxReferences = `
(jsx_opening_element
  name: (identifier) @name @reference.call
  (#match? @name "^[A-Z]"))
(jsx_self_closing_element
  name: (identifier) @name @reference.call
  (#match? @name "^[A-Z]"))
(jsx_opening_element
  name: (member_expression property: (property_identifier) @name) @reference.call
  (#not-any-of? @name "Provider" "Consumer"))
(jsx_self_closing_element
  name: (member_expression property: (property_identifier) @name) @reference.call
  (#not-any-of? @name "Provider" "Consumer"))
(jsx_opening_element
  name: (member_expression
    object: (identifier) @name @reference.jsx
    property: (property_identifier) @_context)
  (#any-of? @_context "Provider" "Consumer"))
`

// definitionFix is what a refiner says about one definition tag, keyed by the
// start byte of the tag's name.
type definitionFix struct {
	// drop removes the tag: the pattern matched a shape that is not a
	// definition.
	drop bool
	// name replaces the captured name; "" keeps it.
	name string
	// kind replaces the tag kind ("definition.class"); "" keeps it.
	kind string
	// owner is the type or object the definition is a member of; the symbol's
	// qualified name becomes owner.name.
	owner string
	// endLine and endCol, when endLine is set, replace the end of the symbol's
	// range. For a declaration the grammar writes as a signature and a sibling
	// body (Dart), where the query can capture only the signature.
	endLine, endCol int
}

// definitionRefiner reads facts about a file's definitions that no query can
// state. It returns nothing for a tag it has no opinion on.
type definitionRefiner func(root *ts.Node, lang *ts.Language, path string, src []byte, tags []ts.Tag) map[uint32]definitionFix

var definitionRefiners = map[string]definitionRefiner{}

// refineDefinitions asks the language's refiner, if it has one, behind a panic
// barrier: the tree was built from a file this project did not write.
func (p *genericParser) refineDefinitions(tree *ts.Tree, src []byte, path string, tags []ts.Tag) (fixes map[uint32]definitionFix) {
	fn := definitionRefiners[p.entry.Name]
	if fn == nil || tree == nil || len(tags) == 0 {
		return nil
	}
	defer func() {
		if recover() != nil {
			fixes = nil
		}
	}()
	root := tree.RootNode()
	if root == nil {
		return nil
	}
	return fn(root, p.lang, path, src, tags)
}

// registerScriptLanguage registers the shared patterns and the refiner for one
// of the three grammars, with extra patterns only that grammar can parse.
func registerScriptLanguage(language, extra string) {
	private := privateMembers
	if language == "javascript" {
		private += jsPrivateFields
	} else {
		private += tsPrivateFields + tsNamespaces
		// An overload signature is folded into the implementation that follows
		// it, see tsStubName.
		registerStubFold(language, tsStubName)
	}
	registerDefinitions(language, scriptDefinitions+private+scriptNewReferences+extra, false)
	definitionRefiners[language] = refineScriptDefinitions
}

// # Private members, namespaces and overload signatures
//
// A `#name` member is a private_property_identifier, a node type of its own
// that the inferred patterns (which name property_identifier) never see: hono
// declares its internal state and helpers that way, and 19 of the 20
// definitions missed there and 6 of the 13 call sites missed were `#name`. A
// member is named WITH its `#`, as the language writes it: `#init` and `init`
// are two different members of one class, and `this.#init()` can only be the
// first.
//
//	#status = 200            field    #status        (public_field_definition in
//	static #count = 0        field    #count          TypeScript, field_definition
//	                                                  in JavaScript)
//	#init(a) {}              method   #init
//	get #size() {}           method   #size
//	this.#init(x)            call     #init, on this
//	obj.#init(x)             call     #init, on obj
//
// JavaScript's other class fields are not definitions (see its fixture), so
// only its private ones are added: a `#field` is a member nothing outside the
// class can name, which is the case for finding it by name here.
//
// `namespace Outer { ... }` and `namespace A.B { ... }` are packages, named as
// written. Members inside are not qualified by it: they stay what they were
// (`inner` is a function), because the call `Outer.inner()` reaches them by the
// plain name too. `declare module "x" {}` and `declare global {}` are not
// namespaces of the program and are left out.
const (
	privateMembers = `
(method_definition name: (private_property_identifier) @name) @definition.method
(call_expression function: (member_expression property: (private_property_identifier) @name)) @reference.call
`
	tsPrivateFields = `
(public_field_definition name: (private_property_identifier) @name) @definition.field
`
	jsPrivateFields = `
(field_definition property: (private_property_identifier) @name) @definition.field
`
	tsNamespaces = `
(internal_module name: (identifier) @name) @definition.module
(internal_module name: (nested_identifier) @name) @definition.module
`
)

// tsStubName reports whether the name belongs to a declaration with no body: a
// function overload signature, or a method signature of a class or interface.
// `function f(a: string): void; function f(a: number): void; function f(a:
// any) {}` is one function, not three, and so is a method overloaded the same
// way. A signature is folded into the declaration that follows it.
func tsStubName(name *ts.Node, lang *ts.Language, _ []byte) bool {
	parent := name.Parent()
	if parent == nil {
		return false
	}
	switch parent.Type(lang) {
	case "function_signature", "method_signature", "abstract_method_signature":
		return true
	}
	return false
}

// scriptNode is a node and the path from the root to it.
type scriptNode struct {
	n    *ts.Node
	path []*ts.Node
}

// scriptRefiner is one refinement of one file.
type scriptRefiner struct {
	lang *ts.Language
	src  []byte
	file string
	root *ts.Node

	// top holds the names the file declares at its top level, and listed the
	// names an `export { a, b as c }` clause exports.
	top    map[string]bool
	listed map[string]bool
}

func (r *scriptRefiner) typ(n *ts.Node) string {
	if n == nil {
		return ""
	}
	return n.Type(r.lang)
}

func (r *scriptRefiner) text(n *ts.Node) string {
	if n == nil {
		return ""
	}
	return n.Text(r.src)
}

func (r *scriptRefiner) field(n *ts.Node, name string) *ts.Node {
	if n == nil {
		return nil
	}
	return n.ChildByFieldName(name, r.lang)
}

// find returns the node whose range is exactly [start, end), outermost first,
// with its ancestors. Children are in source order, so each level is a binary
// search and not a scan: a file of thousands of statements asks this for
// thousands of tags.
func (r *scriptRefiner) find(start, end uint32) (scriptNode, bool) {
	n := r.root
	path := []*ts.Node{n}
	for n != nil {
		if n.StartByte() == start && n.EndByte() == end {
			// A statement written without its semicolon has the range of its
			// expression: the node wanted is the innermost of the two.
			for {
				var same *ts.Node
				for i := 0; i < n.ChildCount(); i++ {
					if c := n.Child(i); c != nil && c.IsNamed() && c.StartByte() == start && c.EndByte() == end {
						same = c
						break
					}
				}
				if same == nil {
					break
				}
				n = same
				path = append(path, n)
			}
			return scriptNode{n: n, path: path}, true
		}
		count := n.ChildCount()
		lo, hi := 0, count
		for lo < hi {
			mid := (lo + hi) / 2
			c := n.Child(mid)
			if c == nil || c.EndByte() <= start {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		var next *ts.Node
		if lo < count {
			if c := n.Child(lo); c != nil && c.StartByte() <= start && c.EndByte() >= end {
				next = c
			}
		}
		if next == nil {
			return scriptNode{}, false
		}
		n = next
		path = append(path, n)
	}
	return scriptNode{}, false
}

// collectTop records the names a file declares at its top level, and the
// names an `export { ... }` clause lists.
func (r *scriptRefiner) collectTop() {
	r.top = make(map[string]bool, 16)
	r.listed = make(map[string]bool, 8)
	for i := 0; i < r.root.NamedChildCount(); i++ {
		stmt := r.root.NamedChild(i)
		if r.typ(stmt) == "export_statement" {
			if r.field(stmt, "source") == nil {
				for j := 0; j < stmt.NamedChildCount(); j++ {
					clause := stmt.NamedChild(j)
					if r.typ(clause) != "export_clause" {
						continue
					}
					for k := 0; k < clause.NamedChildCount(); k++ {
						if spec := clause.NamedChild(k); r.typ(spec) == "export_specifier" {
							if name := r.field(spec, "name"); name != nil {
								r.listed[r.text(name)] = true
							}
						}
					}
				}
			}
			if d := r.field(stmt, "declaration"); d != nil {
				stmt = d
			}
		}
		switch r.typ(stmt) {
		case "lexical_declaration", "variable_declaration":
			for j := 0; j < stmt.NamedChildCount(); j++ {
				d := stmt.NamedChild(j)
				if r.typ(d) != "variable_declarator" {
					continue
				}
				if name := r.field(d, "name"); r.typ(name) == "identifier" {
					r.top[r.text(name)] = true
				}
			}
		case "function_declaration", "generator_function_declaration", "class_declaration", "abstract_class_declaration":
			if name := r.field(stmt, "name"); name != nil {
				r.top[r.text(name)] = true
			}
		}
	}
}

// statementLevel reports whether a node sits in a statement at the top of the
// file: itself the statement's expression, or reached from it only through a
// chain of assignments, a declarator, or (for a pair) the object literal.
func (r *scriptRefiner) statementLevel(path []*ts.Node) bool {
	if len(path) < 3 || r.typ(path[0]) != "program" {
		return false
	}
	switch r.typ(path[1]) {
	case "expression_statement", "lexical_declaration", "variable_declaration":
	default:
		return false
	}
	for _, mid := range path[2 : len(path)-1] {
		switch r.typ(mid) {
		case "assignment_expression", "variable_declarator":
		default:
			return false
		}
	}
	return true
}

// finalValue follows `a = b = value` to its last value.
func (r *scriptRefiner) finalValue(n *ts.Node) *ts.Node {
	for i := 0; i < 8 && r.typ(n) == "assignment_expression"; i++ {
		n = r.field(n, "right")
	}
	return n
}

// objectName returns the single identifier an expression is, or "".
func (r *scriptRefiner) identifier(n *ts.Node) string {
	if r.typ(n) != "identifier" {
		return ""
	}
	return r.text(n)
}

// kindOfValue returns the definition kind a function-like value gives, or "".
func (r *scriptRefiner) kindOfValue(v *ts.Node) string {
	switch r.typ(v) {
	case "function_expression", "arrow_function", "generator_function":
		return "definition.function"
	case "class":
		return "definition.class"
	}
	return ""
}

// stemName is the name a file gives an anonymous module.exports.
func stemName(file string) string {
	file = strings.ReplaceAll(file, "\\", "/")
	base := path.Base(file)
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if base == "index" {
		if dir := path.Base(path.Dir(file)); dir != "" && dir != "." && dir != "/" {
			base = dir
		}
	}
	var b strings.Builder
	upper := false
	for i := 0; i < len(base); i++ {
		c := base[i]
		switch {
		case c == '-' || c == '.' || c == ' ':
			upper = b.Len() > 0
		case upper && c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
			upper = false
		default:
			b.WriteByte(c)
			upper = false
		}
	}
	if name := b.String(); isIdentifier(name) && !strings.ContainsAny(name, "$@?!") {
		return name
	}
	return ""
}

// wrapperCallees are the functions that return a component, so that a
// constant initialised by calling one is a function that renders.
var wrapperCallees = map[string]bool{
	"forwardRef": true, "memo": true, "lazy": true, "observer": true, "defineComponent": true,
	"cva": true, "dynamic": true,
}

// calleeName is the last name of the function a call invokes: `memo` for
// `memo(f)` and for `React.memo(f)`.
func (r *scriptRefiner) calleeName(call *ts.Node) string {
	fn := r.field(call, "function")
	switch r.typ(fn) {
	case "identifier":
		return r.text(fn)
	case "member_expression":
		return r.text(r.field(fn, "property"))
	}
	return ""
}

// isStyled reports whether a call is a styled component: the callee is
// styled.div or styled(Box), tagged with a template or called with an object.
func (r *scriptRefiner) isStyled(call *ts.Node) bool {
	t := strings.TrimSpace(r.text(r.field(call, "function")))
	if !strings.HasPrefix(t, "styled") {
		return false
	}
	rest := t[len("styled"):]
	return rest == "" || rest[0] == '.' || rest[0] == '(' || rest[0] == '<'
}

// ensureTop collects the top-level names once.
func (r *scriptRefiner) ensureTop() {
	if r.top == nil {
		r.collectTop()
	}
}

// refineDeclaration decides what a top-level `const x = value` defines.
func (r *scriptRefiner) refineDeclaration(sn scriptNode, tag ts.Tag, protoOwners map[string]bool) (definitionFix, bool) {
	decl := sn.n
	var declarator *ts.Node
	for i := 0; i < decl.NamedChildCount(); i++ {
		d := decl.NamedChild(i)
		if r.typ(d) != "variable_declarator" {
			continue
		}
		if name := r.field(d, "name"); name != nil && name.StartByte() == tag.NameRange.StartByte {
			declarator = d
			break
		}
	}
	if declarator == nil {
		return definitionFix{}, false
	}
	r.ensureTop()
	topLevel := len(sn.path) >= 2 && (r.typ(sn.path[len(sn.path)-2]) == "program" ||
		(len(sn.path) >= 3 && r.typ(sn.path[len(sn.path)-2]) == "export_statement" && r.typ(sn.path[len(sn.path)-3]) == "program"))
	exported := topLevel && (r.typ(sn.path[len(sn.path)-2]) == "export_statement" || r.listed[tag.Name])
	value := r.field(declarator, "value")
	name := tag.Name

	switch r.typ(value) {
	case "arrow_function", "function_expression", "generator_function":
		if protoOwners[name] {
			return definitionFix{kind: "definition.class"}, true
		}
		return definitionFix{kind: "definition.function"}, true
	case "class":
		return definitionFix{kind: "definition.class"}, true
	case "call_expression":
		callee := r.calleeName(value)
		switch {
		case wrapperCallees[callee], r.isStyled(value):
			return definitionFix{kind: "definition.function"}, true
		case callee == "createContext":
			return definitionFix{kind: "definition.constant"}, true
		}
	}
	if !exported {
		// A pattern for call-valued constants matched a plain value.
		return definitionFix{drop: true}, true
	}
	if r.typ(decl) == "variable_declaration" || (decl.ChildCount() > 0 && r.typ(decl.Child(0)) == "let") {
		return definitionFix{kind: "definition.variable"}, true
	}
	return definitionFix{kind: "definition.constant"}, true
}

// refineScriptDefinitions is the refiner of the three script grammars.
func refineScriptDefinitions(root *ts.Node, lang *ts.Language, file string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	r := &scriptRefiner{lang: lang, src: src, file: file, root: root}
	fixes := make(map[uint32]definitionFix, 16)

	type pending struct {
		tag ts.Tag
		sn  scriptNode
	}
	var decls []pending

	// protoOwners are the names that carry `X.prototype.y = ...`: their
	// declarations are classes.
	protoOwners := map[string]bool{}

	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition") {
			continue
		}
		sn, ok := r.find(tag.Range.StartByte, tag.Range.EndByte)
		if !ok {
			continue
		}
		switch r.typ(sn.n) {
		case "assignment_expression":
			r.ensureTop()
			fix := r.refineAssignment(sn, tag)
			if !fix.drop && fix.owner != "" && strings.HasSuffix(strings.TrimSpace(string(src[sn.n.StartByte():tag.NameRange.StartByte])), ".prototype.") {
				protoOwners[fix.owner] = true
			}
			fixes[tag.NameRange.StartByte] = fix
		case "pair":
			fixes[tag.NameRange.StartByte] = r.refinePair(sn)
		case "lexical_declaration", "variable_declaration":
			decls = append(decls, pending{tag: tag, sn: sn})
		case "function_declaration", "generator_function_declaration":
			decls = append(decls, pending{tag: tag, sn: sn})
		}
	}
	for _, d := range decls {
		switch r.typ(d.sn.n) {
		case "function_declaration", "generator_function_declaration":
			if protoOwners[d.tag.Name] && r.typ(d.sn.n) == "function_declaration" {
				fixes[d.tag.NameRange.StartByte] = definitionFix{kind: "definition.class"}
			}
		default:
			if fix, ok := r.refineDeclaration(d.sn, d.tag, protoOwners); ok {
				fixes[d.tag.NameRange.StartByte] = fix
			}
		}
	}
	if len(fixes) == 0 {
		return nil
	}
	return fixes
}

// refineAssignment decides what `left = right` defines, or that it defines
// nothing.
func (r *scriptRefiner) refineAssignment(sn scriptNode, tag ts.Tag) definitionFix {
	drop := definitionFix{drop: true}
	left := r.field(sn.n, "left")
	right := r.field(sn.n, "right")
	if r.typ(left) != "member_expression" || right == nil {
		return drop
	}
	value := r.finalValue(right)
	kind := r.kindOfValue(value)
	if kind == "" {
		return drop
	}
	object := r.field(left, "object")
	property := r.text(r.field(left, "property"))
	level := r.statementLevel(sn.path)

	// X.prototype.name = function
	if r.typ(object) == "member_expression" {
		if r.text(r.field(object, "property")) == "prototype" {
			owner := r.identifier(r.field(object, "object"))
			if owner != "" && kind == "definition.function" {
				return definitionFix{kind: "definition.method", owner: owner}
			}
			return drop
		}
		// module.exports.name = ...
		if level && r.text(object) == "module.exports" {
			return definitionFix{kind: kind}
		}
		return drop
	}

	holder := r.identifier(object)
	switch {
	case holder == "":
		return drop
	case holder == "exports" && level:
		return definitionFix{kind: kind}
	case holder == "module" && property == "exports" && level:
		// The module itself. A named function or class is called what it is
		// called; an anonymous one is the file.
		if n := r.field(value, "name"); n != nil {
			return definitionFix{name: r.text(n), kind: kind}
		}
		if stem := stemName(r.file); stem != "" {
			return definitionFix{name: stem, kind: kind}
		}
		return drop
	case holder == "module", holder == "exports", holder == "this":
		return drop
	case level && r.top[holder] && kind == "definition.function":
		return definitionFix{kind: "definition.method", owner: holder}
	}
	return drop
}

// refinePair decides whether a function-valued entry of an object literal
// defines something: only the entries of `module.exports = { ... }`.
func (r *scriptRefiner) refinePair(sn scriptNode) definitionFix {
	if len(sn.path) < 3 {
		return definitionFix{drop: true}
	}
	object := sn.path[len(sn.path)-2]
	assign := sn.path[len(sn.path)-3]
	if r.typ(object) != "object" || r.typ(assign) != "assignment_expression" {
		return definitionFix{drop: true}
	}
	left := r.field(assign, "left")
	if strings.TrimSpace(r.text(left)) != "module.exports" || r.field(assign, "right") != object {
		return definitionFix{drop: true}
	}
	// The path to the assignment is the pair's path without the pair and the
	// object.
	if !r.statementLevel(sn.path[:len(sn.path)-2]) {
		return definitionFix{drop: true}
	}
	return definitionFix{kind: "definition.function"}
}

// scriptRequireImports records the path of every `require('p')` in a file,
// wherever it is called, as an import of that path. The names a top-level
// `require` binds are import_bindings.go's.
const scriptRequireImports = `
(call_expression
  function: (identifier) @_f
  arguments: (arguments . (string) @path .)
  (#eq? @_f "require"))
`

func init() {
	registerScriptLanguage("javascript", jsxReferences)
}
