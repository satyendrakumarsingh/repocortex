package parse

import "testing"

// masked applies a finder and returns the text it leaves, which must be as long
// as the input and have its newlines where they were.
func masked(t *testing.T, find func([]byte) []maskSpan, src string) string {
	t.Helper()
	out := string(applyMaskSpans([]byte(src), find([]byte(src))))
	if len(out) != len(src) {
		t.Fatalf("length changed: %d to %d", len(src), len(out))
	}
	for i := range src {
		if (src[i] == '\n') != (out[i] == '\n') {
			t.Fatalf("newline moved at %d", i)
		}
	}
	return out
}

func TestSwiftTypeAttributeMask(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"escaping sendable", "func f(_ h: @escaping @Sendable (Int) -> Void) {}", "func f(_ h: @escaping           (Int) -> Void) {}"},
		{"optional function type", "var h: (@Sendable (Int) -> Void)?", "var h: (          (Int) -> Void)?"},
		{"main actor", "typealias H = @MainActor (Int) -> Void", "typealias H =            (Int) -> Void"},
		{"a declaration attribute stays", "@Sendable func f() {}", "@Sendable func f() {}"},
		{"an attribute with arguments stays", "@MainActor(unsafe) var x", "@MainActor(unsafe) var x"},
		{"in a comment", "// @Sendable (Int)\nlet a = 1", "// @Sendable (Int)\nlet a = 1"},
		{"in a string", `let s = "@Sendable (x)"`, `let s = "@Sendable (x)"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := masked(t, findSwiftTypeAttributes, c.src); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestMakeEmptySubstitutionMask(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"suffix strip", "BOARD = $(notdir $(BOARD_DIR:/=))\n", "BOARD = $(notdir $(BOARD_DIR   ))\n"},
		{"braces", "X = ${OBJ:.o=}\n", "X = ${OBJ    }\n"},
		{"nested pattern", "X = $(A:$(B)=)\n", "X = $(A      )\n"},
		{"a replacement is kept", "X = $(OBJ:.c=.o)\n", "X = $(OBJ:.c=.o)\n"},
		{"a function call is kept", "X = $(subst a,b=,c)\n", "X = $(subst a,b=,c)\n"},
		{"in a comment", "# $(A:.c=)\nX = 1\n", "# $(A:.c=)\nX = 1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := masked(t, findMakeEmptySubstitutions, c.src); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestGroovySignatureMask(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"two typed parameters", "class A {\n    private static run(File f, Helper h) {\n    }\n}\n", "class A {\n    private static run(     f          ) {\n    }\n}\n"},
		{"constructor", "class A {\n    private A(String n, int s) { this(n) }\n}\n", "class A {\n    private A(       n       ) { this(n) }\n}\n"},
		{"generic type with a comma", "class A {\n    protected go(Map<String, Integer> m, int k) {}\n}\n", "class A {\n    protected go(                     m       ) {}\n}\n"},
		{"a bare name is left", "class A {\n    private go(a) {}\n}\n", "class A {\n    private go(a) {}\n}\n"},
		{"a return type is left", "class A {\n    private void go(int a, int b) {}\n}\n", "class A {\n    private void go(int a, int b) {}\n}\n"},
		{"a call is left", "class A {\n    void f() {\n        go(a, b) {\n        }\n    }\n}\n", "class A {\n    void f() {\n        go(a, b) {\n        }\n    }\n}\n"},
		{"in a comment", "/*\n private go(int a, int b) {}\n*/\n", "/*\n private go(int a, int b) {}\n*/\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := masked(t, findGroovySignatures, c.src); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestHaxeMask(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"short typedef", "typedef A = { name : String, ?t : Int };\n", "typedef A = {                         };\n"},
		{"block typedef stays", "typedef A = { var x:Int; var y:Int }\n", "typedef A = { var x:Int; var y:Int }\n"},
		{"extension typedef", "typedef P = {> Base, var z : Int }\n", "typedef P = {                    }\n"},
		{"metadata before an interface", "@:autoBuild(a.b())\nextern interface I {}\n", "                  \nextern interface I {}\n"},
		{"metadata before a function stays", "@:keep\nfunction f() {}\n", "@:keep\nfunction f() {}\n"},
		{"constraint", "class Box<T:Dynamic, K> {}\n", "class Box<T        , K> {}\n"},
		{"nested constraint", "class Box<T:(A, B)> {}\n", "class Box<T       > {}\n"},
		{"empty default", "switch (e) {\n\tcase 1: a();\n\tdefault:\n\t}\n", "switch (e) {\n\tcase 1: a();\n\tcase _:;\n\t}\n"},
		{"default with a body", "switch (e) {\n\tdefault: a();\n}\n", "switch (e) {\n\tdefault: a();\n}\n"},
		{"empty case with several patterns", "switch (e) {\n\tcase A(_), B(_):\n\tcase C: x();\n}\n", "switch (e) {\n\tcase A(_)      :\n\tcase C: x();\n}\n"},
		{"in a string", "var s = \"typedef A = { a : Int }\";\n", "var s = \"typedef A = { a : Int }\";\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := masked(t, findHaxeConstructs, c.src); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
