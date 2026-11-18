package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// MATLAB. A function file `foo.m` defines `foo`, and any function on the path can
// call it with no import: the path is the file's folder and its parents, and the
// repository is taken as one path (infer/matlab.go). A file may hold local
// functions after the first, visible to that file only. A classdef file holds a
// class, and the functions in its `methods` blocks are its methods.
//
//	function f(x)        function f, in f.m or in any file
//	classdef C           class C
//	  methods            the functions in a methods block are methods of C
//	    function C(x)    the constructor, a method named for the class
//
// Calls (queries.go): `f(x)`, `pkg.f(x)`, a command-syntax statement and a function
// handle. A name the enclosing function or script binds (an assignment, a
// parameter, an output, a loop variable) is not a call, because `y(i)` is also how
// an array is indexed; see matlabBinds.
func init() {
	definitionRefiners["matlab"] = matlabOwners
	registerImportFix("matlab", matlabImport)
}

// matlabOwners makes the functions of a classdef methods of that class, so that
// `obj.deposit(5)` and `deposit(obj, 5)` are told apart from a function of the
// same name in a file of its own.
func matlabOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if kindFromTag(tag.Kind) != "function" || !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		var class *ts.Node
		for p := n; p != nil; p = p.Parent() {
			if p.Type(lang) == "class_definition" {
				class = p
				break
			}
		}
		if class == nil {
			continue
		}
		var name *ts.Node
		for i := 0; i < class.ChildCount(); i++ {
			if c := class.Child(i); c != nil && c.Type(lang) == "identifier" {
				name = c
				break
			}
		}
		if name == nil {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fixes[tag.NameRange.StartByte] = definitionFix{kind: "definition.method", owner: name.Text(src)}
	}
	return fixes
}

// matlabImport reads `import pkg.sub.*` and `import pkg.sub.f` into the package
// and what of it is brought in (lang_modules.go has the encoding):
//
//	import pkg.sub.*     pkg.sub, every function and class of the package folder
//	import pkg.sub.f     pkg.sub, import:f
//
// A Java class (`import java.util.ArrayList`) names no folder of the repository
// and is dropped by the resolver, which finds none.
func matlabImport(_ string, imp Import) []Import {
	p := strings.TrimSpace(imp.Path)
	imp.Alias = ""
	if p == "" || strings.ContainsAny(p, " \t") {
		return nil
	}
	if pkg, ok := strings.CutSuffix(p, ".*"); ok && pkg != "" {
		imp.Path, imp.Alias = pkg, ModuleImportAll
		return []Import{imp}
	}
	i := strings.LastIndexByte(p, '.')
	if i <= 0 {
		return nil
	}
	imp.Path, imp.Alias = p[:i], ModuleImportPrefix+p[i+1:]
	return []Import{imp}
}

// matlabBinds reports whether the function (or script) a call is written in binds
// the name: as a parameter or output, on the left of an assignment (`x = ...`,
// `y(i) = ...`, `[a, b] = ...`), as a loop variable, in a `global` or
// `persistent` declaration, or as the parameter of an anonymous function around
// the call. Such a name is a variable, and `name(args)` indexes it.
//
// It errs toward binding nothing: a name bound anywhere in the function counts,
// whatever order it was bound in.
func matlabBinds(root *ts.Node, lang *ts.Language, src []byte, at, end uint32, name string) bool {
	n := root.NamedDescendantForByteRange(at, end)
	if n == nil {
		return false
	}
	scope := root
	for p := n; p != nil; p = p.Parent() {
		switch p.Type(lang) {
		case "lambda":
			if args := firstChildOfType(p, lang, "arguments"); args != nil && namesBoundIn(args, lang, src, name, "identifier") {
				return true
			}
		case "function_definition":
			if scope == root {
				scope = p
			}
		}
	}
	return matlabBindsIn(scope, lang, src, name)
}

func matlabBindsIn(n *ts.Node, lang *ts.Language, src []byte, name string) bool {
	if n == nil {
		return false
	}
	switch n.Type(lang) {
	case "function_output", "function_arguments", "global_operator", "persistent_operator":
		if namesBoundIn(n, lang, src, name, "identifier") {
			return true
		}
	case "iterator":
		if c := firstChildOfType(n, lang, "identifier"); c != nil && c.Text(src) == name {
			return true
		}
	case "assignment":
		if left := n.Child(0); left != nil {
			switch left.Type(lang) {
			case "identifier":
				if left.Text(src) == name {
					return true
				}
			case "function_call":
				// `y(i) = v`: y is indexed, and so is a variable.
				if c := firstChildOfType(left, lang, "identifier"); c != nil && c.Text(src) == name {
					return true
				}
			case "multioutput_variable":
				if namesBoundIn(left, lang, src, name, "identifier") {
					return true
				}
			}
		}
	}
	for i := 0; i < n.ChildCount(); i++ {
		// A nested function has a workspace of its own and is read when a call
		// inside it asks.
		if c := n.Child(i); c != nil && c.Type(lang) != "function_definition" && matlabBindsIn(c, lang, src, name) {
			return true
		}
	}
	return false
}
