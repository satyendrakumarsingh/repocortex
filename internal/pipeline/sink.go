package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// RegistrySink persists pipeline output into a staging index run.
//
// Serialisation is deliberate. registry.Writer is documented as unsafe for
// concurrent use, and SQLite is a single-writer store regardless, so nothing
// is gained by writing in parallel. The parallelism that matters — parsing —
// happens upstream; this end of the pipeline is intentionally a funnel.
type RegistrySink struct {
	mu               sync.Mutex
	writer           *registry.Writer
	secretsFound     int
	duplicateSymbols int
	fileFailures     int
	lastFailure      string
}

// NewRegistrySink adapts a registry Writer to the pipeline's Sink interface.
func NewRegistrySink(w *registry.Writer) *RegistrySink {
	return &RegistrySink{writer: w}
}

// WriteFile persists one file's facts.
//
// References and imports are stored as UNRESOLVED rows, never as edges. A
// reference names an identifier, not a target; turning it into a graph edge
// requires the resolver to bind it and attach a confidence level. Writing edges
// here would mean inventing relationships, which is exactly the failure the
// confidence model exists to prevent.
//
// They are persisted rather than discarded because resolution needs the
// COMPLETE symbol table, which does not exist until every file is parsed.
// Keeping them means a later pass — or a re-resolution after a SCIP index
// arrives — costs no reparsing.
func (s *RegistrySink) WriteFile(ctx context.Context, facts FileFacts) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// One file, one savepoint. If persisting this file fails for a reason
	// local to its rows, undo just those rows and record the file as failed
	// rather than taking the run down with it — see Writer.BeginFile.
	if err := s.writer.BeginFile(ctx); err != nil {
		return err
	}
	if err := s.writeFileRows(ctx, facts); err != nil {
		if rbErr := s.writer.RollbackFile(ctx); rbErr != nil {
			// The rollback failed, so the batch really is doomed. Report the
			// original cause, not the rollback.
			return fmt.Errorf("persist %s: %w", facts.File.Path, err)
		}
		s.fileFailures++
		s.lastFailure = fmt.Sprintf("%s: %v", facts.File.Path, err)
		return s.recordUnpersistableFile(ctx, facts.File, err)
	}
	return s.writer.CommitFile(ctx)
}

// recordUnpersistableFile writes the file row alone, marked failed, so a file
// whose facts could not be stored is still visible as a gap.
//
// Silence would be the worse outcome: `cortex status` reports parse failures,
// and a file that vanished entirely would read as one that simply contains
// nothing.
func (s *RegistrySink) recordUnpersistableFile(ctx context.Context, f model.File, cause error) error {
	f.ParseStatus = "failed"
	f.ParseError = truncateReason(cause.Error())
	if err := s.writer.BeginFile(ctx); err != nil {
		return err
	}
	if _, err := s.writer.AddFile(ctx, &f); err != nil {
		_ = s.writer.RollbackFile(ctx)
		return fmt.Errorf("record failed file %s: %w", f.Path, err)
	}
	return s.writer.CommitFile(ctx)
}

