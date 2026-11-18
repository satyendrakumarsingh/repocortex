# GDScript: known gaps

## Anonymous enums

`enum { SMALL, LARGE }` is an `enum_definition` with an `enumerator_list` and no
`name`. There is nothing to name, so it is not extracted. The enumerators of a
named enum are not extracted either.

## Kinds are by position

A script is a class, but only `class_name` and inner `class` declarations are
symbols. Functions and variables at the top of a script are recorded as
`function` and `variable`; inside an inner class they are `method` and `field`.
Signals are `field` in both places. This is a choice, not a grammar limit.
