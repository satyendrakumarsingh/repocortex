package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// testConfig builds a resolved config the way config.Load would, without
// touching the developer's real ~/.config/cortex/config.yml.
func testConfig(root string, mode config.AccuracyMode) config.Config {
	cfg := config.Defaults()
	cfg.RepoRoot = root
	cfg.Accuracy = mode
	return cfg
}

func polyglotRepo(t *testing.T) string {
	t.Helper()
	return writeRepo(t, map[string]string{
		"go.mod":                   "module x",
		"web/package.json":         "{}",
		"web/tsconfig.json":        "{}",
		"services/api/pom.xml":     "<project/>",
		"tools/gen/pyproject.toml": "",
	})
}

// TestFastModeRunsNothing. `fast` promises tree-sitter only and seconds to
// index, so it must not walk the repository or spawn a single process.
func TestFastModeRunsNothing(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go", "node", "java", "mvn")
	audit := &MemoryAudit{}

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyFast),
		Options{Runner: r, Audit: audit})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if len(plan.Steps) != 0 || len(plan.Projects) != 0 {
		t.Fatalf("fast mode planned work: %+v", plan.Steps)
	}
	if plan.Note == "" {
		t.Error("a plan with no steps must say why")
	}
	if len(r.calls()) != 0 {
		t.Fatalf("fast mode probed the toolchain: %v", r.argvs())
	}

	res, err := Execute(context.Background(), plan, Options{Runner: r, Audit: audit})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Indexes) != 0 || res.OutputDir != "" {
		t.Fatalf("fast mode produced %+v", res)
	}
	if len(audit.Records()) != 0 {
		t.Fatalf("fast mode ran %d subprocesses", len(audit.Records()))
	}
}

// TestAutoModeSkipsWhatItCannotRun. The Go toolchain is present, the JVM is
// not: Go gets indexed, Java is skipped, and the skip says why. Silently
// omitting Java would leave a user wondering why their Java edges are guesses.
func TestAutoModeSkipsWhatItCannotRun(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go", "node")
	r.install("scip-go")
	r.install("scip-typescript")
	r.install("scip-python")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if err := plan.Err(); err != nil {
		t.Fatalf("auto mode must never produce a plan error: %v", err)
	}

	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })

	got := map[Language]bool{}
	for _, ix := range res.Indexes {
		got[ix.Project.Language] = true
		if _, err := os.Stat(ix.IndexPath); err != nil {
			t.Errorf("promised index %s does not exist: %v", ix.IndexPath, err)
		}
		if !strings.HasPrefix(ix.IndexPath, res.OutputDir) {
			t.Errorf("index written outside the output directory: %s", ix.IndexPath)
		}
	}
	for _, want := range []Language{LangGo, LangTypeScript, LangPython} {
		if !got[want] {
			t.Errorf("%s was not indexed", want)
		}
	}
	if got[LangJava] {
		t.Error("java was indexed with no JVM present")
	}

	if len(res.Skipped) != 1 {
		t.Fatalf("skips = %+v, want exactly one (java)", res.Skipped)
	}
	skip := res.Skipped[0]
	if skip.Project.Language != LangJava {
		t.Fatalf("skipped %s, want java", skip.Project.Language)
	}
	if !strings.Contains(skip.Reason, "java") || !strings.Contains(skip.Reason, "mvn") {
		t.Errorf("skip reason %q does not name the missing tools", skip.Reason)
	}
}

