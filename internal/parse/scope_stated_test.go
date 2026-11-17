package parse

import "testing"

// The class a local holds, where the source says so: `x = Foo.new`,
// `$x = new Foo()`, `let x = Foo::new()`. Ruby and PHP write no declaration, so
// the assignment is read as one; Rust's constructor is the conventional `new`.
func TestRubyStatedReceivers(t *testing.T) {
	res := parse(t, "a.rb", `class A
  def run(items)
    o = Order.new(1)
    o.save
    s = Shop::Store.new
    s.place
    m = load(items)
    m.format
    d = Money.new
    d = Order.new
    d.save
    @iv = Order.new
    @iv.total
    q, r = Order.new, 1
    q.save
  end
end
`)
	wantList(t, "receivers", refRows(res), []string{
		"reference.call format recv=m/value args=?",
		"reference.call load recv=/bare args=1",
		"reference.call new recv=Money/name args=?",
		"reference.call new recv=Order/name args=1",
		"reference.call new recv=Order/name args=?",
		"reference.call new recv=Order/name args=?",
		"reference.call new recv=Order/name args=?",
		"reference.call new recv=Shop::Store/scope args=?",
		"reference.call place recv=Shop::Store/type args=?",
		// d is assigned two classes: the claim is withdrawn, and it stays a value.
		"reference.call save recv=Order/type args=?",
		"reference.call save recv=d/value args=?",
		// q is a destructuring target, and an instance variable is a field.
		"reference.call save recv=q/name args=?",
		"reference.call total recv=@iv/name args=?",
	})
}

func TestPHPStatedReceivers(t *testing.T) {
	res := parse(t, "a.php", `<?php
class A {
    public function run(Money $typed, $loose) {
        $o = new Order(1);
        $o->save();
        $m = load($loose);
        $m->short();
        $typed->add(1);
        $this->repo = new Repo();
        $this->repo->find(1);
    }
}
`)
	wantList(t, "receivers", refRows(res), []string{
		"reference.call add recv=Money/type args=1",
		"reference.call find recv=this.repo/path args=1",
		"reference.call load recv=/bare args=1",
		"reference.call save recv=Order/type args=0",
		"reference.call short recv=m/value args=0",
		"reference.call.new Order recv=/bare args=?",
		"reference.call.new Repo recv=/bare args=?",
	})
}

func TestRustStatedReceivers(t *testing.T) {
	res := parse(t, "a.rs", `fn run(typed: &Money, loose: Thing) {
    let a = Cart::new(1);
    a.save();
    let b = Cart::load(1);
    b.save();
    let c = Cart::new(1)?;
    c.save();
    let d = cart::new(1);
    d.save();
    typed.add(1);
    loose.save();
    let e = Tracker::new().context("open (db)")?;
    e.reset();
    let f = Tracker::new().unwrap();
    f.reset();
    let g = Tracker::new().map(wrap)?;
    g.reset();
}
`)
	wantList(t, "receivers", refRows(res), []string{
		"reference.call add recv=Money/type args=1",
		"reference.call context recv=?/expr args=1",
		"reference.call load recv=Cart/scope args=1",
		"reference.call map recv=?/expr args=1",
		"reference.call new recv=Cart/scope args=1",
		"reference.call new recv=Cart/scope args=1",
		"reference.call new recv=Tracker/scope args=0",
		"reference.call new recv=Tracker/scope args=0",
		"reference.call new recv=Tracker/scope args=0",
		"reference.call new recv=cart/scope args=1",
		"reference.call reset recv=Tracker/type args=0",
		"reference.call reset recv=Tracker/type args=0",
		// `.map(..)` is a tail that changes the type, so g is a value.
		"reference.call reset recv=g/value args=0",
		"reference.call save recv=Cart/type args=0",
		"reference.call save recv=Cart/type args=0",
		"reference.call save recv=Thing/type args=0",
		// b is a call of another function and d is a module's: values.
		"reference.call save recv=b/value args=0",
		"reference.call save recv=d/value args=0",
		"reference.call unwrap recv=?/expr args=0",
	})
}
