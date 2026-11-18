package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestImplementationBeatsDeclaration checks, in Java and in C#, the rule the
// PHP fixture was written for: when a class's ancestry has both a class that
// implements a member and an interface that declares it, a call reaches the
// implementation.
//
// `Cart extends Order implements Describable`, and Describable and Base both
// have `describe`; Order inherits it from Base. The interface is the NEARER
// ancestor, and stopping at the nearer level bound the call to the interface's
// declaration. `Ticket` only implements the interface, so there the
// declaration is all there is and stays the answer.
func TestImplementationBeatsDeclaration(t *testing.T) {
	high := model.ConfidenceHigh
	medium := model.ConfidenceMedium
	cases := map[string]struct {
		files  map[string]string
		wants  []want
		forbid []want
		extra  []want
	}{
		"java": {
			files: map[string]string{
				"src/shop/Describable.java": "package shop;\n\npublic interface Describable {\n    String describe(String name);\n}\n",
				"src/shop/Base.java":        "package shop;\n\npublic class Base {\n    public String describe(String name) {\n        return name;\n    }\n}\n",
				"src/shop/Order.java":       "package shop;\n\npublic class Order extends Base {\n}\n",
				"src/shop/Cart.java":        "package shop;\n\npublic class Cart extends Order implements Describable {\n    public String label() {\n        return this.describe(\"cart\");\n    }\n}\n",
				"src/shop/Ticket.java":      "package shop;\n\npublic abstract class Ticket implements Describable {\n    public String show() {\n        return this.describe(\"ticket\");\n    }\n}\n",
			},
			wants: []want{
				{"src/shop/Cart.java:Cart.label", "src/shop/Base.java:Base.describe", high},
				{"src/shop/Ticket.java:Ticket.show", "src/shop/Describable.java:Describable.describe", high},
			},
			forbid: []want{
				{Src: "src/shop/Cart.java:Cart.label", Dst: "Describable.describe"},
				{Src: "src/shop/Ticket.java:Ticket.show", Dst: "Base.describe"},
			},
			extra: []want{
				{"Cart.java:Cart", "Describable", medium},
				{"Cart.java:Cart", "Order", medium},
				{"Order.java:Order", "Base", medium},
				{"Ticket.java:Ticket", "Describable", medium},
			},
		},
		"c_sharp": {
			files: map[string]string{
				"src/Describable.cs": "namespace Shop;\n\npublic interface Describable\n{\n    string Describe(string name);\n}\n",
				"src/Base.cs":        "namespace Shop;\n\npublic class Base\n{\n    public string Describe(string name)\n    {\n        return name;\n    }\n}\n",
				"src/Order.cs":       "namespace Shop;\n\npublic class Order : Base\n{\n}\n",
				"src/Cart.cs":        "namespace Shop;\n\npublic class Cart : Order, Describable\n{\n    public string Label()\n    {\n        return this.Describe(\"cart\");\n    }\n}\n",
				"src/Ticket.cs":      "namespace Shop;\n\npublic abstract class Ticket : Describable\n{\n    public string Show()\n    {\n        return this.Describe(\"ticket\");\n    }\n}\n",
			},
			wants: []want{
				{"src/Cart.cs:Cart.Label", "src/Base.cs:Base.Describe", high},
				{"src/Ticket.cs:Ticket.Show", "src/Describable.cs:Describable.Describe", high},
			},
			forbid: []want{
				{Src: "src/Cart.cs:Cart.Label", Dst: "Describable.Describe"},
				{Src: "src/Ticket.cs:Ticket.Show", Dst: "Base.Describe"},
			},
			extra: []want{
				{"Cart.cs:Cart", "Describable", medium},
				{"Cart.cs:Cart", "Order", medium},
				{"Order.cs:Order", "Base", medium},
				{"Ticket.cs:Ticket", "Describable", medium},
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := langEdges(t, c.files)
			for _, e := range got {
				t.Logf("%s -> %s (%s %s: %s)", e.Src, e.Dst, e.Kind, e.Confidence, e.Reason)
			}
			scoreEdges(t, got, c.wants, c.forbid, c.extra)
		})
	}
}