// TestExactModeFailsLoudlyOnAMissingToolchain is the test this mode exists for.
// `exact` means a compiler proved every edge; degrading to heuristics for Java
// while still publishing the run as exact would be a false claim about the
// graph, so it fails — and the failure has to be actionable, which means naming
// the language and the tool to install.
func TestExactModeFailsLoudlyOnAMissingToolchain(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go", "node") // no java, no mvn
	r.install("scip-go")
	r.install("scip-typescript")
	r.install("scip-python")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyExact),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}

	planErr := plan.Err()
	if planErr == nil {
		t.Fatal("exact mode accepted a plan it cannot satisfy")
	}
	for _, want := range []string{"java", "mvn", "exact"} {
		if !strings.Contains(planErr.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, planErr)
		}
	}
	// It must also tell the user the way out.
	if !strings.Contains(planErr.Error(), "auto") {
		t.Errorf("error offers no remedy: %v", planErr)
	}
	if k := errs.KindOf(planErr); k != errs.KindUnsupported {
		t.Errorf("KindOf = %q, want %q", k, errs.KindUnsupported)
	}

	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err == nil {
		t.Fatal("Execute succeeded in exact mode with an unindexable language")
	}
	if !strings.Contains(err.Error(), "java") {
		t.Errorf("Execute error does not name the language: %v", err)
	}
	// Failing exact must be free of side effects: nothing indexed, no temp
	// directory to clean up, so re-running after installing a JDK starts clean.
	if len(res.Indexes) != 0 || res.OutputDir != "" {
		t.Errorf("exact failed after doing work: %+v", res)
	}
	for _, c := range r.calls() {
		if strings.Contains(strings.Join(c.Args, " "), "--output") {
			t.Errorf("an indexer ran before exact mode gave up: %v", c.Argv())
		}
	}
}

// TestExactModeSucceedsWhenEverythingIsPresent is the other half: exact is not
// merely a way to fail.
func TestExactModeSucceedsWhenEverythingIsPresent(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"go.mod": "module x", "web/tsconfig.json": "{}"})
	r := newFakeRunner("go", "node")
	r.install("scip-go")
	r.install("scip-typescript")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyExact),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if err := plan.Err(); err != nil {
		t.Fatalf("plan.Err = %v, want nil", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if len(res.Indexes) != 2 {
		t.Fatalf("indexed %d projects, want 2", len(res.Indexes))
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("exact mode skipped something: %+v", res.Skipped)
	}
}

// TestPlanHasNoSideEffects. Plan backs `cortex indexers list`, which a user
// runs to find out what WOULD happen. It must not install, prompt, index or
// create anything.
func TestPlanHasNoSideEffects(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	before := snapshot(t, root)

	r := newFakeRunner("go", "node", "java", "mvn", "npm", "docker")
	audit := &MemoryAudit{}
	cfg := testConfig(root, config.AccuracyExact)
	cfg.ProvisionPolicy = config.ProvisionAuto // the most permissive setting

	plan, err := NewPlanWith(context.Background(), root, cfg, Options{
		Runner: r, Audit: audit, AssumeYes: true, AllowRepoBuild: true,
		Prompt: func(context.Context, ProvisionRequest) (bool, error) {
			t.Error("Plan asked the user a question")
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("expected steps to inspect")
	}

	for _, rec := range audit.Records() {
		if rec.Action != ActionProbe {
			t.Errorf("Plan performed a %q action: %v", rec.Action, rec.Result.Argv)
		}
	}
	for _, c := range r.calls() {
		joined := strings.Join(c.Args, " ")
		for _, forbidden := range []string{"install", "pull", "pack", "--output", "index"} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("Plan ran something with side effects: %v", c.Argv())
			}
		}
	}
	if diff := snapshot(t, root); diff != before {
		t.Errorf("Plan modified the repository:\nbefore %s\nafter  %s", before, diff)
	}
}

// TestExecuteRefusesToProvisionWithoutConsent: the default policy is prompt and
// there is no prompt, so nothing is installed and the run degrades honestly.
func TestExecuteRefusesToProvisionWithoutConsent(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"go.mod": "module x"})
	r := newFakeRunner("go") // go is present; scip-go is not
	r.handle("go", func(c Command) (Result, error) {
		if contains(c.Args, "install") {
			t.Error("installed scip-go without consent")
		}
		return Result{Stdout: []byte("go version go1.25.0\n")}, nil
	})

	cfg := testConfig(root, config.AccuracyAuto)
	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if len(plan.Steps) != 1 || !plan.Steps[0].NeedsProvision {
		t.Fatalf("expected one step needing provisioning, got %+v", plan.Steps)
	}

	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if len(res.Indexes) != 0 {
		t.Fatal("indexed with no indexer installed")
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Err == nil {
		t.Fatalf("skips = %+v, want one carrying the refusal", res.Skipped)
	}
	if len(res.Provisioned) != 0 {
		t.Fatalf("provisioned %+v", res.Provisioned)
	}
}

