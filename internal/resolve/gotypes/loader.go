package gotypes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures one Check.
type Options struct {
	// Root is the repository root. Required.
	Root string

	// Files are the repo-relative, slash-separated paths of the Go files the
	// index holds. When non-nil only directories containing one of them are
	// loaded, and only those files carry facts: a file the index does not hold
	// has no symbol for an edge to land on. Nil means every Go file found.
	Files []string

	// Env reads an environment variable; nil means os.Getenv. A parameter so a
	// test can point GOMODCACHE at an empty directory without touching the
	// process.
	Env func(string) string

	// Timeout bounds the whole run; zero means DefaultTimeout.
	Timeout time.Duration

	// MemoryLimit is the heap, after a collection, above which no further
	// dependency is loaded from source (the rest become stubs and the result
	// says so). Zero means DefaultMemoryLimit.
	MemoryLimit uint64

	// Overlay replaces the content of repository files, keyed by their
	// repo-relative slash-separated path, for this run only: a patch is
	// type-checked without being written to disk. A file the overlay names
	// must already exist; one it adds is not discovered.
	Overlay map[string][]byte

	// CollectErrors records every hard type error in a repository file in
	// Result.TypeErrors, for a caller that needs to know what does not
	// compile and where.
	CollectErrors bool
}

const (
	// DefaultTimeout is the ceiling on one run. The standard-library closure
	// of a typical program checks in a few seconds and a very large module in
	// well under a minute (see the measurements in the change that added this
	// package); five minutes is for a pathological repository, after which the
	// run stops and reports what it has rather than holding resolve open.
	DefaultTimeout = 5 * time.Minute

	// DefaultMemoryLimit is the live heap at which dependency loading stops.
	// Function bodies of dependencies are never parsed (see stripBodies), so
	// the live set is the declarations of what is imported; 2 GiB is several
	// times the largest closure measured and well under a developer machine.
	DefaultMemoryLimit = 2 << 30
)

// Check type-checks every Go package of the repository and returns the facts
// the compiler's own front end proves about it. It uses the standard library
// only: no `go list`, no child process, no network. See the package comment.
func Check(ctx context.Context, opts Options) (*Result, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("gotypes: no repository root")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("gotypes: %w", err)
	}
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := opts.MemoryLimit
	if limit == 0 {
		limit = DefaultMemoryLimit
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	l := &loader{
		ctx:       ctx,
		root:      root,
		fset:      token.NewFileSet(),
		env:       env,
		limit:     limit,
		modcache:  findModCache(env),
		ext:       map[string]*extEntry{},
		inprog:    map[string]bool{},
		stubs:     map[string]*types.Package{},
		stubSet:   map[*types.Package]bool{},
		missing:   map[string]struct{}{},
		repoFile:  map[string]string{},
		localDefs: map[string]bool{},
		report:    map[string]bool{},
		allPath:   map[string]*unit{},
		out:       newBuilder(),
	}
	if len(opts.Overlay) > 0 {
		l.overlay = make(map[string][]byte, len(opts.Overlay))
		for rel, src := range opts.Overlay {
			l.overlay[filepath.Join(root, filepath.FromSlash(rel))] = src
		}
	}
	l.collect = opts.CollectErrors
	l.host = build.Default
	l.pure = build.Default
	l.pure.CgoEnabled = false
	l.stats.GOOS, l.stats.GOARCH = l.host.GOOS, l.host.GOARCH
	l.stats.ModCache = l.modcache
	l.sizes = types.SizesFor("gc", l.host.GOARCH)
	if opts.Files != nil {
		l.only = make(map[string]bool, len(opts.Files))
		for _, f := range opts.Files {
			l.only[f] = true
		}
	}

	l.work = loadWorkspace(root)
	l.discover(opts.Files)
	l.goroot = findGoroot(env, l.modcache, l.requiredGo())
	l.stats.Goroot = l.goroot

	for _, u := range l.units {
		if ctx.Err() != nil {
			break
		}
		l.process(u)
	}
	if ctx.Err() != nil {
		l.stats.Aborted = fmt.Sprintf("stopped at the %s time ceiling; packages not yet checked keep their heuristic edges", timeout)
	}

	l.implementations()

	for _, u := range l.units {
		l.stats.tally(u)
	}
	l.stats.finish(l.missing)
	l.stats.Elapsed = time.Since(start)
	res := l.out.result()
	res.Stats = l.stats
	res.TypeErrors = l.typeErrs
	return res, nil
}

