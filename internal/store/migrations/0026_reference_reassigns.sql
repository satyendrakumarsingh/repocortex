-- 0026_reference_reassigns: a call whose result is assigned back to the
-- variable it is called on.
--
-- WHY. A local initialised from a call (`var sum = Money.zero()`) is typed by
-- what that call's definition returns (0023_return_types), and a second
-- assignment to the local withdraws that, since the value may now be anything.
-- `sum = sum.adding(l)` is the exception worth keeping: when `adding` is
-- declared to return the type `sum` already has, the assignment does not change
-- it, and withdrawing the producer made every call on `sum` lose its type.
--
-- The parser can see the shape of that assignment and not what `adding`
-- returns; the resolver knows the return type and not the shape. This column
-- carries the shape: 1 on a call on a local value that is the whole right side
-- of `x = x.f(...)`, 0 everywhere else, including every existing row (which
-- keeps its meaning: such a row was read before the distinction existed, and
-- its variable's producer was already withdrawn by the parser or never
-- reassigned). Files gain it on their next re-parse.

ALTER TABLE symbol_references ADD COLUMN reassigns INTEGER NOT NULL DEFAULT 0;
