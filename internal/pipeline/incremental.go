package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
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

	// Touched marks an unchanged file whose size or modification time no
	// longer matches the index: it was read, and its content is what the index
	// already holds. Incremental records the new size and time so the next
	// pass can leave it unread; nothing is re-parsed.
	Touched     bool
	SizeBytes   int64
	ModTimeUnix int64
}

// IncrementalStats reports what an incremental pass did.
type IncrementalStats struct {
	Examined  int64
	Added     int64
	Modified  int64
	Deleted   int64
	Unchanged int64
	Symbols   int64
	// DefinitionsBackfilled is how many functions and methods were given
	// definition text because the index was built before it was stored.
	DefinitionsBackfilled int64
}

// DiffOptions adjusts how the working tree is compared with the index.
type DiffOptions struct {
	// Rehash reads and hashes every file, as every pass did before files
	// could be trusted by size and time. It is the answer to the cases
	// internal/acquire/fs/trust.go lists as not covered.
	Rehash bool
}

// Diff compares the working tree against a committed index run.
//
// The verdict on a file is its content hash, never its modification time.
// mtime is unreliable across checkouts, clones and restores — `git checkout`
// rewrites timestamps on files whose content is unchanged, which would make an
// mtime-based diff re-index a whole repository after a branch switch.
//
// What size and time are used for is deciding which files need hashing at all.
// A file whose size and times match the index, and are old enough to be
// believed, is not read; every other file is hashed and the hash decides. The
// rule is stated once, with its limits, in internal/acquire/fs/trust.go. It
// exists because hashing was the cost of a pass: on django's 7,090 files a
// re-index that changed nothing spent 1.1 s to 4.5 s reading files to learn
// that, and with it 0.3 s.
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
	return DiffWith(ctx, reg, runID, walker, runningAs, DiffOptions{})
}

// DiffWith is Diff with options.
func DiffWith(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
	runningAs string,
	opts DiffOptions,
) ([]FileChange, error) {
	run, err := reg.Run(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read run %d: %w", runID, err)
	}
	verified := run.StartedUnix
	if opts.Rehash {
		verified = 0
	}
	return diffAgainst(ctx, reg, runID, walker, run.ProducerVersion, runningAs, verified)
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
	verifiedUnix int64,
) ([]FileChange, error) {
	sameProducer := indexedBy != "" && indexedBy == runningAs

	indexed, err := reg.Files(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read indexed files: %w", err)
	}
	prior := make(map[string]string, len(indexed))
	// What the walk may leave unread. Built only when this build wrote the
	// run: across builds every file is re-parsed whatever its hash, so there
	// is nothing a recorded hash could save. verifiedUnix is when the pass
	// that last looked at every file began; zero trusts nothing.
	var trust *fs.Trust
	if sameProducer && verifiedUnix > 0 {
		trust = &fs.Trust{Files: make(map[string]fs.Known, len(indexed)), VerifiedUnix: verifiedUnix}
	}
	stat := make(map[string]registry.FileStat, len(indexed))
	for _, f := range indexed {
		if !sameProducer {
			// Present, so it is not "added" and not "deleted" — but never equal,
			// so every file re-parses under the current build's rules.
			prior[f.Path] = ""
			continue
		}
		prior[f.Path] = f.ContentHash
		stat[f.Path] = registry.FileStat{Path: f.Path, SizeBytes: f.SizeBytes, ModTimeUnix: f.ModTimeUnix}
		if trust != nil {
			known := fs.Known{SizeBytes: f.SizeBytes, ModTimeUnix: f.ModTimeUnix, ContentHash: f.ContentHash}
			if f.ContentHash == "" {
				// A file the walk skipped is stored with its reason as the
				// parse error; see Runner.processFile.
				known.SkipReason = f.ParseError
			}
			trust.Files[f.Path] = known
		}
	}

	current, err := walker.WalkTrusting(ctx, trust)
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
			ch := FileChange{Path: d.Path, Kind: ChangeSame}
			// Read, identical, and stat'ed differently from the index: record
			// the new size and time so the next pass need not read it again.
			if was, ok := stat[d.Path]; ok && !d.Unread && (!d.Skipped || d.SkipReason == fs.SkipBinary) &&
				(was.SizeBytes != d.SizeBytes || was.ModTimeUnix != d.ModTimeUnix) {
				ch.Touched, ch.SizeBytes, ch.ModTimeUnix = true, d.SizeBytes, d.ModTimeUnix
			}
			changes = append(changes, ch)
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
	// Root is the repository root directory. When provided and Go files were
	// changed, CheckGoTypes runs in-process to preserve exact compiler edges.
	Root string
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
// consistent but partially updated, which `repocortex status` reports honestly
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
		parsers = parse.NewRegistry(parse.WithMaxSourceBytes(int(walker.MaxFileSize())))
	}

	var touched []registry.FileStat
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Examined++

		switch ch.Kind {
		case ChangeSame:
			stats.Unchanged++
			if ch.Touched {
				touched = append(touched, registry.FileStat{
					Path: ch.Path, SizeBytes: ch.SizeBytes, ModTimeUnix: ch.ModTimeUnix,
				})
			}
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
			if len(facts.Content) > 0 && model.TextIndexed(f.ParseStatus, f.ParseError) {
				// REDACTED, exactly as the full path redacts (see
				// RegistrySink.writeFileRows). This path used to hand the raw
				// bytes to the search index, so a credential in a file edited
				// under `repocortex watch` was stored unredacted until the next
				// full index. That was a gap for source files, and it could not
				// be left open once configuration formats (YAML, JSON, INI,
				// properties: text-only languages) became searchable, because
				// those are where credentials are written down.
				text, _ := security.RedactFile(f.Path, string(facts.Content))
				replaceOpts = append(replaceOpts, registry.WithContent(text))
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
	if err := reg.RestatFiles(ctx, runID, touched); err != nil {
		return stats, err
	}
	hasGoChanges := false
	for _, ch := range changes {
		if strings.HasSuffix(ch.Path, ".go") {
			hasGoChanges = true
			break
		}
	}
	if opts.Resolve && (stats.Added > 0 || stats.Modified > 0 || stats.Deleted > 0) {
		if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
			return stats, fmt.Errorf("incremental resolve: %w", err)
		}
		if hasGoChanges && opts.Root != "" {
			_ = CheckGoTypes(ctx, reg, runID, GoTypesOptions{Root: opts.Root})
		}
	}
	return stats, nil
}

