package parse

// Pascal and Delphi. What the index holds, see
// fixtures/langs/pascal/expected.tsv.
//
//   - program, unit and library are packages;
//   - procedure and function are functions, at any depth (a procedure nested
//     in another is a named function). A routine written twice, as a header in
//     the interface section and again with its body in the implementation, is
//     two definitions, as a C declaration and definition are;
//   - constructor, destructor, procedure and function inside a class, record,
//     object or interface body are methods, and so is a body written as
//     `procedure TShape.Draw`, named by its last part;
//   - class, record and object types are classes; interface types are
//     interfaces; every other type (enumeration, alias, pointer, string, set,
//     array) is a type alias;
//   - const and var declared at the top of a program, a unit's interface or
//     its implementation are constants and variables (one per name in
//     `A, B: Integer`); the const and var of a routine are local and are not
//     definitions, nor are enumeration members;
//   - the fields of a class, record or object, and its properties, are fields.
//
// Calls and parameters are not definitions.
func init() {
	registerDefinitions("pascal", pascalDefinitions, true)
}

const pascalDefinitions = `
(program (moduleName (identifier) @name)) @definition.module
(unit (moduleName (identifier) @name)) @definition.module
(library (moduleName (identifier) @name)) @definition.module

(defProc . (declProc (genericDot (identifier) (kDot) . (identifier) @name .))) @definition.method
(defProc . (declProc . (_) . (identifier) @name)) @definition.function

(interface (declProc . (_) . (identifier) @name) @definition.function)
(declClass (declProc . (_) . (identifier) @name) @definition.method)
(declSection (declProc . (_) . (identifier) @name) @definition.method)
(declIntf (declProc . (_) . (identifier) @name) @definition.method)

(declClass (declField (identifier) @name @definition.field))
(declSection (declField (identifier) @name @definition.field))
(declClass (declProp . (kProperty) . (identifier) @name) @definition.field)
(declSection (declProp . (kProperty) . (identifier) @name) @definition.field)

(program (declConsts (declConst . (identifier) @name) @definition.constant))
(interface (declConsts (declConst . (identifier) @name) @definition.constant))
(implementation (declConsts (declConst . (identifier) @name) @definition.constant))
(program (declVars (declVar (identifier) @name @definition.variable)))
(interface (declVars (declVar (identifier) @name @definition.variable)))
(implementation (declVars (declVar (identifier) @name @definition.variable)))

(program (declTypes (declType . (identifier) @name (kEq) (declClass)) @definition.class))
(interface (declTypes (declType . (identifier) @name (kEq) (declClass)) @definition.class))
(implementation (declTypes (declType . (identifier) @name (kEq) (declClass)) @definition.class))
(program (declTypes (declType . (identifier) @name (kEq) (declIntf)) @definition.interface))
(interface (declTypes (declType . (identifier) @name (kEq) (declIntf)) @definition.interface))
(implementation (declTypes (declType . (identifier) @name (kEq) (declIntf)) @definition.interface))
(program (declTypes (declType . (identifier) @name (kEq) (type)) @definition.type_alias))
(interface (declTypes (declType . (identifier) @name (kEq) (type)) @definition.type_alias))
(implementation (declTypes (declType . (identifier) @name (kEq) (type)) @definition.type_alias))
`