// truncateReason keeps a stored parse_error readable. A driver error can carry
// the whole offending statement, and the files table is not a log.
func truncateReason(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func (s *RegistrySink) writeFileRows(ctx context.Context, facts FileFacts) error {
	f := facts.File
	fileID, err := s.writer.AddFile(ctx, &f)
	if err != nil {
		return fmt.Errorf("add file %s: %w", f.Path, err)
	}

	// Analysis symbols (endpoints, tables, columns) are written alongside the
	// parser's, not in a separate pass: they belong to the same file and the
	// same run, and splitting them would mean a second write path with its own
	// chance to get provenance wrong.
	symbols := facts.Symbols
	if len(facts.Analysis.Symbols) > 0 {
		symbols = append(append([]model.Symbol(nil), symbols...), facts.Analysis.Symbols...)
	}
	// Deployment nodes join the same list for the same reason: a unit and a
	// configuration key belong to this file and this run, and a second write
	// path for them would be a second chance to get provenance wrong. They are
	// appended LAST so their ids are assigned after the parser's, which keeps
	// the assignment order a pure function of the file's content.
	infraNodes := infraSymbols(facts.Infra)
	if len(infraNodes) > 0 {
		symbols = append(append([]model.Symbol(nil), symbols...), infraNodes...)
	}

	// fqnToID lets the exposure and access wiring below refer to symbols by
	// name without a second query. Real row ids, filled in as each is written.
	fqnToID := make(map[string]int64, len(symbols))

	// Qualified names first, then collapse: identity is (fqn, line), so the
	// duplicates cannot be seen until the names are assigned.
	resolveFQNs(f.Path, symbols)
	symbols, droppedDupes := collapseDuplicateDeclarations(symbols)
	s.duplicateSymbols += droppedDupes

	for i := range symbols {
		sym := symbols[i]
		sym.FileID = fileID
		id, err := s.writer.AddSymbol(ctx, &sym)
		if err != nil {
			return fmt.Errorf("add symbol %s in %s: %w", sym.Name, f.Path, err)
		}
		fqnToID[sym.FQN] = id
		if sym.Name != "" {
			// Also keyed by bare name, so a route naming its handler
			// ("handleCallback") can find the method without knowing how the
			// FQN was spelled. Ambiguity is resolved by first-wins, which is
			// safe here because both ends are in ONE file.
			if _, dup := fqnToID[nameKey(sym.Name)]; !dup {
				fqnToID[nameKey(sym.Name)] = id
			}
		}
	}

	if err := s.writeAnalysis(ctx, f.Path, fileID, facts.Analysis, fqnToID); err != nil {
		return err
	}

	if err := s.writeCalls(ctx, f.Path, fileID, facts.Calls, fqnToID); err != nil {
		return err
	}

	if err := s.writeInfra(ctx, fileID, facts, fqnToID); err != nil {
		return err
	}

	for _, imp := range facts.Imports {
		if err := s.writer.AddImport(ctx, fileID, registry.Import{
			Path:  imp.Path,
			Alias: imp.Alias,
			Line:  imp.Range.Start.Line,
		}); err != nil {
			return fmt.Errorf("add import %s in %s: %w", imp.Path, f.Path, err)
		}
	}

	for _, ref := range facts.References {
		if err := s.writer.AddReference(ctx, fileID, registry.Reference{
			Name:     ref.Name,
			Kind:     ref.Kind,
			Receiver: ref.Receiver,
			ArgCount: ref.ArgCount,
			Line:     ref.Range.Start.Line,
			Col:      ref.Range.Start.Col,
		}); err != nil {
			return fmt.Errorf("add reference %s in %s: %w", ref.Name, f.Path, err)
		}
	}

	// Search rows only for files that actually carry text. Skipped files
	// (binary, oversized) are recorded in `files` so the gap stays visible,
	// but they must not pollute the search index.
	if len(facts.Content) > 0 && f.ParseStatus != string(statusSkipped) {
		// REDACT BEFORE STORING. This is the first of two barriers, and the
		// more important one: a credential that never enters the index cannot
		// leak from it later, through search results, an excerpt, a backup of
		// .cortex/, or a future tool nobody has written yet.
		//
		// RedactFile classifies by path first, so an entire .env or private key
		// is suppressed wholesale rather than pattern-matched line by line.
		text, findings := security.RedactFile(f.Path, string(facts.Content))
		// s.mu is already held for the whole of WriteFile; taking it again here
		// would deadlock on Go's non-reentrant Mutex.
		s.secretsFound += len(findings)
		if err := s.writer.IndexFileText(ctx, fileID, f.Path, text); err != nil {
			return fmt.Errorf("index text for %s: %w", f.Path, err)
		}
	}
	return nil
}

// SecretsRedacted reports how many credentials were removed during indexing.
//
// Surfaced rather than silent: a developer whose repository contains committed
// secrets should learn that from their tools, and a count that quietly rises is
// worth seeing.
func (s *RegistrySink) SecretsRedacted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secretsFound
}

