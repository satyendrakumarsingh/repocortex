#!/usr/bin/perl
use strict;

our $Global = 1;
our ($A, $B);

sub main {
    my @args = @_;
    local $_ = 1;
    return process(@args);
}

sub process($$) {
    my ($x, $y) = @_;
    return $x + $y;
}

sub with_attr :lvalue {
    my $v;
    $v;
}

main(@ARGV);
