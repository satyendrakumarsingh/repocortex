# Janet known gaps

- Destructuring definitions, `(def {:a left :b right} ...)` and
  `(def [first second] ...)`, bind several names from the shape of a value.
  They are not extracted.
- `(defn name ...)` and `(def name ...)` written inside a top-level `(do ...)`,
  `(comment ...)` or other wrapper are not extracted; only forms at the top of
  the file are.
- `(def name (fn ...))` is recognised when the value starts with the symbol
  `fn`; a function built another way (`(def f (partial g 1))`) is held as a
  variable.
