package pipeline

import (
	"context"
	acqfs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeIndexers is an indexer.Runner whose tools exist only on paper: it records
// every command, answers version probes, and writes a real SCIP index when
// scip-typescript is asked to index.
type fakeIndexers struct {
	mu      sync.Mutex
	present map[string]bool
	ran     []string
	// tsFails makes scip-typescript exit non-zero with this message.
	tsFails string
	// leaveTsconfig makes scip-typescript write the tsconfig.json it inferred.
	leaveTsconfig bool
}

func bin(name string) string { return filepath.Join(os.TempDir(), "repocortex-fake-bin", name) }

func (f *fakeIndexers) LookPath(file string) (string, error) {
	if !f.present[file] {
		return "", os.ErrNotExist
	}
	return bin(file), nil
}

func (f *fakeIndexers) Run(_ context.Context, c indexer.Command) (indexer.Result, error) {
	f.mu.Lock()
	f.ran = append(f.ran, filepath.Base(c.Path)+" "+strings.Join(c.Args, " "))
	f.mu.Unlock()
	res := indexer.Result{Argv: c.Argv(), Dir: c.Dir, Stdout: []byte(filepath.Base(c.Path) + " 1.2.3\n")}
	if filepath.Base(c.Path) != "scip-typescript" || len(c.Args) == 0 || c.Args[0] != "index" {
		return res, nil
	}
	if f.tsFails != "" {
		res.ExitCode, res.Stdout, res.Stderr = 1, nil, []byte(f.tsFails)
		return res, nil
	}
	out := ""
	for i, a := range c.Args {
		if a == "--output" && i+1 < len(c.Args) {
			out = c.Args[i+1]
		}
	}
	if f.leaveTsconfig {
		if err := os.WriteFile(filepath.Join(c.Dir, "tsconfig.json"), []byte("{}"), 0o600); err != nil {
			return res, err
		}
	}
	idx := &scippb.Index{
		Metadata: &scippb.Metadata{
			ProjectRoot: "file://" + c.Dir,
			ToolInfo:    &scippb.ToolInfo{Name: "scip-typescript", Version: "1.2.3"},
		},
		Documents: []*scippb.Document{{
			RelativePath: "a.ts", Language: "typescript",
			PositionEncoding: scippb.PositionEncoding_UTF16CodeUnitOffsetFromLineStart,
			Symbols: []*scippb.SymbolInformation{
				{Symbol: "scip-typescript npm proj 1.0.0 `a.ts`/hello().", Kind: scippb.SymbolInformation_Function, DisplayName: "hello"},
			},
			Occurrences: []*scippb.Occurrence{func() *scippb.Occurrence {
				o := &scippb.Occurrence{Symbol: "scip-typescript npm proj 1.0.0 `a.ts`/hello().", SymbolRoles: int32(scippb.SymbolRole_Definition)}
				o.SetSourceRange(scippb.Range{Start: scippb.Position{Line: 0, Character: 9}, End: scippb.Position{Line: 0, Character: 14}})
				o.SetEnclosingSourceRange(scippb.Range{Start: scippb.Position{Line: 0, Character: 0}, End: scippb.Position{Line: 2, Character: 1}})
				return o
			}()},
		}},
	}
	raw, err := proto.Marshal(idx)
	if err != nil {
		return res, err
	}
	return res, os.WriteFile(out, raw, 0o600)
}

func (f *fakeIndexers) indexRuns(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.ran {
		if strings.HasPrefix(r, tool+" index") || (tool == "scip-go" && strings.HasPrefix(r, "scip-go --output")) {
			n++
		}
	}
	return n
}

// repoWith writes a repository and a committed index of its one TypeScript
// file, and returns its root.
func repoWith(t *testing.T, files map[string]string) (string, *registry.Registry, int64, *store.DB) {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "cafe")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := w.AddFile(ctx, &model.File{Path: "a.ts", Language: "typescript", ContentHash: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.AddSymbol(ctx, &model.Symbol{
		FileID: fid, Kind: model.KindFunction, Name: "hello", FQN: "a.ts:hello",
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 3}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return root, reg, run.ID, db
}

func autoConfig() config.Config {
	return config.Config{Accuracy: config.AccuracyAuto, ProvisionPolicy: config.ProvisionNever, IndexerRunner: indexer.RunnerNative}
}

func TestCompilerIndexersRunReuseAndRerun(t *testing.T) {
	ctx := context.Background()
	root, reg, runID, db := repoWith(t, map[string]string{
		"package.json": `{"name":"proj"}`, "a.ts": "function hello() {\n}\n",
		// A Go module: the type check built into the binary reads Go, so no
		// scip-go may run even though it is installed.
		"go.mod": "module example.com/x\n",
	})
	fake := &fakeIndexers{present: map[string]bool{"node": true, "scip-typescript": true, "scip-go": true}, leaveTsconfig: true}
	opts := CompilerIndexOptions{Root: root, Config: autoConfig(), Runner: fake}

	first := RunCompilerIndexers(ctx, reg, runID, opts)
	if first.Err != nil {
		t.Fatalf("first run: %v", first.Err)
	}
	if len(first.Steps) != 1 || !first.Steps[0].Ran || first.Steps[0].Err != nil {
		t.Fatalf("first run steps = %+v, want one that ran and imported", first.Steps)
	}
	if first.Proof.Files != 1 || first.Steps[0].Import.SymbolsMatched != 1 {
		t.Errorf("proof %+v, import %+v: want the one file covered and its symbol matched", first.Proof, first.Steps[0].Import)
	}
	if fake.indexRuns("scip-go") != 0 {
		t.Error("scip-go ran: Go belongs to the built-in type check, which would otherwise be written over")
	}
	if _, err := os.Stat(filepath.Join(root, "tsconfig.json")); err == nil {
		t.Error("the tsconfig.json scip-typescript inferred was left in the repository")
	}
	if _, err := os.Stat(filepath.Join(root, ".repocortex", "scip", "typescript.scip")); err != nil {
		t.Errorf("index not kept under .repocortex/scip: %v", err)
	}

	second := RunCompilerIndexers(ctx, reg, runID, opts)
	if !second.Steps[0].Reused || second.Steps[0].Ran {
		t.Fatalf("second run %+v: want the earlier index reused", second.Steps[0])
	}
	if got := fake.indexRuns("scip-typescript"); got != 1 {
		t.Errorf("scip-typescript indexed %d times after two resolves with nothing changed, want 1", got)
	}
	if second.Proof.Files != 1 {
		t.Errorf("a reused index must still be imported: proof %+v", second.Proof)
	}

	// A source file changes: its content hash in the index run moves.
	if _, err := db.ExecContext(ctx, `UPDATE files SET content_hash = 'v2' WHERE path = 'a.ts'`); err != nil {
		t.Fatal(err)
	}
	third := RunCompilerIndexers(ctx, reg, runID, opts)
	if !third.Steps[0].Ran {
		t.Fatalf("third run %+v: want the indexer to run after a source file changed", third.Steps[0])
	}
	if got := fake.indexRuns("scip-typescript"); got != 2 {
		t.Errorf("scip-typescript indexed %d times, want 2", got)
	}
}

func TestCompilerIndexerFailureIsReportedNotFatal(t *testing.T) {
	root, reg, runID, _ := repoWith(t, map[string]string{"package.json": `{}`, "a.ts": "x\n"})
	fake := &fakeIndexers{
		present: map[string]bool{"node": true, "scip-typescript": true},
		tsFails: "error TS5083: Cannot read file\nsecond line\nthird line\nfourth line",
	}
	out := RunCompilerIndexers(context.Background(), reg, runID, CompilerIndexOptions{Root: root, Config: autoConfig(), Runner: fake})
	if out.Err != nil {
		t.Fatalf("a failed indexer failed the pass: %v", out.Err)
	}
	if len(out.Steps) != 1 || out.Steps[0].Skipped == "" {
		t.Fatalf("steps = %+v, want one skipped", out.Steps)
	}
	if len(out.Steps[0].Diagnostic) == 0 || !strings.Contains(out.Steps[0].Diagnostic[0], "TS5083") {
		t.Errorf("diagnostic %q does not carry the indexer's own first line", out.Steps[0].Diagnostic)
	}
	if len(out.Steps[0].Diagnostic) > 3 {
		t.Errorf("diagnostic has %d lines, want at most 3", len(out.Steps[0].Diagnostic))
	}
	if _, err := os.Stat(filepath.Join(root, ".repocortex", "scip", "typescript.key")); err == nil {
		t.Error("a cache key was recorded for an index that was never written")
	}
	if out.Imported() != 0 {
		t.Errorf("imported %d, want 0", out.Imported())
	}
}

func TestCompilerIndexersRunNothingThePlanBlocksOrIsAbsent(t *testing.T) {
	root, reg, runID, _ := repoWith(t, map[string]string{
		"package.json": `{}`, "a.ts": "x\n",
		// Java runs the repository's own build: blocked unless allowed.
		"pom.xml": "<project/>",
	})
	// scip-typescript is NOT installed; scip-java, mvn and java are.
	fake := &fakeIndexers{present: map[string]bool{"node": true, "scip-java": true, "java": true, "mvn": true}}
	out := RunCompilerIndexers(context.Background(), reg, runID, CompilerIndexOptions{Root: root, Config: autoConfig(), Runner: fake})
	if len(out.Steps) != 2 {
		t.Fatalf("steps = %+v, want a typescript and a java step, both skipped", out.Steps)
	}
	for _, s := range out.Steps {
		if s.Skipped == "" {
			t.Errorf("%s step was not skipped: %+v", s.Language, s)
		}
	}
	for _, r := range fake.ran {
		if strings.HasPrefix(r, "scip-java") && strings.Contains(r, "index") {
			t.Errorf("scip-java indexed a repository whose build is not allowed: %s", r)
		}
		if strings.Contains(r, "install") {
			t.Errorf("an install reached a subprocess: %s", r)
		}
	}
}

func TestFastModeRunsNothing(t *testing.T) {
	root, reg, runID, _ := repoWith(t, map[string]string{"package.json": `{}`, "a.ts": "x\n"})
	fake := &fakeIndexers{present: map[string]bool{"node": true, "scip-typescript": true}}
	cfg := autoConfig()
	cfg.Accuracy = config.AccuracyFast
	out := RunCompilerIndexers(context.Background(), reg, runID, CompilerIndexOptions{Root: root, Config: cfg, Runner: fake})
	if len(fake.ran) != 0 || len(out.Steps) != 0 {
		t.Errorf("fast mode ran %v, steps %+v", fake.ran, out.Steps)
	}
}

func TestNestedPythonProjectsFoldIntoTheOneAbove(t *testing.T) {
	steps := []indexer.Step{
		{Project: indexer.Project{Language: indexer.LangPython, Dir: "."}},
		{Project: indexer.Project{Language: indexer.LangPython, Dir: "examples/a"}},
		{Project: indexer.Project{Language: indexer.LangPython, Dir: "svc"}},
		{Project: indexer.Project{Language: indexer.LangTypeScript, Dir: "web"}},
	}
	if !nestedInPlanned(steps, steps[1]) {
		t.Error("examples/a sits under the root project and should fold")
	}
	if nestedInPlanned(steps, steps[0]) {
		t.Error("the root project is not nested in anything")
	}
	noRoot := steps[1:]
	if nestedInPlanned(noRoot, noRoot[1]) {
		t.Error("svc is a sibling of examples/a, not nested in it")
	}
}

func TestCacheKeyMovesWithWhatTheIndexerReads(t *testing.T) {
	root, _, _, _ := repoWith(t, map[string]string{"package.json": `{"name":"a"}`})
	step := indexer.Step{
		Project: indexer.Project{Language: indexer.LangTypeScript, Dir: "."},
		Indexer: indexer.SCIPTypeScript,
	}
	files := []model.File{{Path: "a.ts", Language: "typescript", ContentHash: "h1"}, {Path: "README.md", Language: "markdown", ContentHash: "r1"}}
	base := cacheKey(root, step, "1.0", "native", files)

	if got := cacheKey(root, step, "1.0", "native", files); got != base {
		t.Error("the key is not deterministic")
	}
	if cacheKey(root, step, "1.1", "native", files) == base {
		t.Error("a new indexer version must re-run it")
	}
	changed := []model.File{{Path: "a.ts", Language: "typescript", ContentHash: "h2"}, files[1]}
	if cacheKey(root, step, "1.0", "native", changed) == base {
		t.Error("a changed source file must re-run it")
	}
	docs := []model.File{files[0], {Path: "README.md", Language: "markdown", ContentHash: "r2"}}
	if cacheKey(root, step, "1.0", "native", docs) != base {
		t.Error("a file of another language is not something the indexer reads")
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cacheKey(root, step, "1.0", "native", files) == base {
		t.Error("a changed package.json must re-run it")
	}
}

// In a workspace the inputs that decide what a package's indexer resolves sit
// above the package: the lock file and hoisted node_modules at the root, a
// tsconfig the package's own one extends, Gradle's settings and version
// catalog. Reading only the package directory let a dependency upgrade reuse
// an index built against the old dependencies.
func TestCacheKeyFollowsWorkspaceInputsAboveTheProject(t *testing.T) {
	root, _, _, _ := repoWith(t, map[string]string{
		"pnpm-lock.yaml":            "lockfileVersion: 9\n",
		"tsconfig.base.json":        `{"compilerOptions":{"strict":true}}`,
		"packages/ui/package.json":  `{"name":"ui"}`,
		"packages/ui/tsconfig.json": "{\n  // shared settings\n  \"extends\": \"../../tsconfig.base.json\"\n}\n",
		"settings.gradle":           "include 'app'\n",
		"gradle/libs.versions.toml": "[versions]\nkotlin = \"2.0.0\"\n",
		"app/build.gradle":          "plugins {}\n",
	})
	files := []model.File{{Path: "packages/ui/a.ts", Language: "typescript", ContentHash: "h1"}}
	ts := indexer.Step{Project: indexer.Project{Language: indexer.LangTypeScript, Dir: "packages/ui"}, Indexer: indexer.SCIPTypeScript}
	base := cacheKey(root, ts, "1.0", "native", files)

	for _, change := range []struct{ rel, content, why string }{
		{"pnpm-lock.yaml", "lockfileVersion: 9\n# upgraded\n", "the workspace lock file"},
		{"tsconfig.base.json", `{"compilerOptions":{"strict":false}}`, "a tsconfig the project extends"},
	} {
		before := cacheKey(root, ts, "1.0", "native", files)
		if err := os.WriteFile(filepath.Join(root, change.rel), []byte(change.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if cacheKey(root, ts, "1.0", "native", files) == before {
			t.Errorf("changing %s did not change the key", change.why)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if cacheKey(root, ts, "1.0", "native", files) == base {
		t.Error("a hoisted node_modules at the workspace root must be part of the key")
	}

	kt := indexer.Step{Project: indexer.Project{Language: indexer.LangJava, Dir: "app"}, Indexer: indexer.SCIPJava}
	jfiles := []model.File{{Path: "app/A.kt", Language: "kotlin", ContentHash: "k1"}}
	before := cacheKey(root, kt, "1.0", "native", jfiles)
	if err := os.WriteFile(filepath.Join(root, "gradle", "libs.versions.toml"), []byte("[versions]\nkotlin = \"2.1.0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cacheKey(root, kt, "1.0", "native", jfiles) == before {
		t.Error("the Gradle version catalog at the root must be part of the key")
	}
}

// A second compiler pass while one is running (a hook's background pass and a
// manual resolve) used to share the staging ledger and the .scip outputs; it
// now leaves the work to the pass that holds the lock and runs nothing.
func TestCompilerPassRunsOneAtATime(t *testing.T) {
	ctx := context.Background()
	root, reg, runID, _ := repoWith(t, map[string]string{
		"package.json": `{"name":"proj"}`, "a.ts": "function hello() {\n}\n",
	})
	fake := &fakeIndexers{present: map[string]bool{"node": true, "scip-typescript": true}, leaveTsconfig: true}
	opts := CompilerIndexOptions{Root: root, Config: autoConfig(), Runner: fake}

	dir := filepath.Join(root, filepath.FromSlash(CompilerIndexDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acqfs.TryLock(filepath.Join(dir, "pass.lock"))
	if err != nil {
		t.Fatal(err)
	}
	busy := RunCompilerIndexers(ctx, reg, runID, opts)
	if busy.Err != nil || !strings.Contains(busy.Note, "another compiler pass") || len(busy.Steps) != 0 {
		t.Fatalf("pass while locked = note %q, err %v, %d steps; want a note and nothing run", busy.Note, busy.Err, len(busy.Steps))
	}
	if fake.indexRuns("scip-typescript") != 0 {
		t.Fatal("an indexer ran while another pass held the lock")
	}
	_ = held.Unlock()

	free := RunCompilerIndexers(ctx, reg, runID, opts)
	if free.Err != nil || len(free.Steps) != 1 || !free.Steps[0].Ran {
		t.Fatalf("pass after release = %+v, want one indexer run", free.Steps)
	}
}
