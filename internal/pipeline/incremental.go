package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ChangeKind classifies what happened to a file since the last index.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeSame     ChangeKind = "unchanged"
)

// FileChange is one difference between the committed index and the working tree.
type FileChange struct {
	Path string
	Kind ChangeKind
}

// IncrementalStats reports what an incremental pass did.
type IncrementalStats struct {
	Examined  int64
	Added     int64
	Modified  int64
	Deleted   int64
	Unchanged int64
	Symbols   int64
}

// Diff compares the working tree against a committed index run.
//
// Comparison is by content hash, not mtime. mtime is unreliable across
// checkouts, clones and restores — `git checkout` rewrites timestamps on files
// whose content is unchanged, which would make an mtime-based diff re-index a
// whole repository after a branch switch. Hashing costs a read; getting this
// wrong costs the entire point of incremental indexing.
//
// A content hash answers "did the SOURCE change". It cannot answer "would this
// build extract something different from the same source", and a version-
// crossing re-index faces both questions — so runningAs names the build doing
// the work, and diffAgainst below explains what happens when it differs.
//
// runningAs is a parameter and not a package variable on purpose. It was a
// package variable first, which meant every entry point had to remember to set
// it; the incremental path did not, so it saw an empty string, concluded it
// could not prove a match, and re-parsed the entire repository on every run.
func Diff(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
	runningAs string,
) ([]FileChange, error) {
	run, err := reg.Run(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read run %d: %w", runID, err)
	}
	return diffAgainst(ctx, reg, runID, walker, run.ProducerVersion, runningAs)
}

