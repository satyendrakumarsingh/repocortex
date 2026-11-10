package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// This file holds the test doubles. Everything in the package reaches the
// operating system through Runner, so a fake one turns planning, provisioning
// and execution into pure functions — which is why no test in this package
// needs a network, a JVM or a Docker daemon.

// fakeBin is where pretend executables live. It is under os.TempDir so that it
// is absolute on every platform, which Command.validate insists on.
func fakeBin(name string) string {
	return filepath.Join(os.TempDir(), "cortex-fake-bin", name)
}

type fakeRunner struct {
	mu sync.Mutex

	// present is the set of tools on the fake PATH.
	present map[string]bool

	// handlers answer a command, keyed by the base name of its executable.
	handlers map[string]func(Command) (Result, error)

	// recorded is every command that was run, in order.
	recorded []Command
}

func newFakeRunner(tools ...string) *fakeRunner {
	f := &fakeRunner{
		present:  map[string]bool{},
		handlers: map[string]func(Command) (Result, error){},
	}
	for _, t := range tools {
		f.present[t] = true
	}
	return f
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present[file] {
		return "", os.ErrNotExist
	}
	return fakeBin(file), nil
}

func (f *fakeRunner) Run(_ context.Context, c Command) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{Argv: c.Argv()}, err
	}
	f.mu.Lock()
	f.recorded = append(f.recorded, c)
	h := f.handlers[filepath.Base(c.Path)]
	f.mu.Unlock()

	if h != nil {
		res, err := h(c)
		res.Argv = c.Argv()
		res.Dir = c.Dir
		return res, err
	}
	// Default: the tool exists and answers with a plausible version banner.
	return Result{Argv: c.Argv(), Dir: c.Dir, Stdout: []byte(filepath.Base(c.Path) + " 1.2.3\n")}, nil
}

func (f *fakeRunner) handle(name string, fn func(Command) (Result, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[name] = fn
}

// minimalSCIPIndex is the smallest byte string Executor.Run will accept: a
// SCIP Index carrying one Document (field 2). It has to be real wire format
// rather than a placeholder string, because Run now distinguishes an index with
// documents from one without — that distinction is what catches an indexer that
// exits 0 having loaded nothing. See scipcontent.go.
//
//	0x0a 0x00  field 1 (metadata), length 0
//	0x12 0x00  field 2 (documents), length 0
var minimalSCIPIndex = []byte{0x0a, 0x00, 0x12, 0x00}

// install makes an indexer binary appear on the fake PATH and makes it write an
// index file when asked to, so Executor.Run's "did it actually produce output"
// check passes.
func (f *fakeRunner) install(name string) {
	f.mu.Lock()
	f.present[name] = true
	f.mu.Unlock()
	f.handle(name, func(c Command) (Result, error) {
		if out := flagValue(c.Args, "--output"); out != "" {
			if err := os.WriteFile(out, minimalSCIPIndex, 0o600); err != nil {
				return Result{ExitCode: 1, Stderr: []byte(err.Error())}, err
			}
		}
		return Result{Stdout: []byte(name + " 1.2.3\n")}, nil
	})
}

func (f *fakeRunner) calls() []Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Command(nil), f.recorded...)
}

// argvs renders every recorded command as one string, for "did this string ever
// reach a subprocess" assertions.
func (f *fakeRunner) argvs() []string {
	var out []string
	for _, c := range f.calls() {
		out = append(out, strings.Join(c.Argv(), " "))
	}
	return out
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// writeRepo materialises a repository from a path -> contents map and returns
// its root.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// projectDirs renders detection output compactly for comparison.
func projectDirs(ps []Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p.Language)+":"+p.Dir)
	}
	return out
}
