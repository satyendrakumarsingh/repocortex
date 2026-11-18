package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ownersBy returns a definitionRefiner that qualifies each definition by the
// types and modules written around it, outermost first, joined with dots
// (`Shop.Cart.total`). It is for a grammar that gives its type declarations no
// `name` and `body` fields, which is what shape.go's own walk reads an owner from.
//
// containers maps the node types that are an owner to the type of their child that
// holds the name; the first such child is the name, and a `::` in it is a dot
// (Crystal's `class Shop::Cart`). skip lists the definition kinds that are never
// qualified (a type is qualified by what is around it, and so is everything else:
// the list is for a kind the language does not nest, such as D's local function).
func ownersBy(containers map[string]string, skip ...model.EntityKind) definitionRefiner {
	return func(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
		var fixes map[uint32]definitionFix
	tags:
		for _, tag := range tags {
			if !strings.HasPrefix(tag.Kind, "definition.") {
				continue
			}
			for _, k := range skip {
				if kindFromTag(tag.Kind) == k {
					continue tags
				}
			}
			n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
			var chain []string
			for p := n; p != nil; p = p.Parent() {
				nameType, ok := containers[p.Type(lang)]
				if !ok {
					continue
				}
				var name *ts.Node
				for i := 0; i < p.ChildCount() && name == nil; i++ {
					if c := p.Child(i); c != nil && c.Type(lang) == nameType {
						name = c
					}
				}
				if name == nil {
					continue
				}
				if name.StartByte() == tag.NameRange.StartByte {
					// The container a definition names is not its own owner, but a namespace
					// written in its name is: `Shop::` of `class Shop::Cart`.
					if text := strings.TrimSpace(name.Text(src)); strings.Contains(text, "::") {
						chain = append([]string{strings.ReplaceAll(text[:strings.LastIndex(text, "::")], "::", ".")}, chain...)
					}
					continue
				}
				chain = append([]string{strings.ReplaceAll(strings.TrimSpace(name.Text(src)), "::", ".")}, chain...)
			}
			if len(chain) == 0 {
				continue
			}
			if fixes == nil {
				fixes = map[uint32]definitionFix{}
			}
			fixes[tag.NameRange.StartByte] = definitionFix{owner: strings.Join(chain, ".")}
		}
		return fixes
	}
}
