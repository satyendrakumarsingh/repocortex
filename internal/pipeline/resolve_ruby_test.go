package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestRubyResolution indexes a small shop, written the way the language is
// written, and checks every call against the definition it reaches.
//
// It is the measurement behind the language's `typed` tier where no compiler
// index exists on the machine, so it is built to be hard on `high`: every edge
// the resolver produces at `high` is listed and was checked by reading the
// source, and anything it produces that is not listed fails the test.
//
// The decoys are calls whose receiver the source does not type: a method of a
// name four classes share called on a parameter, a variable assigned from
// another call and named after a class that declares the method, and a loop
// variable named the same way. Each may be bound, and none may be bound at
// `high`: the entries below that carry a lower confidence are ceilings, not
// expectations.
func TestRubyResolution(t *testing.T) {
	got := langEdges(t, rubyFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"lib/shop/cart.rb:Shop.Cart", "lib/shop/order.rb:Shop.Order", high},
		{"lib/shop/cart.rb:Shop.Cart", "lib/shop/util.rb:Shop.Auditable", high},
		{"lib/shop/cart.rb:Shop.Cart.clone_order", "lib/shop/cart.rb:Shop.Cart", high},
		{"lib/shop/cart.rb:Shop.Cart.clone_order", "lib/shop/money.rb:Shop.Money.zero", high},
		{"lib/shop/cart.rb:Shop.Cart.clone_order", "lib/shop/order.rb:Shop.Order.add_line", high},
		{"lib/shop/cart.rb:Shop.Cart.clone_order", "lib/shop/order.rb:Shop.Order.checkout", high},
		{"lib/shop/cart.rb:Shop.Cart.fresh_total", "lib/shop/money.rb:Shop.Money.zero", high},
		{"lib/shop/cart.rb:Shop.Cart.initialize", "lib/shop/order.rb:Shop.Order.initialize", high},
		{"lib/shop/cart.rb:Shop.Cart.initialize", "lib/shop/util.rb:Shop.Util.slug", high},
		{"lib/shop/cart.rb:Shop.Cart.label", "lib/shop/order.rb:Shop.Base.describe", high},
		{"lib/shop/cart.rb:Shop.Cart.label", "lib/shop/util.rb:Shop.Util.title", high},
		{"lib/shop/cart.rb:Shop.Cart.peek", "lib/shop/order.rb:Shop.Order", high},
		{"lib/shop/cart.rb:Shop.Cart.peek", "lib/shop/order.rb:Shop.Order.total", high},
		{"lib/shop/cart.rb:Shop.Cart.save", "lib/shop/order.rb:Shop.Order.save", high},
		{"lib/shop/cart.rb:Shop.Cart.save", "lib/shop/util.rb:Shop.Auditable.audit_log", high},
		{"lib/shop/cart.rb:Shop.Cart.save", "lib/shop/util.rb:Shop.Util.pad", high},
		{"lib/shop/cart.rb:Shop.Cart.save", "lib/shop/util.rb:Shop.Util.slug", high},
		{"lib/shop/money.rb:Shop.Money.add", "lib/shop/money.rb:Shop.Money", high},
		{"lib/shop/money.rb:Shop.Money.format_short", "lib/shop/money.rb:Shop.Money.format", high},
		{"lib/shop/money.rb:Shop.Money.save", "lib/shop/logger.rb:Shop.Logger.write", high},
		{"lib/shop/order.rb:Shop.Base.audit", "lib/shop/logger.rb:Shop.Logger.write", high},
		{"lib/shop/order.rb:Shop.Order", "lib/shop/order.rb:Shop.Base", high},
		{"lib/shop/order.rb:Shop.Order.checkout", "lib/shop/logger.rb:Shop.Logger", high},
		{"lib/shop/order.rb:Shop.Order.checkout", "lib/shop/logger.rb:Shop.Logger.save", high},
		{"lib/shop/order.rb:Shop.Order.checkout", "lib/shop/money.rb:Shop.Money.from_float", high},
		{"lib/shop/order.rb:Shop.Order.checkout", "lib/shop/order.rb:Shop.Order.save", high},
		{"lib/shop/order.rb:Shop.Order.label", "lib/shop/order.rb:Shop.Base.describe", high},
		{"lib/shop/order.rb:Shop.Order.labels", "lib/shop/order.rb:Shop.Base.describe_all", high},
		{"lib/shop/order.rb:Shop.Order.save", "lib/shop/logger.rb:Shop.Logger.write", high},
		{"lib/shop/order.rb:Shop.Order.total", "lib/shop/money.rb:Shop.Money.zero", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/logger.rb:Shop.Logger", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/logger.rb:Shop.Logger.save", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/money.rb:Shop.Money.from_float", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/money.rb:Shop.Money.zero", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/order.rb:Shop.Order", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/order.rb:Shop.Order.save", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/report.rb:Shop.Report", high},
		{"lib/shop/report.rb:Shop.Report.build", "lib/shop/report.rb:Shop.Report.helper", high},
		{"lib/shop/store.rb:Shop.Store.place", "lib/shop/money.rb:Shop.Money.zero", high},
		{"lib/shop/store.rb:Shop.Store.place", "lib/shop/order.rb:Shop.Order", high},
		{"lib/shop/store.rb:Shop.Store.place", "lib/shop/order.rb:Shop.Order.add_line", high},
		{"lib/shop/store.rb:Shop.Store.place", "lib/shop/order.rb:Shop.Order.save", high},
		{"lib/shop/store.rb:Shop.Store.place", "lib/shop/order.rb:Shop.Order.total", high},
		{"lib/shop/store.rb:Shop.Store.save", "lib/shop/logger.rb:Shop.Logger.debug", high},
		{"lib/shop/util.rb:Shop.Auditable.audit_log", "lib/shop/logger.rb:Shop.Logger.write", high},
		{"lib/shop/util.rb:Shop.Util.title", "lib/shop/util.rb:Shop.Util.slug", high},
	}
	forbid := []want{
		// A bare name is not the method of another module that shares it.
		{Src: "lib/shop/store.rb:Shop.Store.save", Dst: "Shop.Logger.save"},
		{Src: "lib/shop/store.rb:Shop.Store.place", Dst: "Shop.Money.save"},
		// Four classes declare convert and scale, and the receiver is a parameter.
		{Src: "lib/shop/convert.rb", Dst: ""},
	}
	// Edges the resolver may produce, each no higher than the confidence given.
	ceilings := []want{
		{"lib/shop/order.rb:Shop.Order.checkout", "lib/shop/money.rb:Shop.Money.save", model.ConfidenceMedium},
		{"lib/shop/order.rb:Shop.Order.save", "lib/shop/money.rb:Shop.Money.format_short", model.ConfidenceLow},
		{"lib/shop/report.rb:Shop.Report.other", "lib/shop/money.rb:Shop.Money.format", model.ConfidenceMedium},
		{"lib/shop/report.rb:Shop.Report.other", "lib/shop/order.rb:Shop.Order.save", model.ConfidenceLow},
	}
	scoreEdges(t, got, wants, forbid, ceilings)
	capped(t, got, ceilings)

	// Thirty edges at `high` across the forms the language has is the floor
	// the promotion was measured against; no edge at `high` may be wrong, which
	// scoreEdges has already established.
	n := 0
	for _, e := range got {
		if e.Confidence == high {
			n++
		}
	}
	if n < 30 {
		t.Errorf("%d edges at high, want at least 30", n)
	}
	t.Logf("%d edges at high", n)
}

