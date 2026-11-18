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
}

// definitionFlags returns, keyed by the start byte of a definition's name, the
// model.SymbolFlag bits the tree gives it: private, and for PHP a trait. Nil
// when no definition has any.
func (p *genericParser) definitionFlags(tree *ts.Tree, src []byte, tags []ts.Tag) (out map[uint32]int) {
	nodes := privateDefNodes[p.entry.Name]
	if nodes == nil || tree == nil {
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
		flags := 0
		if p.entry.Name == "php" && def.Type(p.lang) == "trait_declaration" {
			flags = model.SymbolFlagTrait
		} else if p.privateMarker(def, name, src) {
			flags = model.SymbolFlagPrivate
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
