package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	acqfs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scipimport"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// CompilerIndexDir is where the compiler indexers' output is kept, under the
// repository root. It is the cache as well as the product: an index that is
// still valid is imported again without running the tool that wrote it.
const CompilerIndexDir = ".repocortex/scip"

// DefaultCompilerIndexTimeout bounds one indexer when the user has not chosen
// a limit of their own (accuracy.indexers.timeout). It was chosen from the
// measured runs in docs/BENCHMARKING.md: the slowest indexer on the largest
// repository measured finished in well under a minute, so five minutes leaves
// an order of magnitude of headroom for a cold machine without letting a hung
// build hold `resolve` for the ten minutes the indexers' own default allows.
const DefaultCompilerIndexTimeout = 5 * time.Minute

// cacheKeyVersion changes when the key's recipe does, so an old sidecar can
// never be mistaken for a current one.
const cacheKeyVersion = "2" // 2: inputs above the project (workspace roots) are keyed

// CompilerIndexOptions configures RunCompilerIndexers.
type CompilerIndexOptions struct {
	// Root is the repository root on disk.
	Root string
	// Config is the user's configuration. Which indexers may run, and how, comes
	// from here and never from the repository.
	Config config.Config
	// Runner reaches the operating system; nil means the real one.
	Runner indexer.Runner
	// Logger receives progress and the audit record of each subprocess.
	Logger *slog.Logger
	// Timeout bounds one indexer; zero means the smaller of the user's setting
	// and DefaultCompilerIndexTimeout.
	Timeout time.Duration
	// AllowRepoBuild is the user's explicit opt-in to indexers that run the
	// repository's own build (Java natively, Rust): `resolve --allow-repo-build`.
	AllowRepoBuild bool
	// Language, when set, limits the run to indexers of that language: for
	// tests.
	Language indexer.Language
}

// CompilerIndexStep is what happened to one project's compiler indexer.
type CompilerIndexStep struct {
	Language string
	Dir      string
	Indexer  string
	Version  string
	// Output is the .scip file, relative to the repository root.
	Output string

	// Ran is true when the indexer ran now; Reused when its earlier output was
	// still valid. Neither means it did not produce an index: see Skipped.
	Ran    bool
	Reused bool
	// Skipped is why nothing was imported from this project: a plan blocked it,
	// the indexer is not installed, or it failed. Diagnostic is the indexer's own
	// first lines when it failed.
	Skipped    string
	Diagnostic []string

	// RunTime is how long the indexer ran; zero when it was reused.
	RunTime time.Duration
	// ImportTime is how long reading the index into the graph took.
	ImportTime time.Duration

	Import scipimport.ImportResult
	Err    error
}

// CompilerIndexOutcome is what RunCompilerIndexers did, for resolve to print.
type CompilerIndexOutcome struct {
	Steps []CompilerIndexStep
	Proof scipimport.ProofStats
	// Withdrawn is the heuristic call edges the compilers proved false.
	Withdrawn int

	// Rebound is the references moved from a withdrawn edge's target to the
	// one definition the compiler proved on that line.
	Rebound int
	// Folded is how many nested Python projects were not run on their own: the
	// index of the project above them already reads every file beneath it.
	Folded int
	// Note explains an empty outcome: fast mode, no project found.
	Note string
	Err  error
}

// Imported is how many steps landed an index.
func (o CompilerIndexOutcome) Imported() int {
	n := 0
	for _, s := range o.Steps {
		if s.Skipped == "" && s.Err == nil && (s.Ran || s.Reused) {
			n++
		}
	}
	return n
}

