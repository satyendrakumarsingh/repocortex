package watch

import (
	"fmt"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// eventQueueSize is how many filesystem events may be in flight between the
// backend and the debouncer.
//
// It exists because the event loop cannot always read immediately: when a
// directory appears it calls dirSet.addTree, which registers a watch and reads
// the directory — filesystem work that takes as long as it takes. During that
// window nothing is draining the backend.
//
// 4096 absorbs a large checkout. It is a bound rather than an unbounded queue
// because an unbounded one turns a runaway generator (a build tool writing in a
// loop) into unbounded memory growth, which is a worse failure than a reported
// gap.
const eventQueueSize = 4096

// drainBackend copies backend events into a buffered channel as fast as the
// backend produces them.
//
// WHY THIS EXISTS. fsnotify's Events channel is unbuffered by default: its
// reader goroutine blocks on every send until someone receives. Our event loop
// is not always able to receive promptly — handle() calls dirSet.addTree for a
// newly created directory, which registers watches and reads the whole subtree.
// A loop that consumed the backend channel directly would hold fsnotify's
// reader blocked for that entire span.
//
// A blocked reader is not draining the kernel. On Linux the inotify queue is
// bounded and overflowing it costs IN_Q_OVERFLOW — fsnotify reports
// ErrEventOverflow and the events behind it are gone. The window that matters
// is precisely the expensive one: `git checkout` of a branch that adds
// directories makes addTree run longest exactly when events arrive fastest.
//
// HONEST SCOPE. This was written while chasing a different bug — a watch on a
// new directory that registered successfully and then never fired — and it did
// NOT fix it. That turned out to be a flag-clobbering race inside fsnotify's
// kqueue backend; see dirSet.rewatchTree. This is kept because the hazard above
// is real on its own, not because it explained that symptom.
//
// Overflow is reported, never silent. A dropped event means the index can miss
// a change, and a consumer that knows it happened can reconcile; one that does
// not will trust a stale index indefinitely.
func drainBackend(src <-chan fsnotify.Event, dst chan<- fsnotify.Event, dropped *atomic.Int64, done <-chan struct{}) {
	defer close(dst)
	for {
		select {
		case <-done:
			return
		case ev, ok := <-src:
			if !ok {
				return
			}
			select {
			case dst <- ev:
			case <-done:
				return
			default:
				// The queue is full: the consumer is far behind. Dropping here
				// keeps the backend draining, which preserves every LATER
				// event. Blocking instead would reproduce the original bug and
				// lose events silently in the kernel rather than visibly here.
				dropped.Add(1)
			}
		}
	}
}

// overflowError describes lost events.
//
// Deliberately an error rather than a log line: a consumer that receives it
// knows its view is incomplete and can fall back to a full reconcile.
// `cortex watch` re-diffs the tree on every batch, so a reported overflow
// costs one extra scan and loses nothing.
func overflowError(n int64) error {
	return fmt.Errorf("watch: %d filesystem event(s) dropped because the queue was full; "+
		"changes may be missing — reconcile against the filesystem rather than trusting "+
		"the last batch", n)
}
