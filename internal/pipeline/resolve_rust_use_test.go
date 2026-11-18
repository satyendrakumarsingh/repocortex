package pipeline

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// edgeTo returns the edges from src to dst, matched by suffix.
func edgeTo(got []langEdge, src, dst string) []langEdge {
	var out []langEdge
	for _, e := range got {
		if strings.HasSuffix(e.Src, src) && strings.HasSuffix(e.Dst, dst) {
			out = append(out, e)
		}
	}
	return out
}

// A name a `use` brings in is bound to the module the path names: from a list,
// renamed, through a glob, and as `core::utils::f()` beside a `mod core;`. A
// neighbour of the same name, which the language does not put in scope, loses.
func TestRustUseBringsNamesIntoScope(t *testing.T) {
	got := langEdges(t, map[string]string{
		"Cargo.toml":        "[package]\nname = \"app\"\nversion = \"0.1.0\"\n",
		"src/main.rs":       "mod core;\nmod app;\nfn main() {\n    core::utils::resolved_command();\n}\n",
		"src/core/mod.rs":   "pub mod utils;\npub mod other;\n",
		"src/core/utils.rs": "pub fn resolved_command() {}\npub fn truncate() {}\n",
		"src/core/other.rs": "pub fn truncate() {}\npub fn beta() {}\n",
		"src/app/mod.rs":    "pub mod gain;\npub mod decoy;\n",
		"src/app/decoy.rs":  "pub fn resolved_command() {}\npub fn beta() {}\n",
		"src/app/gain.rs": `use crate::core::other::*;
use crate::core::utils::{resolved_command, truncate as cut};

pub fn run() {
    resolved_command();
    cut();
    beta();
}
`,
	})
	for _, w := range []want{
		{"src/app/gain.rs:run", "src/core/utils.rs:resolved_command", model.ConfidenceHigh},
		{"src/app/gain.rs:run", "src/core/utils.rs:truncate", model.ConfidenceHigh},
		{"src/app/gain.rs:run", "src/core/other.rs:beta", model.ConfidenceHigh},
		{"src/main.rs:main", "src/core/utils.rs:resolved_command", model.ConfidenceHigh},
	} {
		es := edgeTo(got, w.Src, w.Dst)
		if len(es) == 0 {
			t.Errorf("missing edge %s -> %s", w.Src, w.Dst)
			continue
		}
		if !es[0].Confidence.AtLeast(w.Min) {
			t.Errorf("edge %s -> %s is %s (%s), want %s", w.Src, w.Dst, es[0].Confidence, es[0].Reason, w.Min)
		}
	}
	for _, f := range []want{
		{Src: "src/app/gain.rs:run", Dst: "src/app/decoy.rs:resolved_command"},
		{Src: "src/app/gain.rs:run", Dst: "src/app/decoy.rs:beta"},
		{Src: "src/app/gain.rs:run", Dst: "src/core/other.rs:truncate"},
		{Src: "src/main.rs:main", Dst: "src/app/decoy.rs:resolved_command"},
	} {
		if es := edgeTo(got, f.Src, f.Dst); len(es) > 0 {
			t.Errorf("wrong edge %s -> %s (%s %s)", es[0].Src, es[0].Dst, es[0].Confidence, es[0].Reason)
		}
	}
}

// Two definitions of one name under #[cfg], and one trait implemented for a type
// and for a reference to it: the call reaches the name and not one of the two, so
// it is not `high`. A single definition is, as before.
func TestRustAlternativesAreNotHigh(t *testing.T) {
	got := langEdges(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"app\"\nversion = \"0.1.0\"\n",
		"src/lib.rs": `#[cfg(feature = "std")]
mod imp {
    pub struct Counter {}
    impl Counter {
        pub fn dec(&self) {}
    }
}

#[cfg(not(feature = "std"))]
mod imp {
    pub struct Counter {}
    impl Counter {
        pub fn dec(&self) {}
    }
}

use imp::Counter;

pub struct Plain {}
impl Plain {
    pub fn tick(&self) {}
}

pub struct Bag {
    v: Vec<i32>,
}

impl<'a> IntoIterator for &'a Bag {
    type Item = &'a i32;
    type IntoIter = std::slice::Iter<'a, i32>;
    fn into_iter(self) -> Self::IntoIter {
        self.v.iter()
    }
}

impl IntoIterator for Bag {
    type Item = i32;
    type IntoIter = std::vec::IntoIter<i32>;
    fn into_iter(self) -> Self::IntoIter {
        self.v.into_iter()
    }
}

pub struct Gate {
    counter: Counter,
}

impl Gate {
    pub fn go(&self) {
        self.counter.dec();
    }
}

pub fn single(p: &Plain) {
    p.tick();
}

pub fn consume(b: Bag) {
    b.into_iter();
}
`,
	})
	if es := edgeTo(got, "src/lib.rs:single", "Plain.tick"); len(es) != 1 || es[0].Confidence != model.ConfidenceHigh {
		t.Errorf("a call of a method with one definition: %+v, want one edge at high", es)
	}
	if es := edgeTo(got, "src/lib.rs:Gate.go", "Counter.dec"); len(es) == 0 {
		t.Errorf("the call of a cfg-gated method left no edge at all; it is a medium one")
	}
	for _, c := range []struct{ src, dst string }{
		{"src/lib.rs:Gate.go", "Counter.dec"},
		{"src/lib.rs:consume", "Bag.into_iter"},
	} {
		for _, e := range edgeTo(got, c.src, c.dst) {
			if e.Confidence.AtLeast(model.ConfidenceHigh) {
				t.Errorf("edge %s -> %s is %s (%s), want at most medium", e.Src, e.Dst, e.Confidence, e.Reason)
			}
		}
	}
}

// A variable assigned from a call whose declared return type is not one of the
// repository's types is not typed by its name: `class` here holds a
// `ClassUnicode` out of a `Result`, not the `Class` the name resembles.
func TestRustReturnedTypeOutranksTheName(t *testing.T) {
	got := langEdges(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"app\"\nversion = \"0.1.0\"\n",
		"src/lib.rs": `pub struct Class {}
impl Class {
    pub fn negate(&mut self) {}
}

pub struct ClassUnicode {}
impl ClassUnicode {
    pub fn negate(&mut self) {}
}

pub struct Translator {}
impl Translator {
    fn convert(&self) -> Result<ClassUnicode, String> {
        Ok(ClassUnicode {})
    }

    pub fn run(&self) -> Result<(), String> {
        let mut class = self.convert()?;
        class.negate();
        Ok(())
    }
}
`,
	})
	if es := edgeTo(got, "Translator.run", "Class.negate"); len(es) > 0 {
		for _, e := range es {
			if strings.HasSuffix(e.Dst, ":Class.negate") {
				t.Errorf("edge %s -> %s (%s %s): the variable's declared type is not Class", e.Src, e.Dst, e.Confidence, e.Reason)
			}
		}
	}
}