// RunCompilerIndexers runs the compiler-backed indexers that are already
// installed, imports every index they leave under CompilerIndexDir, and records
// which files they read.
//
// The trust rules are the planner's, unchanged. The plan comes from the user's
// configuration; a step the plan marks blocked does not run (Java, unless its
// build is allowed); nothing is installed or downloaded here, so a step whose
// indexer is absent is reported and skipped; Go is left to the type checker
// built into this binary. A step that fails or times out is reported with the
// indexer's own first lines and does not stop the others, and none of it stops
// `resolve`.
func RunCompilerIndexers(ctx context.Context, reg *registry.Registry, runID int64, opts CompilerIndexOptions) CompilerIndexOutcome {
	var out CompilerIndexOutcome
	cfg := opts.Config
	if cfg.Accuracy == config.AccuracyFast {
		out.Note = "accuracy fast: no compiler indexer runs"
		return out
	}
	iopts := indexer.Options{Runner: opts.Runner, Logger: opts.Logger, AllowRepoBuild: opts.AllowRepoBuild}
	if opts.Logger != nil {
		iopts.Audit = indexer.LogAudit{Logger: opts.Logger}
	}
	plan, err := indexer.NewPlanWith(ctx, opts.Root, cfg, iopts)
	if err != nil {
		out.Err = fmt.Errorf("plan indexers: %w", err)
		return out
	}
	if len(plan.Steps) == 0 {
		out.Note = plan.Note
		return out
	}
	files, err := reg.Files(ctx, runID)
	if err != nil {
		out.Err = fmt.Errorf("read indexed files: %w", err)
		return out
	}

	dir := filepath.Join(opts.Root, filepath.FromSlash(CompilerIndexDir))
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultCompilerIndexTimeout
		if cfg.IndexerTimeout > 0 && cfg.IndexerTimeout < timeout {
			timeout = cfg.IndexerTimeout
		}
	}

	// One compiler pass at a time per repository. Two at once (a hook's
	// background pass and a manual resolve) shared one staging ledger and one
	// set of .scip outputs: the second removed the first one's staged files
	// mid-run and overwrote its indexes. The second pass now leaves the work
	// to the first and keeps the heuristic answers it already has.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		out.Err = fmt.Errorf("create %s: %w", dir, err)
		return out
	}
	passLock, err := acqfs.TryLock(filepath.Join(dir, "pass.lock"))
	if err != nil {
		if errors.Is(err, acqfs.ErrLocked) {
			out.Note = "another compiler pass is running in this repository; its exact answers land when it finishes (run resolve again afterwards to see them here)"
			return out
		}
		out.Err = fmt.Errorf("lock the compiler pass: %w", err)
		return out
	}
	defer func() { _ = passLock.Unlock() }()

	// Files an interrupted run left in the tree are removed first, whether or
	// not anything runs now: only ones still exactly as RepoCortex wrote them.
	if removed, leftover := recoverStaged(dir, opts.Root); opts.Logger != nil {
		if removed > 0 {
			opts.Logger.Info("removed files an interrupted compiler pass left behind", "count", removed)
		}
		for _, f := range leftover {
			opts.Logger.Warn("an interrupted compiler pass may have left this inferred tsconfig.json; remove it if you did not write it", "path", f)
		}
	}

	var toRun []pending
	for _, s := range plan.Steps {
		if opts.Language != "" && s.Project.Language != opts.Language {
			continue
		}
		if s.Project.Language == indexer.LangPython && !s.Blocked && nestedInPlanned(plan.Steps, s) {
			// scip-python indexes every Python file beneath the directory it
			// is run in, so a project inside another one is already read.
			// Measured on pallets/click: the root run covers its ten example
			// projects, and running each of them again took 9 s apiece and
			// added no document. TypeScript is not folded: a nested tsconfig
			// is a separate program the outer one may exclude.
			out.Folded++
			continue
		}
		cs := CompilerIndexStep{
			Language: string(s.Project.Language), Dir: s.Project.Dir,
			Indexer: string(s.Indexer),
		}
		switch {
		case s.Project.Language == indexer.LangGo:
			// The type checker built into this binary reads Go, and writes the
			// edges once: a second producer for the same call sites could only
			// disagree with it.
			continue
		case s.Blocked:
			cs.Skipped = s.Reason
		case s.NeedsProvision || (s.Installed.Path == "" && s.Installed.Image == ""):
			cs.Skipped = fmt.Sprintf("%s is not installed (resolve installs nothing; see 'repocortex indexers install')", s.Pin.Binary)
		default:
			cs.Version = s.Installed.Version
			if cs.Version == "" {
				cs.Version = s.Pin.Version
			}
			cs.Output = filepath.ToSlash(filepath.Join(CompilerIndexDir, slugOf(s.Project)+".scip"))
			key := cacheKey(opts.Root, s, cs.Version, plan.Runner, files)
			if cachedIndexValid(dir, s.Project, key) {
				cs.Reused = true
			} else {
				toRun = append(toRun, pending{step: s, key: key, idx: len(out.Steps)})
			}
		}
		out.Steps = append(out.Steps, cs)
	}

	if len(toRun) > 0 {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			out.Err = fmt.Errorf("create %s: %w", dir, err)
			return out
		}
		// The directory is the repository's own: nothing in it is ever
		// pruned by the indexer package, so whatever an earlier run left for
		// these projects is removed here first. A failure then leaves no index
		// behind to be imported as if it were current.
		// scip-typescript, given --infer-tsconfig, writes the tsconfig.json it
		// inferred into the project and leaves it there. A file the user did
		// not write does not stay in their repository: note which projects
		// have none now, and remove what appears.
		tsInferred := inferredTsconfigs(opts.Root, toRunProjects(toRun))
		written := writePythonSourceRoots(opts.Root, toRunProjects(toRun))
		written = append(written, stageVueMirrors(opts.Root, toRunProjects(toRun))...)
		// Recorded before the indexers run, so a run killed in the middle
		// leaves a ledger the next run can clean up from (see staging.go).
		if err := writeStagingLedger(dir, written, tsInferred); err != nil && opts.Logger != nil {
			opts.Logger.Warn("could not record staged files", "dir", dir, "error", err)
		}
		inferred := append(tsInferred, written...)
		defer func() {
			for _, f := range inferred {
				_ = os.Remove(f)
			}
			clearStagingLedger(dir)
		}()
		sub := plan
		sub.Accuracy = config.AccuracyAuto
		sub.Timeout = timeout
		sub.Steps = nil
		for _, p := range toRun {
			removeCachedIndex(dir, p.step.Project)
			sub.Steps = append(sub.Steps, p.step)
		}
		results, err := indexer.Execute(ctx, sub, indexer.Options{
			Runner: opts.Runner, Logger: opts.Logger, Audit: iopts.Audit, OutputDir: dir,
		})
		if err != nil {
			for _, p := range toRun {
				out.Steps[p.idx].Skipped = firstLine(err.Error())
			}
		}
		byKey := map[string]indexer.Execution{}
		for _, e := range results.Indexes {
			byKey[projectID(e.Project)] = e
		}
		skipped := map[string]indexer.Skip{}
		for _, s := range results.Skipped {
			skipped[projectID(s.Project)] = s
		}
		for _, p := range toRun {
			cs := &out.Steps[p.idx]
			id := projectID(p.step.Project)
			if e, ok := byKey[id]; ok {
				cs.Ran, cs.RunTime = true, e.Duration
				if werr := writeCacheKey(dir, p.step.Project, p.key); werr != nil && opts.Logger != nil {
					opts.Logger.Warn("could not record the indexer cache key", "dir", dir, "error", werr)
				}
				continue
			}
			if s, ok := skipped[id]; ok {
				cs.Skipped = firstLine(s.Reason)
				if errors.Is(s.Err, context.DeadlineExceeded) {
					cs.Skipped = fmt.Sprintf("timed out after %s", timeout)
				}
				cs.Diagnostic = diagnosticLines(s.Diagnostic)
				continue
			}
			if cs.Skipped == "" {
				cs.Skipped = "the indexer did not run"
			}
		}
	}

	proof := scipimport.NewProof()
	shared := &scipimport.SharedTables{}
	for _, own := range ownPackages(opts.Root, files) {
		proof.AddOwn(own[0], own[1])
	}
	for i := range out.Steps {
		cs := &out.Steps[i]
		if cs.Skipped != "" || (!cs.Ran && !cs.Reused) {
			continue
		}
		if err := ctx.Err(); err != nil {
			out.Err = err
			break
		}
		start := time.Now()
		impOpts := scip.Options{
			RepoRoot: opts.Root, MaxBytes: scip.DefaultMaxBytes,
		}
		if cs.Dir != "" && cs.Dir != "." {
			impOpts.PathPrefix = cs.Dir
		}
		imp := scipimport.NewImporter(reg, runID, impOpts).Collect(proof).ShareTables(shared)
		res, err := imp.Import(ctx, filepath.Join(opts.Root, filepath.FromSlash(cs.Output)))
		cs.ImportTime = time.Since(start)
		cs.Import = res
		if err != nil {
			cs.Err = err
		}
	}
	if out.Imported() > 0 && out.Err == nil {
		st, withdrawn, err := proof.Apply(ctx, reg, runID)
		out.Proof, out.Withdrawn = st, withdrawn
		if err != nil {
			out.Err = err
		} else if w, rb, cerr := reg.WithdrawContradictedCalls(ctx, runID); cerr != nil {
			out.Err = cerr
		} else {
			out.Withdrawn += w
			out.Rebound = rb
		}
		// An edge the compilers withdrew can leave a file's synthetic caller
		// with nothing to its name.
		if _, err := reg.PruneFileCallers(ctx, runID); err != nil && out.Err == nil {
			out.Err = err
		}
	}
	return out
}

