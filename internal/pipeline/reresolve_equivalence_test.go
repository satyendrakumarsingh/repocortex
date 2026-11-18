package pipeline

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// The incremental equivalence harness.
//
// Each case indexes a tree in full and resolves it, applies a change on disk,
// and brings the index up to date the way `repocortex index` does
// (Diff, then Incremental with Resolve). The edges that leaves must be the
// edges a full index of the changed tree has, compared as (kind, confidence,
// source FQN, target FQN) with the files they live in. A second pass checks
// the repair path: an index left partly stale by an incremental pass that did
// not resolve must come out the same after `resolve --re-resolve`
// (ResetHeuristicResolutions, then Resolve).
//
// The cases are the ones an adversarial review found (7a to 7e), plus the ones
// it found already working, kept as regression guards.

// equivOp is one change to a tree.
type equivOp struct {
	write, remove, renameTo string
	body                    string
}

func writeOp(path, body string) equivOp { return equivOp{write: path, body: body} }
func removeOp(path string) equivOp      { return equivOp{remove: path} }
func renameOp(from, to string) equivOp  { return equivOp{remove: from, renameTo: to} }

// mixTree is the multi-language fixture the review used.
var mixTree = map[string]string{
	"c/a.h":   "int helper(int x);\nvoid run(void);\n",
	"c/a.c":   "#include \"a.h\"\nstatic int util(int x){ return x+1; }\nint helper(int x){ return util(x); }\nvoid run(void){ helper(2); }\n",
	"c/b.c":   "#include \"a.h\"\nstatic int util(int x){ return x*2; }\nvoid other(void){ helper(1); util(3); run(); }\n",
	"c/c.c":   "static int util(int x){ return x*3; }\nint helper(int x){ return util(x); }\n",
	"c/d.cpp": "#include \"a.h\"\nvoid d(){ helper(3); }\n",

	"dart/main.dart":       "import 'lib/lib.dart';\nvoid main(){ helper(); var t = Thing(); t.run(); p(); }\n",
	"dart/lib/lib.dart":    "library lib;\nexport 'src/a.dart';\nexport 'src/b.dart' hide Hidden;\npart 'src/p1.dart';\npart 'src/p2.dart';\n",
	"dart/lib/src/a.dart":  "class Thing { void run(){} }\nvoid helper(){}\n",
	"dart/lib/src/b.dart":  "class Thing { void run(){} }\nclass Hidden {}\nvoid helper(){}\n",
	"dart/lib/src/p1.dart": "part of '../lib.dart';\nvoid p(){ helper(); Thing().run(); }\n",
	"dart/lib/src/p2.dart": "part of '../lib.dart';\nvoid p(){ helper(); }\n",
	"elixir/a.ex":          "defmodule A do\n  def go(x), do: B.go(x)\n  def go(x, y), do: x + y\nend\ndefmodule B do\n  def go(x), do: A.go(x, 1)\nend\n",
	"elixir/b.ex":          "defmodule B do\n  def go(x), do: x\nend\ndefmodule C.A do\n  def go(x), do: A.go(x)\nend\n",
	"java/Base.java":       "abstract class Base { public void m(){} public abstract void n(); }\n",
	"java/I.java":          "interface I { default void m(){} void n(); }\n",
	"java/Impl.java":       "class Impl extends Base implements I { public void n(){} void t(){ m(); n(); } }\nclass U { void u(Impl i){ i.m(); i.n(); } }\n",
	"objc/Bar.m":           "#import \"Foo.h\"\nvoid use(void){ Foo *f = [Foo make]; [f bar]; [f baz]; }\n",
	"objc/Foo+Cat.m":       "#import \"Foo.h\"\n@implementation Foo (Cat)\n- (void)baz { }\n@end\n",
	"objc/Foo.h":           "@interface Foo : NSObject\n- (void)bar;\n+ (Foo*)make;\n@end\n",
	"objc/Foo.m":           "#import \"Foo.h\"\n@implementation Foo\n- (void)bar { [self baz]; }\n- (void)baz {}\n+ (Foo*)make { return [Foo new]; }\n@end\n",
	"perl/A.pm":            "package A;\nsub new { bless {}, shift }\nsub run { my $s = shift; $s->helper; helper(); }\nsub helper { 1 }\n1;\n",
	"perl/B.pm":            "package B;\nuse A;\nsub run { A::helper(); A->new->run; my $x = A->new; $x->run }\nsub helper { 2 }\n1;\n",
	"ps/a.ps1":             "function Get-Thing { 1 }\nfunction get-thing { 2 }\nfunction Out-Default { 3 }\nGet-Thing\nGET-THING\nout-default\nGet-ChildItem\n",
	"ps/b.ps1":             "function Get-ChildItem { 9 }\n. ./a.ps1\nget-thing\nGET-CHILDITEM\n",
	"rr/DESCRIPTION":       "Package: mypkg\nVersion: 1.0\n",
	"rr/R/a.R":             "f <- function(x) g(x)\ng <- function(x) mypkg::h(x)\n",
	"rr/R/b.R":             "h <- function(x) x\ng <- function(x) 2\n",
}

