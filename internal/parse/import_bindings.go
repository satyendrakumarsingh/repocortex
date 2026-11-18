package parse

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
)

// # Import bindings — which local names an import statement creates
//
// The import query records that a file imports `../core/util.js`. It does not
// record that the file then calls that module `util`, and a resolver that is
// not told has to guess what `util.joinValues(...)` is qualified by. The guess
// it made was a comparison of the receiver's name with a file's base name, and
// on zod — which has a `util.ts` in both its v3 and its v4 tree — that bound
// 121 v4 calls to the v3 file.
//
// An ECMAScript import statement states the binding outright, in three forms,
// and each is recorded the way pyscope.go records the Python equivalents:
//
//	import * as util from "./util.js"     Path "./util.js", Alias "util"
//	import def from "./helpers"           Path "./helpers", Alias ".def"
//	import { a, b as c } from "./m"       Path "./m", Alias ".a" and ".c"
//
// A namespace import names the MODULE, so it is a module alias, exactly like
// Python's `import x as y`. A default or a named import names something INSIDE
// the module, so it is a member, exactly like `from x import y` — the alias
// carries memberAliasPrefix. A renamed member is recorded under its LOCAL name:
// what the resolver needs from it is that a receiver spelled `c` came from
// that module, and where the module is not in the repository that is the whole
// answer.

// importBindingLanguages are the languages whose import statements this file
// reads. They share one grammar family and one statement shape.
var importBindingLanguages = map[string]bool{
	"javascript": true, "typescript": true, "tsx": true,
}

// walkedImports are the languages whose imports importedBindings reads from
// the tree, beside or instead of an import query. TierFor counts them as
// reading imports: Rust has no import query since its use lists became a
// walk (lang_rust_uses.go), and without this it fell to the parsed tier while
// its calls still bound across files. Keep it in step with the switch below.
var walkedImports = map[string]bool{"php": true, "rust": true, "scala": true, "solidity": true}

// ReadsImports reports whether the parser reads a language's imports, by an
// import query or by walking the tree.
func ReadsImports(language string) bool {
	return ImportQuery(language) != "" || walkedImports[language]
}

// importedBindings returns the local names a file's import statements bind,
// as Import rows to merge with the import query's. Nil for a language it does
// not read and for a tree it cannot walk.
func (p *genericParser) importedBindings(tree *ts.Tree, src []byte) (out []Import) {
	if tree != nil && p.entry.Name == "php" {
		return p.phpUses(tree, src)
	}
	if tree != nil && p.entry.Name == "rust" {
		return p.rustUses(tree, src)
	}
	if tree != nil && p.entry.Name == "scala" {
		return p.scalaImports(tree, src)
	}
	if tree != nil && p.entry.Name == "solidity" {
		return p.solidityBindings(tree, src)
	}
	if tree == nil || !importBindingLanguages[p.entry.Name] {
		return nil
	}
	defer func() { _ = recover() }()

	root := tree.RootNode()
	if root == nil {
		return nil
	}
	lang := p.lang
	add := func(path, alias string, at *ts.Node) {
		if path == "" || alias == "" || alias == memberAliasPrefix || at == nil {
			return
		}
		imp := Import{Path: directoryIndex(path), Alias: alias}
		imp.Range = model.Range{
			Start: model.Position{Line: int(at.StartPoint().Row) + 1, Col: int(at.StartPoint().Column)},
			End:   model.Position{Line: int(at.EndPoint().Row) + 1, Col: int(at.EndPoint().Column)},
		}
		out = append(out, imp)
	}

	for i := 0; i < root.ChildCount(); i++ {
		stmt := root.Child(i)
		if stmt != nil && stmt.IsNamed() {
			if t := stmt.Type(lang); t == "lexical_declaration" || t == "variable_declaration" {
				out = append(out, p.requiredBindings(stmt, src)...)
				continue
			}
		}
		if stmt == nil || !stmt.IsNamed() || stmt.Type(lang) != "import_statement" {
			continue
		}
		source := stmt.ChildByFieldName("source", lang)
		if source == nil {
			continue
		}
		path := unquote(source.Text(src))
		for j := 0; j < stmt.ChildCount(); j++ {
			clause := stmt.Child(j)
			if clause == nil || !clause.IsNamed() || clause.Type(lang) != "import_clause" {
				continue
			}
			for k := 0; k < clause.ChildCount(); k++ {
				c := clause.Child(k)
				if c == nil || !c.IsNamed() {
					continue
				}
				switch c.Type(lang) {
				case "identifier":
					// The default import.
					add(path, memberAliasPrefix+c.Text(src), c)
				case "namespace_import":
					for m := 0; m < c.ChildCount(); m++ {
						if id := c.Child(m); id != nil && id.IsNamed() && id.Type(lang) == "identifier" {
							add(path, id.Text(src), id)
						}
					}
				case "named_imports":
					for m := 0; m < c.ChildCount(); m++ {
						spec := c.Child(m)
						if spec == nil || !spec.IsNamed() || spec.Type(lang) != "import_specifier" {
							continue
						}
						local := spec.ChildByFieldName("alias", lang)
						original := spec.ChildByFieldName("name", lang)
						if local == nil {
							local = original
						}
						if local == nil {
							continue
						}
						alias := memberAliasPrefix + local.Text(src)
						// A renamed member carries the name the module exports
						// it by. See renamedMember.
						if original != nil && original != local && original.Text(src) != "default" {
							alias = renamedMember(local.Text(src), original.Text(src))
						}
						add(path, alias, local)
					}
				}
			}
		}
	}
	return out
}

