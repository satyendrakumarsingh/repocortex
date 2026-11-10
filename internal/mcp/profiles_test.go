package mcp

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// A profile that names a tool nobody registered is inert, and inert in the worst
// way: it reads as a promise.
//
// `standard` listed `find_callees` and `get_file_outline`, neither of which is
// registered anywhere in this package. The names did nothing, so `standard` and
// `full` silently exposed an identical set while the profile table implied
// otherwise. Nothing failed, because nothing checked.
//
// The tool surface is a performance constraint here — every exposed schema is
// injected into the agent's context on every request — so which tools a profile
// exposes is a real product decision, and a decision expressed in a list nothing
// validates will drift from the code the first time a tool is renamed.

// TestProfilesOnlyNameRegisteredTools is the check that was missing.
func TestProfilesOnlyNameRegisteredTools(t *testing.T) {
	registered := registeredToolNames(t)

	for _, p := range []config.MCPProfile{
		config.MCPMinimal,
		config.MCPStandard,
		config.MCPFull,
	} {
		for name := range profileTools(p) {
			if !registered[name] {
				t.Errorf("profile %q exposes %q, which is not registered by any tool. "+
					"A profile naming a nonexistent tool is a promise the server cannot keep.",
					p, name)
			}
		}
	}
}

// TestEveryRegisteredToolIsReachableFromSomeProfile catches the mirror defect:
// a tool that was built, registered, and then reachable from no profile at all.
func TestEveryRegisteredToolIsReachableFromSomeProfile(t *testing.T) {
	reachable := map[string]bool{}
	for _, p := range []config.MCPProfile{
		config.MCPMinimal,
		config.MCPStandard,
		config.MCPFull,
	} {
		for name := range profileTools(p) {
			reachable[name] = true
		}
	}

	for name := range registeredToolNames(t) {
		if !reachable[name] {
			t.Errorf("tool %q is registered but no profile exposes it, so no agent can call it", name)
		}
	}
}

// registeredToolNames asks a real server what it actually exposes, rather than
// re-listing the names here — a second hand-maintained list would drift exactly
// like the first one did.
//
// It must be a FULLY WIRED server. A tool is registered only when both its
// profile allows it and its dependencies are present, so a server built without
// a Graph reports `get_task_context`, `impact_analysis` and `find_tests` as
// absent — and this test would then "discover" that three real tools do not
// exist. The first version of it did exactly that.
func registeredToolNames(t *testing.T) map[string]bool {
	t.Helper()
	sess := newGraphTestServer(t, nil)

	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	if len(out) == 0 {
		t.Fatal("the test server registered no tools; this test would pass vacuously")
	}
	return out
}
