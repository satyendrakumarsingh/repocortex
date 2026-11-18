package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCrystalResolution indexes a small shard (a library, an entry file and a spec)
// and checks every call against what Crystal's global, open types make it reach.
//
// What it is built to catch: `Money.zero` where OtherMoney also defines `zero` and
// `plus`; `Money.new(5)`, which constructs; `Shop::Util.slug` and `Util.slug` (a
// module reached by its full name and by the name inside its namespace); a bare
// call of a method of the class around it, against a top-level function of another
// file; `x : Money` and `x.plus(y)`, a call on a stated type; `Cart.build` reached
// by a path that names a class opened in two files; a private method, which no other
// file calls; `puts`, `Array#size` and `JSON.parse`, which are the standard
// library's, while a type of the repository has a `size` and a `parse` too; a
// top-level `def` of one file called from another; and a spec that requires the
// library by a path relative to itself.
//
// What it does not bind, by design: a method a type gets by `include` of a module
// (the call is bare and the module is not read), a call on a value whose type the
// source does not state where several types declare the name, and a macro.
func TestCrystalResolution(t *testing.T) {
	got := langEdges(t, crystalFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		// The class's own members and the file's own functions.
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/cart.cr:Shop.Cart.helper", high},
		{"src/shop/cart.cr:Shop.Cart.again", "src/shop/cart.cr:Shop.Cart.total", high},
		{"src/shop/money.cr:Money.plus", "src/shop/money.cr:Money.secret", high},
		{"src/shop/money.cr:Money.zero", "src/shop/money.cr:Money", high},
		// A type reached by its name, from a file that requires its file.
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/money.cr:Money.zero", high},
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/money.cr:Money", high},
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/util.cr:Shop.Util.slug", high},
		{"src/shop/cart.cr:Shop.Cart.again", "src/shop/util.cr:Shop.Util.pad", high},
		// A method on a stated type.
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/money.cr:Money.plus", high},
		// A top-level def of another file.
		{"src/shop/cart.cr:Shop.Cart.total", "src/shop/util.cr:greet", high},
		// Another file of the shard, and the spec.
		{"src/shop.cr:(top level)", "src/shop/cart.cr:Shop.Cart.build", high},
		{"spec/cart_spec.cr:(top level)", "src/shop/cart.cr:Shop.Cart.build", high},
		{"spec/cart_spec.cr:(top level)", "src/shop/money.cr:Money.zero", high},
		// A method on a value whose type the source does not state.
		{"spec/cart_spec.cr:(top level)", "src/shop/cart.cr:Shop.Cart.total", low},
	}
	forbid := []want{
		// OtherMoney is not the type named.
		{Src: "src/shop/cart.cr", Dst: "src/shop/other.cr"},
		{Src: "spec/", Dst: "src/shop/other.cr"},
		// A private method is the type's own.
		{Src: "src/shop/cart.cr", Dst: "src/shop/money.cr:Money.secret"},
		{Src: "spec/", Dst: "src/shop/money.cr:Money.secret"},
		// The standard library's names are the standard library's.
		{Src: "src/shop/cart.cr", Dst: "src/shop/money.cr:Money.size"},
		{Src: "src/shop/cart.cr", Dst: "src/shop/util.cr:Shop.Util.parse"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func crystalFixture() map[string]string {
	return map[string]string{
		"shard.yml": `name: shop
version: 0.1.0
`,
		"src/shop.cr": `require "./shop/*"

Shop::Cart.build
`,
		"src/shop/cart.cr": `require "json"
require "./money"
require "./util"

module Shop
  class Cart
    def initialize(@items : Array(Int32))
    end

    def self.build
      new([1])
    end

    def total : Int32
      z = Money.zero
      m = Money.new(5)
      x : Money = Money.new(1)
      x.plus(m)
      Shop::Util.slug("a")
      greet("x")
      puts "hi"
      JSON.parse("{}")
      @items.size
      helper(1)
    end

    def again
      Util.pad("b")
      total()
    end

    private def helper(x)
      x
    end
  end
end
`,
		"src/shop/money.cr": `class Money
  def initialize(@cents : Int32)
  end

  def self.zero
    Money.new(0)
  end

  def plus(other : Money)
    secret() + other.secret()
  end

  def size
    1
  end

  private def secret
    0
  end
end
`,
		// The same names as Money, in a type nobody names.
		"src/shop/other.cr": `class OtherMoney
  def self.zero
    1
  end

  def plus(other)
    other
  end
end
`,
		"src/shop/util.cr": `module Shop::Util
  def self.slug(s)
    s
  end

  def self.pad(s)
    s
  end

  def self.parse(s)
    s
  end
end

def greet(name)
  name
end
`,
		"spec/cart_spec.cr": `require "../src/shop"

cart = Shop::Cart.build
cart.total
Money.zero
`,
	}
}
