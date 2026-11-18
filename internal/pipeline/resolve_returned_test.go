package pipeline

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestReturnedTypeTypeScript checks a call on a variable assigned from a call,
// bound through the type the called function declares it returns
// (internal/resolve/infer/returned.go).
//
// Order, Invoice and Lines all declare save or total, so the name alone
// chooses nothing. What each case pins:
//   - run: the producer is bound at high on a stated type, so the call on its
//     result is high too;
//   - named: the producer is bound at medium by a receiver's name, and the
//     call on its result is no more than medium;
//   - loose: the producer is bound to nothing, and the call on its result is
//     not typed by a guess;
//   - inherited: the returned type does not declare the method, and the call
//     is not bound to another type's;
//   - restated: a stated type wins over the returned one.
func TestReturnedTypeTypeScript(t *testing.T) {
	got := langEdges(t, returnedTSFixture())
	high, medium := model.ConfidenceHigh, model.ConfidenceMedium
	wants := []want{
		{"src/shop.ts:Shop.run", "src/repo.ts:Repo.find", high},
		{"src/shop.ts:Shop.run", "src/order.ts:Order.save", high},
		{"src/shop.ts:Shop.named", "src/order.ts:Order.total", medium},
		{"src/shop.ts:Shop.inherited", "src/repo.ts:Repo.lines", high},
		{"src/shop.ts:Shop.restated", "src/repo.ts:Repo.find", high},
		{"src/shop.ts:Shop.restated", "src/invoice.ts:Invoice.save", high},
		{"src/repo.ts:Repo.find", "src/order.ts:Order", high},
		{"src/repo.ts:Repo.lines", "src/lines.ts:Lines", high},
	}
	forbid := []want{
		{Src: "src/shop.ts:Shop.run", Dst: "src/invoice.ts:Invoice.save"},
		{Src: "src/shop.ts:Shop.loose", Dst: ".total"},
		{Src: "src/shop.ts:Shop.inherited", Dst: ".total"},
		{Src: "src/shop.ts:Shop.restated", Dst: "src/order.ts:Order.save"},
	}
	extraOK := []want{
		{"src/shop.ts:Shop.named", "src/repo.ts:Repo.find", medium},
	}
	scoreEdges(t, got, wants, forbid, extraOK)
	capped(t, got, []want{
		// Two bindings, the first at medium: the edge is no stronger.
		{"src/shop.ts:Shop.named", "src/order.ts:Order.total", medium},
	})
	for _, e := range got {
		if strings.Contains(e.Reason, "the call that assigned the receiver returns") && e.Confidence == model.ConfidenceExact {
			t.Errorf("an edge through a return type is exact: %v", e)
		}
	}
}

func returnedTSFixture() map[string]string {
	return map[string]string{
		"package.json": `{"name":"shop"}`,
		"src/order.ts": `export class Order {
  save(): void {}
  total(): number { return 1 }
}
`,
		"src/invoice.ts": `export class Invoice {
  save(): void {}
  total(): number { return 2 }
}
`,
		"src/lines.ts": `export class Lines {
  save(): void {}
}
`,
		"src/repo.ts": `import { Order } from './order'
import { Lines } from './lines'
export class Repo {
  find(id: number): Order { return new Order() }
  lines(): Lines { return new Lines() }
}
`,
		"src/shop.ts": `import { Repo } from './repo'
import { Invoice } from './invoice'
export class Shop {
  constructor(private repo: Repo) {}
  run() {
    const o = this.repo.find(1)
    o.save()
  }
  named(repo) {
    const o = repo.find(2)
    o.total()
  }
  loose(store) {
    const o = store.find(2)
    o.total()
  }
  inherited() {
    const l = this.repo.lines()
    l.total()
  }
  restated() {
    const i: Invoice = this.repo.find(3) as any
    i.save()
  }
}
`,
	}
}

// TestReturnedTypeJava checks the same rule in Java, and that a return type
// is named from where the function is declared, not from the call:
//   - Receipt is declared in the package of the function that returns it, so
//     the type is identified, and the call on the result is high;
//   - Order is declared in another package, and is the only class of that
//     name: identified by the name alone, which is the weaker evidence the
//     language's measured tier for a unique type name says it is (medium);
//   - Money is the name of two classes, neither in the declaring file or
//     package: nothing says which one `price` returns, and the call on its
//     result is not typed by it.
func TestReturnedTypeJava(t *testing.T) {
	got := langEdges(t, returnedJavaFixture())
	high, medium := model.ConfidenceHigh, model.ConfidenceMedium
	wants := []want{
		{"shop/app/Shop.java:Shop.run", "shop/repo/Repo.java:Repo.find", high},
		{"shop/app/Shop.java:Shop.run", "shop/model/Order.java:Order.save", medium},
		{"shop/app/Shop.java:Shop.print", "shop/repo/Repo.java:Repo.receipt", high},
		{"shop/app/Shop.java:Shop.print", "shop/repo/Receipt.java:Receipt.save", high},
		{"shop/app/Shop.java:Shop.pay", "shop/repo/Repo.java:Repo.price", high},
	}
	forbid := []want{
		{Src: "shop/app/Shop.java:Shop.run", Dst: "shop/model/Invoice.java:Invoice.save"},
		{Src: "shop/app/Shop.java:Shop.run", Dst: "shop/repo/Receipt.java:Receipt.save"},
		{Src: "shop/app/Shop.java:Shop.print", Dst: "shop/model/Order.java:Order.save"},
		{Src: "shop/app/Shop.java:Shop.pay", Dst: "Money.java:Money.add"},
	}
	// Type references, which this test is not about.
	extraOK := []want{
		{"shop/app/Shop.java:Shop.repo", "shop/repo/Repo.java:Repo", high},
		{"shop/repo/Repo.java:Repo.find", "shop/model/Order.java:Order", high},
		{"shop/repo/Repo.java:Repo.receipt", "shop/repo/Receipt.java:Receipt", high},
		{"shop/repo/Repo.java:Repo.price", "shop/eur/Money.java:Money", high},
		{"shop/eur/Money.java:Money.add", "shop/eur/Money.java:Money", high},
		{"shop/usd/Money.java:Money.add", "shop/usd/Money.java:Money", high},
	}
	scoreEdges(t, got, wants, forbid, extraOK)
	capped(t, got, []want{{"shop/app/Shop.java:Shop.run", "shop/model/Order.java:Order.save", medium}})
}