// TestJavaNeedsAnExplicitBuildOptIn. Indexing Java runs Maven or Gradle, and a
// Maven plugin is arbitrary code from the repository. Off by default, and the
// reason has to be legible.
func TestJavaNeedsAnExplicitBuildOptIn(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"pom.xml": "<project/>"})
	r := newFakeRunner("java", "mvn")
	r.install("scip-java")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if len(plan.BlockedSteps()) != 1 {
		t.Fatalf("java ran without an opt-in: %+v", plan.Steps)
	}
	reason := plan.BlockedSteps()[0].Reason
	for _, want := range []string{"arbitrary code", "docker"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}

	plan, err = NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r, AllowRepoBuild: true})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if len(plan.Runnable()) != 1 {
		t.Fatalf("java still blocked after the opt-in: %+v", plan.Steps)
	}
}

func TestLanguageFilterIsHonoured(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go", "node")
	r.install("scip-go")
	r.install("scip-typescript")
	r.install("scip-python")

	// A restriction written in the user's own config is authoritative, so an
	// excluded language is a reported skip rather than an exact-mode failure.
	cfg := testConfig(root, config.AccuracyExact)
	cfg.IndexerLanguages = []string{"go"}

	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	if err := plan.Err(); err != nil {
		t.Fatalf("an explicit language restriction must not fail exact mode: %v", err)
	}
	runnable := plan.Runnable()
	if len(runnable) != 1 || runnable[0].Project.Language != LangGo {
		t.Fatalf("runnable = %+v, want only go", runnable)
	}
	for _, s := range plan.BlockedSteps() {
		if !strings.Contains(s.Reason, "accuracy.indexers.languages") {
			t.Errorf("skip reason %q does not name the setting responsible", s.Reason)
		}
	}
}

// TestExecuteHonoursCancellation: a cancelled run is not a failed one, and the
// distinction reaches the caller through the error kind.
func TestExecuteHonoursCancellation(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go", "node")
	r.install("scip-go")
	r.install("scip-typescript")
	r.install("scip-python")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Execute(ctx, plan, Options{Runner: r})
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if k := errs.KindOf(err); k != errs.KindCancelled {
		t.Errorf("KindOf = %q, want %q", k, errs.KindCancelled)
	}
}

// TestIndexerArgvIsSafe checks the shape of what actually gets executed: the
// project directory travels as the working directory, the only writable path is
// the output directory, and no argument was built by interpolation.
func TestIndexerArgvIsSafe(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{
		"services/api/tsconfig.json": "{}",
		"services/api/package.json":  "{}",
	})
	r := newFakeRunner("node")
	r.install("scip-typescript")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })

	var indexCmd *Command
	for i, c := range r.calls() {
		if filepath.Base(c.Path) == "scip-typescript" && contains(c.Args, "index") {
			indexCmd = &r.calls()[i]
		}
	}
	if indexCmd == nil {
		t.Fatal("scip-typescript was never run")
	}
	wantDir := filepath.Join(root, "services", "api")
	if indexCmd.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", indexCmd.Dir, wantDir)
	}
	for _, a := range indexCmd.Args {
		if strings.HasPrefix(a, root) && !strings.HasPrefix(a, res.OutputDir) {
			t.Errorf("a repository path was passed as an argument: %q", a)
		}
	}
	out := flagValue(indexCmd.Args, "--output")
	if filepath.Dir(out) != res.OutputDir {
		t.Errorf("--output %q is not inside the output directory %q", out, res.OutputDir)
	}
	// tsconfig.json is present, so the inferred-config flag must not be passed.
	if contains(indexCmd.Args, "--infer-tsconfig") {
		t.Error("--infer-tsconfig passed despite a tsconfig.json being present")
	}
}

