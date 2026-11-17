package Shape;
use strict;
use warnings;
use constant PI => 3.14159;
use constant {
    SIDES  => 4,
    COLOUR => 'red',
    EDGES  => [1, 2, 3, 4],
    NAME   => 'sq',
};
use constant DEBUG => 0;

our $VERSION = '1.0';
our @ISA = ('Base');
my $private = 5;

sub new {
    my ($class, %args) = @_;
    my $self = bless { %args }, $class;
    return $self;
}

sub area {
    my $self = shift;
    my $helper = sub { return $_[0] * 2 };
    return $helper->($self->{w});
}

sub _internal { return 1 }

package Shape::Circle {
    our $COUNT = 0;
    use constant RADIUS => 1;

    sub new {
        my $class = shift;
        return bless {}, $class;
    }

    sub circumference { return 2 * PI * RADIUS }
}

package Shape::Square;

sub area { my $self = shift; return $self->{side} ** 2 }

sub describe {
    my ($self) = @_;
    print "square\n";
    $self->area();
    Shape::helper(1);
    return;
}

1;
