package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Zig: functions, container types, constants, variables, fields and tests.
//
// Everything Zig declares at file level or inside a container is a
// `variable_declaration` whose value says what it is, so the rules read the
// value:
//   - a value that is a struct, enum, union or opaque, or an error set (also
//     one merged with `||`), is a `class`;
//   - `const T = u64;`, a function type, and a TitleCase name bound to a name,
//     a field access or a call (`const Allocator = mem.Allocator;`,
//     `const Map = std.AutoHashMap(K, V);`) are `type_alias`. Zig has no
//     syntax that tells a type from a value; TitleCase is the language's own
//     convention for types, and a snake_case alias (`const mem = std.mem;`)
//     is a `constant`;
//   - any other `const` is a `constant` and any `var` (also `threadlocal`) a
//     `variable`. `@import("...")` bindings and `const Self = @This();` are
//     not recorded: they are in every file and name nothing new.
//
// A function is a `function`, or a `method` when it is declared inside a
// struct, enum or union (including a type declared inside a function body,
// whose members are still members). A struct or union field is a `field`; an enum
// variant is a `constant`. A string-named `test "..." {}` block is a
// `function` named `test "title"` (the prefix keeps it apart from the function it tests); `test someFn {}` (a doc test, which names an
// existing function) and `comptime {}` blocks are not recorded. The members
// of an error set are not recorded.
//
// The fields of a file that is itself a struct are written at file level and
// are recorded as fields. Declarations inside a function body are locals and
// are not recorded.
func init() {
	var q strings.Builder
	q.WriteString(`
; Calls: f(x), a.f(x) and a.b.f(x). The call node begins at the receiver, so the text
; before the name is how shape.go reads what the call was made on. A builtin
; (@import, @as, @intCast) is its own node and is not a call.
(call_expression function: (identifier) @name) @reference.call
(call_expression function: (field_expression member: (identifier) @name)) @reference.call

(function_declaration . (identifier) @name) @definition.function
(function_declaration . (string) . (identifier) @name) @definition.function

(test_declaration (string)) @name @definition.function

`)

	// Members of a container. A container is a declared type (a const or var at
	// file or container level), the value returned by a function, or the
	// receiver of a field access on one (`return struct { ... }.f;`). A
	// container written as the type of a local variable (`var state: enum {
	// a, b } = .a;`) is a local and its members are not recorded.
	contexts := []string{}
	for _, p := range []string{"source_file", "struct_declaration", "enum_declaration", "union_declaration"} {
		contexts = append(contexts, "("+p+" (variable_declaration . (identifier) %s))")
	}
	contexts = append(contexts, "(return_expression %s)", "(return_expression (field_expression %s))")
	for _, ctx := range contexts {
		for _, k := range []string{"struct_declaration", "union_declaration", "enum_declaration"} {
			member := func(m string) string {
				return strings.Replace(ctx, "%s", "("+k+" "+m+")", 1)
			}
			q.WriteString(member("(function_declaration . (identifier) @name) @definition.method") + "\n")
			q.WriteString(member("(function_declaration . (string) . (identifier) @name) @definition.method") + "\n")
			if k == "enum_declaration" {
				q.WriteString(member("(container_field . (identifier) @name) @definition.constant") + "\n")
			} else {
				q.WriteString(member("(container_field . (identifier) @name) @definition.field") + "\n")
			}
		}
	}
	// A file that is itself a struct (`const Self = @This();`) declares its
	// fields at file level, with no container around them.
	q.WriteString("(source_file (container_field . (identifier) @name) @definition.field)\n")
	const constRx = `^(pub\\s+|export\\s+|extern\\s+(\"[^\"]*\"\\s+)?)*const\\b`
	const varRx = `^(pub\\s+|export\\s+|extern\\s+(\"[^\"]*\"\\s+)?|threadlocal\\s+)*var\\b`
	for _, p := range []string{"source_file", "struct_declaration", "enum_declaration", "union_declaration"} {
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name [(struct_declaration) (enum_declaration) (union_declaration) (opaque_declaration)]) @definition.class)` + "\n")
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name [(error_set_declaration) (binary_expression (error_set_declaration))]) @definition.class)` + "\n")
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name . [(builtin_type) (function_signature) (pointer_type (function_signature))] .) @definition.type_alias` + "\n")
		q.WriteString("  (#match? @definition.type_alias \"" + constRx + "\"))\n")
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name . [(identifier) (field_expression) (call_expression)] .) @definition.type_alias` + "\n")
		// `const Money = @import("money.zig").Money;` is an import and names nothing new.
		q.WriteString("  (#match? @name \"^[A-Z][a-z]\")\n  (#not-match? @definition.type_alias \"@import\")\n  (#match? @definition.type_alias \"" + constRx + "\"))\n")
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name) @definition.constant` + "\n")
		q.WriteString("  (#match? @definition.constant \"" + constRx + "\")\n")
		q.WriteString("  (#not-match? @definition.constant \"=\\\\s*@(import|This)\\\\b\"))\n")
		q.WriteString("(" + p + ` (variable_declaration . (identifier) @name) @definition.variable` + "\n")
		q.WriteString("  (#match? @definition.variable \"" + varRx + "\"))\n")
	}
	registerDefinitions("zig", q.String(), true)
	registerNameFix("zig", zigName)
	registerImportFix("zig", zigImport)
	definitionRefiners["zig"] = zigOwners
}

// zigOwners qualifies a member of a container by the declared type it is in:
// `init` in `pub const Money = struct { ... }` is `Money.init`, and one in a type
// nested in another is `Outer.Inner.init`. Zig has no member syntax, so nothing
// else says that `Money.init(5)` reaches it. A container that is no declaration's
// value (the struct a generic function returns, a local type) names no owner and
// its members keep the names they had.
func zigOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		kind := kindFromTag(tag.Kind)
		if kind != model.KindMethod && kind != model.KindField && kind != model.KindConstant {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		var chain []string
		for p := n; p != nil; p = p.Parent() {
			switch p.Type(lang) {
			case "struct_declaration", "union_declaration", "enum_declaration":
			default:
				continue
			}
			decl := p.Parent()
			if decl == nil || decl.Type(lang) != "variable_declaration" {
				chain = nil
				break
			}
			var name *ts.Node
			for i := 0; i < decl.ChildCount() && name == nil; i++ {
				if c := decl.Child(i); c != nil && c.Type(lang) == "identifier" {
					name = c
				}
			}
			if name == nil || name.StartByte() == tag.NameRange.StartByte {
				continue
			}
			chain = append([]string{name.Text(src)}, chain...)
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

// zigImport reads `const x = @import("x.zig");` into the repository path of the
// file it names, without the extension (what the resolver compares with a
// file's), and the local name x. The same file imported for one member
// (`const f = @import("x.zig").f;`) is a row whose alias is `.f`, the way a
// JavaScript named import is.
//
//	@import("money.zig")         relative to the importing file
//	@import("../lib/util.zig")   likewise, cleaned
//	@import("std")               the standard library, no file here: dropped
//	@import("lib")               a package the build names, no path: dropped
//
// The build's own module names (`b.addModule`, build.zig.zon) are not read, so a
// package import is dropped and the calls made through it bind to nothing.
func zigImport(filePath string, imp Import) []Import {
	m := zigImportStmt.FindStringSubmatch(imp.Path)
	if m == nil {
		return nil
	}
	local, uri, member := m[1], m[2], m[3]
	if !strings.HasSuffix(uri, ".zig") {
		return nil
	}
	target := path.Clean(path.Join(path.Dir(filePath), strings.TrimSuffix(uri, ".zig")))
	if target == "" || target == "." || strings.HasPrefix(target, "..") {
		return nil
	}
	imp.Path = target
	switch {
	case local == "" || local == "_":
		imp.Alias = ""
	case member != "":
		imp.Alias = memberAliasPrefix + local
	default:
		imp.Alias = local
	}
	return []Import{imp}
}

var zigImportStmt = regexp.MustCompile(`^(?:pub\s+)?(?:const\s+(\w+)\s*=\s*|usingnamespace\s+|_\s*=\s*)@import\(\s*"([^"]+)"\s*\)(\.\w+)?\s*;?`)

// zigName reduces a captured test block (`test "title" { ... }`) to
// `test "title"`. Every other captured name is an identifier.
func zigName(name string) string {
	if !strings.HasPrefix(name, "test ") {
		return name
	}
	open := strings.IndexByte(name, '"')
	if open < 0 {
		return name
	}
	end := strings.IndexByte(name[open+1:], '"')
	if end < 0 {
		return name
	}
	return name[:open+end+2]
}
