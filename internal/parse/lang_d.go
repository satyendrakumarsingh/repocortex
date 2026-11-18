package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// D. What the index holds, see fixtures/langs/d/expected.tsv.
//
// The inferred query takes a function wherever it is and names it a function even
// when it is a member of a class, reads every enum member as a function, and knows
// no constructor, no alias and no module-level variable. It is replaced.
//
//   - a class, a struct or a union is a class, an interface an interface, an enum
//     a class (its members are not definitions) and an `alias X = T` a type_alias;
//   - a function is a function, and a method when it is declared in the body of a
//     class, a struct, a union or an interface; a constructor is the method `this`;
//   - a variable written at the top of the module is a variable (`auto x = 1;`,
//     `int n;`) and one written in the body of an aggregate is a field. A `const`
//     or `immutable` one is a constant. A local of a function is not a definition.
//
// Not definitions: parameters, enum members, a `unittest` block (its calls belong
// to the file), a destructor, template parameters.
//
// A module is a file: `module a.b;` names the file a/b.d under some source
// directory the code does not state. `import a.b;` brings every public name of the
// module in, `import a.b : f;` the listed ones, `import x = a.b;` binds a qualifier
// and nothing bare, `static import a.b;` the qualified names only, and `public
// import a.b;` also re-exports it to whoever imports this module (dImport). A
// definition marked `private`, in a `private:` section or not, is the module's own
// and is flagged private (private.go).
//
// Calls: `f(x)`, `x.f(y)` (a method, or a free function in uniform function-call
// syntax, the first argument written first), `a.b.f(x)` and `f!T(x)`. A `new T(x)`
// is the construction of T.
func init() {
	registerDefinitions("d", dDefinitions+dCalls, true)
	registerImportFix("d", dImport)
	// The grammar names no field on a type declaration, so shape.go's walk finds no
	// owner: a member is qualified by the aggregates around it here (`Cart.pub`).
	definitionRefiners["d"] = ownersBy(map[string]string{
		"class_declaration": "identifier", "struct_declaration": "identifier",
		"union_declaration": "identifier", "interface_declaration": "identifier",
	})
}

const dDefinitions = `
(class_declaration (identifier) @name) @definition.class
(struct_declaration (identifier) @name) @definition.class
(union_declaration (identifier) @name) @definition.class
(interface_declaration (identifier) @name) @definition.interface
(enum_declaration (identifier) @name) @definition.class
(alias_declaration (alias_initializer (identifier) @name)) @definition.type_alias

(function_declaration (identifier) @name) @definition.function
(aggregate_body (function_declaration (identifier) @name) @definition.method)
(aggregate_body (constructor (this) @name) @definition.method)

(module_def (variable_declaration (declarator (identifier) @name)) @definition.variable)
(module_def (auto_declaration (identifier) @name) @definition.variable)
(source_file (variable_declaration (declarator (identifier) @name)) @definition.variable)
(source_file (auto_declaration (identifier) @name) @definition.variable)
(aggregate_body (variable_declaration (declarator (identifier) @name)) @definition.field)
`

const dCalls = `
(call_expression (identifier) @name) @reference.call
(call_expression (property_expression (identifier) @name .)) @reference.call
(call_expression (property_expression (template_instance . (identifier) @name))) @reference.call
(call_expression (template_instance . (identifier) @name)) @reference.call
(call_expression (type (template_instance . (identifier) @name))) @reference.call
(call_expression (new_expression (type (identifier) @name))) @reference.call.new
`

// dImport turns an import declaration into rows. The module's name is stored
// with its dots written as slashes, the form a path comparison reads.
//
//	import a.b;                 a/b, import                 every public name of a/b is in scope,
//	                                                        and `a.b.f` names its f
//	import a.b : f, g;          a/b, import:f,g            the listed names only (of the last module
//	                                                        of a statement that names several)
//	import x = a.b;             a/b, alias:x               `x.f` names a/b's f, nothing is bare
//	static import a.b;          a/b, static                qualified names only
//	public import a.b;          a/b, import and a/b, export   whoever imports this module reaches a/b
//
// A module of the standard library or the runtime (`std.`, `core.`, `etc.`) is no
// file of the repository: dropped. An import statement of several modules
// (`import a.b, c.d;`) is one row for each.
func dImport(_ string, imp Import) []Import {
	text := strings.Join(strings.Fields(imp.Path), " ")
	m := dImportStmt.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	public, static := m[1] == "public", m[2] == "static"
	body, binds, _ := strings.Cut(m[3], ":")
	var rows []Import
	add := func(module, alias string) {
		rows = append(rows, Import{Path: module, Alias: alias, Range: imp.Range})
	}
	items := strings.Split(body, ",")
	for i, item := range items {
		item = strings.TrimSpace(item)
		// `import a, b : f;` binds the names of the last module: the list follows it.
		binds := binds
		if i < len(items)-1 {
			binds = ""
		}
		local := ""
		if l, mod, ok := strings.Cut(item, "="); ok {
			local, item = strings.TrimSpace(l), strings.TrimSpace(mod)
		}
		if !dModule.MatchString(item) {
			continue
		}
		switch first, _, _ := strings.Cut(item, "."); first {
		case "std", "core", "etc", "object":
			continue
		}
		module := strings.ReplaceAll(item, ".", "/")
		switch {
		case local != "":
			add(module, ModuleAliasPrefix+local)
		case static:
			add(module, "static")
		case binds != "":
			var names []string
			for _, b := range strings.Split(binds, ",") {
				// `g = f` brings f in under another name, which a bare call here does not write.
				if b = strings.TrimSpace(b); dIdent.MatchString(b) {
					names = append(names, b)
				}
			}
			if len(names) > 0 {
				add(module, ModuleImportPrefix+strings.Join(names, ","))
			} else {
				add(module, "static")
			}
		default:
			add(module, ModuleImportAll)
		}
		if public {
			add(module, ModuleExport)
		}
	}
	return rows
}

var (
	dImportStmt = regexp.MustCompile(`^(?:(public|private|package|export)\s+)?(?:(static)\s+)?import\s+(.*?)\s*;?$`)
	dModule     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	dIdent      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// dPrivate reports whether a definition is private to its module: it carries the
// `private` attribute itself, or follows a `private:` section marker in the
// same body and no later `public:` (or other) one.
func dPrivate(def *ts.Node, lang *ts.Language, src []byte) bool {
	for i := 0; i < def.ChildCount(); i++ {
		if c := def.Child(i); c != nil && c.Type(lang) == "private" {
			return true
		}
		if c := def.Child(i); c != nil && c.Type(lang) == "public" {
			return false
		}
	}
	parent := def.Parent()
	if parent == nil {
		return false
	}
	private := false
	for i := 0; i < parent.ChildCount(); i++ {
		c := parent.Child(i)
		if c == nil || c == def {
			break
		}
		if c.Type(lang) == "attribute_declaration" {
			private = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(c.Text(src)), ":")) == "private"
		}
	}
	return private
}
