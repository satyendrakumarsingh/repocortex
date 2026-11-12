package scale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/cli"
)

// The tests above read a graph inserted straight into the store. These build
// one the way a user does — source files on disk, then `repocortex init`, `index`
// and `resolve` — because a pipeline has costs a synthetic graph cannot show:
// parsing, extraction, the resolver's candidate lookups and the commit.

// sourceTree writes n Go files across n/20 packages under root.
//
// Each file defines an exported type with three methods and two functions,
// calls into the previous file in its package and into a shared util package,
// and every tenth file is a test that calls the file before it. So the result
// has what a real tree has: intra-package calls, cross-package calls, a hub
// (util.Log) and tests reachable through call edges.
func sourceTree(t *testing.T, root string, n int) {
	t.Helper()
	write := func(rel, body string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/scale\n\ngo 1.22\n")
	write("util/log.go", `package util

// Log is called from every generated file: the hub.
func Log(msg string) {}

// Wrap decorates an error.
func Wrap(err error, msg string) error { return err }
`)

	perPkg := 20
	for i := 0; i < n; i++ {
		pkg := fmt.Sprintf("svc%03d", i/perPkg)
		if i%10 == 9 {
			write(fmt.Sprintf("%s/unit%d_test.go", pkg, i), fmt.Sprintf(`package %s

import "testing"

func TestUnit%d(t *testing.T) {
	s := NewUnit%d()
	if err := s.Run("x"); err != nil {
		t.Fatal(err)
	}
}
`, pkg, i-1, i-1))
			continue
		}
		prev := ""
		if i%perPkg != 0 && (i-1)%10 != 9 {
			prev = fmt.Sprintf("\tNewUnit%d().Run(id)\n", i-1)
		}
		write(fmt.Sprintf("%s/unit%d.go", pkg, i), fmt.Sprintf(`package %s

import "example.com/scale/util"

// Unit%d is generated unit %d.
type Unit%d struct{ name string }

// NewUnit%d builds a Unit%d.
func NewUnit%d() *Unit%d { return &Unit%d{name: "unit%d"} }

// Run executes the unit.
func (u *Unit%d) Run(id string) error {
	util.Log(u.name)
%s	return u.validate%d(id)
}

func (u *Unit%d) validate%d(id string) error {
	if id == "" {
		return util.Wrap(nil, "empty id")
	}
	return helper%d(id)
}

func helper%d(id string) error {
	util.Log(id)
	return nil
}

// orphan%d is never called and never named anywhere else.
func orphan%d() int { return %d }
`, pkg, i, i, i, i, i, i, i, i, i, i, prev, i, i, i, i, i, i, i, i))
	}
}

// repocortex runs the CLI in-process against root and returns stdout.
func repocortex(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := cli.NewRoot()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--project", root, "--quiet"}, args...))
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("repocortex %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

type indexRun struct {
	files   int
	elapsed time.Duration
	symbols int
	edges   int
}

// indexTree generates n source files and indexes them end to end.
func indexTree(t *testing.T, n int) (string, indexRun) {
	t.Helper()
	t.Setenv("REPOCORTEX_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	sourceTree(t, root, n)

	start := time.Now()
	repocortex(t, root, "init")
	repocortex(t, root, "index")
	repocortex(t, root, "resolve")
	elapsed := time.Since(start)

	var status struct {
		Files   int `json:"files"`
		Symbols int `json:"symbols"`
		Edges   int `json:"relationships"`
	}
	raw := repocortex(t, root, "--json", "status")
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatalf("status --json: %v\n%s", err, raw)
	}
	return root, indexRun{files: status.Files, elapsed: elapsed, symbols: status.Symbols, edges: status.Edges}
}

// Indexing must scale no worse than linearly: twice the files may take about
// twice as long, never four times.
func TestIndexingScalesLinearly(t *testing.T) {
	n := *flagSourceFiles
	_, half := indexTree(t, n/2)
	root, full := indexTree(t, n)

	t.Logf("%6d files: %8s  %7d symbols %7d edges  (%.0f files/s)",
		half.files, half.elapsed.Round(time.Millisecond), half.symbols, half.edges,
		float64(half.files)/half.elapsed.Seconds())
	t.Logf("%6d files: %8s  %7d symbols %7d edges  (%.0f files/s)",
		full.files, full.elapsed.Round(time.Millisecond), full.symbols, full.edges,
		float64(full.files)/full.elapsed.Seconds())

	if full.files < n {
		t.Fatalf("indexed %d files of the %d generated", full.files, n)
	}
	// Seven definitions per source file, one per test file: the extractor
	// must not be dropping any as the tree grows.
	if min := n * 5; full.symbols < min {
		t.Errorf("found %d symbols in %d files; expected at least %d", full.symbols, n, min)
	}
	if full.edges == 0 {
		t.Error("no relationships were bound; resolve did nothing")
	}

	// Doubling the input may cost up to 3.2x the time before this fails. A
	// linear pipeline measures about 2x; the slack absorbs fixed start-up
	// cost on the smaller run and noise on a shared machine. Skipped below
	// half a second, where the fixed cost dominates and the ratio is noise.
	if half.elapsed > 500*time.Millisecond {
		if ratio := full.elapsed.Seconds() / half.elapsed.Seconds(); ratio > 3.2 {
			t.Errorf("doubling the tree multiplied index time by %.1f; the pipeline is no longer linear", ratio)
		}
	}
	// 4s fixed, 25ms per file: a floor of 40 files a second, several times
	// below what a laptop measures.
	requireWithin(t, "init + index + resolve", measurement{elapsed: full.elapsed},
		budget{fixed: 4 * time.Second, perUnit: 25 * time.Millisecond}, n)

	t.Run("queries on the indexed tree", func(t *testing.T) { queriesOnIndexedTree(t, root, n) })
}

// queriesOnIndexedTree asks the questions a user asks, of a tree this size,
// and checks both the answers and how long they took.
func queriesOnIndexedTree(t *testing.T, root string, n int) {
	q := budget{fixed: 3 * time.Second, perUnit: 2 * time.Millisecond}

	// The hub: every generated source file calls util.Log.
	var callers string
	m := measure(func() { callers = repocortex(t, root, "callers", "Log", "--limit", "20") })
	if !strings.Contains(callers, "20 caller(s)") {
		t.Errorf("util.Log is called from every file; expected a full page of callers:\n%s", callers)
	}
	requireWithin(t, "callers of the hub", m, q, n)

	// Unused code: every source file defines one orphan, named nowhere else.
	var dead struct {
		Likely     int `json:"likely"`
		Candidates []struct {
			Name      string `json:"name"`
			Certainty string `json:"certainty"`
		} `json:"candidates"`
	}
	m = measure(func() {
		raw := repocortex(t, root, "--json", "dead-code", "--limit", "25")
		if err := json.Unmarshal([]byte(raw), &dead); err != nil {
			t.Fatalf("dead-code --json: %v\n%s", err, raw)
		}
	})
	if len(dead.Candidates) != 25 {
		t.Errorf("dead-code returned %d candidates, want a full page of 25", len(dead.Candidates))
	}
	for _, c := range dead.Candidates {
		if !strings.HasPrefix(c.Name, "orphan") || c.Certainty != "likely" {
			t.Errorf("unexpected candidate %s (%s); only the orphans are unused", c.Name, c.Certainty)
		}
	}
	requireWithin(t, "dead-code", m, q, n)

	// A route the generator built: Run -> validate -> helper -> util.Log.
	var route string
	m = measure(func() { route = repocortex(t, root, "path", "helper0", "Log") })
	if !strings.Contains(route, "Route 1 (1 hop(s))") {
		t.Errorf("helper0 calls util.Log directly; expected a one-hop route:\n%s", route)
	}
	requireWithin(t, "path", m, q, n)

	// The same tool an agent calls, through the shell front end.
	var agent string
	m = measure(func() { agent = repocortex(t, root, "tool", "call", "analyze_impact", "symbol=Log") })
	if !strings.Contains(agent, "Risk") {
		t.Errorf("analyze_impact did not answer:\n%s", agent)
	}
	if len(agent) > 24*1024 {
		t.Errorf("analyze_impact on the hub answered with %d bytes, over the 24 KB inline limit", len(agent))
	}
	requireWithin(t, "tool call analyze_impact", m, q, n)

	// Re-indexing an unchanged tree must not redo the work.
	m = measure(func() { repocortex(t, root, "index") })
	requireWithin(t, "re-index, nothing changed", m, budget{fixed: 2 * time.Second, perUnit: 4 * time.Millisecond}, n)
}
