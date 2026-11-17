package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestElixirResolution indexes a small Mix application and its ExUnit tests
// and checks every call against what the module system makes it reach.
//
// What it is built to catch: `Money.zero()` where two modules end in `Money`
// (Shop.Money and Billing.Money) and only one is aliased; `alias A.B, as: C`
// and `alias A.{B, C}`; a bare call reaching a function that an `import`
// brought in (all of a module's, or only those listed) and one it did not; a
// function that shares its name with Kernel's (`length`, `inspect`); a bare
// call to a function of a module that was not imported; `Enum` and `Logger`,
// which leave the repository; two modules in one file; `__MODULE__.f()`; and
// a call to a function that is declared in the same module at two arities.
func TestElixirResolution(t *testing.T) {
	got := langEdges(t, elixirFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// An alias, with and without `as:`, in a group, and written in full.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/money.ex:Shop.Money.zero", high},
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/billing/invoice.ex:Billing.Invoice.render", high},
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/tax.ex:Shop.Tax.rate", high},
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/order.ex:Shop.Order.place", high},
		{"lib/shop/cart.ex:Shop.Cart.checkout", "lib/billing/money.ex:Billing.Money.zero", high},
		// The module's own functions, and through `__MODULE__`.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/cart.ex:Shop.Cart.helper", high},
		{"lib/shop/cart.ex:Shop.Cart.checkout", "lib/shop/cart.ex:Shop.Cart.total", high},
		{"lib/shop/util.ex:Shop.Util.describe", "lib/shop/util.ex:Shop.Util.slugify", high},
		// A function written twice at two arities is one name in one module.
		{"lib/shop/cart.ex:Shop.Cart.checkout", "lib/shop/cart.ex:Shop.Cart.pad", model.ConfidenceMedium},
		// A nested module is named by its last segment inside its parent.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/cart.ex:Shop.Cart.Line.price", high},
		// `import` brings in a module's functions, all of them or the listed ones.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/util.ex:Shop.Util.slugify", high},
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/fmt.ex:Shop.Fmt.fmt", high},
		// `use` runs code nothing records; a function of the module is `low`.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/worker.ex:Shop.Worker.enqueue", model.ConfidenceLow},
		// Two modules in one file, each reached by its own name.
		{"lib/shop/cart.ex:Shop.Cart.total", "lib/shop/geo.ex:Shop.Geo.Zone.name", high},
		// A test aliases and calls the module under test.
		{"test/shop/cart_test.exs:Shop.CartTest", "lib/shop/cart.ex:Shop.Cart.total", high},
		{"test/shop/cart_test.exs:Shop.CartTest", "lib/shop/money.ex:Shop.Money.zero", high},
	}
	forbid := []want{
		// Billing.Money is not aliased by Shop.Cart.total.
		{Src: "lib/shop/cart.ex:Shop.Cart.total", Dst: "lib/billing/money.ex"},
		// `length`, `inspect` and `to_string` are Kernel's.
		{Src: "lib/shop/", Dst: "lib/shop/debug.ex"},
		// Shop.Fmt.pad is not in the import's only: list.
		{Src: "lib/shop/cart.ex", Dst: "lib/shop/fmt.ex:Shop.Fmt.pad"},
		// Never imported.
		{Src: "lib/shop/cart.ex", Dst: "lib/shop/secret.ex"},
	}
	// pad/1 calls pad/2: the two arities are two symbols of one name, and the
	// edge between them is written from whichever the index calls the caller.
	extraOK := []want{{"lib/shop/cart.ex:Shop.Cart.pad", "lib/shop/cart.ex:Shop.Cart.pad", model.ConfidenceMedium}}
	scoreEdges(t, got, wants, forbid, extraOK)
}

func elixirFixture() map[string]string {
	return map[string]string{
		"mix.exs": `defmodule Shop.MixProject do
  use Mix.Project

  def project do
    [app: :shop, version: "0.1.0"]
  end
end
`,
		"lib/shop/money.ex": `defmodule Shop.Money do
  def zero, do: 0
  def add(a, b), do: a + b
end
`,
		"lib/billing/money.ex": `defmodule Billing.Money do
  def zero, do: 0.0
end
`,
		"lib/billing/invoice.ex": `defmodule Billing.Invoice do
  def render(inv) do
    Logger.info("render")
    inv
  end
end
`,
		"lib/shop/tax.ex": `defmodule Shop.Tax do
  def rate(_c), do: 0.2
end
`,
		"lib/shop/order.ex": `defmodule Shop.Order do
  def place(o), do: o
end
`,
		"lib/shop/util.ex": `defmodule Shop.Util do
  def slugify(s), do: s

  def describe(x) do
    inspect(x)
    slugify(x)
  end
end
`,
		"lib/shop/fmt.ex": `defmodule Shop.Fmt do
  def fmt(x), do: to_string(x)
  def pad(x, n), do: String.pad_leading(x, n)
end
`,
		"lib/shop/worker.ex": `defmodule Shop.Worker do
  def enqueue(job), do: job
end
`,
		// Names Kernel owns, defined by a module nobody imports.
		"lib/shop/debug.ex": `defmodule Shop.Debug do
  def inspect(x, _opts), do: x
  def length(x), do: x
  def to_string(x), do: x
end
`,
		// Two modules that both define dup/1, and a caller that imports both: which
		// one `dup(x)` is, is a compile-time conflict and no index can say.
		"lib/shop/dup.ex": `defmodule Shop.DupA do
  def dup(x), do: x
end

defmodule Shop.DupB do
  def dup(x), do: x
end

defmodule Shop.DupUser do
  import Shop.DupA
  import Shop.DupB

  def run(x), do: dup(x)
end
`,
		"lib/shop/secret.ex": `defmodule Shop.Secret do
  def slugify(s), do: s
  def total(c), do: c
end
`,
		"lib/shop/geo.ex": `defmodule Shop.Geo.Region do
  def name(r), do: r
end

defmodule Shop.Geo.Zone do
  def name(z), do: z
end
`,
		"lib/shop/cart.ex": `defmodule Shop.Cart do
  alias Shop.Money
  alias Shop.{Tax, Order}
  alias Billing.Invoice, as: Inv
  alias Shop.Geo.Zone
  import Shop.Util
  import Shop.Fmt, only: [fmt: 1]
  use Shop.Worker

  defmodule Line do
    def price(l), do: l
  end

  def pad(x), do: pad(x, 2)
  def pad(x, n), do: {x, n}

  def total(c) do
    Money.zero()
    Inv.render(c)
    Tax.rate(c)
    c |> Order.place() |> helper()
    slugify(c)
    fmt(c)
    Line.price(c)
    Zone.name(c)
    enqueue(c)
    inspect(c)
    length(c)
    to_string(c)
    Enum.map(c, &fmt/1)
  end

  def checkout(c) do
    Billing.Money.zero()
    __MODULE__.total(c)
    pad(c)
  end

  defp helper(c), do: c
end
`,
		"test/shop/cart_test.exs": `defmodule Shop.CartTest do
  use ExUnit.Case
  alias Shop.{Cart, Money}

  test "total" do
    assert Cart.total(Money.zero()) == 0
  end
end
`,
	}
}
