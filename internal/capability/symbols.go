package capability

import (
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// Symbol is the hydrated view of a symbol a capability report needs.
//
// Same fields as registry.SymbolIndexEntry, restated here so the capability
// layer's public types do not leak the registry's resolution-time vocabulary
// into MCP output. It is deliberately narrow for the same reason the registry's
// is: the whole table is held in memory, so every field costs ~100 kB per
// 1,000 symbols and a 100k-symbol repository is the design point.
type Symbol struct {
	ID       int64  `json:"symbol_id"`
	Name     string `json:"name"`
	FQN      string `json:"fqn"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
	Exported bool   `json:"exported"`
}

// Dir returns the symbol's directory, used for the co-location signal.
func (s Symbol) Dir() string { return path.Dir(s.FilePath) }

// SymbolIndex is the whole symbol table, indexed for the four lookups the
// capabilities perform.
//
// Why hold it all: an impact report that returns bare symbol ids is unusable,
// and hydrating a bounded result set one id at a time is one query per row.
// The registry already loads exactly this shape during resolution and measured
// it at roughly 10 MB per 100,000 symbols, so the memory is known, not hoped.
//
// A SymbolIndex is immutable once built and safe for concurrent readers.
type SymbolIndex struct {
	runID int64

	byID   map[int64]Symbol
	byFQN  map[string]int64
	byName map[string][]int64
	byDir  map[string][]int64

	// names is the sorted, de-duplicated key set of byName. Prefix queries
	// ("everything called TestUpdateStatus*") binary-search it instead of
	// scanning 100k map keys per query — the naming signal issues several
	// prefix probes per call, so a linear scan would be the dominant cost of
	// test discovery.
	names []string
}

func buildSymbolIndex(runID int64, byName map[string][]registry.SymbolIndexEntry) *SymbolIndex {
	idx := &SymbolIndex{
		runID:  runID,
		byID:   make(map[int64]Symbol, len(byName)*2),
		byFQN:  make(map[string]int64, len(byName)*2),
		byName: make(map[string][]int64, len(byName)),
		byDir:  make(map[string][]int64, 256),
		names:  make([]string, 0, len(byName)),
	}
	for name, entries := range byName {
		ids := make([]int64, 0, len(entries))
		for _, e := range entries {
			s := Symbol{
				ID: e.ID, Name: e.Name, FQN: e.FQN, Kind: e.Kind,
				FilePath: e.FilePath, Exported: e.Exported,
			}
			idx.byID[s.ID] = s
			// First writer wins. FQN is unique per repository by contract, but
			// the schema only enforces uniqueness per (run, fqn, file, line),
			// so a duplicate is possible and must not panic or reorder
			// non-deterministically.
			if _, seen := idx.byFQN[s.FQN]; !seen {
				idx.byFQN[s.FQN] = s.ID
			}
			ids = append(ids, s.ID)
			dir := s.Dir()
			idx.byDir[dir] = append(idx.byDir[dir], s.ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		idx.byName[name] = ids
		idx.names = append(idx.names, name)
	}
	sort.Strings(idx.names)
	for dir := range idx.byDir {
		ids := idx.byDir[dir]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return idx
}

// Len reports how many symbols the run contains. Reports quote it so a reader
// can judge the denominator behind "N callers found".
func (idx *SymbolIndex) Len() int { return len(idx.byID) }

// RunID is the run this index was built from.
func (idx *SymbolIndex) RunID() int64 { return idx.runID }

// Lookup hydrates one symbol id.
func (idx *SymbolIndex) Lookup(id int64) (Symbol, bool) {
	s, ok := idx.byID[id]
	return s, ok
}

// ByFQN finds a symbol by fully-qualified name.
func (idx *SymbolIndex) ByFQN(fqn string) (Symbol, bool) {
	id, ok := idx.byFQN[fqn]
	if !ok {
		return Symbol{}, false
	}
	return idx.byID[id], true
}

// ByName returns every symbol with exactly this name, ordered by id.
func (idx *SymbolIndex) ByName(name string) []Symbol {
	return idx.hydrate(idx.byName[name])
}

// InDir returns every symbol defined in files directly under dir.
func (idx *SymbolIndex) InDir(dir string) []Symbol {
	return idx.hydrate(idx.byDir[dir])
}

// WithPrefix returns symbols whose NAME begins with prefix and whose remainder
// satisfies accept.
//
// The accept callback exists because a bare prefix match is too loose to be
// honest: searching "TestUpdate" for target `Update` would also claim
// `TestUpdateStatus` as a test of `Update`. Callers therefore constrain what
// may follow the prefix (see conventionalSuffix in testfiles.go).
func (idx *SymbolIndex) WithPrefix(prefix string, accept func(rest string) bool) []Symbol {
	if prefix == "" {
		return nil
	}
	start := sort.SearchStrings(idx.names, prefix)
	var ids []int64
	for i := start; i < len(idx.names); i++ {
		name := idx.names[i]
		if !strings.HasPrefix(name, prefix) {
			break
		}
		if accept != nil && !accept(name[len(prefix):]) {
			continue
		}
		ids = append(ids, idx.byName[name]...)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return idx.hydrate(ids)
}

func (idx *SymbolIndex) hydrate(ids []int64) []Symbol {
	if len(ids) == 0 {
		return nil
	}
	out := make([]Symbol, 0, len(ids))
	for _, id := range ids {
		if s, ok := idx.byID[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// symbolCache memoises the per-run facts every report needs, keyed by run id.
//
// Exactly one run is kept rather than a map of runs. A RepoCortex process reads
// the committed run; when a new one is committed the old one's rows are
// deleted, so caching more than the current run caches garbage. Holding one
// also bounds memory at "one symbol table", which is the number the design
// budgeted for.
//
// Caching is sound because a run's contents are immutable once committed:
// registry.CommitRun publishes a finished run and never mutates it, and the
// next indexing pass gets a new id. Keying on the run id is therefore both the
// cache key and the invalidation.
type symbolCache struct {
	mu    sync.Mutex
	runID int64
	idx   *SymbolIndex

	statsRun int64
	stats    registry.ResolutionStats
	statsOK  bool
}

func newSymbolCache() *symbolCache { return &symbolCache{} }

// get returns the cached index for runID, calling load exactly once per run.
//
// The lock is held across load deliberately. The alternative — release, load,
// re-acquire — lets N concurrent MCP calls each build a 100k-entry index on a
// cold cache, which is the precise stampede this cache exists to prevent. Load
// is a bounded read of one table, not an unbounded operation, so serialising it
// trades a little concurrency for a bounded memory ceiling.
func (c *symbolCache) get(runID int64, load func() (*SymbolIndex, error)) (*SymbolIndex, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idx != nil && c.runID == runID {
		return c.idx, nil
	}
	idx, err := load()
	if err != nil {
		return nil, err
	}
	c.runID, c.idx = runID, idx
	return idx, nil
}

// getStats memoises the run's resolution outcomes.
//
// Worth caching on its own: ResolutionStats is a GROUP BY over every reference
// row in the run, and a real repository has half a million of them. Measured on
// the 100k-symbol fixture (45,600 reference rows) it cost 7.1ms — which was
// 95% of a small impact query's total time, paid again on every call to
// recompute a number that cannot have changed.
func (c *symbolCache) getStats(runID int64, load func() (registry.ResolutionStats, error)) (registry.ResolutionStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statsOK && c.statsRun == runID {
		return c.stats, nil
	}
	stats, err := load()
	if err != nil {
		return registry.ResolutionStats{}, err
	}
	c.statsRun, c.stats, c.statsOK = runID, stats, true
	return stats, nil
}
