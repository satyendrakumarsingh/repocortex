// Package pipeline wires discovery and parsing into an indexing run.
//
// It deliberately does not know how facts are stored. The Sink interface below
// is the whole contract with the persistence layer, which keeps this package
// testable without a database and lets the registry evolve independently.
//
// Naming note: this is the source-indexing pipeline. internal/indexer is a
// different thing — orchestration of external compiler-backed SCIP indexers.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// FileFacts is everything one file contributed to the index.
type FileFacts struct {
	File       model.File
	Symbols    []model.Symbol
	References []parse.Reference
	Imports    []parse.Import

	// Analysis is the API and database facts this file contributed: endpoint
	// and table nodes, the routes they expose, and the tables the file's code
	// reads or writes. Empty for the overwhelming majority of files, which
	// declare no routes and run no queries.
	Analysis analyze.Result

	// Calls are the outbound HTTP requests this file makes.
	//
	// Extracted here rather than inside analyze.File because they are not
	// resolvable facts about this file: a call names an endpoint some OTHER
	// file declares, so it is staged and bound in a second pass. Keeping it
	// beside Analysis rather than inside it keeps that difference visible at
	// the point a reader asks why one is written immediately and the other is
	// not. See migration 0007.
	Calls []api.Call

	// Infra is what a DEPLOYMENT MANIFEST declared: the services this file says
	// exist, the ports they open, the configuration key names they set, and the
	// documents inside it that could not be read.
	//
	// Kept beside Analysis rather than inside it because the two answer opposite
	// questions. analyze.Result is what this file's SOURCE does; this is what
	// the code RUNS AS, and it is produced from a file that usually has no
	// grammar at all. Nothing here holds a configuration value — see
	// internal/analyze/infra's package comment.
	Infra infra.File
	// HasInfra distinguishes "this file is a manifest that declared nothing"
	// from "this file is not a manifest". Only the first is worth a row.
	HasInfra bool

	// Content is retained only for search indexing. It is not persisted as
	// source; the registry stores an FTS row and line ranges, never a copy of
	// the repository.
	Content []byte
}

// Sink receives facts as they are produced.
//
// Implementations must be safe for concurrent use: the pipeline parses across
// many goroutines and writes as results arrive, rather than buffering an entire
// repository in memory.
type Sink interface {
	// WriteFile persists one file's facts. Returning an error aborts the run,
	// so implementations should only fail on genuine persistence problems —
	// a file that failed to parse is normal data, not an error.
	WriteFile(ctx context.Context, facts FileFacts) error
}

// CompletenessRecorder is an optional capability of a Sink: one that persists
// into a durable run can be told, once, how much of what was discovered the run
// actually covered.
//
// Optional rather than part of Sink because most sinks are test doubles with
// nothing to record it on, and widening the one interface the pipeline's
// persistence contract consists of would make every one of them implement a
// method it has no use for (CLAUDE.md §3, ISP).
//
// The Runner calls it exactly once, after the worker pool is joined and the
// counters are final. A sink that returns an error here fails the run: a
// shortfall the sink refused to record is a shortfall nobody will ever see.
type CompletenessRecorder interface {
	RecordCompleteness(ctx context.Context, r Reconciliation) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, facts FileFacts) error

func (f SinkFunc) WriteFile(ctx context.Context, facts FileFacts) error { return f(ctx, facts) }

// Stats summarises a run. Every number is counted, never estimated.
type Stats struct {
	FilesDiscovered int64
	FilesIndexed    int64
	FilesSkipped    int64
	ParseFailures   int64
	Symbols         int64
	References      int64
	Imports         int64

	// Reconciliation proves the run covered everything it discovered, which is
	// a separate guarantee from the registry's atomic swap proving it was
	// published all-or-nothing. See reconcile.go.
	//
	// Callers that publish a run should persist Reconciliation.State and
	// surface Reconciliation.Warning() when it is non-empty.
	Reconciliation Reconciliation
}

