package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Private definitions: which functions and members the language itself marks
// as reachable only from what encloses them. See migration 0024_symbol_flags
// and internal/resolve/infer (privateOf), which keeps a call from reaching one
// it could not.
//
// WHAT IS READ, per language, and only the marker the language writes:
//   - Java, Kotlin, PHP: the `private` modifier on a member or type;
//   - C#: the same, plus a class or struct member that states no accessibility
//     (private by default). An interface member, a top-level type and a member
//     of a record's primary parts are not defaulted. `private protected` is
//     reachable from a derived type and is not private here;
//   - Clojure: `defn-`, and `^:private` on a definition's name;
//   - C: `static` on a function definition, which gives it internal linkage.
//     The same keyword on a C++ member is a different thing and is not read.
//
// Left unread: everything else. Absent is "not read as private", never
// "public".

// privateDefNodes are, per language, the node types that define something a
// private marker may sit on.
var privateDefNodes = map[string]map[string]bool{
	"java": {
		"method_declaration": true, "constructor_declaration": true, "field_declaration": true,
		"class_declaration": true, "interface_declaration": true, "enum_declaration": true,
	},
	"c_sharp": {
		"method_declaration": true, "property_declaration": true, "field_declaration": true,
		"constructor_declaration": true, "event_field_declaration": true,
	},
	"kotlin": {
		"function_declaration": true, "property_declaration": true, "class_declaration": true,
		"object_declaration": true,
	},
	"php": {
		"method_declaration": true, "property_declaration": true, "const_declaration": true,
		"trait_declaration": true,
	},
	"c":       {"function_definition": true},
	"clojure": {"list_lit": true},
	"ruby":    {"method": true, "singleton_method": true},
}

// definitionFlags returns, keyed by the start byte of a definition's name, the
// model.SymbolFlag bits the tree gives it: private, for PHP a trait, for Ruby a
// singleton method, and for C and C++ a definition under a preprocessor
// conditional. Nil when no definition has any.
//
// orig is the source as it was written. src may be the masked text of a retry
// (cond.go), whose directive lines are blanked, so the conditionals are read
// from orig; every byte offset is the same in both.
func (p *genericParser) definitionFlags(tree *ts.Tree, src, orig []byte, tags []ts.Tag) (out map[uint32]int) {
	if p.entry.Name == "c" || p.entry.Name == "cpp" {
		out = conditionalDefinitions(orig, tags)
	}
	nodes := privateDefNodes[p.entry.Name]
	if nodes == nil || tree == nil {
		return out
	}
	defer func() {
		// An untrusted tree, contained as every other reading of it is.
		if recover() != nil {
			out = nil
		}
	}()
	root := tree.RootNode()
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition") {
			continue
		}
		name := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		def := name
		for i := 0; def != nil && i < 6; i++ {
			if nodes[def.Type(p.lang)] {
				break
			}
			def = def.Parent()
		}
		if def == nil || !nodes[def.Type(p.lang)] {
			continue
		}
		flags := out[tag.NameRange.StartByte]
		if p.entry.Name == "php" && def.Type(p.lang) == "trait_declaration" {
			flags |= model.SymbolFlagTrait
		} else if p.privateMarker(def, name, src) {
			flags |= model.SymbolFlagPrivate
		}
		if p.entry.Name == "ruby" && rubySingleton(def, p.lang) {
			flags |= model.SymbolFlagSingleton
		}
		if flags != 0 {
			if out == nil {
				out = make(map[uint32]int, 8)
			}
			out[tag.NameRange.StartByte] = flags
		}
	}
	return out
}

