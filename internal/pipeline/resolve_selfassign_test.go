package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestSelfAssignedLocalKeepsItsProducer checks the one reassignment that does
// not withdraw a local's call-return type: `s = s.plus(m)`, where plus is
// declared to return the type s already has (internal/parse/shape.go selfCall,
// internal/resolve/infer/returned.go reassignmentsKeepType). The type is
// unchanged, so `s.save()` is still Money.save, at high.
//
// Every other reassignment still withdraws it: a method that returns another
// type, one whose return type is not read, another variable, a conditional, a
// chain, or an operator-assignment. Money and Other both declare `save`, so
// the name alone binds nothing and a withdrawn producer leaves no edge.
func TestSelfAssignedLocalKeepsItsProducer(t *testing.T) {
	type lang struct {
		name  string
		files map[string]string
		file  string           // the file the cases are written in
		sep   string           // between the class and the case name, "" for free functions
		save  string           // the method the cases call
		min   model.Confidence // what a kept producer binds at
	}
	keep := []string{"keep", "loop"}
	drop := []string{"other", "conv", "unread", "cond", "chain", "viaother"}

	langs := []lang{
		{
			name: "java", file: "U.java", sep: "U.", save: "Money.save", min: model.ConfidenceHigh,
			files: map[string]string{
				"Money.java": "class Money {\n    Money plus(Money o) { return o; }\n    Other conv() { return null; }\n    Optional<Money> opt() { return null; }\n    void save() {}\n}\n",
				"Other.java": "class Other {\n    void save() {}\n}\n",
				"Repo.java":  "class Repo {\n    Money zero() { return null; }\n}\n",
				"U.java": `class U {
    void keep(Repo repo, Money m) {
        var s = repo.zero();
        s = s.plus(m);
        s.save();
    }
    void loop(Repo repo, Money m) {
        var s = repo.zero();
        for (int i = 0; i < 3; i++) {
            s = s.plus(m);
        }
        s.save();
    }
    void other(Repo repo, Money m) {
        var s = repo.zero();
        s = repo.zero();
        s.save();
    }
    void conv(Repo repo) {
        var s = repo.zero();
        s = s.conv();
        s.save();
    }
    void unread(Repo repo) {
        var s = repo.zero();
        s = s.opt();
        s.save();
    }
    void cond(Repo repo, Money m, boolean f) {
        var s = repo.zero();
        s = f ? s.plus(m) : repo.zero();
        s.save();
    }
    void chain(Repo repo, Money m) {
        var s = repo.zero();
        s = s.plus(m).conv();
        s.save();
    }
    void viaother(Repo repo, Money m, Money t) {
        var s = repo.zero();
        s = t.plus(m);
        s.save();
    }
}
`,
			},
		},
		{
			name: "c_sharp", file: "a.cs", sep: "U.", save: "Money.Save", min: model.ConfidenceHigh,
			files: map[string]string{
				"a.cs": `class Money {
    public Money Plus(Money o) { return o; }
    public Other Conv() { return null; }
    public Task<Money> Opt() { return null; }
    public void Save() {}
}
class Other { public void Save() {} }
class Repo { public Money Zero() { return null; } }
class U {
    void keep(Repo repo, Money m) {
        var s = repo.Zero();
        s = s.Plus(m);
        s.Save();
    }
    void loop(Repo repo, Money m) {
        var s = repo.Zero();
        for (int i = 0; i < 3; i++) {
            s = s.Plus(m);
        }
        s.Save();
    }
    void other(Repo repo, Money m) {
        var s = repo.Zero();
        s = repo.Zero();
        s.Save();
    }
    void conv(Repo repo) {
        var s = repo.Zero();
        s = s.Conv();
        s.Save();
    }
    void unread(Repo repo) {
        var s = repo.Zero();
        s = s.Opt();
        s.Save();
    }
    void cond(Repo repo, Money m, bool f) {
        var s = repo.Zero();
        s = f ? s.Plus(m) : repo.Zero();
        s.Save();
    }
    void chain(Repo repo, Money m) {
        var s = repo.Zero();
        s = s.Plus(m).Conv();
        s.Save();
    }
    void viaother(Repo repo, Money m, Money t) {
        var s = repo.Zero();
        s = t.Plus(m);
        s.Save();
    }
}
`,
			},
		},
		{
			name: "swift", file: "a.swift", save: "Money.save", min: model.ConfidenceMedium,
			files: map[string]string{
				"a.swift": `class Money {
    func plus(_ o: Money) -> Money { return o }
    func conv() -> Other { return Other() }
    func opt() -> [Money] { return [] }
    func save() {}
}
class Other { func save() {} }
class Repo { func zero() -> Money { return Money() } }
func keep(repo: Repo, m: Money) {
    var s = repo.zero()
    s = s.plus(m)
    s.save()
}
func loop(repo: Repo, ms: [Money]) {
    var s = repo.zero()
    for m in ms { s = s.plus(m) }
    s.save()
}
func other(repo: Repo, m: Money) {
    var s = repo.zero()
    s = repo.zero()
    s.save()
}
func conv(repo: Repo) {
    var s = repo.zero()
    s = s.conv()
    s.save()
}
func unread(repo: Repo) {
    var s = repo.zero()
    s = s.opt()
    s.save()
}
func cond(repo: Repo, m: Money, f: Bool) {
    var s = repo.zero()
    s = f ? s.plus(m) : repo.zero()
    s.save()
}
func chain(repo: Repo, m: Money) {
    var s = repo.zero()
    s = s.plus(m).conv()
    s.save()
}
func viaother(repo: Repo, m: Money, t: Money) {
    var s = repo.zero()
    s = t.plus(m)
    s.save()
}
`,
			},
		},
		{
			name: "rust", file: "a.rs", save: "Money.save", min: model.ConfidenceHigh,
			files: map[string]string{
				"a.rs": `struct Money;
struct Other;
impl Money {
    fn plus(&self, o: &Money) -> Money { Money }
    fn conv(&self) -> Other { Other }
    fn opt(&self) -> Option<Money> { None }
    fn save(&self) {}
}
impl Other { fn save(&self) {} }
struct Repo;
impl Repo { fn zero(&self) -> Money { Money } }
fn keep(repo: &Repo, m: &Money) {
    let mut s = repo.zero();
    s = s.plus(m);
    s.save();
}
fn r#loop(repo: &Repo, m: &Money) {
    let mut s = repo.zero();
    for _ in 0..3 {
        s = s.plus(m);
    }
    s.save();
}
fn other(repo: &Repo) {
    let mut s = repo.zero();
    s = repo.zero();
    s.save();
}
fn conv(repo: &Repo) {
    let mut s = repo.zero();
    s = s.conv();
    s.save();
}
fn unread(repo: &Repo) {
    let mut s = repo.zero();
    s = s.opt();
    s.save();
}
fn cond(repo: &Repo, m: &Money, f: bool) {
    let mut s = repo.zero();
    s = if f { s.plus(m) } else { repo.zero() };
    s.save();
}
fn chain(repo: &Repo, m: &Money) {
    let mut s = repo.zero();
    s = s.plus(m).conv();
    s.save();
}
fn viaother(repo: &Repo, m: &Money, t: &Money) {
    let mut s = repo.zero();
    s = t.plus(m);
    s.save();
}
`,
			},
		},
		{
			name: "typescript", file: "t.ts", save: "Money.save", min: model.ConfidenceHigh,
			files: map[string]string{
				"t.ts": `class Money {
    plus(o: Money): Money { return o; }
    conv(): Other { return new Other(); }
    opt(): Money | null { return null; }
    save(): void {}
}
class Other { save(): void {} }
class Repo { zero(): Money { return new Money(); } }
function keep(repo: Repo, m: Money) {
    let s = repo.zero();
    s = s.plus(m);
    s.save();
}
function loop(repo: Repo, m: Money) {
    let s = repo.zero();
    for (let i = 0; i < 3; i++) {
        s = s.plus(m);
    }
    s.save();
}
function other(repo: Repo) {
    let s = repo.zero();
    s = repo.zero();
    s.save();
}
function conv(repo: Repo) {
    let s = repo.zero();
    s = s.conv();
    s.save();
}
function unread(repo: Repo) {
    let s = repo.zero();
    s = s.opt();
    s.save();
}
function cond(repo: Repo, m: Money, f: boolean) {
    let s = repo.zero();
    s = f ? s.plus(m) : repo.zero();
    s.save();
}
function chain(repo: Repo, m: Money) {
    let s = repo.zero();
    s = s.plus(m).conv();
    s.save();
}
function viaother(repo: Repo, m: Money, t: Money) {
    let s = repo.zero();
    s = t.plus(m);
    s.save();
}
`,
			},
		},
	}
	for _, l := range langs {
		t.Run(l.name, func(t *testing.T) {
			got := langEdges(t, l.files)
			for _, fn := range keep {
				name := fn
				if l.name == "rust" && fn == "loop" {
					name = "r#loop"
				}
				requireEdge(t, got, l.file+":"+l.sep+name, l.save, l.min)
			}
			for _, fn := range drop {
				forbidEdge(t, got, l.file+":"+l.sep+fn, l.save)
			}
		})
	}
}
