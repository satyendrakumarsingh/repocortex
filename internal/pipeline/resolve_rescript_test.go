package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestReScriptResolution indexes a small ReScript project (a library of files, an
// entry file and a test) and checks every call against what the module system makes
// it reach.
//
// What it is built to catch: `Money.add` where OtherMoney.res also defines `add` and
// `zero`; the same names in Money.res and Money.resi, which are one function and
// bind to the implementation; `open Util`, which brings a name in, against a name
// the file defines itself after the open (`format`); `module M = Money` and
// `module T = Tax` (aliases of a file's module); a module written inside a file
// (`Inner`) whose names are its own and which sees the file's; a parameter called
// `slug` that the function applies; a local `let` that shadows an opened name; the
// right of a pipe (`x->Money.scale`, `x->pad`); `Js.log`, `Belt.Array.map` and
// `Array.length`, which are external, while Debug.res defines a `log` and a `map`
// that nobody opens; and `ignore`, which is Pervasives'.
//
// What it does not bind, by design: a function passed as an argument
// (`Belt.Array.map(xs, pad)`), a name written inside a local open, and a call on a
// value.
func TestReScriptResolution(t *testing.T) {
	got := langEdges(t, rescriptFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// The file's own functions, and those of the module a call is written in or inside.
		{"src/Cart.res:Inner.run", "src/Cart.res:Inner.deep", high},
		{"src/Cart.res:Inner.run", "src/Cart.res:helper", high},
		{"src/Cart.res:checkout", "src/Cart.res:total", high},
		{"src/Cart.res:describe", "src/Cart.res:format", high},
		{"src/Cart.res:total", "src/Cart.res:helper", high},
		{"src/Tax.res:apply", "src/Tax.res:rate", high},
		// A name `open` brings in.
		{"src/Cart.res:checkout", "src/Util.res:pad", high},
		{"src/Cart.res:total", "src/Util.res:slug", high},
		// A qualified name: the file's module, an alias of one, or a module of the file.
		{"src/Cart.res:checkout", "src/Cart.res:Inner.run", high},
		{"src/Cart.res:checkout", "src/Money.res:scale", high},
		{"src/Cart.res:describe", "src/Tax.res:rate", high},
		{"src/Cart.res:describe", "src/Util.res:format", high},
		{"src/Cart.res:total", "src/Money.res:add", high},
		{"src/Cart.res:total", "src/Money.res:zero", high},
		{"src/Cart.res:total", "src/Tax.res:rate", high},
		{"src/Main.res:(top level)", "src/Cart.res:checkout", high},
		{"src/Main.res:t", "src/Cart.res:total", high},
		{"src/Main.res:(top level)", "src/Util.res:format", high},
		{"__tests__/Cart_test.res:testMoney", "src/Money.res:add", high},
		{"__tests__/Cart_test.res:testTotal", "src/Cart.res:total", high},
	}
	forbid := []want{
		// OtherMoney.res is not the module that was named.
		{Src: "src/Cart.res", Dst: "src/OtherMoney.res"},
		{Src: "src/Main.res", Dst: "src/OtherMoney.res"},
		{Src: "__tests__/", Dst: "src/OtherMoney.res"},
		// The interface says what the implementation does, and is not a second function.
		{Src: "src/Cart.res", Dst: "src/Money.resi"},
		{Src: "__tests__/", Dst: "src/Money.resi"},
		// Js, Belt and Array are the language's, whatever Debug.res defines.
		{Src: "src/", Dst: "src/Debug.res"},
		{Src: "__tests__/", Dst: "src/Debug.res"},
		// A parameter named slug is applied by twice; a local let named pad by padded.
		{Src: "src/Cart.res:twice", Dst: "src/Util.res:slug"},
		{Src: "src/Cart.res:padded", Dst: "src/Util.res:pad"},
		{Src: "src/Cart.res:applyPad", Dst: "src/Util.res:pad"},
		// Util.format is not what the file's own `format` calls.
		{Src: "src/Cart.res:format", Dst: "src/Util.res:format"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func rescriptFixture() map[string]string {
	return map[string]string{
		"rescript.json": `{"name": "shop", "sources": ["src"]}
`,
		"src/Main.res": `let t = Cart.total([1, 2])
Js.log(Cart.checkout([t]))
Js.log(Util.format(t))
`,
		"src/Cart.res": `open Util
module M = Money
module T = Tax

let helper = x => x

let total = items => {
  let z = Money.zero()
  let t = T.rate(Array.length(items)) + helper(z)
  Js.log("x")
  Belt.Array.forEach(items, i => ignore(slug(i)))
  M.add(z, t)
}

module Inner = {
  let deep = x => x + 1
  let run = x => deep(x) + helper(x)
}

let checkout = c => {
  let n = total(c)
  Inner.run(n)->Money.scale(2)->pad(3)
}

let format = c => Belt.Int.toString(Array.length(c))

let describe = c => format(c) ++ Util.format(T.rate(3))

// slug is a parameter here, and the function applies it: Util.slug is not called.
let twice = (slug, n) => slug(slug(n))

// pad is a local let here, and the function applies it: Util.pad is not called.
let padded = n => {
  let pad = x => x
  pad(n)
}

// a single parameter written without parentheses is applied as well.
let applyPad = pad => pad(1)
`,
		// Names that the language owns, in a module nobody opens.
		"src/Debug.res": `let trace = x => x
let log = s => s
let map = (f, xs) => xs
`,
		"src/Money.res": `let zero = () => 0
let add = (a, b) => a + b
let scale = (n, k) => k * n
`,
		"src/Money.resi": `let zero: unit => int
let add: (int, int) => int
let scale: (int, int) => int
`,
		// The same names as Money, in a module nobody names.
		"src/OtherMoney.res": `let zero = () => 0.0
let add = (a, b) => a +. b
`,
		"src/Tax.res": `let rate = c => c / 5
let apply = c => c + rate(c)
`,
		"src/Util.res": `let slug = s => s
let pad = (n, s) => s
let format = n => Belt.Int.toString(n)
`,
		"__tests__/Cart_test.res": `let testTotal = () => Cart.total([1, 2])

let testMoney = () => Money.add(1, 2)
`,
	}
}