func returnedJavaFixture() map[string]string {
	return map[string]string{
		"src/main/java/shop/model/Order.java": `package shop.model;
public class Order { public void save() {} }
`,
		"src/main/java/shop/model/Invoice.java": `package shop.model;
public class Invoice { public void save() {} }
`,
		"src/main/java/shop/eur/Money.java": `package shop.eur;
public class Money { public Money add(int c) { return this; } }
`,
		"src/main/java/shop/usd/Money.java": `package shop.usd;
public class Money { public Money add(int c) { return this; } }
`,
		"src/main/java/shop/repo/Repo.java": `package shop.repo;
import shop.model.Order;
import shop.eur.Money;
public class Repo {
  public Receipt receipt(int id) { return null; }
  public Order find(int id) { return null; }
  public Money price(int id) { return null; }
}
`,
		"src/main/java/shop/repo/Receipt.java": `package shop.repo;
public class Receipt { public void save() {} }
`,
		"src/main/java/shop/app/Shop.java": `package shop.app;
import shop.repo.Repo;
public class Shop {
  private Repo repo;
  void run() {
    var o = repo.find(1);
    o.save();
  }
  void print() {
    var r = repo.receipt(1);
    r.save();
  }
  void pay() {
    var m = repo.price(1);
    m.add(1);
  }
}
`,
	}
}

// TestReturnedTypeOtherLanguages runs the rule once in each other language
// whose return types are read: Order and Invoice both declare save, and only
// find's declared return type says which one `o` is.
func TestReturnedTypeOtherLanguages(t *testing.T) {
	cases := map[string]struct {
		files     map[string]string
		run, save string
	}{
		"c_sharp": {map[string]string{
			"Shop/Models.cs": `namespace Shop {
public class Order { public void Save() {} }
public class Invoice { public void Save() {} }
public class Repo { public Order Find(int id) { return null; } }
}
`,
			"Shop/App.cs": `namespace Shop {
public class App {
  private Repo repo;
  void Run() { var o = repo.Find(1); o.Save(); }
}
}
`}, "App.cs:App.Run", "Models.cs:Order.Save"},
		"rust": {map[string]string{
			"Cargo.toml": "[package]\nname = \"shop\"\n",
			"src/models.rs": `pub struct Order {}
impl Order { pub fn save(&self) {} }
pub struct Invoice {}
impl Invoice { pub fn save(&self) {} }
pub struct Repo {}
impl Repo { pub fn find(&self, id: i32) -> Order { Order {} } }
`,
			"src/app.rs": `use crate::models::Repo;
pub fn run(repo: &Repo) {
    let o = repo.find(1);
    o.save();
}
`}, "src/app.rs:run", "src/models.rs:Order.save"},
		"php": {map[string]string{
			"src/Models.php": `<?php
namespace Shop;
class Order { public function save() {} }
class Invoice { public function save() {} }
class Repo { public function find(int $id): Order { return new Order(); } }
`,
			"src/App.php": `<?php
namespace Shop;
class App {
  public function run(Repo $repo) {
    $o = $repo->find(1);
    $o->save();
  }
}
`}, "src/App.php:App.run", "src/Models.php:Order.save"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := langEdges(t, c.files)
			found := false
			for _, e := range got {
				if !strings.HasSuffix(e.Src, c.run) || e.Kind != model.EdgeCalls {
					continue
				}
				if strings.Contains(e.Dst, "Invoice") {
					t.Errorf("bound to the other class: %v", e)
				}
				if strings.HasSuffix(e.Dst, c.save) {
					found = true
					if !e.Confidence.AtLeast(model.ConfidenceHigh) || !strings.Contains(e.Reason, "the call that assigned the receiver returns") {
						t.Errorf("not bound through the return type at high: %v", e)
					}
				}
			}
			if !found {
				t.Errorf("no edge %s -> %s in %v", c.run, c.save, got)
			}
		})
	}
}