// loader holds one run's state. Nothing here is shared between runs.
type loader struct {
	ctx      context.Context
	root     string
	overlay  map[string][]byte // by absolute path
	collect  bool
	typeErrs []TypeError
	errMu    sync.Mutex
	fset     *token.FileSet
	env      func(string) string
	limit    uint64
	goroot   string
	modcache string
	host     build.Context // repository files: the host platform as the go tool sees it
	pure     build.Context // dependencies and std: pure Go, no cgo
	sizes    types.Sizes
	only     map[string]bool // indexed files, or nil
	work     *workspace      // go.work at the root, or nil

	mods    []*module
	units   []*unit
	allPath map[string]*unit // import path -> unit, first module wins

	ext     map[string]*extEntry // directory -> dependency package
	inprog  map[string]bool
	stubs   map[string]*types.Package
	stubSet map[*types.Package]bool
	missing map[string]struct{}

	localDefs map[string]bool
	repoFile  map[string]string // absolute filename -> repo-relative, every file of every unit
	report    map[string]bool   // repo-relative files that may carry facts

	extCount   int
	heapTick   int
	heapPeak   uint64
	memStopped bool

	stats Stats
	out   *builder
	impl  []implCandidate
}

type module struct {
	dir       string
	file      modFile
	goVersion string
	vendor    string
	byPath    map[string]*unit
	notfound  map[string]bool
	work      *workspace // the go.work this module is a member of, or nil
}

// requirements is what the module's dependencies are built at: its own go.mod,
// or, inside a workspace, the highest version any member requires.
func (m *module) requirements() map[string]string {
	if m.work != nil {
		return m.work.require
	}
	return m.file.Require
}

// replacements is the replace directives that apply to the module.
func (m *module) replacements() []modReplace {
	if m.work != nil {
		return m.work.replace
	}
	return m.file.Replace
}

// unit is one directory: up to three type-checks, as the go tool does them.
type unit struct {
	dir        string
	rel        string
	mod        *module
	importPath string

	srcFiles []string // absolute, non-test, accepted by the host's build constraints
	tstFiles []string // absolute _test.go, accepted
	rejected int      // .go files the host's build constraints exclude
	pkgName  string

	splitDone  bool
	inPkgTests []string
	xtestTests []string

	res [3]*variantResult

	parseFailed bool
	stubbed     bool
	errors      int
	cgo         bool
}

type variant int

const (
	varPkg   variant = iota // the package's non-test files: what importers get
	varTest                 // the package plus its in-package tests
	varXTest                // the external _test package
)

type variantResult struct {
	pkg    *types.Package
	failed bool
}

type extEntry struct {
	pkg *types.Package
	err error
}

