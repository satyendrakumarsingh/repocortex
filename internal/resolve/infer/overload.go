package infer

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// # Several declarations under one name
//
// A language with overloads lets one type declare `get(Class)`, `get(Type)` and
// `get(String, String, String...)`. Knowing the type and the name then leaves a
// choice, and the first declaration in the file is not the answer to it: it was
// the callee 10 times in 30 on javapoet, 6 in 30 on serilog's `low` tier and 22
// of 30 `medium` edges wrong on moshi, where `toJson(JsonWriter, T)`,
// `toJson(T)` and `toJson(BufferedSink, T)` share a type.
//
// Two facts can narrow the choice, and both come from the parser: the number of
// arguments the call passes (Reference.ArgCount) and the number of parameters a
// definition declares (SymbolIndexEntry.ParamCount). Each is "unknown" for a
// language the parser has no count for and for a signature whose count is not a
// number (a variadic or defaulted one). So the rule is written against the
// data:
//
//   - a declaration is set aside only when BOTH counts are known and differ,
//     because only then is the call impossible for it;
//   - a count that is absent excludes nothing, which makes the whole rule a
//     no-op for a language whose counts are not recorded;
//   - exactly one declaration left is the callee, and the binding keeps the tier
//     of the rule that found the type;
//   - more than one left, or none, is a choice nothing here makes. The first
//     declaration is recorded, at `low` at most, and the reason says that the
//     name is overloaded and which declaration was taken.
//
// This is a filter on candidates, and the note above Resolver.ResolveFile
// explains why such filters were distrusted. This one is applied to the members
// of ONE type in ONE place, after the type is identified, so it can only change
// which of that type's own declarations is named and what tier the binding
// gets. It cannot make an ambiguous reference resolve, and it can only move a
// binding up a tier when the count leaves exactly one declaration.

// narrowByArity returns the declarations the call's argument count allows, in
// the order given. When the call's count is unknown nothing is excluded.
func narrowByArity(ref registry.Reference, group []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	if !ref.ArgCount.Known() {
		return group
	}
	out := make([]registry.SymbolIndexEntry, 0, len(group))
	for _, c := range group {
		if c.ParamCount.Known() && c.ParamCount.Count() != ref.ArgCount.Count() {
			continue
		}
		out = append(out, c)
	}
	return out
}

// earliest is the declaration with the lowest id, which is the first in source
// order within a file.
func earliest(group []registry.SymbolIndexEntry) registry.SymbolIndexEntry {
	first := group[0]
	for _, c := range group[1:] {
		if declaredBefore(c, first) {
			first = c
		}
	}
	return first
}

// overloadChoice is what choosing among the declarations of one name came to.
type overloadChoice struct {
	// first is the declaration the binding names.
	first registry.SymbolIndexEntry
	// chosen is true when the call identifies it: it is the only declaration,
	// or the only one the argument count allows.
	chosen bool
	// declared is how many declarations the group holds, and allowed how many
	// of them the call's argument count leaves possible.
	declared, allowed int
	// differ is true where the language's declarations of one name are
	// different methods (CallTraits.Overloads). Where false, several of them
	// are one function and the first is the definition.
	differ bool
}

// overloaded reports that the group holds several different methods and the
// call does not pick one.
func (o overloadChoice) overloaded() bool { return o.differ && !o.chosen && o.declared > 1 }

// chooseOverload picks the declaration a call to one type's member reaches.
func chooseOverload(ref registry.Reference, group []registry.SymbolIndexEntry) overloadChoice {
	o := overloadChoice{declared: len(group), differ: true}
	remaining := narrowByArity(ref, group)
	o.allowed = len(remaining)
	switch len(remaining) {
	case 1:
		o.first, o.chosen = remaining[0], true
	case 0:
		// Every declaration is impossible for the argument count. The data
		// contradict each other (a miscount, or a callee that is not here), and
		// nothing is excluded on the strength of that.
		o.first = earliest(group)
	default:
		o.first = earliest(remaining)
	}
	return o
}

// overloadedReason states what was taken and why it is a choice. The text up to
// the first comma is the rule's own, so precision can still be grouped by it.
func overloadedReason(rule, owner string, o overloadChoice) string {
	why := fmt.Sprintf("%d declarations of %s", o.declared, o.first.Name)
	switch {
	case o.allowed == 0:
		why += ", none takes the argument count"
	case o.allowed < o.declared:
		why += fmt.Sprintf(", %d allow the argument count", o.allowed)
	}
	where := ""
	if owner != "" {
		where = " on " + owner
	}
	return fmt.Sprintf("%s, overloaded: %s%s; the first (%s) was taken", rule, why, where, o.first.FQN)
}