// privateMarker reads the marker off one definition node.
func (p *genericParser) privateMarker(def, name *ts.Node, src []byte) bool {
	switch p.entry.Name {
	case "java":
		return childWords(def, p.lang, src, "modifiers")["private"]
	case "kotlin":
		for i := 0; i < def.ChildCount(); i++ {
			m := def.Child(i)
			if m == nil || m.Type(p.lang) != "modifiers" {
				continue
			}
			for j := 0; j < m.ChildCount(); j++ {
				if v := m.Child(j); v != nil && v.Type(p.lang) == "visibility_modifier" && strings.TrimSpace(v.Text(src)) == "private" {
					return true
				}
			}
		}
		return false
	case "php":
		for i := 0; i < def.ChildCount(); i++ {
			if v := def.Child(i); v != nil && v.Type(p.lang) == "visibility_modifier" && strings.TrimSpace(v.Text(src)) == "private" {
				return true
			}
		}
		return false
	case "c_sharp":
		words := map[string]bool{}
		for i := 0; i < def.ChildCount(); i++ {
			if m := def.Child(i); m != nil && m.Type(p.lang) == "modifier" {
				words[strings.TrimSpace(m.Text(src))] = true
			}
		}
		if words["private"] {
			return !words["protected"]
		}
		if words["public"] || words["protected"] || words["internal"] {
			return false
		}
		// No accessibility stated: private in a class or struct, public in an
		// interface, internal for a top-level type (not read here).
		body := def.Parent()
		if body == nil || body.Type(p.lang) != "declaration_list" {
			return false
		}
		owner := body.Parent()
		if owner == nil {
			return false
		}
		switch owner.Type(p.lang) {
		case "class_declaration", "struct_declaration":
			return true
		}
		return false
	case "c":
		for i := 0; i < def.ChildCount(); i++ {
			if s := def.Child(i); s != nil && s.Type(p.lang) == "storage_class_specifier" && strings.TrimSpace(s.Text(src)) == "static" {
				return true
			}
		}
		return false
	case "clojure":
		return clojurePrivate(def, name, p.lang, src)
	}
	return false
}

// rubySingleton reports whether a Ruby method definition is on the class or
// module itself: `def self.x`, `def Const.x`, or a `def x` under `class << self`.
// The index names both kinds `Class.x`, and a bare call inside an instance
// method does not reach the singleton one (nor the reverse), so the resolver
// needs to tell them apart. A definition made by `define_method` or `alias` is
// not read here and stays unflagged.
func rubySingleton(def *ts.Node, lang *ts.Language) bool {
	if def.Type(lang) == "singleton_method" {
		return true
	}
	for n := def.Parent(); n != nil; n = n.Parent() {
		switch n.Type(lang) {
		case "singleton_class":
			return true
		case "class", "module", "program":
			return false
		}
	}
	return false
}

// rubyCallBlocks returns, keyed by the start byte of a call's name, the calls of
// a Ruby file that are written with a block: `patch do ... end`,
// `resource("x") { ... }`, `a.each do |x| ... end`. The block is a child of the
// call node, beside the method name and the argument list. Nil when there is
// none, and for every other language.
func (p *genericParser) rubyCallBlocks(tree *ts.Tree, tags []ts.Tag) (out map[uint32]bool) {
	if p.entry.Name != "ruby" || tree == nil {
		return nil
	}
	defer func() {
		// An untrusted tree, contained as every other reading of it is.
		if recover() != nil {
			out = nil
		}
	}()
	root := tree.RootNode()
	for _, tag := range tags {
		if !isCallTag(tag.Kind) {
			continue
		}
		name := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		if name == nil {
			continue
		}
		call := name.Parent()
		if call == nil || call.Type(p.lang) != "call" {
			continue
		}
		if m := call.ChildByFieldName("method", p.lang); m == nil || m.StartByte() != name.StartByte() {
			continue
		}
		for i := 0; i < call.ChildCount(); i++ {
			if c := call.Child(i); c != nil {
				if t := c.Type(p.lang); t == "block" || t == "do_block" {
					if out == nil {
						out = make(map[uint32]bool, 8)
					}
					out[tag.NameRange.StartByte] = true
					break
				}
			}
		}
	}
	return out
}