// returnsTree is the return-type fixture (r1).
var returnsTree = map[string]string{
	"Customer.java": "class Customer {\n    void save() {}\n    void notifyAll2() {}\n}\n",
	"Order.java":    "class Order {\n    void save() {}\n    void ship() {}\n}\n",
	"Repo.java": "class Repo {\n    Order find(int id) { return null; }\n    Customer find(String name) { return null; }\n" +
		"    Order make() { return null; }\n    Customer other() { return null; }\n}\n",
	"Use.java": "class Use {\n" +
		"    void a(Repo repo) {\n        var o = repo.make();\n        o = repo.other();\n        o.save();\n    }\n" +
		"    void b(Repo repo, String s) {\n        var o = repo.find(s);\n        o.save();\n    }\n" +
		"    void c(Repo repo) {\n        var o = repo.make();\n        if (true) {\n            var o2 = repo.other();\n            o2.save();\n        }\n        o.save();\n    }\n" +
		"    void d(Repo repo) {\n        var o = repo.make();\n        for (int i = 0; i < 3; i++) {\n            o.save();\n            o = repo.other();\n        }\n    }\n" +
		"    void e(Repo repo, Customer o) {\n        var x = repo.make();\n        Runnable r = () -> { var x2 = repo.other(); x2.save(); };\n        x.save();\n    }\n}\n",
	"t.ts": "class Order { save(): void {} }\nclass Customer { save(): void {} }\nclass Repo {\n    make(): Order { return new Order(); }\n" +
		"    other(): Customer { return new Customer(); }\n}\nfunction a(repo: Repo) {\n    let o = repo.make();\n    o = repo.other() as any;\n    o.save();\n}\n" +
		"function b(repo: Repo) {\n    let o = repo.make();\n    if (Math.random() > 0.5) { o = repo.other(); }\n    o.save();\n}\n",
}

// hierarchyTree is the inheritance fixture (j1), one declaration per line.
var hierarchyTree = map[string]string{
	"Base.java": "abstract class Base {\n    public void m() {}\n    public abstract void n();\n}\n",
	"I.java":    "interface I {\n    default void m() {}\n    void n();\n}\n",
	"Impl.java": "class Impl extends Base implements I {\n    public void n() {}\n    void t() {\n        m();\n        n();\n    }\n}\n",
	"U.java":    "class U {\n    void u(Impl i) {\n        i.m();\n        i.n();\n    }\n}\n",
}

