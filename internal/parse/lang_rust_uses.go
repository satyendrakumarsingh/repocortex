package parse

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
)

// # Rust `use` declarations — one row per leaf
//
// The import query reads `use a::b::c;` and nothing wider: a plain path. The
// forms real code is written in — a list (`use a::{b, c::d}`), a rename
// (`use a::b as e`) and a glob (`use a::*`) — produced no row at all, and a
// bare call to a function brought in by one of them (`resolved_command(..)`
// after `use crate::core::utils::{resolved_command, tool_exists};`) was left
// with nothing to say which module it came from.
//
// Each leaf of a declaration becomes the row the same names written one per
// line would have been:
//
//	use a::b::c;               Path "a::b::c"
//	use a::{b, c::{d, e}};     Path "a::b", "a::c::d", "a::c::e"
//	use a::b::{self, c};       Path "a::b", "a::b::c"
//	use a::b::c as d;          Path "a::b",  Alias ".d=c"   (a member, renamed)
//	use a::b as d;             Path "a",     Alias ".d=b"
//	use b as d;                Path "b",     Alias "d"      (a crate renamed)
//	use a::b::*;               Path "a::b::*"
//	use super::*;              Path "super::*"
//
// A leaf's last segment may be a module or an item and the syntax does not
// say which; the resolver tells them apart by the files the repository has
// (internal/resolve/infer/rustuse.go). A rename is written as a member row, the
// form Python's `from m import x as y` already uses, because the local name is
// what a call spells. `as _` binds no name and writes no row. A leading `::` is
// dropped.

// rustUses reads every use declaration in a file, in any module or function
// body, as Import rows.
func (p *genericParser) rustUses(tree *ts.Tree, src []byte) (out []Import) {
	defer func() { _ = recover() }()
	root := tree.RootNode()
	if root == nil {
		return nil
	}
	lang := p.lang

	add := func(path, alias string, at *ts.Node) {
		if path == "" {
			return
		}
		imp := Import{Path: path, Alias: alias}
		imp.Range = model.Range{
			Start: model.Position{Line: int(at.StartPoint().Row) + 1, Col: int(at.StartPoint().Column)},
			End:   model.Position{Line: int(at.EndPoint().Row) + 1, Col: int(at.EndPoint().Column)},
		}
		out = append(out, imp)
	}
	compact := func(s string) string {
		return strings.Join(strings.Fields(s), "")
	}
	join := func(prefix, tail string) string {
		switch {
		case prefix == "":
			return tail
		case tail == "":
			return prefix
		}
		return prefix + "::" + tail
	}
	// leaf records the name a path binds, renamed or not.
	leaf := func(full, alias string, at *ts.Node) {
		full = strings.TrimPrefix(full, "::")
		if full == "" || alias == "_" {
			return
		}
		i := strings.LastIndex(full, "::")
		last := full
		if i >= 0 {
			last = full[i+2:]
		}
		if alias == "" || alias == last {
			add(full, "", at)
			return
		}
		if i < 0 {
			add(full, alias, at)
			return
		}
		add(full[:i], memberAliasPrefix+alias+renameSeparator+last, at)
	}

	var use func(n *ts.Node, prefix, alias string, depth int, at *ts.Node)
	use = func(n *ts.Node, prefix, alias string, depth int, at *ts.Node) {
		if n == nil || depth > 12 {
			return
		}
		switch n.Type(lang) {
		case "identifier", "scoped_identifier", "crate", "self", "super", "metavariable":
			full := join(prefix, compact(n.Text(src)))
			if n.Type(lang) == "self" && prefix != "" {
				// `{self}` is the module itself.
				full = prefix
			}
			leaf(full, alias, at)
		case "use_as_clause":
			path := n.ChildByFieldName("path", lang)
			name := n.ChildByFieldName("alias", lang)
			if path == nil || name == nil {
				return
			}
			use(path, prefix, name.Text(src), depth+1, at)
		case "use_wildcard":
			base := prefix
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.IsNamed() {
					base = join(prefix, compact(c.Text(src)))
				}
			}
			add(join(strings.TrimPrefix(base, "::"), "*"), "", at)
		case "scoped_use_list":
			base := prefix
			if path := n.ChildByFieldName("path", lang); path != nil {
				base = join(prefix, compact(path.Text(src)))
			}
			if list := n.ChildByFieldName("list", lang); list != nil {
				use(list, base, "", depth+1, at)
			}
		case "use_list":
			for i := 0; i < n.ChildCount(); i++ {
				if c := n.Child(i); c != nil && c.IsNamed() {
					use(c, prefix, "", depth+1, at)
				}
			}
		}
	}

	// Every use declaration, wherever it is: a module body, a function body, an
	// impl. Iterative, so a deep tree cannot exhaust the stack.
	stack := []*ts.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Type(lang) == "use_declaration" {
			use(n.ChildByFieldName("argument", lang), "", "", 0, n)
			continue
		}
		for i := n.ChildCount() - 1; i >= 0; i-- {
			if c := n.Child(i); c != nil && c.IsNamed() {
				stack = append(stack, c)
			}
		}
	}
	return out
}
