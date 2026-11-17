package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestClojureResolution indexes a small Leiningen project and its clojure.test
// namespace and checks every call against what `:require` makes it reach.
//
// What it is built to catch: `money/zero` where two namespaces end in `money`
// (shop.money and billing.money) and only one is required; `:as`, `:refer
// [f]`, `:refer :all`, `:use ... :only`, a prefix list, a namespace written in
// full with no alias, and a dashed name (`shop.line-items` is
// shop/line_items.clj); a bare name that a namespace not required also defines
// (`slugify`, `pad`); `map`, `first` and `println`, which are clojure.core's even
// where a namespace of the repository defines them; `str/join`, `Math/abs` and
// `.getTime`, which leave the repository; a multimethod, whose callable is the
// defmulti and not its methods; and a test that requires the namespace under test.
func TestClojureResolution(t *testing.T) {
	got := langEdges(t, clojureFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// `:as`, and a name written in full.
		{"src/shop/cart.clj:total", "src/shop/money.clj:zero", high},
		{"src/shop/cart.clj:total", "src/shop/money.clj:add", high},
		{"src/shop/cart.clj:checkout", "src/billing/money.clj:zero", high},
		{"src/shop/cart.clj:total", "src/shop/tax.clj:apply-tax", high},
		// `:refer [f]`, `:refer :all`, and `:use ... :only`.
		{"src/shop/cart.clj:total", "src/shop/tax.clj:rate", high},
		{"src/shop/cart.clj:total", "src/shop/util.clj:slugify", high},
		{"src/shop/cart.clj:total", "src/shop/fmt.clj:fmt", high},
		// A prefix list.
		{"src/shop/cart.clj:total", "src/shop/data/db.clj:query", high},
		{"src/shop/cart.clj:total", "src/shop/data/cache.clj:fetch", high},
		// A dashed namespace is an underscored path.
		{"src/shop/cart.clj:total", "src/shop/line_items.clj:line-total", high},
		// The namespace's own functions, private ones included.
		{"src/shop/cart.clj:total", "src/shop/cart.clj:helper", high},
		{"src/shop/cart.clj:checkout", "src/shop/cart.clj:total", high},
		// A namespace kept for both platforms is one per platform: the .clj file
		// loads store.clj and not store.cljs.
		{"src/shop/cart.clj:total", "src/shop/store.clj:save", high},
		// A multimethod is called by the name of its defmulti.
		{"src/shop/cart.clj:total", "src/shop/geo.clj:area", high},
		{"src/shop/geo.clj:area-circle", "src/shop/geo.clj:area", high},
		{"src/shop/tax.clj:apply-tax", "src/shop/tax.clj:rate", high},
		// A test requires the namespace under test.
		{"test/shop/cart_test.clj:(top level)", "src/shop/cart.clj:total", high},
		{"test/shop/cart_test.clj:(top level)", "src/shop/money.clj:zero", high},
	}
	forbid := []want{
		// billing.money is not the namespace that was aliased, in total.
		{Src: "src/shop/cart.clj:total", Dst: "src/billing/money.clj"},
		// shop.secret is never required, and has a slugify and a pad of its own.
		{Src: "src/shop/cart.clj", Dst: "src/shop/secret.clj"},
		// shop.fmt/pad is not in `:only [fmt]`.
		{Src: "src/shop/cart.clj", Dst: "src/shop/fmt.clj:pad"},
		// clojure.core's `first`, `map` and `println` are not the debug namespace's.
		{Src: "src/", Dst: "src/shop/debug.clj"},
		{Src: "src/shop/cart.clj", Dst: "src/shop/store.cljs"},
		// A multimethod's methods are what it dispatches to, not what a call names.
		{Src: "src/shop/cart.clj", Dst: "src/shop/geo.clj:area-circle"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func clojureFixture() map[string]string {
	return map[string]string{
		"project.clj": `(defproject shop "0.1.0"
  :dependencies [[org.clojure/clojure "1.11.1"]])
`,
		"src/shop/money.clj": `(ns shop.money)

(defn zero [] 0)

(defn add [a b] (+ a b))
`,
		"src/billing/money.clj": `(ns billing.money)

(defn zero [] 0.0)
`,
		"src/shop/tax.clj": `(ns shop.tax)

(defn rate [c] 0.2)

(defn apply-tax [c] (* c (rate c)))
`,
		"src/shop/util.clj": `(ns shop.util)

(defn slugify [s] s)

(defn pad [s n] s)
`,
		"src/shop/fmt.clj": `(ns shop.fmt)

(defn fmt [x] (str x))

(defn pad [x n] x)
`,
		"src/shop/data/db.clj": `(ns shop.data.db)

(defn query [q] q)
`,
		"src/shop/data/cache.clj": `(ns shop.data.cache)

(defn fetch [k] k)
`,
		"src/shop/line_items.clj": `(ns shop.line-items)

(defn line-total [item] (:price item))
`,
		"src/shop/store.clj": `(ns shop.store)

(defn save [x] x)
`,
		"src/shop/store.cljs": `(ns shop.store)

(defn save [x] x)
`,
		"src/shop/geo.clj": `(ns shop.geo)

(defmulti area :shape)

(defmethod area :circle [c] (* 3 (:r c)))

(defn area-circle [c] (area c))
`,
		// Never required. Same names as shop.util, and a pair that are
		// clojure.core's own.
		"src/shop/secret.clj": `(ns shop.secret)

(defn slugify [s] s)

(defn pad [s n] s)
`,
		"src/shop/debug.clj": `(ns shop.debug
  (:refer-clojure :exclude [first map println]))

(defn first [xs] xs)

(defn map [f xs] xs)

(defn println [x] x)
`,
		"src/shop/cart.clj": `(ns shop.cart
  (:require [shop.money :as money]
            [shop.tax :refer [rate]]
            [shop.util :refer :all]
            [clojure.string :as str]
            [shop.data
             [db :as db]
             [cache :refer [fetch]]]
            [shop.line-items :as li]
            [shop.geo :as geo]
            [shop.store :as store]
            shop.tax)
  (:use [shop.fmt :only [fmt]])
  (:import (java.util Date)))

(defn- helper [x] (inc x))

(defn total
  "The cart's total."
  [items]
  (let [s (money/zero)]
    (->> items (map helper) (reduce +))
    (str/join "," items)
    (first items)
    (println s)
    (money/add s 1)
    (shop.tax/apply-tax s)
    (rate s)
    (slugify s)
    (fmt s)
    (db/query s)
    (fetch s)
    (li/line-total s)
    (geo/area s)
    (store/save s)
    (.getTime (Date.))
    (Math/abs 1)
    (helper s)))

(defn checkout [c]
  (billing.money/zero)
  (total c))
`,
		"test/shop/cart_test.clj": `(ns shop.cart-test
  (:require [clojure.test :refer :all]
            [shop.cart :as cart]
            [shop.money :as money]))

(deftest total-test
  (is (= 0 (cart/total [])))
  (is (= 0 (money/zero))))
`,
	}
}
