package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGoIndexToolchainRaisesToTheProjectDirective is the bug that emptied Go
// indexing: six of seven modules in a real repository declared `go 1.26.5`,
// GOTOOLCHAIN was pinned to RepoCortex's own older build, and every one of them
// failed to load with "go.mod requires go >= 1.26.5" and wrote no file.
func TestGoIndexToolchainRaisesToTheProjectDirective(t *testing.T) {
	t.Parallel()
	self, ok := parseGoVersion(runtime.Version())
	if !ok {
		t.Skipf("running a non-release toolchain (%s); the pin is left alone by design", runtime.Version())
	}
	newer := goVersion{1, self.minor + 1, 0}

	dir := writeRepo(t, map[string]string{"go.mod": "module x\n\ngo " +
		strings.TrimPrefix(newer.String(), "go") + "\n"})
	if got := goIndexToolchain(dir); got != newer.String() {
		t.Errorf("goIndexToolchain = %q, want %q: a project that needs a newer Go must get one, "+
			"or it produces no index at all", got, newer.String())
	}
}

// TestGoIndexToolchainNeverLowers: raising is safe, lowering is not. A
// repository must not be able to select an older compiler than the one RepoCortex
// would otherwise have used.
func TestGoIndexToolchainNeverLowers(t *testing.T) {
	t.Parallel()
	dir := writeRepo(t, map[string]string{"go.mod": "module x\n\ngo 1.16\n"})
	if got := goIndexToolchain(dir); got != runtime.Version() {
		t.Errorf("goIndexToolchain = %q, want the running toolchain %q", got, runtime.Version())
	}
}

// TestGoToolchainIgnoresToolchainDirective is the security property this fix had
// to preserve. GOTOOLCHAIN=auto, or the "<version>+auto" form, obeys a
// `toolchain` line and will DOWNLOAD AND EXECUTE the compiler it names —
// verified directly against the Go command:
//
//	$ GOTOOLCHAIN=go1.25.13+auto go list ./...   # go.mod: toolchain go1.99.9
//	go: downloading go1.99.9 (darwin/amd64)
//	$ GOTOOLCHAIN=go1.25.13 go list ./...        # same go.mod
//	x
//
// So the directive must never reach the value this package computes.
func TestGoToolchainIgnoresToolchainDirective(t *testing.T) {
	t.Parallel()
	dir := writeRepo(t, map[string]string{
		"go.mod": "module x\n\ngo 1.21\n\ntoolchain go1.99.9\n",
	})
	got := goIndexToolchain(dir)
	if strings.Contains(got, "99") {
		t.Errorf("goIndexToolchain = %q; the repository's toolchain directive chose the compiler", got)
	}
	if strings.Contains(got, "auto") {
		t.Errorf("goIndexToolchain = %q; +auto hands toolchain selection back to the repository", got)
	}
}

// TestGoDirectiveIsReRenderedFromIntegers: whatever a go.mod contains, the value
// that reaches the environment is built from parsed numbers. A go.mod cannot put
// a string of its choosing into a RepoCortex subprocess's environment.
func TestGoDirectiveIsReRenderedFromIntegers(t *testing.T) {
	t.Parallel()
	for _, mod := range []string{
		"module x\ngo 1.21 ; rm -rf /\n",
		"module x\ngo $(whoami)\n",
		"module x\ngo 1.999999999\n",
		"module x\ngo 2.0\n",
		"module x\n// go 1.99\n",
		"module x\ngo 1.99.9-attacker\n",
		"module x\n",
		"",
	} {
		dir := writeRepo(t, map[string]string{"go.mod": mod})
		got := goIndexToolchain(dir)
		if got != runtime.Version() {
			t.Errorf("go.mod %q gave GOTOOLCHAIN %q, want the unchanged pin %q",
				mod, got, runtime.Version())
		}
	}
}

// TestGoIndexToolchainWithoutGoMod: a directory with no go.mod leaves the pin
// exactly where goEnv put it.
func TestGoIndexToolchainWithoutGoMod(t *testing.T) {
	t.Parallel()
	if got := goIndexToolchain(t.TempDir()); got != runtime.Version() {
		t.Errorf("goIndexToolchain = %q, want %q", got, runtime.Version())
	}
}

// TestSCIPGoCommandCarriesAPinnedToolchain guards the wiring: the env scip-go
// runs with must name a specific release, never "auto", never empty.
func TestSCIPGoCommandCarriesAPinnedToolchain(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n"})
	e := &Executor{Runner: newFakeRunner("go")}
	cmd, err := e.command(
		Installed{Indexer: SCIPGo, Path: fakeBin("scip-go")},
		Project{Language: LangGo, Dir: "."},
		"", repo, repo, t.TempDir(), filepath.Join(t.TempDir(), "go.scip"))
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	got := envValue(cmd.Env, "GOTOOLCHAIN")
	if got == "" || got == "auto" || strings.HasSuffix(got, "+auto") {
		t.Errorf("GOTOOLCHAIN = %q; must name one release so no third party chooses the compiler", got)
	}
	if _, ok := parseGoVersion(got); !ok && got != runtime.Version() {
		t.Errorf("GOTOOLCHAIN = %q is not a Go release identifier", got)
	}
}

// --- empty index detection -------------------------------------------------

// emptyIndexBytes is what scip-go actually wrote for a module go/packages could
// not load: a Metadata field and nothing else.
var emptyIndexBytes = []byte{0x0a, 0x02, 0x08, 0x01}

func TestSCIPHasDocuments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		bytes   []byte
		want    bool
		wantErr bool
	}{
		{"metadata only", emptyIndexBytes, false, false},
		{"one document", minimalSCIPIndex, true, false},
		{"external symbols but no documents", []byte{0x0a, 0x00, 0x1a, 0x00}, false, false},
		{"empty file", nil, false, false},
		{"truncated length prefix", []byte{0x0a, 0x7f}, false, true},
		{"group wire type", []byte{0x0b}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "x.scip")
			if err := os.WriteFile(path, tc.bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := scipHasDocuments(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("scipHasDocuments = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunRejectsAnIndexWithNoDocuments: an indexer that exits 0 having loaded
// nothing must be a failure here, where the project can still be named. It used
// to be reported as success and surfaced at import as "none of the index's 0
// definition(s) matched a symbol in this repository", which blames the
// repository for a tool that never read it.
func TestRunRejectsAnIndexWithNoDocuments(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{"go.mod": "module x\n\ngo 1.21\n"})
	out := t.TempDir()

	r := newFakeRunner("go")
	r.present["scip-go"] = true
	r.handle("scip-go", func(c Command) (Result, error) {
		if o := flagValue(c.Args, "--output"); o != "" {
			if err := os.WriteFile(o, emptyIndexBytes, 0o600); err != nil {
				return Result{}, err
			}
		}
		// Exactly what scip-go does: success, and an empty index.
		return Result{Stdout: []byte("Visiting Packages [0/0]\n")}, nil
	})

	e := &Executor{Runner: r}
	ex, err := e.Run(context.Background(),
		Installed{Indexer: SCIPGo, Path: fakeBin("scip-go")},
		Project{Language: LangGo, Dir: "."}, repo, out)
	if err == nil {
		t.Fatal("Run reported success for an index containing no documents")
	}
	if !strings.Contains(err.Error(), "no documents") {
		t.Errorf("error = %v, want it to say the index has no documents", err)
	}
	if ex.IndexPath != "" {
		t.Errorf("IndexPath = %q, want empty so no caller tries to import it", ex.IndexPath)
	}
}