// # A renamed import
//
//	import { Calendar as CalendarPrimitive } from "./cal"
//	<CalendarPrimitive />
//
// binds the local name CalendarPrimitive to the export Calendar of "./cal". The
// use keeps the name it is written with: it IS a use of CalendarPrimitive, and
// a different symbol of that name, or of the name Calendar, in this file or its
// neighbours is not what it reaches. What it reaches is stated by ONE row, and
// the resolver binds the use through it (viaRenamedImport in internal/resolve):
//
//	Path "./cal", Alias ".CalendarPrimitive=Calendar"
//
// The alias is the member form (".local") followed by "=" and the exported
// name. "=" cannot occur in an identifier, so a reader that does not know the
// suffix cannot mistake it for part of a name, and one that does splits on the
// first. A default import (`import X from`, `{ default as X }`) names the
// module's default, whose declared name this file cannot see, and carries no
// suffix.

// renamedMember is the Alias of a member imported under another name.
func renamedMember(local, exported string) string {
	return memberAliasPrefix + local + renameSeparator + exported
}

// renameSeparator joins the local name and the exported name in an Alias.
const renameSeparator = "="

// # CommonJS
//
// `require` is an ordinary call, so a file imports through it wherever it
// likes. Only the declarations at the top of the file are read, because those
// bind a name for the whole file, which is what a row says:
//
//	const x = require('./x')              Path "./x", Alias "x"
//	const { a, b: c } = require('./x')    Path "./x", Alias ".a" and ".c"
//	const y = require('./x').z            Path "./x", Alias ".y"
//
// The first names the MODULE, as `import * as x` does: `x.f()` is a call of f
// in that file, and a bare `x()` is a call of the file's own function (see
// bareByName). The other two name something INSIDE the module, as a named
// import does, recorded under the local name for the reason the ES form is:
// what the resolver needs is that the receiver came from that module. A
// rename carries the exported name as well (see renamedMember).
//
// A `require` inside a function binds a name only there, and two functions
// may bind one name to two modules, so it is left to the import query, which
// records the path alone.

