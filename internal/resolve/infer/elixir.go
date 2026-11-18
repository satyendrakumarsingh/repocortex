package infer

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Elixir: a call reaches a MODULE, and a module is a name
//
// Everything the generic rules in this package ask of a call is a question
// about files: which file does an import name, which file is beside this one,
// which file is the module a qualifier names. An Elixir call asks none of them.
// `Shop.Money.zero()` reaches the function `zero` of the module that is
// called `Shop.Money`, wherever the repository keeps it (`lib/shop/money.ex` by
// convention, three modules to a file by choice), and `alias`, `import` and
// `use` rename or bring in modules, never files. So the language has its own
// resolution, here, and resolveCall hands it every call whose spec is Elixir's
// and nothing else: no other language reaches this file, and nothing in it
// changes what they are asked.
//
// What it works from:
//
//   - A function's qualified name is `path:Module.name`, so its module is what
//     precedes the name (parse.elixirOwners), compared in full, never by its
//     last segment: `Shop.Money` and `Billing.Money` are two modules.
//   - A file's imports are directives (parse.ElixirAliasPrefix and the others):
//     `alias` binds a local name to a module, `import` brings a module's
//     functions in under their own names (all of them, or those `only:` lists),
//     `use` runs the module's `__using__`, whose effect no index holds.
//
// What a call reaches, in the order the compiler asks:
//
//	f(x)           a function of the module the call is written in; one a
//	               module imported by name defines; the language's own
//	               (Kernel, ExUnit), which a definition of the repository's does
//	               not replace; one of a module `use`d, at `low`
//	Mod.f(x)       the function f of the module Mod names: written in full, or
//	               its first segment an alias (`alias A.B` makes `B.f()` mean
//	               `A.B.f()`, and `as:` renames it), or `__MODULE__`
//
// A qualified call whose module the repository does not define (Enum, Logger,
// a dependency) leaves the repository.
func (r *Resolver) callElixir(c call, _ *Stats) decision {
	facts := c.factsOf()
	module := facts.elixirEnclosingModule(c.ref.Line)
	dirs := facts.elixirDirectives()

	if c.ref.ReceiverKind != model.ReceiverBare {
		target := elixirQualifier(c, module, dirs)
		var inModule []registry.SymbolIndexEntry
		for _, s := range c.cands {
			if elixirOwner(s) == target {
				inModule = append(inModule, s)
			}
		}
		if len(inModule) == 0 && !strings.Contains(c.ref.Receiver, ".") && module != "" {
			// A module declared inside the one the call is written in is named
			// by its last segment there: `defmodule Line` in `defmodule Cart`
			// is `Line` to Cart's own code, and only to code in the same file.
			for _, s := range c.cands {
				if s.FileID == c.fr.FileID && elixirOwner(s) == module+"."+c.ref.Receiver {
					inModule = append(inModule, s)
				}
			}
		}
		if len(inModule) > 0 {
			return elixirPick(c, inModule, "qualified by the module that defines it ("+target+")")
		}
		if c.spec.hasExternalRoot(receiverSegments(target)) {
			return externalCall
		}
		return unresolvedCall
	}

	// A bare call.
	var local []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if module != "" && s.FileID == c.fr.FileID && elixirOwner(s) == module {
			local = append(local, s)
		}
	}
	if len(local) > 0 {
		return elixirPick(c, local, "defined in the same module")
	}

	var imported []registry.SymbolIndexEntry
	for _, d := range dirs.imports {
		if d.only != nil && !d.only[c.ref.Name] {
			continue
		}
		for _, s := range c.cands {
			if elixirOwner(s) == d.module {
				imported = append(imported, s)
			}
		}
	}
	if len(imported) > 0 {
		return elixirPick(c, imported, "imported from the module")
	}

	if c.spec.isCoreFunction(c.ref.Name) {
		return unresolvedCall
	}

	var used []registry.SymbolIndexEntry
	for _, m := range dirs.uses {
		for _, s := range c.cands {
			if elixirOwner(s) == m {
				used = append(used, s)
			}
		}
	}
	if len(used) == 1 {
		return bound(candidate{used[0], model.ConfidenceLow, "defined by a module this one `use`s"})
	}
	return unresolvedCall
}

