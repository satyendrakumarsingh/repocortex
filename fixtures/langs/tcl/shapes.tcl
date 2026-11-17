#!/usr/bin/env tclsh
# Shape helpers.
package require Tcl 8.6
package provide shapes 1.0

set pi_approx 3.14159
set unit_name "unit"
variable top_var 1

proc area {r} {
    set squared [expr {$r * $r}]
    return [expr {$::pi_approx * $squared}]
}

proc perimeter {r} { expr {2 * $::pi_approx * $r} }

proc with_defaults {a {b 2} args} {
    return [list $a $b $args]
}

proc ::shapes::qualified {x} {
    return $x
}

proc outer {} {
    proc inner_defined {} { return 1 }
    set local_value 2
    variable local_var 3
    return [inner_defined]
}

namespace eval shapes {
    variable count 0
    variable names {}
    set default_name "circle"

    proc make {name} {
        variable count
        incr count
        return $name
    }

    proc reset {} {
        variable count
        set count 0
    }

    namespace eval nested {
        variable depth 1
        proc descend {} { return 1 }
    }

    namespace export make reset
}

namespace eval ::absolute::path {
    proc helper {} { return 1 }
}

oo::class create Shape {
    variable name sides

    constructor {n} {
        set name $n
    }

    destructor {
        puts bye
    }

    method describe {} {
        return "shape $name"
    }

    method area {} {
        return 0
    }

    method {set name} {n} {
        set name $n
    }

    unexport describe

    class method create_default {} {
        return [my new default]
    }
}

oo::class create Circle {
    superclass Shape
    variable radius

    method area {} {
        return [expr {3.14 * $radius * $radius}]
    }
}

oo::define Shape {
    method extra {} { return 1 }
}

oo::object create singleton
oo::objdefine singleton {
    method only_here {} { return 1 }
}

if {$argc > 0} {
    proc in_if {} { return 1 }
}

set handler [list apply {{x} { return $x }}]

puts [area 2]