// DefaultInFlightBytes caps the source bytes being parsed simultaneously.
//
// 32 MiB of source, not of memory. Tree-sitter trees are one to two orders of
// magnitude larger than their input, so this admits roughly a gigabyte of peak
// parse state — enough to keep every core busy on ordinary code, low enough
// that a directory of generated files cannot exhaust the machine.
//
// Chosen from measurement rather than taste: see the table in budget.go.
//
// It governs ADMISSION, not peak memory. Varying it eightfold moved peak RSS
// on spring-boot by less than run-to-run noise; see Options.MaxInFlightBytes
// for the numbers and DefaultDrainThreshold for the knob that does move it.
const DefaultInFlightBytes int64 = 32 << 20

// defaultWorkers picks parse concurrency for memory, not for CPU.
//
// runtime.NumCPU() is the obvious default and it is wrong here. Measured on a
// 10,920-file tree:
//
//	workers   peak heap   elapsed
//	      2     1,293 MB       58s
//	      4     1,595 MB       54s
//	      8     2,502 MB       54s
//	     10     6,531 MB       44s
//
// Going from 4 to 10 workers bought 10 seconds and cost 4 GB. Tree-sitter's
// GLR parser allocates large node arenas per file, so every additional worker
// multiplies peak memory, while throughput saturates early because parsing is
// bound by memory bandwidth rather than CPU.
//
// Four is the knee of that curve: near-peak throughput at a footprint that fits
// comfortably on a developer laptop alongside an IDE. Raise it with
// Options.Workers when indexing on a machine with memory to spare.
func defaultWorkers() int {
	const knee = 4
	if n := runtime.NumCPU(); n < knee {
		return n
	}
	return knee
}

// DefaultDrainThreshold is the heap size above which the pipeline starts
// returning pooled parser memory.
//
// This is the knob that actually bounds indexing's peak memory. It was 1 GiB,
// on the reasoning that a gigabyte is "comfortably above what an ordinary
// repository ever reaches" — and real repositories disproved that. Indexing
// three of them, best of three runs each:
//
//	repository            files    1 GiB      256 MiB
//	prometheus (Go)       1,262    837 MB     632 MB   -24%, +3.7% time
//	nest (TypeScript)     2,297    839 MB     502 MB   -40%, +11% time
//	spring-boot (Java)   11,851  1,279 MB     744 MB   -42%, +5.4% time
//
// A 1,262-file Go repository reaching 837 MB is not an extreme case, it is a
// small one. At 1 GiB the pool effectively never drained on any of the three,
// so the threshold was not a safety valve, it was the peak.
//
// 256 MiB buys a quarter to nearly half the memory back for single-digit to
// low-double-digit percent of the time. Peak memory is what decides whether
// indexing survives a 2 GB CI container or a laptop already running an IDE;
// a few percent of wall clock on a command that runs in seconds is not.
const DefaultDrainThreshold int64 = 256 << 20

// LargeFileDrainThreshold is the file size (256 KiB) above which a worker checks
// for pool draining immediately after parsing, so that large generated files do not
// pin hundreds of megabytes in parser arena pools until the next DrainInterval.
const LargeFileDrainThreshold int64 = 256 * 1024

// Options configures a run.
type Options struct {
	// Workers bounds parse concurrency. Defaults to runtime.NumCPU().
	Workers int

	// MaxInFlightBytes bounds the source bytes parsed simultaneously.
	// Defaults to DefaultInFlightBytes.
	//
	// It bounds admission, NOT peak memory, whatever this comment used to
	// claim. Measured on spring-boot, best of two runs at each setting:
	// 4 MiB -> 1,347 MB, 8 MiB -> 1,305 MB, 16 MiB -> 1,377 MB,
	// 32 MiB -> 1,350 MB. An eightfold change in the budget moved peak RSS by
	// less than its own run-to-run noise, because what dominates is pooled
	// parser arenas that outlive any single file. DrainThresholdBytes is the
	// control that moves peak memory; this one keeps a single enormous file
	// from being admitted alongside everything else.
	MaxInFlightBytes int64

	// DrainThresholdBytes is the heap size above which pooled parser memory is
	// released mid-run. Defaults to DefaultDrainThreshold.
	DrainThresholdBytes int64

	// Parsers resolves a file to its parser. Defaults to parse.NewRegistry().
	Parsers parse.Registry

	// OnProgress, when set, is called as files complete. It must be cheap and
	// safe for concurrent use.
	OnProgress func(done, total int64)
}