// conditionalDefinitions returns, keyed by the start byte of a definition's
// name, the C and C++ definitions written inside a branch of `#if`, `#ifdef`,
// `#ifndef`, `#elif` or `#else`, as model.SymbolFlagConditional. Another
// definition of the same name may be the other branch, and which one a build
// sees depends on flags the source does not carry: `compat_fs.c` defines
// `cbm_fopen` once for Windows and once for POSIX.
//
// It reads the directives from the text, with cond.go's scanner, and not from
// the tree. The tree is the masked retry's where a conditional cut a
// declaration apart, and masking blanks the directives of exactly the files
// that have the most of them.
//
// Two kinds of conditional are not alternatives and are skipped: an include
// guard (`#ifndef X` and then `#define X`), and a group that names
// `__cplusplus`, which wraps `extern "C" {` and defines nothing of its own. An
// `#ifdef _WIN32` around a whole platform file is neither, and is read.
func conditionalDefinitions(src []byte, tags []ts.Tag) map[uint32]int {
	groups := buildCondGroups(scanCondDirectives(src, false))
	if len(groups) == 0 {
		return nil
	}
	var spans []condSpan
	for i := range groups {
		g := &groups[i]
		if isIncludeGuard(src, g) || mentionsCplusplus(g) {
			continue
		}
		for _, b := range g.branches {
			spans = append(spans, condSpan{b.bodyStart, b.bodyEnd})
		}
	}
	if len(spans) == 0 {
		return nil
	}
	var out map[uint32]int
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition") {
			continue
		}
		at := int(tag.NameRange.StartByte)
		for _, sp := range spans {
			if at >= sp.start && at < sp.end {
				if out == nil {
					out = make(map[uint32]int, 8)
				}
				out[tag.NameRange.StartByte] = model.SymbolFlagConditional
				break
			}
		}
	}
	return out
}

// isIncludeGuard reports whether a conditional group is `#ifndef X` followed
// at once by `#define X`, and has no other branch.
func isIncludeGuard(src []byte, g *condGroup) bool {
	if len(g.branches) != 1 {
		return false
	}
	b := g.branches[0]
	name, want, ok := condMacroTest(b.dir.cond)
	if !ok || want {
		return false
	}
	body := strings.TrimSpace(string(src[b.bodyStart:b.bodyEnd]))
	rest, found := strings.CutPrefix(body, "#")
	if !found {
		return false
	}
	rest, found = strings.CutPrefix(strings.TrimSpace(rest), "define")
	if !found {
		return false
	}
	fields := strings.Fields(rest)
	return len(fields) > 0 && fields[0] == name
}

// mentionsCplusplus reports whether any branch of the group tests __cplusplus.
func mentionsCplusplus(g *condGroup) bool {
	for _, b := range g.branches {
		if strings.Contains(b.dir.cond, "__cplusplus") {
			return true
		}
	}
	return false
}

// childWords returns the whitespace-separated words of the text of def's
// direct child of the given type.
func childWords(def *ts.Node, lang *ts.Language, src []byte, childType string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < def.ChildCount(); i++ {
		if c := def.Child(i); c != nil && c.Type(lang) == childType {
			for _, w := range strings.Fields(c.Text(src)) {
				out[w] = true
			}
		}
	}
	return out
}

// clojurePrivate reports whether a definition form is `defn-` (or
// `defmacro-`-like forms spelled with a trailing dash), or names its symbol
// with `^:private` metadata.
func clojurePrivate(def, name *ts.Node, lang *ts.Language, src []byte) bool {
	var head *ts.Node
	for i := 0; i < def.NamedChildCount(); i++ {
		if c := def.NamedChild(i); c != nil && c.Type(lang) == "sym_lit" {
			head = c
			break
		}
	}
	if head == nil {
		return false
	}
	switch strings.TrimSpace(head.Text(src)) {
	case "defn-", "defmacro-":
		return true
	}
	// `^:private` on the name: the name's sym_lit carries a meta_lit child.
	for n := name; n != nil && n != def; n = n.Parent() {
		if n.Type(lang) != "sym_lit" {
			continue
		}
		for i := 0; i < n.NamedChildCount(); i++ {
			if m := n.NamedChild(i); m != nil && m.Type(lang) == "meta_lit" {
				if t := m.Text(src); strings.Contains(t, ":private") || strings.Contains(t, "private true") {
					return true
				}
			}
		}
		break
	}
	return false
}
