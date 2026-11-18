package infer

import (
	"fmt"
	"path"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ownPackageCall binds `pkg::f()` where pkg is a package the repository
// declares itself.
//
// What a package's `f` is: a function defined in one of its R/ files, which
// share one namespace. Nothing else qualifies, and that is the point of the
// rule: a script directory's `f`, a test helper's `f` and another package's
// `f` are not `pkg::f`, however well the name matches. A name R/ does not
// define is unresolved (an export from a file the index never saw, or a
// generated one), not another package's and not external.
//
// Two definitions in R/ are the package's last-collated one and nothing here
// knows the collation order, so the call is left ambiguous.
func (c call) ownPackageCall(pkg string, dirs []string) decision {
	var here []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if !isModuleLevel(s) || !inNamespaceDir(s.FilePath, dirs, c.spec.NamespaceDirs) {
			continue
		}
		here = append(here, s)
	}
	switch len(here) {
	case 0:
		return unresolvedCall
	case 1:
		return bound(candidate{
			entry:      here[0],
			confidence: model.ConfidenceHigh,
			reason:     fmt.Sprintf("qualified by %s, the package this repository declares", pkg),
		})
	}
	if first, ok := firstDeclaration(here); ok {
		return bound(candidate{
			entry:      first,
			confidence: model.ConfidenceHigh,
			reason:     fmt.Sprintf("qualified by %s, the package this repository declares", pkg),
		})
	}
	return ambiguousCall
}

// inNamespaceDir reports whether a file sits directly in a namespace
// directory (R/) of one of the package directories.
func inNamespaceDir(file string, pkgDirs, namespaceDirs []string) bool {
	dir := path.Dir(file)
	for _, p := range pkgDirs {
		for _, n := range namespaceDirs {
			if dir == path.Join(p, n) {
				return true
			}
		}
	}
	return false
}
