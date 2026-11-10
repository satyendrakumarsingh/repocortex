package watch

import (
	"sync"
	"time"
)

// Clock is the package's only source of time.
//
// Debouncing is the one thing in this package that is genuinely time-dependent,
// and a test that proves debouncing works by sleeping proves very little: it
// passes on a fast laptop and flakes on a loaded CI runner. Every timer the
// watcher arms goes through this interface, so tests drive the debounce window
// forward instantly and deterministically with ManualClock. Nothing in the
// package calls time.Now, time.After or time.NewTimer directly.
type Clock interface {
	Now() time.Time

	// NewTimer returns a timer that fires once after d. The watcher creates a
	// fresh timer every time it re-arms rather than calling Reset, because a
	// timer's creation is then an observable event that ManualClock.BlockUntil
	// can synchronise on. Re-arming happens at most once per filesystem event,
	// so the allocation is not on any hot path.
	NewTimer(d time.Duration) Timer
}

// Timer is one pending deadline.
type Timer interface {
	// C returns the channel the timer fires on. It must be safe to select on
	// after Stop.
	C() <-chan time.Time

	// Stop prevents the timer from firing. It reports whether the timer was
	// still pending. Stopping an already-stopped timer must not panic.
	Stop() bool
}

// RealClock is the production Clock, backed by the time package.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer implements Clock.
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }

// Stop needs no channel drain. Since Go 1.23 a timer's channel is unbuffered
// and Stop guarantees no stale value can be observed afterwards, so the
// pre-1.23 "stop, then drain if !stopped" dance would now itself block. The
// module requires Go 1.25.
func (r *realTimer) Stop() bool { return r.t.Stop() }

// ManualClock is a Clock whose time only moves when a test says so.
//
// It is exported deliberately: a consumer of this package (the incremental
// re-index loop) has the same "assert on debounced output without sleeping"
// problem, and duplicating a fake clock per package is how the two drift apart.
//
// All methods are safe for concurrent use, which matters because the watcher's
// event loop arms timers from its own goroutine while the test advances time
// from another.
type ManualClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	created int
	pending []*manualTimer
}

// NewManualClock returns a clock reading start.
func NewManualClock(start time.Time) *ManualClock {
	c := &ManualClock{now: start}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Now implements Clock.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer implements Clock.
func (c *ManualClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &manualTimer{
		clock: c,
		// Buffered so firing never blocks whoever advances the clock. The
		// watcher may be busy elsewhere when its deadline passes; the tick must
		// still be delivered, exactly as time.Timer does it.
		ch:       make(chan time.Time, 1),
		deadline: c.now.Add(d),
	}
	c.created++
	c.cond.Broadcast()

	// A non-positive duration fires at once, matching time.NewTimer. This is
	// not a nicety: the watcher re-arms with the remaining time to an existing
	// deadline, so an event arriving just after that deadline passed produces
	// NewTimer(0). Queueing it for the next Advance instead would strand a
	// batch that is already due.
	if !t.deadline.After(c.now) {
		t.fireLocked(c.now)
		return t
	}
	c.pending = append(c.pending, t)
	return t
}

// Advance moves time forward and fires every timer whose deadline has passed.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
	kept := c.pending[:0]
	for _, t := range c.pending {
		switch {
		case t.stopped:
			// dropped
		case !t.deadline.After(c.now):
			// Firing under the lock cannot block: the channel has capacity 1
			// and a timer fires at most once.
			t.fireLocked(c.now)
		default:
			kept = append(kept, t)
		}
	}
	c.pending = kept
}

// BlockUntil waits until n timers have been created on this clock in total.
//
// This is the synchronisation point that makes watcher-level tests deterministic
// without sleeping. A test feeds an event to the watcher, waits here for the
// event loop to have re-armed its debounce timer, and only then advances time —
// so time never moves past a deadline that has not been set yet.
func (c *ManualClock) BlockUntil(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.created < n {
		c.cond.Wait()
	}
}

// Created reports how many timers have been created on this clock.
func (c *ManualClock) Created() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created
}

// manualTimer's mutable state is guarded by its clock's mutex rather than one
// of its own. The watcher stops timers from its event loop while a test
// advances the clock from another goroutine, so the two must be ordered against
// each other; two independent locks would only make that race harder to see.
type manualTimer struct {
	clock    *ManualClock
	ch       chan time.Time
	deadline time.Time
	stopped  bool
	fired    bool
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

// fireLocked must be called with the clock's mutex held.
func (t *manualTimer) fireLocked(now time.Time) {
	if t.stopped || t.fired {
		return
	}
	t.fired = true
	t.ch <- now
}
