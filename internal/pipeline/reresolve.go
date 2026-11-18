package pipeline

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Which references an incremental pass resolves again
//
// THE REQUIREMENT. After an incremental pass the graph must be the one a full
// index of the same tree would build, and the pass must cost what changed, not
// what the repository holds. Resolution binds only references marked pending,
// so the whole question is which references to mark.
//
// It used to be "the references in changed files, and the references bound to
// a definition in one". Everything else kept its old outcome, and three kinds
// of reference were left wrong: one that had found nothing and now would (a
// file added a definition of its name: 0 edges where a full index has 4), one
// that had found something through a file that now says otherwise (a barrel
// dropped an export, a DESCRIPTION was deleted, a function's return type
// changed), and one whose class lost or gained a supertype. `resolve
// --re-resolve` reset only the 'resolved' ones, so it repaired none of the
// first kind either.
//
// THE UNIT IS A FILE. A reference is reset by resetting its whole file
// (registry.ResetFileResolutions), never alone: the call that typed a variable
// is looked up among the calls bound in the same pass over the same file, and
// two references on one line share one edge row. A file is selected when any
// one of these holds:
//
//  1. It changed or was added. (It is re-parsed, so this was always so.)
//  2. It holds a reference that is pending for any reason, or evidences an
//     edge that touched a definition of a changed or deleted file. Replacing a
//     file resets the references bound into it, and those references' files
//     must then be resolved whole, not one reference at a time.
//  3. It references, by name or by any segment of a receiver or of an import,
//     a name whose DEFINITIONS changed: a definition added, removed, or
//     changed in any field a candidate is judged by (registry.SymbolSignature).
//     This is what binds a new definition's old callers (7a) and unbinds a
//     deleted one's (7e), in any outcome: resolved, ambiguous or unresolved.
//     A file whose definitions changed only in their bodies changes no
//     signature, so an ordinary edit selects nothing here.
//  4. It depends on a changed file in a way names do not show:
//     - it imports the file, or imports through one that re-exports it (a Dart
//       barrel or part, a C header), when that file was added, deleted, or
//       changed what it imports;
//     - a package the repository declares (an R DESCRIPTION) appeared, went or
//       moved: every reference qualified by that package name;
//     - it holds a call bound to a definition whose declared return type names
//       a changed type (the producer a variable's type is read from);
//     - a dependency a manifest declares appeared or went: every file whose
//       imports name it;
//     - the repository's layout moved: a file stem or a directory appeared or
//       went (every reference or import that names it), a project marker
//       appeared or went (every file under it, and every reference to a name
//       defined under it), or a go.mod or __init__.py did (every Go or Python
//       file: the whole class, because module and package roots decide how
//       every import of the language reads, and that is not scoped by name).
//  5. Its class's ancestry moved. This one cannot be known before resolving,
//     because the EXTENDS edge that says so is what resolution writes. So the
//     selected files are resolved, the supertype relations they evidence are
//     compared with what they were, and the members of every supertype that
//     came or went (with their own ancestors) become changed names for rule 3.
//     That repeats until nothing moves, or a bound on rounds is hit, past which
//     every file is resolved again rather than leave the answer unconverged.
//
// What a file is reset to is what a first resolve sees: every reference
// pending and none of the resolver's edges. A reference a compiler settled is
// left alone, because resolution does not re-create it; the compiler passes
// that do run after resolution.
//
// COST. Rules 1, 2 and 4 cost what changed. Rule 3 reads the run's distinct
// receivers and imports once (an index scan) when a signature changed, which a
// body-only edit never does, and selects only the files that name one of the
// changed names. Rule 5 costs a second resolve when, and only when, a
// supertype relation actually moved.

// maxAncestryRounds bounds rule 5. Each round can only follow supertype
// relations that the previous one changed, so real code settles in one or two.
const maxAncestryRounds = 4