// pending is a step that has to run, with the cache key to record for it and
// its place in the outcome.
type pending struct {
	step indexer.Step
	key  string
	idx  int
}

func toRunProjects(ps []pending) []indexer.Project {
	out := make([]indexer.Project, len(ps))
	for i, p := range ps {
		out[i] = p.step.Project
	}
	return out
}

// inferredTsconfigs lists the tsconfig.json files an indexer is about to
// create: those of TypeScript projects that have none.
func inferredTsconfigs(root string, projects []indexer.Project) []string {
	var out []string
	for _, p := range projects {
		if p.Language != indexer.LangTypeScript {
			continue
		}
		f := filepath.Join(root, filepath.FromSlash(p.Dir), "tsconfig.json")
		if _, err := os.Lstat(f); errors.Is(err, os.ErrNotExist) {
			out = append(out, f)
		}
	}
	return out
}

// pythonRootConfig is the whole of the configuration written for a Python
// project: a fixed literal naming the one directory a project is laid out in.
const pythonRootConfig = "{\"extraPaths\": [\"src\"]}\n"

// writePythonSourceRoots tells scip-python's static analysis where a src-layout
// project's own modules are, and returns the files it wrote so they can be
// removed after the run.
//
// Without it `import click` in click's tests resolved to a click 8.1.8 in the
// user's site-packages and not to the checkout, so the compiler spelled the
// checkout's symbols as another copy's and no call in the tests joined to a
// definition of the repository (pallets/click: 902 calls proven, and the test
// call TestType().to_info_dict() was never bound to the checkout's method).
//
// This is analysis configuration. The repository's code is not run and its
// text is not read: the file is written only when the project has a directory
// named src (not a link) and no pyrightconfig.json of its own, and its content
// is the literal above. Putting src on PYTHONPATH instead was rejected: the
// indexer starts a Python interpreter, which would import a sitecustomize.py,
// a .pth file or a file named like a standard-library module from it.
func writePythonSourceRoots(root string, projects []indexer.Project) []string {
	var out []string
	for _, p := range projects {
		if p.Language != indexer.LangPython {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(p.Dir))
		if fi, err := os.Lstat(filepath.Join(dir, "src")); err != nil || !fi.IsDir() {
			continue
		}
		f := filepath.Join(dir, "pyrightconfig.json")
		w, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a fixed name inside the project being indexed
		if err != nil {
			continue // one already exists: the project's own governs
		}
		_, werr := w.WriteString(pythonRootConfig)
		if cerr := w.Close(); werr != nil || cerr != nil {
			_ = os.Remove(f)
			continue
		}
		out = append(out, f)
	}
	return out
}

