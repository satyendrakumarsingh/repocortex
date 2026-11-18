package parse

import (
	"fmt"
	"sort"
	"testing"
)

// returnRows lists each function's declared return type and each call with
// the position of the call its receiver's value came from.
func returnRows(res *Result) []string {
	var out []string
	for _, s := range res.Symbols {
		if s.ReturnType != "" {
			out = append(out, "def "+s.Name+" -> "+s.ReturnType)
		}
	}
	for _, r := range res.References {
		if !isCallTag(r.Kind) {
			continue
		}
		row := fmt.Sprintf("call %s recv=%s/%s at %d:%d", r.Name, r.Receiver, r.ReceiverKind, r.Range.Start.Line, r.Range.Start.Col)
		if r.Value.Line != 0 {
			row += fmt.Sprintf(" from %d:%d", r.Value.Line, r.Value.Col)
		}
		out = append(out, row)
	}
	sort.Strings(out)
	return out
}

// A local initialised from one call carries that call's position, and every
// function records the plain type it declares it returns, in each language
// whose return types are read.
func TestReturnTypesAndProducers(t *testing.T) {
	cases := []struct {
		path, src string
		want      []string
	}{
		{"a.java", `class R {
  Order find(int id) { return null; }
  <T> T any() { return null; }
  java.util.Optional<Order> maybe() { return null; }
  void run() {
    var o = repo.find(1);
    o.save();
    Order p = repo.find(2);
    p.save();
    var q = repo.find(3).copy();
    q.save();
  }
}`, []string{
			"call copy recv=?/expr at 10:12",
			"call find recv=repo/name at 10:12",
			"call find recv=repo/name at 6:12",
			"call find recv=repo/name at 8:14",
			// A stated type wins: p is an Order by its declaration.
			"call save recv=Order/type at 9:4",
			"call save recv=o/value at 7:4 from 6:12",
			// A chain is not one call: q holds what copy returns.
			"call save recv=q/value at 11:4",
			"def find -> Order",
			// The outer type, as a stated Optional<Order> is read.
			"def maybe -> java.util.Optional",
		}},
		{"a.cs", `class R {
  Order Find(int id) { return null; }
  void Run() { var o = repo.Find(1); o.Save(); }
}`, []string{
			"call Find recv=repo/name at 3:23",
			"call Save recv=o/value at 3:37 from 3:23",
			"def Find -> Order",
		}},
		{"a.ts", `class R {
  find(id: number): Order { return null }
  name(): string { return "" }
  self(): this { return this }
  async run() {
    const o = this.repo.find(1)
    o.save()
    const p = await this.repo.find(1)
    p.save()
    const [a, b] = this.repo.find(1)
    a.save()
  }
}
function make(): Order { return null }
`, []string{
			"call find recv=this.repo/path at 10:19",
			"call find recv=this.repo/path at 6:14",
			"call find recv=this.repo/path at 8:20",
			// A destructuring target holds a part of the value, not the value.
			"call save recv=a/name at 11:4",
			"call save recv=o/value at 7:4 from 6:14",
			"call save recv=p/value at 9:4 from 8:20",
			"def find -> Order",
			"def make -> Order",
		}},
		{"a.swift", `class R {
  func find(id: Int) -> Order? { return nil }
  func run() {
    let o = try repo.find(id: 1)
    o.save()
  }
}`, []string{
			"call find recv=repo/name at 4:16",
			"call save recv=o/value at 5:4 from 4:16",
			"def find -> Order",
		}},
		{"a.rs", `impl R {
    fn find(&self, id: i32) -> Order { todo!() }
    fn build(self) -> Self { self }
    fn get<T>(&self) -> T { todo!() }
    fn run(&self) {
        let o = self.repo.find(1);
        o.save();
        let c = Cart::new();
        c.save();
    }
}`, []string{
			"call find recv=self.repo/path at 6:16",
			"call new recv=Cart/scope at 8:16",
			// A constructor states its type, and wins.
			"call save recv=Cart/type at 9:8",
			"call save recv=o/value at 7:8 from 6:16",
			"call todo recv=/bare at 2:39",
			"call todo recv=/bare at 4:28",
			"def find -> Order",
		}},
		{"a.php", `<?php class R {
  function find(int $id): ?Order { return null; }
  function run() {
    $o = $this->repo->find(1);
    $o->save();
    $m = $this->repo->find(1);
    $m = $this->other();
    $m->save();
  }
}`, []string{
			"call find recv=this.repo/path at 4:9",
			"call find recv=this.repo/path at 6:9",
			"call other recv=this/self at 7:9",
			// Assigned two calls' results: neither is what it holds.
			"call save recv=m/value at 8:4",
			"call save recv=o/value at 5:4 from 4:9",
			"def find -> Order",
		}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			wantList(t, "return types", returnRows(parse(t, c.path, c.src)), c.want)
		})
	}
}

// Languages whose return types are not read record none, and link nothing.
func TestReturnTypesOnlyWhereRead(t *testing.T) {
	for path, src := range map[string]string{
		"a.kt":    "class R { fun find(id: Int): Order { return x }\n fun f() { val o = repo.find(1); o.save() } }",
		"a.dart":  "class R { Order find(int id) { return null; } void f() { var o = repo.find(1); o.save(); } }",
		"a.scala": "class R { def find(id: Int): Order = null\n def f() = { val o = repo.find(1); o.save() } }",
		"a.py":    "class R:\n  def find(self, id) -> Order:\n    return None\n  def f(self):\n    o = self.repo.find(1)\n    o.save()\n",
	} {
		for _, row := range returnRows(parse(t, path, src)) {
			if len(row) > 4 && row[:4] == "def " {
				t.Errorf("%s: %s", path, row)
			}
		}
		for _, r := range parse(t, path, src).References {
			if r.Value.Line != 0 {
				t.Errorf("%s: %s linked to %v", path, r.Name, r.Value)
			}
		}
	}
}

func TestPlainReturnType(t *testing.T) {
	for in, want := range map[string]string{
		": Order":           "Order",
		"?Order":            "Order",
		"Order?":            "Order",
		"&Order":            "Order",
		"Box<Order>":        "Order",
		"Promise<Order>":    "Promise",
		"Result<Order, E>":  "Result",
		"crate::m::Order":   "crate::m::Order",
		"Self":              "",
		"void":              "",
		"string":            "",
		"T":                 "",
		"Order | undefined": "",
		"Order[]":           "",
	} {
		if got := plainReturnType(in, ""); got != want {
			t.Errorf("plainReturnType(%q) = %q, want %q", in, got, want)
		}
	}
	if got := plainReturnType("Item", "<Item>"); got != "" {
		t.Errorf("a type parameter was read as a type: %q", got)
	}
}
