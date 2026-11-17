package parse

import (
	"sort"
	"strings"

	"github.com/odvcencio/gotreesitter/grammars"
)

// A language's definition query can come from three places, tried in order.
//
//  1. The query gotreesitter infers for the grammar. It is built from a fixed
//     list of common node names (function_definition, class_declaration and
//     the like), so it finds plain functions and classes in a grammar that
//     happens to use those names and nothing a language spells its own way.
//     For many grammars it is empty, or it matches nothing.
//  2. A supplement in the supplements map, added to the inferred query.
//  3. A query registered here from a lang_<name>.go file, written by hand
//     against the grammar's real node types. It is added to the above, or
//     replaces the inferred query when the inferred one is wrong rather than
//     merely incomplete (it over-captures, or names nodes the grammar lacks).
//
// The third exists so that each language is one file plus one fixture. A
// language verified by hand should be changeable without touching a map that
// every other language shares.
type registeredQuery struct {
	query           string
	replaceInferred bool
}

var (
	registeredDefinitions = map[string]registeredQuery{}

	// duplicateRegistrations is checked by a test instead of panicking at
	// init: two files claiming one language is a mistake in the source tree,
	// and a panic at start-up would take every other language down with it.
	duplicateRegistrations []string
)

// registerDefinitions sets the hand-written definition query for a language.
// Call it from an init function in lang_<language>.go.
//
// With replaceInferred the inferred query is discarded and only this one (and
// any supplement) is used. Without it, this query is appended.
//
// The query uses tree-sitter tag captures: @name for the identifier and
// @definition.KIND for the declaring node, where KIND is one kindFromTag
// understands (function, method, constructor, class, struct, enum, interface,
// trait, type_alias, module, namespace, constant, variable, field).
// References are @reference.KIND with the same @name.
func registerDefinitions(language, query string, replaceInferred bool) {
	if _, dup := registeredDefinitions[language]; dup {
		duplicateRegistrations = append(duplicateRegistrations, language)
		return
	}
	registeredDefinitions[language] = registeredQuery{query: query, replaceInferred: replaceInferred}
}

// HandWrittenLanguages lists the languages whose definition query was written
// by hand and registered, sorted. Each must have a conformance fixture.
func HandWrittenLanguages() []string {
	out := make([]string, 0, len(registeredDefinitions))
	for l := range registeredDefinitions {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// definitionQueryText is the query a parser for this grammar is built from,
// before the inheritance patterns are appended. Empty means no parser can be
// built and every file of the language is skipped.
func definitionQueryText(entry grammars.LangEntry) string {
	reg, registered := registeredDefinitions[entry.Name]

	var parts []string
	if !registered || !reg.replaceInferred {
		if inferred := grammars.ResolveTagsQuery(entry); strings.TrimSpace(inferred) != "" {
			parts = append(parts, inferred)
		}
	}
	if sup := Supplement(entry.Name); sup != "" {
		parts = append(parts, sup)
	}
	if registered && strings.TrimSpace(reg.query) != "" {
		parts = append(parts, reg.query)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// nameFixes holds, per language, a correction applied to every captured name.
//
// Some grammars lex a name together with a neighbouring character: Authzed
// reads `relation admin: user` as the name "admin:". A query cannot trim a
// capture, and a symbol with a wrong name is worse than a missing one, so the
// correction is made here, on the text, after the query has matched.
var nameFixes = map[string]func(string) string{}

// registerNameFix registers the correction for a language's captured names.
// Call it from init in the language's lang_<language>.go.
func registerNameFix(language string, fix func(string) string) {
	if _, dup := nameFixes[language]; dup {
		duplicateRegistrations = append(duplicateRegistrations, language+" (name fix)")
	}
	nameFixes[language] = fix
}

// cleanName is the name a capture stands for: its text without surrounding
// white space, then the language's own correction when it has one.
//
// The white space is trimmed for every language. No identifier begins or ends
// with it, and a grammar that includes it in the token (Circom does, for the
// name after `template`) would otherwise produce a symbol nothing can look up.
func cleanName(language, name string) string {
	name = strings.TrimSpace(name)
	if fix := nameFixes[language]; fix != nil {
		name = fix(name)
	}
	return name
}

// importFixes holds, per language, a rewrite of each import the import query
// captured, made with the importing file's own path in hand.
//
// A query sees text, and for some languages the text of an import does not
// name what the resolver compares it with. Swift's `import Core.Models` names
// the module `Core`; Dart's `import '../util.dart'` names a file relative to
// the one that wrote it, and `package:shop/a.dart` names lib/a.dart. Neither
// can be said in a pattern, and the resolver (which matches the trailing
// segments of an import against a file's) cannot do it either, because it is
// handed the path alone. A rewrite may return no import (the import names
// nothing in the repository) or several (an include that is relative to the
// file AND to a search path names two files, and the resolver prefers the
// longer match).
var importFixes = map[string]func(filePath string, imp Import) []Import{}

// registerImportFix registers the rewrite for a language's imports. Call it
// from init in the language's lang_<language>.go.
func registerImportFix(language string, fix func(filePath string, imp Import) []Import) {
	if _, dup := importFixes[language]; dup {
		duplicateRegistrations = append(duplicateRegistrations, language+" (import fix)")
	}
	importFixes[language] = fix
}

// fixImports applies a language's import rewrite, if it has one, and returns
// the imports in the order and without the repeats extractImports gives.
func fixImports(language, filePath string, in []Import) []Import {
	fix := importFixes[language]
	if fix == nil || len(in) == 0 {
		return in
	}
	out := make([]Import, 0, len(in))
	for _, imp := range in {
		out = append(out, fix(filePath, imp)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if keepsEveryDirective[language] {
		return dedupeDirectives(out)
	}
	return dedupeImports(out)
}