// reresolvePlan is what an incremental pass knows about its changes before and
// after writing them, for choosing what to resolve again.
type reresolvePlan struct {
	kinds map[string]ChangeKind

	before map[string]registry.FileSnapshot
	after  map[string]registry.FileSnapshot

	// touching is the files evidencing an edge into a changed or deleted
	// file's definitions, read before the cascade removed it.
	touching []string
	// inheritance is the supertype relations touching changed and deleted
	// files, read before they were replaced.
	inheritance []registry.InheritancePair
	// oldPaths is the run's file list before the pass; read only when files
	// are added or deleted, which is when the layout can move.
	oldPaths []string
}

// planReresolve reads what the committed run holds for the files a pass is
// about to replace or delete.
func planReresolve(ctx context.Context, reg *registry.Registry, runID int64, changes []FileChange) (*reresolvePlan, error) {
	p := &reresolvePlan{
		kinds:  map[string]ChangeKind{},
		before: map[string]registry.FileSnapshot{},
		after:  map[string]registry.FileSnapshot{},
	}
	var old []string
	layoutMoves := false
	for _, ch := range changes {
		switch ch.Kind {
		case ChangeAdded:
			layoutMoves = true
		case ChangeDeleted:
			layoutMoves = true
		case ChangeModified:
		default:
			continue
		}
		p.kinds[ch.Path] = ch.Kind
		if ch.Kind == ChangeAdded {
			continue
		}
		snap, ok, err := reg.SnapshotFile(ctx, runID, ch.Path)
		if err != nil {
			return nil, err
		}
		if ok {
			p.before[ch.Path] = snap
			old = append(old, ch.Path)
		}
	}
	if len(p.kinds) == 0 {
		return p, nil
	}
	var err error
	if p.touching, err = reg.EdgeEvidenceTouching(ctx, runID, old); err != nil {
		return nil, err
	}
	if p.inheritance, err = reg.HeuristicInheritance(ctx, runID, old); err != nil {
		return nil, err
	}
	if layoutMoves {
		files, err := reg.RunFiles(ctx, runID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			p.oldPaths = append(p.oldPaths, f.Path)
		}
	}
	return p, nil
}

// noteWritten records a replaced or added file's new definitions and imports.
func (p *reresolvePlan) noteWritten(f model.File, symbols []model.Symbol, imports []registry.Import) {
	snap := registry.FileSnapshot{Path: f.Path, Language: f.Language, Imports: imports}
	for _, s := range symbols {
		if string(s.Kind) == registry.FileCallerKind {
			continue
		}
		snap.Symbols = append(snap.Symbols, registry.SymbolSignature{
			Name: s.Name, Kind: string(s.Kind), FQN: s.FQN, Exported: s.Exported,
			ParamCount: int(s.ParamCount), ReturnType: s.ReturnType,
		})
	}
	p.after[f.Path] = snap
}

// foldedLanguages are the languages whose names resolution compares without
// case (infer.LanguageSpec.FoldNames).
var foldedLanguages = []string{"powershell"}

// implicitNameLanguages are the languages in which a reference also looks up
// a name it does not spell (infer.CallTraits.ObjectsAreCallable: a call on a
// Scala object is a call of its apply). A change to that name selects every
// file of the language.
var implicitNameLanguages = map[string][]string{"apply": {"scala"}}

// passThroughExts are files whose importers also see what they import: C and
// C++ headers. Dart files are pass-through by language (export and part).
var passThroughExts = map[string]bool{
	".h": true, ".hh": true, ".hpp": true, ".hxx": true, ".h++": true, ".inc": true,
	".ipp": true, ".tpp": true, ".cuh": true,
}

// moduleStems are file stems that stand for their directory when imported.
var moduleStems = map[string]bool{
	"__init__": true, "index": true, "mod": true, "lib": true, "main": true, "init": true, "package": true,
}

// projectMarkers mirrors internal/resolve/infer/layout.go, which decides from
// them where one project of a monorepo ends.
var projectMarkers = map[string]bool{
	"package.json": true, "tsconfig.json": true, "deno.json": true, "deno.jsonc": true,
	"go.mod": true, "go.work": true, "pyproject.toml": true, "requirements.txt": true,
	"setup.py": true, "setup.cfg": true, "Pipfile": true, "pom.xml": true,
	"build.gradle": true, "build.gradle.kts": true, "Cargo.toml": true,
	"composer.json": true, "Gemfile": true, "Package.swift": true, "pubspec.yaml": true,
	"mix.exs": true, "rebar.config": true, "compile_commands.json": true,
	"CMakeLists.txt": true, "meson.build": true,
}

