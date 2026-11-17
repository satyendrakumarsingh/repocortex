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
	Target    capability.Target `json:"target"`
	RiskScore int               `json:"risk_score"`
	RiskLevel string            `json:"risk_level"`

	// RiskUndetermined is impact.Risk.Undetermined: non-empty when the callers
	// of the target could not be determined. RiskLevel is then "unknown"
	// wherever it would have been "low", RiskScore is not a risk estimate, and
	// the caller steps below are the callers that were found, not the callers.
	RiskUndetermined string `json:"risk_undetermined,omitempty"`

	// RiskPartial is impact.Risk.Partial: why RiskLevel is a floor for a reason
	// other than undetermined callers (a walk cut at a bound, a name that occurs
	// where no call is recorded).
	//
	// Not in the JSON form; the plan-change command adds them to its own JSON.
	RiskPartial string `json:"-"`

	// ValueUse is the impact analysis's text check for a function passed as a
	// value. When it has a finding the caller steps are the callers that were
	// recorded, and the covering tests are the tests that were recorded.
	ValueUse capability.ValueUse `json:"-"`

	// TestsUndetermined is tests.TestReport.Undetermined for the target.
	TestsUndetermined string                  `json:"tests_undetermined,omitempty"`
	TotalCallers      int                     `json:"total_callers"`
	DirectCallers     int                     `json:"direct_callers"`
	Steps             []Step                  `json:"steps"`
	AffectedTests     []string                `json:"affected_tests"`
	Preconditions     []string                `json:"preconditions"`
	Postconditions    []string                `json:"postconditions"`
	Completeness      capability.Completeness `json:"completeness"`
}

// Options controls change plan generation.
type Options struct {
	MinConfidence  model.Confidence
	HubDegreeLimit int
}

// DefaultOptions returns standard change planning options.
// maxCallerProbes is how many direct callers are probed for covering tests.
const maxCallerProbes = 24

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

	tgt, err := capability.ResolveTarget(ctx, p.deps, runID, targetSymbol)
	if err != nil {
		return nil, err
	}
	return p.PlanTarget(ctx, runID, tgt, opts)
}

// PlanTarget is Plan for a caller that has already chosen the definition. See
// impact.AnalyzeTarget for why a name is not always enough to choose one.
func (p *Planner) PlanTarget(ctx context.Context, runID int64, tgt capability.Target, opts Options) (*Plan, error) {
	// 1. Run impact analysis to find direct callers, indirect callers, and risk score.
	imp, err := impact.AnalyzeTarget(ctx, p.deps, runID, tgt, impact.Options{
		MinConfidence:  opts.MinConfidence,
		HubDegreeLimit: opts.HubDegreeLimit,
		CheckValueUse:  true,
	})
	if err != nil {
		return nil, err
	}

	// 2. Discover covering tests for the target.
	//
	// By the resolved target, not by its qualified name. Overloads share one
	// qualified name, so re-resolving by it found the first overload's tests
	// for whichever overload was being planned.
	var testCmds []string
	testNames := make(map[string]bool)
	var testsUndetermined string

	testHit, err := tests.FindTestsForTarget(ctx, p.deps, runID, imp.Target, tests.Options{})
	if err == nil {
		testsUndetermined = testHit.Undetermined
		for _, th := range testHit.Tests {
			if !testNames[th.FilePath] {
				testNames[th.FilePath] = true
				testCmds = append(testCmds, th.FilePath)
			}
		}
	}

	// Also check direct callers for covering tests. Each is a bounded probe
	// and only the first maxCallerProbes callers are asked: a hub has a
	// thousand direct callers, and a full discovery from each made this plan
	// take 4.4 seconds on a 250,000-symbol index for tests the target's own
	// discovery above had mostly already found.
	for i, caller := range imp.Direct {
		if i >= maxCallerProbes {
			break
		}
		cTestHit, err := tests.FindTestsWith(ctx, p.deps, runID, caller.FQN, tests.ProbeOptions())
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
		Target:    imp.Target,
		RiskScore: imp.Risk.Score,
		RiskLevel: imp.Risk.Level,

		RiskUndetermined:  imp.Risk.Undetermined,
		RiskPartial:       imp.Risk.Partial,
		ValueUse:          imp.ValueUse,
		TestsUndetermined: testsUndetermined,
		TotalCallers:      imp.TotalReached(),
		DirectCallers:     len(imp.Direct),
		AffectedTests:     testCmds,
		Completeness:      imp.Completeness,
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
