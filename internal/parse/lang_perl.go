package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Perl. What the index holds, see fixtures/langs/perl/expected.tsv.
//
//   - sub NAME is a function wherever it is written: at file level, after a
//     `package` statement, inside a `package NAME { }` block. Perl has no
//     method syntax, so a sub reached through `$self->` is still a function.
//     A prototype or attribute list does not matter;
//   - package NAME, in statement or block form, is a package;
//   - `use constant NAME => value` and every key of
//     `use constant { A => 1, B => 2 }` are constants. In the hash form the
//     keys are the bare words that follow a value, so a bare-word value is
//     read as one more key;
//   - `our $x`, `our @x`, `our %x` and `our ($a, $b)` at file level or
//     directly inside a package block are variables (`my` is not: it is
//     private to the file or block).
//
// Anonymous subs, `my` and `local` variables, calls and `use Module` are not
// definitions.
//
// Calls: `f(x)`, `f x`, `A::B::f(x)` (the name is the last segment, and the text
// before it is the package), and `A::B->f(x)`, `$self->f(x)`, `__PACKAGE__->f`
// (see perlShape). The interpreter's own functions are tagged like any other and
// are told apart by the resolver. Every definition is qualified by the package
// it is written in (`Shop::Cart.total`, see perlOwners), because a call reaches a
// sub through its package and a file may hold several.
func init() {
	registerDefinitions("perl", perlCalls+perlDefinitions, true)
	definitionRefiners["perl"] = perlOwners
	tagAdjusters["perl"] = perlSplitQualified
	registerImportFix("perl", perlImport)
}

const perlCalls = `
(function_call_expression function: (function) @name) @reference.call
(ambiguous_function_call_expression function: (function) @name) @reference.call
(method_call_expression method: (method) @name) @reference.call
`

// perlSplitQualified moves a call's name to the last segment of a qualified one.
// The grammar makes `Shop::Money::zero` one `function` and `SUPER::new` one
// `method`, so the tag names the whole path; the receiver of the call is the
// text before the last `::`, which shape.go reads from the tag's ranges.
func perlSplitQualified(src []byte, tags []ts.Tag) []ts.Tag {
	for i, t := range tags {
		if !isCallTag(t.Kind) {
			continue
		}
		// `&helper(1)` is a call of helper, and the sigil is not part of its name.
		cut := 0
		if strings.HasPrefix(t.Name, "&") {
			cut = 1
		}
		if last := strings.LastIndex(t.Name, "::"); last >= 0 {
			cut = last + 2
		}
		if cut == 0 || int(t.NameRange.StartByte)+cut > int(t.NameRange.EndByte) {
			continue
		}
		t.Name = t.Name[cut:]
		t.NameRange.StartByte += uint32(cut)
		t.NameRange.StartPoint.Column += uint32(cut)
		tags[i] = t
	}
	return tags
}

// perlOwners says which package every definition is in: the block of a
// `package NAME { }` that holds it, or else the last `package NAME;` before it
// among the statements around. "" is `main`, the package of code written before
// any, and is left off.
func perlOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") || kindFromTag(tag.Kind) == model.KindPackage {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		owner := ""
	climb:
		for p := n; p != nil; p = p.Parent() {
			if p.Type(lang) == "package_statement" && perlBlockOf(p, lang) != nil {
				owner = perlPackageOf(p, lang, src)
				break
			}
			for sib := p.PrevSibling(); sib != nil; sib = sib.PrevSibling() {
				if sib.Type(lang) == "package_statement" && perlBlockOf(sib, lang) == nil {
					owner = perlPackageOf(sib, lang, src)
					break climb
				}
			}
		}
		if owner == "" {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fixes[tag.NameRange.StartByte] = definitionFix{owner: owner}
	}
	return fixes
}

func perlBlockOf(pkg *ts.Node, lang *ts.Language) *ts.Node {
	for i := 0; i < pkg.ChildCount(); i++ {
		if c := pkg.Child(i); c != nil && c.Type(lang) == "block" {
			return c
		}
	}
	return nil
}

func perlPackageOf(pkg *ts.Node, lang *ts.Language, src []byte) string {
	if name := pkg.ChildByFieldName("name", lang); name != nil {
		return name.Text(src)
	}
	return ""
}