// selection is the files chosen for one round, by id.
type selection map[int64]struct{}

func (s selection) add(ids ...int64) {
	for _, id := range ids {
		s[id] = struct{}{}
	}
}

func (s selection) sorted() []int64 {
	out := make([]int64, 0, len(s))
	for id := range s {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// reresolveIncremental selects, resets and resolves; see the comment at the
// top of this file. It returns the paths of the files resolved again.
//
// Resolve runs at least once even when nothing is selected: its run-wide
// passes (route mounts, endpoint handlers, client calls, deployment units)
// must see a file that was added or deleted whether or not it holds a
// reference.
func reresolveIncremental(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	p *reresolvePlan,
) ([]string, error) {
	files, err := reg.RunFiles(ctx, runID)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]registry.FileRef, len(files))
	byID := make(map[int64]registry.FileRef, len(files))
	for _, f := range files {
		byPath[f.Path] = f
		byID[f.ID] = f
	}

	sel := selection{}

	// Rules 1 and 2.
	pending, err := reg.FilesWithPendingReferences(ctx, runID)
	if err != nil {
		return nil, err
	}
	sel.add(pending...)
	for path := range p.after {
		if f, ok := byPath[path]; ok {
			sel.add(f.ID)
		}
	}
	for _, path := range p.touching {
		if f, ok := byPath[path]; ok {
			sel.add(f.ID)
		}
	}

	// Rules 3 and 4, from what changed.
	names := map[string]struct{}{}
	deps := map[string]struct{}{}
	tokens := map[string]struct{}{}     // layout segments, lower-case
	importKeys := map[string]struct{}{} // what an import of a changed file ends with, lower-case
	classes := map[string]struct{}{}    // languages re-resolved whole
	var subtrees []string

	changed := make([]string, 0, len(p.kinds))
	for path := range p.kinds {
		changed = append(changed, path)
	}
	sort.Strings(changed)
	for _, rel := range changed {
		kind := p.kinds[rel]
		before, after := p.before[rel], p.after[rel]
		for _, n := range changedNames(before, after) {
			names[n.Name] = struct{}{}
			if n.Kind == "dependency" {
				deps[strings.ToLower(n.Name)] = struct{}{}
			}
		}
		if kind != ChangeModified || !sameImports(before.Imports, after.Imports) {
			for _, k := range importKeysOf(rel) {
				importKeys[k] = struct{}{}
			}
		}
		if kind == ChangeModified {
			continue
		}
		dir, base := path.Dir(rel), path.Base(rel)
		if dir == "." {
			dir = ""
		}
		switch base {
		case "go.mod", "go.work":
			classes["go"] = struct{}{}
		case "__init__.py", "__init__.pyi":
			classes["python"] = struct{}{}
		}
		if (projectMarkers[base] || strings.HasSuffix(base, ".csproj") || strings.HasSuffix(base, ".sln")) && dir != "" {
			subtrees = append(subtrees, dir)
		}
	}
	for t := range layoutTokens(p.oldPaths, files, len(p.oldPaths) > 0) {
		tokens[t] = struct{}{}
	}
	for _, dir := range subtrees {
		under, err := reg.SymbolNamesUnder(ctx, runID, dir)
		if err != nil {
			return nil, err
		}
		for _, n := range under {
			names[n] = struct{}{}
		}
		for _, f := range files {
			if strings.HasPrefix(f.Path, dir+"/") {
				sel.add(f.ID)
			}
		}
	}
	for n := range names {
		for _, lang := range implicitNameLanguages[n] {
			classes[lang] = struct{}{}
		}
	}
	for _, f := range files {
		if _, ok := classes[f.Language]; ok {
			sel.add(f.ID)
		}
	}

	byName, err := filesNaming(ctx, reg, runID, files, keys(names), keys(tokens))
	if err != nil {
		return nil, err
	}
	sel.add(byName...)

	if len(names) > 0 {
		ids, err := reg.FilesBoundToReturnTypes(ctx, runID, keys(names))
		if err != nil {
			return nil, err
		}
		sel.add(ids...)
	}

	importers, err := filesImporting(ctx, reg, runID, byID, names, tokens, deps, importKeys)
	if err != nil {
		return nil, err
	}
	sel.add(importers...)

	// The Go router mounts read only Go files and go.mod; with none of them
	// changed they would rewrite what is there. See ResolveOptions.
	goChanged := false
	for _, rel := range changed {
		if strings.HasSuffix(rel, ".go") || path.Base(rel) == "go.mod" || path.Base(rel) == "go.work" {
			goChanged = true
			break
		}
	}

	// Resolve, then follow any ancestry that moved (rule 5).
	done := selection{}
	inheritanceBefore := p.inheritance
	extraPaths := changed
	for round := 0; round == 0 || len(sel) > 0; round++ {
		if round == maxAncestryRounds {
			// Still moving after the bound: resolve everything not yet
			// resolved, so the result is converged even if the pass is not
			// cheap. Real hierarchies do not get here.
			for _, f := range files {
				if _, ok := done[f.ID]; !ok {
					sel.add(f.ID)
				}
			}
		}
		ids := sel.sorted()
		paths := make([]string, 0, len(ids)+len(extraPaths))
		for _, id := range ids {
			paths = append(paths, byID[id].Path)
		}
		paths = append(paths, extraPaths...)

		old, err := reg.HeuristicInheritance(ctx, runID, paths)
		if err != nil {
			return nil, err
		}
		old = append(old, inheritanceBefore...)
		if _, _, err := reg.ResetFileResolutions(ctx, runID, ids); err != nil {
			return nil, err
		}
		if _, err := Resolve(ctx, reg, runID, ResolveOptions{SkipGoMounts: !goChanged}); err != nil {
			return nil, err
		}
		for _, id := range ids {
			done[id] = struct{}{}
		}
		if round == maxAncestryRounds {
			break
		}
		now, err := reg.HeuristicInheritance(ctx, runID, paths)
		if err != nil {
			return nil, err
		}
		moved := movedSupertypes(old, now)
		if len(moved) == 0 {
			break
		}
		members, err := ancestryMembers(ctx, reg, runID, moved, old)
		if err != nil {
			return nil, err
		}
		next, err := filesNaming(ctx, reg, runID, files, members, nil)
		if err != nil {
			return nil, err
		}
		sel = selection{}
		for _, id := range next {
			if _, ok := done[id]; !ok {
				sel.add(id)
			}
		}
		inheritanceBefore, extraPaths = nil, nil
	}

	out := make([]string, 0, len(done))
	for id := range done {
		if f, ok := byID[id]; ok {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out, nil
}

// changedNames is the definitions present in one version of a file and not
// the other, compared by signature. A file whose language changed counts
// every definition of both versions.
func changedNames(before, after registry.FileSnapshot) []registry.SymbolSignature {
	if before.Language != after.Language {
		return append(append([]registry.SymbolSignature(nil), before.Symbols...), after.Symbols...)
	}
	count := map[string]int{}
	for _, s := range before.Symbols {
		count[s.Key()]++
	}
	for _, s := range after.Symbols {
		count[s.Key()]--
	}
	var out []registry.SymbolSignature
	for _, s := range before.Symbols {
		if count[s.Key()] > 0 {
			out = append(out, s)
		}
	}
	for _, s := range after.Symbols {
		if count[s.Key()] < 0 {
			out = append(out, s)
		}
	}
	return out
}

func sameImports(a, b []registry.Import) bool {
	key := func(imps []registry.Import) []string {
		out := make([]string, 0, len(imps))
		for _, i := range imps {
			out = append(out, i.Path+"\x00"+i.Alias)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// importKeysOf is what an import naming this file ends with: its stem, and
// for a file that stands for its directory, the directory's name.
func importKeysOf(p string) []string {
	base := path.Base(p)
	stem := strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))
	out := []string{stem}
	if moduleStems[stem] {
		if d := path.Base(path.Dir(p)); d != "." && d != "/" {
			out = append(out, strings.ToLower(d))
		}
	}
	return out
}

// segments splits an import path, a receiver or a qualified name into the
// names it is written with, lower-cased.
func segments(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		switch r {
		case '/', '\\', '.', ':', '>', '-', '$', '@', '(', ')', '*', '&', '<', ' ', ',', '\'', '"':
			return true
		}
		return false
	})
}

// lastSegments is the names the final element of an import path is written
// with ("foo.js" gives foo and js; "com.x.Later" gives com, x and later).
func lastSegments(s string) []string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return segments(s)
}

