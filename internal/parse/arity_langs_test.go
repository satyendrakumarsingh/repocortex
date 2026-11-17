package parse

import (
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// paramsByName lists, in source order, the parameter count of every symbol of
// the name: "2" for a known count, "?" for none.
func paramsByName(res *Result, name string) string {
	var out []string
	for _, s := range res.Symbols {
		if s.Name == name {
			out = append(out, arityText(s.ParamCount))
		}
	}
	return strings.Join(out, ",")
}

// argsByName is the same for the call references of the name.
func argsByName(res *Result, name string) string {
	var out []string
	for _, r := range res.References {
		if r.Name == name && strings.HasPrefix(r.Kind, "reference.call") {
			out = append(out, arityText(r.ArgCount))
		}
	}
	return strings.Join(out, ",")
}

func arityText(a model.Arity) string {
	if !a.Known() {
		return "?"
	}
	return fmt.Sprint(a.Count())
}

func checkArity(t *testing.T, res *Result, params, args map[string]string) {
	t.Helper()
	for name, want := range params {
		if got := paramsByName(res, name); got != want {
			t.Errorf("param counts of %s = %q, want %q", name, got, want)
		}
	}
	for name, want := range args {
		if got := argsByName(res, name); got != want {
			t.Errorf("arg counts of %s = %q, want %q", name, got, want)
		}
	}
}

// moshi's JsonAdapter and Moshi overload sets: the case that motivated the
// Kotlin counts (param_count was 0 for every overload).
func TestKotlinArity(t *testing.T) {
	res := parse(t, "A.kt", `package p

abstract class JsonAdapter<T> {
  abstract fun fromJson(reader: JsonReader): T
  fun fromJson(source: BufferedSource): T = fromJson(JsonReader.of(source))
  fun fromJson(string: String): T { return fromJson(JsonReader.of(Buffer().writeUtf8(string))) }
  abstract fun toJson(writer: JsonWriter, value: T)
  fun toJson(sink: BufferedSink, value: T) { toJson(JsonWriter.of(sink), value) }
  fun toJson(value: T): String { return "" }
  fun none() {}
  fun withDefault(a: Int, b: Int = 2) {}
  fun withVararg(vararg xs: Int) {}
  fun withModifier(crossinline f: () -> Unit, b: Int) {}
  fun <T> adapter(type: Type): JsonAdapter<T> = adapter(type, NO_ANNOTATIONS)
  fun <T> adapter(type: Type, annotations: Set<Annotation>): JsonAdapter<T> = adapter(type, annotations, null)
  fun <T> adapter(
    type: Type,
    annotations: Set<Annotation>,
    fieldName: String?,
  ): JsonAdapter<T> = x
}

fun String.ext(n: Int) {}

fun main() {
  val a = A(1)
  a.none()
  a.fromJson(reader)
  a.toJson(writer, v)
  a.toJson(v)
  a.toJson(writer, value = v)
  a.lam { println("x") }
  a.lam2(1) { }
  a.lam3() { }
  foo(*arr)
  foo(1, *arr)
  bar<Int>(1, 2)
}
`)
	checkArity(t, res, map[string]string{
		"fromJson":     "1,1,1",
		"toJson":       "2,2,1",
		"none":         "0",
		"withDefault":  "?",
		"withVararg":   "?",
		"withModifier": "2",
		"adapter":      "1,2,3",
		"ext":          "1",
	}, map[string]string{
		"none":     "0",
		"fromJson": "1,1,1",
		"toJson":   "2,2,1,2",
		"lam":      "1",
		"lam2":     "2",
		"lam3":     "1",
		"foo":      "?,?",
		"bar":      "2",
	})
}

// Scala counts the first list only; defaults and repeated parameters publish
// nothing; a call with a block has no count.
func TestScalaArity(t *testing.T) {
	res := parse(t, "A.scala", `package p

class A(val x: Int) {
  def none(): Unit = ()
  def one(a: Int) = 1
  def two(a: Int, b: String): Int = 1
  def curried(a: Int)(b: Int, c: Int) = 1
  def dflt(a: Int, b: Int = 2) = 1
  def vara(xs: Int*) = 1
  def /(that: A): A = this
  def gen[T](a: T, b: T): T = a
  def noparens = 1
}

object M {
  def main(args: Array[String]): Unit = {
    a.none()
    a.one(1)
    a.two(1, "x")
    a.two(b = "x", a = 1)
    a.curried(1)(2, 3)
    a.gen[Int](3, 4)
    foo(1, 2)
    foo { x => x }
    foo(xs: _*)
    a one 2
  }
}
`)
	checkArity(t, res, map[string]string{
		"none":     "0",
		"one":      "1",
		"two":      "2",
		"curried":  "1",
		"dflt":     "?",
		"vara":     "?",
		"gen":      "2",
		"noparens": "?",
	}, map[string]string{
		"none":    "0",
		"one":     "1,?",
		"two":     "2,2",
		"curried": "1",
		"gen":     "2",
		"foo":     "2,?,?",
	})
}

// serilog's Logger.Write overload set, with the params form and a span form.
func TestCSharpArity(t *testing.T) {
	res := parse(t, "A.cs", `namespace N
{
    public class Logger
    {
        public Logger() { }
        public Logger(int a) { }
        public void Write(LogEventLevel level, string messageTemplate) { }
        public void Write<T>(LogEventLevel level, string messageTemplate, T propertyValue) { }
        public void Write(LogEventLevel level, string messageTemplate, params object?[]? propertyValues) { }
        public void Write(LogEventLevel level, Exception? exception, string messageTemplate) { }
        public void Dflt(int a, int b = 2) { }
        public void Dflt2(int a, string b = Names.Empty) { }
        public void Ref(ref int a, out int b, in int c) { b = 0; }
        public static void Ext(this string s, int n) { }
        public int Expr(int a) => a;
        public void None() { }
        public void Run()
        {
            None();
            Write(level, messageTemplate);
            Write(level, ex, messageTemplate, NoPropertyValues);
            Write<int>(level, messageTemplate, 1);
            _logger.Write(level, messageTemplate, a, b);
            Write(level: x, messageTemplate: y);
            Ref(ref q, out r, in s);
            a?.Write(level, messageTemplate);
            this.Expr(1);
            foo(bar(1), baz(2, 3));
        }
    }
}
`)
	checkArity(t, res, map[string]string{
		"Logger": "?,0,1",
		"Write":  "2,3,?,3",
		"Dflt":   "?",
		"Dflt2":  "?",
		"Ref":    "3",
		"Ext":    "?",
		"Expr":   "1",
		"None":   "0",
	}, map[string]string{
		"None":  "0",
		"Write": "2,4,4,2",
		"Ref":   "3",
		"Expr":  "1",
		"foo":   "2",
		"bar":   "1",
		"baz":   "2",
	})
}

// benchmark's FormatKV overloads: five definitions of one name, each of two
// parameters, one of which has a const reference and a void list.
func TestCppArity(t *testing.T) {
	res := parse(t, "a.cc", `
std::string FormatKV(std::string const& key, std::string const& value) { return key; }
std::string FormatKV(std::string const& key, const char* value) { return key; }
std::string FormatKV(std::string const& key, bool value) { return key; }
std::string FormatKV(std::string const& key, int64_t value) { return key; }
std::string FormatKV(std::string const& key, int value) {
  return FormatKV(key, static_cast<int64_t>(value));
}
void voidp(void) {}
void none() {}
void unnamed(int, double) {}
void dflt(int a, int b = 2) {}
void va(int a, ...) {}
template <typename... Ts> void pack(Ts... xs) {}
void C::Out(int a, int b) {}
class C {
 public:
  C() {}
  C(int a) : x(a) {}
  void M(int a, int b) const {}
};
int main() {
  none();
  voidp();
  FormatKV("a", 1);
  c.M(1, 2);
  p->M(3, 4);
  ns::none();
  va(1, 2, 3);
  pack(xs...);
  foo(bar(1), baz(2, 3));
  gen<int>(1, 2);
}
`)
	checkArity(t, res, map[string]string{
		"FormatKV": "2,2,2,2,2",
		"voidp":    "0",
		"none":     "0",
		"unnamed":  "2",
		"dflt":     "?",
		"va":       "?",
		"pack":     "?",
		"Out":      "2",
		"C":        "?,0,1",
		"M":        "2",
	}, map[string]string{
		"FormatKV": "2,2",
		"none":     "0",
		"voidp":    "0",
		"M":        "2,2",
		"va":       "3",
		"pack":     "?",
		"foo":      "2",
		"bar":      "1",
		"baz":      "2",
	})
}

func TestJavaScriptArity(t *testing.T) {
	res := parse(t, "a.js", `function plain(a, b) {}
function dflt(a, b = 1) {}
function rest(a, ...r) {}
function destructured({ x }, [y], z) {}
function none() {}
const arrow = (a, b, c) => a;
const shorthand = x => x;
const fexpr = function (a) {};
class K {
  method(a, b) { plain(1, 2); this.method(1); dflt(1); rest(1, 2, 3); none(); }
}
exports.exported = function (a, b) {};
plain(...xs);
plain(1, 2);
arrow(1, 2, 3);
`)
	checkArity(t, res,
		map[string]string{
			"plain": "2", "dflt": "?", "rest": "?", "destructured": "3", "none": "0",
			"arrow": "3", "shorthand": "?", "fexpr": "1", "method": "2", "exported": "2",
		},
		map[string]string{
			"plain": "2,?,2", "method": "1", "dflt": "1", "rest": "3", "none": "0", "arrow": "3",
		})
}

func TestRubyArity(t *testing.T) {
	res := parse(t, "a.rb", `class A
  def plain(a, b); end
  def dflt(a, b = 1); end
  def kw(a, k:); end
  def splat(*xs); end
  def blk(a, &b); end
  def self.build(a); end
  def bare; end
  def run
    plain(1, 2)
    plain(1, k: 2)
    plain(*xs)
    other.plain(1, 2)
    blk(1) { }
    blk(1, &fn)
    build(1)
  end
end
`)
	checkArity(t, res,
		map[string]string{
			"plain": "2", "dflt": "?", "kw": "?", "splat": "?", "blk": "1", "build": "1", "bare": "?",
		},
		map[string]string{
			"plain": "2,?,?,2", "blk": "1,1", "build": "1",
		})
}

func TestPHPArity(t *testing.T) {
	res := parse(t, "a.php", `<?php
function plain($a, $b) {}
function dflt($a, $b = 1) {}
function variadic($a, ...$r) {}
class A {
    public function __construct(private int $a, int $b) {}
    public function method(int $a, B $b) {}
    public static function make($a) {}
    public function run() {
        plain(1, 2);
        plain(...$xs);
        plain(a: 1, b: 2);
        $this->method(1, 2);
        self::make(1);
        \App\plain(1, 2);
        $x?->method(1);
    }
}
`)
	checkArity(t, res,
		map[string]string{
			"plain": "2", "dflt": "?", "variadic": "?", "__construct": "2", "method": "2", "make": "1",
		},
		map[string]string{
			"plain": "2,?,?,2", "method": "2,1", "make": "1",
		})
}

func TestRustArity(t *testing.T) {
	res := parse(t, "a.rs", `struct A;
impl A {
    fn new(a: i32) -> A { A }
    fn method(&self, a: i32, b: &str) {}
    fn typed_self(self: &Self, a: i32) {}
    fn gen<T>(x: T, (a, b): (i32, i32)) {}
    fn none() {}
}
fn plain(a: i32, b: i32) {}
fn run() {
    plain(1, 2);
    A::new(1);
    A::none();
    a.method(1, "x");
    A::gen::<i32>(1, (1, 2));
    println!("{}", 1);
}
`)
	checkArity(t, res,
		map[string]string{
			"plain": "2", "new": "1", "method": "2", "typed_self": "?", "gen": "2", "none": "0",
		},
		map[string]string{
			"plain": "2", "new": "1", "none": "0", "method": "2",
		})
}
