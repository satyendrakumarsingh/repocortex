package infer

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// # Rust: alternatives the source leaves to something else
//
// Two definitions of one qualified name that are not one definition twice.
//
// # #[cfg(...)]
//
// `#[cfg(feature = "std")] mod recursion { impl RecursionCounter { fn
// try_decrease } }` and `#[cfg(not(feature = "std"))] mod recursion { ... }`
// are two definitions of one qualified name, of which a build keeps one. The
// index holds both, and a call that reaches that name reaches both: choosing the
// first is a guess about the build, and measured against rust-analyzer it was
// the wrong one on sqlparser (`try_decrease`, three calls).
//
// # The same trait for T and for &T
//
// `impl<'a, T> IntoIterator for &'a Wrapper<T>` and `impl<T> IntoIterator for
// Wrapper<T>` are two `into_iter` of one qualified name, told apart only by
// whether the receiver is borrowed. A call on a name whose type is known and
// not whether it is borrowed (`ours.into_iter()` for an owned `ours`) bound to
// the first declared, the borrowed one, in sqlparser.
//
// So a binding to a definition that has such a twin in the repository is held
// at `medium`, and says why. The edge stays: the call does reach that name, and
// the twin is the other half of a choice the source leaves to the build or to
// the receiver. A definition with no twin is bound as any other.
func capConditionalTwins(c call, d decision) decision {
	if c.spec.Name != "rust" || d.verdict != verdictBound || d.best.confidence != model.ConfidenceHigh {
		return d
	}
	best := d.best.entry
	if _, isLocal := localScope(best); isLocal {
		// A function nested in the one the call is written in: the twin is the
		// same nested function of the other alternative, which this call cannot
		// reach.
		return d
	}
	for _, s := range c.cands {
		if s.ID == best.ID || s.FQN != best.FQN {
			continue
		}
		switch {
		case s.Conditional && best.Conditional:
			d.best.confidence = model.ConfidenceMedium
			d.best.reason += " (one of several definitions under #[cfg]; the build picks one)"
			return d
		case s.RefImpl != best.RefImpl:
			d.best.confidence = model.ConfidenceMedium
			d.best.reason += " (the trait is implemented for the type and for a reference to it; the receiver decides)"
			return d
		}
	}
	return d
}
