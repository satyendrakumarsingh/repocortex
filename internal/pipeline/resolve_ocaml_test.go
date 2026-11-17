package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestOCamlResolution indexes a small dune project (a library, an executable
// and an Alcotest suite) and checks every call against what the module system
// makes it reach.
//
// What it is built to catch: `Money.add` where `other_money.ml` also defines
// `add` and `zero`; the same name in money.ml and money.mli, which is one
// function and binds to the implementation; `module M = Money` and `module T =
// Shop.Tax` (an alias, and a module behind the name of a dune library);
// `open Util`, which brings a name in, against a name the file defines itself
// after the open (`format`); a module written inside a file (`Inner`) whose
// names are its own and which sees the file's; `print_endline`, `ignore`,
// `List.map` and `Hashtbl.length`, which are the Stdlib's, while debug.ml defines
// a `print_endline` and a `map` nobody opens; `module L = List`, an alias of a
// Stdlib module; a local open (`Money.(add n ...)`), which names a module for one
// expression and binds nothing; a parameter called `slug` that the function
// applies; the right of `|>`; and a test that calls the library by its wrapped
// name.
//
// What it does not bind, by design: a function passed as an argument (`List.map
// pad xs`) and a name written inside a local open.
func TestOCamlResolution(t *testing.T) {
	got := langEdges(t, ocamlFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// The file's own functions, and those of the module a call is written in or inside.
		{"src/cart.ml:Inner.run", "src/cart.ml:Inner.deep", high},
		{"src/cart.ml:Inner.run", "src/cart.ml:helper", high},
		{"src/cart.ml:checkout", "src/cart.ml:total", high},
		{"src/cart.ml:describe", "src/cart.ml:format", high},
		{"src/cart.ml:total", "src/cart.ml:helper", high},
		{"src/tax.ml:apply", "src/tax.ml:rate", high},
		// A name `open` brings in.
		{"src/cart.ml:checkout", "src/util.ml:pad", high},
		{"src/cart.ml:total", "src/util.ml:slug", high},
		// A qualified name: the file's module, an alias of one, a module of the file, or the module a dune library wraps.
		{"bin/main.ml:(top level)", "src/cart.ml:checkout", high},
		{"bin/main.ml:(top level)", "src/cart.ml:total", high},
		{"bin/main.ml:(top level)", "src/tax.ml:apply", high},
		{"bin/main.ml:(top level)", "src/util.ml:format", high},
		{"src/cart.ml:checkout", "src/cart.ml:Inner.run", high},
		{"src/cart.ml:checkout", "src/money.ml:scale", high},
		{"src/cart.ml:describe", "src/tax.ml:rate", high},
		{"src/cart.ml:describe", "src/util.ml:format", high},
		{"src/cart.ml:total", "src/money.ml:add", high},
		{"src/cart.ml:total", "src/money.ml:zero", high},
		{"src/cart.ml:total", "src/tax.ml:rate", high},
		{"test/test_cart.ml:test_money", "src/money.ml:add", high},
		{"test/test_cart.ml:test_total", "src/cart.ml:total", high},
	}
	forbid := []want{
		// other_money.ml is not the module that was named.
		{Src: "src/cart.ml", Dst: "src/other_money.ml"},
		{Src: "bin/", Dst: "src/other_money.ml"},
		{Src: "test/", Dst: "src/other_money.ml"},
		// The interface says what the implementation does, and is not a second function.
		{Src: "src/cart.ml", Dst: "src/money.mli"},
		{Src: "test/", Dst: "src/money.mli"},
		// Stdlib's names are Stdlib's, whatever debug.ml defines.
		{Src: "src/", Dst: "src/debug.ml"},
		{Src: "bin/", Dst: "src/debug.ml"},
		{Src: "test/", Dst: "src/debug.ml"},
		// A parameter named slug is applied by twice.
		{Src: "src/cart.ml:twice", Dst: "src/util.ml:slug"},
		// Inside a local open the names are written bare and are not the file's imports.
		{Src: "src/cart.ml:scaled", Dst: "src/money.ml"},
		// Util.format is not what the file's own `format` calls.
		{Src: "src/cart.ml:format", Dst: "src/util.ml:format"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func ocamlFixture() map[string]string {
	return map[string]string{
		"bin/main.ml": `open Shop

let () =
  let t = Cart.total [ 1; 2 ] in
  print_endline (Cart.checkout [ t ]);
  print_endline (Util.format t);
  ignore (Tax.apply t)
`,
		"dune-project": `(lang dune 3.0)
`,
		"src/cart.ml": `open Util
module M = Money
module T = Shop.Tax
module L = List

let helper x = x

let total items =
  let z = Money.zero () in
  let t = T.rate (List.length items) + helper z in
  print_endline "x";
  List.iter (fun i -> ignore (slug i)) items;
  Printf.printf "%d" t;
  M.add z t

module Inner = struct
  let deep x = x + 1
  let run x = deep x + helper x
end

let checkout c =
  let n = total c in
  Inner.run n |> M.scale 2 |> pad 3

let counts m = Hashtbl.length m

let format c = string_of_int (L.length c)

let describe c = format c ^ Util.format (T.rate 3)

let scaled n = Money.(add n (scale 2 n))

(* slug is a parameter here, and the function applies it: Util.slug is not called. *)
let twice slug n = slug (slug n)
`,
		// Names that Stdlib owns, in a module nobody opens.
		"src/debug.ml": `let trace x = x
let print_endline s = s
let map f xs = xs
`,
		"src/dune": `(library (name shop))
`,
		"src/money.ml": `let zero () = 0
let add a b = a + b
let scale k n = k * n
`,
		"src/money.mli": `val zero : unit -> int
val add : int -> int -> int
val scale : int -> int -> int
`,
		// The same names as Money, in a module nobody names.
		"src/other_money.ml": `let zero () = 0.0
let add a b = a +. b
`,
		"src/tax.ml": `let rate c = c / 5
let apply c = c + rate c
`,
		"src/util.ml": `let slug s = s
let pad n s = s
let format n = string_of_int n
`,
		"test/test_cart.ml": `let test_total () =
  Alcotest.(check int) "sum" 3 (Shop.Cart.total [ 1; 2 ])

let test_money () =
  Alcotest.(check int) "add" 3 (Shop.Money.add 1 2)

let () =
  Alcotest.run "cart" [ ("total", [ Alcotest.test_case "total" ` + "`" + `Quick test_total; Alcotest.test_case "money" ` + "`" + `Quick test_money ]) ]
`,
	}
}
