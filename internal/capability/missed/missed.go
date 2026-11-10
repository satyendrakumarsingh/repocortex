package missed

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// BlindspotKind classifies the category of hidden or unresolved debt.
type BlindspotKind string

const (
	BlindspotUnresolvedCall    BlindspotKind = "unresolved_call"
	BlindspotUntestedCritical  BlindspotKind = "untested_critical"
	BlindspotUnmappedEndpoint  BlindspotKind = "unmapped_endpoint"
	BlindspotOrphanTable       BlindspotKind = "orphan_table"
	BlindspotMissingConfigDecl BlindspotKind = "missing_config"
	BlindspotUnparsedFile      BlindspotKind = "unparsed_file"
)

// Blindspot represents a single actionable gap in code intelligence or test safety.
type Blindspot struct {
	Kind        BlindspotKind `json:"kind"`
	Severity    string        `json:"severity"` // "high", "medium", "low"
	Target      string        `json:"target"`
	Path        string        `json:"path,omitempty"`
	Line        int           `json:"line,omitempty"`
	Description string        `json:"description"`
	Remediation string        `json:"remediation"`
}

// Options bounds the blindspot detection scan.
type Options struct {
	MaxItemsPerKind int
}

// DefaultOptions returns standard blindspot detection options.
func DefaultOptions() Options {
	return Options{
		MaxItemsPerKind: 50,
	}
}

// Report aggregates all detected blindspots across the repository.
type Report struct {
	RunID           int64                   `json:"run_id"`
	TotalBlindspots int                     `json:"total_blindspots"`
	ByKind          map[BlindspotKind]int   `json:"by_kind"`
	Blindspots      []Blindspot             `json:"blindspots"`
	Completeness    capability.Completeness `json:"completeness"`
}

// Detector discovers code intelligence gaps and untracked dependencies.
type Detector struct {
	reg  *registry.Registry
	caps capability.Deps
}

// NewDetector constructs a blindspot detector.
func NewDetector(reg *registry.Registry, caps capability.Deps) *Detector {
	return &Detector{
		reg:  reg,
		caps: caps,
	}
}

