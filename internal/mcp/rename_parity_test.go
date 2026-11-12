package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/rename"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// TestRenameCollectAgreesWithPlanRename: `repocortex rename` must act on the
// sites plan_rename shows an agent, or the two surfaces describe different
// renames. The definition site is the one deliberate addition.
func TestRenameCollectAgreesWithPlanRename(t *testing.T) {
	var (
		reg   *registry.Registry
		runID int64
	)
	// plan_rename is a standard-profile tool; newGraphTestServer enables it.
	sess := newGraphTestServer(t, func(ctx context.Context, r *registry.Registry, id int64) {
		seedMixedConfidence(ctx, r, id)
		reg, runID = r, id
	})

	res := callTool(t, sess, "plan_rename", map[string]any{"name": "Login", "new_name": "SignIn", "path": "auth/"})
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		RenamePlan *RenamePlan `json:"rename_plan"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.RenamePlan == nil {
		t.Fatalf("no rename_plan in %s (%v)", raw, err)
	}
	var fromMCP []string
	for _, e := range append(resp.RenamePlan.Apply, resp.RenamePlan.Review...) {
		fromMCP = append(fromMCP, fmt.Sprintf("%s:%d:%s", e.Path, e.Line, e.Confidence))
	}

	p, err := rename.Collect(context.Background(), reg, runID, rename.Request{Old: "Login", New: "SignIn", Path: "auth/"})
	if err != nil {
		t.Fatal(err)
	}
	var fromCLI []string
	for _, s := range p.Sites {
		if s.Kind == rename.KindDefinition {
			continue
		}
		fromCLI = append(fromCLI, fmt.Sprintf("%s:%d:%s", s.Path, s.Line, s.Confidence))
	}

	if len(fromMCP) == 0 {
		t.Fatal("fixture premise broken: plan_rename returned no sites, so agreement proves nothing")
	}
	sort.Strings(fromMCP)
	sort.Strings(fromCLI)
	if fmt.Sprint(fromMCP) != fmt.Sprint(fromCLI) {
		t.Errorf("plan_rename sites %v\nrename.Collect sites %v", fromMCP, fromCLI)
	}
}
