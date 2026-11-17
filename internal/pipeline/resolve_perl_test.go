package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestPerlResolution indexes a small Perl distribution (modules under lib/, a
// script and a Test::More file) and checks every call against what `package`,
// `use` and `use parent` make it reach.
//
// What it is built to catch: `Shop::Money::zero()` and `Shop::Money->new` where
// Shop::Other also has a `zero` and a `new`; `use Shop::Util qw(slug pad)` (the
// listed names) against `use Shop::Other ()` (none); `push` and `join`, which are
// the interpreter's while Shop::Debug and Shop::Util define subs of those names;
// `$self->checkout`, `__PACKAGE__->helper` and `$class->SUPER::new`, which are
// on the package the call is written in or the one it inherits from; a second
// package in the same file with a `helper` of its own, which a bare call in
// that package reaches and the first package's does not; a method called on a
// variable (`$cart->total`), whose class no source states; `&helper(1)`;
// `print`, `bless` and `sum`; and a test that `use`s the module under test.
//
// What it does not bind, by design: a method on an object held in a variable
// (`$m->fmt`, `$cart->total`), whose class only the program's execution knows,
// and a sub that the module exports through a `use` with no list, which is
// read as every sub of the module.
func TestPerlResolution(t *testing.T) {
	got := langEdges(t, perlFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A sub of the package the call is written in, in a file that holds two packages.
		{"bin/run.pl:(top level)", "bin/run.pl:main", high},
		{"bin/run.pl:main", "bin/run.pl:helper", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Cart.pm:Shop::Cart.helper", high},
		{"lib/Shop/Cart.pm:Shop::Cart::Item.label", "lib/Shop/Cart.pm:Shop::Cart::Item.helper", high},
		{"lib/Shop/Cart.pm:Shop::Cart::Item.label", "lib/Shop/Cart.pm:Shop::Cart::Item.name", high},
		// A name `use Module qw(...)` brings in.
		{"bin/run.pl:main", "lib/Shop/Util.pm:Shop::Util.slug", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Util.pm:Shop::Util.pad", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Util.pm:Shop::Util.slug", high},
		{"t/cart.t:(top level)", "lib/Shop/Money.pm:Shop::Money.add", high},
		// `Package::f()`, `Package->f()` and `$self->f()` / `__PACKAGE__->f()` name the package.
		{"bin/run.pl:main", "lib/Shop/Cart.pm:Shop::Cart.checkout", high},
		{"bin/run.pl:main", "lib/Shop/Cart.pm:Shop::Cart.new", high},
		{"lib/Shop/Cart.pm:Shop::Cart.checkout", "lib/Shop/Cart.pm:Shop::Cart.helper", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Cart.pm:Shop::Cart.checkout", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Money.pm:Shop::Money.new", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Money.pm:Shop::Money.zero", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Other.pm:Shop::Other.add", high},
		{"t/cart.t:(top level)", "lib/Shop/Cart.pm:Shop::Cart.new", high},
		// A sub the package inherits through `use parent`, called on `$self` or as `SUPER::`.
		{"lib/Shop/Cart.pm:Shop::Cart.new", "lib/Shop/Base.pm:Shop::Base.new", high},
		{"lib/Shop/Cart.pm:Shop::Cart.total", "lib/Shop/Base.pm:Shop::Base.describe", high},
	}
	forbid := []want{
		// Shop::Other is named only for its add, and has no zero or new that is called.
		{Src: "lib/Shop/Cart.pm:Shop::Cart.total", Dst: "lib/Shop/Other.pm:Shop::Other.zero"},
		{Src: "lib/Shop/Cart.pm:Shop::Cart.total", Dst: "lib/Shop/Other.pm:Shop::Other.new"},
		// push and join are the interpreter's.
		{Src: "lib/Shop/Cart.pm:Shop::Cart.total", Dst: "lib/Shop/Debug.pm:Shop::Debug.push"},
		{Src: "lib/Shop/Cart.pm:Shop::Cart.total", Dst: "lib/Shop/Util.pm:Shop::Util.join"},
		// The other package of the file has its own helper.
		{Src: "lib/Shop/Cart.pm:Shop::Cart.total", Dst: "lib/Shop/Cart.pm:Shop::Cart::Item.helper"},
		{Src: "lib/Shop/Cart.pm:Shop::Cart::Item.label", Dst: "lib/Shop/Cart.pm:Shop::Cart.helper"},
		// The script's own helper is not the package's.
		{Src: "bin/run.pl:main", Dst: "lib/Shop/Cart.pm:Shop::Cart.helper"},
		// A script that uses Shop::Debug () imports nothing from it.
		{Src: "bin/run.pl", Dst: "lib/Shop/Debug.pm"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func perlFixture() map[string]string {
	return map[string]string{
		"bin/run.pl": `#!/usr/bin/perl
use strict;
use Shop::Cart;
use Shop::Util qw(slug);
use Shop::Debug ();

sub main {
    my $cart = Shop::Cart->new(items => []);
    my $t = $cart->total([1, 2]);
    print slug("x"), "\n";
    Shop::Cart::checkout($cart, $t);
    helper();
    return 0;
}

sub helper { return 1; }

main();
`,
		"lib/Shop/Base.pm": `package Shop::Base;
use strict;

sub new { my ($class, %args) = @_; return bless {%args}, $class; }
sub describe { my $self = shift; return 'base'; }
sub tag { return 't'; }

1;
`,
		"lib/Shop/Cart.pm": `package Shop::Cart;
use strict;
use warnings;
use parent -norequire, 'Shop::Base';
use Shop::Money;
use Shop::Util qw(slug pad);
use Shop::Other ();
use List::Util qw(sum);
require Shop::Extra;

sub new {
    my ($class, %args) = @_;
    my $self = $class->SUPER::new(%args);
    return $self;
}

sub total {
    my ($self, $items) = @_;
    my $z = Shop::Money::zero();
    my $m = Shop::Money->new(5);
    push @$items, slug($items);
    print "x";
    $self->checkout($z);
    $self->describe();
    $m->fmt();
    return sum(map { pad(3, $_) } @$items) + helper($z) + Shop::Other::add(1, 2);
}

sub checkout {
    my ($self, $c) = @_;
    return __PACKAGE__->helper($c);
}

sub helper { return $_[0]; }

package Shop::Cart::Item;

sub name { return 'item'; }
sub label { my $self = shift; return name() . helper(); }
sub helper { return 'h'; }

1;
`,
		// Subs named for functions the interpreter provides, in a package nobody imports from.
		"lib/Shop/Debug.pm": `package Shop::Debug;
use strict;
use Exporter 'import';
our @EXPORT = qw(trace push);

sub trace { return shift; }
sub push { return 0; }

1;
`,
		"lib/Shop/Money.pm": `package Shop::Money;
use strict;
use warnings;
use Exporter 'import';
our @EXPORT_OK = qw(zero add);

sub new { my ($class, $n) = @_; return bless { n => $n }, $class; }
sub zero { return 0; }
sub add { my ($a, $b) = @_; return $a + $b; }
sub fmt { my $self = shift; return "$self->{n}"; }

1;
`,
		// The same names as Money, in a package the cart reaches only by Shop::Other::add.
		"lib/Shop/Other.pm": `package Shop::Other;
use strict;
use Exporter 'import';
our @EXPORT_OK = qw(zero add);

sub new { return bless {}, shift; }
sub zero { return 0.0; }
sub add { return $_[0] + $_[1]; }

1;
`,
		"lib/Shop/Util.pm": `package Shop::Util;
use strict;
use Exporter 'import';
our @EXPORT_OK = qw(slug pad);

sub slug { return lc shift; }
sub pad { my ($n, $s) = @_; return $s; }
sub join { return 1; }
sub format { return 'x'; }

1;
`,
		"t/cart.t": `use strict;
use Test::More tests => 2;
use Shop::Cart;
use Shop::Money qw(add);

my $c = Shop::Cart->new;
is($c->total([1]), 1, 'total');
is(add(1, 2), 3, 'add');
`,
	}
}