// Detect gathers blindspots for the specified index run.
func (d *Detector) Detect(ctx context.Context, runID int64, opts Options) (*Report, error) {
	if opts.MaxItemsPerKind <= 0 {
		opts.MaxItemsPerKind = 50
	}

	stats, _ := d.reg.ResolutionStats(ctx, runID)
	counts, _ := d.reg.Counts(ctx, runID)

	rep := &Report{
		RunID:        runID,
		ByKind:       make(map[BlindspotKind]int),
		Blindspots:   []Blindspot{},
		Completeness: capability.NewCompleteness(stats, int(counts.Symbols), false, ""),
	}

	add := func(b Blindspot) {
		rep.Blindspots = append(rep.Blindspots, b)
		rep.ByKind[b.Kind]++
	}

	files, _ := d.reg.Files(ctx, runID)
	filePaths := make(map[int64]string, len(files))
	for _, f := range files {
		filePaths[f.ID] = f.Path
	}

	// 1. Unresolved Call Sites
	unresolved, err := d.reg.UnresolvedReferences(ctx, runID, opts.MaxItemsPerKind)
	if err == nil {
		for _, u := range unresolved {
			desc := fmt.Sprintf("Call to %q could not be bound to any definition", u.Name)
			if u.Receiver != "" {
				desc = fmt.Sprintf("Call to %s.%s could not be bound", u.Receiver, u.Name)
			}
			add(Blindspot{
				Kind:        BlindspotUnresolvedCall,
				Severity:    "medium",
				Target:      u.Name,
				Path:        u.Path,
				Line:        u.Line,
				Description: desc,
				Remediation: "Verify import paths or install SCIP indexer for full external library binding.",
			})
		}
	}

	// 2. Unmapped Endpoints
	endpoints, err := d.reg.EndpointReport(ctx, runID)
	if err == nil {
		count := 0
		for _, ep := range endpoints {
			if count >= opts.MaxItemsPerKind {
				break
			}
			if ep.Handler == "" {
				add(Blindspot{
					Kind:        BlindspotUnmappedEndpoint,
					Severity:    "high",
					Target:      ep.Signature,
					Path:        ep.FilePath,
					Line:        ep.Line,
					Description: fmt.Sprintf("HTTP endpoint %q has no resolved handler symbol", ep.Signature),
					Remediation: "Ensure handler function is defined and exported in the enclosing package.",
				})
				count++
			}
		}
	}

	// 3. Orphan Database Tables
	tables, err := d.reg.TableUsageReport(ctx, runID)
	if err == nil {
		count := 0
		for _, t := range tables {
			if count >= opts.MaxItemsPerKind {
				break
			}
			if t.Readers == 0 && t.Writers == 0 {
				add(Blindspot{
					Kind:        BlindspotOrphanTable,
					Severity:    "low",
					Target:      t.Table,
					Description: fmt.Sprintf("Database table %q is declared in schema but never read or written in code", t.Table),
					Remediation: "Check if table is deprecated or accessed dynamically via unindexed queries.",
				})
				count++
			}
		}
	}

	// 4. Missing Config Declarations
	configs, err := d.reg.ConfigReport(ctx, runID)
	if err == nil {
		count := 0
		for _, c := range configs {
			if count >= opts.MaxItemsPerKind {
				break
			}
			if c.Readers > 0 && !c.Declared {
				add(Blindspot{
					Kind:        BlindspotMissingConfigDecl,
					Severity:    "high",
					Target:      c.Name,
					Path:        c.FilePath,
					Line:        c.Line,
					Description: fmt.Sprintf("Config key %q is read in code %d time(s) but not declared in any config file", c.Name, c.Readers),
					Remediation: "Add default value to .env.example or application configuration schema to prevent runtime start-up failure.",
				})
				count++
			}
		}
	}

	// 5. Unparsed / Errored Files
	if len(files) > 0 {
		count := 0
		for _, f := range files {
			if count >= opts.MaxItemsPerKind {
				break
			}
			if f.ParseStatus != "ok" {
				add(Blindspot{
					Kind:        BlindspotUnparsedFile,
					Severity:    "high",
					Target:      f.Path,
					Path:        f.Path,
					Description: fmt.Sprintf("File failed complete parsing with status %q", f.ParseStatus),
					Remediation: "Check file syntax or language grammar parser configuration.",
				})
				count++
			}
		}
	}

	// 6. Untested Critical Symbols
	symbols, err := d.reg.Symbols(ctx, runID)
	if err == nil && d.caps.Graph != nil {
		type candidate struct {
			sym      registry.SymbolHit
			path     string
			inDegree int
		}
		var candidates []candidate
		count := 0
		for _, s := range symbols {
			if !s.Exported || (s.Kind != "function" && s.Kind != "method") {
				continue
			}
			// Estimate in-degree using edge lookup
			edges, _ := d.reg.IncomingEdgeCount(ctx, runID, s.ID, registry.EdgeFilter{}, nil)
			if edges >= 2 {
				candidates = append(candidates, candidate{
					sym: registry.SymbolHit{
						ID:       s.ID,
						Name:     s.Name,
						FQN:      s.FQN,
						FilePath: filePaths[s.FileID],
						Kind:     string(s.Kind),
					},
					path:     filePaths[s.FileID],
					inDegree: edges,
				})
			}
		}

		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].inDegree > candidates[j].inDegree
		})

		for _, cand := range candidates {
			if count >= opts.MaxItemsPerKind {
				break
			}
			hit, err := tests.FindTestsWith(ctx, d.caps, runID, cand.sym.FQN, tests.Options{})
			if err == nil && len(hit.Tests) == 0 {
				add(Blindspot{
					Kind:        BlindspotUntestedCritical,
					Severity:    "high",
					Target:      cand.sym.FQN,
					Path:        cand.path,
					Description: fmt.Sprintf("Critical symbol %q has %d incoming callers but 0 discovered tests", cand.sym.FQN, cand.inDegree),
					Remediation: "Add dedicated unit or integration test covering this entrypoint before refactoring.",
				})
				count++
			}
		}
	}

	rep.TotalBlindspots = len(rep.Blindspots)
	return rep, nil
}
