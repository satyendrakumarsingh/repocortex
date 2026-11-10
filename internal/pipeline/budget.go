package pipeline

import (
	"context"
	"sync"
)

// memBudget bounds how many source bytes are being parsed at once.
//
// WHY A WORKER COUNT IS NOT ENOUGH, measured on a 2,110-file Go corpus:
//
//	parse cap   files   peak heap
//	   64 KB     1807      962 MB
//	  256 KB     2088    2,260 MB
//	    2 MB     2110    6,667 MB
//
// Twenty-two additional files cost 4.4 GB. Tree-sitter parse trees run one to
// two orders of magnitude larger than their source, so a fixed worker count
// bounds concurrency but not memory: eight workers that each happen to pick up
// a megabyte of generated code will allocate gigabytes between them, and the
// scheduler decides when that happens. Peak memory becomes a property of file
// ordering, which is not something a tool should leave to chance.
//
// So workers acquire a weight proportional to the bytes they are about to
// parse. Small files run wide; a large file waits until the budget frees up
// and then effectively runs alone. Throughput on ordinary code is unchanged
// because ordinary files are small.
//
// A file larger than the whole budget is admitted alone rather than deadlocking
// — the alternative is a file that can never be indexed, which is worse than a
// brief spike.
type memBudget struct {
	mu        sync.Mutex
	cond      *sync.Cond
	available int64
	total     int64
}

func newMemBudget(total int64) *memBudget {
	if total <= 0 {
		total = DefaultInFlightBytes
	}
	b := &memBudget{available: total, total: total}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until n bytes of budget are free, or ctx is done.
//
// Returns the weight actually taken, which must be passed to release. It may
// differ from n when n exceeds the total budget.
func (b *memBudget) acquire(ctx context.Context, n int64) (int64, error) {
	if n <= 0 {
		n = 1
	}
	// Oversized files are clamped to the full budget so they run exclusively
	// instead of waiting for an amount that can never become available.
	if n > b.total {
		n = b.total
	}

	// Wake the waiter if the context is cancelled while it is parked on the
	// condition variable, which has no select-based wait of its own.
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.cond.Broadcast()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.available < n {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		b.cond.Wait()
	}
	b.available -= n
	return n, nil
}

// release returns weight to the budget.
func (b *memBudget) release(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.available += n
	if b.available > b.total {
		// Defensive: a release without a matching acquire is a bug, but
		// silently growing the budget would turn it into an unbounded one.
		b.available = b.total
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}
