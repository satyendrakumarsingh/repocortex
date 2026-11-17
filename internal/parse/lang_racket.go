package parse

import "strings"

// Racket. What the index holds, see fixtures/langs/racket/expected.tsv.
//
// The grammar reads every form as a plain list of symbols, so a definition is
// recognised by the symbol that opens the list. It counts only at the top of
// the file or directly inside a `module`, `module*` or `module+` form; a
// `define` in a function body, a `let`, a `when` or a `for` is a local.
//
//   - module, module* and module+ are packages;
//   - define with a parameter list is a function, curried definitions
//     (`(define ((f a) b) ...)`) included; so are `define-syntax`,
//     `define-syntax-rule`, `define/contract` and `define-inline` forms
//     (macros are functions), and `(define f (lambda ...))` or `(λ ...)`;
//   - any other `(define name value)` is a variable, and each name of
//     `define-values` is a variable;
//   - struct and define-struct are classes (the generated accessors are not
//     held, and neither are the fields);
//   - `(define C (class ...))` is a class, and the `define/public`,
//     `define/private`, `define/override` and similar forms directly in its
//     body are methods;
//   - define-generics is an interface and each method it lists is a method.
//
// Not definitions: require, provide, define-logger, a define that is not at
// the top or in a module, the `define`s inside a class body that are not
// methods (fields), and calls.
func init() {
	registerDefinitions("racket", racketDefinitions(), true)
}

const racketLambdas = `"lambda" "λ" "case-lambda" "curry" "curryr"`

func racketDefinitions() string {
	// The module context: a form directly inside module, module* or module+.
	// The forms are written once against the list that holds them.
	type ctx struct{ open, close string }
	contexts := []ctx{
		{"(program ", ")"},
		{`(program (list . (symbol) @_m `, ` (#any-of? @_m "module" "module*" "module+")))`},
	}
	var forms []string
	add := func(f string) { forms = append(forms, f) }

	// function: (define (f args) ..), (define-syntax-rule (m a) ..)
	add(`(list . (symbol) @_k . (list . (symbol) @name)
    (#any-of? @_k "define" "define/contract" "define-inline" "define-syntax" "define-syntax-rule" "define/match" "define/generic")) @definition.function`)
	// function, curried: (define ((f a) b) ..)
	add(`(list . (symbol) @_k . (list . (list . (symbol) @name))
    (#any-of? @_k "define" "define/contract")) @definition.function`)
	// function: (define f (lambda ..)), (define-syntax m (syntax-rules ..))
	add(`(list . (symbol) @_k . (symbol) @name . (list . (symbol) @_v)
    (#any-of? @_k "define" "define/contract")
    (#any-of? @_v ` + racketLambdas + `)) @definition.function`)
	add(`(list . (symbol) @_k . (symbol) @name
    (#eq? @_k "define-syntax")) @definition.function`)
	// class: (define C (class ..))
	add(`(list . (symbol) @_k . (symbol) @name . (list . (symbol) @_v)
    (#eq? @_k "define")
    (#any-of? @_v "class" "class*")) @definition.class`)
	// variable: (define x value), unless the value makes it a function or a class
	add(`(list . (symbol) @_k . (symbol) @name
    (#any-of? @_k "define" "define/contract")
    (#not-match? @definition.variable "^\\(define(/contract)?\\s+[^\\s()]+\\s*\\(\\s*(lambda|λ|case-lambda|curry|curryr|class\\*?)[\\s)]")) @definition.variable`)
	// variables: (define-values (a b) ..)
	add(`(list . (symbol) @_k . (list (symbol) @name) @definition.variable
    (#eq? @_k "define-values"))`)
	// classes: struct and define-struct
	add(`(list . (symbol) @_k . (symbol) @name
    (#any-of? @_k "struct" "define-struct")) @definition.class`)
	add(`(list . (symbol) @_k . (list . (symbol) @name)
    (#eq? @_k "define-struct")) @definition.class`)
	// interface: define-generics, and the methods it lists
	add(`(list . (symbol) @_k . (symbol) @name
    (#eq? @_k "define-generics")) @definition.interface`)
	add(`(list . (symbol) @_k . (symbol)
    (list . (symbol) @name) @definition.method
    (#eq? @_k "define-generics"))`)

	var out []string
	for _, c := range contexts {
		for _, f := range forms {
			out = append(out, c.open+f+c.close)
		}
	}
	// packages
	out = append(out, `(program (list . (symbol) @_k . (symbol) @name (#any-of? @_k "module" "module*" "module+")) @definition.module)`)
	// methods of a class written inline in a top-level define
	out = append(out, `(program (list . (symbol) @_k . (symbol)
    (list . (symbol) @_c
      (list . (symbol) @_m . (list . (symbol) @name)) @definition.method)
    (#eq? @_k "define")
    (#any-of? @_c "class" "class*")
    (#match? @_m "^define/(public|private|override|augment|pubment|overment|augride)(-final)?$")))`)
	return "\n" + strings.Join(out, "\n") + "\n"
}