// projectID names a project for matching an Execute result to its step.
func projectID(p indexer.Project) string { return string(p.Language) + "\x00" + p.Dir }

// slugOf is the output file stem of a project, the one the indexer package
// writes.
func slugOf(p indexer.Project) string { return indexer.OutputSlug(p) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// diagnosticLines keeps the indexer's own first few lines, which usually name
// the fix.
func diagnosticLines(d string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(d), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200] + "..."
			}
			out = append(out, l)
		}
		if len(out) == 3 {
			break
		}
	}
	return out
}

// languageFamily is the file languages an indexer reads.
func languageFamily(l indexer.Language) map[string]bool {
	switch l {
	case indexer.LangTypeScript:
		return map[string]bool{"typescript": true, "tsx": true, "javascript": true, "jsx": true}
	case indexer.LangPython:
		return map[string]bool{"python": true}
	case indexer.LangJava:
		return map[string]bool{"java": true, "kotlin": true, "scala": true}
	case indexer.LangGo:
		return map[string]bool{"go": true}
	case indexer.LangCPP:
		return map[string]bool{"c": true, "cpp": true}
	case indexer.LangRust:
		return map[string]bool{"rust": true}
	}
	return nil
}

// buildInputs are files whose content decides what an indexer reads without
// being source: how the project is configured and which dependencies it pins.
var buildInputs = map[string]bool{
	"package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true, "yarn.lock": true,
	"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "uv.lock": true, "poetry.lock": true,
	"Pipfile.lock": true, "pom.xml": true,
	// scip-clang reads the compilation database, so a changed flag or include
	// path changes what it proves; cargo reads the manifest and lock file.
	"compile_commands.json": true, "Cargo.toml": true, "Cargo.lock": true, "build.gradle": true, "build.gradle.kts": true,
	// Workspace and multi-project build files, usually at the root above the
	// project: they decide which packages and versions the build resolves.
	"pnpm-workspace.yaml": true, "settings.gradle": true, "settings.gradle.kts": true, "gradle.properties": true,
}