// diffAgainst is Diff with the two producer identities supplied, so the
// version-crossing rule is testable without a second binary.
//
// THE RULE: if the committed run was written by a different build, every
// discovered file is dirty, regardless of its content hash.
//
// This was found empirically and it is not theoretical. A fix to the SQL
// extractor started recognising `SELECT token FROM card_tokens`, which the
// previous build had rejected as prose. Re-indexing with the new binary changed
// nothing: the file's content hash was identical, so incremental indexing
// skipped it and served the old build's analysis. The read edge stayed missing,
// and the compliance check that depended on it kept reporting "no endpoint
// reaches classified data" — a wrong answer, produced confidently, by a build
// that had already been fixed.
//
// Content hashing is exactly right for what it was designed for and simply does
// not cover this: it compares the INPUT while the thing that changed was the
// FUNCTION. An empty stamp is treated as a mismatch because a run that cannot
// say what wrote it cannot be shown to match.
//
// The cost is one full re-index the first time a user runs a new build, which is
// what they would have had to do by hand anyway once they noticed — and the
// point is that they would not have noticed.
func diffAgainst(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
	indexedBy, runningAs string,
) ([]FileChange, error) {
	sameProducer := indexedBy != "" && indexedBy == runningAs

	indexed, err := reg.Files(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read indexed files: %w", err)
	}
	prior := make(map[string]string, len(indexed))
	for _, f := range indexed {
		if !sameProducer {
			// Present, so it is not "added" and not "deleted" — but never equal,
			// so every file re-parses under the current build's rules.
			prior[f.Path] = ""
			continue
		}
		prior[f.Path] = f.ContentHash
	}

	current, err := walker.Walk(ctx)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	var changes []FileChange
	seen := make(map[string]struct{}, len(current))

	for _, d := range current {
		seen[d.Path] = struct{}{}
		oldHash, existed := prior[d.Path]
		switch {
		case !existed:
			changes = append(changes, FileChange{Path: d.Path, Kind: ChangeAdded})
		case oldHash != d.ContentHash:
			changes = append(changes, FileChange{Path: d.Path, Kind: ChangeModified})
		default:
			changes = append(changes, FileChange{Path: d.Path, Kind: ChangeSame})
		}
	}

	// Anything indexed but no longer discovered has been deleted, moved, or
	// newly excluded. All three mean the same thing to the registry: its
	// symbols and edges must go, or queries will return results for code that
	// is not there.
	for path := range prior {
		if _, still := seen[path]; !still {
			changes = append(changes, FileChange{Path: path, Kind: ChangeDeleted})
		}
	}

	// Deterministic ordering keeps incremental runs reproducible, which the
	// registry digest depends on.
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// IncrementalOptions configures an incremental pass.
type IncrementalOptions struct {
	Parsers parse.Registry
	// Resolve runs reference resolution for the changed files (and any caller
	// references un-resolved by their replacement) immediately after indexing,
	// restoring call edges across the graph.
	Resolve bool
}

// Incremental re-indexes only what changed, in place, on the committed run.
//
// Unlike a full index this does NOT create a new run and swap. Each file is
// replaced in its own transaction, so the index stays continuously queryable
// and the cost scales with the number of changed files rather than repository
// size — which is the entire promise of spec §12.
//
// The trade-off is deliberate: a full index is atomic all-or-nothing, while an
// incremental pass is atomic per file. Interrupting one leaves the index
// consistent but partially updated, which `cortex status` reports honestly
// rather than papering over.
func Incremental(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
	changes []FileChange,
	opts IncrementalOptions,
) (IncrementalStats, error) {
	var stats IncrementalStats

	parsers := opts.Parsers
	if parsers == nil {
		parsers = parse.NewRegistry()
	}

	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Examined++

		switch ch.Kind {
		case ChangeSame:
			stats.Unchanged++
			continue

		case ChangeDeleted:
			// Removes the file's symbols, its search rows, and every edge it
			// evidenced. Stale edges surviving a delete is precisely the
			// failure spec §44 requires us to prevent.
			if err := reg.DeleteFile(ctx, runID, ch.Path, registry.AllowCommittedRun()); err != nil {
				return stats, fmt.Errorf("delete %s: %w", ch.Path, err)
			}
			stats.Deleted++
			continue

		case ChangeAdded, ChangeModified:
			facts, err := reindexOne(ctx, walker, parsers, ch.Path)
			if err != nil {
				return stats, fmt.Errorf("reindex %s: %w", ch.Path, err)
			}
			// Analysis symbols (endpoints, tables, columns, config keys) are
			// written with the parser's, exactly as the full path does. Leaving
			// them out is what made a one-file edit erase them.
			symbols := append(append([]model.Symbol(nil), facts.Symbols...),
				facts.Analysis.Symbols...)
			// Deployment nodes, appended last exactly as the full path appends
			// them, so a re-indexed manifest keeps its units and its keys.
			symbols = append(symbols, infraSymbols(facts.Infra)...)
			// model.DefaultFQN, not a fourth copy of the rule. The analyzer
			// attributes statements by the FQN the sink WILL assign, so a
			// local spelling here silently detaches every access from the
			// definition that made it.
			resolveFQNs(facts.File.Path, symbols)
			// Same collapse the full path performs, for the same constraint:
			// without it, editing a file whose tags produce two declarations on
			// one line fails the UNIQUE constraint and aborts the update.
			symbols, _ = collapseDuplicateDeclarations(symbols)
			f := facts.File
			// Incremental updates mutate the live run by design; see
			// registry/inplace.go for the consistency trade this makes.
			replaceOpts := []registry.ReplaceOption{registry.AllowCommittedRun()}
			if len(facts.Content) > 0 && f.ParseStatus != string(parse.StatusSkipped) {
				replaceOpts = append(replaceOpts, registry.WithContent(string(facts.Content)))
			}
			if err := reg.ReplaceFile(ctx, runID, &f, symbols, nil, replaceOpts...); err != nil {
				return stats, fmt.Errorf("replace %s: %w", ch.Path, err)
			}
			// ReplaceFile has written real row ids back into symbols and f, so
			// the staged analysis rows can be attributed now.
			if err := writeIncrementalAnalysis(ctx, reg, runID, f.ID, facts, symbols); err != nil {
				return stats, fmt.Errorf("analysis for %s: %w", ch.Path, err)
			}
			// References and imports are extracted above and, before this,
			// were never written on the incremental path. A re-indexed file
			// was left with no references at all, so nothing in it could
			// resolve and its call edges never came back.
			if err := writeIncrementalReferences(ctx, reg, runID, f.ID, facts); err != nil {
				return stats, fmt.Errorf("references for %s: %w", ch.Path, err)
			}
			stats.Symbols += int64(len(symbols))
			if ch.Kind == ChangeAdded {
				stats.Added++
			} else {
				stats.Modified++
			}
		}
	}
	if opts.Resolve && (stats.Added > 0 || stats.Modified > 0 || stats.Deleted > 0) {
		if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
			return stats, fmt.Errorf("incremental resolve: %w", err)
		}
	}
	return stats, nil
}