// TestExecuteReportsAnIndexerThatWroteNothing: exit 0 with no output file is a
// real failure mode for these tools, and returning a path that does not exist
// would surface later as a RepoCortex bug.
func TestExecuteReportsAnIndexerThatWroteNothing(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"go.mod": "module x"})
	r := newFakeRunner("go", "scip-go")
	r.handle("scip-go", func(Command) (Result, error) { return Result{}, nil }) // exits 0, writes nothing

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto),
		Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if len(res.Indexes) != 0 {
		t.Fatalf("reported an index that was never written: %+v", res.Indexes)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Err == nil {
		t.Fatalf("skips = %+v", res.Skipped)
	}
}

func TestCleanupOutputOnlyRemovesOurOwnDirectories(t *testing.T) {
	t.Parallel()
	precious := writeRepo(t, map[string]string{"important.txt": "keep me"})
	CleanupOutput(precious)
	if _, err := os.Stat(filepath.Join(precious, "important.txt")); err != nil {
		t.Fatalf("CleanupOutput removed a directory it did not create: %v", err)
	}

	dir, err := OutputDir()
	if err != nil {
		t.Fatal(err)
	}
	CleanupOutput(dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("output directory survived cleanup: %v", err)
	}
}

func TestPlanTimeoutFallsBackToTheDefault(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"go.mod": "module x"})
	cfg := testConfig(root, config.AccuracyAuto)
	cfg.IndexerTimeout = 0

	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: newFakeRunner("go")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Timeout != DefaultTimeout {
		t.Fatalf("timeout = %s, want %s", plan.Timeout, DefaultTimeout)
	}

	cfg.IndexerTimeout = 42 * time.Second
	plan, err = NewPlanWith(context.Background(), root, cfg, Options{Runner: newFakeRunner("go")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Timeout != 42*time.Second {
		t.Fatalf("timeout = %s, want the configured value", plan.Timeout)
	}
}

// snapshot renders a repository's contents so a test can prove nothing changed.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(filepath.ToSlash(rel))
		if !info.IsDir() {
			b.WriteString(":" + itoa(int(info.Size())))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return b.String()
}

// TestExecuteLeavesOnlyIndexesInTheOutputDirectory pins what the output
// directory is for.
//
// The container route points HOME and the JVM's user.home at the output
// mount, so every build cache — Maven's .m2, coursier's JDK archives — lands
// there instead of in the repository. That is the containment the runner
// promises. It is not a promise to keep 700 MB of somebody else's JDK under
// the system temp directory until the OS gets round to it: Results.OutputDir
// survives Execute only so `scip import` can read the index later, and the
// index is the only thing that has to survive.
func TestExecuteLeavesOnlyIndexesInTheOutputDirectory(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go")
	r.install("scip-go")
	// The indexer behaves like a real build: it writes its caches beside the
	// index, in the one directory it was told is writable.
	r.handle("scip-go", func(c Command) (Result, error) {
		out := flagValue(c.Args, "--output")
		if out == "" {
			return Result{}, nil
		}
		for _, junk := range []string{".m2/repository/x.jar", ".cache/coursier/arc/jdk.tar.gz", "stray.log"} {
			p := filepath.Join(filepath.Dir(out), junk)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return Result{}, err
			}
			if err := os.WriteFile(p, []byte("cache"), 0o600); err != nil {
				return Result{}, err
			}
		}
		return Result{}, os.WriteFile(out, minimalSCIPIndex, 0o600)
	})

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto), Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if len(res.Indexes) == 0 {
		t.Fatal("nothing was indexed, so the test proves nothing")
	}

	entries, err := os.ReadDir(res.OutputDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".scip" {
			t.Errorf("%s survived in the output directory; only the indexes should", e.Name())
		}
	}
	for _, ix := range res.Indexes {
		if _, err := os.Stat(ix.IndexPath); err != nil {
			t.Errorf("the index itself was pruned: %v", err)
		}
	}
}

