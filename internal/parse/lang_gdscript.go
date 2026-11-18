package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// GDScript: a script is itself a class. What it declares at the top of the
// file (outside any `class`) is recorded as a function, constant, variable;
// what an inner `class` declares is a method, constant, field. A signal is a
// field. Locals inside a function body are `variable_statement` nodes under a
// `body`, so every pattern is anchored to the source file or a class body.
//
// An anonymous `enum { A, B }` has no name and is not extracted, nor are the
// enumerators of a named one.
//
// A script is a class, named by its `class_name` or, without one, reached only by
// the path a `preload` or an `extends` writes. Every definition is qualified by it
// (`Player.take_damage`; a script with no class_name by its file's name, so
// `weapon.fire`), and the members of an inner class by it and the class
// (`Player.Inventory.add`): see gdscriptOwners. The class_name itself and the
// script's own name are one definition, `Player`.
//
// The imports are what names another script:
//
//	extends "res://scripts/base.gd"      scripts/base, include    the parent's members are this script's own
//	extends Base                         class:Base, include      likewise, by a class_name
//	const Weapon = preload("res://w.gd") w, alias:Weapon          `Weapon.new()` and `Weapon.f()` name that script
//	var Enemy = load("res://e.gd")       e, alias:Enemy           likewise
//
// A `res://` path maps to the directory that holds project.godot, which is not a
// source file: the row keeps the path beneath it, and the resolver matches it to the
// tail of a file's path. A path relative to the script (`./x.gd`, `x.gd`) is
// rewritten to the repository path of the file it names. A script that extends an
// engine class (`extends Node`) names no file here.
//
// Calls: `f(x)`, `x.f(y)`, `A.f(x)`, `self.f()` and `super.f()`; `A.new()` is the
// construction of A.
func init() {
	registerDefinitions("gdscript", `
(source (class_name_statement (name) @name) @definition.class)
(source (class_definition (name) @name) @definition.class)
(class_body (class_definition (name) @name) @definition.class)

(source (function_definition (name) @name) @definition.function)
(class_body (function_definition (name) @name) @definition.method)

(source (constructor_definition "_init" @name) @definition.function)
(class_body (constructor_definition "_init" @name) @definition.method)

(source (variable_statement (name) @name) @definition.variable)
(class_body (variable_statement (name) @name) @definition.field)
(source (export_variable_statement (name) @name) @definition.variable)
(source (onready_variable_statement (name) @name) @definition.variable)
(class_body (export_variable_statement (name) @name) @definition.field)
(class_body (onready_variable_statement (name) @name) @definition.field)

(source (const_statement (name) @name) @definition.constant)
(class_body (const_statement (name) @name) @definition.constant)

(source (signal_statement (name) @name) @definition.field)
(class_body (signal_statement (name) @name) @definition.field)

(source (enum_definition (name) @name) @definition.class)
(class_body (enum_definition (name) @name) @definition.class)

(call . (identifier) @name) @reference.call
(attribute (attribute_call . (identifier) @name)) @reference.call
`, true)
	registerImportFix("gdscript", gdscriptImport)
	definitionRefiners["gdscript"] = gdscriptOwners
}

// gdscriptOwners qualifies every definition by the script it is in and the inner
// classes around it, outermost first.
func gdscriptOwners(root *ts.Node, lang *ts.Language, filePath string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	script := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
	for i := 0; i < root.ChildCount(); i++ {
		c := root.Child(i)
		if c == nil || c.Type(lang) != "class_name_statement" {
			continue
		}
		for j := 0; j < c.ChildCount(); j++ {
			if n := c.Child(j); n != nil && n.Type(lang) == "name" {
				script = strings.TrimSpace(n.Text(src))
			}
		}
		break
	}
	inner := ownersBy(map[string]string{"class_definition": "name"})(root, lang, filePath, src, tags)
	fixes := make(map[uint32]definitionFix, len(tags))
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		at := tag.NameRange.StartByte
		if kindFromTag(tag.Kind) == model.KindClass && strings.TrimSpace(string(src[at:tag.NameRange.EndByte])) == script &&
			isClassNameName(root, lang, at) {
			// The script's own name is the one definition of it.
			continue
		}
		owner := script
		if f, ok := inner[at]; ok {
			owner = script + "." + f.owner
		}
		fixes[at] = definitionFix{owner: owner}
	}
	return fixes
}

// isClassNameName reports whether the name at a position is the one a `class_name`
// statement declares.
func isClassNameName(root *ts.Node, lang *ts.Language, at uint32) bool {
	n := root.NamedDescendantForByteRange(at, at+1)
	for ; n != nil; n = n.Parent() {
		if n.Type(lang) == "class_name_statement" {
			return true
		}
	}
	return false
}

// gdscriptImport turns an extends, a const or var that preloads or loads a script,
// into a row. See the comment on init for the rows.
func gdscriptImport(filePath string, imp Import) []Import {
	text := strings.Join(strings.Fields(imp.Path), " ")
	if m := gdExtendsPath.FindStringSubmatch(text); m != nil {
		if p := gdScriptPath(filePath, m[1]); p != "" {
			imp.Path, imp.Alias = p, ModuleInclude
			return []Import{imp}
		}
		return nil
	}
	if m := gdExtendsClass.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = "class:"+m[1], ModuleInclude
		return []Import{imp}
	}
	if m := gdPreload.FindStringSubmatch(text); m != nil {
		if p := gdScriptPath(filePath, m[2]); p != "" {
			imp.Path, imp.Alias = p, ModuleAliasPrefix+m[1]
			return []Import{imp}
		}
	}
	return nil
}

var (
	gdExtendsPath  = regexp.MustCompile(`^extends\s+"([^"]+)"`)
	gdExtendsClass = regexp.MustCompile(`^extends\s+([A-Z][A-Za-z0-9_]*)\s*$`)
	gdPreload      = regexp.MustCompile(`^(?:@\w+(?:\([^)]*\))?\s+)*(?:const|var)\s+([A-Za-z_]\w*)\s*(?::\s*\w+\s*)?(?::=|=)\s*(?:preload|load)\(\s*"([^"]+)"\s*\)`)
)

// gdScriptPath is the name an import row stores for a script: for `res://a/b.gd` the
// path beneath the project, `a/b`; for a path relative to the importing file, its
// repository path. "" for anything that is not a script (a scene, a resource, a
// `user://` path).
func gdScriptPath(filePath, target string) string {
	if !strings.HasSuffix(target, ".gd") {
		return ""
	}
	target = strings.TrimSuffix(target, ".gd")
	switch {
	case strings.HasPrefix(target, "res://"):
		return path.Clean(strings.TrimPrefix(target, "res://"))
	case strings.Contains(target, "://"):
		return ""
	}
	rel := path.Join(path.Dir(filePath), target)
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return rel
}