// DuplicateSymbolsDropped counts declarations collapsed because they shared a
// file, qualified name and line with one already written.
//
// Reported rather than swallowed: it used to be fatal — a single TypeScript
// line declaring a function and a same-named method signature aborted an
// 8,094-file index at 80% — and a defect that once took the whole run down
// should stay visible as a number after it stops being fatal.
func (s *RegistrySink) DuplicateSymbolsDropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicateSymbols
}

// UnpersistableFiles counts files whose facts could not be stored, and names
// the last one.
//
// A count of zero is the normal case and the interesting one is any other
// number: these are files the parser handled but the registry rejected, which
// means extraction produced something the schema does not model. That is a bug
// report waiting to be written, so it must not be silent.
func (s *RegistrySink) UnpersistableFiles() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fileFailures, s.lastFailure
}

// writeCalls stages this file's outbound HTTP calls.
//
// The third case in the taxonomy writeAnalysis sets out, and the reason it is a
// function of its own. An exposure is intra-file and is written as an edge here.
// An access names a table declared anywhere and is staged. A CALL is the second
// kind: `fetch("/api/orders/42")` in a component names a route a controller
// declares, possibly in a file this run has not reached yet, so the edge cannot
// exist until every file has been seen. Staged now, bound by
// registry.BindClientCalls — which is also what makes the binding survive an
// incremental update that reparses only the component.
//
// The caller is resolved to a symbol id the same way an exposure's handler is,
// through the bare-name key, and for the same reason: both ends of THAT lookup
// are in one file, so first-wins is safe. A call outside every declaration is
// staged with no source symbol rather than dropped — it still counts toward how
// many of this file's calls were resolved.
func (s *RegistrySink) writeCalls(
	ctx context.Context,
	path string,
	fileID int64,
	calls []api.Call,
	fqnToID map[string]int64,
) error {
	for _, c := range calls {
		var srcID int64
		if c.Caller != "" {
			srcID = fqnToID[nameKey(c.Caller)]
		}
		if err := s.writer.AddClientCall(ctx, fileID, registry.ClientCall{
			Method:     c.Method,
			Path:       c.Path,
			Client:     string(c.Client),
			SrcID:      srcID,
			Line:       c.Line,
			Dynamic:    c.Dynamic,
			Confidence: c.Confidence,
		}); err != nil {
			return fmt.Errorf("add client call %q in %s: %w", c.Path, path, err)
		}
	}
	return nil
}

// RecordCompleteness persists the run's coverage verdict onto the staging run.
//
// This is the whole of A28's wiring: reconcile.go computes the proof, the
// registry owns the columns, and this is the one place that translates between
// them. The translation is explicit — a registry.Completeness built field by
// field — because the registry must not import this package (the dependency
// runs pipeline -> registry) and so cannot be handed a Reconciliation.
//
// Recorded even when the run is complete. "Complete" is a measurement, and a
// column that is only ever written when something went wrong cannot be
// distinguished from a column nobody wrote.
func (s *RegistrySink) RecordCompleteness(ctx context.Context, r Reconciliation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state := model.IndexStateComplete
	if !r.Complete() {
		state = model.IndexStatePartial
	}
	return s.writer.RecordCompleteness(ctx, registry.Completeness{
		FilesDiscovered:  r.Discovered,
		FilesUnaccounted: r.Unaccounted,
		State:            state,
	})
}

// Flush commits any partially-filled batch.
func (s *RegistrySink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writer.Flush(ctx)
}

// statusSkipped mirrors parse.StatusSkipped without importing the parse
// package here, keeping the persistence adapter free of parser concerns.
const statusSkipped = "skipped"

