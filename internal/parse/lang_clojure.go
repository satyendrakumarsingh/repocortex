package parse

import (
	"strings"
	"unicode"
)

// Clojure. What the index holds, see fixtures/langs/clojure/expected.tsv.
//
// The inferred query for this grammar matches a form anywhere in the file, so
// a `(def x ...)` inside a function body, inside `(comment ...)` or inside a
// `let` was indexed as a definition, and it knows only def, defn, defn- and
// defmacro. It is replaced.
//
// A definition is a defining form at the top level of the file, or directly
// inside a top-level reader conditional (`#?(:clj (defn f ...))`):
//
//   - ns is a package;
//   - defn, defn- and defmacro are functions;
//   - def and defonce are variables;
//   - defmulti is a function, and each defmethod is a method named by the
//     multimethod it extends (the dispatch value is not part of the name, so
//     the several methods of one multimethod share a name and differ by line);
//   - defprotocol and definterface are interfaces, and each method signature
//     inside them is a method;
//   - defrecord and deftype are classes.
//
// Metadata on the name (`^:private`, `^String`, `^{:doc "..."}`) is skipped by
// the grammar. Not definitions: forms nested in another form (a `def` inside a
// `defn`, `let`, `do`, `when` or `comment`), protocol implementations inside
// defrecord, deftype, extend-protocol, extend-type or reify, deftest, and the
// bindings of let.
func init() {
	registerDefinitions("clojure", clojureDefinitions(), true)
	registerImportFix("clojure", clojureImport)
}

// clojureForms names the special forms and the macros that open a construct and
// are written like a call. A call of one is not a call of anything the
// repository defines, so no reference is recorded for it. Quoted for a query
// predicate.
const clojureSpecialForms = `"def" "defn" "defn-" "defmacro" "defmulti" "defmethod" "defonce" "defprotocol" ` +
	`"defrecord" "deftype" "definterface" "defstruct" "defmacro" "declare" "ns" "in-ns" "let" "letfn" "fn" "fn*" "if" ` +
	`"if-not" "if-let" "if-some" "when" "when-not" "when-let" "when-some" "when-first" "cond" "condp" "case" ` +
	`"do" "loop" "recur" "quote" "var" "try" "catch" "finally" "throw" "binding" "for" "doseq" "dotimes" ` +
	`"while" "and" "or" "->" "->>" "as->" "some->" "some->>" "cond->" "cond->>" "doto" "set!" "new" "." ".." ` +
	`"comment" "require" "use" "import" "refer" "extend-type" "extend-protocol" "reify" "proxy" "deftest" ` +
	`"testing" "is" "are" "use-fixtures" "with-open" "with-redefs" "with-local-vars" "lazy-seq" "delay" "future" ` +
	`"assert" "locking" "time" "defprotocol" "gen-class" "extend" "specify" "defcard" "go" "go-loop" "->>"`

// clojureForm is one defining form: the heads that introduce it and the tag
// kind it is recorded as.
type clojureForm struct {
	heads string
	kind  string
}

var clojureForms = []clojureForm{
	{`"defn" "defn-" "defmacro" "defmulti"`, "function"},
	{`"def" "defonce"`, "variable"},
	{`"defmethod"`, "method"},
	{`"defprotocol" "definterface"`, "interface"},
	{`"defrecord" "deftype"`, "class"},
	{`"ns"`, "package"},
}

// clojureCalls are the call patterns: `(f x)`, `(ns/f x)`, and the same as the
// body of an anonymous function `#(f %)`. The list node begins at the call's
// own parenthesis, so the text before the name is how shape.go reads what a
// call was made on (`money/` in `(money/zero)`). A Java method (`.getTime`), a
// constructor (`Date.`), a class named in an `:import` (`java.util`) and a quoted
// list are not calls of repository code.
const clojureCalls = `
((list_lit . (sym_lit . (sym_name) @name)) @reference.call
  (#not-any-of? @name ` + clojureSpecialForms + `)
  (#not-match? @name "[.]|^%")
  (#not-has-ancestor? @reference.call quoting_lit))
((list_lit . (sym_lit (sym_ns) (sym_name) @name)) @reference.call
  (#not-match? @name "^[.]|[.]$|^%")
  (#not-has-ancestor? @reference.call quoting_lit))
((anon_fn_lit . (sym_lit . (sym_name) @name)) @reference.call
  (#not-any-of? @name ` + clojureSpecialForms + `)
  (#not-match? @name "[.]|^%"))
((anon_fn_lit . (sym_lit (sym_ns) (sym_name) @name)) @reference.call
  (#not-match? @name "^[.]|[.]$|^%"))
`