// requiredBindings reads one top-level declaration for `require` bindings.
func (p *genericParser) requiredBindings(decl *ts.Node, src []byte) (out []Import) {
	lang := p.lang
	for i := 0; i < decl.NamedChildCount(); i++ {
		d := decl.NamedChild(i)
		if d == nil || d.Type(lang) != "variable_declarator" {
			continue
		}
		name := d.ChildByFieldName("name", lang)
		path, member, ok := requireTarget(d.ChildByFieldName("value", lang), lang, src)
		if !ok || name == nil {
			continue
		}
		switch name.Type(lang) {
		case "identifier":
			alias := name.Text(src)
			if member {
				alias = memberAliasPrefix + alias
				// `const y = require('./x').z` takes the export z under the
				// name y.
				if v := d.ChildByFieldName("value", lang); v != nil && v.Type(lang) == "member_expression" {
					prop := v.ChildByFieldName("property", lang)
					if _, inner, found := requireTarget(v.ChildByFieldName("object", lang), lang, src); found && !inner && prop != nil &&
						prop.Type(lang) == "property_identifier" && prop.Text(src) != name.Text(src) && prop.Text(src) != "default" {
						alias = renamedMember(name.Text(src), prop.Text(src))
					}
				}
			}
			out = appendBinding(out, path, alias, name)
		case "object_pattern":
			if member {
				continue
			}
			for j := 0; j < name.NamedChildCount(); j++ {
				c := name.NamedChild(j)
				switch c.Type(lang) {
				case "shorthand_property_identifier_pattern":
					out = appendBinding(out, path, memberAliasPrefix+c.Text(src), c)
				case "pair_pattern":
					local := c.ChildByFieldName("value", lang)
					if local != nil && local.Type(lang) == "assignment_pattern" {
						local = local.ChildByFieldName("left", lang)
					}
					if local != nil && local.Type(lang) == "identifier" {
						alias := memberAliasPrefix + local.Text(src)
						// `{ a: b }` takes the export a under the name b.
						if key := c.ChildByFieldName("key", lang); key != nil && key.Text(src) != local.Text(src) &&
							key.Text(src) != "default" && (key.Type(lang) == "property_identifier" || key.Type(lang) == "identifier") {
							alias = renamedMember(local.Text(src), key.Text(src))
						}
						out = appendBinding(out, path, alias, local)
					}
				case "object_assignment_pattern":
					if left := c.ChildByFieldName("left", lang); left != nil {
						out = appendBinding(out, path, memberAliasPrefix+left.Text(src), left)
					}
				}
			}
		}
	}
	return out
}

// requireTarget reads `require('p')` and `require('p').member`: the module's
// path as written, and whether a member of it was taken.
func requireTarget(v *ts.Node, lang *ts.Language, src []byte) (path string, member, ok bool) {
	if v == nil {
		return "", false, false
	}
	if v.Type(lang) == "member_expression" {
		prop := v.ChildByFieldName("property", lang)
		path, _, ok = requireTarget(v.ChildByFieldName("object", lang), lang, src)
		return path, true, ok && prop != nil
	}
	if v.Type(lang) != "call_expression" {
		return "", false, false
	}
	fn := v.ChildByFieldName("function", lang)
	args := v.ChildByFieldName("arguments", lang)
	if fn == nil || args == nil || fn.Type(lang) != "identifier" || fn.Text(src) != "require" || args.NamedChildCount() != 1 {
		return "", false, false
	}
	lit := args.NamedChild(0)
	if lit == nil || lit.Type(lang) != "string" {
		return "", false, false
	}
	path = unquote(lit.Text(src))
	return path, false, path != ""
}

// directoryIndex names the file a bare directory specifier loads: `require('..')`
// and `require('./')` are the index of that directory, and a path with no name
// in it is a path the resolver's import table has nothing to match.
func directoryIndex(path string) string {
	if strings.Trim(path, "./") != "" || !strings.HasPrefix(path, ".") {
		return path
	}
	return strings.TrimSuffix(path, "/") + "/index"
}

// appendBinding adds one Import row positioned at the node that names it.
func appendBinding(out []Import, path, alias string, at *ts.Node) []Import {
	if path == "" || alias == "" || alias == memberAliasPrefix || at == nil {
		return out
	}
	imp := Import{Path: directoryIndex(path), Alias: alias}
	imp.Range = model.Range{
		Start: model.Position{Line: int(at.StartPoint().Row) + 1, Col: int(at.StartPoint().Column)},
		End:   model.Position{Line: int(at.EndPoint().Row) + 1, Col: int(at.EndPoint().Column)},
	}
	return append(out, imp)
}
