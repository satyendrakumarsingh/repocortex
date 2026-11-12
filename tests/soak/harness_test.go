//go:build soak

package soak

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acqfs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// Knobs. Each has an environment variable equivalent because CI configures
// these through a job matrix, where editing a command line per variant is far
// more error-prone than setting one variable.
var (
	flagDuration = flag.Duration("soak.duration", envDuration("REPOCORTEX_SOAK_DURATION", 30*time.Second),
		"how long EACH soak test runs; the suite takes roughly this times the number of tests")
	flagFiles = flag.Int("soak.files", envInt("REPOCORTEX_SOAK_FILES", 200),
		"size of the synthetic repository, in files")
	flagInterval = flag.Duration("soak.interval", envDuration("REPOCORTEX_SOAK_INTERVAL", 3*time.Millisecond),
		"pause between filesystem mutations; lower means more events per second")
	flagSeed = flag.Int64("soak.seed", envInt64("REPOCORTEX_SOAK_SEED", 1),
		"seed for the mutation stream, so a failing run can be replayed")
)

func envDuration(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(name); ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if v, ok := os.LookupEnv(name); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// budget is the wall-clock allowance for one test.
func budget() time.Duration {
	if *flagDuration > 0 {
		return *flagDuration
	}
	return 30 * time.Second
}

// ---------------------------------------------------------------------------
// Resource sampling
// ---------------------------------------------------------------------------

// Thresholds live together, with their reasoning, because the single thing
// that kills a soak suite is a threshold nobody can defend: it fires once on a
// loaded CI box, gets doubled without analysis, and by the third doubling it no
// longer detects anything.

// goroutineSlack is how far above baseline the goroutine count may sit at the
// end of a run before it is called a leak.
//
// Absolute, not proportional, and deliberately small. The leak this exists to
// catch — a watch, a timer or an event-loop goroutine not released when its
// directory disappears — grows once per churn cycle, and a soak run does
// thousands of cycles, so a real leak arrives in the hundreds or thousands and
// is nowhere near this line. What sits just above zero is the ordinary
// population caught mid-flight at sample time: database/sql's connection
// opener and cleaner, fsnotify's backend goroutine during a restart, and the
// testing package's own. 16 sits in the empty space between those two
// populations rather than splitting either.
// harnessProducer stamps the harness registry and is passed to pipeline.Diff as
// the running build. They must match: an empty or differing stamp makes Diff
// treat every file as dirty, which would defeat the convergence oracle.
const harnessProducer = "soak-harness"

const goroutineSlack = 16

// heapGrowthFactor and heapGrowthFloor gate the heap check, and BOTH must be
// exceeded for a failure.
//
// Either one alone is useless. A purely relative bound is meaningless at a
// small baseline: a 4 MiB baseline "doubles" the moment one parse buffer
// happens to be live when the sample is taken. A purely absolute bound is
// meaningless at a large one: 32 MiB of drift on a 1 GiB heap is noise, and on
// a 5 MiB heap it is a catastrophe. Requiring both means the check scales with
// the workload without firing on the arithmetic of small numbers.
const (
	heapGrowthFactor = 2.0
	heapGrowthFloor  = 32 << 20 // 32 MiB
)

// settleAttempts and settleInterval bound how long settle() waits for the
// runtime to go quiet. 40 x 25ms = 1s worst case, which is negligible against
// a sample interval measured in seconds but is comfortably longer than the
// scheduler needs to retire a batch of finished goroutines.
const (
	settleAttempts = 40
	settleInterval = 25 * time.Millisecond
)

// sample is one observation of the process's resource footprint.
type sample struct {
	elapsed    time.Duration
	goroutines int
	heapInuse  uint64
}

// settle drives the runtime into a comparable, quiescent state and then
// samples it.
//
// Both halves matter, for different reasons.
//
// Goroutines: a goroutine that has returned still counts until the scheduler
// retires it, so a sample taken immediately after a work burst reads high for
// reasons that have nothing to do with a leak. Polling until the count stops
// moving costs milliseconds and removes an entire class of false positive.
//
// Heap: Go's collector is lazy by design, so HeapInuse without a forced cycle
// measures how recently the GC ran, not how much memory is retained. Two
// cycles rather than one because the first frees objects that were only
// reachable from things the first cycle collected — the registry's row and
// symbol structures are exactly that shape — and a single cycle would credit
// their bytes to the wrong sample.
func settle() (goroutines int, heapInuse uint64) {
	prev, stable := -1, 0
	for i := 0; i < settleAttempts; i++ {
		n := runtime.NumGoroutine()
		if n == prev {
			stable++
			if stable >= 2 {
				break
			}
		} else {
			stable = 0
		}
		prev = n
		time.Sleep(settleInterval)
	}
	runtime.GC()
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return runtime.NumGoroutine(), ms.HeapInuse
}

// tracker collects samples over one soak run and turns them into a verdict
// plus a report a human can act on.
type tracker struct {
	name     string
	start    time.Time
	baseline sample
	samples  []sample
	notes    []string
}

// newTracker takes the baseline AFTER the system under test is warm.
//
// Baselining before startup would fold the watcher's own goroutines, the
// SQLite connection pool and the loaded tree-sitter grammar into the "growth"
// measurement, and every run would look like a leak.
func newTracker(name string) *tracker {
	g, h := settle()
	now := time.Now()
	return &tracker{
		name:     name,
		start:    now,
		baseline: sample{elapsed: 0, goroutines: g, heapInuse: h},
	}
}

// sample records the current footprint. Callers should space these evenly
// across the run: the trend analysis assumes the series describes the whole
// window, not a cluster at one end of it.
func (tr *tracker) sample() sample {
	g, h := settle()
	s := sample{elapsed: time.Since(tr.start), goroutines: g, heapInuse: h}
	tr.samples = append(tr.samples, s)
	return s
}

// notef attaches a workload fact — iterations, events, errors tolerated — to
// the report. Resource numbers are uninterpretable without them: 40 MiB of
// growth over 200 iterations and over 2,000,000 mean opposite things.
func (tr *tracker) notef(format string, args ...any) {
	tr.notes = append(tr.notes, fmt.Sprintf(format, args...))
}

func (tr *tracker) goroutineRange() (minG, maxG int) {
	minG, maxG = tr.baseline.goroutines, tr.baseline.goroutines
	for _, s := range tr.samples {
		if s.goroutines < minG {
			minG = s.goroutines
		}
		if s.goroutines > maxG {
			maxG = s.goroutines
		}
	}
	return minG, maxG
}

func (tr *tracker) heapPeak() uint64 {
	peak := tr.baseline.heapInuse
	for _, s := range tr.samples {
		if s.heapInuse > peak {
			peak = s.heapInuse
		}
	}
	return peak
}

func (tr *tracker) last() sample {
	if len(tr.samples) == 0 {
		return tr.baseline
	}
	return tr.samples[len(tr.samples)-1]
}

// heapSlope fits a least-squares line through the sampled heap series and
// returns its gradient in bytes per second, with the r^2 that says whether the
// gradient means anything.
//
// It is reported rather than asserted on. A slope is the right shape for
// "growth without bound" but a terrible gate: a single GC-timing outlier in a
// twelve-point series moves it enormously, and r^2 near zero — which is the
// normal, healthy case — makes the number meaningless anyway. The pass/fail
// decision lives in verdict(); this is the diagnostic that tells the reader
// whether a marginal result is drift or a trend.
func (tr *tracker) heapSlope() (bytesPerSec, r2 float64) {
	pts := append([]sample{tr.baseline}, tr.samples...)
	n := float64(len(pts))
	if n < 3 {
		return 0, 0
	}
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		x, y := p.elapsed.Seconds(), float64(p.heapInuse)
		sx, sy, sxx, sxy = sx+x, sy+y, sxx+x*x, sxy+x*y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, 0
	}
	slope := (n*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / n

	meanY := sy / n
	var ssTot, ssRes float64
	for _, p := range pts {
		x, y := p.elapsed.Seconds(), float64(p.heapInuse)
		pred := intercept + slope*x
		ssTot += (y - meanY) * (y - meanY)
		ssRes += (y - pred) * (y - pred)
	}
	if ssTot == 0 {
		return slope, 0
	}
	return slope, 1 - ssRes/ssTot
}

// verdict returns one string per problem found, empty when the run is clean.
//
// Every string carries the delta and the numbers behind it. The whole series
// is printed separately by report(), which callers must log regardless of
// outcome — the point of a soak run is the shape of the curve, and a bare
// "leaked" tells the next person nothing about whether it was a step or a
// ramp.
func (tr *tracker) verdict() []string {
	var problems []string
	final := tr.last()

	if delta := final.goroutines - tr.baseline.goroutines; delta > goroutineSlack {
		_, maxG := tr.goroutineRange()
		problems = append(problems, fmt.Sprintf(
			"goroutine leak: settled at %d, baseline %d (delta +%d, slack %d, peak %d during the run)",
			final.goroutines, tr.baseline.goroutines, delta, goroutineSlack, maxG))
	}

	// Retention, measured on the settled final sample rather than the peak.
	// Peak heap is a property of the workload's burst size; what a leak check
	// is asking is whether memory came back afterwards.
	if final.heapInuse > tr.baseline.heapInuse {
		grown := final.heapInuse - tr.baseline.heapInuse
		factor := float64(final.heapInuse) / float64(maxU64(tr.baseline.heapInuse, 1))
		if factor > heapGrowthFactor && grown > heapGrowthFloor {
			problems = append(problems, fmt.Sprintf(
				"heap grew without returning: %s -> %s (+%s, %.2fx; thresholds %.1fx AND +%s)",
				humanBytes(tr.baseline.heapInuse), humanBytes(final.heapInuse),
				humanBytes(grown), factor, heapGrowthFactor, humanBytes(heapGrowthFloor)))
		}
	}
	return problems
}

// report renders everything observed. Always log it, pass or fail.
func (tr *tracker) report() string {
	minG, maxG := tr.goroutineRange()
	final := tr.last()
	slope, r2 := tr.heapSlope()

	var b strings.Builder
	fmt.Fprintf(&b, "\nsoak report: %s\n", tr.name)
	fmt.Fprintf(&b, "  window       %s across %d samples\n",
		final.elapsed.Round(time.Second), len(tr.samples))
	for _, n := range tr.notes {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	fmt.Fprintf(&b, "  goroutines   baseline %d, min %d, max %d, final %d (%+d)\n",
		tr.baseline.goroutines, minG, maxG, final.goroutines,
		final.goroutines-tr.baseline.goroutines)
	fmt.Fprintf(&b, "  heap inuse   baseline %s, peak %s, final %s (%+s)\n",
		humanBytes(tr.baseline.heapInuse), humanBytes(tr.heapPeak()),
		humanBytes(final.heapInuse), signedBytes(final.heapInuse, tr.baseline.heapInuse))
	fmt.Fprintf(&b, "  heap trend   %s/s (r2 %.2f — near 0 means no trend, just churn)\n",
		signedRate(slope), r2)

	b.WriteString("  series      ")
	for i, s := range append([]sample{tr.baseline}, tr.samples...) {
		if i > 0 {
			b.WriteString(" |")
		}
		fmt.Fprintf(&b, " t=%s g=%d h=%s",
			s.elapsed.Round(time.Second), s.goroutines, humanBytes(s.heapInuse))
	}
	b.WriteString("\n")
	return b.String()
}

// finish logs the report and fails the test for each problem found.
func (tr *tracker) finish(t *testing.T) {
	t.Helper()
	t.Log(tr.report())
	for _, p := range tr.verdict() {
		t.Errorf("%s: %s", tr.name, p)
	}
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func signedBytes(now, base uint64) string {
	if now >= base {
		return "+" + humanBytes(now-base)
	}
	return "-" + humanBytes(base-now)
}

func signedRate(bytesPerSec float64) string {
	if bytesPerSec >= 0 {
		return "+" + humanBytes(uint64(bytesPerSec))
	}
	return "-" + humanBytes(uint64(-bytesPerSec))
}

// ---------------------------------------------------------------------------
// Synthetic repository
// ---------------------------------------------------------------------------

// pkgDirs is how many package directories the fixture spreads files over.
// More than one so renames can move a file between directories — the case that
// exercises the watcher's rename pairing and the walker's path handling — and
// few enough that the watched-directory count stays a small, checkable number.
const pkgDirs = 8

// versionRe extracts the fixture version embedded in a symbol name. See
// fixtureSource for why the version is carried in the name at all.
var versionRe = regexp.MustCompile(`V(\d+)$`)

// fixtureSource generates one file's contents.
//
// Every symbol name embeds the file's version number. That is the whole trick
// behind the torn-read check in the concurrency soak: a reader that lists
// symbols and sees V3 and V4 together in one file has caught a replacement
// that was not atomic, and it can say so without needing a consistent snapshot
// of any other table.
func fixtureSource(pkg string, id, version int) string {
	return fmt.Sprintf(`package %s

// Soak fixture %d, version %d. The version is part of every symbol name so a
// concurrent reader can prove it never saw two versions of one file at once.
type S%dV%d struct{}

func New%dV%d() *S%dV%d { return &S%dV%d{} }
`, pkg, id, version, id, version, id, version, id, version, id, version)
}

func pkgName(i int) string { return fmt.Sprintf("pkg%02d", i%pkgDirs) }
func filePath(dir, id int) string {
	return fmt.Sprintf("%s/f%04d.go", pkgName(dir), id)
}

// repo is a synthetic repository with a live index over it.
type repo struct {
	t    *testing.T
	root string

	db      *store.DB
	reg     *registry.Registry
	walker  *acqfs.Walker
	parsers parse.Registry
	runID   int64

	// stable are files the mutator never touches. They are the control group:
	// their symbols must be exactly right at every instant, which is how
	// collateral damage from a concurrent replace of some OTHER file gets
	// caught.
	stable []string
	// mutable are the files the mutator is free to edit, delete and rename.
	mutable []string
}

// newRepo writes a fixture tree, opens an on-disk index beside it, and runs a
// full index so there is a committed run to update incrementally.
//
// On disk, not in memory: WAL, the busy timeout and the eight-connection pool
// only exist for a file-backed store, and those are precisely the mechanisms
// the concurrency soak is meant to stress. An in-memory store would be a
// single connection and would prove nothing about lock contention.
func newRepo(t *testing.T, nFiles int) *repo {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	r := &repo{t: t, root: root, parsers: parse.NewRegistry()}

	// The first quarter of the tree is the never-mutated control group.
	stableCount := nFiles / 4
	for id := 0; id < nFiles; id++ {
		rel := filePath(id, id)
		r.writeFile(rel, fixtureSource(pkgName(id), id, 0))
		if id < stableCount {
			r.stable = append(r.stable, rel)
		} else {
			r.mutable = append(r.mutable, rel)
		}
	}

	// The index lives inside the repository, exactly as `repocortex init` puts
	// it. config.Defaults excludes .repocortex from discovery and watch's
	// builtinIgnoredDirs refuses to watch it, so this placement also asserts
	// that the index's own WAL writes do not feed back in as changes to index.
	dbPath := filepath.Join(root, ".repocortex", "index.db")
	db, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r.db = db
	r.reg = registry.New(db, registry.WithProducerVersion(harnessProducer))

	walker, err := acqfs.NewWalker(root, config.Defaults())
	if err != nil {
		t.Fatalf("new walker: %v", err)
	}
	r.walker = walker

	if _, err := r.fullIndex(ctx); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	return r
}

// fullIndex builds a fresh staging run over the current tree and swaps it in,
// leaving r.runID pointing at the newly committed run.
func (r *repo) fullIndex(ctx context.Context) (int64, error) {
	run, err := r.reg.BeginRun(ctx, "fast", "")
	if err != nil {
		return 0, fmt.Errorf("begin run: %w", err)
	}
	w, err := r.reg.NewWriter(ctx, run.ID)
	if err != nil {
		return 0, fmt.Errorf("new writer: %w", err)
	}
	sink := pipeline.NewRegistrySink(w)
	// One shared parser registry for the whole soak, not one per run. A fresh
	// registry loads the grammar again, and grammar allocations accumulating
	// across iterations would read as a leak in the very test that exists to
	// detect leaks. A long-lived watcher shares one registry too, so this also
	// matches production.
	if _, err := pipeline.NewRunner(r.walker, pipeline.Options{Parsers: r.parsers}).Run(ctx, sink); err != nil {
		_ = w.Close()
		return 0, fmt.Errorf("run: %w", err)
	}
	if err := sink.Flush(ctx); err != nil {
		_ = w.Close()
		return 0, fmt.Errorf("flush: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("close writer: %w", err)
	}
	if err := r.reg.CommitRun(ctx, run.ID); err != nil {
		return 0, fmt.Errorf("commit run: %w", err)
	}
	r.runID = run.ID
	return run.ID, nil
}

func (r *repo) abs(rel string) string {
	return filepath.Join(r.root, filepath.FromSlash(rel))
}

func (r *repo) writeFile(rel, body string) {
	r.t.Helper()
	abs := r.abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

// applyChange re-indexes exactly one path.
//
// One call per change rather than one per batch, because Incremental aborts
// the whole slice on the first error and a soak deliberately races the
// filesystem: a file can be deleted between the event that announced it and
// the moment we go to read it. That is normal churn, not a fault, so a
// not-exist error is absorbed here and counted. Any other error is real and is
// returned.
func (r *repo) applyChange(ctx context.Context, ch pipeline.FileChange) (raced bool, err error) {
	_, err = pipeline.Incremental(ctx, r.reg, r.runID, r.walker,
		[]pipeline.FileChange{ch}, pipeline.IncrementalOptions{Parsers: r.parsers})
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	default:
		return false, err
	}
}

// reconcile is the full walk-and-diff path that cli/watch.go falls back to.
func (r *repo) reconcile(ctx context.Context) (pipeline.IncrementalStats, error) {
	changes, err := pipeline.Diff(ctx, r.reg, r.runID, r.walker, harnessProducer)
	if err != nil {
		return pipeline.IncrementalStats{}, err
	}
	return pipeline.Incremental(ctx, r.reg, r.runID, r.walker, changes,
		pipeline.IncrementalOptions{Parsers: r.parsers})
}

// divergence lists the paths where the index and the filesystem disagree.
//
// This is the convergence oracle. Intermediate states are allowed to lag — the
// index trails the tree by at least a debounce interval by construction — but
// once the tree stops moving the index must catch up exactly. Anything left
// here after quiesce is an event that was lost permanently rather than late.
func (r *repo) divergence(ctx context.Context) ([]pipeline.FileChange, error) {
	changes, err := pipeline.Diff(ctx, r.reg, r.runID, r.walker, harnessProducer)
	if err != nil {
		return nil, err
	}
	var diverged []pipeline.FileChange
	for _, c := range changes {
		if c.Kind != pipeline.ChangeSame {
			diverged = append(diverged, c)
		}
	}
	return diverged, nil
}

// integrityProblems reports referential damage accumulated in the store.
//
// This is the invariant gate `repocortex doctor --verify-index` enforces, run
// against SQL directly because the Go read API cannot see the damage:
// Registry.Symbols JOINs files, so a symbol whose file row is gone is filtered
// out of the very query a reader would use to notice it. The corruption is
// real and invisible at the same time, which is exactly why it needs its own
// check here.
//
// Each query below corresponds to a cascade the schema promises. A non-zero
// count means the promise did not hold — and because the damage is cumulative,
// a soak run is where it becomes large enough to be unmistakable.
func (r *repo) integrityProblems(ctx context.Context) []string {
	checks := []struct {
		what  string
		query string
	}{
		{
			"symbols whose file row no longer exists (a reader asked \"where is this defined?\" would be sent to a path that is not there)",
			`SELECT count(*) FROM symbols s LEFT JOIN files f ON f.id = s.file_id WHERE f.id IS NULL`,
		},
		{
			"edges whose src symbol no longer exists",
			`SELECT count(*) FROM edges e LEFT JOIN symbols s ON s.id = e.src_id WHERE s.id IS NULL`,
		},
		{
			"edges whose dst symbol no longer exists",
			`SELECT count(*) FROM edges e LEFT JOIN symbols s ON s.id = e.dst_id WHERE s.id IS NULL`,
		},
		{
			"files_fts_map rows whose file row no longer exists (these collide with a recycled FTS rowid and make the next ReplaceFile fail outright)",
			`SELECT count(*) FROM files_fts_map m LEFT JOIN files f ON f.id = m.file_id WHERE f.id IS NULL`,
		},
	}

	var problems []string
	for _, c := range checks {
		var n int64
		if err := r.db.QueryRowContext(ctx, c.query).Scan(&n); err != nil {
			problems = append(problems, fmt.Sprintf("integrity check %q failed to run: %v", c.what, err))
			continue
		}
		if n > 0 {
			problems = append(problems, fmt.Sprintf("%d %s", n, c.what))
		}
	}
	if len(problems) > 0 {
		problems = append(problems, r.cascadeDiagnosis(ctx))
	}
	return problems
}

// cascadeDiagnosis samples PRAGMA foreign_keys across the connection pool.
//
// Every orphan above depends on ON DELETE CASCADE firing, and in SQLite
// foreign key enforcement is a per-CONNECTION setting. Reporting the actual
// per-connection state alongside the orphan counts turns "the index is
// corrupt" into a diagnosis, which is the difference between a soak failure
// somebody fixes and one somebody re-runs until it passes.
func (r *repo) cascadeDiagnosis(ctx context.Context) string {
	const probes = 8
	states := make([]string, 0, probes)
	conns := make([]*sql.Conn, 0, probes)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	// Hold each connection open while probing the next, so the pool is forced
	// to hand out distinct connections rather than the same one repeatedly.
	for i := 0; i < probes; i++ {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			break
		}
		conns = append(conns, conn)
		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			states = append(states, "?")
			continue
		}
		states = append(states, strconv.Itoa(fk))
	}
	return fmt.Sprintf(
		"DIAGNOSIS: `PRAGMA foreign_keys` across %d pooled connections reads [%s]. "+
			"Foreign key enforcement is per-connection in SQLite, so any 0 here means ON DELETE CASCADE "+
			"silently does not fire on that connection and the orphans above are the result. "+
			"Note that store.applyPragmas verifies the pragma on whichever single connection served its "+
			"own Exec, so the existing check cannot detect this",
		len(states), strings.Join(states, " "))
}

// ---------------------------------------------------------------------------
// Mutation stream
// ---------------------------------------------------------------------------

// counters are the workload totals reported at the end of a run.
type counters struct {
	created, modified, deleted, renamed atomic.Int64
	batches, events                     atomic.Int64
	raced, overflows                    atomic.Int64
}

// mutator drives a deterministic pseudo-random stream of edits against the
// fixture tree. Deterministic because a soak failure that cannot be replayed
// with -soak.seed is a rumour, not a bug report.
//
// It is driven by a single goroutine and holds no locks.
type mutator struct {
	r   *repo
	rnd *rand.Rand
	c   *counters

	live    []string       // mutable files that currently exist
	version map[string]int // rel path -> fixture version
	nextID  int            // ids for newly created files, above the fixture range
}

func newMutator(r *repo, c *counters, seed int64) *mutator {
	m := &mutator{
		r:       r,
		rnd:     rand.New(rand.NewPCG(uint64(seed), 0x9E3779B97F4A7C15)),
		c:       c,
		live:    append([]string(nil), r.mutable...),
		version: make(map[string]int, len(r.mutable)),
		nextID:  len(r.stable) + len(r.mutable),
	}
	for _, rel := range r.mutable {
		m.version[rel] = 0
	}
	return m
}

// idOf recovers the numeric id embedded in a fixture path.
func idOf(rel string) int {
	base := filepath.Base(rel)
	base = strings.TrimSuffix(strings.TrimPrefix(base, "f"), ".go")
	n, err := strconv.Atoi(base)
	if err != nil {
		return 0
	}
	return n
}

// step performs one mutation, weighted to look like a working developer with
// their thumb on the fast-forward button: mostly saves, with creates, deletes
// and moves mixed in, plus the two pathological bursts that break naive
// watchers — a save storm on one file and a rename storm across many.
func (m *mutator) step() {
	// Keep the population inside a band so the tree neither empties nor grows
	// without limit over an hour of churn.
	switch {
	case len(m.live) < len(m.r.mutable)/2:
		m.create()
		return
	case len(m.live) > len(m.r.mutable)*2:
		m.delete()
		return
	}

	switch n := m.rnd.IntN(100); {
	case n < 45:
		m.modify()
	case n < 60:
		m.create()
	case n < 75:
		m.delete()
	case n < 90:
		m.rename()
	case n < 95:
		m.saveBurst()
	default:
		m.renameStorm()
	}
}

func (m *mutator) pick() (int, string, bool) {
	if len(m.live) == 0 {
		return 0, "", false
	}
	i := m.rnd.IntN(len(m.live))
	return i, m.live[i], true
}

func (m *mutator) modify() {
	_, rel, ok := m.pick()
	if !ok {
		return
	}
	m.rewrite(rel)
	m.c.modified.Add(1)
}

func (m *mutator) rewrite(rel string) {
	v := m.version[rel] + 1
	m.version[rel] = v
	m.r.writeFile(rel, fixtureSource(filepath.Base(filepath.Dir(rel)), idOf(rel), v))
}

func (m *mutator) create() {
	id := m.nextID
	m.nextID++
	rel := filePath(m.rnd.IntN(pkgDirs), id)
	m.r.writeFile(rel, fixtureSource(filepath.Base(filepath.Dir(rel)), id, 1))
	m.version[rel] = 1
	m.live = append(m.live, rel)
	m.c.created.Add(1)
}

func (m *mutator) delete() {
	i, rel, ok := m.pick()
	if !ok {
		return
	}
	if err := os.Remove(m.r.abs(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.r.t.Errorf("remove %s: %v", rel, err)
		return
	}
	m.live = append(m.live[:i], m.live[i+1:]...)
	delete(m.version, rel)
	m.c.deleted.Add(1)
}

// rename moves a file between two existing package directories, keeping its
// basename. Same basename because that is the case the watcher's rename
// pairing recognises, and existing directories because directory churn is
// tested separately — mixing the two would make a convergence failure
// ambiguous between "lost a file event" and "lost a directory event".
func (m *mutator) rename() {
	i, from, ok := m.pick()
	if !ok {
		return
	}
	id := idOf(from)
	to := filePath(m.rnd.IntN(pkgDirs), id)
	if to == from {
		return
	}
	dst := m.r.abs(to)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		m.r.t.Errorf("mkdir for %s: %v", to, err)
		return
	}
	if err := os.Rename(m.r.abs(from), dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		m.r.t.Errorf("rename %s -> %s: %v", from, to, err)
		return
	}
	m.live[i] = to
	m.version[to] = m.version[from]
	delete(m.version, from)
	m.c.renamed.Add(1)
}

// saveBurst is what one editor save actually looks like multiplied by ten: a
// rapid sequence of writes to a single path with no pause between them. It
// must collapse into a small number of re-indexes, not ten.
func (m *mutator) saveBurst() {
	_, rel, ok := m.pick()
	if !ok {
		return
	}
	for i := 0; i < 10; i++ {
		m.rewrite(rel)
	}
	m.c.modified.Add(10)
}

// renameStorm moves several files at once, which is what `git checkout` of a
// refactoring branch looks like. It is the case where rename pairing is most
// likely to cross-wire two unrelated moves.
func (m *mutator) renameStorm() {
	for i := 0; i < 6; i++ {
		m.rename()
	}
}

// runUntil drives the mutation stream until the deadline or until stop closes.
func (m *mutator) runUntil(deadline time.Time, stop <-chan struct{}) {
	for time.Now().Before(deadline) {
		select {
		case <-stop:
			return
		default:
		}
		m.step()
		if *flagInterval > 0 {
			time.Sleep(*flagInterval)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared assertions
// ---------------------------------------------------------------------------

// lockErrorRe matches the ways SQLite contention surfaces through
// modernc.org/sqlite and database/sql.
//
// The invariant under test is that the store's busy timeout and the registry's
// bounded write batches absorb contention entirely. If any of these reaches a
// caller, indexing under a concurrent reader is not merely slow, it fails —
// and it would fail intermittently, on a user's machine, under load.
var lockErrorRe = regexp.MustCompile(`(?i)database is locked|database table is locked|SQLITE_BUSY|SQLITE_LOCKED|busy`)

func isLockError(err error) bool {
	return err != nil && lockErrorRe.MatchString(err.Error())
}
