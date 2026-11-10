package planchange

import (
	"context"
	"errors"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestPlanChangeNegativeEmptyTarget(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	caps := capability.NewDeps(reg, graph.New(db))
	planner := NewPlanner(caps)

	_, err = planner.Plan(ctx, 1, "", DefaultOptions())
	if err == nil {
		t.Fatal("expected error for empty target, got nil")
	}
}

func TestPlanChangeNegativeTargetNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	caps := capability.NewDeps(reg, graph.New(db))
	planner := NewPlanner(caps)

	_, err = planner.Plan(ctx, run.ID, "NonExistentService", DefaultOptions())
	if err == nil {
		t.Fatal("expected error for non-existent target, got nil")
	}
	if !errors.Is(err, capability.ErrTargetNotFound) {
		t.Errorf("expected ErrTargetNotFound, got: %v", err)
	}
}

func TestPlanChangePositiveWithCallers(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}

	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	fTarget := model.File{Path: "internal/service/user.go", Language: "go", ContentHash: "hUser", ParseStatus: "ok"}
	fileIDTarget, err := w.AddFile(ctx, &fTarget)
	if err != nil {
		t.Fatal(err)
	}

	fCaller := model.File{Path: "internal/handler/user.go", Language: "go", ContentHash: "hHandler", ParseStatus: "ok"}
	fileIDCaller, err := w.AddFile(ctx, &fCaller)
	if err != nil {
		t.Fatal(err)
	}

	targetSym := model.Symbol{
		FileID:     fileIDTarget,
		Kind:       model.KindFunction,
		Name:       "UpdateUser",
		FQN:        "service.UpdateUser",
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
		Exported:   true,
		Range:      model.Range{Start: model.Position{Line: 20}},
	}
	targetID, err := w.AddSymbol(ctx, &targetSym)
	if err != nil {
		t.Fatal(err)
	}

	callerSym := model.Symbol{
		FileID:     fileIDCaller,
		Kind:       model.KindFunction,
		Name:       "HandleUpdate",
		FQN:        "handler.HandleUpdate",
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
		Exported:   true,
		Range:      model.Range{Start: model.Position{Line: 45}},
	}
	callerID, err := w.AddSymbol(ctx, &callerSym)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Add call edge from caller to target
	edge := model.Edge{
		SrcID:      callerID,
		DstID:      targetID,
		Kind:       model.EdgeCalls,
		Confidence: model.ConfidenceHigh,
		Evidence: model.Evidence{
			FilePath: "internal/handler/user.go",
			Range:    model.Range{Start: model.Position{Line: 50}},
			Source:   model.SourceSourceCode,
		},
	}
	if _, err := reg.AddResolvedEdges(ctx, run.ID, []model.Edge{edge}); err != nil {
		t.Fatal(err)
	}

	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	gr := graph.New(db)
	caps := capability.NewDeps(reg, gr)
	planner := NewPlanner(caps)

	plan, err := planner.Plan(ctx, run.ID, "service.UpdateUser", DefaultOptions())
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	if plan.Target.Name != "UpdateUser" {
		t.Errorf("expected target Name UpdateUser, got %s", plan.Target.Name)
	}
	if plan.DirectCallers != 1 {
		t.Errorf("expected 1 direct caller, got %d", plan.DirectCallers)
	}

	// Verify plan steps
	var hasModifyTarget, hasUpdateCaller bool
	for _, s := range plan.Steps {
		if s.Kind == StepModifyTarget && s.Target == "service.UpdateUser" {
			hasModifyTarget = true
		}
		if s.Kind == StepUpdateCaller && s.Target == "handler.HandleUpdate" {
			hasUpdateCaller = true
		}
	}

	if !hasModifyTarget {
		t.Error("expected StepModifyTarget in plan steps")
	}
	if !hasUpdateCaller {
		t.Error("expected StepUpdateCaller in plan steps")
	}
	if len(plan.Preconditions) == 0 {
		t.Error("expected preconditions in plan")
	}
	if len(plan.Postconditions) == 0 {
		t.Error("expected postconditions in plan")
	}
}
