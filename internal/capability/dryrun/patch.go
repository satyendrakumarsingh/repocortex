package dryrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// PatchChangeKind indicates how a symbol was altered by the speculative patch.
type PatchChangeKind string

const (
	ChangeSymbolAdded     PatchChangeKind = "added"
	ChangeSymbolDeleted   PatchChangeKind = "deleted"
	ChangeSymbolModified  PatchChangeKind = "signature_changed"
	ChangeSymbolUnchanged PatchChangeKind = "unchanged"
)

// SymbolDiff describes the delta on one symbol definition.
type SymbolDiff struct {
	Name         string          `json:"name"`
	FQN          string          `json:"fqn"`
	Kind         string          `json:"kind"`
	Change       PatchChangeKind `json:"change"`
	OldSignature string          `json:"old_signature,omitempty"`
	NewSignature string          `json:"new_signature,omitempty"`
	Line         int             `json:"line"`
}

// CallerImpact describes an upstream caller that reaches a modified or deleted symbol.
type CallerImpact struct {
	CallerName string `json:"caller_name"`
	CallerPath string `json:"caller_path"`
	CallerLine int    `json:"caller_line"`
	TargetName string `json:"target_name"`
	TargetFQN  string `json:"target_fqn"`
	Reason     string `json:"reason"`
}

// Report holds the in-memory speculative dry-run analysis.
type Report struct {
	Path          string         `json:"path"`
	Language      string         `json:"language"`
	Safe          bool           `json:"safe"`
	RiskScore     int            `json:"risk_score"`
	ChangedCount  int            `json:"changed_symbols"`
	BrokenCount   int            `json:"broken_caller_count"`
	SymbolDiffs   []SymbolDiff   `json:"symbol_diffs"`
	BrokenCallers []CallerImpact `json:"broken_callers,omitempty"`
	Summary       string         `json:"summary"`
}

