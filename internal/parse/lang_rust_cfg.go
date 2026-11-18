package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// # Rust: an item under #[cfg(...)]
//
// sqlparser declares `RecursionCounter` twice, once in `#[cfg(feature = "std")]
// mod recursion` and once in `#[cfg(not(feature = "std"))] mod recursion`, each
// with a `try_decrease`. A build keeps one; the index holds both under one
// qualified name, and a call of `try_decrease` bound to the first at `high`
// while rust-analyzer, which analyses one configuration, said it was the second.
//
// The parser records that a definition is conditional, so the resolver can tell
// two alternatives from one definition. An item is conditional when it carries
// a `cfg` attribute itself or sits inside an item that does: the attribute
// walks down to everything in the module or impl it gates. `cfg_attr` is not
// one: it gates an attribute, not the item.

// rustConditional reports whether a definition node, or an item enclosing it,
// has an outer `#[cfg(...)]` attribute.
func (p *genericParser) rustConditional(def *ts.Node, src []byte) bool {
	for n, depth := def, 0; n != nil && depth < 24; n, depth = n.Parent(), depth+1 {
		for sib := n.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
			switch sib.Type(p.lang) {
			case "attribute_item":
				if isCfgAttribute(sib.Text(src)) {
					return true
				}
			case "line_comment", "block_comment":
			default:
				sib = nil
			}
			if sib == nil {
				break
			}
		}
	}
	return false
}

// isCfgAttribute reports whether the text of an attribute is `#[cfg(...)]`.
func isCfgAttribute(text string) bool {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "#")
	t = strings.TrimSpace(strings.TrimPrefix(t, "["))
	if !strings.HasPrefix(t, "cfg") {
		return false
	}
	rest := strings.TrimSpace(t[len("cfg"):])
	return strings.HasPrefix(rest, "(")
}