// discover finds the modules and the package directories under the root.
func (l *loader) discover(files []string) {
	dirs := map[string]bool{}
	if files != nil {
		for _, f := range files {
			if !strings.HasSuffix(f, ".go") || skipPath(f) {
				continue
			}
			dirs[path.Dir(f)] = true
		}
	} else {
		_ = filepath.WalkDir(l.root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // an unreadable entry is not part of the walk; the rest is
			}
			rel, _ := filepath.Rel(l.root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel != "." && skipPath(rel+"/x.go") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".go") {
				dirs[path.Dir(rel)] = true
			}
			return nil
		})
	}
	rels := make([]string, 0, len(dirs))
	for d := range dirs {
		rels = append(rels, d)
	}
	sort.Strings(rels)

	modAt := map[string]*module{}
	var findMod func(dir string) *module
	findMod = func(dir string) *module {
		if m, ok := modAt[dir]; ok {
			return m
		}
		var m *module
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil { //nolint:gosec // the repository's own go.mod
			mf := parseGoMod(data)
			m = &module{dir: dir, file: mf, byPath: map[string]*unit{}, notfound: map[string]bool{}}
			m.goVersion = l.langVersion(mf.GoVersion)
			if l.work != nil && l.work.dirs[dir] {
				m.work = l.work
			}
			if fi, err := os.Stat(filepath.Join(dir, "vendor")); err == nil && fi.IsDir() {
				m.vendor = filepath.Join(dir, "vendor")
			}
			l.mods = append(l.mods, m)
		} else if dir != l.root {
			m = findMod(filepath.Dir(dir))
		}
		modAt[dir] = m
		return m
	}

	for _, rel := range rels {
		dir := filepath.Join(l.root, filepath.FromSlash(rel))
		m := findMod(dir)
		if m == nil || m.file.Path == "" {
			l.stats.NoModule++
			continue
		}
		u := &unit{dir: dir, rel: rel, mod: m}
		sub, _ := filepath.Rel(m.dir, dir)
		sub = filepath.ToSlash(sub)
		u.importPath = m.file.Path
		if sub != "." {
			u.importPath = m.file.Path + "/" + sub
		}
		if !l.scanDir(u) {
			continue
		}
		l.units = append(l.units, u)
		if _, dup := m.byPath[u.importPath]; !dup {
			m.byPath[u.importPath] = u
		}
		if _, dup := l.allPath[u.importPath]; !dup {
			l.allPath[u.importPath] = u
		}
	}
	sort.Slice(l.units, func(i, j int) bool {
		return l.units[i].importPath+"|"+l.units[i].rel < l.units[j].importPath+"|"+l.units[j].rel
	})
	sort.Slice(l.mods, func(i, j int) bool { return l.mods[i].dir < l.mods[j].dir })
	l.stats.Modules = len(l.mods)
	l.stats.Packages = len(l.units)
}

// requiredGo is the newest Go release any module of the repository asks for in
// its go.mod `go` line, or "" when none does.
func (l *loader) requiredGo() string {
	need := ""
	for _, m := range l.mods {
		v := m.file.GoVersion
		if v == "" || !version.IsValid("go"+v) {
			continue
		}
		if need == "" || version.Compare("go"+v, "go"+need) > 0 {
			need = v
		}
	}
	return need
}

// skipPath reports whether a repo-relative file lives where the go tool does
// not look for packages: vendor, testdata, or a directory starting with '.' or
// '_'. node_modules is skipped too: it can hold Go files, never of this module.
func skipPath(rel string) bool {
	for _, part := range strings.Split(path.Dir(rel), "/") {
		switch {
		case part == "." || part == "":
		case part == "vendor" || part == "testdata" || part == "node_modules":
			return true
		case part[0] == '.' || part[0] == '_':
			return true
		}
	}
	return false
}

// scanDir lists the unit's Go files that the host platform's build
// constraints accept, and learns the package name. Files the constraints
// reject are counted and never read again.
func (l *loader) scanDir(u *unit) bool {
	ents, err := os.ReadDir(u.dir)
	if err != nil {
		return false
	}
	names := map[string]int{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") {
			continue
		}
		ok, err := l.host.MatchFile(u.dir, n)
		if err != nil || !ok {
			u.rejected++
			continue
		}
		abs := filepath.Join(u.dir, n)
		rel := path.Join(u.rel, n)
		if u.rel == "." {
			rel = n
		}
		l.repoFile[abs] = rel
		if l.only == nil || l.only[rel] {
			l.report[rel] = true
		}
		name, cgo := l.packageName(abs)
		if name == "" {
			continue
		}
		if cgo {
			u.cgo = true
		}
		if strings.HasSuffix(n, "_test.go") {
			u.tstFiles = append(u.tstFiles, abs)
			if base := strings.TrimSuffix(name, "_test"); base != "" {
				names[base]++
			}
			continue
		}
		u.srcFiles = append(u.srcFiles, abs)
		names[name]++
	}
	best, bestN := "", 0
	for n, c := range names {
		if c > bestN || (c == bestN && n < best) {
			best, bestN = n, c
		}
	}
	u.pkgName = best
	l.stats.FilesRejected += u.rejected
	return best != ""
}

