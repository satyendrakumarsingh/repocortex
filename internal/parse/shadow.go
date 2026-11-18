package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// # A name a function binds is not a call of the module's function
//
// Haskell, OCaml and F# apply by juxtaposition, so `f x` is a call of whatever
// `f` is: a function of the module, or a parameter, a lambda's argument, a name
// a pattern bound, a local `let`. The resolver binds a bare call to a function
// the module or an import provides, and without this a parameter called `slug`
// that the function applies (`\slug -> slug x`) was a call of the imported
// Util.slug.
//
// shadowedByLocal reads the tree above a call and says whether some enclosing
// construct binds the name: the parameters of the function the call is written
// in (its own name is not one: a function calls itself), a lambda's, a `case`
// or `match` arm's pattern, a name bound in a `do` block, and a `let` or `where`
// that defines it locally. Such a call is marked indirect, as a call of a
// parameter is in every other language (shape.go), and binds to nothing.
//
// It is an approximation and says so by erring toward binding nothing: a name
// bound anywhere in an enclosing pattern counts, whatever order it was bound in.
var shadowLanguages = map[string]bool{"haskell": true, "ocaml": true, "fsharp": true, "matlab": true}

func shadowedByLocal(language string, root *ts.Node, lang *ts.Language, src []byte, at, end uint32, name string) bool {
	if language == "matlab" {
		// `y(i)` indexes a variable the function binds (lang_matlab.go).
		return matlabBinds(root, lang, src, at, end, name)
	}
	n := root.NamedDescendantForByteRange(at, end)
	if n == nil {
		return false
	}
	binds := func(node *ts.Node, types ...string) bool {
		return node != nil && namesBoundIn(node, lang, src, name, types...)
	}
	for child, p := n, n.Parent(); p != nil; child, p = p, p.Parent() {
		switch language {
		case "haskell":
			switch p.Type(lang) {
			case "function", "lambda":
				if pat := p.ChildByFieldName("patterns", lang); pat != nil && pat != child && binds(pat, "variable") {
					return true
				}
			case "alternative":
				if pat := p.ChildByFieldName("pattern", lang); pat != nil && pat != child && binds(pat, "variable") {
					return true
				}
			case "do":
				for i := 0; i < p.ChildCount(); i++ {
					st := p.Child(i)
					if st != nil && st.Type(lang) == "bind" && st != child && binds(st.ChildByFieldName("pattern", lang), "variable") {
						return true
					}
				}
			}
			if decls := p.ChildByFieldName("binds", lang); decls != nil && decls != child {
				for i := 0; i < decls.ChildCount(); i++ {
					d := decls.Child(i)
					if d == nil || (d.Type(lang) != "bind" && d.Type(lang) != "function") {
						continue
					}
					if v := d.ChildByFieldName("name", lang); v != nil && v.Text(src) == name {
						return true
					}
				}
			}
		case "ocaml":
			switch p.Type(lang) {
			case "let_binding", "fun_expression", "function_expression":
				for i := 0; i < p.ChildCount(); i++ {
					if c := p.Child(i); c != nil && c.Type(lang) == "parameter" && binds(c, "value_pattern") {
						return true
					}
				}
			case "match_case":
				if binds(p.ChildByFieldName("pattern", lang), "value_pattern") {
					return true
				}
			case "let_expression":
				for i := 0; i < p.ChildCount(); i++ {
					def := p.Child(i)
					if def == nil || def.Type(lang) != "value_definition" || def == child {
						continue
					}
					for j := 0; j < def.ChildCount(); j++ {
						if lb := def.Child(j); lb != nil && lb.Type(lang) == "let_binding" {
							if v := lb.ChildByFieldName("pattern", lang); v != nil && v.Text(src) == name {
								return true
							}
						}
					}
				}
			}
		case "fsharp":
			switch p.Type(lang) {
			case "function_or_value_defn":
				left := firstChildOfType(p, lang, "value_declaration_left")
				if left == nil {
					left = firstChildOfType(p, lang, "function_declaration_left")
				}
				if left == nil || left == child {
					break
				}
				// A top-level function's own name is no shadow (it calls itself); a
				// local one is, and so is every parameter.
				local := hasAncestorOfType(p.Parent(), lang, "function_or_value_defn")
				if local && binds(left, "identifier") || !local && boundBeyondFirst(left, lang, src, name) {
					return true
				}
			case "fun_expression":
				if binds(firstChildOfType(p, lang, "argument_patterns"), "identifier") {
					return true
				}
			case "rule":
				if binds(p.ChildByFieldName("pattern", lang), "identifier") {
					return true
				}
			case "declaration_expression":
				// `let helper y = ... in` : the definition beside the body names a
				// local, when this declaration is itself inside a function.
				if hasAncestorOfType(p.Parent(), lang, "function_or_value_defn") {
					for i := 0; i < p.ChildCount(); i++ {
						d := p.Child(i)
						if d == nil || d.Type(lang) != "function_or_value_defn" || d == child {
							continue
						}
						if left := firstChildOfType(d, lang, "value_declaration_left"); left != nil && binds(left, "identifier") {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

// namesBoundIn reports whether a node holds, among its descendants of the given
// types, one whose text is the name.
func namesBoundIn(node *ts.Node, lang *ts.Language, src []byte, name string, types ...string) bool {
	if node == nil {
		return false
	}
	t := node.Type(lang)
	for _, want := range types {
		if t == want && node.Text(src) == name {
			return true
		}
	}
	for i := 0; i < node.ChildCount(); i++ {
		if namesBoundIn(node.Child(i), lang, src, name, types...) {
			return true
		}
	}
	return false
}

// boundBeyondFirst is namesBoundIn for an F# definition's left side, leaving out
// the first identifier, which is the name the definition declares.
func boundBeyondFirst(left *ts.Node, lang *ts.Language, src []byte, name string) bool {
	first := true
	var walk func(n *ts.Node) bool
	walk = func(n *ts.Node) bool {
		if n == nil {
			return false
		}
		if n.Type(lang) == "identifier" {
			if first {
				first = false
				return false
			}
			return strings.TrimSpace(n.Text(src)) == name
		}
		for i := 0; i < n.ChildCount(); i++ {
			if walk(n.Child(i)) {
				return true
			}
		}
		return false
	}
	return walk(left)
}
