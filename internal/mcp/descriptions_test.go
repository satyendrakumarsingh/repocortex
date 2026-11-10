package mcp

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// A description is a prompt. These assert the properties that make it one,
// not its exact wording.
func TestEveryToolDescriptionIsSubstantial(t *testing.T) {
	for name, desc := range toolDescriptions() {
		if len(desc) < 120 {
			t.Errorf("%s: description is %d chars; too short to steer a model", name, len(desc))
		}
	}
}

// The tools that return confidence must say what to do about it, not merely
// that it exists.
func TestConfidenceToolsExplainTheTiers(t *testing.T) {
	for _, name := range []string{"find_references", "find_callers", "plan_rename"} {
		desc, ok := toolDescriptions()[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if !strings.Contains(desc, "exact") {
			t.Errorf("%s: description never mentions the exact tier", name)
		}
	}
}

// The empty-result ambiguity is the single most dangerous thing this server can
// return. Every relationship tool must name it.
func TestRelationshipToolsWarnAboutEmptyResults(t *testing.T) {
	for _, name := range []string{"find_references", "find_callers"} {
		desc := toolDescriptions()[name]
		lower := strings.ToLower(desc)
		if !strings.Contains(lower, "empty") && !strings.Contains(lower, "no results") &&
			!strings.Contains(lower, "absence") {
			t.Errorf("%s: description does not warn that an empty result is ambiguous", name)
		}
	}
}

func TestOverviewToolIsMarkedAsTheStartingPoint(t *testing.T) {
	desc := toolDescriptions()["get_project_overview"]
	if !strings.Contains(strings.ToLower(desc), "first") &&
		!strings.Contains(strings.ToLower(desc), "start here") {
		t.Errorf("get_project_overview must tell the agent to call it first, got %q", desc)
	}
}

// newDescriptionsTestServer builds a *Server with the MCPFull profile and the
// graph wired in, so every tool this repository can register actually
// registers: registerTaskContext and registerCapabilities both gate on
// Deps.Graph, and profileTools(MCPFull) returns a nil allowlist (no filter).
// Anything less than full+graph here would make
// TestEveryRegisteredToolHasADescription pass vacuously by never registering
// the tools it is supposed to be checking.
//
// This goes through the same New(Deps{...}) construction every other test in
// this package uses (see newTestServer, newGraphTestServer) — it is not a
// second construction path, just the one that returns *Server instead of a
// connected client session, which is what Tools() requires.
func newDescriptionsTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := t.Context()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.ProjectName = "testproj"
	cfg.MCPProfile = config.MCPFull

	srv, err := New(Deps{Registry: reg, Graph: graph.New(db), Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// A tool registered without an entry in the table would ship with an empty
// description, which is worse than a bad one: the model gets no steer at all.
func TestEveryRegisteredToolHasADescription(t *testing.T) {
	s := newDescriptionsTestServer(t)
	descs := toolDescriptions()
	for _, name := range s.Tools() {
		if descs[name] == "" {
			t.Errorf("tool %q is registered with no description entry", name)
		}
	}
}
