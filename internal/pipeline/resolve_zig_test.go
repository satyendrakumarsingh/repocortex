package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestZigResolution indexes a small Zig package (a library of files, an
// executable and a test file) and checks every call against what `@import`
// makes it reach.
//
// What it is built to catch: `money.zero()` where other.zig also defines `zero`
// and `add` and a `Money` of its own, and is not the file imported; `const add =
// @import("money.zig").add;` (a member imported under its own name) and `const
// Money = @import("money.zig").Money;` (a type, whose `init` is a member of it);
// `@import("lib/util.zig")` and `@import("../src/cart.zig")` (paths relative to
// the importing file); `std.debug.print` and `std.testing.expect`, which leave
// the repository, while util.zig has a `print` of its own that a script
// qualifies by name (`util.print`) and a local `print` that a bare call reaches;
// `@import("vendor")`, a package the build names, whose calls bind to nothing;
// `@as`, which is a builtin and not a call; and `test "..." {}` blocks, which are
// functions of their file.
//
// What it does not bind, by design: a package import (`@import("vendor")`: the
// build's module table is not read) and a method on a value whose type the source
// does not write, which is bound at low where one member of that name exists.
func TestZigResolution(t *testing.T) {
	got := langEdges(t, zigFixture())
	high, low := model.ConfidenceHigh, model.ConfidenceLow
	wants := []want{
		// The file's own functions, called from a function or from a test block.
		{"src/cart.zig:checkout", "src/cart.zig:total", high},
		{"src/cart.zig:test \"total works\"", "src/cart.zig:total", high},
		{"src/cart.zig:total", "src/cart.zig:helper", high},
		{"src/main.zig:main", "src/main.zig:print", high},
		{"src/money.zig:test \"add\"", "src/money.zig:add", high},
		{"src/tax.zig:apply", "src/tax.zig:rate", high},
		// A name imported by member: `const add = @import("money.zig").add;`.
		{"src/cart.zig:total", "src/money.zig:add", high},
		// `m.f()` where m is the file imported, and `T.init()` where T is a type imported by member.
		{"src/cart.zig:checkout", "src/money.zig:add", high},
		{"src/cart.zig:total", "src/lib/util.zig:slug", high},
		{"src/cart.zig:total", "src/money.zig:Money.init", high},
		{"src/cart.zig:total", "src/money.zig:zero", high},
		{"src/cart.zig:total", "src/tax.zig:rate", high},
		{"src/main.zig:main", "src/cart.zig:checkout", high},
		{"src/main.zig:main", "src/cart.zig:total", high},
		{"src/main.zig:main", "src/lib/util.zig:print", high},
		{"test/cart_test.zig:test \"cart\"", "src/cart.zig:total", high},
		{"test/cart_test.zig:test \"cart\"", "src/money.zig:zero", high},
		// A method called on a value whose type the source does not state: the only member of the name, so low.
		{"src/cart.zig:total", "src/money.zig:Money.fmt", low},
	}
	forbid := []want{
		// other.zig is not the file imported.
		{Src: "src/cart.zig:total", Dst: "src/other.zig:zero"},
		{Src: "src/cart.zig:total", Dst: "src/other.zig:add"},
		{Src: "src/cart.zig:total", Dst: "src/other.zig:Money.init"},
		{Src: "test/cart_test.zig", Dst: "src/other.zig"},
		// `vendor.append` is a package the build names and `list.append` a standard
		// library type's: neither is util's free function of that name.
		{Src: "src/main.zig:main", Dst: "src/lib/util.zig:append"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func zigFixture() map[string]string {
	return map[string]string{
		"build.zig": `const std = @import("std");
pub fn build(b: *std.Build) void {
    _ = b;
}
`,
		"src/cart.zig": `const std = @import("std");
const money = @import("money.zig");
const util = @import("lib/util.zig");
const Money = @import("money.zig").Money;
const add = @import("money.zig").add;
const tax = @import("tax.zig");

pub fn total(items: []const u8) i64 {
    const z = money.zero();
    var t = add(z, 1);
    t += tax.rate(t);
    _ = util.slug(items);
    std.debug.print("x", .{});
    const m = Money.init(5);
    _ = m.fmt();
    return helper(t) + @as(i64, 1);
}

pub fn checkout(items: []const u8) i64 {
    return total(items) + money.add(1, 2);
}

fn helper(x: i64) i64 {
    return x;
}

test "total works" {
    try std.testing.expect(total("") == 0);
}
`,
		"src/lib/util.zig": `pub fn slug(s: []const u8) []const u8 {
    return s;
}

pub fn pad(n: usize) usize {
    return n;
}

pub fn print(x: i64) void {
    _ = x;
}

pub fn append(list: anytype, x: i64) void {
    _ = list;
    _ = x;
}
`,
		"src/main.zig": `const std = @import("std");
const cart = @import("cart.zig");
const util = @import("lib/util.zig");
const vendor = @import("vendor");

pub fn main() !void {
    const t = cart.total("x");
    std.debug.print("{}\n", .{cart.checkout("y")});
    util.print(t);
    print(t);
    vendor.append(t);
    var list = std.ArrayList(i64).init(std.heap.page_allocator);
    list.append(t) catch {};
}

fn print(x: i64) void {
    _ = x;
}
`,
		"src/money.zig": `const std = @import("std");

pub const Money = struct {
    cents: i64,

    pub fn init(c: i64) Money {
        return .{ .cents = c };
    }

    pub fn fmt(self: Money) i64 {
        return self.cents;
    }
};

pub fn zero() i64 {
    return 0;
}

pub fn add(a: i64, b: i64) i64 {
    return a + b;
}

test "add" {
    try std.testing.expect(add(1, 2) == 3);
}
`,
		// The same names as money.zig, in a file nobody imports.
		"src/other.zig": `pub fn zero() f64 {
    return 0.0;
}

pub fn add(a: f64, b: f64) f64 {
    return a + b;
}

pub const Money = struct {
    pub fn init(c: f64) Money {
        return .{};
    }
};
`,
		"src/tax.zig": `pub fn rate(c: i64) i64 {
    return @divTrunc(c, 5);
}

pub fn apply(c: i64) i64 {
    return c + rate(c);
}
`,
		"test/cart_test.zig": `const std = @import("std");
const cart = @import("../src/cart.zig");
const Money = @import("../src/money.zig");

test "cart" {
    try std.testing.expect(cart.total("") == 0);
    _ = Money.zero();
}
`,
	}
}