// packageName reads only the package clause and the imports of a file.
func (l *loader) packageName(file string) (name string, cgo bool) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if f == nil || (err != nil && f.Name == nil) {
		return "", false
	}
	for _, im := range f.Imports {
		if im.Path != nil && im.Path.Value == `"C"` {
			cgo = true
		}
	}
	return f.Name.Name, cgo
}

// langVersion turns a go.mod `go` line into a go/types language version the
// running toolchain can honour, or "" for the toolchain's own.
func (l *loader) langVersion(v string) string {
	if v == "" {
		return ""
	}
	gv := "go" + v
	if !version.IsValid(gv) {
		return ""
	}
	if version.Compare(version.Lang(gv), version.Lang(runtime.Version())) > 0 {
		return ""
	}
	return version.Lang(gv)
}

// pos is where a token really is. fset.Position would apply //line
// directives, which generated code (cgo, yacc, templates) uses to point at the
// file it was generated from: a use reported at the generator's file and line
// is a fact about a place that does not hold it.
func (l *loader) pos(p token.Pos) token.Position { return l.fset.PositionFor(p, false) }

// process runs the checks of one directory and extracts the facts.
func (l *loader) process(u *unit) {
	if u.parseFailed {
		return
	}
	if len(u.srcFiles) > 0 {
		l.checkPkg(u)
	}
	if u.parseFailed {
		return
	}
	inPkg, xtest := l.splitTests(u)
	if len(inPkg) > 0 {
		l.checkVariant(u, varTest, nil, true)
	}
	if len(xtest) > 0 {
		under := u.res[varTest]
		if under == nil || under.pkg == nil {
			under = u.res[varPkg]
		}
		if under != nil && under.pkg != nil {
			ov := &override{path: u.importPath, pkg: under.pkg, rebuilt: map[string]*types.Package{}}
			l.checkVariant(u, varXTest, ov, true)
		}
	}
}

// splitTests separates a unit's test files into the in-package ones and the
// external `_test` package's, by package clause.
func (l *loader) splitTests(u *unit) (inPkg, xtest []string) {
	if u.splitDone {
		return u.inPkgTests, u.xtestTests
	}
	defer func() { u.splitDone, u.inPkgTests, u.xtestTests = true, inPkg, xtest }()
	for _, f := range u.tstFiles {
		name, _ := l.packageName(f)
		switch name {
		case u.pkgName:
			inPkg = append(inPkg, f)
		case u.pkgName + "_test":
			xtest = append(xtest, f)
		}
	}
	return inPkg, xtest
}

// checkPkg type-checks the non-test files of a unit once, with cycle
// detection. It is what an importer receives.
func (l *loader) checkPkg(u *unit) *variantResult {
	if r := u.res[varPkg]; r != nil {
		return r
	}
	key := "unit:" + u.dir
	if l.inprog[key] {
		return nil
	}
	l.inprog[key] = true
	defer delete(l.inprog, key)
	return l.checkVariant(u, varPkg, nil, true)
}

