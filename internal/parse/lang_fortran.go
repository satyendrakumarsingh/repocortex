package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Fortran: program units (program, module, submodule, block data),
// procedures (function, subroutine, wherever they sit: in a module, after
// `contains`, in an interface block, or at file level), derived types, generic
// interfaces, type-bound procedure bindings, and the variables and named
// constants a module declares. Variables of a procedure or of a main program
// are locals and are not extracted.
//
// The name of every unit is the `name` of its opening statement.
//
// Calls: `call sub(x)` and `f(x)`. The grammar reads an array element `a(i)` as the
// same node as a function reference, and the resolver binds a name only to a
// procedure a `use` brings in or the file defines, so an array is no call of a
// procedure unless it shares its name with one that is in scope (a program may not).
// `call obj%method(x)` names a type-bound procedure by a value and is not a call of
// a name; it is left to the receiver rules and binds to nothing.
//
// Every definition is qualified by the program unit around it (`shapes.area` for
// `area` in `module shapes`, see fortranOwners), because a call reaches a procedure
// through the module a `use` names. A procedure with no unit around it is an
// external procedure, called by its name from anywhere in the program.
func init() {
	definitionRefiners["fortran"] = fortranOwners
	registerImportFix("fortran", fortranImport)
	registerDefinitions("fortran", `
(call_expression (identifier) @name) @reference.call
(subroutine_call (identifier) @name @reference.call)

(program (program_statement (name) @name)) @definition.module
(module (module_statement (name) @name)) @definition.module
(submodule (submodule_statement . (module_name) (name) @name)) @definition.module
(block_data (block_data_statement (name) @name)) @definition.module

(function (function_statement (name) @name)) @definition.function
(subroutine (subroutine_statement (name) @name)) @definition.function

(derived_type_definition (derived_type_statement (type_name) @name)) @definition.class
(interface (interface_statement (name) @name)) @definition.interface

(derived_type_procedures (procedure_statement (binding (binding_name (identifier) @name)) @definition.method))

(module (variable_declaration [(identifier) @name (init_declarator . (identifier) @name) (sized_declarator . (identifier) @name)]) @definition.variable)
(module (variable_declaration (type_qualifier) @_q [(identifier) @name (init_declarator . (identifier) @name) (sized_declarator . (identifier) @name)]) @definition.constant
  (#match? @_q "(?i)^parameter$"))
`, true)
}

// FortranInterfaceSegment and fortranLocalsSegment are the owners of a procedure
// declared in an interface block (an external or a dummy procedure's interface,
// which declares a procedure and does not define one) and of an internal
// procedure (one after the `contains` of another procedure). Neither can be named
// by a `use`.
const (
	FortranInterfaceSegment = "<interface>"
	fortranLocalsSegment    = "<locals>"

	// FortranPrivateSegment follows a module's name in the owner of a procedure the
	// module does not export: `private` by default, or named in a `private ::`
	// statement. A `use` of the module does not bring it in; the module's own code
	// still calls it.
	FortranPrivateSegment = "<private>"
)

// fortranOwners says which program unit every definition is in: the module,
// submodule or program around it, a host procedure's `<locals>` for an internal
// procedure, and the derived type for a type-bound procedure. A unit is qualified
// by the ones around it and not by itself.
func fortranOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		var chain []string
		for p := n; p != nil; p = p.Parent() {
			typ := p.Type(lang)
			switch typ {
			case "module", "submodule", "program", "derived_type_definition", "function", "subroutine", "module_procedure":
				name := fortranUnitName(p, lang, src, typ)
				if name == nil || name.StartByte() == tag.NameRange.StartByte {
					continue
				}
				switch typ {
				case "function", "subroutine", "module_procedure":
					chain = append([]string{name.Text(src), fortranLocalsSegment}, chain...)
				case "module":
					if len(chain) == 0 && kindFromTag(tag.Kind) == model.KindFunction &&
						!fortranExports(p, lang, src, tag.Name) {
						chain = append([]string{FortranPrivateSegment}, chain...)
					}
					chain = append([]string{name.Text(src)}, chain...)
				default:
					chain = append([]string{name.Text(src)}, chain...)
				}
			case "interface":
				// The interface a generic name declares (`interface area`) is a
				// definition of its module; what its body declares is not.
				if name := fortranUnitName(p, lang, src, typ); name != nil && name.StartByte() == tag.NameRange.StartByte {
					continue
				}
				// The interface body of a procedure that a module implements in a
				// submodule (`module subroutine bar()`) is that module's procedure.
				if !fortranModuleProcedureBody(n, p, lang, src) {
					chain = append([]string{FortranInterfaceSegment}, chain...)
				}
			}
		}
		if len(chain) == 0 {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fixes[tag.NameRange.StartByte] = definitionFix{owner: strings.Join(chain, ".")}
	}
	return fixes
}