// layoutTokens is the file stems and directory names present in one file list
// and not the other: what infer.Layout answers HasStem, HasDirNamed and
// HasDirEnding from. Lower-case, as the layout compares them.
func layoutTokens(oldPaths []string, now []registry.FileRef, moved bool) map[string]struct{} {
	out := map[string]struct{}{}
	if !moved {
		return out
	}
	facts := func(paths []string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, p := range paths {
			base := path.Base(p)
			m["stem\x00"+strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))] = struct{}{}
			d := path.Dir(p)
			for d != "." && d != "/" && d != "" {
				m["dir\x00"+d] = struct{}{}
				d = path.Dir(d)
			}
		}
		return m
	}
	newPaths := make([]string, 0, len(now))
	for _, f := range now {
		newPaths = append(newPaths, f.Path)
	}
	a, b := facts(oldPaths), facts(newPaths)
	diff := func(x, y map[string]struct{}) {
		for k := range x {
			if _, ok := y[k]; ok {
				continue
			}
			_, v, _ := strings.Cut(k, "\x00")
			for _, s := range segments(v) {
				out[s] = struct{}{}
			}
		}
	}
	diff(a, b)
	diff(b, a)
	return out
}

// filesNaming is rule 3: the files with a reference written with one of names,
// or on a receiver one of whose segments is one of names or tokens.
func filesNaming(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	files []registry.FileRef,
	names, tokens []string,
) ([]int64, error) {
	if len(names) == 0 && len(tokens) == 0 {
		return nil, nil
	}
	var out []int64
	ids, err := reg.FilesReferencingNames(ctx, runID, names)
	if err != nil {
		return nil, err
	}
	out = append(out, ids...)

	want := map[string]struct{}{}
	lowered := make([]string, 0, len(names))
	for _, n := range names {
		l := strings.ToLower(n)
		lowered = append(lowered, l)
		for _, s := range segments(l) {
			want[s] = struct{}{}
		}
	}
	for _, t := range tokens {
		want[t] = struct{}{}
	}

	var folded []string
	for _, lang := range foldedLanguages {
		for _, f := range files {
			if f.Language == lang {
				folded = append(folded, lang)
				break
			}
		}
	}
	if ids, err = reg.FilesReferencingNamesFolded(ctx, runID, folded, lowered); err != nil {
		return nil, err
	}
	out = append(out, ids...)

	receivers, err := reg.DistinctReceivers(ctx, runID)
	if err != nil {
		return nil, err
	}
	var hit []string
	for _, r := range receivers {
		for _, s := range segments(r) {
			if _, ok := want[s]; ok {
				hit = append(hit, r)
				break
			}
		}
	}
	if ids, err = reg.FilesWithReceivers(ctx, runID, hit); err != nil {
		return nil, err
	}
	return append(out, ids...), nil
}

