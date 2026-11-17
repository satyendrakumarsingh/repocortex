package capability

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// TestFileCallerIsNamedButIsNotADefinition: an edge can start at the synthetic
// caller of a file's own code, so a caller list must be able to look it up by
// id; but it is in no name, qualified-name or directory lookup and not in the
// definition count.
func TestFileCallerIsNamedButIsNotADefinition(t *testing.T) {
	t.Parallel()
	name := registry.FileCallerName("src/a.test.ts")
	idx := buildSymbolIndex(1, map[string][]registry.SymbolIndexEntry{
		"helper": {{ID: 1, Name: "helper", FQN: "src/a.test.ts:helper", Kind: "function", FilePath: "src/a.test.ts"}},
		name: {{ID: 2, Name: name, FQN: registry.FileCallerFQN("src/a.test.ts"),
			Kind: registry.FileCallerKind, FilePath: "src/a.test.ts"}},
	})
	if got := idx.Len(); got != 1 {
		t.Errorf("Len = %d, want the one definition", got)
	}
	if s, ok := idx.Lookup(2); !ok || s.Name != name {
		t.Errorf("Lookup(2) = %+v, %v; the caller of an edge must be nameable", s, ok)
	}
	if got := idx.ByName(name); len(got) != 0 {
		t.Errorf("ByName(%q) = %v, want none", name, got)
	}
	if _, ok := idx.ByFQN(registry.FileCallerFQN("src/a.test.ts")); ok {
		t.Error("the synthetic caller is reachable by qualified name")
	}
	if got := idx.InDir("src"); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("InDir = %v, want only the definition", got)
	}
	if got := idx.WithPrefix("(top level", nil); len(got) != 0 {
		t.Errorf("WithPrefix found %v", got)
	}
}

// TestFileCallerOfATestFileIsATest: code outside every definition of a test
// file is test code, and a runner runs it wherever the language has no
// convention that says otherwise; in a Go test file it is not a Test function.
func TestFileCallerOfATestFileIsATest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		path string
		want bool
	}{
		{"src/request.test.ts", true},
		{"tests/test_info_dict.py", true},
		{"spec/app_spec.rb", true},
		{"pkg/model/model_test.go", false},
		{"src/request.ts", false},
	} {
		got := IsRunnableTest(registry.FileCallerKind, registry.FileCallerName(c.path), c.path)
		if got != c.want {
			t.Errorf("IsRunnableTest(file, %q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestFileCallerIsTestCodeExactlyWhenItsFileIs: the top level of a file is
// test code by its path and never by its name, which is built from the path
// and so cannot carry a test marker of its own. "(top level of
// scripts/test_data.sh)" is not a test because a word in it looks like one.
func TestFileCallerIsTestCodeExactlyWhenItsFileIs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		path string
		want bool
	}{
		{"tests/test_ops.py", true},
		{"src/app.test.ts", true},
		{"pkg/model/model_test.go", true},
		{"scripts/deploy.py", false},
		{"scripts/run_Test.sh", false},
		{"web/app.js", false},
	} {
		if got := IsTestSymbol(registry.FileCallerKind, registry.FileCallerName(c.path), c.path); got != c.want {
			t.Errorf("IsTestSymbol(top level of %s) = %v, want %v", c.path, got, c.want)
		}
	}
}