// RefreshOptions configures Refresh.
type RefreshOptions struct {
	// RunningAs names the build doing the work; see Diff.
	RunningAs string
	// Rehash reads every file instead of trusting size and time; see
	// DiffOptions.
	Rehash  bool
	Parsers parse.Registry
	// Root is the repository root directory, passed to Incremental.
	Root string
	// Now is the wall clock, injectable for tests. It must be the clock file
	// times are stamped by, which is why it is not the registry's.
	Now func() time.Time
}

// Refresh brings a committed run up to date with the working tree, in place,
// and records that it did.
//
// It is Diff, then Incremental, then the three facts a completed pass
// establishes: which build's rules the run now reflects, when the pass began,
// and when it ended. `repocortex index` and `repocortex watch` both go through
// it, because they used to do those steps separately and differently. Watch
// never stamped the build, so after an upgrade every batch of events re-parsed
// the whole repository; and neither stamped a time, so an index updated a
// minute ago reported the age of its first full build.
//
// The time the pass BEGAN is taken before discovery starts and is what the
// next pass trusts file times against. Nothing is stamped when the pass fails.
func Refresh(
	ctx context.Context,
	reg *registry.Registry,
	runID int64,
	walker *fs.Walker,
	opts RefreshOptions,
) (IncrementalStats, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	started := now().Unix()

	// An index built before definition text was stored has none. Filled
	// from the stored file text before any file is replaced: a replaced file
	// writes its own rows, and the backfill runs only while there are none.
	backfilled, err := reg.BackfillDefinitionText(ctx, runID)
	if err != nil {
		return IncrementalStats{}, err
	}

	changes, err := DiffWith(ctx, reg, runID, walker, opts.RunningAs, DiffOptions{Rehash: opts.Rehash})
	if err != nil {
		return IncrementalStats{}, fmt.Errorf("diff working tree: %w", err)
	}
	stats, err := Incremental(ctx, reg, runID, walker, changes, IncrementalOptions{
		Parsers: opts.Parsers, Resolve: true, Root: opts.Root,
	})
	stats.DefinitionsBackfilled = int64(backfilled)
	if err != nil {
		return stats, err
	}
	// The run's own status was written when it was first published. Files have
	// just been rewritten in place, so recompute it from what the run holds
	// now; see registry.RefreshRunHealth for the case that made this matter.
	if stats.Added+stats.Modified+stats.Deleted > 0 {
		if err := reg.RefreshRunHealth(ctx, runID); err != nil {
			return stats, err
		}
		// And the binding figures every relationship answer opens with, which
		// the pass above changed. See registry.RefreshRunStats.
		if err := reg.RefreshRunStats(ctx, runID); err != nil {
			return stats, err
		}
	}
	// The pass succeeded, so this run's contents are now this build's work.
	// Skipping this would turn the build-crossing rule into a permanent
	// penalty: the differ treats a version mismatch as "everything is dirty".
	if err := reg.StampProducer(ctx, runID, opts.RunningAs); err != nil {
		return stats, err
	}
	if err := reg.MarkUpdated(ctx, runID, started); err != nil {
		return stats, err
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

	// The size limit, checked BEFORE the read. The full path never reads a
	// file discovery refused; this path is handed bare paths, so without the
	// check an edit to a multi-gigabyte file would be read into memory whole
	// in order to be told it is too large.
	if limit := walker.MaxFileSize(); info.Size() > limit {
		return FileFacts{File: model.File{
			Path:        relPath,
			Language:    languageForPath(parsers, relPath),
			SizeBytes:   info.Size(),
			ModTimeUnix: info.ModTime().Unix(),
			ParseStatus: string(parse.StatusSkipped),
			ParseError:  parse.TooLargeError(info.Size(), limit),
		}}, nil
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

	// Detected from the bytes just read, by the walker's own detector, so a
	// re-indexed file cannot arrive at a different language than the full
	// index gave it.
	det := walker.Detect(relPath, src)
	facts.File.DetectedBy = string(det.Method)
	p := parsers.ForLanguage(det.Language)
	if p == nil {
		facts.File.ParseStatus = string(parse.StatusSkipped)
		facts.File.ParseError = noGrammarReason(det.Recognised)
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
	// `transactions` lost both after the file was touched — `repocortex api`
	// went from 1 endpoint to 0. With `repocortex watch` running, the whole
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
			Name:         ref.Name,
			Kind:         ref.Kind,
			Receiver:     ref.Receiver,
			ReceiverKind: ref.ReceiverKind,
			ArgCount:     ref.ArgCount,
			Line:         ref.Range.Start.Line,
			Col:          ref.Range.Start.Col,
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

// languageForPath names the language a path would be parsed as, or "" when no
// grammar claims it. Used for a file that is recorded without being read.
func languageForPath(parsers parse.Registry, relPath string) string {
	if p := parsers.ForFile(relPath); p != nil {
		return p.Language()
	}
	return ""
}
