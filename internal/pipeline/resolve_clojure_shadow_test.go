package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestClojureLocalsShadowReferredNames checks that a parameter or a `let`
// binding named like a referred function is not a call of it, and that a
// private `defn-` is not reachable through `:refer`.
func TestClojureLocalsShadowReferredNames(t *testing.T) {
	high := model.ConfidenceHigh
	got := langEdges(t, map[string]string{
		"src/app/core.clj": "(ns app.core)\n(defn- secret [x] x)\n(defn shared [x] x)\n(defn pub [x] x)\n",
		"src/app/util.clj": "(ns app.util)\n(defn shared [x] (* 2 x))\n",
		"src/app/m2.clj": `(ns app.m2
  (:require [app.core :refer [pub]]))
(defn pub2 [pub] (pub 3))
(defn pub3 ([a] (pub a)) ([pub b] (pub b)))
(defn pub4 [{:keys [pub]}] (pub 1))
(defn run2 [] (pub 1))
(defn run3 [] (let [x 1] (pub x)))
`,
		"src/app/main.clj": `(ns app.main
  (:require [app.core :refer :all]
            [app.util :refer :all]))
(defn run []
  (secret 1)
  (let [pub (fn [x] x)]
    (pub 3)))
(defn run4 [] (pub 4))
`,
	})
	// Parameters and let bindings shadow.
	forbidEdge(t, got, "m2.clj:pub2", "core.clj:pub")
	forbidEdge(t, got, "m2.clj:pub4", "core.clj:pub")
	forbidEdge(t, got, "main.clj:run", "core.clj:pub")
	// A multi-arity function: the arity that binds the name shadows it, the
	// other arity still calls the referred one (pub3 has that one edge).
	// Not shadowed.
	requireEdge(t, got, "m2.clj:run2", "core.clj:pub", high)
	requireEdge(t, got, "m2.clj:run3", "core.clj:pub", high)
	requireEdge(t, got, "main.clj:run4", "core.clj:pub", high)
	requireEdge(t, got, "m2.clj:pub3", "core.clj:pub", high)
	// A private defn- is not referred, by name or by :refer :all.
	forbidEdge(t, got, "main.clj:run", "core.clj:secret")
}