// checkVariant is one go/types run over one variant of a unit.
//
// override, when set, is an external test's view of the world: the package
// under test is the test-augmented one, and anything that imports it is
// re-checked against it, as the go tool recompiles dependants.
func (l *loader) checkVariant(u *unit, v variant, ov *override, extract bool) *variantResult {
	if extract && u.res[v] != nil {
		return u.res[v]
	}
	files, reportFiles := l.variantFiles(u, v)
	r := &variantResult{}
	if extract {
		u.res[v] = r
	}
	if len(files) == 0 {
		r.failed = true
		return r
	}
	asts, ok := l.parseAll(files, false, nil)
	if !ok {
		r.failed = true
		if extract {
			u.parseFailed = true
			l.stats.SkippedParse++
		}
		return r
	}

	flags := &importFlags{}
	imp := &importer{l: l, mod: u.mod, flags: flags, ov: ov, self: u}
	info := &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	hard := 0
	conf := types.Config{
		Importer:    imp,
		FakeImportC: true,
		GoVersion:   u.mod.goVersion,
		Sizes:       l.sizes,
		Error: func(err error) {
			var te types.Error
			if errors.As(err, &te) && te.Soft {
				return
			}
			hard++
			if l.collect && extract && errors.As(err, &te) {
				pos := te.Fset.Position(te.Pos)
				if rel, rerr := filepath.Rel(l.root, pos.Filename); rerr == nil && !strings.HasPrefix(rel, "..") {
					l.errMu.Lock()
					l.typeErrs = append(l.typeErrs, TypeError{Path: filepath.ToSlash(rel), Line: pos.Line, Col: pos.Column, Msg: te.Msg})
					l.errMu.Unlock()
				}
			}
			if len(l.stats.ErrorSamples) < 8 && extract {
				l.stats.ErrorSamples = append(l.stats.ErrorSamples, err.Error())
			}
		},
	}
	pkgPath := u.importPath
	if v == varXTest {
		pkgPath += "_test"
	}
	pkg := l.safeCheck(&conf, pkgPath, asts, info, &hard)
	r.pkg = pkg
	if extract {
		u.errors += hard
		if flags.stubbed {
			u.stubbed = true
		}
		l.stats.FilesChecked += len(files)
	}
	if extract && pkg != nil {
		x := &extractor{l: l, info: info, pkg: pkg}
		x.run(asts, reportFiles)
		l.collectImplCandidates(u, v, pkg)
	}
	return r
}

// safeCheck runs the checker and turns a panic inside it into an error count:
// a repository must not be able to take the whole resolve down.
func (l *loader) safeCheck(conf *types.Config, path string, files []*ast.File, info *types.Info, hard *int) (pkg *types.Package) {
	defer func() {
		if r := recover(); r != nil {
			*hard++
			if len(l.stats.ErrorSamples) < 8 {
				l.stats.ErrorSamples = append(l.stats.ErrorSamples, fmt.Sprintf("go/types panicked on %s: %v", path, r))
			}
		}
	}()
	pkg, _ = conf.Check(path, l.fset, files, info)
	return pkg
}

// variantFiles lists the files a variant checks and the subset that carries
// facts (the files only this variant is the authority for).
func (l *loader) variantFiles(u *unit, v variant) (files []string, report map[string]bool) {
	report = map[string]bool{}
	inPkg, xtest := l.splitTests(u)
	switch v {
	case varPkg:
		files = u.srcFiles
		for _, f := range files {
			report[f] = true
		}
	case varTest:
		files = append(append([]string(nil), u.srcFiles...), inPkg...)
		for _, f := range inPkg {
			report[f] = true
		}
	case varXTest:
		files = xtest
		for _, f := range xtest {
			report[f] = true
		}
	}
	return files, report
}

var parseWorkers = max(2, runtime.GOMAXPROCS(0))

// parseAll parses files in parallel and returns them in input order, so the
// result does not depend on scheduling. ok is false when any file did not
// parse: a package whose own files are not syntax is skipped, not guessed at.
// strip blanks function bodies first (dependencies only).
func (l *loader) parseAll(files []string, strip bool, data map[string][]byte) ([]*ast.File, bool) {
	out := make([]*ast.File, len(files))
	bad := make([]bool, len(files))
	sem := make(chan struct{}, parseWorkers)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			src, ok := data[f]
			if !ok {
				src, ok = l.overlay[f]
			}
			if !ok {
				var err error
				src, err = os.ReadFile(f) //nolint:gosec // a file of the repository or of the Go toolchain/module cache
				if err != nil {
					bad[i] = true
					return
				}
			}
			if strip {
				src = stripBodies(src)
			}
			af, err := parser.ParseFile(l.fset, f, src, parser.SkipObjectResolution)
			if err != nil || af == nil {
				bad[i] = true
				if strip && af != nil {
					out[i], bad[i] = af, false // a dependency keeps what parsed
				}
				return
			}
			out[i] = af
		}()
	}
	wg.Wait()
	kept := out[:0]
	for i := range out {
		if bad[i] {
			if !strip {
				return nil, false
			}
			continue
		}
		if out[i] != nil {
			kept = append(kept, out[i])
		}
	}
	return kept, true
}