// ExpandForSearch is exposed so callers can pre-compute identifier expansion
// when they already hold the text. The registry does this internally; this
// exists for tests and for callers assembling search rows directly.
func ExpandForSearch(content string) string { return search.ExpandIdentifiers(content) }

// nameKey namespaces a bare symbol name so it cannot collide with an FQN in the
// same lookup table.
func nameKey(name string) string { return "\x00name\x00" + name }

// writeAnalysis persists the API and database facts for one file.
//
// Two different jobs, and the difference is which pass can finish them.
//
// An EXPOSURE is intra-file: the route and the handler it dispatches to are
// both in the file being written, so the edge is complete here and is written
// here. Deferring it would add a pass for no benefit.
//
// An ACCESS is not. The table a query names is declared by a migration that may
// be parsed later, or on an incremental update not parsed at all. So accesses
// are staged and bound afterwards — the same shape, and the same reasoning, as
// symbol_references. See migration 0003.
func (s *RegistrySink) writeAnalysis(
	ctx context.Context,
	path string,
	fileID int64,
	res analyze.Result,
	fqnToID map[string]int64,
) error {
	for _, exp := range res.Exposures {
		endpointID, ok := fqnToID[exp.EndpointFQN]
		if !ok {
			continue
		}
		if exp.HandlerName == "" {
			// A route whose handler the framework did not reveal. The endpoint
			// node still exists and is still reported; inventing an edge to the
			// nearest function would be a guess wearing an edge's clothes.
			continue
		}
		handlerID, ok := fqnToID[nameKey(exp.HandlerName)]
		if !ok {
			continue
		}
		edge := model.Edge{
			SrcID: endpointID, DstID: handlerID, Kind: model.EdgeExposes,
			Confidence: exp.Confidence,
			Evidence: model.Evidence{
				FilePath: path,
				Range:    model.Range{Start: model.Position{Line: exp.Line}},
				Source:   model.SourceSourceCode,
				Detail:   exp.Detail,
			},
		}
		if _, err := s.writer.AddEdge(ctx, &edge); err != nil {
			return fmt.Errorf("add exposes edge in %s: %w", path, err)
		}
	}

	for _, read := range res.ConfigReads {
		var srcID int64
		if read.SrcSymbolFQN != "" {
			srcID = fqnToID[read.SrcSymbolFQN]
		}
		if err := s.writer.AddConfigUsage(ctx, fileID, registry.ConfigUsage{
			Key:       read.Key,
			Mechanism: read.Mechanism,
			SrcID:     srcID,
			Line:      read.Line,
		}); err != nil {
			return fmt.Errorf("add config usage %s in %s: %w", read.Key, path, err)
		}
	}

	for _, acc := range res.Accesses {
		var srcID int64
		if acc.SrcSymbolFQN != "" {
			srcID = fqnToID[acc.SrcSymbolFQN]
		}
		if err := s.writer.AddTableAccess(ctx, fileID, registry.TableAccess{
			Table:   acc.Table,
			Access:  string(acc.Mode),
			Verb:    acc.Verb,
			SrcID:   srcID,
			Line:    acc.Line,
			Certain: acc.Certain,
		}); err != nil {
			return fmt.Errorf("add table access %s in %s: %w", acc.Table, path, err)
		}
	}

	for _, b := range res.EventBindings {
		eventFQN := analyze.EventFQN(b.Broker, b.Topic)
		eventID, ok := fqnToID[eventFQN]
		if !ok {
			continue
		}
		var srcID int64
		if b.Function != "" {
			srcID = fqnToID[nameKey(b.Function)]
		}
		if srcID == 0 && b.SrcSymbolFQN != "" {
			srcID = fqnToID[b.SrcSymbolFQN]
		}
		if srcID == 0 {
			continue
		}
		edge := model.Edge{
			SrcID:      srcID,
			DstID:      eventID,
			Kind:       b.EdgeKind,
			Confidence: b.Confidence,
			Evidence: model.Evidence{
				FilePath: path,
				Range:    model.Range{Start: model.Position{Line: b.Line}},
				Source:   model.SourceSourceCode,
				Detail:   string(b.Broker) + " " + string(b.Direction),
			},
		}
		if _, err := s.writer.AddEdge(ctx, &edge); err != nil {
			return fmt.Errorf("add event %s edge in %s: %w", b.EdgeKind, path, err)
		}
	}
	return nil
}

