package infer

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Receivers typed by what a call returns
//
// `order = repo.find(id); order.save()` states nothing about `order`, so
// `save` used to be bound by its name alone. When `find` declares that it
// returns an Order, the language has already said what `order` holds. The
// parser records the two halves (migration 0023_return_types): on `save`, the
// position of the `find` call its receiver was initialised from, and on every
// definition the plain type it declares it returns. This file joins them.
//
// THE EDGE RESTS ON TWO BINDINGS: the producing call to its definition, and
// the method to the returned type. Either can be wrong, so the edge is never
// more confident than the weaker of the two, and since neither is a compiler
// fact it is never exact. A producer this pass did not bind (unresolved,
// ambiguous, parked for the ancestry pass, or below the minimum confidence)
// gives the consumer nothing, and the consumer is bound as it always was.
//
// WHAT IT DOES NOT DO.
//   - Chains (`repo.find(id).save()`): only a variable. The parser links a
//     variable to one call whose own receiver is not a call (producerOf), so
//     no binding here ever rests on more than two others.
//   - Inherited members: when the returned type does not itself declare the
//     method, the call falls back to the rules it went through before, rather
//     than being parked for the ancestry pass, whose answer would not carry
//     the producer's confidence.
//   - Override a stated type. A variable whose declaration states a type, or
//     constructs one, is a model.ReceiverType call and never comes here; the
//     parser links a call only to a variable that states nothing.
//   - Python, Kotlin, Scala, Dart and Go: see internal/parse/returns.go.

// ruleReturnedType is the reason an edge bound through a return type carries.
const ruleReturnedType = "declared on the type the call that assigned the receiver returns"

// producedBy is what one file's first pass bound each call to, by the position
// of the call's reference. Filled as references are bound, in the order the
// registry hands them over (by line, then column), which puts a producing call
// before every call on the variable it initialises.
type producedBy map[[2]int]candidate

// callReturned binds a call on a variable initialised from another call
// through the type that call's definition declares it returns. ok is false
// when no part of that is known, and the caller goes on as before.
func (r *Resolver) callReturned(c call, stats *Stats) (decision, bool) {
	if c.ref.ValueLine == 0 || c.produced == nil || c.typesNamed == nil {
		return decision{}, false
	}
	producer, ok := c.produced[[2]int{c.ref.ValueLine, c.ref.ValueCol}]
	if !ok || producer.entry.ReturnType == "" {
		return decision{}, false
	}
	t, ok := c.returnedType(producer.entry)
	if !ok {
		return decision{}, false
	}
	d, ok := c.bindMember(t, ruleReturnedType)
	if !ok || d.verdict != verdictBound {
		return decision{}, false
	}
	d.best.confidence = weaker(d.best.confidence, producer.confidence)
	if d.best.confidence == model.ConfidenceExact {
		// Unreachable while both halves are heuristic; stated so that it stays so.
		d.best.confidence = model.ConfidenceHigh
	}
	d.best.reason += ", from " + producer.entry.Name
	stats.ReturnTyped++
	return d, true
}

// returnedType identifies the type a definition declares it returns, from
// where the DEFINITION is: its own file first, then its package, then the one
// type of that name in the repository. The calling file's view is the wrong
// one; `Order` in the repository's file is whatever that file means by it.
// More than one type at the first level that has any is no answer.
func (c call) returnedType(def registry.SymbolIndexEntry) (typeRef, bool) {
	name := def.ReturnType
	if i := strings.LastIndexAny(name, ".:\\"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return typeRef{}, false
	}
	var inFile, inPackage, all []registry.SymbolIndexEntry
	pkg := c.spec.packageOf(def.FilePath)
	for _, s := range c.typesNamed(name) {
		if !isTypeKind(s.Kind) || !c.spec.canBind(s, model.EdgeReferences) {
			continue
		}
		all = append(all, s)
		switch {
		case s.FileID == def.FileID:
			inFile = append(inFile, s)
		case c.spec.packageOf(s.FilePath) == pkg:
			inPackage = append(inPackage, s)
		}
	}
	for i, group := range [][]registry.SymbolIndexEntry{inFile, inPackage, all} {
		if len(group) == 0 {
			continue
		}
		if len(group) > 1 {
			return typeRef{}, false
		}
		s := group[0]
		qual, _ := strings.CutPrefix(s.FQN, s.FilePath+":")
		// Strong only where the definition's own file or package holds the
		// type; one type of the name elsewhere is the weaker evidence
		// bindMember already knows how to weigh.
		return typeRef{name: name, fileID: s.FileID, filePath: s.FilePath, qual: qual, strong: i < 2}, true
	}
	return typeRef{}, false
}
