package infer

import (
	"context"
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// # Sourcing and including, followed past the first file
//
// `source b.sh` in a.sh brings b's functions into a, and so does whatever b
// sources in turn: the shell reads c.sh while it reads b.sh, so a calls c's
// functions as freely as b's. A C-family `#include "b.h"` is the same, since the
// preprocessor pastes b.h's own includes into the translation unit. The same
// holds for dot-sourcing in PowerShell and for a shader's #include.
//
// The parser records only a file's own rows, so the import tier saw one level.
// With LanguageSpec.TransitiveImports a file's import set also carries the files
// its imports reach, each with how many steps away it is.
//
//   - A file is followed through the import rows of the file it names, found by
//     the path written (exact, or the one file ending in it: a row is recorded
//     both relative to the script and as written, and the one that names no file is
//     skipped). A row that names two files, or none, is not followed.
//   - Each file is visited once, nearest first, so a cycle ends where it closes
//     and a file's depth is its shortest. The walk stops at maxIncludeDepth and
//     keeps at most maxTransitiveFiles files, in the order found.
//   - A reached file is known exactly (its key is a whole repository path), so it
//     covers that file alone. A direct import is matched by the tail of its path,
//     which is how a path written without its directory still finds the file, and a
//     reached file is not: a decoy with the same stem under another directory is
//     not made to look imported.
//   - A nearer file wins over a farther one (importSet.coverage): a definition in
//     the file the caller sources beats the same name in a file only a sourced file
//     reaches. Among equals the usual order applies, path then symbol.
//
// Not followed: a path built from a variable, `Import-Module Name` (a module by
// name has no path), and an include a build adds with `-I` that no file writes.

// maxIncludeDepth bounds how far a chain is followed. Real include and source
// chains run three or four deep; the bound is for chains that are wrong.
const maxIncludeDepth = 8

// maxTransitiveFiles bounds the files one file reaches. A header that includes a
// project's whole tree would otherwise put thousands of entries in every
// includer's set.
const maxTransitiveFiles = 512

// loadImportGraph reads a language's import rows once.
func (r *Resolver) loadImportGraph(ctx context.Context, lang string) *reexportGraph {
	if g, done := r.importGraphs[lang]; done {
		return g
	}
	if r.importGraphs == nil {
		r.importGraphs = map[string]*reexportGraph{}
	}
	r.importGraphs[lang] = nil
	if src, ok := r.candidates.(ImportGraph); ok {
		if rows, err := src.ImportGraph(ctx, lang); err == nil && len(rows) > 0 {
			files := make(map[string]registry.FileDirectives, len(rows))
			for k, imports := range rows {
				files[k] = registry.FileDirectives{Exports: imports}
			}
			r.importGraphs[lang] = newReexportGraph(files)
		}
	}
	return r.importGraphs[lang]
}

// reachedFiles returns the files a file reaches through the files it imports,
// by key (path without extension), with the number of steps each is from the
// file: 1 for a file a direct import's file imports, and so on. A file the file
// imports itself is not here, nor is the file.
func (r *Resolver) reachedFiles(ctx context.Context, spec LanguageSpec, fr registry.FileRefs, direct []registry.Import) map[string]int {
	g := r.loadImportGraph(ctx, spec.Name)
	if g == nil {
		return nil
	}
	seen := map[string]struct{}{strings.TrimSuffix(fr.Path, path.Ext(fr.Path)): {}}
	var frontier []string
	enqueue := func(p string, out *[]string) bool {
		k, ok := g.lookupKey(p)
		if !ok {
			return false
		}
		if _, dup := seen[k]; dup {
			return false
		}
		seen[k] = struct{}{}
		*out = append(*out, k)
		return true
	}
	for _, imp := range direct {
		if strings.HasPrefix(imp.Alias, memberAliasPrefix) {
			continue
		}
		enqueue(imp.Path, &frontier)
	}
	var reached map[string]int
	add := func(k string, depth int) {
		if reached == nil {
			reached = map[string]int{}
		}
		if n, ok := reached[k]; !ok || depth < n {
			reached[k] = depth
		}
	}
	for depth := 1; depth <= maxIncludeDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, k := range frontier {
			for _, p := range g.files[k].Exports {
				if len(reached) >= maxTransitiveFiles {
					return reached
				}
				var found []string
				if !enqueue(p, &found) {
					continue
				}
				add(found[0], depth)
				next = append(next, found[0])
				if spec.HeaderPairs {
					for _, unit := range r.headerUnits(spec.Name, g, found[0]) {
						if _, dup := seen[unit]; !dup {
							seen[unit] = struct{}{}
							add(unit, depth)
						}
					}
				}
			}
		}
		frontier = next
	}
	return reached
}

// headerUnits are the files that define what a header declares: those of its
// stem, in any directory, that include it. `src/util.c` is the unit of
// `include/util.h`; a `third_party/util.c` that includes its own
// `third_party/util.h` is not. A unit that never includes its own header is not
// found: nothing says it is that header's.
func (r *Resolver) headerUnits(lang string, g *reexportGraph, header string) []string {
	if r.importStems == nil {
		r.importStems = map[string]map[string][]string{}
	}
	stems, ok := r.importStems[lang]
	if !ok {
		stems = make(map[string][]string, len(g.keys))
		for _, k := range g.keys {
			b := path.Base(k)
			stems[b] = append(stems[b], k)
		}
		r.importStems[lang] = stems
	}
	var including []string
	for _, k := range stems[path.Base(header)] {
		if k == header {
			continue
		}
		for _, p := range g.files[k].Exports {
			if got, ok := g.lookupKey(p); ok && got == header {
				including = append(including, k)
				break
			}
		}
	}
	return including
}

// ImportGraph is the optional port a Candidates source offers for the import
// rows of a language's files, keyed by each file's path without extension. A
// source without it follows nothing past a file's own imports, as before.
type ImportGraph interface {
	ImportGraph(ctx context.Context, language string) (map[string][]string, error)
}