func clojureDefinitions() string {
	// Each pattern is written once against LIST and placed under both a
	// top-level form and a top-level reader conditional, and again for a form
	// that opens with metadata of its own (`^:clj (ns x)`), where the grammar
	// puts the metadata inside the list before the head.
	var patterns []string
	for _, lead := range []string{". ", ". (meta_lit) . "} {
		for _, f := range clojureForms {
			patterns = append(patterns,
				`(list_lit `+lead+`(sym_lit (sym_name) @_head) . (sym_lit (sym_name) @name) (#any-of? @_head `+f.heads+`)) @definition.`+f.kind)
		}
		// A method signature inside a protocol or interface: `(perimeter [this])`.
		patterns = append(patterns,
			`(list_lit `+lead+`(sym_lit (sym_name) @_head)
  (list_lit . (sym_lit (sym_name) @name) . (vec_lit)) @definition.method
  (#any-of? @_head "defprotocol" "definterface"))`)
	}

	var out []string
	for _, p := range patterns {
		out = append(out, "(source "+p+")")
		out = append(out, "(source (read_cond_lit "+p+"))")
	}
	return clojureCalls + strings.Join(out, "\n")
}

// clojureImport turns a captured `(ns ...)`, `(require ...)` or `(use ...)` form
// into one row per namespace it loads. A namespace is named by what it is called
// (`shop.money`), and the row says what the directive does with it: the qualifier
// it binds (`:as m`) and the names it brings in (`:refer [f g]`, `:refer :all`,
// and `:use`'s `:only`). See lang_modules.go for the encoding.
//
//	(:require [shop.money :as m :refer [zero]])     alias:m;import:zero
//	(:require [shop.util :refer :all])              import
//	(:require [shop.data [db :as db] [cache]])      shop.data.db (alias:db), shop.data.cache
//	(:require shop.tax)                             shop.tax, no directive
//	(:use [shop.fmt :only [fmt]])                   import:fmt
//	(:use shop.fmt)                                 import
//
// `(:import ...)` names Java classes and `(:refer-clojure ...)` names clojure.core,
// and neither is a namespace of the repository.
func clojureImport(_ string, imp Import) []Import {
	forms := clojureRead(imp.Path)
	if len(forms) == 0 || forms[0].kind != cljList || len(forms[0].items) == 0 {
		return nil
	}
	top := forms[0].items
	head := top[0].text
	var out []Import
	row := func(module string, alias string) {
		next := imp
		next.Path, next.Alias = module, alias
		out = append(out, next)
	}
	var load func(prefix string, spec cljForm, use bool)
	load = func(prefix string, spec cljForm, use bool) {
		switch spec.kind {
		case cljSymbol:
			if use {
				row(prefix+spec.text, ModuleImportAll)
			} else {
				row(prefix+spec.text, "")
			}
		case cljVector, cljList:
			if len(spec.items) == 0 || spec.items[0].kind != cljSymbol {
				return
			}
			lib := prefix + spec.items[0].text
			rest := spec.items[1:]
			// A prefix list: `[shop.data [db :as db] [cache]]` loads shop.data.db
			// and shop.data.cache and not shop.data itself.
			if len(rest) > 0 && (rest[0].kind == cljVector || rest[0].kind == cljList) {
				for _, sub := range rest {
					load(lib+".", sub, use)
				}
				return
			}
			var parts []string
			all := use
			var names []string
			haveNames := false
			for i := 0; i < len(rest); i++ {
				if rest[i].kind != cljKeyword || i+1 >= len(rest) {
					continue
				}
				val := rest[i+1]
				switch rest[i].text {
				case ":as":
					if val.kind == cljSymbol {
						parts = append(parts, ModuleAliasPrefix+val.text)
					}
				case ":refer", ":only":
					switch {
					case val.kind == cljKeyword && val.text == ":all":
						all = true
					case val.kind == cljVector:
						haveNames = true
						for _, n := range val.items {
							if n.kind == cljSymbol {
								names = append(names, n.text)
							}
						}
					}
				}
			}
			switch {
			case haveNames:
				parts = append(parts, ModuleImportPrefix+strings.Join(names, ","))
			case all:
				parts = append(parts, ModuleImportAll)
			}
			row(lib, moduleDirectives(parts...))
		}
	}
	switch head {
	case "ns":
		for _, clause := range top[1:] {
			if clause.kind != cljList || len(clause.items) == 0 || clause.items[0].kind != cljKeyword {
				continue
			}
			switch clause.items[0].text {
			case ":require":
				for _, spec := range clause.items[1:] {
					load("", spec, false)
				}
			case ":use":
				for _, spec := range clause.items[1:] {
					load("", spec, true)
				}
			}
		}
	case "require", "use":
		for _, spec := range top[1:] {
			load("", spec, head == "use")
		}
	}
	return out
}