// TestExecuteRemovesAnOutputDirectoryItCreatedAndDidNotUse: a run whose every
// step was skipped after the directory was made must not leave an empty
// cortex-scip-* behind, and must not advertise a directory that no longer
// exists.
func TestExecuteRemovesAnOutputDirectoryItCreatedAndDidNotUse(t *testing.T) {
	t.Parallel()
	root := polyglotRepo(t)
	r := newFakeRunner("go")
	r.install("scip-go")
	r.handle("scip-go", func(Command) (Result, error) {
		return Result{ExitCode: 1, Stderr: []byte("build failed")}, nil
	})

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto), Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute in auto mode reports a failed indexer as a skip, not an error: %v", err)
	}
	if len(res.Indexes) != 0 {
		t.Fatalf("expected no indexes, got %d", len(res.Indexes))
	}
	if res.OutputDir != "" {
		if _, statErr := os.Stat(res.OutputDir); statErr == nil {
			t.Errorf("an output directory with no index in it was left behind: %s", res.OutputDir)
		} else {
			t.Errorf("Results names an output directory that does not exist: %s", res.OutputDir)
		}
		CleanupOutput(res.OutputDir)
	}
}

// TestOutputDirGuardRefusesWhatItDidNotCreate pins the guard both
// CleanupOutput and pruneOutput rely on. The basename prefix alone is not
// ownership: a repository can contain a directory called cortex-scip-x, and
// a symlink under the temp root can carry the prefix while pointing anywhere.
// Only a directory that really sits directly under os.TempDir() is ours.
func TestOutputDirGuardRefusesWhatItDidNotCreate(t *testing.T) {
	t.Parallel()
	mk := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// A decoy with the right name in the wrong place: nested under a per-test
	// directory, so not a direct child of the temp root.
	decoy := mk(t, filepath.Join(t.TempDir(), "cortex-scip-decoy"))
	CleanupOutput(decoy)
	if err := pruneOutput(decoy, nil); err != nil {
		t.Errorf("pruneOutput on a decoy returned %v; it should refuse silently", err)
	}
	if _, err := os.Stat(filepath.Join(decoy, "keep.txt")); err != nil {
		t.Errorf("a directory this package did not create was touched: %v", err)
	}

	// A symlink directly under the temp root, correctly named, pointing at a
	// directory somewhere else. The name is ours; the target is not.
	victim := mk(t, filepath.Join(t.TempDir(), "victim"))
	link := filepath.Join(os.TempDir(), "cortex-scip-link-"+filepath.Base(t.TempDir()))
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	CleanupOutput(link)
	if err := pruneOutput(link, nil); err != nil {
		t.Errorf("pruneOutput through a symlink returned %v; it should refuse silently", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Errorf("the symlink's target was touched: %v", err)
	}

	// The real thing, for contrast: created by OutputDir, directly under the
	// temp root, and therefore pruned and removable.
	real, err := OutputDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(real) })
	mk(t, filepath.Join(real, ".cache"))
	if err := pruneOutput(real, nil); err != nil {
		t.Fatalf("pruneOutput on our own directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, ".cache")); !os.IsNotExist(err) {
		t.Errorf("our own cache directory survived pruning: %v", err)
	}
	CleanupOutput(real)
	if _, err := os.Stat(real); !os.IsNotExist(err) {
		t.Errorf("our own output directory survived cleanup: %v", err)
	}
}

// TestExecuteWritesIntoACallerOutputDirAndLeavesItAlone pins the contract
// `indexers install --out-dir` relies on: a caller-supplied directory receives
// the indexes, and Execute never prunes or removes it, because it did not
// create it (ownedOutputDir).
func TestExecuteWritesIntoACallerOutputDirAndLeavesItAlone(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"go.mod": "module x"})
	r := newFakeRunner("go")
	r.install("scip-go")

	plan, err := NewPlanWith(context.Background(), root, testConfig(root, config.AccuracyAuto), Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	out := t.TempDir()
	marker := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Execute(context.Background(), plan, Options{Runner: r, OutputDir: out})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.OutputDir != out {
		t.Fatalf("OutputDir = %q, want the caller's %q", res.OutputDir, out)
	}
	if len(res.Indexes) != 1 || filepath.Dir(res.Indexes[0].IndexPath) != out {
		t.Fatalf("want one index inside %s, got %+v", out, res.Indexes)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("Execute pruned a directory it does not own: %v", err)
	}
}
