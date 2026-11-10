package fs

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

func TestIgnorePredicateExcludesNoisyDirectories(t *testing.T) {
	w, err := NewWalker(t.TempDir(), config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	ignore := w.IgnorePredicate()

	// Watching these is the motivating case: .git alone produces thousands of
	// events during any ordinary git operation.
	for _, dir := range []string{".git", "node_modules", "vendor", "target", "build", "dist"} {
		if !ignore(dir, true) {
			t.Errorf("%q should be ignored for watching", dir)
		}
	}
	for _, dir := range []string{"src", "internal", "cmd", "app/service"} {
		if ignore(dir, true) {
			t.Errorf("%q should be watched", dir)
		}
	}
}

func TestIgnorePredicateOnFiles(t *testing.T) {
	w, err := NewWalker(t.TempDir(), config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	ignore := w.IgnorePredicate()

	if !ignore(".git/config", false) {
		t.Error(".git/config should be ignored")
	}
	if ignore("internal/store/sqlite.go", false) {
		t.Error("a normal source file should not be ignored")
	}
}

// TestIgnorePredicateRespectsRepoHardening: a repository cannot weaken the
// default exclusions, so the predicate must not either.
func TestIgnorePredicateCannotBeWeakenedByRepo(t *testing.T) {
	cfg := config.Defaults()
	// Exactly what a hostile .cortex.yml yields after config.applyRepo.
	cfg.Exclude = append(cfg.Exclude, "!.git/**", "!node_modules/**")

	w, err := NewWalker(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ignore := w.IgnorePredicate()

	if !ignore(".git", true) {
		t.Error("a repository re-included .git for watching")
	}
	if !ignore("node_modules", true) {
		t.Error("a repository re-included node_modules for watching")
	}
}

func TestIgnorePredicateHandlesRoot(t *testing.T) {
	w, err := NewWalker(t.TempDir(), config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	ignore := w.IgnorePredicate()
	for _, root := range []string{"", "."} {
		if ignore(root, true) {
			t.Errorf("root %q must never be ignored, or nothing is watched at all", root)
		}
	}
}
