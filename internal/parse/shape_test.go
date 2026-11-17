package parse

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// shapeOf parses one file and returns each call reference's shape, keyed by
// "name@line", plus the whole result for assertions about what is NOT there.
func shapeOf(t *testing.T, file, src string) (map[string]Reference, *Result) {
	t.Helper()
	p := NewRegistry().ForFile(file)
	if p == nil {
		t.Fatalf("no parser for %s", file)
	}
	res, err := p.Parse(context.Background(), file, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Reference{}
	for _, r := range res.References {
		if !strings.Contains(r.Kind, "call") {
			continue
		}
		key := r.Name
		if _, dup := out[key]; dup {
			key = r.Name + "@" + itoa(r.Range.Start.Line)
		}
		out[key] = r
	}
	return out, res
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

type wantShape struct{ call, kind, receiver string }

func checkShapes(t *testing.T, file, src string, want []wantShape, notCalls []string) *Result {
	t.Helper()
	got, res := shapeOf(t, file, src)
	for _, w := range want {
		r, ok := got[w.call]
		if !ok {
			t.Errorf("%s: no call reference %q (have %v)", file, w.call, keysOf(got))
			continue
		}
		if r.ReceiverKind != w.kind || r.Receiver != w.receiver {
			t.Errorf("%s: %s is recorded as %s %q, want %s %q",
				file, w.call, r.ReceiverKind, r.Receiver, w.kind, w.receiver)
		}
	}
	for _, name := range notCalls {
		if r, ok := got[name]; ok {
			t.Errorf("%s: %q is recorded as a call (%s %q); it is the receiver or qualifier of one",
				file, name, r.ReceiverKind, r.Receiver)
		}
	}
	return res
}

func keysOf(m map[string]Reference) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCallShapesAreRecordedForEveryLanguage pins the representation: what
// stands before the dot of a call, in the one vocabulary every language
// shares (model.ReceiverBare and its siblings).
//
// Each source is a wrong binding the verification record lists
// (docs/verification/2026-11-12), reduced to the line that caused it. The
// resolver could not get any of them right while the parser reported the same
// thing — an empty receiver — for a bare call, a chained one and a qualified
// one.
func TestCallShapesAreRecordedForEveryLanguage(t *testing.T) {
	t.Run("go", func(t *testing.T) {
		checkShapes(t, "a.go", `package x

import (
	"errors"
	h "net/http"
)

type T struct{ in *U }

func (t *T) M(w h.ResponseWriter, u *U) error {
	x := &U{}
	root := pick()
	t.in.Get()
	w.Header().Peek("a")
	x.Do()
	root.addRoute()
	a[0].Index()
	T{}.Lit(nil, nil)
	callback := u.Fn
	callback()
	return errors.New("x")
}
`, []wantShape{
			{"pick", model.ReceiverBare, ""},
			{"Get", model.ReceiverType, "U"},
			{"Header", model.ReceiverType, "h.ResponseWriter"},
			{"Peek", model.ReceiverExpr, model.OpaqueReceiver},
			{"Do", model.ReceiverType, "U"},
			{"addRoute", model.ReceiverValue, "root"},
			{"Index", model.ReceiverExpr, model.OpaqueReceiver},
			{"Lit", model.ReceiverType, "T"},
			{"New", model.ReceiverName, "errors"},
		}, nil)
	})

	t.Run("java", func(t *testing.T) {
		res := checkShapes(t, "A.java", `import java.util.List;
class A extends B {
  private final JsonReader reader;
  int m(List<String> strings, Foo foo) {
    reader.peek();
    this.reader.skip();
    strings.add("x");
    asBigDecimal().intValue();
    super.clone();
    Util.stat(1);
    new GsonBuilder().create();
    return this.own(foo);
  }
}
`, []wantShape{
			{"peek", model.ReceiverType, "JsonReader"},
			{"skip", model.ReceiverType, "JsonReader"},
			{"add", model.ReceiverType, "List"},
			{"asBigDecimal", model.ReceiverBare, ""},
			{"intValue", model.ReceiverExpr, model.OpaqueReceiver},
			{"clone", model.ReceiverSuper, "super"},
			{"stat", model.ReceiverName, "Util"},
			{"create", model.ReceiverType, "GsonBuilder"},
			{"own", model.ReceiverSelf, "this"},
		}, []string{"reader", "strings", "Util"})
		if res.Qualifiers == 0 {
			t.Error("no qualifier rows were counted as dropped; the Java tags query emits one per receiver")
		}
	})

	t.Run("typescript", func(t *testing.T) {
		checkShapes(t, "a.ts", `import * as util from "../core/util.js";
export class A extends B {
  private reader: Reader;
  run(types: string[], r: Reader): void {
    util.joinValues(types);
    Object.defineProperty(this, "x", {});
    types.includes("a");
    this.reader.peek();
    this.again([], r);
    super.base([], r);
    make().go();
  }
}
`, []wantShape{
			{"joinValues", model.ReceiverName, "util"},
			{"defineProperty", model.ReceiverName, "Object"},
			{"includes", model.ReceiverValue, "types"},
			{"peek", model.ReceiverType, "Reader"},
			{"again", model.ReceiverSelf, "this"},
			{"base", model.ReceiverSuper, "super"},
			{"make", model.ReceiverBare, ""},
			{"go", model.ReceiverExpr, model.OpaqueReceiver},
		}, nil)
	})

	t.Run("rust", func(t *testing.T) {
		checkShapes(t, "a.rs", `struct W { inner: Inner }
impl W {
    fn foo(&self, b: &Builder) -> u8 {
        self.inner.foo();
        self.bar();
        Self::make(1);
        messages::errored();
        crate::messages::set(1);
        b.line_number(false);
        SearcherBuilder::new().multi_line(false);
        Vec::<u8>::with_capacity(1);
        helper()
    }
}
`, []wantShape{
			{"foo", model.ReceiverType, "Inner"},
			{"bar", model.ReceiverSelf, "self"},
			{"make", model.ReceiverSelf, "Self"},
			{"errored", model.ReceiverScope, "messages"},
			{"set", model.ReceiverScope, "crate::messages"},
			{"line_number", model.ReceiverType, "Builder"},
			{"new", model.ReceiverScope, "SearcherBuilder"},
			{"multi_line", model.ReceiverExpr, model.OpaqueReceiver},
			{"with_capacity", model.ReceiverScope, "Vec"},
			{"helper", model.ReceiverBare, ""},
		}, []string{"messages", "SearcherBuilder", "Self", "Vec"})
	})

	t.Run("ruby", func(t *testing.T) {
		checkShapes(t, "a.rb", `class Base
  def call(env)
    String.new
    Rack::Mutex.new
    @app.dispatch(env)
    helper env
    self.respond_to?(:x)
    Queue.new.push(1)
  end
end
`, []wantShape{
			{"new", model.ReceiverName, "String"},
			{"new@4", model.ReceiverScope, "Rack::Mutex"},
			{"dispatch", model.ReceiverName, "@app"},
			{"helper", model.ReceiverBare, ""},
			{"respond_to?", model.ReceiverSelf, "self"},
			{"push", model.ReceiverType, "Queue"},
		}, nil)
	})

	t.Run("cpp", func(t *testing.T) {
		checkShapes(t, "a.cc", `class Iter {
 public:
  void Next() { iter_->Advance(); helper(); this->Reset(); }
  Iterator* iter_;
};
void run(Parser* parser) {
  parser->callbacks.error(p_ctx);
  std::string x;
  x.append("a");
}
`, []wantShape{
			{"Advance", model.ReceiverType, "Iterator"},
			{"helper", model.ReceiverBare, ""},
			{"Reset", model.ReceiverSelf, "this"},
			{"error", model.ReceiverPath, "parser.callbacks"},
			{"append", model.ReceiverType, "std::string"},
		}, nil)
	})

	t.Run("c_sharp", func(t *testing.T) {
		checkShapes(t, "A.cs", `class A : B {
  private readonly JsonWriter _writer;
  int M(JsonReader reader) {
    reader.Read();
    _writer.WriteStartObject();
    StringUtils.IsNullOrEmpty("x");
    Helper();
    var t = new JToken();
    t.Render();
    return 0;
  }
}
`, []wantShape{
			{"Read", model.ReceiverType, "JsonReader"},
			{"WriteStartObject", model.ReceiverType, "JsonWriter"},
			{"IsNullOrEmpty", model.ReceiverName, "StringUtils"},
			{"Helper", model.ReceiverBare, ""},
			{"Render", model.ReceiverType, "JToken"},
		}, nil)
	})

	t.Run("python", func(t *testing.T) {
		checkShapes(t, "a.py", `import json as _json

class Client(Base):
    def get(self, client, entry: Ledger):
        client.fetch("/")
        _json.dumps({})
        self.save()
        super().get(client)
        entry.post()
        make().run()
        open("x")
`, []wantShape{
			{"fetch", model.ReceiverValue, "client"},
			{"dumps", model.ReceiverName, "_json"},
			{"save", model.ReceiverSelf, "self"},
			{"get", model.ReceiverSuper, "super"},
			{"post", model.ReceiverType, "Ledger"},
			{"run", model.ReceiverExpr, model.OpaqueReceiver},
			{"open", model.ReceiverBare, ""},
		}, []string{"client", "_json", "entry"})
	})
}

// TestDefinitionsAreQualifiedByWhatEnclosesThem pins the other half of the
// walk: a member's qualified name states its type, in every language, the way
// pyscope.go states it for Python. Without it `self.f()` cannot be told from a
// call to whichever `f` the file happens to contain.
func TestDefinitionsAreQualifiedByWhatEnclosesThem(t *testing.T) {
	for _, tc := range []struct {
		file, src string
		want      map[string]string
	}{
		{"a.go", "package x\ntype T struct{}\nfunc (t *T) M() {}\nfunc F() {}\n",
			map[string]string{"M": "a.go:T.M", "F": ""}},
		{"A.java", "class A {\n  int f;\n  A() {}\n  void m() {}\n  class In { void n() {} }\n}\n",
			map[string]string{"m": "A.java:A.m", "n": "A.java:A.In.n", "f": "A.java:A.f"}},
		{"a.ts", "export class A {\n  run(): void {}\n}\nfunction f() { function inner() {} }\nconst g = () => { function deep() {} };\n",
			map[string]string{"run": "a.ts:A.run", "inner": "a.ts:f.<locals>.inner", "deep": "a.ts:g.<locals>.deep", "f": ""}},
		{"a.rs", "struct W;\nimpl W {\n    fn foo(&self) {}\n}\nimpl Tr for W {\n    fn t(&self) {}\n}\nfn free() {}\n",
			map[string]string{"foo": "a.rs:W.foo", "t": "a.rs:W.t", "free": ""}},
		{"a.rb", "module Sinatra\n  class Base\n    def self.new; end\n    def call; end\n  end\nend\ndef top; end\n",
			map[string]string{"new": "a.rb:Sinatra.Base.new", "call": "a.rb:Sinatra.Base.call", "top": ""}},
		{"A.cs", "namespace N {\n  class A {\n    void M() {}\n  }\n}\n",
			map[string]string{"M": "A.cs:A.M"}},
		{"a.cc", "namespace db {\nclass Iter {\n public:\n  void Next() {}\n};\nstatic void helper() {}\n}\n",
			map[string]string{"Next": "a.cc:Iter.Next", "helper": ""}},
	} {
		p := NewRegistry().ForFile(tc.file)
		res, err := p.Parse(context.Background(), tc.file, []byte(tc.src))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, s := range res.Symbols {
			got[s.Name] = s.FQN
		}
		for name, want := range tc.want {
			fqn, ok := got[name]
			if !ok {
				t.Errorf("%s: no symbol %q (have %v)", tc.file, name, got)
				continue
			}
			if fqn != want {
				t.Errorf("%s: %s has qualified name %q, want %q", tc.file, name, fqn, want)
			}
		}
	}
}

// TestOrphanedMembersAreAdoptedByTheTypeBeforeThem: a C# class body broken by
// a preprocessor directive is closed early in the syntax tree, and the methods
// after the directive are left outside their class. In a language where a
// method cannot stand outside a type, the type declared before it is its
// owner. Measured on Newtonsoft.Json: 671 of 4,545 methods.
func TestOrphanedMembersAreAdoptedByTheTypeBeforeThem(t *testing.T) {
	src := `namespace N {
  public class Reader {
    void First() {}
#if HAVE_BIG_INTEGER
    void Guarded() {}
#else
    void Other() {}
#endif
    void After() {}
  }
}
`
	p := NewRegistry().ForFile("Reader.cs")
	res, err := p.Parse(context.Background(), "Reader.cs", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Symbols {
		if s.Kind != model.KindMethod {
			continue
		}
		if s.FQN != "Reader.cs:Reader."+s.Name {
			t.Errorf("method %s has qualified name %q, want it owned by Reader", s.Name, s.FQN)
		}
	}
}

// TestReceiverClassificationIsTextual pins classifyReceiver directly, on the
// shapes no single-language source above reaches.
func TestReceiverClassificationIsTextual(t *testing.T) {
	for _, tc := range []struct {
		language, text, op string
		kind, receiver     string
	}{
		{"go", "x", ".", model.ReceiverName, "x"},
		{"go", "a.b.c", ".", model.ReceiverPath, "a.b.c"},
		{"go", "a\n\t.b", ".", model.ReceiverPath, "a.b"},
		{"go", "f()", ".", model.ReceiverExpr, model.OpaqueReceiver},
		{"go", "a[0]", ".", model.ReceiverExpr, model.OpaqueReceiver},
		{"go", "&T{}", ".", model.ReceiverType, "T"},
		{"typescript", "a?.b", ".", model.ReceiverPath, "a.b"},
		{"typescript", "new Foo<T>(1)", ".", model.ReceiverType, "Foo"},
		{"typescript", "new Foo().bar()", ".", model.ReceiverExpr, model.OpaqueReceiver},
		{"cpp", "p->q", ".", model.ReceiverPath, "p.q"},
		{"cpp", "Status", "::", model.ReceiverScope, "Status"},
		{"rust", "self", ".", model.ReceiverSelf, "self"},
		{"rust", "self", "::", model.ReceiverScope, "self"},
		{"rust", "super", "::", model.ReceiverScope, "super"},
		{"rust", "Self", "::", model.ReceiverSelf, "Self"},
		{"rust", "HashMap::<K, V>", "::", model.ReceiverScope, "HashMap"},
		{"java", "super", ".", model.ReceiverSuper, "super"},
		{"c_sharp", "base", ".", model.ReceiverSuper, "base"},
		{"java", "base", ".", model.ReceiverName, "base"},
		{"python", "super()", ".", model.ReceiverSuper, "super"},
		{"php", "$this", "->", model.ReceiverSelf, "this"},
		{"php", "parent", "::", model.ReceiverSuper, "parent"},
		{"ruby", "Rack::Mutex", ".", model.ReceiverScope, "Rack::Mutex"},
		{"ruby", "a b", ".", model.ReceiverExpr, model.OpaqueReceiver},
	} {
		got := classifyReceiver(tc.language, tc.text, tc.op)
		if got.kind != tc.kind || got.receiver != tc.receiver {
			t.Errorf("%s %q before %q: %s %q, want %s %q",
				tc.language, tc.text, tc.op, got.kind, got.receiver, tc.kind, tc.receiver)
		}
	}
}

// TestDeclaredTypeIsWithdrawnByASecondDeclaration: a variable has a type only
// where the function binds that name once. This is pyscope.go's rule, kept for
// every language: tracking which declaration reaches which call is inference.
func TestDeclaredTypeIsWithdrawnByASecondDeclaration(t *testing.T) {
	got, _ := shapeOf(t, "a.go", `package x

func f(a *A) {
	a.First()
	if true {
		a := other()
		a.Second()
	}
}
`)
	for _, name := range []string{"First", "Second"} {
		if r := got[name]; r.ReceiverKind != model.ReceiverValue {
			t.Errorf("%s is recorded as %s %q; the name is declared twice with different types, "+
				"so neither call has a stated type", name, r.ReceiverKind, r.Receiver)
		}
	}
}

// R reads `$` as the member operator and `:::` as `::`; the head of an Elixir
// clause and of a typespec is not a call; a PowerShell type literal names the
// type of a static call, with any namespace dropped, and a bash command is bare.
func TestCallShapesOfScriptLanguages(t *testing.T) {
	checkShapes(t, "a.R", `f <- function(o) {
  helper(1)
  pkg::g(2)
  pkg:::h(3)
  o$run(4)
  self$step(5)
  xs[[1]]$walk(6)
}
`, []wantShape{
		{"helper", "bare", ""}, {"g", "scope", "pkg"}, {"h", "scope", "pkg"},
		{"run", "value", "o"}, {"step", "self", "self"}, {"walk", "expr", "?"},
	}, nil)

	checkShapes(t, "a.ex", `defmodule A do
  @spec total(t) :: number
  def total(c) when is_map(c), do: Money.zero()
  defp helper(c), do: total(c)
end
`, []wantShape{{"zero", "name", "Money"}, {"is_map", "bare", ""}, {"total", "bare", ""}}, nil)

	checkShapes(t, "a.ps1", `function F {
  [Acct]::Create(5)
  [System.IO.File]::Exists($p)
  $this.Save()
  Get-Thing -Arg 1
}
`, []wantShape{
		{"Create", "type", "Acct"}, {"Exists", "type", "File"}, {"Save", "self", "this"}, {"Get-Thing", "bare", ""},
	}, nil)

	checkShapes(t, "a.sh", "deploy() {\n  FOO=1 build --now | grep x\n}\n", []wantShape{{"build", "bare", ""}, {"grep", "bare", ""}}, nil)
}