// perlImport reads a statement that names a package.
//
//	use A::B;                        A::B, import      whatever the module exports
//	use A::B qw(f g);                A::B, import:f,g
//	use A::B ();                     A::B              nothing is imported
//	require A::B;                    A::B              `A::B::f()` only
//	use parent -norequire, 'A::B';   A::B, parent      the package inherits from A::B
//	use base 'A::B';                 A::B, parent
//
// A pragma (`use strict`, `use constant`, `use lib`) names no package: they are
// the lower-case words, and a package is capitalised by convention.
func perlImport(_ string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	if m := perlRequireStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[1], ""
		return []Import{imp}
	}
	m := perlUseStmt.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	module, rest := m[1], strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[2]), ";"))
	if module == "parent" || module == "base" {
		var out []Import
		for _, q := range perlQuoted.FindAllStringSubmatch(rest, -1) {
			for _, parent := range strings.Fields(q[1] + q[2] + q[3]) {
				if parent[0] < 'A' || parent[0] > 'Z' {
					continue
				}
				row := imp
				row.Path, row.Alias = parent, ModuleParent
				out = append(out, row)
			}
		}
		return out
	}
	if module[0] < 'A' || module[0] > 'Z' {
		return nil
	}
	imp.Path = module
	switch {
	case rest == "" || perlVersion.MatchString(rest):
		imp.Alias = ModuleImportAll
	case strings.HasPrefix(rest, "()"):
		imp.Alias = ""
	default:
		var names []string
		for _, w := range perlWords.FindAllStringSubmatch(rest, -1) {
			names = append(names, w[1])
		}
		if q := perlQw.FindStringSubmatch(rest); q != nil {
			names = strings.Fields(q[1])
		}
		var funcs []string
		for _, n := range names {
			if haskellVariable(n) {
				funcs = append(funcs, n)
			}
		}
		if len(funcs) > 0 {
			imp.Alias = ModuleImportPrefix + strings.Join(funcs, ",")
		} else {
			imp.Alias = ModuleImportAll
		}
	}
	return []Import{imp}
}

var (
	perlUseStmt     = regexp.MustCompile(`(?s)^use\s+([A-Za-z][A-Za-z0-9_]*(?:::[A-Za-z0-9_]+)*)\s*(.*)$`)
	perlRequireStmt = regexp.MustCompile(`^require\s+([A-Z][A-Za-z0-9_]*(?:::[A-Za-z0-9_]+)*)\s*;?$`)
	perlQw          = regexp.MustCompile(`(?s)qw\s*[(\[{/]([^)\]}/]*)[)\]}/]`)
	perlQuoted      = regexp.MustCompile(`'([^']*)'|"([^"]*)"|qw\s*[(\[{/]([^)\]}/]*)[)\]}/]`)
	perlVersion     = regexp.MustCompile(`^v?[0-9][0-9._]*$`)
	perlWords       = regexp.MustCompile(`['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)
)

const perlDefinitions = `
(subroutine_declaration_statement . (bareword) @name) @definition.function
(package_statement . (package) @name) @definition.module

(use_statement
  . (package) @_kw
  (list_expression . (autoquoted_bareword) @name @definition.constant)
  (#eq? @_kw "constant"))
(use_statement
  . (package) @_kw
  (anonymous_hash_expression (list_expression . (autoquoted_bareword) @name @definition.constant))
  (#eq? @_kw "constant"))
(use_statement
  . (package) @_kw
  (anonymous_hash_expression (list_expression (list_expression (_) . (autoquoted_bareword) @name @definition.constant)))
  (#eq? @_kw "constant"))

(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (scalar (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (scalar (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (array (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (array (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (hash (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (hash (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (scalar (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (scalar (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (array (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (array (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (hash (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (hash (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
`

// perlShape reads what a call was made on from the text before its name:
// `Shop::Money->new` and `Shop::Money::zero` are on the package, `$self->f`,
// `$class->f` and `__PACKAGE__->f` on the package the call is written in,
// `$class->SUPER::new` on the package it inherits from, and any other variable
// (`$m->fmt`) or expression on a value nothing here can name. A bare call has no
// text before it and never gets here.
func perlShape(pre string) callShape {
	opaque := callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	if pre == "&" {
		// `&helper(1)`: the sigil that says a sub is called, before its name.
		return callShape{kind: model.ReceiverBare}
	}
	var recv string
	switch {
	case strings.HasSuffix(pre, "->"):
		recv = strings.TrimSpace(strings.TrimSuffix(pre, "->"))
	case strings.HasSuffix(pre, "::"):
		recv = strings.TrimSuffix(pre, "::")
		if strings.HasSuffix(recv, "->SUPER") || recv == "SUPER" {
			return callShape{kind: model.ReceiverSuper, receiver: "SUPER"}
		}
	default:
		return opaque
	}
	switch {
	case recv == "$self" || recv == "$class" || recv == "__PACKAGE__" || recv == "$proto":
		return callShape{kind: model.ReceiverSelf, receiver: recv}
	case strings.HasPrefix(recv, "$"):
		return callShape{kind: model.ReceiverValue, receiver: recv}
	case perlPackageName.MatchString(recv):
		return callShape{kind: model.ReceiverScope, receiver: recv}
	}
	return opaque
}

var perlPackageName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(::[A-Za-z0-9_]+)*$`)