// Runner executes indexing runs.
type Runner struct {
	walker  *fs.Walker
	parsers parse.Registry
	workers int
	budget  *memBudget
	// drainThreshold is the heap size above which pooled parser memory is
	// released. See maybeDrain.
	drainThreshold int64
	onProg         func(done, total int64)
}

// NewRunner builds a Runner over an existing walker.
func NewRunner(walker *fs.Walker, opts Options) *Runner {
	r := &Runner{
		walker:  walker,
		parsers: opts.Parsers,
		workers: opts.Workers,
		onProg:  opts.OnProgress,
	}
	if r.parsers == nil {
		r.parsers = parse.NewRegistry()
	}
	if r.workers <= 0 {
		r.workers = defaultWorkers()
	}
	r.budget = newMemBudget(opts.MaxInFlightBytes)
	r.drainThreshold = opts.DrainThresholdBytes
	if r.drainThreshold <= 0 {
		r.drainThreshold = DefaultDrainThreshold
	}
	return r
}

// Run discovers, parses and emits facts for every eligible file.
//
// Failure policy: a file that cannot be read or parsed is recorded with a
// non-ok parse status and the run continues. Only context cancellation or a
// Sink error aborts. A single unparseable file in a large repository must not
// cost the entire index — spec §13.
func (r *Runner) Run(ctx context.Context, sink Sink) (Stats, error) {
	var stats Stats

	discovered, err := r.walker.Walk(ctx)
	if err != nil {
		return stats, fmt.Errorf("discovery: %w", err)
	}
	stats.FilesDiscovered = int64(len(discovered))

	// Deterministic ordering. The registry digest that backs the determinism
	// gate must not depend on scheduling, so work is dispatched in a stable
	// order even though completion order varies.
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].Path < discovered[j].Path })

	var (
		symbols, refs, imports   atomic.Int64
		indexed, skipped, failed atomic.Int64
		done                     atomic.Int64
		total                    = int64(len(discovered))
	)

	work := make(chan fs.Discovered)
	var wg sync.WaitGroup

	// One error aborts the run. errOnce keeps the first, which is the
	// informative one; later failures are usually cascade noise.
	var (
		errMu  sync.Mutex
		runErr error
	)
	failRun := func(e error) {
		errMu.Lock()
		if runErr == nil {
			runErr = e
		}
		errMu.Unlock()
	}

	// Two contexts with deliberately different lifetimes.
	//
	// sinkCtx is the caller's. A Sink typically holds an open database
	// transaction that must survive past Run's return so the caller can flush
	// and commit it.
	//
	// ctx below is internal: cancelling it stops workers early when a sink
	// fails. Passing it to the Sink would be a subtle disaster — database/sql
	// rolls back a transaction as soon as the context it was created with is
	// cancelled, so `defer cancel()` would silently destroy the very rows we
	// just wrote, and the caller's Flush would fail with ErrTxDone. That bug
	// was real; this comment is what stops it coming back.
	sinkCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < r.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				if ctx.Err() != nil {
					return
				}

				facts, outcome := r.processFile(ctx, d)
				switch outcome {
				case outcomeSkipped:
					skipped.Add(1)
				case outcomeFailed:
					failed.Add(1)
				}

				// sinkCtx, not ctx — see the lifetime note above.
				if err := sink.WriteFile(sinkCtx, facts); err != nil {
					failRun(fmt.Errorf("persist %s: %w", d.Path, err))
					cancel()
					return
				}

				if outcome == outcomeIndexed {
					indexed.Add(1)
					symbols.Add(int64(len(facts.Symbols)))
					refs.Add(int64(len(facts.References)))
					imports.Add(int64(len(facts.Imports)))
				}

				n := done.Add(1)

				// Return pooled parser arenas periodically. gotreesitter holds
				// them as strong references until drained, so without this the
				// pool grows with the repository rather than with concurrency
				// and peak memory becomes the sum of every parse. See
				// parse.ReleasePooledMemory for the measurements.
				//
				// In addition, if a worker just parsed a large file (>256 KiB),
				// pooled arenas can spike memory significantly. Check the heap
				// immediately instead of waiting for DrainInterval so generated
				// files do not pin hundreds of megabytes in the pool.
				if n%parse.DrainInterval == 0 || d.SizeBytes > LargeFileDrainThreshold {
					r.maybeDrain()
				}

				if r.onProg != nil {
					r.onProg(n, total)
				}
			}
		}()
	}

	for _, d := range discovered {
		select {
		case work <- d:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()

	// Final drain: the run is over, so every arena in the pool is garbage.
	// Leaving them reachable would keep the indexer's peak footprint resident
	// for the lifetime of a long-running `cortex watch`.
	r.drainPools()

	stats.FilesIndexed = indexed.Load()
	stats.FilesSkipped = skipped.Load()
	stats.ParseFailures = failed.Load()
	stats.Symbols = symbols.Load()
	stats.References = refs.Load()
	stats.Imports = imports.Load()

	// Reconcile once, after wg.Wait, so every counter is final and no worker
	// can still be mutating one. Arithmetic over settled values: no lock, no
	// extra atomic on the hot path, and a result that cannot vary with
	// scheduling. A run may be atomically published and still not cover what it
	// discovered; this is the check that tells the two apart. See reconcile.go.
	stats.Reconciliation = reconcile(stats, ctx.Err() != nil)

	// Persist the verdict through the sink, if the sink can hold one.
	//
	// context.WithoutCancel deliberately: the run this matters most for is the
	// cancelled one, and recording its shortfall through the context that just
	// cancelled it would be the one write guaranteed to fail. The work is one
	// UPDATE of a row the sink already owns.
	if rec, ok := sink.(CompletenessRecorder); ok {
		if err := rec.RecordCompleteness(context.WithoutCancel(ctx), stats.Reconciliation); err != nil && runErr == nil {
			return stats, fmt.Errorf("record index completeness: %w", err)
		}
	}

	if runErr != nil {
		return stats, runErr
	}
	// Cancellation is not a run failure: the caller asked to stop, so Run
	// returns cleanly and lets the caller decide what to do with what was
	// built. A deadline, which nobody asked for, is an error.
	//
	// This is the one path that returns nil with files left unprocessed, and it
	// is why Stats.Reconciliation exists rather than an error being enough. The
	// promise "a partial run is never committed as if complete" is kept by the
	// caller reading Stats.Reconciliation.State, not by this return value.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return stats, err
	}
	return stats, nil
}

