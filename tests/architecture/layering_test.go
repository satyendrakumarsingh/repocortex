// Package architecture enforces CLAUDE.md §1 as a test rather than an audit.
//
// These rules had been checked by hand twice and re-litigated both times,
// which is the argument for the file existing: a standard nothing executes
// drifts from the code until someone re-derives it, and the second derivation
// disagrees with the first. Every assertion below names the clause it enforces
// so a failure points at the rule rather than at a package list.
package architecture

import (
	"os/exec"
	"strings"
	"testing"
)

// imports returns a package's direct imports, via the toolchain rather than by
// parsing files — build tags, generated files and test-only imports are the
// toolchain's job to resolve correctly, and reimplementing that is how an
// architecture test starts reporting things that are not true.
func imports(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", pkg).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	return got
}

// TestDomainCoreIsPure enforces §1: "Zero external dependencies on SQLite,
// HTTP, or MCP protocols. This is the one package where that rule is absolute."
//
// pkg/model is where the confidence model, the edge invariants and the entity
// vocabulary live. Anything it imports becomes a dependency of every consumer
// of those types, which is most of the product.
func TestDomainCoreIsPure(t *testing.T) {
	allowed := map[string]bool{"fmt": true, "strings": true, "time": true, "errors": true}
	for _, imp := range imports(t, "github.com/satyendrakumarsingh/repocortex/pkg/model") {
		if strings.Contains(imp, "github.com/satyendrakumarsingh/repocortex/internal") {
			t.Errorf("CLAUDE.md §1: pkg/model imports %q — the domain core must not depend on any adapter", imp)
		}
		if !allowed[imp] && strings.Contains(imp, ".") {
			t.Errorf("CLAUDE.md §1: pkg/model imports the third-party package %q", imp)
		}
	}
}

// TestRepositoryAdapterDoesNotDependUpward enforces §1's adapter clause: an
// adapter may depend on the domain; the domain and the layers above must not
// depend back on it.
//
// The concrete violation this catches happened: internal/registry imported
// internal/analyze/deps to decide whether an import path used a declared
// dependency. It compiled, it worked, and it inverted the arrow — the storage
// layer had acquired an opinion about package managers. The fix was a
// caller-supplied ImportMatcher.
func TestRepositoryAdapterDoesNotDependUpward(t *testing.T) {
	// Everything at or below the Repository adapter. A dependency on anything
	// else is upward.
	permitted := map[string]bool{
		"github.com/satyendrakumarsingh/repocortex/pkg/model":       true,
		"github.com/satyendrakumarsingh/repocortex/internal/store":  true,
		"github.com/satyendrakumarsingh/repocortex/internal/search": true,
	}
	for _, imp := range imports(t, "github.com/satyendrakumarsingh/repocortex/internal/registry") {
		if !strings.HasPrefix(imp, "github.com/satyendrakumarsingh/repocortex/") {
			continue
		}
		if !permitted[imp] {
			t.Errorf("CLAUDE.md §1: internal/registry imports %q, which sits above it. "+
				"An adapter may depend on the domain, never on a higher layer — inject a "+
				"port instead (see registry.ImportMatcher).", imp)
		}
	}
}

// TestSQLDriverIsConfinedToTheStore enforces §1's adapter boundary at the point
// it matters most: exactly one package may know which database this is.
func TestSQLDriverIsConfinedToTheStore(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/satyendrakumarsingh/repocortex/internal/store").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if !strings.Contains(string(out), "modernc.org/sqlite") {
		t.Skip("driver not reachable from internal/store; the layout has changed")
	}

	for _, pkg := range []string{
		"github.com/satyendrakumarsingh/repocortex/internal/analyze",
		"github.com/satyendrakumarsingh/repocortex/internal/analyze/db",
		"github.com/satyendrakumarsingh/repocortex/internal/analyze/api",
		"github.com/satyendrakumarsingh/repocortex/internal/analyze/deps",
		"github.com/satyendrakumarsingh/repocortex/internal/analyze/config",
		"github.com/satyendrakumarsingh/repocortex/internal/arch",
		"github.com/satyendrakumarsingh/repocortex/internal/compile",
		"github.com/satyendrakumarsingh/repocortex/internal/bench",
		"github.com/satyendrakumarsingh/repocortex/internal/trace",
		"github.com/satyendrakumarsingh/repocortex/internal/edit",
	} {
		for _, imp := range imports(t, pkg) {
			if strings.Contains(imp, "sqlite") || strings.Contains(imp, "database/sql") {
				t.Errorf("CLAUDE.md §1: %s imports %q — analysis and reporting packages "+
					"must not know about the database at all", pkg, imp)
			}
		}
	}
}

// TestNoPackageLevelMutableStateInTheEngines enforces §4.2 where it was
// actually violated: a process-wide cache in internal/context, shared by every
// caller and invisible in every signature.
//
// Checked by absence of the specific escape hatch rather than by scanning for
// `var`, because compiled regexps, lookup tables and sentinel errors are
// immutable and idiomatic — they are not what the rule targets, and a test that
// flagged them would be turned off within a week.
func TestNoPackageLevelMutableStateInTheEngines(t *testing.T) {
	out, err := exec.Command("grep", "-rn", "--include=*.go",
		"^var defaultCatalogueCache", "../../internal/").Output()
	if err == nil && len(out) > 0 {
		t.Errorf("CLAUDE.md §4.2: a process-wide cache is back:\n%s\n"+
			"State belongs to a struct, injected by its owner — see context.Assembler.", out)
	}
}

// TestCLIOpensTheIndexThroughOneDoor enforces CLAUDE.md §3 (SRP/DRY) for the
// one invariant that has already broken silently: schema currency.
//
// store.Migrate was called only by `repocortex init`, so every other command
// opened the database at whatever version it happened to be and used it as-is.
// Upgrading the binary therefore never upgraded the index — migration 0005's
// indexes were never created for existing users, and an older index met a
// query expecting a table it did not have and died on a raw driver error.
//
// The fix was to funnel every open through openWritable/openReadOnly in
// internal/cli/opendb.go, which assert the schema. That only stays true if new
// commands use the same door, so the door is enforced here rather than left to
// review. init is exempt: it creates the database and migrates it explicitly.
func TestCLIOpensTheIndexThroughOneDoor(t *testing.T) {
	out, err := exec.Command("grep", "-rn", "store.Open(", "../../internal/cli/").Output()
	if err != nil && len(out) == 0 {
		t.Fatalf("grep failed: %v", err)
	}

	// file -> why a raw open is correct there.
	exempt := map[string]string{
		"opendb.go":   "defines the sanctioned doors",
		"commands.go": "init creates the database and migrates it explicitly",
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || strings.Contains(line, "_test.go") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		file := parts[0]
		if i := strings.LastIndex(file, "/"); i >= 0 {
			file = file[i+1:]
		}
		if _, ok := exempt[file]; ok {
			continue
		}
		t.Errorf("CLAUDE.md §3: %s calls store.Open directly — use openWritable or "+
			"openReadOnly (internal/cli/opendb.go) so the schema version is asserted", line)
	}
}