// reindexOne parses a single file, mirroring the full pipeline's failure
// policy: a file that cannot be read or parsed is recorded with a non-ok status
// rather than aborting.
func reindexOne(ctx context.Context, walker *fs.Walker, parsers parse.Registry, relPath string) (FileFacts, error) {
	abs := filepath.Join(walker.Root(), filepath.FromSlash(relPath))

	info, err := os.Stat(abs)
	if err != nil {
		return FileFacts{}, err
	}

	//nolint:gosec // Reading discovered repository files IS this package's
	// purpose. abs is built from walker.Root() plus a path the walker produced,
	// and the walker refuses symlinks that escape the root.
	src, err := os.ReadFile(abs)
	if err != nil {
		return FileFacts{}, err
	}

	facts := FileFacts{
		File: model.File{
			Path:        relPath,
			SizeBytes:   info.Size(),
			ModTimeUnix: info.ModTime().Unix(),
			ContentHash: fs.HashBytes(src),
			ParseStatus: string(parse.StatusOK),
		},
		Content: src,
	}

	p := parsers.ForFile(relPath)
	if p == nil {
		facts.File.ParseStatus = string(parse.StatusSkipped)
		facts.File.ParseError = "no grammar for this file type"
		// Mirrors the full path: no grammar does not mean nothing to learn.
		// Configuration files and dependency manifests carry declarations that
		// need text rather than a syntax tree.
		if analyze.Wants(relPath) {
			facts.Analysis = analyze.File(relPath, src, nil)
		}
		// And so does the deployment layer. Editing a compose file is exactly
		// the change a watch session sees most, and without this it would
		// delete every service the file declares: ReplaceFile drops the old
		// rows first, and rows nobody re-staged do not come back.
		if infra.Candidate(relPath) {
			facts.Infra, facts.HasInfra = infra.Parse(relPath, src)
		}
		return facts, nil
	}
	facts.File.Language = p.Language()

	res, err := p.Parse(ctx, relPath, src)
	if err != nil {
		return facts, err
	}
	facts.File.ParseStatus = string(res.Status)
	if res.Err != nil {
		facts.File.ParseError = res.Err.Error()
	}
	facts.Symbols = res.Symbols
	facts.References = res.References
	facts.Imports = res.Imports

	// THE INCREMENTAL PATH MUST ANALYSE TOO.
	//
	// It did not, and the consequence was silent data loss rather than a
	// missing feature: ReplaceFile deletes the file's existing rows before
	// writing the new ones, so re-indexing a file with no analysis attached
	// ERASED its endpoints, its table accesses and its config usages.
	//
	// Reproduced: a controller declaring POST /payment/callback and writing
	// `transactions` lost both after the file was touched — `cortex api`
	// went from 1 endpoint to 0. With `cortex watch` running, the whole
	// analysis layer erodes over a working session, and nothing reports it,
	// because an empty result and a correct empty result look identical.
	//
	// The full path in pipeline.go has always done this. Two write paths, one
	// of them updated — the same shape as every other bug in this file's
	// history.
	facts.Analysis = analyze.File(relPath, src, res.Symbols)

	// Outbound calls for the same reason and with the same hazard: they are
	// deleted with the old file row, so a component re-indexed on save would
	// otherwise lose every API call it makes until the next full index.
	facts.Calls = api.ExtractCalls(relPath, src)
	if infra.Candidate(relPath) {
		facts.Infra, facts.HasInfra = infra.Parse(relPath, src)
	}
	return facts, nil
}

