package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/spf13/cobra"
)

func newMissedCmd(g *Global) *cobra.Command {
	var (
		kindFilter string
		limit      int
		failOn     string
	)

	cmd := &cobra.Command{
		Use:   "missed",
		Short: "Detect code intelligence blindspots and hidden risks",
		Long: `Audits the index for blindspots:
  - Unresolved external and internal calls
  - Untested critical entrypoints with high fan-in
  - Unmapped HTTP endpoints lacking handler definitions
  - Orphan database tables declared but unused, or used but undefined
  - Missing configuration declarations read in code (crash risk)
  - Partially parsed or errored source files

Examples:
  cortex missed
  cortex missed --kind unresolved_call
  cortex missed --json
  cortex missed --fail-on high    # exit 1 in CI when a high-severity blindspot exists`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Validated before the index is opened: a mistyped --kind used to
			// filter everything out and print "no blindspots detected", which
			// reads as a clean bill of health.
			if kindFilter != "" && !validBlindspotKind(kindFilter) {
				return fmt.Errorf("--kind %q: want one of %s", kindFilter, strings.Join(blindspotKinds(), ", "))
			}
			if failOn != "" && blindspotSeverityRank(failOn) == 0 {
				return fmt.Errorf("--fail-on %q: want high, medium or low", failOn)
			}
			out := cmd.OutOrStdout()
			db, err := openReadOnly(ctx, g.DBPath())
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			reg := registry.New(db)
			currentRun, err := reg.CurrentRun(ctx)
			if err != nil {
				return fmt.Errorf("read current index run: %w", err)
			}
			if currentRun == nil {
				return fmt.Errorf("index is empty: run 'cortex index' first")
			}

			gr := graph.New(db)
			caps := capability.NewDeps(reg, gr)
			detector := missed.NewDetector(reg, caps)

			rep, err := detector.Detect(ctx, currentRun.ID, missed.Options{
				MaxItemsPerKind: limit,
			})
			if err != nil {
				return fmt.Errorf("blindspot detection: %w", err)
			}

			// Filter by kind if specified
			if kindFilter != "" {
				filtered := make([]missed.Blindspot, 0, len(rep.Blindspots))
				rep.ByKind = make(map[missed.BlindspotKind]int)
				for _, b := range rep.Blindspots {
					if string(b.Kind) == kindFilter {
						filtered = append(filtered, b)
						rep.ByKind[b.Kind]++
					}
				}
				rep.Blindspots = filtered
				rep.TotalBlindspots = len(filtered)
			}

			if g.JSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
				return blindspotGate(rep.Blindspots, failOn)
			}

			// Human readable output
			fmt.Fprintf(out, "RepoCortex Blindspot Report (Run %d)\n", rep.RunID)
			fmt.Fprintf(out, "Total Blindspots: %d\n\n", rep.TotalBlindspots)

			if rep.TotalBlindspots == 0 {
				fmt.Fprintln(out, "No code intelligence blindspots detected. Graph and configurations are complete.")
				return nil
			}

			kinds := make([]string, 0, len(rep.ByKind))
			for kind := range rep.ByKind {
				kinds = append(kinds, string(kind))
			}
			sort.Strings(kinds)
			for _, kind := range kinds {
				fmt.Fprintf(out, "• %s: %d\n", kind, rep.ByKind[missed.BlindspotKind(kind)])
			}
			fmt.Fprintln(out)

			fmt.Fprintln(out, "Top Actionable Findings:")
			for i, b := range rep.Blindspots {
				if i >= 20 {
					fmt.Fprintf(out, "... and %d more items (use --json or --limit to inspect all)\n", len(rep.Blindspots)-20)
					break
				}
				loc := ""
				if b.Path != "" {
					loc = fmt.Sprintf(" (%s:%d)", b.Path, b.Line)
				}
				fmt.Fprintf(out, "[%s] %s%s: %s\n  -> Remediation: %s\n",
					b.Severity, b.Target, loc, b.Description, b.Remediation)
			}

			return blindspotGate(rep.Blindspots, failOn)
		},
	}

	cmd.Flags().StringVar(&kindFilter, "kind", "", "Filter by blindspot kind (unresolved_call, untested_critical, unmapped_endpoint, orphan_table, missing_config, unparsed_file)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum blindspots to report per category")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit non-zero when a blindspot of this severity or worse exists (high|medium|low)")
	return cmd
}

// blindspotKinds lists the kinds --kind accepts, in a stable order.
func blindspotKinds() []string {
	return []string{
		string(missed.BlindspotUnresolvedCall), string(missed.BlindspotUntestedCritical),
		string(missed.BlindspotUnmappedEndpoint), string(missed.BlindspotOrphanTable),
		string(missed.BlindspotMissingConfigDecl), string(missed.BlindspotUnparsedFile),
	}
}

func validBlindspotKind(k string) bool {
	for _, v := range blindspotKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// blindspotSeverityRank orders severities; 0 means unknown.
func blindspotSeverityRank(sev string) int {
	switch sev {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// blindspotGate turns --fail-on into an exit status. With failOn empty it
// never fails, which is the command's behaviour before the flag existed.
func blindspotGate(found []missed.Blindspot, failOn string) error {
	if failOn == "" {
		return nil
	}
	threshold := blindspotSeverityRank(failOn)
	n := 0
	for _, b := range found {
		// untested_critical is a coverage heuristic: it counts a symbol as untested
		// when no test binds to it by name, and misses tests that reach it through
		// an interface or a shared method name. It is reported, but a merge should
		// not be blocked by a measurement the tool itself calls a lower bound.
		if b.Kind == missed.BlindspotUntestedCritical {
			continue
		}
		if blindspotSeverityRank(b.Severity) >= threshold {
			n++
		}
	}
	if n > 0 {
		return fmt.Errorf("%d blindspot(s) at severity %s or worse (--fail-on %s)", n, failOn, failOn)
	}
	return nil
}