// fortranExports reports whether a module's own `use`rs see a name: every name,
// unless the module says `private` with nothing after it, and then only the ones a
// `public ::` statement lists. A `private :: name` statement hides one either way.
func fortranExports(module *ts.Node, lang *ts.Language, src []byte, name string) bool {
	defaultPrivate := false
	listed := map[string]bool{}
	for i := 0; i < module.ChildCount(); i++ {
		stmt := module.Child(i)
		if stmt == nil {
			continue
		}
		typ := stmt.Type(lang)
		if typ != "public_statement" && typ != "private_statement" {
			continue
		}
		items := 0
		for j := 0; j < stmt.ChildCount(); j++ {
			if c := stmt.Child(j); c != nil && c.IsNamed() {
				items++
				if c.Type(lang) == "identifier" {
					listed[strings.ToLower(c.Text(src))] = typ == "public_statement"
				}
			}
		}
		if items == 0 && typ == "private_statement" {
			defaultPrivate = true
		}
	}
	if public, ok := listed[strings.ToLower(name)]; ok {
		return public
	}
	return !defaultPrivate
}

// fortranModuleProcedureBody reports whether a definition inside an interface
// block is a `module` procedure's interface body.
func fortranModuleProcedureBody(name, iface *ts.Node, lang *ts.Language, src []byte) bool {
	for p := name; p != nil && p != iface; p = p.Parent() {
		switch p.Type(lang) {
		case "function", "subroutine":
			for i := 0; i < p.ChildCount(); i++ {
				stmt := p.Child(i)
				if stmt == nil || !strings.HasSuffix(stmt.Type(lang), "_statement") {
					continue
				}
				for j := 0; j < stmt.ChildCount(); j++ {
					if q := stmt.Child(j); q != nil && q.Type(lang) == "procedure_qualifier" &&
						strings.EqualFold(strings.TrimSpace(q.Text(src)), "module") {
						return true
					}
				}
				return false
			}
			return false
		}
	}
	return false
}

// fortranUnitName is the name node of a unit's opening statement.
func fortranUnitName(unit *ts.Node, lang *ts.Language, src []byte, typ string) *ts.Node {
	for i := 0; i < unit.ChildCount(); i++ {
		stmt := unit.Child(i)
		if stmt == nil || !strings.HasSuffix(stmt.Type(lang), "_statement") {
			continue
		}
		want := "name"
		if typ == "derived_type_definition" {
			want = "type_name"
		}
		for j := 0; j < stmt.ChildCount(); j++ {
			if c := stmt.Child(j); c != nil && c.Type(lang) == want {
				return c
			}
		}
		return nil
	}
	return nil
}

// The import rows of a Fortran file name a module, lowercased because the language
// does not tell cases apart, and what the statement brings in (lang_modules.go):
//
//	use shapes                       shapes, import           every public name
//	use shapes, only: area, circle   shapes, import:area,circle
//	use shapes, only: a => b         (a name brought in under another is not listed)
//	use shapes, loc => area          shapes, import           every public name, one renamed
//	use, intrinsic :: iso_c_binding  no row: not a module of the repository
//
// `use` of a name by another name binds nothing: a call is looked up by what the
// source calls it.
var (
	fortranUse    = regexp.MustCompile(`(?is)^use\s*(,\s*((?:non_)?intrinsic)\s*)?(?:::)?\s*([a-z_][a-z0-9_]*)\s*(?:,\s*(.*))?$`)
	fortranOnly   = regexp.MustCompile(`(?is)^only\s*:(.*)$`)
	fortranIdent  = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	fortranRemark = regexp.MustCompile(`![^\n]*`)
)

func fortranImport(_ string, imp Import) []Import {
	text := fortranRemark.ReplaceAllString(imp.Path, "")
	// A continuation (`&` at the end of a line, and again at the start of the next).
	text = strings.NewReplacer("&", " ", "\n", " ", "\r", " ").Replace(text)
	text = strings.TrimSpace(text)
	m := fortranUse.FindStringSubmatch(text)
	if m == nil || strings.EqualFold(m[2], "intrinsic") {
		return nil
	}
	module, rest := strings.ToLower(m[3]), strings.TrimSpace(m[4])
	imp.Path, imp.Alias = module, ModuleImportAll
	if only := fortranOnly.FindStringSubmatch(rest); only != nil {
		var listed []string
		for _, item := range strings.Split(only[1], ",") {
			item = strings.ToLower(strings.TrimSpace(item))
			if fortranIdent.MatchString(item) {
				listed = append(listed, item)
			}
		}
		imp.Alias = ""
		if len(listed) > 0 {
			imp.Alias = ModuleImportPrefix + strings.Join(listed, ",")
		}
	}
	return []Import{imp}
}
