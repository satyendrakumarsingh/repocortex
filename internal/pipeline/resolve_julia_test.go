package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestJuliaResolution indexes a small package and its test suite and checks
// every call against what `module`, `using`, `import` and `include` make it reach.
//
// What it is built to catch: `Money.zero_money()` where `OtherMoney` also defines
// one and is not the module named; `using .Money` (every exported name) against
// `import .Tax: rate` (the listed ones) and a name of a module that is neither;
// `import Money as M`; `include`, which makes the definitions of the file named
// part of the including module; a call in a nested module, which does not see the
// names of the module around it; `println`, `push!` and `sum`, which are Base's;
// `LinearAlgebra.norm`, which leaves the repository; a struct's constructor; a
// short-form definition (`helper(x) = ...`), which is not a call; one function with
// two methods, which the argument types choose between and no index can tell
// apart; and a test file that does `using Shop`.
func TestJuliaResolution(t *testing.T) {
	got := langEdges(t, juliaFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A module, qualified by its name, written in full or aliased.
		{"src/Shop.jl:Shop.total", "src/money.jl:Money.add", high},
		{"src/Shop.jl:Shop.checkout_total", "src/money.jl:Money.add", high},
		// `using .Money` brings in an exported name; `import .Tax: rate` one name.
		{"src/Shop.jl:Shop.total", "src/money.jl:Money.zero_money", high},
		{"src/Shop.jl:Shop.total", "src/tax.jl:Tax.rate", high},
		{"src/tax.jl:Tax.apply", "src/tax.jl:Tax.rate", high},
		// The module's own functions and type, one function written in the short form.
		{"src/Shop.jl:Shop.total", "src/Shop.jl:Shop.helper", high},
		{"src/Shop.jl:Shop.total", "src/Shop.jl:Shop.Item", high},
		// An include: the file named is part of this module.
		{"src/Shop.jl:Shop.total", "src/cart.jl:checkout", high},
		{"src/Shop.jl:Shop.total", "src/util/format.jl:fmt", high},
		{"src/cart.jl:checkout", "src/cart.jl:finish", high},
		// A nested module sees its own names; `using .Inner: deep` brings one out.
		{"src/Shop.jl:Shop.Inner.run", "src/Shop.jl:Shop.Inner.deep", high},
		{"src/Shop.jl:Shop.total", "src/Shop.jl:Shop.Inner.deep", high},
		// A test file uses the package.
		{"test/runtests.jl:(top level)", "src/Shop.jl:Shop.total", high},
		{"test/runtests.jl:(top level)", "src/Shop.jl:Shop.Item", high},
	}
	forbid := []want{
		// OtherMoney is not the module named.
		{Src: "src/Shop.jl", Dst: "src/other.jl"},
		{Src: "test/", Dst: "src/other.jl"},
		// Tax.apply is not in `import .Tax: rate`.
		{Src: "src/Shop.jl", Dst: "src/tax.jl:Tax.apply"},
		// A nested module does not see the module around it.
		{Src: "src/Shop.jl:Shop.Inner.run", Dst: "src/Shop.jl:Shop.total"},
		// Base's names are Base's.
		{Src: "src/", Dst: "src/debug.jl"},
	}
	// describe has two methods, `Int` and `String`. Both are in one module, so the
	// module is right; which method a call reaches depends on its argument's type,
	// which is not in the source, and the first is bound at low. The one call in
	// the fixture passes a String, which is the second method's: a known wrong
	// edge, counted against precision.
	known := []want{{"src/Shop.jl:Shop.total", "src/Shop.jl:Shop.describe", model.ConfidenceLow}}
	scoreEdges(t, got, wants, forbid, nil, known...)
	for _, e := range got {
		if e.Dst == "src/Shop.jl:Shop.describe" && e.Confidence != model.ConfidenceLow {
			t.Errorf("%s -> describe is %s, want low (two methods of the name)", e.Src, e.Confidence)
		}
	}
}

func juliaFixture() map[string]string {
	return map[string]string{
		"Project.toml": `name = "Shop"
uuid = "5b0d5d1c-0000-0000-0000-000000000001"
`,
		"src/money.jl": `module Money

export zero_money, add

zero_money() = 0

function add(a, b)
    return a + b
end

end
`,
		"src/tax.jl": `module Tax

export rate

function rate(c)
    return 0.2
end

function apply(c)
    return c * rate(c)
end

end
`,
		// The same names as Money, in a module nobody names.
		"src/other.jl": `module OtherMoney

export zero_money

function zero_money()
    return 0.0
end

end
`,
		// Names that are Base's, defined by a file nobody includes.
		"src/debug.jl": `module Debug

function println(x)
    return x
end

function length(x)
    return x
end

function sum(xs)
    return xs
end

end
`,
		"src/cart.jl": `function checkout(c)
    finish(c)
end

function finish(c)
    return c
end
`,
		"src/util/format.jl": `function fmt(x)
    return string(x)
end
`,
		"src/Shop.jl": `module Shop

using LinearAlgebra
using .Money
import .Tax: rate
import .Money as M

include("cart.jl")
include("util/format.jl")

export total, Item

struct Item
    name::String
    price::Float64
end

helper(x) = x * 2

describe(x::Int) = "int"
describe(x::String) = "string"

function total(items)
    s = zero_money()
    for i in items
        s += Money.add(s, i.price)
    end
    println(s)
    push!(items, s)
    sum(items)
    LinearAlgebra.norm(items)
    Item("a", 1.0)
    rate(s)
    helper(s)
    checkout(s)
    fmt(s)
    deep(s)
    describe("a")
    return s
end

function checkout_total(c)
    return M.add(c, 1)
end

module Inner
    function deep(x)
        return x
    end

    function run(x)
        deep(x)
        total(x)
    end
end

using .Inner: deep

end
`,
		"test/runtests.jl": `using Test
using Shop

@testset "shop" begin
    @test total([]) == 0
    @test Item("a", 1.0).price == 1.0
    @test Shop.Money.zero_money() == 0
end
`,
	}
}