var equivalenceCases = []struct {
	name string
	tree map[string]string
	ops  []equivOp
}{
	{
		// 7a: names a new file defines were referenced, unresolved, before it existed.
		name: "7a add files defining referenced names",
		tree: map[string]string{
			"a.py":     "from b import helper\ndef run():\n    helper()\n    later()\n",
			"Use.java": "class Use {\n    void u(Later l) {\n        l.go();\n    }\n}\n",
		},
		ops: []equivOp{
			writeOp("b.py", "def helper():\n    pass\ndef later():\n    pass\n"),
			writeOp("Later.java", "class Later {\n    void go() {}\n}\n"),
		},
	},
	{
		name: "7b remove a Dart barrel export",
		tree: mixTree,
		ops: []equivOp{writeOp("dart/lib/lib.dart",
			"library lib;\nexport 'src/b.dart' hide Hidden;\npart 'src/p1.dart';\npart 'src/p2.dart';\n")},
	},
	{
		name: "7c delete an R DESCRIPTION",
		tree: mixTree,
		ops:  []equivOp{removeOp("rr/DESCRIPTION")},
	},
	{
		name: "7d change a return type",
		tree: returnsTree,
		ops: []equivOp{writeOp("Repo.java", strings.Replace(returnsTree["Repo.java"],
			"Order make()", "Customer make()", 1))},
	},
	{
		name: "7e delete the base class (mix)",
		tree: mixTree,
		ops:  []equivOp{removeOp("java/Base.java")},
	},
	{
		name: "7e delete the base class (j1)",
		tree: hierarchyTree,
		ops:  []equivOp{removeOp("Base.java")},
	},
	{
		// An unchanged class gains an ancestry through a new file: Impl's
		// `extends Base` binds only once Base exists, and Base extends Root,
		// whose member r() U reaches through Impl. U names nothing the new
		// file defines, so only the moved ancestry can select it.
		name: "add a base class that has its own ancestor",
		tree: map[string]string{
			"Root.java": "class Root {\n    void r() {}\n}\n",
			"I.java":    hierarchyTree["I.java"],
			"Impl.java": hierarchyTree["Impl.java"],
			"U.java":    "class U {\n    void u(Impl i) {\n        i.r();\n    }\n}\n",
		},
		ops: []equivOp{writeOp("Base.java", "abstract class Base extends Root {\n    public void m() {}\n    public abstract void n();\n}\n")},
	},
	{
		name: "change which class an unchanged caller's class extends",
		tree: map[string]string{
			"Base.java":  hierarchyTree["Base.java"],
			"Base2.java": "abstract class Base2 {\n    public void k() {}\n}\n",
			"I.java":     hierarchyTree["I.java"],
			"Impl.java":  hierarchyTree["Impl.java"],
			"U.java":     "class U {\n    void u(Impl i) {\n        i.m();\n        i.k();\n    }\n}\n",
		},
		ops: []equivOp{writeOp("Impl.java", strings.Replace(hierarchyTree["Impl.java"], "extends Base ", "extends Base2 ", 1))},
	},
	// Regression guards: these converged before the change.
	{
		name: "rename a Perl package",
		tree: mixTree,
		ops:  []equivOp{writeOp("perl/A.pm", strings.Replace(mixTree["perl/A.pm"], "package A;", "package AA;", 1))},
	},
	{
		name: "rename a C header",
		tree: mixTree,
		ops:  []equivOp{renameOp("c/a.h", "c/z.h")},
	},
	{
		name: "delete an Objective-C category file",
		tree: mixTree,
		ops:  []equivOp{removeOp("objc/Foo+Cat.m")},
	},
	{
		name: "add a class with the same method name",
		tree: returnsTree,
		ops:  []equivOp{writeOp("Extra.java", "class Extra {\n    void save() {}\n}\n")},
	},
	{
		name: "edit a body only",
		tree: returnsTree,
		ops:  []equivOp{writeOp("Order.java", "class Order {\n    void save() { int x = 1; }\n    void ship() {}\n}\n")},
	},
}

// applyOps changes a tree map the way ops change a directory.
func applyOps(tree map[string]string, ops []equivOp) map[string]string {
	out := make(map[string]string, len(tree)+len(ops))
	for k, v := range tree {
		out[k] = v
	}
	for _, op := range ops {
		switch {
		case op.write != "":
			out[op.write] = op.body
		case op.renameTo != "":
			out[op.renameTo] = out[op.remove]
			delete(out, op.remove)
		default:
			delete(out, op.remove)
		}
	}
	return out
}

func (h *harness) apply(ops []equivOp) {
	h.t.Helper()
	for _, op := range ops {
		switch {
		case op.write != "":
			h.write(op.write, op.body)
		case op.renameTo != "":
			h.rename(op.remove, op.renameTo)
		default:
			h.remove(op.remove)
		}
	}
}