// override is an external test package's view: path resolves to pkg, and any
// repository package that depends on path is checked again against it.
type override struct {
	path    string
	pkg     *types.Package
	rebuilt map[string]*types.Package
}

type importFlags struct{ stubbed bool }

type importer struct {
	l     *loader
	mod   *module
	flags *importFlags
	ov    *override
	std   bool // importing from the standard library: its vendor directory applies
	self  *unit
}

func (i *importer) Import(p string) (*types.Package, error) { return i.ImportFrom(p, "", 0) }

func (i *importer) ImportFrom(p, _ string, _ types.ImportMode) (*types.Package, error) {
	l := i.l
	if p == "unsafe" {
		return types.Unsafe, nil
	}
	if err := l.ctx.Err(); err != nil {
		return i.stub(p), nil //nolint:nilerr // a stopped run degrades to stubs, it does not fail the check
	}
	if !i.std {
		if u := i.repoUnit(p); u != nil {
			if pkg := i.importRepo(u, p); pkg != nil {
				return pkg, nil
			}
			return nil, fmt.Errorf("import %q: package could not be checked", p)
		}
	}
	if dir, std := l.locate(p, i); dir != "" {
		if pkg := l.loadExt(dir, p, std, i); pkg != nil {
			return pkg, nil
		}
	}
	return i.stub(p), nil
}

func (i *importer) repoUnit(p string) *unit {
	if i.mod != nil {
		if u := i.mod.byPath[p]; u != nil {
			return u
		}
	}
	if u := i.l.allPath[p]; u != nil {
		return u
	}
	return i.l.lazyUnit(i.mod, p)
}

// lazyUnit makes a unit for a package of a repository module that the index
// holds no file of (a directory it excludes, say), because an import of it
// must still be a real package and not a stub. It is checked when imported and
// carries no facts: none of its files is indexed.
func (l *loader) lazyUnit(m *module, p string) *unit {
	for _, cand := range append([]*module{m}, l.mods...) {
		if cand == nil || cand.file.Path == "" {
			continue
		}
		sub, ok := strings.CutPrefix(p, cand.file.Path+"/")
		if !ok && p != cand.file.Path {
			continue
		}
		dir := cand.dir
		if ok {
			dir = filepath.Join(cand.dir, filepath.FromSlash(sub))
		}
		if cand.notfound[p] || !hasGoFiles(dir) || skipPath(sub+"/x.go") {
			continue
		}
		if _, nested := os.Stat(filepath.Join(dir, "go.mod")); nested == nil && dir != cand.dir {
			continue
		}
		rel, err := filepath.Rel(l.root, dir)
		if err != nil {
			continue
		}
		u := &unit{dir: dir, rel: filepath.ToSlash(rel), mod: cand, importPath: p}
		if !l.scanDir(u) {
			cand.notfound[p] = true
			continue
		}
		cand.byPath[p] = u
		return u
	}
	return nil
}

func (i *importer) importRepo(u *unit, p string) *types.Package {
	l := i.l
	if i.ov != nil {
		if p == i.ov.path {
			return i.ov.pkg
		}
		if pkg, ok := i.ov.rebuilt[p]; ok {
			return pkg
		}
		if r := l.checkPkg(u); r != nil && r.pkg != nil && dependsOn(r.pkg, i.ov.path, map[*types.Package]bool{}) {
			key := "rebuild:" + p
			if l.inprog[key] {
				return nil
			}
			l.inprog[key] = true
			defer delete(l.inprog, key)
			r2 := l.checkVariant(u, varPkg, i.ov, false)
			i.ov.rebuilt[p] = r2.pkg
			return r2.pkg
		}
	}
	r := l.checkPkg(u)
	if r == nil || r.pkg == nil {
		return nil
	}
	return r.pkg
}

func dependsOn(p *types.Package, target string, seen map[*types.Package]bool) bool {
	if seen[p] {
		return false
	}
	seen[p] = true
	for _, im := range p.Imports() {
		if im.Path() == target || dependsOn(im, target, seen) {
			return true
		}
	}
	return false
}