// maxKeyedInput bounds how much of a build input is read for the key.
const maxKeyedInput = 8 << 20

// cacheKey is what an indexer's output depends on, as one digest.
//
// It is made of: the indexer, its version (what the tool reports about itself,
// so an upgrade runs it again) and the runner; the project and its language;
// the content hash of every indexed source file of the indexer's languages
// under the project (from the index run, which already hashed them); the
// content of the project's configuration and lock files, read from disk; and a
// stamp of the dependency directory, because scip-typescript and scip-python
// resolve imports through it and report less, without failing, when it
// changes. The stamp is the directory's modification time, which an install or
// removal of a top-level package moves; a package changed in place does not,
// which is the key's one known blind spot.
func cacheKey(root string, s indexer.Step, version, runner string, files []model.File) string {
	h := sha256.New()
	put := func(parts ...string) {
		for _, p := range parts {
			_, _ = io.WriteString(h, p)
			_, _ = h.Write([]byte{0})
		}
	}
	put("repocortex-compiler-index", cacheKeyVersion, string(s.Indexer), version, runner,
		string(s.Project.Language), s.Project.Dir)
	if s.Installed.Image != "" {
		put(s.Installed.Image)
	}
	family := languageFamily(s.Project.Language)
	prefix := ""
	if s.Project.Dir != "." && s.Project.Dir != "" {
		prefix = s.Project.Dir + "/"
	}
	var lines []string
	for _, f := range files {
		if prefix != "" && !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		if family[f.Language] {
			lines = append(lines, f.Path+"\x00"+f.ContentHash)
		}
	}
	sort.Strings(lines)
	put(lines...)

	projectDir := filepath.Join(root, filepath.FromSlash(s.Project.Dir))
	// The project directory and every directory above it up to the
	// repository root: in a workspace the lock file, the hoisted
	// node_modules and the multi-project build settings live at the root.
	var inputs []string
	for _, dir := range dirsUpToRoot(root, projectDir) {
		rel, _ := filepath.Rel(root, dir)
		rel = filepath.ToSlash(rel)
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				name := e.Name()
				// tsconfig*.json only: tsconfig.tsbuildinfo is tsc -b output, not input.
				isInput := buildInputs[name] || (strings.HasPrefix(name, "tsconfig") && strings.HasSuffix(name, ".json")) || strings.HasPrefix(name, "requirements")
				if e.IsDir() || !isInput {
					continue
				}
				inputs = append(inputs, rel+"/"+name+"\x00"+hashFile(filepath.Join(dir, name)))
			}
		}
		// Gradle's version catalog sits in gradle/ beside settings.gradle.
		if catalog := filepath.Join(dir, "gradle", "libs.versions.toml"); fileIsRegular(catalog) {
			inputs = append(inputs, rel+"/gradle/libs.versions.toml\x00"+hashFile(catalog))
		}
		for _, dep := range []string{"node_modules", ".venv", "venv"} {
			if fi, err := os.Stat(filepath.Join(dir, dep)); err == nil && fi.IsDir() {
				inputs = append(inputs, rel+"/"+dep+"\x00"+fmt.Sprint(fi.ModTime().UnixNano()))
			}
		}
	}
	// A tsconfig.json can extend one outside the project (../../tsconfig.base.json),
	// which the walk above reaches only when it sits in an ancestor directory.
	for _, ext := range tsconfigExtendsChain(filepath.Join(projectDir, "tsconfig.json")) {
		rel, _ := filepath.Rel(root, ext)
		inputs = append(inputs, "extends:"+filepath.ToSlash(rel)+"\x00"+hashFile(ext))
	}
	sort.Strings(inputs)
	put(inputs...)
	return hex.EncodeToString(h.Sum(nil))
}