func (h *harness) resolveAll() {
	h.t.Helper()
	if _, err := Resolve(context.Background(), h.reg, h.runID, ResolveOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

// edgeSet is the run's edges as (kind, confidence, source, target), each end
// named by its qualified name and file.
func (h *harness) edgeSet() []string {
	h.t.Helper()
	rows, err := h.db.QueryContext(context.Background(), `
		SELECT e.kind, e.confidence, s.fqn || '@' || sf.path, d.fqn || '@' || df.path
		  FROM edges e
		  JOIN symbols s ON s.id = e.src_id JOIN files sf ON sf.id = s.file_id
		  JOIN symbols d ON d.id = e.dst_id JOIN files df ON df.id = d.file_id
		 WHERE e.index_run_id = ?`, h.runID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]struct{}{}
	for rows.Next() {
		var k, c, s, d string
		if err := rows.Scan(&k, &c, &s, &d); err != nil {
			h.t.Fatal(err)
		}
		seen[k+" "+c+" "+s+" -> "+d] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

func (h *harness) syncResolving() IncrementalStats {
	h.t.Helper()
	ctx := context.Background()
	changes, err := Diff(ctx, h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		h.t.Fatal(err)
	}
	stats, err := Incremental(ctx, h.reg, h.runID, h.walker, changes, IncrementalOptions{Resolve: true})
	if err != nil {
		h.t.Fatal(err)
	}
	return stats
}

func diffEdges(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := map[string]bool{}, map[string]bool{}
	for _, e := range got {
		g[e] = true
	}
	for _, e := range want {
		w[e] = true
	}
	var extra, missing []string
	for _, e := range got {
		if !w[e] {
			extra = append(extra, e)
		}
	}
	for _, e := range want {
		if !g[e] {
			missing = append(missing, e)
		}
	}
	if len(extra)+len(missing) > 0 {
		t.Errorf("%s: %d edges, a full index has %d\n  stale (not in the full index):\n    %s\n  missing (in the full index):\n    %s",
			label, len(got), len(want), strings.Join(extra, "\n    "), strings.Join(missing, "\n    "))
	}
}

// TestIncrementalEquivalence: an incremental index converges to a full one.
func TestIncrementalEquivalence(t *testing.T) {
	for _, tc := range equivalenceCases {
		t.Run(tc.name, func(t *testing.T) {
			full := newHarness(t, applyOps(tc.tree, tc.ops))
			full.resolveAll()
			want := full.edgeSet()

			inc := newHarness(t, tc.tree)
			inc.resolveAll()
			inc.apply(tc.ops)
			stats := inc.syncResolving()
			diffEdges(t, "incremental index", inc.edgeSet(), want)

			// What `repocortex resolve` runs after the index: nothing may move.
			inc.resolveAll()
			diffEdges(t, "incremental index then resolve", inc.edgeSet(), want)

			if total := int64(len(inc.filePaths())); stats.ReResolved > total {
				t.Errorf("re-resolved %d files of %d", stats.ReResolved, total)
			}
		})
	}
}

// TestReResolveRepairsEveryOutcome: `resolve --re-resolve` over an index an
// incremental pass left stale (it ran without resolving, so only the changed
// files' references were pending) produces the full index's edges. It reset
// only 'resolved' references, so an unresolved one was never retried.
func TestReResolveRepairsEveryOutcome(t *testing.T) {
	for _, tc := range equivalenceCases {
		t.Run(tc.name, func(t *testing.T) {
			full := newHarness(t, applyOps(tc.tree, tc.ops))
			full.resolveAll()
			want := full.edgeSet()

			inc := newHarness(t, tc.tree)
			inc.resolveAll()
			inc.apply(tc.ops)
			inc.sync()
			inc.resolveAll()
			if _, _, err := inc.reg.ResetHeuristicResolutions(context.Background(), inc.runID); err != nil {
				t.Fatal(err)
			}
			inc.resolveAll()
			diffEdges(t, "re-resolve", inc.edgeSet(), want)
		})
	}
}

// TestIncrementalReResolvesOnlyWhatChanged: a body-only edit re-resolves the
// edited file and the files bound into it, not the repository.
func TestIncrementalReResolvesOnlyWhatChanged(t *testing.T) {
	tree := map[string]string{
		"a.go": "package p\n\nfunc A() { B() }\n",
		"b.go": "package p\n\nfunc B() {}\n",
	}
	for i := 0; i < 20; i++ {
		tree["u"+string(rune('a'+i))+".go"] = "package p\n\nfunc U" + string(rune('A'+i)) + "() {}\n"
	}
	h := newHarness(t, tree)
	h.resolveAll()
	h.write("b.go", "package p\n\nfunc B() { _ = 1 }\n")
	stats := h.syncResolving()
	if stats.ReResolved != 2 {
		t.Errorf("re-resolved %d files after a body edit to b.go, want 2 (b.go and its caller a.go)", stats.ReResolved)
	}
}
