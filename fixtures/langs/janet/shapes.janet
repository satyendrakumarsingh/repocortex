# Shape helpers.
(import spork/json)
(use ./helpers)

(def pi-approx 3.14159)

(def unit-name "unit")

(def- private-value 7)

(var counter 0)

(var- hidden-counter 1)

(defn area
  "Area of a circle."
  [r]
  (def squared (* r r))
  (* pi-approx squared))

(defn perimeter [r] (* 2 pi-approx r))

(defn- private-helper [x]
  (+ x 1))

(defn variadic [first & rest]
  [first ;rest])

(def square (fn [x] (* x x)))

(def named-fn (fn named-inner [x] x))

(def- hidden-square (fn [x] (* x x)))

(defmacro swap! [a b]
  ~(let [tmp ,a]
     (set ,a ,b)
     (set ,b tmp)))

(defmacro- private-macro [x]
  ~(+ ,x 1))

(def-macro-like-name 1)

(defglobal global-val 4)

(varglobal global-var 5)

(defdyn *shape-scale* "Scale of shapes.")

(defn outer []
  (defn inner [] 1)
  (def local-value 2)
  (var local-var 3)
  (let [f (fn [x] x)]
    (inner)))

(let [hidden 1]
  (defn in-let [] hidden))

(when true
  (def in-when 3))

(each x [1 2 3]
  (print x))

(print (area 2))

(def {:a left :b right} {:a 1 :b 2})

(def [first-item second-item] [1 2])

(defn with-docs
  ``Docs in a long string.``
  [x]
  x)