// elixirPick chooses among functions of one module that share a name: the
// arities of one function, or the same module declared in two files. One
// answer is `high`; several arities the call's argument count does not tell
// apart are the same function at `medium`; two modules, or one module in two
// files, are not told apart at all.
func elixirPick(c call, among []registry.SymbolIndexEntry, reason string) decision {
	modules := map[string]struct{}{}
	files := map[int64]struct{}{}
	for _, s := range among {
		modules[elixirOwner(s)] = struct{}{}
		files[s.FileID] = struct{}{}
	}
	if len(modules) > 1 || len(files) > 1 {
		return ambiguousCall
	}
	if len(among) == 1 {
		return bound(candidate{among[0], model.ConfidenceHigh, reason})
	}
	sortCandidates(among)
	if s, ok := preferByArity(c.ref, among); ok {
		return bound(candidate{s, model.ConfidenceHigh, reason + "; argument count selects the arity"})
	}
	return bound(candidate{among[0], model.ConfidenceMedium, reason + " (several arities)"})
}

// elixirOwner is the module a function is defined in: its qualified name less
// the file and its own name. "" for a symbol that is no member of a module.
func elixirOwner(c registry.SymbolIndexEntry) string {
	qual, ok := qualOf(c)
	if !ok || !strings.HasSuffix(qual, c.Name) {
		return ""
	}
	head := qual[:len(qual)-len(c.Name)]
	if len(head) < 2 || head[len(head)-1] != '.' {
		return ""
	}
	return head[:len(head)-1]
}

// elixirEnclosingModule is the module the code at a line is written in: the
// owner of the innermost function around it, or the innermost module itself
// for code in a module's body.
func elixirEnclosingModule(fr registry.FileRefs, line int) string {
	var inner *registry.EnclosingSymbol
	for i := range fr.Symbols {
		s := &fr.Symbols[i]
		if s.StartLine > line {
			break
		}
		end := s.EndLine
		if end == 0 {
			end = s.StartLine
		}
		if line <= end {
			inner = s
		}
	}
	if inner == nil {
		return ""
	}
	qual, _ := strings.CutPrefix(inner.FQN, fr.Path+":")
	switch inner.Kind {
	case string(model.KindPackage), string(model.KindInterface), string(model.KindClass):
		return qual
	}
	if !strings.HasSuffix(qual, inner.Name) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(qual, inner.Name), ".")
}

// elixirQualifier is the module a call's qualifier names, in full: an alias
// expanded, `__MODULE__` replaced by the module the call is in.
func elixirQualifier(c call, module string, dirs elixirDirectiveSet) string {
	written := c.ref.Receiver
	if written == "__MODULE__" {
		return module
	}
	head, rest, _ := strings.Cut(written, ".")
	if full, ok := dirs.aliases[head]; ok {
		if rest == "" {
			return full
		}
		return full + "." + rest
	}
	return written
}

// elixirDirectiveSet is a file's directives, read from its import rows.
type elixirDirectiveSet struct {
	aliases map[string]string
	imports []elixirImported
	uses    []string
}

type elixirImported struct {
	module string
	only   map[string]bool
}

func elixirDirectives(imports []registry.Import) elixirDirectiveSet {
	var set elixirDirectiveSet
	for _, imp := range imports {
		switch {
		case strings.HasPrefix(imp.Alias, parse.ElixirAliasPrefix):
			if set.aliases == nil {
				set.aliases = map[string]string{}
			}
			local := strings.TrimPrefix(imp.Alias, parse.ElixirAliasPrefix)
			if _, dup := set.aliases[local]; !dup {
				set.aliases[local] = imp.Path
			}
		case imp.Alias == parse.ElixirImportAlias:
			set.imports = append(set.imports, elixirImported{module: imp.Path})
		case strings.HasPrefix(imp.Alias, parse.ElixirImportPrefix):
			only := map[string]bool{}
			for _, n := range strings.Split(strings.TrimPrefix(imp.Alias, parse.ElixirImportPrefix), ",") {
				only[n] = true
			}
			set.imports = append(set.imports, elixirImported{module: imp.Path, only: only})
		case imp.Alias == parse.ElixirUseAlias:
			set.uses = append(set.uses, imp.Path)
		}
	}
	return set
}
