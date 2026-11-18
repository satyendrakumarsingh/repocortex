package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestRustResolution indexes a small shop, written the way the language is
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
func TestRustResolution(t *testing.T) {
	got := langEdges(t, rustShopFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/cart.rs:Cart", "src/cart.rs:Describe", high},
		{"src/cart.rs:Cart.clone_order", "src/cart.rs:Cart.label", high},
		{"src/cart.rs:Cart.clone_order", "src/cart.rs:Cart.new", high},
		{"src/cart.rs:Cart.clone_order", "src/cart.rs:Cart.save", high},
		{"src/cart.rs:Cart.clone_order", "src/money.rs:Money.new", high},
		{"src/cart.rs:Cart.clone_order", "src/money.rs:Money.zero", high},
		{"src/cart.rs:Cart.clone_order", "src/order.rs:Order.add_line", high},
		{"src/cart.rs:Cart.clone_order", "src/order.rs:Order.checkout", high},
		{"src/cart.rs:Cart.describe", "src/util.rs:title", high},
		{"src/cart.rs:Cart.fresh_total", "src/money.rs:Money.add", high},
		{"src/cart.rs:Cart.fresh_total", "src/money.rs:Money.save", high},
		{"src/cart.rs:Cart.fresh_total", "src/money.rs:Money.zero", high},
		{"src/cart.rs:Cart.label", "src/cart.rs:Cart.describe", high},
		{"src/cart.rs:Cart.new", "src/order.rs:Order.new", high},
		// `use crate::util::{self, pad, slug};` names both: a grouped import is read.
		{"src/cart.rs:Cart.new", "src/util.rs:slug", high},
		{"src/cart.rs:Cart.save", "src/util.rs:pad", high},
		{"src/cart.rs:Cart.save", "src/util.rs:slug", high},
		{"src/cart.rs:Cart.save", "src/order.rs:Order.save", high},
		{"src/cart.rs:Cart.save", "src/order.rs:Order.validate", high},
		{"src/cart.rs:Money.describe", "src/money.rs:Money.short", high},
		{"src/money.rs:Money", "src/cart.rs:Describe", high},
		{"src/money.rs:Money.add", "src/money.rs:Money.new", high},
		{"src/money.rs:Money.from_float", "src/money.rs:Money.new", high},
		{"src/money.rs:Money.save", "src/logger.rs:Logger.write", high},
		{"src/money.rs:Money.short", "src/money.rs:Money.format", high},
		{"src/money.rs:Money.zero", "src/money.rs:Money.new", high},
		{"src/order.rs:Order", "src/order.rs:Validate", high},
		{"src/order.rs:Order.add_line", "src/order.rs:Order.validate", high},
		{"src/order.rs:Order.checkout", "src/logger.rs:Logger.new", high},
		{"src/order.rs:Order.checkout", "src/logger.rs:Logger.save", high},
		{"src/order.rs:Order.checkout", "src/money.rs:Money.save", high},
		{"src/order.rs:Order.checkout", "src/order.rs:Order.helper", high},
		{"src/order.rs:Order.checkout", "src/order.rs:Order.save", high},
		{"src/order.rs:Order.label", "src/order.rs:Order.describe", high},
		{"src/order.rs:Order.save", "src/logger.rs:Logger.write", high},
		{"src/order.rs:Order.save", "src/order.rs:Order.total", high},
		{"src/order.rs:Order.save", "src/order.rs:Order.validate", high},
		{"src/order.rs:Order.total", "src/money.rs:Money.zero", high},
		{"src/store.rs:Store.place", "src/money.rs:Money.zero", high},
		{"src/store.rs:Store.place", "src/order.rs:Order.add_line", high},
		{"src/store.rs:Store.place", "src/order.rs:Order.new", high},
		{"src/store.rs:Store.place", "src/order.rs:Order.save", high},
		{"src/store.rs:Store.place", "src/order.rs:Order.total", high},
		{"src/store.rs:Store.run", "src/money.rs:Money.from_float", high},
		{"src/store.rs:Store.run", "src/money.rs:Money.new", high},
		{"src/store.rs:Store.run", "src/money.rs:Money.save", high},
		{"src/store.rs:Store.run", "src/money.rs:Money.short", high},
		{"src/store.rs:Store.run", "src/order.rs:Order.save", high},
		{"src/store.rs:Store.run", "src/store.rs:Store.place", high},
		{"src/store.rs:Store.run", "src/store.rs:Store.save", high},
		{"src/store.rs:Store.save", "src/logger.rs:Logger.debug", high},
		{"src/util.rs:title", "src/util.rs:slug", high},
	}
	forbid := []want{
		{Src: "src/store.rs:Store.save", Dst: "Logger.save"},
		{Src: "src/store.rs:Store.save", Dst: "Money.save"},
		{Src: "src/store.rs:Store.place", Dst: "Money.save"},
		// util::fmt::format and format! are not Money's format.
		{Src: "src/cart.rs:Cart.label", Dst: "Money.format"},
	}
	// Edges the resolver may produce, each no higher than the confidence given.
	ceilings := []want{
		{"src/convert.rs:Converter.run", "src/money.rs:Money.short", model.ConfidenceMedium},
		{"src/convert.rs:Converter.run", "src/order.rs:Order.save", model.ConfidenceMedium},
		{"src/order.rs:Order.save", "src/money.rs:Money.short", model.ConfidenceLow},
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

func rustShopFixture() map[string]string {
	return map[string]string{
		"Cargo.toml": "[package]\nname = \"shop\"\nversion = \"0.1.0\"\n",
		"src/lib.rs": `pub mod money;
pub mod logger;
pub mod order;
pub mod store;
`,
		"src/money.rs": `use crate::logger::Logger;

pub struct Money {
    pub cents: i64,
}

impl Money {
    pub fn new(cents: i64) -> Money {
        Money { cents }
    }

    pub fn zero() -> Money {
        Money::new(0)
    }

    pub fn from_float(value: f64) -> Money {
        Money::new((value * 100.0) as i64)
    }

    pub fn add(&self, other: &Money) -> Money {
        Money::new(self.cents + other.cents)
    }

    pub fn format(&self, prefix: &str, digits: usize) -> String {
        format!("{}{}", prefix, self.cents)
    }

    pub fn short(&self) -> String {
        self.format("$", 2)
    }

    pub fn save(&self) {
        Logger::write("money saved");
    }
}
`,
		"src/logger.rs": `pub struct Logger {
    pub level: u8,
}

impl Logger {
    pub fn new() -> Logger {
        Logger { level: 0 }
    }

    pub fn write(message: &str) {
        println!("{}", message);
    }

    pub fn debug(message: &str, level: u8) {
        println!("{} {}", level, message);
    }

    pub fn save(&self) -> bool {
        true
    }
}
`,
		"src/order.rs": `use crate::logger::Logger;
use crate::money::Money;

pub trait Validate {
    fn validate(&self) -> bool;
}

pub struct Order {
    lines: Vec<Money>,
}

impl Validate for Order {
    fn validate(&self) -> bool {
        true
    }
}

impl Order {
    pub fn new() -> Order {
        Order { lines: Vec::new() }
    }

    pub fn add_line(&mut self, money: Money) {
        self.lines.push(money);
        self.validate();
    }

    pub fn total(&self) -> Money {
        let sum = Money::zero();
        sum
    }

    pub fn save(&self) {
        self.validate();
        Logger::write("order saved");
        self.total().short();
    }

    pub fn label(&self) -> String {
        self.describe("order")
    }

    fn describe(&self, name: &str) -> String {
        name.to_string()
    }

    pub fn checkout(&self, m: &Money) {
        m.save();
        self.save();
        let logger = Logger::new();
        logger.save();
        Order::helper(1);
        Self::helper(2);
    }

    pub fn helper(n: i32) -> i32 {
        n
    }
}
`,
		"src/convert.rs": `pub struct Meters;
pub struct Feet;

impl Meters {
    pub fn convert(&self, value: f64) -> f64 { value }
    pub fn scale(&self, value: f64, factor: f64, offset: f64) -> f64 { value * factor + offset }
}

impl Feet {
    pub fn convert(&self, value: f64, precision: u32) -> f64 { value + precision as f64 }
    pub fn scale(&self, value: f64) -> f64 { value }
}

pub struct Converter;

impl Converter {
    pub fn run<T>(unit: T, items: Vec<String>) -> usize {
        unit.convert(1.0);
        unit.convert(1.0, 2);
        unit.scale(1.0, 2.0, 3.0);
        unit.scale(1.0);
        let joined = items.join(",");
        let first = items.first();
        first.format("a", 1);
        let money = load_money(&unit);
        money.short();
        for order in items.iter() {
            order.save();
        }
        joined.len()
    }
}
`,
		"src/util.rs": `pub fn slug(text: &str) -> String {
    text.to_lowercase()
}

pub fn pad(text: &str, width: usize) -> String {
    format!("{:width$}", text, width = width)
}

pub fn title(text: &str) -> String {
    slug(text)
}

pub mod fmt {
    pub fn format(text: &str) -> String {
        text.to_string()
    }
}
`,
		"src/cart.rs": `use crate::money::Money;
use crate::order::{Order, Validate};
use crate::util::{self, pad, slug};

pub struct Cart {
    order: Order,
    tag: String,
}

pub trait Describe {
    fn describe(&self) -> String;
}

impl Describe for Cart {
    fn describe(&self) -> String {
        util::title("cart")
    }
}

impl Describe for Money {
    fn describe(&self) -> String {
        self.short()
    }
}

impl Cart {
    pub fn new() -> Cart {
        Cart { order: Order::new(), tag: slug("cart") }
    }

    pub fn save(&self) -> String {
        self.order.save();
        self.order.validate();
        pad(&slug("x"), 4)
    }

    pub fn label(&self) -> String {
        let text = self.describe();
        util::fmt::format(&text)
    }

    pub fn fresh_total(&self) -> Money {
        let m = Money { cents: 0 };
        m.save();
        m.add(&Money::zero())
    }

    pub fn clone_order(&mut self, other: &mut Order, extra: Option<Money>) {
        other.add_line(Money::zero());
        other.checkout(&Money::new(1));
        let c = Cart::new();
        c.save();
        c.label();
    }
}
`,
		"src/store.rs": `use crate::money::Money;
use crate::order::Order;
use crate::logger::Logger;

pub struct Store {
    orders: Vec<Order>,
}

impl Store {
    pub fn new() -> Store {
        Store { orders: Vec::new() }
    }

    pub fn place(&mut self) {
        let mut order = Order::new();
        order.add_line(Money::zero());
        order.save();
        order.total();
        self.orders.push(order);
    }

    pub fn save(&self) {
        Logger::debug("store", 1);
        for o in self.orders.iter() {
            o.save();
        }
    }

    pub fn run(&mut self, order: &Order, money: &Money) {
        self.place();
        self.save();
        order.save();
        money.save();
        let m = Money::from_float(2.5);
        m.save();
        let x = Money::new(5);
        x.short();
    }
}
`,
	}
}
