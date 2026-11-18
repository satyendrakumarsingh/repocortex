package pipeline

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// forbidEdge fails for every edge from a symbol whose FQN ends in src to one whose
// FQN ends in dst, whatever its confidence: a wrong edge is wrong at low too.
func forbidEdge(t *testing.T, got []langEdge, src, dst string) {
	t.Helper()
	for _, e := range got {
		if strings.HasSuffix(e.Src, src) && strings.HasSuffix(e.Dst, dst) {
			t.Errorf("wrong edge %s -> %s (%s %s: %s)", e.Src, e.Dst, e.Kind, e.Confidence, e.Reason)
		}
	}
}

// requireEdge fails unless an edge from src to dst exists at min or above.
func requireEdge(t *testing.T, got []langEdge, src, dst string, min model.Confidence) {
	t.Helper()
	for _, e := range got {
		if strings.HasSuffix(e.Src, src) && strings.HasSuffix(e.Dst, dst) {
			if !e.Confidence.AtLeast(min) {
				t.Errorf("edge %s -> %s is %s, want at least %s (%s)", e.Src, e.Dst, e.Confidence, min, e.Reason)
			}
			return
		}
	}
	t.Errorf("missing edge %s -> %s", src, dst)
}

// TestReassignedLocalKeepsNoProducer checks that a local whose type was read
// from the call it was initialised with loses that type the moment the
// function assigns it again (internal/parse/shape.go). `var o = repo.make();
// o = repo.other(); o.save()` calls Customer.save, and binding Order.save
// because the declaration said Order is wrong however it is weighted.
func TestReassignedLocalKeepsNoProducer(t *testing.T) {
	high := model.ConfidenceHigh
	t.Run("java", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"Order.java":    "class Order {\n    void save() {}\n    void ship() {}\n}\n",
			"Customer.java": "class Customer {\n    void save() {}\n    void notifyAll2() {}\n}\n",
			"Repo.java": `class Repo {
    Order find(int id) { return null; }
    Customer find(String name) { return null; }
    Order make() { return null; }
    Customer other() { return null; }
}
`,
			"Use.java": `class Use {
    void a(Repo repo) {
        var o = repo.make();
        o = repo.other();
        o.save();
    }
    void b(Repo repo, String s) {
        var o = repo.find(s);
        o.save();
    }
    void c(Repo repo) {
        var o = repo.make();
        if (true) {
            var o2 = repo.other();
            o2.save();
        }
        o.save();
    }
    void d(Repo repo) {
        var o = repo.make();
        for (int i = 0; i < 3; i++) {
            o.save();
            o = repo.other();
        }
    }
    void e(Repo repo) {
        var x = repo.make();
        Runnable r = () -> { var x2 = repo.other(); x2.save(); };
        x.save();
    }
}
`,
		})
		forbidEdge(t, got, "Use.java:Use.a", "Order.save")
		forbidEdge(t, got, "Use.java:Use.d", "Order.save")
		// Overloads of find differ by parameter type and return different
		// types: nothing says which one `find(s)` is for the resolver.
		forbidEdge(t, got, "Use.java:Use.b", "Order.save")
		forbidEdge(t, got, "Use.java:Use.b", "Customer.save")
		// Untouched by any of it: a second variable, and a lambda's own.
		requireEdge(t, got, "Use.java:Use.c", "Order.save", high)
		requireEdge(t, got, "Use.java:Use.c", "Customer.save", high)
		requireEdge(t, got, "Use.java:Use.e", "Order.save", high)
		requireEdge(t, got, "Use.java:Use.e", "Customer.save", high)
	})
	t.Run("typescript", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"t.ts": `class Order { save(): void {} }
class Customer { save(): void {} }
class Repo {
    make(): Order { return new Order(); }
    other(): Customer { return new Customer(); }
}
function a(repo: Repo) {
    let o = repo.make();
    o = repo.other() as any;
    o.save();
}
function b(repo: Repo) {
    let o = repo.make();
    if (Math.random() > 0.5) { o = repo.other(); }
    o.save();
}
function c(repo: Repo) {
    const o = repo.make();
    o.save();
}
`})
		forbidEdge(t, got, "t.ts:a", "Order.save")
		forbidEdge(t, got, "t.ts:b", "Order.save")
		requireEdge(t, got, "t.ts:c", "Order.save", high)
	})
	t.Run("c_sharp", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"a.cs": `class Order { public void Save() {} }
class Customer { public void Save() {} }
class Repo {
    public Order Make() { return null; }
    public Customer Other() { return null; }
}
class U {
    void A(Repo repo) {
        var o = repo.Make();
        o = repo.Other();
        o.Save();
    }
    void B(Repo repo) {
        var o = repo.Make();
        o.Save();
    }
}
`})
		forbidEdge(t, got, "a.cs:U.A", "Order.Save")
		requireEdge(t, got, "a.cs:U.B", "Order.Save", high)
	})
	t.Run("rust", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"a.rs": `struct Order;
struct Customer;
impl Order { fn save(&self) {} }
impl Customer { fn save(&self) {} }
struct Repo;
impl Repo {
    fn make(&self) -> Order { Order }
    fn other(&self) -> Customer { Customer }
}
fn c(repo: &Repo) {
    let mut o = repo.make();
    o = repo.other();
    o.save();
}
fn d(repo: &Repo) {
    let o = repo.make();
    o.save();
}
`})
		forbidEdge(t, got, "a.rs:c", "Order.save")
		requireEdge(t, got, "a.rs:d", "Order.save", high)
	})
	t.Run("swift", func(t *testing.T) {
		got := langEdges(t, map[string]string{
			"a.swift": `class Order { func save() {} }
class Customer { func save() {} }
class Repo {
    func make() -> Order { return Order() }
    func other() -> Customer { return Customer() }
}
func a(repo: Repo) {
    var o = repo.make()
    o = repo.other()
    o.save()
}
func d(repo: Repo) {
    let o = repo.make()
    o.save()
}
`})
		forbidEdge(t, got, "a.swift:a", "Order.save")
		requireEdge(t, got, "a.swift:d", "Order.save", model.ConfidenceMedium)
	})
}
