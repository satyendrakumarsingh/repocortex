package parse

import (
	"path"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Solidity. What the index holds, see fixtures/langs/solidity/expected.tsv:
//
//   - contract (also abstract) and library are a class, and so are a struct
//     and an enum, at file level or inside a contract; interface is an
//     interface;
//   - function is a function, free or in a contract, interface or library;
//     event and custom error declarations are a function (they are called by
//     emit and revert);
//   - modifier, constructor, receive and fallback are a method: they exist
//     only inside a contract and are not called by name;
//   - `type Price is uint256;` is a type alias;
//   - a constant or immutable, in a contract or at file level, is a constant;
//     any other state variable is a field.
//
// Struct members, enum values, parameters, local variables and `using`
// directives are not definitions.
func init() {
	registerDefinitions("solidity", solidityDefinitions, true)
	registerImportFix("solidity", solidityImport)
}

// solidityImport rewrites an import path to the repository path of the file it
// names, without the extension, which is what the resolver compares with a
// file's.
//
//	"./A.sol"                  relative to the importing file's directory
//	"../lib/B.sol"             likewise, cleaned
//	"src/C.sol"                as written: Foundry and Hardhat both resolve a
//	                           path that is not relative from the project root
//	"@oz/token/ERC20.sol"      as written: a remapping or a package, which the
//	                           resolver finds only if a file of the repository
//	                           ends in that path, and otherwise calls external
//
// A path that climbs out of the repository names no file in it and is dropped.
func solidityImport(filePath string, imp Import) []Import {
	p := imp.Path
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
		p = path.Join(path.Dir(filePath), p)
	}
	p = strings.TrimSuffix(path.Clean(p), ".sol")
	if p == "" || p == "." || strings.HasPrefix(p, "..") {
		return nil
	}
	imp.Path = p
	return []Import{imp}
}

// solidityBindings reads the names an import directive binds, which the import
// query (the path alone) cannot:
//
//	import * as L from "./L.sol";        Path "./L.sol", Alias "L"
//	import "./L.sol" as L;               Path "./L.sol", Alias "L"
//	import {A, B as C} from "./x.sol";   Path "./x.sol", Alias ".A" and ".C=B"
//
// A namespace names the MODULE, so `L.f()` is a call of f in that file; a
// named import names something INSIDE the module, recorded under its local
// name, and a renamed one carries the exported name as well (renamedMember).
// The forms are the ECMAScript ones the same rows were designed for
// (import_bindings.go). A plain `import "./A.sol";` binds nothing by name: it
// brings every global of the file into scope, which the path row says.
func (p *genericParser) solidityBindings(tree *ts.Tree, src []byte) (out []Import) {
	defer func() { _ = recover() }()
	root := tree.RootNode()
	if root == nil {
		return nil
	}
	lang := p.lang
	for i := 0; i < root.ChildCount(); i++ {
		d := root.Child(i)
		if d == nil || !d.IsNamed() || d.Type(lang) != "import_directive" {
			continue
		}
		var dest string
		type bound struct {
			alias string
			at    *ts.Node
		}
		var rows []bound
		mode, sawAs := "", false
		var last *ts.Node // the exported name awaiting its local one
		flush := func() {
			if last != nil {
				rows = append(rows, bound{memberAliasPrefix + last.Text(src), last})
				last = nil
			}
		}
		for j := 0; j < d.ChildCount(); j++ {
			c := d.Child(j)
			if c == nil {
				continue
			}
			switch c.Type(lang) {
			case "*":
				mode = "namespace"
			case "{":
				mode = "named"
			case "}":
				flush()
			case "as":
				sawAs = true
			case "string":
				dest = unquote(c.Text(src))
			case "identifier":
				name := c.Text(src)
				switch {
				case mode == "named" && sawAs && last != nil:
					rows = append(rows, bound{renamedMember(name, last.Text(src)), c})
					last, sawAs = nil, false
				case mode == "named":
					flush()
					last = c
				case sawAs:
					// `* as L`, or `"./L.sol" as L`.
					rows = append(rows, bound{name, c})
					sawAs = false
				}
			}
		}
		flush()
		for _, r := range rows {
			out = appendBinding(out, dest, r.alias, r.at)
		}
	}
	return out
}

const solidityDefinitions = `
(contract_declaration name: (identifier) @name) @definition.class
(library_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.class
(enum_declaration name: (identifier) @name) @definition.class
(function_definition name: (identifier) @name) @definition.function
(event_definition name: (identifier) @name) @definition.function
(error_declaration name: (identifier) @name) @definition.function
(modifier_definition name: (identifier) @name) @definition.method
(constructor_definition "constructor" @name) @definition.method
(fallback_receive_definition ["receive" "fallback"] @name) @definition.method
(user_defined_type_definition name: (identifier) @name) @definition.type_alias
(constant_variable_declaration name: (identifier) @name) @definition.constant

((state_variable_declaration name: (identifier) @name) @definition.constant
  (#match? @definition.constant "^[^=;]*\\b(constant|immutable)\\b"))

((state_variable_declaration name: (identifier) @name) @definition.field
  (#not-match? @definition.field "^[^=;]*\\b(constant|immutable)\\b"))
`
