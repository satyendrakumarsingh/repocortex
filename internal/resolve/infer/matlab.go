package infer

import (
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # MATLAB: a function is reached by its file's name
//
// A MATLAB call asks no question about imports. `foo(x)` reaches the function the
// file `foo.m` defines, from any file, if that folder is on the path; the path is a
// list of folders, the current one first, and what a repository can say of it is
// where its files are. The language has three more rules, each about a folder:
//
//	foo.m             defines `foo`, the first function in the file. Every later
//	                  function of the file is local to it, called by that file alone
//	+pkg/foo.m        the function `pkg.foo`: called as `pkg.foo(x)`, or as `foo(x)`
//	                  after `import pkg.*` or `import pkg.foo`, and never as `foo(x)`
//	                  from a file that did not import it, not even a neighbour
//	@Cls/foo.m        a method of the class `Cls`, called on an object (`foo(obj)`
//	                  dispatches on its class), and no function of the path
//	private/foo.m     called by the files of the folder above `private`, and by the
//	                  files of `private`
//
// A classdef file `Account.m` defines the class (which is the constructor, called
// `Account(n)`, a method of the same name) and its `methods`, which are called on an
// object, or as `Account.make(n)` for a static one. The parser qualifies them
// (parse.matlabOwners) so that a method is told from a function of the path.
//
// What a call reaches, in the order the language asks:
//
//	f(x)         a function or method the file defines; one an `import` names; the
//	             language's own (corefunctions.go), which no file of the repository
//	             replaces except by being in the same file; the one function file of
//	             that name on the path
//	pkg.f(x)     the function f of the folder +pkg (+a/+b for `a.b.f`)
//	Cls.f(x)     the static method f of the class Cls
//
// Several files of one name on the path are told apart by their distance from the
// caller: the caller's own folder, then the nearest. A call that none of this
// accounts for (`obj.method(x)` on a value, a function of a toolbox that was not
// indexed) is unresolved, never a guess. A name the function binds as a variable
// is no call (parse.matlabBinds).
func (r *Resolver) callMatlab(c call, _ *Stats) decision {
	switch c.ref.ReceiverKind {
	case model.ReceiverBare:
		return r.callMatlabBare(c)
	case model.ReceiverName, model.ReceiverPath:
		return r.callMatlabQualified(c)
	}
	// A call on a value: nothing here says what class it is.
	return unresolvedCall
}

func (r *Resolver) callMatlabBare(c call) decision {
	var here, imported, onPath []registry.SymbolIndexEntry
	dirs := c.factsOf().moduleDirectives()
	for _, s := range c.cands {
		switch {
		case s.FileID == c.fr.FileID:
			here = append(here, s)
			continue
		case matlabPackage(s.FilePath) != "":
			for _, d := range dirs.imports {
				if d.module == matlabPackage(s.FilePath) && d.brings(c.ref) && matlabPrimary(s) {
					imported = append(imported, s)
				}
			}
			continue
		}
		if matlabOnPath(s, c.fr.Path) {
			onPath = append(onPath, s)
		}
	}

	if len(here) > 0 {
		return pickMatlab(c, here, "defined in the same file", model.ConfidenceHigh)
	}
	if len(imported) > 0 {
		return pickMatlab(c, imported, "imported from the package folder that holds it", model.ConfidenceHigh)
	}
	if c.spec.isCoreFunction(c.ref.Name) {
		return externalCall
	}
	if len(onPath) == 0 {
		return unresolvedCall
	}

	// Several files of one name: the caller's folder, then the nearest.
	files := map[string]int{}
	for _, s := range onPath {
		files[s.FilePath] = commonDirs(c.fr.Path, s.FilePath)
	}
	best, atBest := -1, 0
	for _, d := range files {
		switch {
		case d > best:
			best, atBest = d, 1
		case d == best:
			atBest++
		}
	}
	if atBest > 1 {
		return ambiguousCall
	}
	var nearest []registry.SymbolIndexEntry
	for _, s := range onPath {
		if files[s.FilePath] == best {
			nearest = append(nearest, s)
		}
	}
	// Where several files are named alike, which one is first on the path is the
	// order the user gave addpath, and the repository does not hold it. The caller's
	// own folder is what a script or a test runs from; any other is a guess.
	switch {
	case len(files) == 1:
		return pickMatlab(c, nearest, "the one function file of that name on the path", model.ConfidenceHigh)
	case path.Dir(nearest[0].FilePath) == path.Dir(c.fr.Path):
		return pickMatlab(c, nearest, "the function file in the caller's own folder, the first place a call looks", model.ConfidenceMedium)
	}
	return pickMatlab(c, nearest, "the nearest of several function files of that name", model.ConfidenceLow)
}

func (r *Resolver) callMatlabQualified(c call) decision {
	recv := c.ref.Receiver

	// A package function or class: `pkg.f(x)`, `a.b.f(x)`.
	var inPackage []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if matlabPackage(s.FilePath) == recv && matlabPrimary(s) {
			inPackage = append(inPackage, s)
		}
	}
	if len(inPackage) > 0 {
		return pickMatlab(c, inPackage, "qualified by the package folder that holds it ("+recv+")", model.ConfidenceHigh)
	}

	// A static method of a class: `Account.make(n)`, `pkg.Account.make(n)`.
	class, pkg := recv, ""
	if i := strings.LastIndexByte(recv, '.'); i >= 0 {
		class, pkg = recv[i+1:], recv[:i]
	}
	var methods []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.Kind != string(model.KindMethod) {
			continue
		}
		if owner, ok := memberOwner(s); ok && owner == class && matlabPackage(s.FilePath) == pkg {
			methods = append(methods, s)
		}
	}
	if len(methods) > 0 {
		return pickMatlab(c, methods, "a method of the class "+class, model.ConfidenceHigh)
	}

	if c.spec.hasExternalRoot(receiverSegments(recv)) {
		return externalCall
	}
	// A value, a struct, an object of a class the index does not hold.
	return unresolvedCall
}