type cljKind uint8

const (
	cljSymbol cljKind = iota + 1
	cljKeyword
	cljString
	cljList
	cljVector
	cljMap
)

// cljForm is one datum of a Clojure form, as much of it as an `ns` form needs.
type cljForm struct {
	kind  cljKind
	text  string
	items []cljForm
}

// clojureRead reads the data in a piece of Clojure source: lists, vectors, maps,
// symbols, keywords and strings. A quote, a syntax quote, metadata, a reader
// discard (`#_`) and comments are skipped over; anything else is a symbol.
func clojureRead(src string) []cljForm {
	r := &cljReader{src: src}
	var out []cljForm
	for {
		f, ok := r.next()
		if !ok {
			return out
		}
		out = append(out, f)
	}
}

type cljReader struct {
	src string
	pos int
}

func (r *cljReader) skip() {
	for r.pos < len(r.src) {
		c := r.src[r.pos]
		switch {
		case c == ';':
			for r.pos < len(r.src) && r.src[r.pos] != '\n' {
				r.pos++
			}
		case unicode.IsSpace(rune(c)) || c == ',':
			r.pos++
		case c == '\'' || c == '`' || c == '~' || c == '@':
			r.pos++
		case c == '^':
			// Metadata: the next datum is not part of the form.
			r.pos++
			r.skip()
			r.next()
		case c == '#' && r.pos+1 < len(r.src) && r.src[r.pos+1] == '_':
			r.pos += 2
			r.skip()
			r.next()
		default:
			return
		}
	}
}

func (r *cljReader) next() (cljForm, bool) {
	r.skip()
	if r.pos >= len(r.src) {
		return cljForm{}, false
	}
	c := r.src[r.pos]
	switch c {
	case '(', '[', '{':
		closer := map[byte]byte{'(': ')', '[': ']', '{': '}'}[c]
		kind := map[byte]cljKind{'(': cljList, '[': cljVector, '{': cljMap}[c]
		r.pos++
		form := cljForm{kind: kind}
		for {
			r.skip()
			if r.pos >= len(r.src) {
				return form, true
			}
			if r.src[r.pos] == closer {
				r.pos++
				return form, true
			}
			if r.src[r.pos] == ')' || r.src[r.pos] == ']' || r.src[r.pos] == '}' {
				// A mismatched closer: the form is malformed.
				return form, true
			}
			item, ok := r.next()
			if !ok {
				return form, true
			}
			form.items = append(form.items, item)
		}
	case '"':
		start := r.pos
		r.pos++
		for r.pos < len(r.src) && r.src[r.pos] != '"' {
			if r.src[r.pos] == '\\' {
				r.pos++
			}
			r.pos++
		}
		r.pos++
		if r.pos > len(r.src) {
			r.pos = len(r.src)
		}
		return cljForm{kind: cljString, text: r.src[start:r.pos]}, true
	case '#':
		// A tagged literal, a set, an anonymous function, a regex: opaque.
		start := r.pos
		r.pos++
		if r.pos < len(r.src) && (r.src[r.pos] == '(' || r.src[r.pos] == '{' || r.src[r.pos] == '[') {
			inner, _ := r.next()
			inner.text = r.src[start:r.pos]
			return inner, true
		}
		for r.pos < len(r.src) && !strings.ContainsRune("()[]{}, \t\r\n", rune(r.src[r.pos])) {
			r.pos++
		}
		return cljForm{kind: cljSymbol, text: r.src[start:r.pos]}, true
	}
	start := r.pos
	for r.pos < len(r.src) && !strings.ContainsRune("()[]{}\", \t\r\n;", rune(r.src[r.pos])) {
		r.pos++
	}
	if r.pos == start {
		// A stray byte the loop above does not consume.
		r.pos++
		return cljForm{kind: cljSymbol, text: r.src[start:r.pos]}, true
	}
	text := r.src[start:r.pos]
	if text[0] == ':' {
		return cljForm{kind: cljKeyword, text: text}, true
	}
	return cljForm{kind: cljSymbol, text: text}, true
}
