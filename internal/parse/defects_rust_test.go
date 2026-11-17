package parse

import (
	"strings"
	"testing"
)

func TestRustMacroInvocationsAreNotCalls(t *testing.T) {
	res := parse(t, "a.rs", `macro_rules! print_error { () => {}; }
fn format() {}
fn main() {
    let v = vec![1];
    let s = format!("{}", 1);
    format();
    print_error!("x");
    std::println!("y");
    obj.format();
}
`)
	wantList(t, "references", refRows(res), []string{
		"reference.call format recv=/bare args=0",
		"reference.call format recv=obj/name args=0",
		"reference.call.macro format recv=/bare args=?",
		"reference.call.macro print_error recv=/bare args=?",
		"reference.call.macro println recv=std/scope args=?",
		"reference.call.macro vec recv=/bare args=?",
	})
}

// A macro_rules! definition is a function by kind, so that a lookup of a
// function by name finds it, and a macro by its qualified name, which is what
// the resolver tells it from an `fn` by (internal/resolve/infer/macro.go).
func TestRustMacroDefinitionIsMarkedByItsQualifiedName(t *testing.T) {
	res := parse(t, "src/a.rs", `macro_rules! print_error { () => {}; }
fn print_error() {}
mod m {
    macro_rules! inner { () => {}; }
}
fn f() {
    macro_rules! local { () => {}; }
}
`)
	got := map[string]string{}
	for _, s := range res.Symbols {
		got[s.FQN] = string(s.Kind)
	}
	if got["src/a.rs:print_error!"] != "function" {
		t.Errorf("the macro has kind %q, want function; all: %v", got["src/a.rs:print_error!"], got)
	}
	bare := 0
	for _, s := range res.Symbols {
		if s.Name == "print_error" && !strings.HasSuffix(s.FQN, "!") {
			bare++
		}
	}
	if bare != 1 {
		t.Errorf("%d definitions of print_error without the marker, want the one fn; all: %v", bare, got)
	}
	for _, s := range res.Symbols {
		if (s.Name == "inner" || s.Name == "local") && !strings.HasSuffix(s.FQN, "!") {
			t.Errorf("macro %s has qualified name %q, want it to end in !", s.Name, s.FQN)
		}
	}
}