// AnalyzePatch performs an in-memory speculative dry-run of proposed file content.
// It parses the content in memory, compares extracted definitions against the
// indexed graph, and identifies all upstream callers impacted by signature changes
// or deletions in milliseconds, without writing to disk.
func AnalyzePatch(
	ctx context.Context,
	reg *registry.Registry,
	g *graph.Graph,
	runID int64,
	path string,
	newContent string,
) (*Report, error) {
	normPath := strings.TrimPrefix(path, "./")
	parser := parse.NewRegistry().ForFile(normPath)
	if parser == nil {
		return nil, fmt.Errorf("no parser available for file %s", normPath)
	}

	parseRes, err := parser.Parse(ctx, normPath, []byte(newContent))
	if err != nil {
		return nil, fmt.Errorf("in-memory parse failed: %w", err)
	}

	newSymsByFQN := make(map[string]model.Symbol, len(parseRes.Symbols))
	newSymsByName := make(map[string]model.Symbol, len(parseRes.Symbols))
	newSignatures := make(map[string]string)
	contentLines := strings.Split(newContent, "\n")

	for _, s := range parseRes.Symbols {
		sig := s.Signature
		if sig == "" {
			sig = extractDeclarationSignature(contentLines, s.Range)
		}
		newSignatures[s.Name] = sig
		if s.FQN != "" {
			newSymsByFQN[s.FQN] = s
			newSignatures[s.FQN] = sig
		}
		newSymsByName[s.Name] = s
	}

	files, err := reg.Files(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read files: %w", err)
	}
	fileMap := make(map[int64]string, len(files))
	var existingFileID int64 = -1
	for _, f := range files {
		fileMap[f.ID] = f.Path
		if f.Path == normPath {
			existingFileID = f.ID
		}
	}

	allSyms, err := reg.Symbols(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read symbols: %w", err)
	}
	symByID := make(map[int64]model.Symbol, len(allSyms))
	var existingSyms []model.Symbol
	for _, s := range allSyms {
		symByID[s.ID] = s
		if s.FileID == existingFileID {
			existingSyms = append(existingSyms, s)
		}
	}

	var oldLines []string
	if existingFileID >= 0 {
		if old, err := reg.FileContent(ctx, runID, normPath); err == nil {
			oldLines = strings.Split(old, "\n")
		}
	}

	var diffs []SymbolDiff
	var impactedCallers []CallerImpact
	visitedCallers := make(map[string]bool)

	// Check existing symbols against new AST
	for _, oldSym := range existingSyms {
		newSym, exists := newSymsByFQN[oldSym.FQN]
		if !exists {
			newSym, exists = newSymsByName[oldSym.Name]
		}

		if !exists {
			diffs = append(diffs, SymbolDiff{
				Name:         oldSym.Name,
				FQN:          oldSym.FQN,
				Kind:         string(oldSym.Kind),
				Change:       ChangeSymbolDeleted,
				OldSignature: oldSym.Signature,
				Line:         oldSym.Range.Start.Line,
			})
			// Query callers of deleted symbol
			callers, _ := g.Neighbours(ctx, oldSym.ID, graph.Incoming, []model.EdgeKind{model.EdgeCalls}, model.ConfidenceMedium)
			for _, n := range callers {
				cSym, ok := symByID[n.SymbolID]
				if ok {
					cPath := fileMap[cSym.FileID]
					key := fmt.Sprintf("%s:%d->%s", cPath, cSym.Range.Start.Line, oldSym.FQN)
					if !visitedCallers[key] {
						visitedCallers[key] = true
						impactedCallers = append(impactedCallers, CallerImpact{
							CallerName: cSym.Name,
							CallerPath: cPath,
							CallerLine: cSym.Range.Start.Line,
							TargetName: oldSym.Name,
							TargetFQN:  oldSym.FQN,
							Reason:     "symbol deleted in proposed edit",
						})
					}
				}
			}
		} else {
			newSig := newSignatures[newSym.Name]
			if newSig == "" && newSym.FQN != "" {
				newSig = newSignatures[newSym.FQN]
			}
			oldSig := oldSym.Signature
			if oldSig == "" {
				// Most parsers store no signature. Read the declaration
				// the same way the new content's is read, from the
				// indexed text, so the two lines can be compared.
				oldSig = extractDeclarationSignature(oldLines, oldSym.Range)
			}
			modified, reason := isSignatureModified(oldSym, newSym, oldSig, newSig)
			if modified {
				diffs = append(diffs, SymbolDiff{
					Name:         oldSym.Name,
					FQN:          oldSym.FQN,
					Kind:         string(oldSym.Kind),
					Change:       ChangeSymbolModified,
					OldSignature: oldSym.Signature,
					NewSignature: newSig,
					Line:         newSym.Range.Start.Line,
				})
				// Query callers of modified signature
				callers, _ := g.Neighbours(ctx, oldSym.ID, graph.Incoming, []model.EdgeKind{model.EdgeCalls}, model.ConfidenceMedium)
				for _, n := range callers {
					cSym, ok := symByID[n.SymbolID]
					if ok {
						cPath := fileMap[cSym.FileID]
						key := fmt.Sprintf("%s:%d->%s", cPath, cSym.Range.Start.Line, oldSym.FQN)
						if !visitedCallers[key] {
							visitedCallers[key] = true
							impactedCallers = append(impactedCallers, CallerImpact{
								CallerName: cSym.Name,
								CallerPath: cPath,
								CallerLine: cSym.Range.Start.Line,
								TargetName: oldSym.Name,
								TargetFQN:  oldSym.FQN,
								Reason:     reason,
							})
						}
					}
				}
			}
		}
	}

	// Check for newly added symbols
	for _, newSym := range parseRes.Symbols {
		found := false
		for _, oldSym := range existingSyms {
			if oldSym.FQN == newSym.FQN || oldSym.Name == newSym.Name {
				found = true
				break
			}
		}
		if !found {
			diffs = append(diffs, SymbolDiff{
				Name:         newSym.Name,
				FQN:          newSym.FQN,
				Kind:         string(newSym.Kind),
				Change:       ChangeSymbolAdded,
				NewSignature: newSym.Signature,
				Line:         newSym.Range.Start.Line,
			})
		}
	}

	riskScore := len(impactedCallers)*10 + len(diffs)*2
	safe := len(impactedCallers) == 0

	var b strings.Builder
	fmt.Fprintf(&b, "Speculative Dry-Run for %s (%s):\n", normPath, parser.Language())
	if safe {
		fmt.Fprintf(&b, "✓ SAFE: 0 broken callers. %d symbol changes detected.\n", len(diffs))
	} else {
		fmt.Fprintf(&b, "⚠ BREAKING CHANGE: %d broken caller(s) detected across graph!\n", len(impactedCallers))
		b.WriteString("Broken Callers:\n")
		for _, c := range impactedCallers {
			fmt.Fprintf(&b, "  - %s (%s:%d) -> %s: %s\n", c.CallerName, c.CallerPath, c.CallerLine, c.TargetName, c.Reason)
		}
	}

	return &Report{
		Path:          normPath,
		Language:      parser.Language(),
		Safe:          safe,
		RiskScore:     riskScore,
		ChangedCount:  len(diffs),
		BrokenCount:   len(impactedCallers),
		SymbolDiffs:   diffs,
		BrokenCallers: impactedCallers,
		Summary:       b.String(),
	}, nil
}

func extractDeclarationSignature(contentLines []string, rng model.Range) string {
	lineIdx := rng.Start.Line - 1
	if lineIdx < 0 || lineIdx >= len(contentLines) {
		return ""
	}
	line := strings.TrimSpace(contentLines[lineIdx])
	if idx := strings.Index(line, "{"); idx != -1 {
		line = strings.TrimSpace(line[:idx])
	}
	return line
}

func isSignatureModified(oldSym, newSym model.Symbol, oldSig, newSig string) (bool, string) {
	if oldSym.ParamCount.Known() && newSym.ParamCount.Known() && oldSym.ParamCount != newSym.ParamCount {
		return true, fmt.Sprintf("parameter count changed from %d to %d", oldSym.ParamCount.Count(), newSym.ParamCount.Count())
	}
	if oldSig != "" && newSig != "" && strings.Join(strings.Fields(oldSig), " ") != strings.Join(strings.Fields(newSig), " ") {
		return true, fmt.Sprintf("signature changed from %q to %q", oldSig, newSig)
	}
	return false, ""
}
