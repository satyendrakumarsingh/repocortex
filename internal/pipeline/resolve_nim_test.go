package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestNimResolution indexes a small Nim package (a library of modules, an
// application and a test) and checks every call against what the module system
// makes it reach.
//
// What it is built to catch: `plus(x)` where other.nim also exports `plus` and
// `zero` and is not the module imported; `from tax import rate`, which brings one
// name in and not `apply`; a name written without the export marker (`secret`,
// `helper`), which no other module sees; `import money as m` and `m.scale(2)`;
// `util.slug "x"`, in command syntax, where src/lib/util.nim has a `slug` of its
// own and is not the file imported; `import util` from src/app.nim, which is the
// file beside it and not src/lib/util.nim; `import util except pad`; `include
// extras`; `import prelude`, whose `export tax` brings tax's names in;
// `Money(cents: 1)`, which constructs; method-call syntax (`m.scale(2)`) as
// uniform function-call syntax; `strutils.split` and `echo`, which are the
// standard library's; and a test beside the library that imports it by a path
// relative to itself.
//
// What it does not bind, by design: a name the module does not export, a name
// `from ... import` did not list, and a call on a value whose type the source does
// not state, which is bound at medium where one visible function has the name and
// at low where the name is also the language's own (`c.items.add(4)` on a
// sequence reaches the stack module's `add`, wrongly, at low).
func TestNimResolution(t *testing.T) {
	got := langEdges(t, nimFixture())
	high, medium, low := model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow
	wants := []want{
		// The file's own definitions, and what an `include` brings.
		{"src/cart.nim:total", "src/cart.nim:helper", high},
		{"src/cart.nim:checkout", "src/cart.nim:total", high},
		{"src/cart.nim:total", "src/extras.nim:extraPub", high},
		{"src/money.nim:plus", "src/money.nim:secret", high},
		// A name `import` brings in, `from ... import` lists, or `export` passes on.
		{"src/cart.nim:total", "src/money.nim:zero", high},
		{"src/cart.nim:total", "src/money.nim:plus", high},
		{"src/cart.nim:total", "src/tax.nim:rate", high},
		{"src/app.nim:main", "src/tax.nim:apply", high},
		{"src/app.nim:main", "src/util.nim:slug", high},
		// A qualified name: the module, its alias, or a module written by a relative path.
		{"src/cart.nim:total", "src/util.nim:slug", high},
		{"src/cart.nim:total", "src/util.nim:pad", high},
		{"src/app.nim:main", "src/cart.nim:total", high},
		{"src/app.nim:main", "src/money.nim:plus", high},
		// Construction.
		{"src/cart.nim:total", "src/money.nim:Money", high},
		{"src/app.nim:main", "src/cart.nim:Cart", high},
		{"tests/tcart.nim:c", "src/cart.nim:Cart", high},
		{"src/tax.nim:apply", "src/tax.nim:rate", high},
		// Method-call syntax, and a test beside the library.
		{"src/cart.nim:total", "src/money.nim:scale", medium},
		{"tests/tcart.nim:(top level)", "src/cart.nim:total", high},
		{"tests/tcart.nim:(top level)", "src/money.nim:zero", high},
	}
	forbid := []want{
		// other.nim is not the module imported, nor is src/lib/util.nim.
		{Src: "src/cart.nim", Dst: "src/other.nim"},
		{Src: "src/app.nim", Dst: "src/other.nim"},
		{Src: "src/cart.nim", Dst: "src/lib/util.nim"},
		{Src: "src/app.nim", Dst: "src/lib/util.nim"},
		// `from tax import rate` brings rate and nothing else; cart.nim does not call apply.
		{Src: "src/cart.nim", Dst: "src/tax.nim:apply"},
		// A name without the export marker is the module's own.
		{Src: "src/cart.nim", Dst: "src/money.nim:secret"},
		{Src: "src/app.nim", Dst: "src/util.nim:hidden"},
		// `import util except pad`.
		{Src: "src/app.nim", Dst: "src/util.nim:pad"},
		// The standard library's names are the standard library's.
		{Src: "src/", Dst: "src/debug.nim"},
	}
	// The one wrong edge the fixture is built to produce: `add` on a sequence reaches
	// the stack module's `add`, and it is low, as the language's own name is.
	known := want{Src: "src/cart.nim:total", Dst: "src/stack.nim:add"}
	scoreEdges(t, got, wants, forbid, nil, known)
	capped(t, got, []want{{"src/cart.nim:total", "src/stack.nim:add", low}})
}

func nimFixture() map[string]string {
	return map[string]string{
		"shop.nimble": `version = "0.1.0"
srcDir = "src"
`,
		"src/app.nim": `import cart, prelude
import money as m
import util except pad

proc main*() =
  let c = Cart()
  echo cart.total(c)
  echo m.plus(1, 2)
  echo slug("x")
  echo apply(3)
`,
		"src/cart.nim": `import std/strutils
import money, ./util, stack
from tax import rate
include extras

type
  Cart* = object
    items*: seq[int]

proc helper(x: int): int = x + 1

proc total*(c: Cart): int =
  let z = zero()
  var t = plus(z, 1)
  t += rate(t)
  echo helper(t)
  echo util.slug("x") & util.pad("y")
  echo strutils.split("a b").len
  let m = Money(cents: 1)
  discard m.scale(2)
  discard extraPub(1)
  c.items.add(4)
  t

proc checkout*(c: Cart): int =
  total(c)
`,
		"src/debug.nim": `proc trace*(x: int): int = x
proc echo*(s: string) = discard
proc split*(s: string): seq[string] = @[s]
`,
		"src/extras.nim": `proc extraPub*(x: int): int = x
`,
		"src/lib/util.nim": `proc slug*(s: string): string = s
`,
		"src/money.nim": `type
  Money* = object
    cents*: int

proc secret(x: int): int = x

proc zero*(): int = 0

proc plus*(a, b: int): int = a + b + secret(0)

proc scale*(m: Money, k: int): int = m.cents * k
`,
		// The same names as money.nim, in a module nobody imports.
		"src/other.nim": `proc zero*(): float = 0.0
proc plus*(a, b: float): float = a + b
`,
		"src/prelude.nim": `import tax
export tax
`,
		"src/stack.nim": `type
  Stack* = object
    xs*: seq[int]

proc add*(s: var Stack, x: int) =
  s.xs.add(x)
`,
		"src/tax.nim": `proc rate*(c: int): int = c div 5

proc apply*(c: int): int = c + rate(c)
`,
		"src/util.nim": `proc slug*(s: string): string = s

proc pad*(s: string): string = s

proc hidden(s: string): string = s
`,
		"tests/tcart.nim": `import ../src/cart
import ../src/money

let c = Cart()
doAssert total(c) == zero()
`,
	}
}
