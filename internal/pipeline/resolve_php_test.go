package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestPHPResolution indexes a small shop, written the way the language is
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
func TestPHPResolution(t *testing.T) {
	got := langEdges(t, phpFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/Base.php:Base.audit", "src/Logger.php:Logger.write", high},
		{"src/Cart.php:Cart.__construct", "src/Util.php:Util.slug", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Cart.php:Cart", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Cart.php:Cart.label", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Cart.php:Cart.save", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Money.php:Money", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Money.php:Money.save", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Money.php:Money.zero", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Order.php:Order.addLine", high},
		{"src/Cart.php:Cart.cloneOrder", "src/Order.php:Order.checkout", high},
		{"src/Cart.php:Cart.freshTotal", "src/Money.php:Money.zero", high},
		{"src/Cart.php:Cart.label", "src/Base.php:Base.describe", high},
		{"src/Cart.php:Cart.label", "src/Util.php:Util.title", high},
		{"src/Cart.php:Cart.peek", "src/Order.php:Order", high},
		{"src/Cart.php:Cart.save", "src/Order.php:Order.save", high},
		{"src/Cart.php:Cart.save", "src/Util.php:Auditable.auditLog", high},
		{"src/Cart.php:Cart.save", "src/Util.php:Util.pad", high},
		{"src/Cart.php:Cart.save", "src/Util.php:Util.slug", high},
		{"src/Money.php:Money.add", "src/Money.php:Money", high},
		{"src/Money.php:Money.fromFloat", "src/Money.php:Money", high},
		{"src/Money.php:Money.save", "src/Logger.php:Logger.write", high},
		{"src/Money.php:Money.short", "src/Money.php:Money.format", high},
		{"src/Money.php:Money.zero", "src/Money.php:Money", high},
		{"src/Order.php:Order.addLine", "src/Base.php:Base.validate", high},
		{"src/Order.php:Order.checkout", "src/Base.php:Base.validate", high},
		{"src/Order.php:Order.checkout", "src/Logger.php:Logger", high},
		{"src/Order.php:Order.checkout", "src/Logger.php:Logger.save", high},
		{"src/Order.php:Order.checkout", "src/Money.php:Money.save", high},
		{"src/Order.php:Order.checkout", "src/Order.php:Order.helper", high},
		{"src/Order.php:Order.checkout", "src/Order.php:Order.save", high},
		{"src/Order.php:Order.label", "src/Base.php:Base.describe", high},
		{"src/Order.php:Order.save", "src/Base.php:Base.audit", high},
		{"src/Order.php:Order.save", "src/Base.php:Base.validate", high},
		{"src/Order.php:Order.save", "src/Logger.php:Logger.write", high},
		{"src/Order.php:Order.save", "src/Order.php:Order.total", high},
		{"src/Order.php:Order.total", "src/Money.php:Money.zero", high},
		{"src/Store.php:Store.place", "src/Money.php:Money.zero", high},
		{"src/Store.php:Store.place", "src/Order.php:Order", high},
		{"src/Store.php:Store.place", "src/Order.php:Order.addLine", high},
		{"src/Store.php:Store.place", "src/Order.php:Order.save", high},
		{"src/Store.php:Store.place", "src/Order.php:Order.total", high},
		{"src/Store.php:Store.run", "src/Money.php:Money", high},
		{"src/Store.php:Store.run", "src/Money.php:Money.fromFloat", high},
		{"src/Store.php:Store.run", "src/Money.php:Money.save", high},
		{"src/Store.php:Store.run", "src/Money.php:Money.short", high},
		{"src/Store.php:Store.run", "src/Order.php:Order.save", high},
		{"src/Store.php:Store.run", "src/Store.php:Store.place", high},
		{"src/Store.php:Store.run", "src/Store.php:Store.save", high},
		{"src/Store.php:Store.save", "src/Logger.php:Logger.debug", high},
		{"src/Util.php:Auditable.auditLog", "src/Logger.php:Logger.write", high},
		{"src/Util.php:Util.title", "src/Util.php:Util.slug", high},
	}
	forbid := []want{
		{Src: "src/Store.php:Store.save", Dst: "Logger.save"},
		{Src: "src/Store.php:Store.save", Dst: "Money.save"},
		{Src: "src/Store.php:Store.place", Dst: "Money.save"},
	}
	// Edges the resolver may produce, each no higher than the confidence given.
	ceilings := []want{
		{"src/Cart.php:Cart", "src/Order.php:Order", model.ConfidenceMedium},
		{"src/Cart.php:Cart", "src/Util.php:Auditable", model.ConfidenceMedium},
		{"src/Cart.php:Cart.peek", "src/Order.php:Order.total", model.ConfidenceLow},
		{"src/Convert.php:Converter.run", "src/Money.php:Money.short", model.ConfidenceMedium},
		{"src/Convert.php:Converter.run", "src/Order.php:Order.save", model.ConfidenceLow},
		{"src/Order.php:Order", "src/Base.php:Base", model.ConfidenceMedium},
		{"src/Order.php:Order.save", "src/Money.php:Money.short", model.ConfidenceLow},
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

func phpFixture() map[string]string {
	return map[string]string{
		"composer.json": `{"autoload":{"psr-4":{"Shop\\":"src/"}}}`,
		"src/Money.php": `<?php
namespace Shop;

class Money
{
    public function __construct(private int $cents)
    {
    }

    public static function zero(): Money
    {
        return new Money(0);
    }

    public static function fromFloat(float $value): Money
    {
        return new Money((int) ($value * 100));
    }

    public function add(Money $other): Money
    {
        return new Money($this->cents + $other->cents);
    }

    public function format(string $prefix, int $digits): string
    {
        return $prefix . ($this->cents / 100);
    }

    public function short(): string
    {
        return $this->format('$', 2);
    }

    public function save(): void
    {
        Logger::write('money saved');
    }
}
`,
		"src/Logger.php": `<?php
namespace Shop;

class Logger
{
    public static function write(string $message): void
    {
        echo $message;
    }

    public static function debug(string $message, int $level): void
    {
        echo $level . $message;
    }

    public function save(): bool
    {
        return true;
    }
}
`,
		"src/Base.php": `<?php
namespace Shop;

abstract class Base
{
    public function validate(): bool
    {
        return true;
    }

    public function describe(string $name): string
    {
        return $name;
    }

    public function audit(): void
    {
        Logger::write('audit');
    }
}
`,
		"src/Order.php": `<?php
namespace Shop;

class Order extends Base
{
    private array $lines = [];

    public function addLine(Money $money): void
    {
        $this->lines[] = $money;
        $this->validate();
    }

    public function total(): Money
    {
        $sum = Money::zero();
        return $sum;
    }

    public function save(): void
    {
        $this->validate();
        Logger::write('order saved');
        $this->audit();
        $this->total()->short();
    }

    public function label(): string
    {
        return $this->describe('order');
    }

    public function checkout(Money $m): void
    {
        $m->save();
        $this->save();
        $logger = new Logger();
        $logger->save();
        self::helper(1);
        parent::validate();
    }

    public static function helper(int $n): int
    {
        return $n;
    }
}
`,
		"src/Convert.php": `<?php
namespace Shop;

class Meters
{
    public function convert($value) { return $value; }
    public function scale($value, $factor, $offset) { return $value; }
}

class Feet
{
    public function convert($value, $precision) { return round($value, $precision); }
    public function scale($value) { return $value; }
}

class Converter
{
    public function run($unit, array $items)
    {
        $unit->convert(1);
        $unit->convert(1, 2);
        $unit->scale(1, 2, 3);
        $unit->scale(1);
        $text = implode(",", $items);
        $first = array_shift($items);
        $first->format('a', 1);
        $money = loadMoney($unit);
        $money->short();
        foreach ($items as $order) {
            $order->save();
        }
        foreach ($items as $money) {
            $money->short();
        }
        return strlen($text);
    }
}
`,
		"src/Util.php": `<?php
namespace Shop;

class Util
{
    public static function slug(string $text): string
    {
        return strtolower($text);
    }

    public static function pad(string $text, int $width): string
    {
        return str_pad($text, $width);
    }

    public static function title(string $text): string
    {
        return ucfirst(self::slug($text));
    }

    public static function make(): static
    {
        return new static();
    }
}

trait Auditable
{
    public function auditLog(string $message): void
    {
        Logger::write($message);
    }
}

interface Describable
{
    public function describe(string $name): string;
}
`,
		"src/Cart.php": `<?php
namespace Shop;

class Cart extends Order
{
    use Auditable;

    private string $tag;

    public function __construct()
    {
        $this->tag = Util::slug('cart');
    }

    public function save(): void
    {
        parent::save();
        $this->auditLog('cart saved');
        Util::pad(Util::slug('x'), 4);
    }

    public function label(): string
    {
        return $this->describe(Util::title('cart'));
    }

    public function freshTotal(): Money
    {
        return Money::zero()->add(Money::zero());
    }

    public function peek(): Money
    {
        return (new Order())->total();
    }

    public function cloneOrder(Order $other, ?Money $extra): void
    {
        $other->addLine(Money::zero());
        $other->checkout(new Money(1));
        $c = new Cart();
        $c->save();
        $c->label();
        $extra->save();
    }
}
`,
		"src/Store.php": `<?php
namespace Shop;

use Shop\Order;
use Shop\Money;

class Store
{
    private array $orders = [];

    public function place(): void
    {
        $order = new Order();
        $order->addLine(Money::zero());
        $order->save();
        $this->orders[] = $order;
        $order->total();
    }

    public function save(): void
    {
        Logger::debug('store', 1);
        foreach ($this->orders as $o) {
            $o->save();
        }
    }

    public function run(Order $order, ?Money $money): void
    {
        $this->place();
        $this->save();
        $order->save();
        $money->save();
        $m = Money::fromFloat(2.5);
        $m->save();
        $x = new Money(5);
        $x->short();
    }
}
`,
	}
}
