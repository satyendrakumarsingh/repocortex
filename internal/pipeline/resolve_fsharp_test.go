package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestFSharpResolution indexes a small F# project (modules, a namespace with a
// nested module, a program and an Expecto test) and checks every call against what
// `namespace`, `module` and `open` make it reach.
//
// What it is built to catch: `Money.zero ()` where `Shop.OtherMoney` also defines
// `zero` and `add`, which a partly written name (`Money`) must not reach; `open
// Shop.Util` (a name brought in) against a module nobody opens (Shop.Debug has a
// `printfn` and a `map` of its own); `module M = Shop.Money` (an alias); a module
// nested in another (`Cart.Inner`), whose names are its own and which sees the
// ones around it; a namespace whose module is in another file (`Tax`);
// `printfn`, `ignore`, `string` and `List.map`, which are FSharp.Core's; the right
// of `|>`; a call the grammar nests the wrong way round (`f a + g b` read as `(f a
// + g) b`); a name of three segments (`Shop.Tax.rate`); a parameter called `slug`
// that the function applies, which is not Util's; and a test that opens the
// namespace.
//
// What it does not bind, by design: a member of a type (`order.Run()`), a function
// passed as an argument, and the compile order. F# compiles the files in the order
// the .fsproj lists them and a file sees only the files before it; the .fsproj is
// not read, so a name is bound whichever file holds it.
func TestFSharpResolution(t *testing.T) {
	got := langEdges(t, fsharpFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A module's own functions, and those of the modules around the call.
		{"src/Cart.fs:Shop.Cart.Inner.run", "src/Cart.fs:Shop.Cart.Inner.deep", high},
		{"src/Cart.fs:Shop.Cart.Inner.run", "src/Cart.fs:Shop.Cart.helper", high},
		{"src/Cart.fs:Shop.Cart.checkout", "src/Cart.fs:Shop.Cart.total", high},
		{"src/Cart.fs:Shop.Cart.total", "src/Cart.fs:Shop.Cart.helper", high},
		{"src/Tax.fs:Shop.Tax.apply", "src/Tax.fs:Shop.Tax.rate", high},
		// A name `open` brings in.
		{"src/Cart.fs:Shop.Cart.total", "src/Util.fs:Shop.Util.pad", high},
		{"src/Cart.fs:Shop.Cart.total", "src/Util.fs:Shop.Util.slug", high},
		// A qualified name: a module written in full, partly (`Money.zero`), or through `module M = Shop.Money`.
		{"src/Cart.fs:Shop.Cart.checkout", "src/Cart.fs:Shop.Cart.Inner.run", high},
		{"src/Cart.fs:Shop.Cart.total", "src/Money.fs:Shop.Money.zero", high},
		{"src/Cart.fs:Shop.Cart.total", "src/Tax.fs:Shop.Tax.rate", high},
		{"src/Program.fs:Shop.Program.main", "src/Cart.fs:Shop.Cart.checkout", high},
		{"src/Program.fs:Shop.Program.main", "src/Cart.fs:Shop.Cart.total", high},
		{"src/Program.fs:Shop.Program.main", "src/Tax.fs:Shop.Tax.apply", high},
		{"src/Program.fs:Shop.Program.main", "src/Util.fs:Shop.Util.format", high},
		{"tests/CartTests.fs:Shop.Tests.CartTests.tests", "src/Cart.fs:Shop.Cart.total", high},
		{"tests/CartTests.fs:Shop.Tests.CartTests.tests", "src/Money.fs:Shop.Money.add", high},
	}
	forbid := []want{
		// Shop.OtherMoney is not the module `Money` names.
		{Src: "src/Cart.fs", Dst: "src/Other.fs"},
		{Src: "src/Program.fs", Dst: "src/Other.fs"},
		{Src: "tests/", Dst: "src/Other.fs"},
		// Nothing opens Shop.Debug, whose names are FSharp.Core's.
		{Src: "src/", Dst: "src/Debug.fs"},
		{Src: "tests/", Dst: "src/Debug.fs"},
		// A parameter named slug is applied by twice.
		{Src: "src/Cart.fs:Shop.Cart.twice", Dst: "src/Util.fs:Shop.Util.slug"},
		// Inner is a module of its own: the nested names are not the outer's.
		{Src: "src/Cart.fs:Shop.Cart.total", Dst: "src/Cart.fs:Shop.Cart.Inner.deep"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func fsharpFixture() map[string]string {
	return map[string]string{
		"Shop.fsproj": `<Project Sdk="Microsoft.NET.Sdk">
  <ItemGroup>
    <Compile Include="src/Money.fs" />
    <Compile Include="src/Util.fs" />
    <Compile Include="src/Cart.fs" />
  </ItemGroup>
</Project>
`,
		"src/Cart.fs": `namespace Shop

open System
open Shop.Util
module M = Shop.Money

module Cart =
    let helper x = x

    let total (items: int list) =
        let z = Money.zero ()
        let t = M.add z (Tax.rate 3)
        printfn "x"
        slug "a" |> ignore
        items |> List.map (pad 3) |> ignore
        helper t

    module Inner =
        let deep x = x + 1

        let run x = deep (helper x)

    let checkout c =
        let n = total c
        Inner.run n |> M.scale 2

    // slug is a parameter here, and the function applies it: Util.slug is not called.
    let twice slug n = slug (slug n)
`,
		// Names that FSharp.Core owns, in a module nobody opens.
		"src/Debug.fs": `module Shop.Debug

let trace x = x

let printfn x = x

let map f xs = xs
`,
		"src/Money.fs": `module Shop.Money

let zero () = 0

let add a b = a + b

let scale k n = k * n
`,
		// The same names as Money, in a module nobody names.
		"src/Other.fs": `module Shop.OtherMoney

let zero () = 0.0

let add (a: float) b = a + b
`,
		"src/Program.fs": `module Shop.Program

open Shop

let main argv =
    let t = Cart.total [ 1; 2 ]
    printfn "%d" (Cart.checkout [ t ])
    Util.format t |> ignore
    Tax.apply t
`,
		"src/Tax.fs": `namespace Shop

module Tax =
    let rate c = c / 5

    let apply c = c + rate c
`,
		"src/Util.fs": `module Shop.Util

let slug (s: string) = s.ToLower()

let pad n (s: string) = s.PadLeft n

let format n = string n
`,
		"tests/CartTests.fs": `module Shop.Tests.CartTests

open Expecto
open Shop

let tests =
    testList "cart" [
        testCase "total" <| fun () ->
            Expect.equal (Cart.total [ 1 ]) 1 "total"
            Expect.equal (Money.add 1 2) 3 "add"
    ]
`,
	}
}
