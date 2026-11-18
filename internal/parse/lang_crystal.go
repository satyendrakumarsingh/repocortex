package parse

import (
	"path"
	"regexp"
	"strings"
)

// Crystal. What the index holds, see fixtures/langs/crystal/expected.tsv.
//
// The inferred query names a method after the last identifier of its body as
// well as its own name (`def total ... helper(1) ... end` was two methods), and
// qualifies nothing. It is replaced.
//
//   - class, struct and enum are classes, module a module, `alias X = T` a
//     type_alias, and a constant (`MAX = 10`) a constant. A name written with a
//     namespace (`class Shop::Cart`) is the last segment, and the rest is what
//     it is a member of;
//   - `def` is a method in the body of a class, struct, module or enum (`def self.f`
//     included, and an operator is named by its symbol) and a function anywhere
//     else; `abstract def` is a method;
//   - `@items : T` in a class body is a field.
//
// Not definitions: `getter`, `property` and `setter` (macro calls that generate
// methods), a macro, a lib or a fun, a local variable, a block parameter.
//
// Types and modules are open and global by name (`class Cart` in two files is
// one class), so a definition is qualified by the namespaces around it
// (`Shop.Cart.total`, see ownersBy) and a call finds a type by its name from any
// file. `require "./x"` and `require "x"` order what a program compiles and are read
// as imports (crystalImport), which is what makes one file's definitions known to
// another; the standard library (`require "json"`) names no file here.
//
// Calls: `f(x)`, `x.f(y)`, `A::B.f(x)` and `A.new(x)`; a call made without
// parentheses (`puts "a"`) is one when it has arguments. A call with neither
// (`items.size`) is a `call` node as well, and so is `x.f` with no arguments.
func init() {
	registerDefinitions("crystal", crystalDefinitions+crystalCalls, true)
	registerNameFix("crystal", func(name string) string {
		if i := strings.LastIndex(name, "::"); i >= 0 {
			return name[i+2:]
		}
		return name
	})
	registerImportFix("crystal", crystalImport)
	definitionRefiners["crystal"] = ownersBy(map[string]string{
		"class_def": "constant", "struct_def": "constant", "module_def": "constant", "enum_def": "constant",
	})
}

const crystalDefinitions = `
(class_def "class" . (constant) @name) @definition.class
(struct_def "struct" . (constant) @name) @definition.class
(enum_def "enum" . (constant) @name) @definition.class
(module_def "module" . (constant) @name) @definition.module
(alias "alias" . (constant) @name) @definition.type_alias
(const_assign . (constant) @name) @definition.constant

(source_file (method_def "def" . [(identifier) (operator)] @name) @definition.function)
(source_file (method_def "def" . (self) . "." . (identifier) @name) @definition.function)
(class_def (method_def "def" . [(identifier) (operator)] @name) @definition.method)
(class_def (method_def "def" . (self) . "." . (identifier) @name) @definition.method)
(struct_def (method_def "def" . [(identifier) (operator)] @name) @definition.method)
(struct_def (method_def "def" . (self) . "." . (identifier) @name) @definition.method)
(module_def (method_def "def" . [(identifier) (operator)] @name) @definition.method)
(module_def (method_def "def" . (self) . "." . (identifier) @name) @definition.method)
(enum_def (method_def "def" . [(identifier) (operator)] @name) @definition.method)
(abstract_method_def "def" . (identifier) @name) @definition.method

(class_def (type_declaration . (instance_var) @name) @definition.field)
(struct_def (type_declaration . (instance_var) @name) @definition.field)
`

const crystalCalls = `
(call "." . (identifier) @name) @reference.call
(call . (identifier) @name . [(argument_list) (block)]) @reference.call
`

// crystalImport turns a `require` into a row.
//
//	require "./money"          src/money                      the file, relative to this one
//	require "../lib/util"      lib/util                       likewise, cleaned
//	require "./shop/*"         src/shop/*                     every file of the directory
//	require "./shop/**"        src/shop/**                    every file beneath it
//	require "shop/util"        shop/util                      a shard, or a file under src/
//	require "json"             json                           the standard library: no file here
//
// The path of a relative one is the repository path of the file it names, without
// extension. A path that is not relative is kept as written: the resolver finds
// it as `src/shop/util`, `lib/*/src/shop/util` or a file ending that way.
func crystalImport(filePath string, imp Import) []Import {
	m := crystalRequire.FindStringSubmatch(strings.TrimSpace(imp.Path))
	if m == nil {
		return nil
	}
	target := strings.TrimSuffix(m[1], ".cr")
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		target = path.Join(path.Dir(filePath), target)
		if target == "" || target == "." || strings.HasPrefix(target, "..") {
			return nil
		}
	} else {
		target = path.Clean(target)
	}
	imp.Path, imp.Alias = target, ""
	return []Import{imp}
}

var crystalRequire = regexp.MustCompile(`^require\s+"([^"]+)"`)
