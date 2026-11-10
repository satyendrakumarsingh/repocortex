package parse

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// arityOf parses src and returns the parameter count recorded for the named
// symbol, plus whether that symbol was found at all.
func arityOf(t *testing.T, file, src, name string) (model.Arity, bool) {
	t.Helper()
	p := NewRegistry().ForFile(file)
	if p == nil {
		t.Fatalf("no parser for %s", file)
	}
	res, err := p.Parse(context.Background(), file, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Symbols {
		if s.Name == name {
			return s.ParamCount, true
		}
	}
	return model.ArityUnknown, false
}

// argsOf returns the argument count recorded for the first reference of name.
func argsOf(t *testing.T, file, src, name string) (model.Arity, bool) {
	t.Helper()
	p := NewRegistry().ForFile(file)
	if p == nil {
		t.Fatalf("no parser for %s", file)
	}
	res, err := p.Parse(context.Background(), file, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.References {
		if r.Name == name {
			return r.ArgCount, true
		}
	}
	return model.ArityUnknown, false
}

// TestGoParameterCountsCountNamesNotNodes is the one shape a naive
// implementation gets wrong.
//
// `func f(a, b int)` is ONE parameter_declaration node carrying two `name:`
// children, so counting list children reports an arity of 1 for a function of
// arity 2 — and a wrong arity is worse than none, because it is evidence
// pointing at the wrong candidate. Probed against the grammar rather than
// assumed; the shapes are in the arityDialect doc.
func TestGoParameterCountsCountNamesNotNodes(t *testing.T) {
	const src = `package p

func none() {}
func one(a int) {}
func grouped(a, b int, c string) {}
func unnamed(int, string) error { return nil }

type T struct{}

func (t *T) Method(x, y int) {}
func (t T) Niladic() {}
`
	for _, tc := range []struct {
		name string
		want int
	}{
		{"none", 0},
		{"one", 1},
		{"grouped", 3},
		{"unnamed", 2},
		{"Method", 2},
		{"Niladic", 0},
	} {
		got, ok := arityOf(t, "t.go", src, tc.name)
		if !ok {
			t.Errorf("%s: symbol not found", tc.name)
			continue
		}
		if !got.Known() {
			t.Errorf("%s: arity unknown, want %d", tc.name, tc.want)
			continue
		}
		if got.Count() != tc.want {
			t.Errorf("%s: arity %d, want %d", tc.name, got.Count(), tc.want)
		}
	}
}

// TestVariadicArityIsUnknownNotACount pins the refusal that keeps A4 out of the
// class of change A19 belongs to.
//
// A variadic signature accepts any number of arguments, so comparing it to a
// call site is meaningless in BOTH directions. Reporting the declared count
// would make `v(1, 2, 3)` look like it disagrees with `func v(xs ...int)`, and
// an arity rule that treated disagreement as evidence would then prefer the
// wrong candidate. Unknown is the only honest answer.
func TestVariadicArityIsUnknownNotACount(t *testing.T) {
	const src = `package p

func variadic(xs ...int) {}
func mixed(a int, xs ...string) {}
`
	for _, name := range []string{"variadic", "mixed"} {
		got, ok := arityOf(t, "t.go", src, name)
		if !ok {
			t.Fatalf("%s: symbol not found", name)
		}
		if got.Known() {
			t.Errorf("%s: arity reported as %d, want unknown", name, got.Count())
		}
	}
}

// TestZeroArgumentsIsNotUnknown is the distinction the whole encoding exists
// for.
//
// A18 broke every chained Go call by reading "the query could not express a
// receiver" as "the source wrote none". A count of zero and no count are the
// same failure one field over: `Close()` genuinely passes nothing, and that is
// evidence narrowing candidates, while a call the parser did not count is
// evidence of nothing at all.
func TestZeroArgumentsIsNotUnknown(t *testing.T) {
	const src = `package p

func run() {
	Close()
	Save(1, 2)
	xs := []int{}
	Spread(xs...)
}
`
	got, ok := argsOf(t, "t.go", src, "Close")
	if !ok {
		t.Fatal("Close reference not found")
	}
	if !got.Known() || got.Count() != 0 {
		t.Errorf("Close(): arity %v, want a KNOWN count of 0", got)
	}

	got, ok = argsOf(t, "t.go", src, "Save")
	if !ok {
		t.Fatal("Save reference not found")
	}
	if !got.Known() || got.Count() != 2 {
		t.Errorf("Save(1, 2): arity %v, want a known count of 2", got)
	}

	// A spread passes an unknown number of arguments.
	got, ok = argsOf(t, "t.go", src, "Spread")
	if !ok {
		t.Fatal("Spread reference not found")
	}
	if got.Known() {
		t.Errorf("Spread(xs...): arity reported as %d, want unknown", got.Count())
	}
}

// TestChainedCallStillGetsAnArgumentCount is the end-to-end shape A18 got
// wrong, checked on the field A4 adds.
//
// receiverQueries capture only a SIMPLE IDENTIFIER receiver, so `s.repo.Save(id)`
// arrives with an EMPTY receiver — which is why an empty receiver may never be
// read as "the source wrote none". The argument query is anchored on the
// call_expression instead of on the receiver, so it must still count a chained
// call's arguments.
func TestChainedCallStillGetsAnArgumentCount(t *testing.T) {
	const src = `package p

func run(s S) {
	s.repo.Save(1, 2)
}
`
	got, ok := argsOf(t, "t.go", src, "Save")
	if !ok {
		t.Fatal("Save reference not found")
	}
	if !got.Known() || got.Count() != 2 {
		t.Errorf("s.repo.Save(1, 2): arity %v, want a known count of 2", got)
	}
}

// TestLanguagesWithoutAnArityQueryReportUnknown pins that this is additive.
//
// A language with no query must report unknown rather than zero,
// so the resolver ranks it exactly as it did before.
func TestLanguagesWithoutAnArityQueryReportUnknown(t *testing.T) {
	got, ok := argsOf(t, "t.py", "def run():\n    db.query('x')\n", "query")
	if !ok {
		t.Fatal("query reference not found")
	}
	if got.Known() {
		t.Errorf("python args: arity reported as %d, want unknown", got.Count())
	}

	got, ok = arityOf(t, "t.py", "def run(a):\n    pass\n", "run")
	if !ok {
		t.Fatal("run symbol not found")
	}
	if got.Known() {
		t.Errorf("python params: arity reported as %d, want unknown", got.Count())
	}
}

// TestJavaParameterCounts checks method and constructor parameter counts.
func TestJavaParameterCounts(t *testing.T) {
	const src = `package com.example;

public class Service {
    public Service() {}
    public Service(String config) {}

    public void zero() {}
    public void one(int a) {}
    public void two(int a, String b) {}
    public void three(int a, String b, double c) {}
}
`
	for _, tc := range []struct {
		name string
		want int
	}{
		{"zero", 0},
		{"one", 1},
		{"two", 2},
		{"three", 3},
	} {
		got, ok := arityOf(t, "Service.java", src, tc.name)
		if !ok {
			t.Errorf("%s: symbol not found", tc.name)
			continue
		}
		if !got.Known() {
			t.Errorf("%s: arity unknown, want %d", tc.name, tc.want)
			continue
		}
		if got.Count() != tc.want {
			t.Errorf("%s: arity %d, want %d", tc.name, got.Count(), tc.want)
		}
	}

	// Constructors also declare parameter lists and are captured with arity.
	got, ok := arityOf(t, "Service.java", "class Service { Service(String a, int b) {} }", "Service")
	if !ok {
		t.Fatal("constructor symbol not found")
	}
	if !got.Known() || got.Count() != 2 {
		t.Errorf("Service constructor: arity %v, want 2", got)
	}
}

// TestJavaArgumentCounts checks method invocation argument counts.
func TestJavaArgumentCounts(t *testing.T) {
	const src = `package com.example;

class Client {
    void execute(Service s) {
        s.zero();
        s.one(42);
        s.two(1, "hello");
        s.three(1, "hello", 3.14);
        bareCall(10, 20);
    }
}
`
	for _, tc := range []struct {
		name string
		want int
	}{
		{"zero", 0},
		{"one", 1},
		{"two", 2},
		{"three", 3},
		{"bareCall", 2},
	} {
		got, ok := argsOf(t, "Client.java", src, tc.name)
		if !ok {
			t.Errorf("%s: reference not found", tc.name)
			continue
		}
		if !got.Known() {
			t.Errorf("%s: arity unknown, want %d", tc.name, tc.want)
			continue
		}
		if got.Count() != tc.want {
			t.Errorf("%s: arity %d, want %d", tc.name, got.Count(), tc.want)
		}
	}
}

// TestTypeScriptParameterCounts checks function and method parameter counts.
func TestTypeScriptParameterCounts(t *testing.T) {
	const src = `
function fn0(): void {}
function fn1(a: number): void {}
function fn2(a: number, b: string): void {}

class Calculator {
    method0(): void {}
    method1(x: number): void {}
    method2(x: number, y: string): void {}
}
`
	for _, tc := range []struct {
		name string
		want int
	}{
		{"fn0", 0},
		{"fn1", 1},
		{"fn2", 2},
		{"method0", 0},
		{"method1", 1},
		{"method2", 2},
	} {
		got, ok := arityOf(t, "app.ts", src, tc.name)
		if !ok {
			t.Errorf("%s: symbol not found", tc.name)
			continue
		}
		if !got.Known() {
			t.Errorf("%s: arity unknown, want %d", tc.name, tc.want)
			continue
		}
		if got.Count() != tc.want {
			t.Errorf("%s: arity %d, want %d", tc.name, got.Count(), tc.want)
		}
	}
}

// TestTypeScriptArgumentCounts checks direct and member call argument counts.
func TestTypeScriptArgumentCounts(t *testing.T) {
	const src = `
function run(calc: Calculator) {
    fn0();
    fn1(42);
    fn2(42, "hello");
    calc.method0();
    calc.method1(10);
    calc.method2(10, "world");
}
`
	for _, tc := range []struct {
		name string
		want int
	}{
		{"fn0", 0},
		{"fn1", 1},
		{"fn2", 2},
		{"method0", 0},
		{"method1", 1},
		{"method2", 2},
	} {
		got, ok := argsOf(t, "client.ts", src, tc.name)
		if !ok {
			t.Errorf("%s: reference not found", tc.name)
			continue
		}
		if !got.Known() {
			t.Errorf("%s: arity unknown, want %d", tc.name, tc.want)
			continue
		}
		if got.Count() != tc.want {
			t.Errorf("%s: arity %d, want %d", tc.name, got.Count(), tc.want)
		}
	}
}

// TestJavaVariadicArityIsUnknown verifies that varargs methods are reported as unknown.
func TestJavaVariadicArityIsUnknown(t *testing.T) {
	const src = `package com.example;

class Varargs {
    void varOnly(String... items) {}
    void mixed(int a, String... items) {}
}
`
	for _, name := range []string{"varOnly", "mixed"} {
		got, ok := arityOf(t, "Varargs.java", src, name)
		if !ok {
			t.Fatalf("%s: symbol not found", name)
		}
		if got.Known() {
			t.Errorf("%s: arity reported as %d, want unknown", name, got.Count())
		}
	}
}

// TestTypeScriptVariadicArityIsUnknown verifies rest parameters and spread arguments report unknown.
func TestTypeScriptVariadicArityIsUnknown(t *testing.T) {
	const src = `
function varFunc(...args: string[]): void {}
function mixedVarFunc(first: number, ...rest: string[]): void {}

class VarClass {
    varMethod(...args: any[]): void {}
}

function caller(xs: string[]) {
    spreadCall(...xs);
}
`
	for _, name := range []string{"varFunc", "mixedVarFunc", "varMethod"} {
		got, ok := arityOf(t, "var.ts", src, name)
		if !ok {
			t.Fatalf("%s: symbol not found", name)
		}
		if got.Known() {
			t.Errorf("%s: arity reported as %d, want unknown", name, got.Count())
		}
	}

	got, ok := argsOf(t, "var.ts", src, "spreadCall")
	if !ok {
		t.Fatal("spreadCall reference not found")
	}
	if got.Known() {
		t.Errorf("spreadCall(...xs): arity reported as %d, want unknown", got.Count())
	}
}