// writeIncrementalAnalysis re-attaches one file's analysis facts after an
// in-place replace.
//
// Mirrors RegistrySink.writeAnalysis, which the full path uses. Two write paths
// existing at all is the underlying hazard — one of them was updated when the
// analysis layer landed and the other was not, and the failure was silent.
func writeIncrementalAnalysis(
	ctx context.Context,
	reg *registry.Registry,
	runID, fileID int64,
	facts FileFacts,
	written []model.Symbol,
) error {
	res := facts.Analysis
	if len(res.Symbols) == 0 && len(res.Accesses) == 0 &&
		len(res.ConfigReads) == 0 && len(facts.Calls) == 0 && !facts.HasInfra {
		return nil
	}

	// Real ids, keyed both by FQN and by bare name — a route names its handler
	// by name, and both ends are in this one file.
	byFQN := make(map[string]int64, len(written)*2)
	for _, s := range written {
		byFQN[s.FQN] = s.ID
		if s.Name != "" {
			if _, dup := byFQN["\x00"+s.Name]; !dup {
				byFQN["\x00"+s.Name] = s.ID
			}
		}
	}

	var edges []model.Edge
	for _, exp := range res.Exposures {
		endpointID, ok := byFQN[exp.EndpointFQN]
		if !ok || exp.HandlerName == "" {
			continue
		}
		handlerID, ok := byFQN["\x00"+exp.HandlerName]
		if !ok {
			continue
		}
		edges = append(edges, model.Edge{
			SrcID: endpointID, DstID: handlerID, Kind: model.EdgeExposes,
			Confidence: exp.Confidence,
			Evidence: model.Evidence{
				FilePath: facts.File.Path,
				Range:    model.Range{Start: model.Position{Line: exp.Line}},
				Source:   model.SourceSourceCode,
				Detail:   exp.Detail,
			},
		})
	}

	for _, b := range res.EventBindings {
		eventFQN := analyze.EventFQN(b.Broker, b.Topic)
		eventID, ok := byFQN[eventFQN]
		if !ok {
			continue
		}
		var srcID int64
		if b.Function != "" {
			srcID = byFQN["\x00"+b.Function]
		}
		if srcID == 0 && b.SrcSymbolFQN != "" {
			srcID = byFQN[b.SrcSymbolFQN]
		}
		if srcID == 0 {
			continue
		}
		edges = append(edges, model.Edge{
			SrcID:      srcID,
			DstID:      eventID,
			Kind:       b.EdgeKind,
			Confidence: b.Confidence,
			Evidence: model.Evidence{
				FilePath: facts.File.Path,
				Range:    model.Range{Start: model.Position{Line: b.Line}},
				Source:   model.SourceSourceCode,
				Detail:   string(b.Broker) + " " + string(b.Direction),
			},
		})
	}

	if len(edges) > 0 {
		if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
			return err
		}
	}

	accesses := make([]registry.TableAccess, 0, len(res.Accesses))
	for _, a := range res.Accesses {
		accesses = append(accesses, registry.TableAccess{
			Table: a.Table, Access: string(a.Mode), Verb: a.Verb,
			SrcID: byFQN[a.SrcSymbolFQN], Line: a.Line, Certain: a.Certain,
		})
	}
	usages := make([]registry.ConfigUsage, 0, len(res.ConfigReads))
	for _, u := range res.ConfigReads {
		usages = append(usages, registry.ConfigUsage{
			Key: u.Key, Mechanism: u.Mechanism,
			SrcID: byFQN[u.SrcSymbolFQN], Line: u.Line,
		})
	}
	if err := reg.ReplaceFileAnalysis(ctx, runID, fileID, accesses, usages); err != nil {
		return err
	}

	calls := make([]registry.ClientCall, 0, len(facts.Calls))
	for _, c := range facts.Calls {
		var srcID int64
		if c.Caller != "" {
			srcID = byFQN["\x00"+c.Caller]
		}
		calls = append(calls, registry.ClientCall{
			Method: c.Method, Path: c.Path, Client: string(c.Client),
			SrcID: srcID, Line: c.Line, Dynamic: c.Dynamic, Confidence: c.Confidence,
		})
	}
	if err := reg.ReplaceFileCalls(ctx, runID, fileID, calls); err != nil {
		return err
	}

	// Deployment facts, re-staged for the same reason and with the same
	// hazard. The edges a manifest states are intra-file, so they are rebuilt
	// here from the ids ReplaceFile has just written; the cross-file join is
	// left to the next resolve, which is where it lives on the full path too.
	if facts.HasInfra {
		units, keySets, problems := infraRows(facts.Infra, func(fqn string) int64 { return byFQN[fqn] })
		if err := reg.ReplaceFileInfra(ctx, runID, fileID, units, keySets, problems); err != nil {
			return err
		}
		infraEdgeSet := infraEdges(facts.Infra, func(fqn string) int64 { return byFQN[fqn] })
		if len(infraEdgeSet) > 0 {
			if _, err := reg.AddResolvedEdges(ctx, runID, infraEdgeSet); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeIncrementalReferences re-attaches one file's references and imports
// after ReplaceFile has recreated it.
//
// The full path writes these through the batched Writer; the incremental path
// does not use the Writer and simply dropped them. See
// registry.ReplaceFileReferences for what that cost.
func writeIncrementalReferences(
	ctx context.Context,
	reg *registry.Registry,
	runID, fileID int64,
	facts FileFacts,
) error {
	refs := make([]registry.Reference, 0, len(facts.References))
	for _, ref := range facts.References {
		refs = append(refs, registry.Reference{
			Name:     ref.Name,
			Kind:     ref.Kind,
			Receiver: ref.Receiver,
			ArgCount: ref.ArgCount,
			Line:     ref.Range.Start.Line,
			Col:      ref.Range.Start.Col,
		})
	}
	imports := make([]registry.Import, 0, len(facts.Imports))
	for _, imp := range facts.Imports {
		imports = append(imports, registry.Import{
			Path:  imp.Path,
			Alias: imp.Alias,
			Line:  imp.Range.Start.Line,
		})
	}
	return reg.ReplaceFileReferences(ctx, runID, fileID, refs, imports)
}
