package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// C#. The type declarations and the calls are in the supplement (queries.go);
// this registers what the inferred query for the grammar gets wrong and what it
// never had.
//
// The inferred query is replaced, because two of its patterns name no field:
//
//	(method_declaration (identifier) @name)
//	(constructor_declaration (identifier) @name)
//
// match EVERY identifier child, and a method's return type is an identifier
// when it names a type: `ILogger ForContext(...)` produced a method called
// ILogger as well as ForContext (serilog: 20 such symbols in three files). The
// patterns below take the identifier that stands directly before the parameter
// list (or the accessors of a property). The `name:` field is not used: in a
// region the grammar had to recover from, the node keeps its children and loses
// its field names, and a type declaration found there before is found still.
//
// # What is a definition
//
//	class, struct, record, interface, enum     as before
//	method, constructor                        method
//	auto, full and expression-bodied property  field
//	event (`event Action E;`, `event ... E {}`) field
//	indexer                                    nothing: it has no name, only `this`
//	delegate, operator, destructor, local function
//	                                           not defined, as before
//
// A property is a field because it is read and written as one: the index
// answers "where is Name declared" with the property, and `x.Name` is a use of
// it. Its accessors are not separate symbols.
//
// # new
//
// `new Widget(3)` is a construction of Widget, recorded as Java's is
// (lang_java.go): the type is the tag, its kind is reference.call.new, and a
// qualified or generic name is tagged whole so the qualifier is the receiver.
//
//	new Widget(3)                    Widget
//	new Shop.Widget(3)               Widget, qualified by Shop
//	new Box<int>(3)                  Box
//	new Shop.Box<int>(3)             Box, qualified by Shop
//	new() / new { A = 1 } / new[] {} no type is written: nothing
//
// `this(...)` and `base(...)` after a constructor's parameter list are built
// from the tree, see delegation.go.
func init() {
	registerDefinitions("c_sharp", `
(class_declaration (identifier) @name) @definition.class
(interface_declaration (identifier) @name) @definition.interface
(enum_declaration (identifier) @name) @definition.enum
(method_declaration (identifier) @name . [(parameter_list) (type_parameter_list)]) @definition.method
(constructor_declaration (identifier) @name . (parameter_list)) @definition.constructor
(property_declaration (identifier) @name . [(accessor_list) (arrow_expression_clause)]) @definition.field
(event_declaration (identifier) @name . [(accessor_list) (arrow_expression_clause)]) @definition.field
(event_field_declaration (variable_declaration (variable_declarator (identifier) @name))) @definition.field

(object_creation_expression
  type: (identifier) @name @reference.call.new)
(object_creation_expression
  type: (qualified_name (identifier) @name .) @reference.call.new)
(object_creation_expression
  type: (generic_name . (identifier) @name) @reference.call.new)
(object_creation_expression
  type: (qualified_name (generic_name . (identifier) @name) .) @reference.call.new)
`, true)
}

// A call that follows `=>` is not a declaration.
//
// Serilog's interface members have default bodies written between #if lines:
//
//	void Verbose(string messageTemplate)
//	#if FEATURE_DEFAULT_INTERFACE
//	    => Write(LogEventLevel.Verbose, messageTemplate, NoPropertyValues)
//	#endif
//	;
//
// The grammar cannot read a preprocessor line inside a member and recovers
// with a method whose name is the called function: 64 of the 134 symbols
// extracted from that one file were calls such as `Write(...)`. A method
// cannot begin right after `=>`; whatever the tree calls a declaration there is
// the expression body of the member before it, and is dropped.
func init() {
	definitionRefiners["c_sharp"] = func(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
		var fixes map[uint32]definitionFix
		for _, tag := range tags {
			if !strings.HasPrefix(tag.Kind, "definition") {
				continue
			}
			if followsArrow(src, tag.Range.StartByte) {
				if fixes == nil {
					fixes = map[uint32]definitionFix{}
				}
				fixes[tag.NameRange.StartByte] = definitionFix{drop: true}
				continue
			}
			if iface := explicitInterface(root, lang, src, tag); iface != "" {
				if fixes == nil {
					fixes = map[uint32]definitionFix{}
				}
				fixes[tag.NameRange.StartByte] = definitionFix{name: iface + "." + tag.Name}
			}
		}
		return fixes
	}
}

// explicitInterface is the interface an explicit implementation names:
// `void I.M() {}` is a member of the class that only a value typed I can
// reach, and `M()` written in the class does not call it. It is indexed under
// the interface-qualified name (`I.M`), which no unqualified call can match.
// "" for any other definition.
func explicitInterface(root *ts.Node, lang *ts.Language, src []byte, tag ts.Tag) string {
	if root == nil {
		return ""
	}
	n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
	if n == nil {
		return ""
	}
	def := n.Parent()
	if def == nil {
		return ""
	}
	for i := 0; i < def.NamedChildCount(); i++ {
		c := def.NamedChild(i)
		if c != nil && c.Type(lang) == "explicit_interface_specifier" {
			name := strings.TrimSuffix(strings.TrimSpace(c.Text(src)), ".")
			if j := strings.LastIndex(name, "."); j >= 0 {
				name = name[j+1:]
			}
			if k := strings.Index(name, "<"); k >= 0 {
				name = name[:k]
			}
			return name
		}
	}
	return ""
}

// followsArrow reports whether the text before pos, ignoring white space, ends
// with `=>`.
func followsArrow(src []byte, pos uint32) bool {
	i := int(pos)
	for i > 0 && (src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n' || src[i-1] == '\r') {
		i--
	}
	return i >= 2 && src[i-2] == '=' && src[i-1] == '>'
}