// writeInfra persists what one deployment manifest declared.
//
// The fourth case in the taxonomy writeAnalysis sets out, and the one that is
// both at once. A unit's configuration keys are intra-file — the compose file
// names its own services and its own keys — so those edges are complete here
// and are written here. The unit's BUILD CONTEXT is not: it names a directory
// whose endpoints are declared by files this run may not have reached, so the
// unit rows are staged and joined by registry.BindInfraUnits afterwards.
//
// A manifest that declared nothing but PROBLEMS still writes rows. That is the
// whole point of the problems table: "this repository has no Kubernetes
// manifests" and "this repository's Kubernetes manifests could not be parsed"
// have opposite consequences, and an index that stored only successes makes
// them indistinguishable to every consumer downstream.
//
// NO VALUE IS WRITTEN. infra.EnvKey and infra.KeySet hold key names, and there
// is no field on either that a value fits in — so there is no expression in
// this function that could name one.
func (s *RegistrySink) writeInfra(
	ctx context.Context,
	fileID int64,
	facts FileFacts,
	fqnToID map[string]int64,
) error {
	if !facts.HasInfra {
		return nil
	}
	f := facts.Infra
	symbolID := func(fqn string) int64 { return fqnToID[fqn] }

	units, keySets, problems := infraRows(f, symbolID)

	for _, u := range units {
		unitID, err := s.writer.AddInfraUnit(ctx, fileID, u.InfraUnit)
		if err != nil {
			return fmt.Errorf("add deployment unit %s in %s: %w", u.Name, f.Path, err)
		}
		for _, p := range u.Ports {
			if err := s.writer.AddInfraPort(ctx, unitID, p); err != nil {
				return fmt.Errorf("add port for %s in %s: %w", u.Name, f.Path, err)
			}
		}
		for _, l := range u.Limits {
			if err := s.writer.AddInfraLimit(ctx, unitID, l); err != nil {
				return fmt.Errorf("add limit for %s in %s: %w", u.Name, f.Path, err)
			}
		}
		for _, e := range u.EnvFrom {
			if err := s.writer.AddInfraEnvFrom(ctx, unitID, e); err != nil {
				return fmt.Errorf("add envFrom for %s in %s: %w", u.Name, f.Path, err)
			}
		}
		for _, k := range u.Keys {
			k.UnitID = unitID
			if err := s.writer.AddInfraKey(ctx, fileID, k); err != nil {
				return fmt.Errorf("add config key %s for %s in %s: %w", k.Name, u.Name, f.Path, err)
			}
		}
	}

	for _, ks := range keySets {
		for _, k := range ks.Keys {
			if err := s.writer.AddInfraKey(ctx, fileID, k); err != nil {
				return fmt.Errorf("add config key %s in %s: %w", k.Name, f.Path, err)
			}
		}
	}

	for _, p := range problems {
		if err := s.writer.AddInfraProblem(ctx, fileID, p); err != nil {
			return fmt.Errorf("add manifest problem in %s: %w", f.Path, err)
		}
	}

	for _, e := range infraEdges(f, symbolID) {
		edge := e
		if _, err := s.writer.AddEdge(ctx, &edge); err != nil {
			return fmt.Errorf("add %s edge in %s: %w", edge.Kind, f.Path, err)
		}
	}
	return nil
}