// stub is the last resort for an import that cannot be found: an empty,
// complete package, so that checking continues and every use of it is an
// error the checker records and does not resolve.
func (i *importer) stub(p string) *types.Package {
	l := i.l
	if i.flags != nil {
		i.flags.stubbed = true
	}
	l.missing[p] = struct{}{}
	if s, ok := l.stubs[p]; ok {
		return s
	}
	s := types.NewPackage(p, guessName(p))
	s.MarkComplete()
	l.stubs[p] = s
	l.stubSet[s] = true
	return s
}

// guessName names a stub the way importers conventionally name a package:
// the last element, minus a major-version suffix, a "go-" prefix or ".go"
// suffix.
func guessName(p string) string {
	elems := strings.Split(p, "/")
	n := elems[len(elems)-1]
	if len(elems) > 1 && len(n) >= 2 && n[0] == 'v' && strings.Trim(n[1:], "0123456789") == "" {
		n = elems[len(elems)-2]
	}
	if i := strings.Index(n, ".v"); i > 0 {
		n = n[:i]
	}
	n = strings.TrimPrefix(n, "go-")
	n = strings.TrimSuffix(strings.TrimSuffix(n, ".go"), "-go")
	n = strings.NewReplacer("-", "_", ".", "_").Replace(n)
	if n == "" {
		return "stub"
	}
	return n
}

// locate finds where an import that is not a repository package lives:
// the standard library, then the repository's vendor directory, then the
// module cache. It returns the directory and whether it is the standard
// library, or "" when none has it.
func (l *loader) locate(p string, i *importer) (dir string, std bool) {
	if l.goroot != "" {
		if i.std {
			if d := filepath.Join(l.goroot, "src", "vendor", filepath.FromSlash(p)); hasGoFiles(d) {
				return d, true
			}
		}
		first := p
		if k := strings.Index(p, "/"); k >= 0 {
			first = p[:k]
		}
		if !strings.Contains(first, ".") {
			if d := filepath.Join(l.goroot, "src", filepath.FromSlash(p)); hasGoFiles(d) {
				return d, true
			}
		}
	}
	if i.std || i.mod == nil {
		return "", false
	}
	if i.mod.vendor != "" {
		if d := filepath.Join(i.mod.vendor, filepath.FromSlash(p)); hasGoFiles(d) {
			return d, false
		}
	}
	if d := l.moduleDir(i.mod, p); d != "" {
		return d, false
	}
	return "", false
}

// moduleDir resolves a dependency's directory the way the main module's go.mod
// says: the required version, with `replace` honoured (by version or by a
// local directory), in the module cache's case-escaped layout.
func (l *loader) moduleDir(m *module, p string) string {
	for cand := p; cand != "." && cand != ""; {
		ver, required := m.requirements()[cand]
		if required {
			suffix := strings.TrimPrefix(strings.TrimPrefix(p, cand), "/")
			modPath, modVer, local := cand, ver, ""
			for _, r := range m.replacements() {
				if r.Old != cand || (r.OldVersion != "" && r.OldVersion != ver) {
					continue
				}
				if strings.HasPrefix(r.New, "./") || strings.HasPrefix(r.New, "../") || filepath.IsAbs(r.New) {
					local = r.New
					if !filepath.IsAbs(local) {
						local = filepath.Join(m.dir, local)
					}
				} else {
					modPath, modVer = r.New, r.NewVersion
				}
				break
			}
			var base string
			switch {
			case local != "":
				base = local
			case l.modcache != "" && modVer != "":
				base = filepath.Join(l.modcache, escapePath(modPath)+"@"+modVer)
			}
			if base != "" {
				d := filepath.Join(base, filepath.FromSlash(suffix))
				if hasGoFiles(d) {
					return d
				}
			}
		}
		k := strings.LastIndex(cand, "/")
		if k < 0 {
			break
		}
		cand = cand[:k]
	}
	return ""
}

func hasGoFiles(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			return true
		}
	}
	return false
}

