package parse

import (
	"path"
	"strings"
)

// Objective-C: the C declarations (functions, typedefs, file-level
// variables) and the Objective-C ones (@interface, @implementation,
// @protocol, categories, methods, properties, instance variables).
//
// The inferred query is replaced because it tags every identifier of a method
// declaration (the parameter names too) as a method and finds no classes. The
// call references it provided are kept.
//
// A method is named by its first selector part: `scaleBy:animated:` is
// recorded as `scaleBy`, the only part that is a node of its own.
//
// A message send is a call, named by the same first selector part: [view scaleBy:2
// animated:YES] is a reference to `scaleBy`. A selector's later parts are not part
// of the name on either side, so `add:` and `add:to:` of one class are one name.
//
// A category (`@interface Shape (Printing)`) and a class extension
// (`@interface Shape ()`) have the same tree as a class with a superclass:
// two identifiers, no field to tell them apart, and the parentheses are not
// nodes a pattern can name. They are told apart by the first words of the
// declaration's own text. A category is recorded under its own name; an
// extension has none and is not recorded.
func init() {
	registerDefinitions("objc", `
(call_expression (identifier) @name) @reference.call
(call_expression (field_identifier) @name) @reference.call

; A message send [receiver doThing:x with:y] is a call of doThing:with:, named here by its
; FIRST selector part, which is how the method is named where it is declared and defined (see
; below), so that a send and the method it reaches carry one name. The call node begins at the
; bracket, and the text from there to the name is the receiver (see objcReceiver in shape.go).
(message_expression receiver: (_) . method: (identifier) @name (#not-eq? @name "alloc")) @reference.call

; C functions
(function_definition (function_declarator . (identifier) @name)) @definition.function
(function_definition (pointer_declarator (function_declarator . (identifier) @name))) @definition.function

; @interface Name : Super, @interface Name, and @interface Name (Category)
(class_interface . (identifier) @name) @definition.class
  (#not-match? @definition.class "^@interface\\s+[A-Za-z0-9_]+\\s*\\(")
(class_interface . (identifier) (identifier) @name) @definition.class
  (#match? @definition.class "^@interface\\s+[A-Za-z0-9_]+\\s*\\(")
(class_implementation . (identifier) @name) @definition.class
  (#not-match? @definition.class "^@implementation\\s+[A-Za-z0-9_]+\\s*\\(")
(class_implementation . (identifier) (identifier) @name) @definition.class
  (#match? @definition.class "^@implementation\\s+[A-Za-z0-9_]+\\s*\\(")
(protocol_declaration . (identifier) @name) @definition.interface

; methods, declared or defined; "+" and "-" alike
(method_declaration (method_type) . (identifier) @name) @definition.method
(method_definition (method_type) . (identifier) @name) @definition.method

; @property and instance variables
(property_declaration (struct_declaration (struct_declarator [(identifier) @name (pointer_declarator (identifier) @name)]))) @definition.field
(property_declaration (struct_declaration (struct_declarator (function_declarator (parenthesized_declarator (block_pointer_declarator (identifier) @name)))))) @definition.field
(instance_variable (struct_declaration (struct_declarator [(identifier) @name (pointer_declarator (identifier) @name)]))) @definition.field

; typedefs
(type_definition (struct_specifier) (type_identifier) @name .) @definition.class
(type_definition (enum_specifier) (type_identifier) @name .) @definition.class
(type_definition (union_specifier) (type_identifier) @name .) @definition.class
(type_definition [(type_identifier) (primitive_type) (sized_type_specifier)] (type_identifier) @name .) @definition.type_alias
(type_definition (function_declarator (parenthesized_declarator [(block_pointer_declarator (type_identifier) @name) (pointer_declarator (type_identifier) @name)]))) @definition.type_alias

; file-level variables with an initialiser (constants when const), and without one
; when the declaration starts with a storage class or a built-in type; a plain
; "Foo bar;" is not told apart from a function whose return-type macro the
; grammar does not know. A storage class written as a macro (FOUNDATION_EXPORT,
; UIKIT_EXTERN) is an extern declaration, which is not a definition; and a
; declaration that begins with some other word (NS_ASSUME_NONNULL_BEGIN, read as
; its type) is a macro the grammar glued to the next line, not a variable.
(translation_unit (declaration (init_declarator . [(identifier) @name (pointer_declarator (identifier) @name) (pointer_declarator (pointer_declarator (identifier) @name))])) @definition.variable)
(translation_unit (declaration (type_qualifier) @_q (init_declarator . [(identifier) @name (pointer_declarator (identifier) @name)])) @definition.constant
  (#match? @_q "^const$"))
(translation_unit (declaration (init_declarator . (pointer_declarator (type_qualifier) @_q (identifier) @name))) @definition.constant
  (#match? @_q "^const$"))
(translation_unit (declaration . [(storage_class_specifier) @_s (primitive_type) (sized_type_specifier)] [(identifier) @name (pointer_declarator (identifier) @name)] .) @definition.variable
  (#not-match? @definition.variable "^extern\\b")
  (#not-match? @_s "^[A-Z_][A-Z0-9_]*$"))
(translation_unit (declaration . (type_qualifier) [(primitive_type) (sized_type_specifier)] [(identifier) @name (pointer_declarator (identifier) @name)] .) @definition.variable
  (#not-match? @definition.variable "^extern\\b"))
`, true)
}

func init() {
	registerImportFix("objc", objcImport)
}

// objcImport names the file a quoted #import or #include reaches without its
// extension, so that `#import "Cart.h"` covers Cart.h and the Cart.m beside it,
// which is where the methods it declares are defined. A framework header
// (`<Foundation/Foundation.h>`) is not captured at all.
//
// A quoted include is looked for beside the including file first and then on
// the header search paths, which the source does not state. Both readings are
// recorded: the path as written, which the resolver matches against the end of
// any header's path, and the path joined to the importing file's directory,
// which is the longer and so the better match when such a header exists. A
// repository with two Pricing.h under different directories is told apart by
// the one that sits where the include says.
func objcImport(filePath string, imp Import) []Import {
	p := path.Clean(imp.Path)
	if ext := path.Ext(p); ext == ".h" || ext == ".hpp" || ext == ".m" || ext == ".mm" {
		p = p[:len(p)-len(ext)]
	}
	if p == "" || p == "." || strings.HasPrefix(p, "..") || p[0] == '/' {
		return nil
	}
	written := imp
	written.Path = p
	out := []Import{written}
	if dir := path.Dir(filePath); dir != "." {
		rooted := imp
		rooted.Path = path.Join(dir, p)
		out = append(out, rooted)
	}
	return out
}
