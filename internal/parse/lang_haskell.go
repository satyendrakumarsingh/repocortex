package parse

import (
	"regexp"
	"strings"
)

// Haskell. What the index holds, see fixtures/langs/haskell/expected.tsv.
//
//   - module M is a package;
//   - a top-level equation is a function: `f x = ...`, `f = ...`, an operator
//     definition (`a |+| b = ...`) and a back-quoted one (x `plus` y = ...).
//     A function of several equations is written as one `function` node per
//     equation; consecutive equations of one name are folded into one symbol.
//     The type signature is not a definition of its own: the equation is, so a
//     function is held once whether or not it carries a signature, and a
//     point-free value (`total = sum . map area`) is a function too, since
//     without type inference the two cannot be told apart;
//   - `foreign import` names a function;
//   - data, newtype and data family are classes; type synonyms and type
//     families are type aliases; class is an interface, and the methods
//     declared in its body (signature lines) are methods;
//   - the fields of a record constructor are fields.
//
// Instance bodies, where-bound and let-bound names, data constructors,
// pattern synonyms and the signatures themselves are not definitions.
//
// Calls. Haskell applies by juxtaposition, so a call is the function position of
// an `apply` (`slug xs`), a qualified name wherever it is written (`Money.add`:
// a qualified name cannot be a local variable, so it is a use of the function of
// that module even as an argument), and a function on either side of the
// operators that exist to apply or compose (`f $ x`, `f . g`, `f <$> xs`,
// `m >>= f`) or in backquotes (x `plus` y). A name passed as an argument
// (`map total xs`) is not tagged: without scopes it cannot be told from a
// parameter of the same name.
//
// Imports are read from the statement's text (see haskellImport): the module
// goes in Path and what the statement does with it in Alias, in the encoding
// lang_modules.go describes, because a call reaches a function through the
// MODULE it is in and `import qualified A.B as C` says which qualifier names it.
func init() {
	registerDefinitions("haskell", haskellCalls+haskellDefinitions, true)
	registerClauseMerge("haskell", nil)
	registerImportFix("haskell", haskellImport)
}

const haskellCalls = `
(apply function: (variable) @name) @reference.call
(qualified module: (module) id: (variable) @name) @reference.call
(infix left_operand: (variable) @name @reference.call operator: (operator) @_op
  (#any-of? @_op "$" "." "<$>" "<$" "=<<" "<&>" ">>>" "<<<"))
(infix operator: (operator) @_op right_operand: (variable) @name @reference.call
  (#any-of? @_op "." ">>=" "&" ">>>" "<<<"))
(infix_id (variable) @name @reference.call)
`

// haskellImport reads one import statement into the row that names the module
// and what the statement does with it.
//
//	import A.B                       A.B, import           every name of A.B, qualified or not
//	import A.B (f, g)                A.B, import:f,g
//	import A.B hiding (f)            A.B, hiding:f
//	import qualified A.B as C       A.B, alias:C          `C.f` names A.B's f
//	import A.B as C                  A.B, import;alias:C   both
//	import qualified A.B            A.B                   `A.B.f` only
//
// What the lists hold is the lower-case names, the functions: an operator, a
// type or a constructor is not a call this resolver binds. A name listed under a
// type (`Shape(area, perimeter)`) is a class method, and is kept.
func haskellImport(_ string, imp Import) []Import {
	m := haskellImportStmt.FindStringSubmatch(imp.Path)
	if m == nil {
		return nil
	}
	qualified := m[1] != "" || m[3] != ""
	module, as, hiding, list := m[2], m[4], m[5] != "", m[6]

	var directives []string
	switch {
	case qualified:
	case list == "" && !hiding:
		directives = append(directives, ModuleImportAll)
	case hiding:
		if names := haskellNames(list); len(names) > 0 {
			directives = append(directives, ModuleHidingPrefix+strings.Join(names, ","))
		} else {
			directives = append(directives, ModuleImportAll)
		}
	default:
		if names := haskellNames(list); len(names) > 0 {
			directives = append(directives, ModuleImportPrefix+strings.Join(names, ","))
		}
	}
	if as != "" {
		directives = append(directives, ModuleAliasPrefix+as)
	}
	imp.Path = module
	imp.Alias = moduleDirectives(directives...)
	return []Import{imp}
}

var haskellImportStmt = regexp.MustCompile(`(?s)^import\s+(?:\{-#.*?#-\}\s+)?(?:safe\s+)?(qualified\s+)?(?:"[^"]*"\s+)?([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*(qualified\s+)?(?:as\s+([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*)?(hiding\s*)?(\(.*\))?`)

// haskellNames is the functions an import list names: its lower-case entries,
// and the lower-case names inside a type's own list.
func haskellNames(list string) []string {
	list = strings.TrimSpace(list)
	list = strings.TrimPrefix(list, "(")
	list = strings.TrimSuffix(list, ")")
	var names []string
	depth, start := 0, 0
	flush := func(end int) {
		entry := strings.TrimSpace(list[start:end])
		if i := strings.IndexByte(entry, '('); i > 0 && entry[0] != '(' {
			// `Shape(area, perimeter)`: the class's methods or the record's fields.
			inner := strings.TrimSuffix(entry[i+1:], ")")
			for _, n := range strings.Split(inner, ",") {
				n = strings.TrimSpace(n)
				if haskellVariable(n) {
					names = append(names, n)
				}
			}
			return
		}
		if haskellVariable(entry) {
			names = append(names, entry)
		}
	}
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	flush(len(list))
	return names
}

// haskellVariable reports whether an import entry is a lower-case name.
func haskellVariable(s string) bool {
	if s == "" || s == ".." {
		return false
	}
	c := s[0]
	if c != '_' && (c < 'a' || c > 'z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '_' && c != '\'' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

const haskellDefinitions = `
(header (module) @name) @definition.module

(declarations (function . (variable) @name) @definition.function)
(declarations (bind . (variable) @name) @definition.function)
(declarations (function . (infix (_) . (operator) @name)) @definition.function)
(declarations (function . (infix (_) . (infix_id (variable) @name))) @definition.function)
(declarations (foreign_import (signature . (variable) @name)) @definition.function)

(declarations (data_type . (name) @name) @definition.class)
(declarations (newtype . (name) @name) @definition.class)
(declarations (data_family . (name) @name) @definition.class)
(declarations (type_synomym . (name) @name) @definition.type_alias)
(declarations (type_family . (name) @name) @definition.type_alias)
(declarations (class (name) @name . (type_params)) @definition.interface)
(class_declarations (signature . (variable) @name) @definition.method)

(field (field_name (variable) @name)) @definition.field
`