// filesImporting is the import half of rules 3 and 4: files whose imports name
// a changed name, a layout token or a dependency that moved, and files that
// import a changed file, followed through the files that pass what they
// import on to their own importers.
func filesImporting(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	byID map[int64]registry.FileRef,
	names, tokens, deps, importKeys map[string]struct{},
) ([]int64, error) {
	if len(names) == 0 && len(tokens) == 0 && len(deps) == 0 && len(importKeys) == 0 {
		return nil, nil
	}
	imports, err := reg.DistinctImportRows(ctx, runID)
	if err != nil {
		return nil, err
	}
	wanted := map[string]struct{}{}
	for n := range names {
		for _, s := range segments(n) {
			wanted[s] = struct{}{}
		}
	}
	for t := range tokens {
		wanted[t] = struct{}{}
	}

	matched := map[string]struct{}{}
	for _, imp := range imports {
		if hasAny(segments(imp.Path), wanted) || hasAny(segments(imp.Alias), wanted) || declares(deps, imp.Path) {
			matched[imp.Path] = struct{}{}
		}
	}

	// The importers of each changed file, then the importers of each of those
	// that passes its imports on, until no new file is reached.
	seen := selection{}
	keysSeen := map[string]struct{}{}
	frontier := importKeys
	for first := true; first || len(frontier) > 0; first = false {
		for k := range frontier {
			keysSeen[k] = struct{}{}
		}
		for _, imp := range imports {
			if hasAny(lastSegments(imp.Path), frontier) {
				matched[imp.Path] = struct{}{}
			}
		}
		ids, err := reg.FileIDsImporting(ctx, runID, keys(matched))
		if err != nil {
			return nil, err
		}
		next := map[string]struct{}{}
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen.add(id)
			f := byID[id]
			if f.Language == "dart" || passThroughExts[strings.ToLower(path.Ext(f.Path))] {
				for _, k := range importKeysOf(f.Path) {
					if _, ok := keysSeen[k]; !ok {
						next[k] = struct{}{}
					}
				}
			}
		}
		frontier = next
	}
	return seen.sorted(), nil
}

