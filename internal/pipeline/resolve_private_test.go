package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestPrivateMemberIsNotInherited checks that a call in a subclass never binds
// to a private method of its base class (migration 0024_symbol_private,
// internal/resolve/infer/inherited.go). Private members are not inherited, so
// `m()` in `Impl extends Base implements I` reaches the interface's default
// `m`, and never the base class's own helper, which the "an implementation
// beats a declaration" rule used to prefer.
func TestPrivateMemberIsNotInherited(t *testing.T) {
	atLeastLow := model.ConfidenceLow
	t.Run("java", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"Base.java": "abstract class Base {\n    private void m() {}\n    public abstract void p();\n    static void q() {}\n}\n",
			"I.java":    "interface I {\n    default void m() {}\n    default void p() {}\n    default void q() {}\n}\n",
			"Impl.java": "class Impl extends Base implements I {\n    public void p() {}\n    void t() {\n        m();\n        p();\n        q();\n    }\n}\n",
		})
		forbidEdge(t, got, "Impl.java:Impl.t", "Base.java:Base.m")
		requireEdge(t, got, "Impl.java:Impl.t", "I.java:I.m", atLeastLow)
		requireEdge(t, got, "Impl.java:Impl.t", "Impl.java:Impl.p", atLeastLow)
		// A non-private class member still beats the interface's default.
		requireEdge(t, got, "Impl.java:Impl.t", "Base.java:Base.q", atLeastLow)
	})
	t.Run("c_sharp", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"a.cs": `interface I {
    void M();
}
class Base {
    private void M() {}
    public void K() {}
}
class Impl : Base, I {
    void I.M() {}
    void T() {
        M();
        K();
    }
}
class Impl2 : Base, I {
    public void M() {}
    void T() { M(); }
}
class U {
    void u(Impl2 x, I y) {
        x.M();
        y.M();
    }
}
`})
		// `void I.M()` is reached through an I, never by a plain `M()`; and
		// Base.M is private, so neither is bound.
		forbidEdge(t, got, "a.cs:Impl.T", "a.cs:Impl.M")
		forbidEdge(t, got, "a.cs:Impl.T", "a.cs:Impl.I.M")
		forbidEdge(t, got, "a.cs:Impl.T", "a.cs:Base.M")
		requireEdge(t, got, "a.cs:Impl.T", "a.cs:Base.K", atLeastLow)
		requireEdge(t, got, "a.cs:Impl2.T", "a.cs:Impl2.M", atLeastLow)
		requireEdge(t, got, "a.cs:U.u", "a.cs:Impl2.M", atLeastLow)
		forbidEdge(t, got, "a.cs:U.u", "a.cs:Impl.M")
		forbidEdge(t, got, "a.cs:U.u", "a.cs:Impl.I.M")
	})
}

// TestPhpTraitMethodOverridesInherited checks the order PHP looks a method up
// in: the class's own, then its traits', then the parent's. `use T` is
// recorded as an EXTENDS edge from the class, as the parent is, so the
// ancestor walk met T and B at one level and took the parent's.
func TestPhpTraitMethodOverridesInherited(t *testing.T) {
	atLeastLow := model.ConfidenceLow
	got := langEdges(t, map[string]string{
		"a.php": `<?php
interface I {
    public function m();
    public function n();
}
trait T {
    public function m() { return 1; }
}
class B {
    public function m() { return 2; }
    private function n() { return 3; }
    public function k() { return 4; }
}
class C extends B implements I {
    use T;
    public function n() { return 5; }
    public function t() {
        $this->m();
        $this->n();
        $this->k();
    }
}
function f(C $c) {
    $c->m();
    $c->n();
}
`})
	forbidEdge(t, got, "a.php:C.t", "a.php:B.m")
	forbidEdge(t, got, "a.php:C.t", "a.php:I.m")
	forbidEdge(t, got, "a.php:f", "a.php:B.m")
	forbidEdge(t, got, "a.php:f", "a.php:I.m")
	forbidEdge(t, got, "a.php:C.t", "a.php:B.n")
	requireEdge(t, got, "a.php:C.t", "a.php:T.m", atLeastLow)
	requireEdge(t, got, "a.php:C.t", "a.php:C.n", atLeastLow)
	requireEdge(t, got, "a.php:C.t", "a.php:B.k", atLeastLow)
	requireEdge(t, got, "a.php:f", "a.php:T.m", atLeastLow)
	requireEdge(t, got, "a.php:f", "a.php:C.n", atLeastLow)
}

// TestCStaticFunctionStaysInItsFile checks that a C `static` function is never
// the target of a call from another translation unit: with a.c and b.c each
// holding `static int util()`, an `extern int util(int)` and a call in e.c
// reaches neither. The exception is a function a header defines, which each
// includer has its own copy of.
func TestCStaticFunctionStaysInItsFile(t *testing.T) {
	atLeastLow := model.ConfidenceLow
	got := langEdges(t, map[string]string{
		"a.h": "void pub(void);\n",
		"a.c": "#include \"a.h\"\nstatic int util(int x){ return x; }\nvoid pub(void){ util(1); }\n",
		"b.c": "#include \"a.h\"\nstatic int util(int x){ return x+1; }\nvoid other(void){ util(2); }\n",
		"e.c": "#include \"a.h\"\nextern int util(int x);\nvoid e(void){ util(3); pub(); }\n",
		"f.c": "void f(void){ util(4); }\n",
		"h.h": "static inline int util(int x){ return x; }\n",
		"h.c": "#include \"h.h\"\nvoid hh(void){ util(6); }\n",
	})
	requireEdge(t, got, "a.c:pub", "a.c:util", atLeastLow)
	requireEdge(t, got, "b.c:other", "b.c:util", atLeastLow)
	requireEdge(t, got, "e.c:e", "a.c:pub", atLeastLow)
	requireEdge(t, got, "h.c:hh", "h.h:util", atLeastLow)
	forbidEdge(t, got, "e.c:e", "a.c:util")
	forbidEdge(t, got, "e.c:e", "b.c:util")
	forbidEdge(t, got, "f.c:f", "a.c:util")
	forbidEdge(t, got, "f.c:f", "b.c:util")
	forbidEdge(t, got, "a.c:pub", "b.c:util")
	forbidEdge(t, got, "b.c:other", "a.c:util")
	forbidEdge(t, got, "h.c:hh", "a.c:util")
	forbidEdge(t, got, "h.c:hh", "b.c:util")
}