// dirsUpToRoot lists dir and its ancestors up to and including root.
func dirsUpToRoot(root, dir string) []string {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	var out []string
	for i := 0; i < 64; i++ { // bounded: a path cannot be deeper than this in practice
		out = append(out, dir)
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent, root) {
			break
		}
		dir = parent
	}
	return out
}

func fileIsRegular(p string) bool {
	fi, err := os.Stat(p) //nolint:gosec // G703: a build-input path this package derives inside the repository being indexed
	return err == nil && fi.Mode().IsRegular()
}

// tsconfigExtends reads the "extends" of a tsconfig file. tsconfig allows
// comments, so this is a pattern match rather than a JSON parse; only a
// relative path is followed (a package name resolves inside node_modules,
// which the key already stamps).
var tsconfigExtends = regexp.MustCompile(`"extends"\s*:\s*"(\.{1,2}/[^"]+)"`)

// tsconfigExtendsChain follows relative "extends" from a tsconfig, at most a
// few levels, and returns the files it reaches.
func tsconfigExtendsChain(start string) []string {
	var out []string
	cur := start
	for i := 0; i < 5; i++ {
		raw, err := os.ReadFile(cur) //nolint:gosec // G304: a tsconfig inside the repository, read for its extends only
		if err != nil || len(raw) > maxKeyedInput {
			break
		}
		m := tsconfigExtends.FindSubmatch(raw)
		if m == nil {
			break
		}
		next := filepath.Join(filepath.Dir(cur), filepath.FromSlash(string(m[1])))
		if !strings.HasSuffix(next, ".json") && !fileIsRegular(next) {
			next += ".json"
		}
		if !fileIsRegular(next) {
			break
		}
		out = append(out, next)
		cur = next
	}
	return out
}

