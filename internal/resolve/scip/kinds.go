package scip

import (
	"strings"

	scippb "github.com/scip-code/scip/bindings/go/scip"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// entityKind maps a SCIP symbol to one of RepoCortex's entity kinds.
//
// SCIP's kind enum is far richer than model.EntityKind — it distinguishes a
// Lean `Lemma` from a `Theorem` — and collapsing it loses information. That is
// the right trade here: the graph's consumers ask "is this callable" and "is
// this a type", not "is this a tactic", and a kind vocabulary that grew with
// every indexed language would push language knowledge into the context engine,
// which CONTRIBUTING forbids.
//
// Many indexers leave SymbolInformation.kind unset, so the symbol string's own
// descriptor suffix is the fallback. That suffix is part of the SCIP grammar,
// not a guess about the language.
func entityKind(k scippb.SymbolInformation_Kind, symbol string) model.EntityKind {
	switch k {
	case scippb.SymbolInformation_Class,
		scippb.SymbolInformation_Struct,
		scippb.SymbolInformation_Object,
		scippb.SymbolInformation_Enum,
		scippb.SymbolInformation_Union,
		scippb.SymbolInformation_Contract,
		scippb.SymbolInformation_Library,
		scippb.SymbolInformation_SingletonClass,
		scippb.SymbolInformation_Message,
		scippb.SymbolInformation_Mixin,
		scippb.SymbolInformation_Extension,
		scippb.SymbolInformation_DataFamily,
		scippb.SymbolInformation_Instance,
		scippb.SymbolInformation_Concept,
		scippb.SymbolInformation_Type,
		scippb.SymbolInformation_TypeFamily:
		return model.KindClass

	case scippb.SymbolInformation_Interface,
		scippb.SymbolInformation_Protocol,
		scippb.SymbolInformation_Trait,
		scippb.SymbolInformation_TypeClass,
		scippb.SymbolInformation_Signature:
		return model.KindInterface

	case scippb.SymbolInformation_Method,
		scippb.SymbolInformation_AbstractMethod,
		scippb.SymbolInformation_StaticMethod,
		scippb.SymbolInformation_SingletonMethod,
		scippb.SymbolInformation_MethodAlias,
		scippb.SymbolInformation_MethodSpecification,
		scippb.SymbolInformation_ProtocolMethod,
		scippb.SymbolInformation_PureVirtualMethod,
		scippb.SymbolInformation_TraitMethod,
		scippb.SymbolInformation_TypeClassMethod,
		scippb.SymbolInformation_Constructor,
		scippb.SymbolInformation_Getter,
		scippb.SymbolInformation_Setter,
		scippb.SymbolInformation_Accessor,
		scippb.SymbolInformation_Operator,
		scippb.SymbolInformation_Subscript:
		return model.KindMethod

	case scippb.SymbolInformation_Function,
		scippb.SymbolInformation_Macro,
		scippb.SymbolInformation_Quasiquoter,
		scippb.SymbolInformation_Delegate,
		scippb.SymbolInformation_Modifier,
		scippb.SymbolInformation_Predicate,
		scippb.SymbolInformation_Tactic:
		return model.KindFunction

	case scippb.SymbolInformation_Field,
		scippb.SymbolInformation_StaticField,
		scippb.SymbolInformation_StaticDataMember,
		scippb.SymbolInformation_Property,
		scippb.SymbolInformation_StaticProperty,
		scippb.SymbolInformation_Attribute,
		scippb.SymbolInformation_Key:
		return model.KindField

	case scippb.SymbolInformation_Constant,
		scippb.SymbolInformation_EnumMember,
		scippb.SymbolInformation_Axiom,
		scippb.SymbolInformation_Fact,
		scippb.SymbolInformation_Assertion,
		scippb.SymbolInformation_Lemma,
		scippb.SymbolInformation_Theorem:
		return model.KindConstant

	case scippb.SymbolInformation_TypeAlias,
		scippb.SymbolInformation_AssociatedType:
		return model.KindTypeAlias

	case scippb.SymbolInformation_Namespace,
		scippb.SymbolInformation_Package,
		scippb.SymbolInformation_PackageObject,
		scippb.SymbolInformation_Lang,
		scippb.SymbolInformation_Grammar:
		return model.KindPackage

	case scippb.SymbolInformation_Module:
		return model.KindModule

	case scippb.SymbolInformation_File:
		return model.KindFile

	case scippb.SymbolInformation_Event,
		scippb.SymbolInformation_StaticEvent:
		return model.KindEvent

	case scippb.SymbolInformation_Variable,
		scippb.SymbolInformation_StaticVariable,
		scippb.SymbolInformation_Parameter,
		scippb.SymbolInformation_ParameterLabel,
		scippb.SymbolInformation_SelfParameter,
		scippb.SymbolInformation_ThisParameter,
		scippb.SymbolInformation_MethodReceiver,
		scippb.SymbolInformation_TypeParameter,
		scippb.SymbolInformation_Pattern,
		scippb.SymbolInformation_Value,
		scippb.SymbolInformation_String,
		scippb.SymbolInformation_Number,
		scippb.SymbolInformation_Boolean,
		scippb.SymbolInformation_Array,
		scippb.SymbolInformation_Null,
		scippb.SymbolInformation_Error:
		return model.KindVariable
	}
	return descriptorKind(symbol)
}

// descriptorKind reads the kind out of a SCIP symbol string's final descriptor.
//
// The grammar terminates each descriptor with a character that names its class:
// '/' namespace, '#' type, '.' term, ').' method, ':' meta, '!' macro,
// ']' type parameter, ')' parameter. Inspecting the last one or two bytes is
// enough, and it avoids running the full symbol parser once per occurrence —
// parsing every symbol string in a large index costs more than the rest of
// ingestion put together.
func descriptorKind(symbol string) model.EntityKind {
	if scippb.IsLocalSymbol(symbol) {
		return model.KindVariable
	}
	if symbol == "" {
		return model.KindVariable
	}
	switch symbol[len(symbol)-1] {
	case '/':
		return model.KindPackage
	case '#':
		return model.KindClass
	case '!':
		return model.KindFunction
	case ':':
		return model.KindConstant
	case ']', ')':
		return model.KindVariable
	case '.':
		if strings.HasSuffix(symbol, ").") {
			return model.KindMethod
		}
		return model.KindVariable
	}
	return model.KindVariable
}

// edgeKind maps an occurrence's role bitset onto a relationship kind.
//
// Only IMPORTS is distinguished. Notably absent:
//
//   - CALLS. SCIP records that an identifier occurs and what it resolves to,
//     never whether the occurrence is a call, a method value, or a name in a
//     doc comment. Deriving CALLS *from the index alone* would mean inferring
//     it from the target's kind, and an inferred fact stamped confidence=exact
//     is precisely the lie the confidence system exists to prevent. That
//     objection stands, and this function still obeys it: nothing here reads a
//     symbol's kind to decide that an occurrence is a call.
//
//     WHAT CHANGED. CALLS edges now DO come out of this package — see
//     PromoteCalls in calls.go — but not from SCIP. They come from a join of
//     two independently sourced facts: RepoCortex's own tree-sitter parser
//     recorded a call expression at a file and line (a syntactic fact about the
//     source text, from symbol_references), and the compiler resolved that same
//     line to exactly one target (this index). Neither half is inferred, and
//     neither is derived from the other; the new thing is only the join, and it
//     is the same one internal/cli/accuracy.go already treats as ground truth
//     when adjudicating the heuristic resolver. A line the compiler resolved to
//     more than one target stays REFERENCES, because there "which symbol does
//     the call reach" has no answer and any choice would be the guess this
//     paragraph forbids.
//
//     The promotion deliberately happens AFTER edges exist rather than here.
//     edgeKind sees one occurrence's role bits; the join needs the whole file's
//     resolved targets per line plus a fact from outside the index entirely,
//     and burying that in a bitmask switch would hide exactly the second source
//     that makes it legitimate.
//
//   - READS/WRITES. SCIP's ReadAccess and WriteAccess roles describe variable
//     access, while model's READS/WRITES denote data-store access (they sit
//     beside TABLE and COLUMN in the vocabulary). Reusing them would make
//     "which code writes this table" return every assignment in the repository.
//     Both fold into REFERENCES; the role bits are still counted in Stats.
func edgeKind(roles int32) model.EdgeKind {
	if roles&int32(scippb.SymbolRole_Import) != 0 {
		return model.EdgeImports
	}
	return model.EdgeReferences
}

// exported guesses visibility from the symbol string.
//
// SCIP has no visibility field, so this is genuinely a heuristic — but it lands
// on model.Symbol.Exported, which is a display hint, never on an edge's
// confidence. Local symbols are unexported by definition; everything else is
// treated as exported, because a global SCIP symbol is by construction
// referenceable from outside its document.
func exported(symbol string) bool { return !scippb.IsLocalSymbol(symbol) }