// pickMatlab chooses among definitions of one name that the call can reach: a
// function or method before the class whose constructor it is, then the argument
// count where the declarations state one. Definitions in several files are not told
// apart here, and the caller narrows to one before it asks.
func pickMatlab(c call, among []registry.SymbolIndexEntry, reason string, confidence model.Confidence) decision {
	var callables []registry.SymbolIndexEntry
	for _, s := range among {
		if isCallableKind(s.Kind) {
			callables = append(callables, s)
		}
	}
	if len(callables) > 0 {
		among = callables
	}
	files := map[int64]struct{}{}
	for _, s := range among {
		files[s.FileID] = struct{}{}
	}
	if len(files) > 1 {
		return ambiguousCall
	}
	if len(among) == 1 {
		return bound(candidate{among[0], confidence, reason})
	}
	sortCandidates(among)
	if s, ok := preferByArity(c.ref, among); ok {
		return bound(candidate{s, confidence, reason + "; argument count selects one"})
	}
	return bound(candidate{among[0], model.ConfidenceLow, reason + " (several definitions of the name)"})
}

// matlabPackage is the package a file's folder makes it part of: `a.b` for a file in
// `+a/+b`, "" for a file in no package folder. Only the folders directly above the
// file count: a `+pkg` further up is a package of the folders beneath it, which are
// not files of the path either, and none of their functions is a bare name.
func matlabPackage(filePath string) string {
	dirs := strings.Split(path.Dir(filePath), "/")
	var pkg []string
	for i := len(dirs) - 1; i >= 0 && strings.HasPrefix(dirs[i], "+") && len(dirs[i]) > 1; i-- {
		pkg = append([]string{dirs[i][1:]}, pkg...)
	}
	return strings.Join(pkg, ".")
}

// matlabClassFolder is the class an `@Class` folder makes a file a method of.
func matlabClassFolder(filePath string) string {
	if base := path.Base(path.Dir(filePath)); strings.HasPrefix(base, "@") && len(base) > 1 {
		return base[1:]
	}
	return ""
}

// matlabPrimary reports whether a definition is the one its file is named for: the
// function of `foo.m` called `foo`, or the class of `Foo.m`, or its constructor.
// Every other definition of a file is local to it or a method.
func matlabPrimary(s registry.SymbolIndexEntry) bool {
	if fileBaseName(s.FilePath) != s.Name {
		return false
	}
	if s.Kind == string(model.KindMethod) {
		// The constructor of a classdef: a method named for its class.
		owner, ok := memberOwner(s)
		return ok && owner == s.Name
	}
	return s.Kind == string(model.KindFunction) || s.Kind == string(model.KindClass)
}

// matlabOnPath reports whether a file of the repository defines a function that a
// bare call in the file `from` reaches by the file's name.
func matlabOnPath(s registry.SymbolIndexEntry, from string) bool {
	if !matlabPrimary(s) {
		return false
	}
	if matlabPackage(s.FilePath) != "" {
		return false
	}
	if class := matlabClassFolder(s.FilePath); class != "" && class != s.Name {
		// A method file of an @Class folder is called on an object of the class.
		return false
	}
	dir := path.Dir(s.FilePath)
	if path.Base(dir) == "private" {
		above := path.Dir(dir)
		callerDir := path.Dir(from)
		return callerDir == above || callerDir == dir
	}
	return true
}

// commonDirs counts the leading folders two files share: the larger, the nearer.
func commonDirs(a, b string) int {
	as, bs := strings.Split(path.Dir(a), "/"), strings.Split(path.Dir(b), "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}
