package scip

import (
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestEntityKindVocabularyIsStable pins the half of an agreement whose other
// half lives in internal/parse (kindFromTag), tested there by
// TestKindFromTagMatchesTheCompilerBackedVocabulary.
//
// The two paths must answer the same question the same way, because a
// heuristic edge is UPGRADED IN PLACE when a compiler-backed index arrives. If
// this file called a struct a class while the tree-sitter path called it an
// alias, the upgrade would silently change what a symbol is, and the SCIP
// cross-check that measures heuristic precision would report divergences that
// are really just two vocabularies.
//
// This file is the authority of the two: it is anchored on SCIP's own enum,
// which is a published protocol rather than a naming convention. So it is
// asserted here first, and internal/parse follows.
func TestEntityKindVocabularyIsStable(t *testing.T) {
	cases := map[scippb.SymbolInformation_Kind]model.EntityKind{
		// Aggregate types collapse to class. The vocabulary deliberately does
		// not grow an entry per language construct — see the comment on
		// entityKind — because the graph's consumers ask "is this a type", not
		// "is this a record or a struct or an object".
		scippb.SymbolInformation_Class:  model.KindClass,
		scippb.SymbolInformation_Struct: model.KindClass,
		scippb.SymbolInformation_Enum:   model.KindClass,
		scippb.SymbolInformation_Union:  model.KindClass,
		scippb.SymbolInformation_Type:   model.KindClass,

		scippb.SymbolInformation_Interface: model.KindInterface,
		scippb.SymbolInformation_Trait:     model.KindInterface,
		scippb.SymbolInformation_Protocol:  model.KindInterface,

		// Only a genuine alias is an alias.
		scippb.SymbolInformation_TypeAlias:      model.KindTypeAlias,
		scippb.SymbolInformation_AssociatedType: model.KindTypeAlias,

		scippb.SymbolInformation_Method:   model.KindMethod,
		scippb.SymbolInformation_Function: model.KindFunction,
	}

	for kind, want := range cases {
		if got := entityKind(kind, ""); got != want {
			t.Errorf("entityKind(%v) = %q, want %q — internal/parse.kindFromTag "+
				"mirrors this mapping and must be changed with it", kind, got, want)
		}
	}
}