// hashFile is the content digest of a small file, or a marker for one that
// cannot be read.
func hashFile(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: a fixed build-input name inside the repository being indexed, read for its digest only
	if err != nil {
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxKeyedInput)); err != nil {
		return "unreadable"
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cacheSidecar is the record kept beside an index.
type cacheSidecar struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

func sidecarPath(dir string, p indexer.Project) string {
	return filepath.Join(dir, slugOf(p)+".key")
}

func indexPath(dir string, p indexer.Project) string {
	return filepath.Join(dir, slugOf(p)+".scip")
}

// cachedIndexValid reports whether the index on disk for p was written under
// key and is still the file that was written.
func cachedIndexValid(dir string, p indexer.Project, key string) bool {
	raw, err := os.ReadFile(sidecarPath(dir, p)) //nolint:gosec // G304: a name this package derives from the project, inside the repository's .repocortex directory
	if err != nil {
		return false
	}
	var sc cacheSidecar
	if json.Unmarshal(raw, &sc) != nil || sc.Key != key {
		return false
	}
	fi, err := os.Stat(indexPath(dir, p))
	return err == nil && fi.Size() > 0 && fi.Size() == sc.Size
}

func writeCacheKey(dir string, p indexer.Project, key string) error {
	fi, err := os.Stat(indexPath(dir, p))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cacheSidecar{Key: key, Size: fi.Size()})
	if err != nil {
		return err
	}
	return os.WriteFile(sidecarPath(dir, p), raw, 0o600)
}

// removeCachedIndex deletes an earlier index and its key before a run.
func removeCachedIndex(dir string, p indexer.Project) {
	_ = os.Remove(sidecarPath(dir, p))
	_ = os.Remove(indexPath(dir, p))
}

// nestedInPlanned reports whether another runnable Python step is a proper
// ancestor of s's directory.
func nestedInPlanned(steps []indexer.Step, s indexer.Step) bool {
	for _, o := range steps {
		if o.Project.Language != indexer.LangPython || o.Blocked || o.Project.Dir == s.Project.Dir {
			continue
		}
		if o.Project.Dir == "." || strings.HasPrefix(s.Project.Dir, o.Project.Dir+"/") {
			return true
		}
	}
	return false
}

// ownPackages are the packages the repository itself provides, as (manager,
// name) pairs: the Python packages and modules it contains, and the npm
// packages its package.json files name. See scipimport.Proof.AddOwn for why an
// indexer's view of them has to be recognised.
func ownPackages(root string, files []model.File) [][2]string {
	seen := map[[2]string]bool{}
	var out [][2]string
	add := func(manager, name string) {
		k := [2]string{manager, name}
		if name != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, f := range files {
		switch {
		case f.Language == "python":
			parts := strings.Split(strings.TrimSuffix(f.Path, ".py"), "/")
			if len(parts) > 0 && parts[len(parts)-1] == "__init__" {
				parts = parts[:len(parts)-1]
			}
			for _, part := range parts {
				add("python", part)
			}
		case filepath.Base(f.Path) == "Cargo.toml":
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path))) //nolint:gosec // G304: Cargo.toml inside the repository being indexed, read for its package name only
			if err != nil || len(raw) > maxKeyedInput {
				continue
			}
			add("cargo", cargoPackageName(string(raw)))
		case filepath.Base(f.Path) == "package.json":
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path))) //nolint:gosec // G304: package.json inside the repository being indexed, read for its "name" only
			if err != nil || len(raw) > maxKeyedInput {
				continue
			}
			var pj struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &pj) == nil {
				add("npm", pj.Name)
			}
		}
	}
	return out
}

// cargoPackageName is the `name` of a Cargo.toml's [package] table, or "".
// Read for provenance only (see Proof.AddOwn): the SCIP symbols of a Cargo
// package carry it as their package name.
func cargoPackageName(manifest string) string {
	inPackage := false
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inPackage = line == "[package]"
			continue
		}
		if !inPackage {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "name" {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			return val[1 : len(val)-1]
		}
	}
	return ""
}
