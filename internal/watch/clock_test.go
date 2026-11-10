package watch

import (
	"sync"
	"testing"
	"time"
)

func TestManualClockFiresOnlyAtDeadline(t *testing.T) {
	c := NewManualClock(epoch)
	timer := c.NewTimer(300 * time.Millisecond)

	c.Advance(299 * time.Millisecond)
	select {
	case <-timer.C():
		t.Fatal("timer fired before its deadline")
	default:
	}

	c.Advance(time.Millisecond)
	select {
	case at := <-timer.C():
		if !at.Equal(epoch.Add(300 * time.Millisecond)) {
			t.Errorf("fired at %v, want %v", at, epoch.Add(300*time.Millisecond))
		}
	default:
		t.Fatal("timer did not fire at its deadline")
	}

	if !c.Now().Equal(epoch.Add(300 * time.Millisecond)) {
		t.Errorf("Now() = %v", c.Now())
	}
}

func TestManualClockStop(t *testing.T) {
	c := NewManualClock(epoch)
	timer := c.NewTimer(time.Second)

	if !timer.Stop() {
		t.Error("Stop on a pending timer reported not pending")
	}
	// Idempotence matters: the event loop stops the previous timer on every
	// re-arm without tracking whether it already fired.
	if timer.Stop() {
		t.Error("second Stop reported the timer as pending")
	}

	c.Advance(2 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("stopped timer fired")
	default:
	}
}

// TestManualClockBlockUntil covers the synchronisation primitive the watcher
// tests rely on: a test must be able to wait for the event loop to have armed
// its debounce timer before advancing past the deadline.
func TestManualClockBlockUntil(t *testing.T) {
	c := NewManualClock(epoch)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.BlockUntil(2)
	}()

	c.NewTimer(time.Second)
	c.NewTimer(time.Second)
	wg.Wait() // hangs the test on regression, which is the intended signal

	if got := c.Created(); got != 2 {
		t.Errorf("Created() = %d, want 2", got)
	}
}

func TestRealClockTimer(t *testing.T) {
	c := RealClock{}
	if time.Since(c.Now()) > time.Second {
		t.Error("RealClock.Now is not tracking wall time")
	}
	timer := c.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("real timer never fired")
	}
	timer.Stop() // must not panic after firing
}
