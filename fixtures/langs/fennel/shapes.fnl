;; Shape helpers.
(local fennel (require :fennel))
(local {: view} (require :fennel))

(local pi-approx 3.14159)

(local unit-name "unit")

(local M {})

(var counter 0)

(global shapes-global 7)

(fn area [r]
  "Area of a circle."
  (let [squared (* r r)]
    (* pi-approx squared)))

(fn perimeter [r] (* 2 pi-approx r))

(fn variadic [first ...]
  (values first ...))

(local square (fn [x] (* x x)))

(local named-lambda (lambda [x] x))

(local cube (λ [x] (* x x x)))

(lambda checked [n]
  (+ n 1))

(λ greek [n] n)

(fn M.method [self x]
  (+ x 1))

(fn M.nested.deep [x] x)

(set M.assigned (fn [x] x))

(set M.value 3)

(macro swap! [a b]
  `(let [tmp# ,a]
     (set ,a ,b)
     (set ,b tmp#)))

(macros {:twice (fn [x] `(* 2 ,x))
         :thrice (fn [x] `(* 3 ,x))})

(import-macros {: unless} :macros)

(fn outer []
  (fn inner [] 1)
  (local local-value 2)
  (var local-var 3)
  (let [f (fn [x] x)]
    (inner)))

(let [hidden 1]
  (fn in-let [] hidden))

(when true
  (fn in-when [] 1))

(each [k v (pairs M)]
  (print k v))

(local {: left : right} (values 1 2))

(local [first-item second-item] [1 2])

(print (area 2))

M