// maybeDrain releases pooled parser memory only when the heap warrants it.
//
// An unconditional periodic drain measured 17% lower peak memory for 80% worse
// throughput on a 10,879-file corpus — it discards warm parser state constantly,
// including while memory is perfectly healthy. Sampling the heap first makes the
// cost proportional to the problem: on a repository that never grows large,
// nothing is ever drained and there is no cost at all.
//
// ReadMemStats stops the world briefly, so it runs once per DrainInterval files
// (or immediately after parsing a file exceeding LargeFileDrainThreshold) rather
// than after every file.
func (r *Runner) maybeDrain() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Compare in uint64 space. Converting HeapInuse to int64 would wrap on a
	// heap above 8 EiB — unreachable today, but a silent sign flip would turn
	// this guard permanently on, and the comparison is free either way.
	if r.drainThreshold < 0 || ms.HeapInuse < uint64(r.drainThreshold) {
		return
	}
	r.drainPools()
}

// drainPools releases pooled parser memory. Falls back to the library-only
// drain when the registry cannot drain its own Tagger pools, which is the case
// for a caller-supplied test registry.
func (r *Runner) drainPools() {
	if d, ok := r.parsers.(parse.Drainable); ok {
		d.DrainPools()
		return
	}
	parse.ReleasePooledMemory()
}

type outcome int

const (
	outcomeIndexed outcome = iota
	outcomeSkipped
	outcomeFailed
)

