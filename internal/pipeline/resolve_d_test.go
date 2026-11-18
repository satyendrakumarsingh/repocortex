package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestDResolution indexes a small D package (a library of modules and an
// application) and checks every call against what the module system makes it reach.
//
// What it is built to catch: `plus(x)` where other.d also defines `plus` and
// `zero` and is not the module imported; `import shop.util : slug`, which brings one
// name in and not `pad`; `import fmt = shop.format`, which names the qualifier fmt
// and brings nothing in bare; `static import shop.util` and `shop.util.pad(x)`;
// `public import shop.tax` in base.d, which brings tax's names to whoever imports
// base; a `private` function, which no other module sees; a member called by its
// bare name inside the class that declares it, against a free function of the same
// name in another module; `new Money(1)`, which constructs; `Cart.make()`, a static
// member reached through its type; `m.scale(2)` on a value whose type is not stated,
// which is bound to the one member of that name at low; `writeln`, which is
// std.stdio's, while debug.d defines one that nobody imports; and a `unittest`
// block, whose calls belong to the file.
//
// What it does not bind, by design: a name an `import a : f` did not list, a call on
// an import that renames (`import x = a.b`) written bare, and a member inherited from
// a base class.
func TestDResolution(t *testing.T) {
	got := langEdges(t, dFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		// The file's own definitions, and the members of the class around the call.
		{"src/shop/cart.d:Cart.total", "src/shop/cart.d:Cart.helper", high},
		{"src/shop/cart.d:Cart.again", "src/shop/cart.d:Cart.total", high},
		{"src/shop/cart.d:Cart.total", "src/shop/cart.d:Cart.tally", high},
		{"src/shop/cart.d:topLevel", "src/shop/cart.d:Cart.make", high},
		{"src/shop/money.d:plus", "src/shop/money.d:secret", high},
		{"src/shop/tax.d:apply", "src/shop/tax.d:rate", high},
		// A name `import` brings in or `import a : f` lists.
		{"src/shop/cart.d:Cart.total", "src/shop/money.d:zero", high},
		{"src/shop/cart.d:Cart.total", "src/shop/money.d:plus", high},
		{"src/shop/cart.d:Cart.total", "src/shop/util.d:slug", high},
		// A name that a `public import` passes on.
		{"src/app.d:main", "src/shop/tax.d:apply", high},
		// A qualified name: an alias, the module written in full, or a type.
		{"src/shop/cart.d:Cart.total", "src/shop/format.d:pad", high},
		{"src/app.d:main", "src/shop/util.d:pad", high},
		{"src/app.d:main", "src/shop/cart.d:Cart.make", high},
		// Construction.
		{"src/shop/cart.d:Cart.total", "src/shop/money.d:Money", high},
		{"src/shop/cart.d:Cart.make", "src/shop/cart.d:Cart", high},
		{"src/app.d:main", "src/shop/cart.d:Cart", high},
		// A method on a value whose type the source does not state.
		{"src/shop/cart.d:Cart.total", "src/shop/money.d:Money.scale", low},
		// The calls of a unittest block belong to the file.
		{"src/shop/cart.d:(top level)", "src/shop/cart.d:Cart.total", high},
		{"src/shop/cart.d:(top level)", "src/shop/cart.d:Cart", high},
		// A free function of the file, beside a free function of the same name in a module that is
		// imported under a qualifier only.
		{"src/shop/cart.d:Cart.padded", "src/shop/cart.d:pad", high},
	}
	forbid := []want{
		// other.d is not the module imported.
		{Src: "src/shop/cart.d", Dst: "src/shop/other.d"},
		{Src: "src/app.d", Dst: "src/shop/other.d"},
		// `import shop.util : slug` lists one name.
		{Src: "src/shop/cart.d:Cart.total", Dst: "src/shop/util.d:pad"},
		// A private name is the module's own, and so is one of a private class member.
		{Src: "src/shop/cart.d", Dst: "src/shop/money.d:secret"},
		{Src: "src/app.d", Dst: "src/shop/util.d:hidden"},
		// std.stdio's names are std.stdio's.
		{Src: "src/", Dst: "src/shop/debug.d"},
		// `import fmt = shop.format` brings nothing in bare: the file's own pad is not format's.
		{Src: "src/shop/cart.d:Cart.padded", Dst: "src/shop/format.d:pad"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func dFixture() map[string]string {
	return map[string]string{
		"dub.json": `{"name": "shop", "sourcePaths": ["src"]}
`,
		"src/app.d": `module app;

import shop.cart;
import shop.base;
static import shop.util;

void main() {
    auto c = new Cart();
    Cart.make();
    apply(3);
    shop.util.pad("x");
}
`,
		"src/shop/base.d": `module shop.base;

public import shop.tax;
`,
		"src/shop/cart.d": `module shop.cart;

import std.stdio;
import shop.money;
import shop.util : slug;
import fmt = shop.format;

class Cart {
    private int helper(int x) { return x; }

    int tally() { return 1; }

    int total() {
        auto z = zero();
        auto m = new Money(1);
        m.scale(2);
        writeln("x");
        slug("a");
        fmt.pad("b");
        return helper(1) + tally() + plus(z, 1);
    }

    int again() { return total(); }

    int padded() { return pad("c"); }

    static Cart make() { return new Cart(); }
}

int pad(string s) { return 1; }

void topLevel() {
    Cart.make();
}

unittest {
    assert(new Cart().total() == 0);
}
`,
		"src/shop/debug.d": `module shop.debug;

void writeln(string s) {}
void trace(int x) {}
`,
		"src/shop/format.d": `module shop.format;

string pad(string s) { return s; }
`,
		"src/shop/money.d": `module shop.money;

class Money {
    int cents;
    this(int c) { cents = c; }
    int scale(int k) { return cents * k; }
}

private int secret(int x) { return x; }

int zero() { return 0; }

int plus(int a, int b) { return a + b + secret(0); }
`,
		// The same names as money.d, in a module nobody imports.
		"src/shop/other.d": `module shop.other;

int zero() { return 1; }
int plus(int a, int b) { return a + b; }
`,
		"src/shop/tax.d": `module shop.tax;

int rate(int c) { return c / 5; }

int apply(int c) { return c + rate(c); }
`,
		"src/shop/util.d": `module shop.util;

string slug(string s) { return s; }

string pad(string s) { return s; }

private string hidden(string s) { return s; }
`,
	}
}
