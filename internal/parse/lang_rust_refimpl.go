package parse

import ts "github.com/odvcencio/gotreesitter"

// # Rust: a method of an impl for a reference type
//
// `impl<'a, T> IntoIterator for &'a OneOrManyWithParens<T>` and `impl<T>
// IntoIterator for OneOrManyWithParens<T>` give the type two `into_iter`, which
// the index names alike. `ours.into_iter()` on an owned value is the second and
// on `&ours` the first, and the receiver's expression is not always there to
// say. The method is marked so the resolver can tell the pair from a repeat.

// rustRefImpl reports whether a definition is a direct member of an impl whose
// self type is a reference.
func (p *genericParser) rustRefImpl(def *ts.Node) bool {
	body := def.Parent()
	if body == nil || body.Type(p.lang) != "declaration_list" {
		return false
	}
	impl := body.Parent()
	if impl == nil || impl.Type(p.lang) != "impl_item" {
		return false
	}
	t := impl.ChildByFieldName("type", p.lang)
	return t != nil && t.Type(p.lang) == "reference_type"
}
