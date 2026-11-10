package context

import (
	stdctx "context"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// catalogueCache reuses a built catalogue across Assemble calls.
//
// WHY THIS EXISTS. Measured on a synthetic run:
//
//	10,000 symbols   catalogue 55.6 ms of 93.7 ms total   (59%)
//	94,000 symbols   catalogue dominates 1.34 s total
//
// The catalogue is derived data — two table scans plus enclosure resolution and
// index building — and it is identical for every query against the same run
// content. Rebuilding it per request means an agent asking three questions in a
// session pays for it three times, and the cost grows with the repository
// rather than with the question.
//
// WHY IT IS SAFE TO SHARE. A catalogue is immutable once built: nothing outside
// loadCatalogue writes to it, and every consumer only reads its maps. Go maps
// are safe for concurrent reads, so one instance can serve many in-flight MCP
// requests.
//
// WHY IT IS KEYED ON A FINGERPRINT AND NOT A RUN ID. A committed run is NOT
// immutable — incremental indexing updates it in place, so the same id can mean
// different content minute to minute while a developer edits. Keying on the id
// alone would serve pre-edit answers forever, which is the precise staleness
// this product exists to remove. registry.CatalogueFingerprint costs four
// indexed aggregates and is checked on every call.
//
// WHY THE REGISTRY IDENTITY IS PART OF THE KEY. A fingerprint describes CONTENT
// within one database; it does not identify the database. Two registries opened
// on different projects can hold runs with the same id and the same counts, and
// therefore the same fingerprint, while containing entirely different code. A
// content-only key would serve one project's catalogue for another project's
// query.
//
// This was not hypothetical: keying on content alone broke two tests
// immediately, because every test in this package builds its own in-memory
// database and several use fixtures of identical shape. The same collision
// would hit any process holding two projects open.
//
// A single entry is deliberate. One server serves one index; an LRU would add
// eviction policy and bookkeeping to solve a problem nobody has. Switching
// registries simply misses and rebuilds, which is correct if not optimal.
type catalogueCache struct {
	mu sync.Mutex

	// reg identifies WHICH database the cached catalogue came from. Compared by
	// pointer: two Registry values are the same store only if they are the same
	// object.
	reg    Registry
	fp     registry.Fingerprint
	cat    *catalogue
	loaded bool

	// hits and misses are for the benchmark and for `doctor`, so the cache's
	// effectiveness is observable rather than assumed.
	hits, misses int64
}

// get returns a catalogue for runID, building one only when the run's content
// has actually changed.
//
// The lock is held across the load so concurrent first-requests do not each
// build their own copy. They would otherwise all miss, all build, and all but
// one throw the result away — the thundering herd that makes a cache look
// useless under exactly the concurrency it was added for.
func (c *catalogueCache) get(
	ctx stdctx.Context,
	reg Registry,
	runID int64,
) (*catalogue, error) {
	fp, err := reg.CatalogueFingerprint(ctx, runID)
	if err != nil {
		// A fingerprint failure must not fail the request: fall back to
		// building fresh, which is always correct, merely slower.
		cat, lerr := loadCatalogue(ctx, reg, runID)
		return cat, lerr
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.loaded && c.reg == reg && c.fp == fp {
		c.hits++
		return c.cat, nil
	}

	cat, err := loadCatalogue(ctx, reg, runID)
	if err != nil {
		return nil, err
	}
	c.reg, c.fp, c.cat, c.loaded = reg, fp, cat, true
	c.misses++
	return cat, nil
}

// stats reports hit/miss counts.
func (c *catalogueCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}
