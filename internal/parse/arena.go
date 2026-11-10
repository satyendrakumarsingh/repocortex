package parse

import (
	ts "github.com/odvcencio/gotreesitter"
)

// ReleasePooledMemory returns cached parser arenas to the garbage collector.
//
// THIS IS NOT OPTIONAL FOR BATCH INDEXING. gotreesitter pools the arenas it
// allocates for parse trees, and its own documentation is explicit:
//
//	"Arenas held in the pool are strong Go references and are not collected
//	 by the GC until explicitly drained or the process exits."
//
// Indexing a repository is exactly the batch scan that warning describes. Left
// undrained, the arena for every file ever parsed stays reachable, so peak
// memory becomes the sum of every parse rather than the largest few — and a
// single directory of generated files pins hundreds of megabytes for the life
// of the process.
//
// Measured on a 2,110-file Go corpus before this call existed: 6.7 GB peak heap
// with a 2 MiB parse cap, of which the great majority was pooled arenas that no
// live object referenced.
//
// Safe to call concurrently with active parsing: a pool holds arenas that have
// been RETURNED, while a parser in progress owns its arena outside the pool.
// Draining costs the next parse a fresh allocation, which is why callers drain
// periodically rather than after every file.
func ReleasePooledMemory() {
	ts.DrainArenaPools()
}

// DrainPools releases both gotreesitter's arena pools and RepoCortex's own
// Tagger pools.
//
// The second half matters more than the first. A pooled Tagger owns a live
// Parser, and its arena is therefore NOT in gotreesitter's pool and NOT
// reachable by DrainArenaPools. Profiling a 10,896-file run showed 3 GB of live
// arenas held this way. Draining only the library's pools leaves that memory
// pinned for as long as the process runs.
func (r *registry) DrainPools() {
	r.mu.RLock()
	parsers := make([]*genericParser, 0, len(r.parsers))
	for _, p := range r.parsers {
		parsers = append(parsers, p)
	}
	r.mu.RUnlock()

	for _, p := range parsers {
		p.DrainPool()
	}
	ts.DrainArenaPools()
}

// Drainable is implemented by registries that can release pooled memory.
// The pipeline uses it to keep peak memory a function of concurrency rather
// than repository size.
type Drainable interface{ DrainPools() }

// DrainInterval is how many files an indexing run parses between drains.
//
// A trade-off with no universally right answer: drain too often and every parse
// re-allocates its arena; drain too rarely and the pool grows without bound.
// 128 keeps the pool small enough that peak memory tracks concurrency rather
// than repository size, while amortising re-allocation across enough files that
// the cost does not show up in wall-clock time.
const DrainInterval = 128