// declares is infer.Layout.DeclaresDependency for a set of dependency names
// that moved: the import path or one of its parent paths is one.
func declares(deps map[string]struct{}, importPath string) bool {
	if len(deps) == 0 {
		return false
	}
	p := strings.ToLower(importPath)
	for {
		if _, ok := deps[p]; ok {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i <= 0 {
			return false
		}
		p = p[:i]
	}
}

func hasAny(ss []string, set map[string]struct{}) bool {
	for _, s := range ss {
		if _, ok := set[s]; ok {
			return true
		}
	}
	return false
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// movedSupertypes is the supertype relations in one set and not the other.
func movedSupertypes(before, after []registry.InheritancePair) []registry.InheritancePair {
	key := func(p registry.InheritancePair) string { return p.Kind + "\x00" + p.Sub + "\x00" + p.Super }
	a, b := map[string]registry.InheritancePair{}, map[string]registry.InheritancePair{}
	for _, p := range before {
		a[key(p)] = p
	}
	for _, p := range after {
		b[key(p)] = p
	}
	var out []registry.InheritancePair
	for k, p := range a {
		if _, ok := b[k]; !ok {
			out = append(out, p)
		}
	}
	for k, p := range b {
		if _, ok := a[k]; !ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// ancestryMembers is the names a moved supertype relation can change the
// binding of: every member of the supertypes that came or went, and of all
// their own ancestors, as the run holds them now or held them before.
func ancestryMembers(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	moved, before []registry.InheritancePair,
) ([]string, error) {
	was := map[string][]string{}
	for _, p := range before {
		was[p.Sub] = append(was[p.Sub], p.Super)
	}
	types := map[string]struct{}{}
	var frontier []string
	for _, m := range moved {
		if _, ok := types[m.Super]; !ok {
			types[m.Super] = struct{}{}
			frontier = append(frontier, m.Super)
		}
	}
	for depth := 0; len(frontier) > 0 && depth < 32; depth++ {
		sup, err := reg.Supertypes(ctx, runID, frontier)
		if err != nil {
			return nil, fmt.Errorf("ancestry of moved supertypes: %w", err)
		}
		var next []string
		for _, t := range frontier {
			for _, s := range append(append([]string(nil), sup[t]...), was[t]...) {
				if _, ok := types[s]; !ok {
					types[s] = struct{}{}
					next = append(next, s)
				}
			}
		}
		frontier = next
	}
	return reg.NamesInFilesDefining(ctx, runID, keys(types))
}
