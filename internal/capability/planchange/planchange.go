package planchange

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// StepKind categorizes an ordered action in the refactoring plan.
type StepKind string

const (
	StepVerifyBaseline StepKind = "verify_baseline"
	StepModifyTarget   StepKind = "modify_target"
	StepUpdateCaller   StepKind = "update_caller"
	StepVerifyTests    StepKind = "verify_tests"
)

// Step is one deterministic operation in the change sequence.
type Step struct {
	Order       int      `json:"order"`
	Kind        StepKind `json:"kind"`
	Target      string   `json:"target"`
	Path        string   `json:"path,omitempty"`
	Line        int      `json:"line,omitempty"`
	Description string   `json:"description"`
	Command     string   `json:"command,omitempty"`
}

// Plan is a complete, topologically ordered refactoring and verification blueprint.
type Plan struct {
	Target         capability.Target       `json:"target"`
	RiskScore      int                     `json:"risk_score"`
	RiskLevel      string                  `json:"risk_level"`
	TotalCallers   int                     `json:"total_callers"`
	DirectCallers  int                     `json:"direct_callers"`
	Steps          []Step                  `json:"steps"`
	AffectedTests  []string                `json:"affected_tests"`
	Preconditions  []string                `json:"preconditions"`
	Postconditions []string                `json:"postconditions"`
	Completeness   capability.Completeness `json:"completeness"`
}

// Options controls change plan generation.
type Options struct {
	MinConfidence  model.Confidence
	HubDegreeLimit int
}

// DefaultOptions returns standard change planning options.
func DefaultOptions() Options {
	return Options{
		MinConfidence: model.ConfidenceMedium,
	}
}

// Planner designs deterministic refactoring sequences from the graph.
type Planner struct {
	deps capability.Deps
}

// NewPlanner constructs a change planner.
func NewPlanner(deps capability.Deps) *Planner {
	return &Planner{deps: deps}
}

// Plan builds a change plan for modifying or refactoring targetSymbol.
func (p *Planner) Plan(ctx context.Context, runID int64, targetSymbol string, opts Options) (*Plan, error) {
	if strings.TrimSpace(targetSymbol) == "" {
		return nil, fmt.Errorf("target symbol must not be empty")
	}

	// 1. Run impact analysis to find direct callers, indirect callers, and risk score.
	imp, err := impact.AnalyzeWith(ctx, p.deps, runID, targetSymbol, impact.Options{
		MinConfidence:  opts.MinConfidence,
		HubDegreeLimit: opts.HubDegreeLimit,
	})
	if err != nil {
		return nil, err
	}

	// 2. Discover covering tests for the target.
	var testCmds []string
	testNames := make(map[string]bool)

	testHit, err := tests.FindTestsWith(ctx, p.deps, runID, imp.Target.FQN, tests.Options{})
	if err == nil {
		for _, th := range testHit.Tests {
			if !testNames[th.FilePath] {
				testNames[th.FilePath] = true
				testCmds = append(testCmds, th.FilePath)
			}
		}
	}

	// Also check direct callers for covering tests
	for _, caller := range imp.Direct {
		cTestHit, err := tests.FindTestsWith(ctx, p.deps, runID, caller.FQN, tests.Options{})
		if err == nil {
			for _, th := range cTestHit.Tests {
				if !testNames[th.FilePath] {
					testNames[th.FilePath] = true
					testCmds = append(testCmds, th.FilePath)
				}
			}
		}
	}

	plan := &Plan{
		Target:        imp.Target,
		RiskScore:     imp.Risk.Score,
		RiskLevel:     imp.Risk.Level,
		TotalCallers:  imp.TotalReached(),
		DirectCallers: len(imp.Direct),
		AffectedTests: testCmds,
		Completeness:  imp.Completeness,
		Preconditions: []string{
			"Target symbol exists and index resolution is current.",
			"Existing test suite passes cleanly before code modifications.",
		},
		Postconditions: []string{
			"Target symbol implementation matches new functional specification.",
			fmt.Sprintf("All %d direct caller(s) updated to new signature or contract.", len(imp.Direct)),
			"All affected test suites pass without regression.",
		},
	}

	stepOrder := 1

	// Step 1: Baseline verification
	if len(testCmds) > 0 {
		plan.Steps = append(plan.Steps, Step{
			Order:       stepOrder,
			Kind:        StepVerifyBaseline,
			Target:      "test_suite",
			Description: fmt.Sprintf("Verify baseline: run existing %d test file(s) before applying changes", len(testCmds)),
			Command:     fmt.Sprintf("go test -v %s", strings.Join(testCmds, " ")),
		})
		stepOrder++
	}

	// Step 2: Modify Target
	plan.Steps = append(plan.Steps, Step{
		Order:       stepOrder,
		Kind:        StepModifyTarget,
		Target:      imp.Target.FQN,
		Path:        imp.Target.FilePath,
		Line:        imp.Target.Line,
		Description: fmt.Sprintf("Modify target declaration and implementation for %s (%s)", imp.Target.Name, imp.Target.Kind),
	})
	stepOrder++

	// Step 3: Update Callers (in order)
	for _, caller := range imp.Direct {
		plan.Steps = append(plan.Steps, Step{
			Order:       stepOrder,
			Kind:        StepUpdateCaller,
			Target:      caller.FQN,
			Path:        caller.FilePath,
			Description: fmt.Sprintf("Update direct caller %s in %s to match new contract", caller.Name, caller.FilePath),
		})
		stepOrder++
	}

	// Step 4: Verification gate
	if len(testCmds) > 0 {
		plan.Steps = append(plan.Steps, Step{
			Order:       stepOrder,
			Kind:        StepVerifyTests,
			Target:      "regression_check",
			Description: "Verify refactoring: re-run all affected test suites and ensure all assertions pass",
			Command:     fmt.Sprintf("go test -v %s", strings.Join(testCmds, " ")),
		})
	}

	return plan, nil
}