func rubyFixture() map[string]string {
	return map[string]string{
		"lib/shop.rb": `require_relative "shop/money"
require_relative "shop/order"
require_relative "shop/store"
require_relative "shop/logger"

module Shop
  VERSION = "1.0"
end
`,
		"lib/shop/money.rb": `module Shop
  class Money
    attr_reader :cents

    def initialize(cents)
      @cents = cents
    end

    def self.zero
      new(0)
    end

    def self.from_float(value)
      new((value * 100).to_i)
    end

    def add(other)
      Money.new(cents + other.cents)
    end

    def format(prefix, digits)
      "#{prefix}#{cents / 100.0}"
    end

    def format_short
      format("$", 2)
    end

    def save
      Logger.write("money saved")
    end
  end
end
`,
		"lib/shop/logger.rb": `module Shop
  class Logger
    def self.write(message)
      puts message
    end

    def self.debug(message, level)
      puts "#{level}: #{message}"
    end

    def save
      true
    end
  end
end
`,
		"lib/shop/order.rb": `require_relative "money"
require_relative "logger"

module Shop
  class Base
    def validate
      true
    end

    def describe(name)
      name.to_s
    end

    def describe_all(names, sep)
      names.join(sep)
    end

    def audit
      Logger.write("audit")
    end
  end

  class Order < Base
    def initialize
      @lines = []
    end

    def add_line(money)
      @lines << money
      validate
    end

    def total
      sum = Money.zero
      @lines.each { |m| sum = sum.add(m) }
      sum
    end

    def save
      validate
      Logger.write("order saved")
      audit
      total.format_short
    end

    def label
      describe("order")
    end

    def labels
      describe_all(["a"], ",")
    end

    def checkout
      money = Money.from_float(2.5)
      money.save
      self.save
      logger = Logger.new
      logger.save
    end
  end
end
`,
		"lib/shop/report.rb": `require_relative "order"
require_relative "logger"
require_relative "money"

module Shop
  class Report
    def build
      o = Order.new
      o.save
      m = Money.zero
      m.save
      l = Shop::Logger.new
      l.save
      Shop::Money.from_float(1.0).save
      x = Report.new
      x.helper(1)
    end

    def helper(n)
      n
    end

    def other(items)
      items.each do |i|
        i.save
      end
      thing = items.first
      thing.save
      order = items.first
      order.save
      money = other_money(items)
      money.format("a", 2)
    end
  end
end
`,
		"lib/shop/convert.rb": `module Shop
  class Meters
    def convert(value)
      value
    end

    def scale(value, factor, offset)
      value * factor + offset
    end
  end

  class Feet
    def convert(value, precision)
      value.round(precision)
    end

    def scale(value)
      value
    end
  end

  class Converter
    def run(unit, items)
      unit.convert(1)
      unit.convert(1, 2)
      unit.scale(1, 2, 3)
      unit.scale(1)
      items.map { |i| i.to_s }
      puts items.join(",")
      name = items.first
      name.format("a", 1)
    end
  end
end
`,
		"lib/shop/util.rb": `module Shop
  module Util
    def self.slug(text)
      text.downcase
    end

    def self.pad(text, width)
      text.ljust(width)
    end

    def self.title(text)
      slug(text).capitalize
    end
  end

  module Auditable
    def audit_log(message)
      Logger.write(message)
    end
  end
end
`,
		"lib/shop/cart.rb": `require_relative "util"
require_relative "order"
require_relative "money"

module Shop
  class Cart < Order
    include Auditable

    def initialize
      super()
      @tag = Util.slug("cart")
    end

    def save
      super
      self.audit_log("cart saved")
      Util.pad(Util.slug("x"), 4)
    end

    def label
      self.describe(Util.title("cart"))
    end

    def fresh_total
      Money.zero.add(Money.zero)
    end

    def peek
      Order.new.total
    end

    def clone_order
      other = Cart.new
      other.add_line(Money.zero)
      other.checkout
    end
  end
end
`,
		"lib/shop/store.rb": `require_relative "order"
require_relative "money"

module Shop
  class Store
    def initialize
      @orders = []
    end

    def place
      order = Order.new
      order.add_line(Money.zero)
      order.save
      @orders << order
      order.total
    end

    def save
      Logger.debug("store", 1)
      @orders.each(&:save)
    end

    def run
      place
      save
    end
  end
end
`,
	}
}
