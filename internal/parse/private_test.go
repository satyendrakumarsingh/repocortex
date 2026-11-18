package parse

import (
	"sort"
	"strings"
	"testing"
)

// TestPrivateDefinitions checks which definitions each language's own marker
// makes private (private.go), and that a PHP trait is flagged as one.
func TestPrivateDefinitions(t *testing.T) {
	cases := []struct {
		name, path, src string
		private         string // the symbols read as private, sorted and comma-joined
		trait           string
	}{
		{"java", "A.java", "abstract class Base {\n private void m() {}\n public abstract void p();\n void q() {}\n private static int f;\n}\n", "f,m", ""},
		{"c_sharp", "A.cs", "class Base {\n private void M() {}\n void D() {}\n public void K() {}\n protected void Q() {}\n private protected void R() {}\n}\ninterface I { void X(); }\n", "D,M", ""},
		{"kotlin", "A.kt", "abstract class Base {\n private fun m() {}\n fun k() {}\n protected fun z() {}\n}\n", "m", ""},
		{"php", "A.php", "<?php\ntrait T { public function t() {} }\nclass B {\n private function n() {}\n public function k() {}\n static private function s() {}\n}\n", "n,s", "T"},
		{"c", "A.c", "static int util(int x){ return x; }\nint pub(void){ return 1; }\nstatic int proto(int);\n", "util", ""},
		{"clojure", "A.clj", "(ns a.b)\n(defn- secret [x] x)\n(defn pub [x] x)\n(def ^:private p 1)\n", "p,secret", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parse(t, c.path, c.src)
			var private, trait []string
			for _, s := range res.Symbols {
				if s.Private {
					private = append(private, s.Name)
				}
				if s.Trait {
					trait = append(trait, s.Name)
				}
			}
			sort.Strings(private)
			sort.Strings(trait)
			if got := strings.Join(private, ","); got != c.private {
				t.Errorf("private = %q, want %q", got, c.private)
			}
			if got := strings.Join(trait, ","); got != c.trait {
				t.Errorf("trait = %q, want %q", got, c.trait)
			}
		})
	}
}
