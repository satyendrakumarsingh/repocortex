package infer

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// # Rust: a variable's name against what it was assigned
//
// `let mut class = self.convert_unicode_class_error(..)?;` and then
// `class.negate()`: the receiver is named `class`, the repository has a type
// `Class` with a `negate`, and the edge was bound to it at medium. The call that
// assigned `class` is declared to return `Result<hir::ClassUnicode>`, which is
// what `class` holds once `?` has unwrapped it. The declaration says what the
// variable is; the name only resembles a type. Measured against rust-analyzer
// on regex-syntax, 2 of the 8 edges this rule made on a declared variable were
// wrong, and every one had a producer whose return type was a wrapper.
//
// A return type that is a type of the repository already types the variable
// (callReturned). One that is not — `Result`, `Option`, a type of another
// crate — types it too, as something this index cannot name, and the name of the
// variable is not evidence against that. The call is then left to the rules that
// do not read the name.
func rustReturnWins(c call) bool {
	if c.spec.Name != "rust" || c.ref.ValueLine == 0 || c.produced == nil {
		return false
	}
	producer, ok := c.produced[[2]int{c.ref.ValueLine, c.ref.ValueCol}]
	return ok && producer.entry.ReturnType != ""
}

// rustValueNameTier is the tier of a variable named after a type when its
// declaration says nothing about what it holds. Medium was the hint's cap;
// measured on regex-syntax and sqlparser, 10 right of 15 (67%), the wrong ones
// `query.span()` on a `Select` returned by `parse_select().unwrap()` and
// `class.negate()` on a `ClassBytes`. A tier that wrong is not `medium`. A name
// no declaration binds (a closure parameter, a loop variable) measured 35 of 35
// and keeps it.
func rustValueNameTier(c call, tier model.Confidence) model.Confidence {
	if c.spec.Name == "rust" && c.ref.ReceiverKind == model.ReceiverValue {
		return weaker(tier, model.ConfidenceLow)
	}
	return tier
}