func (r *Runner) processFile(ctx context.Context, d fs.Discovered) (FileFacts, outcome) {
	facts := FileFacts{
		File: model.File{
			Path:        d.Path,
			Language:    d.Language,
			ContentHash: d.ContentHash,
			SizeBytes:   d.SizeBytes,
			ModTimeUnix: d.ModTimeUnix,
			ParseStatus: string(parse.StatusOK),
		},
	}

	// Discovery already decided to skip (binary, oversized, symlink). The file
	// is still recorded so the gap is visible in the index rather than looking
	// like it never existed.
	if d.Skipped {
		facts.File.ParseStatus = string(parse.StatusSkipped)
		facts.File.ParseError = d.SkipReason
		// A manifest the walk refused is the case the infra report must not
		// swallow: it reads as "no manifests here" and it is not. Recorded as
		// a problem so the index can say a deployment file exists and was not
		// understood, which is a different claim from its absence.
		if infra.Candidate(d.Path) {
			facts.Infra = infra.File{
				Path:     d.Path,
				Problems: []infra.Problem{{Reason: d.SkipReason}},
			}
			facts.HasInfra = true
		}
		return facts, outcomeSkipped
	}

	p := r.parsers.ForFile(d.Path)
	if p == nil {
		facts.File.ParseStatus = string(parse.StatusSkipped)
		facts.File.ParseError = "no grammar for this file type"
		// No grammar does not mean nothing to learn. Configuration files and
		// dependency manifests carry declarations that need text rather than a
		// syntax tree, and they are precisely the files no grammar claims —
		// returning here left `cortex config` reporting zero declared keys on
		// a repository with a .env in its root.
		//
		// Deployment manifests are the same case and then some: a Dockerfile
		// has no grammar by definition, and a compose file is YAML that no
		// parser here claims. Skipping them left every service, port and
		// configuration key out of the index, which is what `cortex infra`
		// meant when it printed NOT INDEXED on every run.
		if analyze.Wants(d.Path) || infra.Candidate(d.Path) {
			if src, err := os.ReadFile(filepath.Join(r.walker.Root(), filepath.FromSlash(d.Path))); err == nil {
				facts.Content = src
				if analyze.Wants(d.Path) {
					facts.Analysis = analyze.File(d.Path, src, nil)
				}
				facts.Infra, facts.HasInfra = infra.Parse(d.Path, src)
			}
		}
		return facts, outcomeSkipped
	}

	// Reserve memory before reading. Acquiring after the read would let every
	// worker buffer a large file simultaneously, which is the spike the budget
	// exists to prevent.
	weight, err := r.budget.acquire(ctx, d.SizeBytes)
	if err != nil {
		facts.File.ParseStatus = string(parse.StatusFailed)
		facts.File.ParseError = err.Error()
		return facts, outcomeFailed
	}
	defer r.budget.release(weight)

	src, err := os.ReadFile(filepath.Join(r.walker.Root(), filepath.FromSlash(d.Path)))
	if err != nil {
		facts.File.ParseStatus = string(parse.StatusFailed)
		facts.File.ParseError = "read: " + err.Error()
		return facts, outcomeFailed
	}
	facts.Content = src

	res, err := p.Parse(ctx, d.Path, src)
	if err != nil {
		// Only cancellation reaches here; parse problems come back in res.
		facts.File.ParseStatus = string(parse.StatusFailed)
		facts.File.ParseError = err.Error()
		return facts, outcomeFailed
	}

	facts.File.ParseStatus = string(res.Status)
	if res.Err != nil {
		facts.File.ParseError = res.Err.Error()
	}
	facts.Symbols = res.Symbols
	facts.References = res.References
	facts.Imports = res.Imports

	// API and database analysis runs on the SAME source the parser just read,
	// while it is still in hand. Doing it later would mean either re-reading
	// every file or persisting content, and the registry deliberately stores
	// neither. It is given the file's symbols so a statement can be attributed
	// to the definition that runs it.
	facts.Analysis = analyze.File(d.Path, src, res.Symbols)
	facts.Calls = api.ExtractCalls(d.Path, src)
	// A manifest that a grammar DOES claim — a .yaml when a YAML parser is
	// registered — must be read as a manifest too. Gating this on the absence
	// of a grammar would make deployment indexing depend on which parsers
	// happen to be compiled in, which is not a property of the repository.
	if infra.Candidate(d.Path) {
		facts.Infra, facts.HasInfra = infra.Parse(d.Path, src)
	}

	if res.Status == parse.StatusFailed {
		return facts, outcomeFailed
	}
	if res.Status == parse.StatusSkipped {
		return facts, outcomeSkipped
	}
	return facts, outcomeIndexed
}
