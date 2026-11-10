package cli

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/verify"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// newVerifyCmd checks a completed change against the contract it was given.
//
// This is the half of the loop that did not exist. `cortex compile` states
// what a task is; nothing then asked whether the change that followed honoured
// it. `doctor --verify-index` checks that the graph is internally consistent,
// which is a different question entirely.
func newVerifyCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check a completed change against the task contract it was given",
		Long: `Reads a contract produced by 'cortex compile --json' and a list of changed
files, and answers four questions:

  1. Did the change stay inside the contract's declared scope?
  2. Did it violate a declared architecture rule?
  3. Did it miss a caller the blast radius flagged?
  4. Do the tests the contract named still cover the changed symbols?

Every finding carries its evidence and says whether it is a FACT or a LEAD. A
violation on a compiler-proven edge is a fact; one on a heuristic edge is a
lead, because the edge itself may be wrong. Presenting the two identically makes
the report either alarmist or ignorable, and both end with it switched off.

  cortex compile "Add idempotency to payment callbacks" --json > contract.json
  cortex verify --contract contract.json --changed $(git diff --name-only | paste -sd, -)

Exit codes are the point of the command:

  0  pass       every question asked, nothing found
  0  advisory   findings exist, all of them leads
  1  fail       at least one fact
  2  unverified there was nothing to check against

'unverified' is deliberately not 'pass'. "We looked and found nothing" and
"there was nothing to look at" must not render the same, or the gate stops
gating on the day contract generation breaks and nobody notices.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVerify(cmd, g)
		},
	}

	cmd.Flags().String("contract", "", "path to a contract from 'cortex compile --json' (required)")
	cmd.Flags().String("changed", "", "comma-separated changed file paths")
	cmd.Flags().String("changed-from", "", "file holding one changed path per line ('-' for stdin)")
	cmd.Flags().String("min-confidence", "", "raise the contract's confidence floor (exact|high|medium|low)")
	cmd.Flags().Bool("fail-on-leads", false, "exit non-zero when only leads were found")
	return cmd
}

func runVerify(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()

	contractPath, _ := cmd.Flags().GetString("contract")
	if contractPath == "" {
		return errors.New("--contract is required: verify checks a change against a contract, and cannot invent one")
	}
	changed, err := changedPaths(cmd)
	if err != nil {
		return err
	}

	var contract compile.Contract
	raw, err := os.ReadFile(contractPath) //nolint:gosec // operator-supplied path
	if err != nil {
		return fmt.Errorf("verify: read contract %s: %w", contractPath, err)
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		return fmt.Errorf("verify: parse contract %s (expected the output of 'cortex compile --json'): %w",
			contractPath, err)
	}

	s, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer s.close()

	opts := []verify.Option{
		verify.WithArchitecture(archSource{reg: s.reg, model: g.Config().Architecture}, s.runID),
	}
	if raw, _ := cmd.Flags().GetString("min-confidence"); raw != "" {
		c := model.Confidence(raw)
		if !c.Valid() {
			return fmt.Errorf("--min-confidence %q is not one of exact, high, medium, low", raw)
		}
		opts = append(opts, verify.WithMinConfidence(c))
	}

	rep, err := verify.New(opts...).Verify(ctx, contract, changed)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
			return err
		}
	} else {
		renderVerify(cmd, rep)
	}

	code := rep.Disposition.ExitCode()
	if failLeads, _ := cmd.Flags().GetBool("fail-on-leads"); failLeads && rep.Disposition == verify.DispositionAdvisory {
		code = 1
	}
	if code != 0 {
		// Returned, not os.Exit'd, so the codes are testable — see ExitCodeError.
		// Silent because the report is already on stdout: the code is the verdict,
		// and an "error:" line under it would read as a malfunction instead.
		return &ExitCodeError{Code: code, Silent: true,
			Err: fmt.Errorf("verify: %s", rep.Disposition)}
	}
	return nil
}

// changedPaths reads the change set from whichever flag supplied it.
//
// Deliberately no git invocation. This package shells out to nothing, and a
// verify that ran its own `git diff` would silently disagree with the caller
// about staged, unstaged and untracked files.
func changedPaths(cmd *cobra.Command) ([]string, error) {
	inline, _ := cmd.Flags().GetString("changed")
	from, _ := cmd.Flags().GetString("changed-from")

	switch {
	case inline != "" && from != "":
		return nil, errors.New("pass --changed or --changed-from, not both")
	case inline != "":
		return splitNonEmpty(inline, ","), nil
	case from == "":
		return nil, errors.New("--changed or --changed-from is required: verify needs to know what changed")
	}

	var raw []byte
	var err error
	if from == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(from) //nolint:gosec // operator-supplied path
	}
	if err != nil {
		return nil, fmt.Errorf("verify: read changed paths: %w", err)
	}
	return splitNonEmpty(string(raw), "\n"), nil
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func renderVerify(cmd *cobra.Command, rep verify.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "VERIFY: %s\n\n", strings.ToUpper(string(rep.Disposition)))

	if len(rep.Findings) == 0 {
		fmt.Fprintf(out, "No findings.\n")
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(out, "[%s] %s\n", strings.ToUpper(string(f.Standing)), f.Summary)
		if f.Evidence != "" {
			fmt.Fprintf(out, "  evidence: %s\n", f.Evidence)
		}
		if f.Detail != "" {
			fmt.Fprintf(out, "  %s\n", f.Detail)
		}
	}

	// Exclusions and unknowns are printed, always. A gate that silently filters
	// is the failure mode both compile and verify are built to avoid.
	if len(rep.Excluded) > 0 {
		fmt.Fprintf(out, "\nNOT CHECKED\n")
		for _, e := range rep.Excluded {
			fmt.Fprintf(out, "  %s\n", e.Reason)
		}
	}
	if len(rep.Unknowns) > 0 {
		fmt.Fprintf(out, "\nUNKNOWN\n")
		for _, u := range rep.Unknowns {
			fmt.Fprintf(out, "  %s\n", u)
		}
	}
}

// archSource adapts capability/architecture to verify's port.
type archSource struct {
	reg   architecture.Registry
	model arch.Model
}

func (a archSource) Check(ctx stdctx.Context, runID int64, min model.Confidence) (architecture.Report, error) {
	return architecture.Check(ctx, a.reg, runID, a.model, min)
}

// archView and impactView adapt the same capabilities to compile's ports, so a
// contract is compiled knowing the declared rules and the blast radius rather
// than only the context pack.
type archView struct {
	resolver *arch.Resolver
	model    arch.Model
}

func (v archView) ComponentOf(path string) string { return v.resolver.ComponentOf(path) }
func (v archView) Rules() []arch.Rule             { return v.model.Rules }

type impactView struct {
	deps  capability.Deps
	runID int64
}

// Reach maps a capability report into the terms a contract can state.
//
// A target the index does not hold is NOT an error: it is an unresolved Reach,
// which the contract reports as a named gap. Treating it as a failure would make
// one unindexed symbol prevent the whole contract from compiling.
func (v impactView) Reach(ctx stdctx.Context, target string) (compile.Reach, error) {
	rep, err := impact.Analyze(ctx, v.deps, v.runID, target)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		if errors.Is(err, capability.ErrTargetNotFound) || errors.As(err, &amb) {
			return compile.Reach{Resolved: false}, nil
		}
		return compile.Reach{}, err
	}

	r := compile.Reach{
		Resolved:  true,
		Target:    rep.Target.Name,
		Floor:     string(rep.ConfidenceFloor),
		Hidden:    rep.LowConfidenceHidden,
		Truncated: rep.Truncated,
	}
	// Dispatch is included, and its omission was a real gap: a symbol reached
	// through interface or virtual dispatch is not called by name anywhere, so it
	// is exactly the caller a human reviewer misses — which is the case a blast
	// radius exists to catch. Leaving it out made the contract quietly narrower
	// than the analysis that produced it.
	//
	// Ordered strongest claim first so the dedup below keeps the stronger one:
	// a symbol proven to call the target directly should not be described as
	// merely reachable through dispatch.
	seen := make(map[string]struct{})
	add := func(im impact.Impacted, evidence string) {
		key := im.FilePath + "#" + im.Name
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		r.Callers = append(r.Callers, compile.Reached{
			Symbol:     im.Name,
			Path:       im.FilePath,
			Kind:       im.Kind,
			Depth:      im.Depth,
			Confidence: string(im.Confidence),
			// Marked, not dropped. A test that calls the target is reached the
			// same way a caller is, but it means the opposite thing: leaving it
			// unmarked makes `verify` report every covering test as a caller the
			// change forgot to update, which is noise on exactly the changes that
			// were best covered.
			IsTest:   capability.IsTestSymbol(im.Kind, im.Name, im.FilePath),
			Evidence: evidence,
		})
	}

	for _, group := range [][]impact.Impacted{rep.Direct, rep.Indirect} {
		for _, im := range group {
			add(im, firstEvidence(im))
		}
	}
	for _, d := range rep.Dispatch {
		// Its own sentence, not a file:line, because a dispatch reach HAS no call
		// site — that is the whole claim. Handing back an empty evidence string
		// would make it read as an ordinary caller the analysis merely failed to
		// locate, when in fact no call names it and that is precisely why a human
		// reviewer misses it.
		add(d.Impacted, d.Detail)
	}

	tr, err := tests.FindTests(ctx, v.deps, v.runID, target)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		if errors.Is(err, capability.ErrTargetNotFound) || errors.As(err, &amb) {
			return r, nil
		}
		return compile.Reach{}, err
	}
	for _, t := range tr.Tests {
		r.Tests = append(r.Tests, compile.TestRef{
			Name:       t.Name,
			Path:       t.FilePath,
			Signal:     string(t.Strongest),
			Confidence: string(t.Confidence),
		})
	}
	return r, nil
}

func firstEvidence(im impact.Impacted) string {
	if len(im.Evidence) == 0 {
		return ""
	}
	e := im.Evidence[0]
	return fmt.Sprintf("%s:%d", e.FilePath, e.Range.Start.Line)
}

// compilePorts builds the ports for a contract compiled against this index.
func compilePorts(s *session, g *Global) compile.Ports {
	m := g.Config().Architecture
	return compile.Ports{
		Architecture: archView{resolver: arch.NewResolver(m), model: m},
		Impact: impactView{
			deps:  capability.Deps{Registry: s.reg, Graph: graph.NewForRun(s.db, s.runID)},
			runID: s.runID,
		},
	}
}