// loadExt type-checks a dependency or standard-library package from source,
// declarations only. Results are shared by directory.
func (l *loader) loadExt(dir, p string, std bool, from *importer) *types.Package {
	if e, ok := l.ext[dir]; ok {
		if e.err != nil {
			return nil
		}
		return e.pkg
	}
	if l.inprog[dir] {
		return nil
	}
	if l.memoryExceeded() {
		return nil
	}
	l.inprog[dir] = true
	defer delete(l.inprog, dir)

	ents, err := os.ReadDir(dir)
	if err != nil {
		l.ext[dir] = &extEntry{err: err}
		return nil
	}
	// Every candidate file is read ONCE: the build-constraint check and the
	// parse both work from these bytes. A closure of hundreds of packages is
	// tens of thousands of files and opening each twice was the largest single
	// cost of the run.
	var cands []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		cands = append(cands, n)
	}
	data := l.readAll(dir, cands)
	ctxt := l.pure
	ctxt.OpenFile = func(p string) (io.ReadCloser, error) {
		if b, ok := data[p]; ok {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return os.Open(p) //nolint:gosec // a file of the Go toolchain or module cache
	}
	var files []string
	for _, n := range cands {
		if ok, err := ctxt.MatchFile(dir, n); err != nil || !ok {
			continue
		}
		files = append(files, filepath.Join(dir, n))
	}
	asts, _ := l.parseAll(files, true, data)
	asts = majorityPackage(asts)
	if len(asts) == 0 {
		l.ext[dir] = &extEntry{err: fmt.Errorf("no Go files")}
		return nil
	}
	imp := &importer{l: l, mod: from.mod, flags: nil, std: std}
	errs := 0
	conf := types.Config{
		Importer:         imp,
		FakeImportC:      true,
		IgnoreFuncBodies: true,
		Sizes:            l.sizes,
		Error:            func(error) { errs++ },
	}
	pkg := l.safeCheck(&conf, p, asts, &types.Info{}, &errs)
	l.extCount++
	l.stats.Dependencies++
	if errs > 0 {
		l.stats.DependencyErrors++
	}
	l.ext[dir] = &extEntry{pkg: pkg}
	return pkg
}

// readAll reads the named files of a directory in parallel, keyed by the path
// MatchFile will ask for. A file that cannot be read is absent from the map.
func (l *loader) readAll(dir string, names []string) map[string][]byte {
	out := make(map[string][]byte, len(names))
	var mu sync.Mutex
	sem := make(chan struct{}, parseWorkers)
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p := filepath.Join(dir, n)
			b, err := os.ReadFile(p) //nolint:gosec // a file of the Go toolchain or module cache
			if err != nil {
				return
			}
			mu.Lock()
			out[p] = b
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// majorityPackage keeps the files of the most common package clause, so a
// stray `package main` or documentation file cannot poison a package.
func majorityPackage(fs []*ast.File) []*ast.File {
	count := map[string]int{}
	for _, f := range fs {
		count[f.Name.Name]++
	}
	best, n := "", 0
	for name, c := range count {
		if c > n || (c == n && name < best) {
			best, n = name, c
		}
	}
	out := fs[:0]
	for _, f := range fs {
		if f.Name.Name == best {
			out = append(out, f)
		}
	}
	return out
}

// memoryExceeded is the memory ceiling. Every few dependency loads it reads
// the heap; above the limit it collects once and re-reads, and only a live set
// still over the limit stops further loading (the rest become stubs, and the
// result says so).
func (l *loader) memoryExceeded() bool {
	if l.memStopped {
		return true
	}
	l.heapTick++
	if l.heapTick%8 != 0 {
		return false
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapInuse > l.heapPeak {
		l.heapPeak = ms.HeapInuse
		l.stats.PeakHeapMB = int(l.heapPeak >> 20)
	}
	if ms.HeapInuse <= l.limit {
		return false
	}
	runtime.GC()
	runtime.ReadMemStats(&ms)
	if ms.HeapInuse > l.limit {
		l.memStopped = true
		l.stats.Aborted = fmt.Sprintf("stopped loading dependencies at the %d MiB memory ceiling; the rest are stubs", l.limit>>20)
		return true
	}
	return false
}
