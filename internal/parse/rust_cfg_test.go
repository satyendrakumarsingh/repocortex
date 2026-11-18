package parse

import "testing"

// A Rust item under #[cfg(...)], itself or through what encloses it, is marked
// conditional; a method of an impl for a reference type is marked as one.
func TestRustConditionalAndReferenceImpls(t *testing.T) {
	res := parse(t, "src/lib.rs", `#[cfg(feature = "std")]
mod imp {
    pub struct Counter {}
    impl Counter {
        /// doc between the attribute and the item
        pub fn dec(&self) {}
    }
}

#[cfg_attr(feature = "std", derive(Debug))]
pub struct Plain {}

impl Plain {
    #[cfg(unix)]
    #[inline]
    pub fn tick(&self) {}
    pub fn tock(&self) {}
}

pub struct Bag {}
impl<'a> IntoIterator for &'a Bag {
    type Item = i32;
    type IntoIter = std::vec::IntoIter<i32>;
    fn into_iter(self) -> Self::IntoIter { unimplemented!() }
}
impl IntoIterator for Bag {
    type Item = i32;
    type IntoIter = std::vec::IntoIter<i32>;
    fn into_iter(self) -> Self::IntoIter { unimplemented!() }
}
`)
	type flags struct{ conditional, refImpl bool }
	got := map[string][]flags{}
	for _, s := range res.Symbols {
		got[s.Name] = append(got[s.Name], flags{s.Conditional, s.RefImpl})
	}
	check := func(name string, want ...flags) {
		t.Helper()
		g := got[name]
		if len(g) != len(want) {
			t.Fatalf("%s: %d definitions, want %d (%v)", name, len(g), len(want), got)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Errorf("%s #%d: %+v, want %+v", name, i, g[i], want[i])
			}
		}
	}
	check("dec", flags{conditional: true})
	check("Counter", flags{conditional: true})
	check("tick", flags{conditional: true})
	check("tock", flags{})
	check("Plain", flags{})
	check("into_iter", flags{refImpl: true}, flags{})
}
