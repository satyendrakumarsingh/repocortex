package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestReconcileArithmetic is the unit-level truth table for the invariant
// indexed + skipped + errored == discovered.
//
// The 37-file row is the case this check was written for: a run that returned
// no error, published atomically, and quietly did not cover 37 of the files it
// had found.
func TestReconcileArithmetic(t *testing.T) {
	tests := []struct {
		name        string
		stats       Stats
		cancelled   bool
		wantState   IndexState
		wantUnacct  int64
		wantWarnSub string
	}{
		{
			name:      "everything indexed",
			stats:     Stats{FilesDiscovered: 10, FilesIndexed: 10},
			wantState: IndexStateComplete,
		},
		{
			// Skipped and failed files are ACCOUNTED FOR. A repository full of
			// binaries and broken source is complete, not partial.
			name: "skips and parse failures are accounted for",
			stats: Stats{
				FilesDiscovered: 10,
				FilesIndexed:    4,
				FilesSkipped:    5,
				ParseFailures:   1,
			},
			wantState: IndexStateComplete,
		},
		{
			name:      "nothing discovered",
			stats:     Stats{},
			wantState: IndexStateComplete,
		},
		{
			name: "37 files short with no error",
			stats: Stats{
				FilesDiscovered: 1200,
				FilesIndexed:    1100,
				FilesSkipped:    60,
				ParseFailures:   3,
			},
			wantState:   IndexStatePartial,
			wantUnacct:  37,
			wantWarnSub: "37 of 1200",
		},
		{
			name: "cancelled run names cancellation",
			stats: Stats{
				FilesDiscovered: 200,
				FilesIndexed:    12,
			},
			cancelled:   true,
			wantState:   IndexStatePartial,
			wantUnacct:  188,
			wantWarnSub: "cancelled with 188 of 200",
		},
		{
			// Double counting is the same class of bug: the numbers cannot be
			// trusted, so the run is not certified complete.
			name: "double counted file",
			stats: Stats{
				FilesDiscovered: 10,
				FilesIndexed:    9,
				FilesSkipped:    2,
			},
			wantState:   IndexStatePartial,
			wantUnacct:  -1,
			wantWarnSub: "accounting is inconsistent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcile(tc.stats, tc.cancelled)

			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q (%s)", got.State, tc.wantState, got)
			}
			if got.Complete() != (tc.wantState == IndexStateComplete) {
				t.Errorf("Complete() = %v, disagrees with State %q", got.Complete(), got.State)
			}
			if got.Unaccounted != tc.wantUnacct {
				t.Errorf("Unaccounted = %d, want %d", got.Unaccounted, tc.wantUnacct)
			}
			if got.Accounted != tc.stats.FilesIndexed+tc.stats.FilesSkipped+tc.stats.ParseFailures {
				t.Errorf("Accounted = %d, does not equal indexed+skipped+errored", got.Accounted)
			}

			warn := got.Warning()
			if tc.wantWarnSub == "" {
				if warn != "" {
					t.Errorf("complete run produced a warning: %q", warn)
				}
				return
			}
			if !strings.Contains(warn, tc.wantWarnSub) {
				t.Errorf("Warning() = %q, want it to contain %q", warn, tc.wantWarnSub)
			}
			if !strings.Contains(warn, "partial") {
				t.Errorf("Warning() = %q, want it to name the run partial", warn)
			}
		})
	}
}

// TestHealthyRunReportsComplete is the anti-vacuity test: if this ever passes
// only because nothing is ever partial, the shortfall tests below are
// meaningless. It runs a tree with all three legitimate outcomes present —
// indexed source, a skipped binary, a grammar-less file and a malformed one —
// at several pool widths, because the invariant must hold independently of
// worker scheduling.
func TestHealthyRunReportsComplete(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"notes.zzzz": "no grammar claims this file",
		"broken.go":  "package p\nfunc Broken( { { {",
	}
	for i := 0; i < 60; i++ {
		files[fmt.Sprintf("pkg/f%02d.go", i)] = fmt.Sprintf("package pkg\n\nfunc F%02d() {}\n", i)
	}
	writeTree(t, root, files)

	for _, workers := range []int{1, 2, 4, 8, 16} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var c collector
			stats, err := runnerFor(t, root, Options{Workers: workers}).
				Run(context.Background(), &c)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			rec := stats.Reconciliation
			if !rec.Complete() {
				t.Fatalf("healthy run reported %s: %s", rec.State, rec.Warning())
			}
			if rec.Warning() != "" {
				t.Errorf("complete run carried a warning: %q", rec.Warning())
			}
			if rec.Unaccounted != 0 {
				t.Errorf("Unaccounted = %d, want 0 (%s)", rec.Unaccounted, rec)
			}
			if rec.Discovered != stats.FilesDiscovered || rec.Discovered != int64(len(files)) {
				t.Errorf("Discovered = %d, want %d", rec.Discovered, len(files))
			}
			if rec.Accounted != rec.Discovered {
				t.Errorf("indexed(%d)+skipped(%d)+errored(%d) = %d, want %d discovered",
					rec.Indexed, rec.Skipped, rec.Errored, rec.Accounted, rec.Discovered)
			}
			// The run must exercise more than one outcome, or "complete" is
			// only proving that indexed == discovered.
			if rec.Skipped == 0 {
				t.Error("fixture produced no skipped files; the accounted-for path is untested")
			}
			if rec.Cancelled {
				t.Error("uncancelled run reported Cancelled")
			}
		})
	}
}

// TestCancelledRunReportsPartial injects a real shortfall through the one path
// in the current code that loses files and still returns nil: the caller
// cancels, workers abandon whatever they were holding, and the dispatch loop
// stops feeding the pool. Nothing about the returned error distinguishes this
// run from a complete one — only the reconciliation does.
func TestCancelledRunReportsPartial(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 300; i++ {
		files[fmt.Sprintf("pkg/f%03d.go", i)] = fmt.Sprintf("package pkg\n\nfunc F%03d() {}\n", i)
	}
	writeTree(t, root, files)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu      sync.Mutex
		written int64
		once    sync.Once
	)
	sink := SinkFunc(func(_ context.Context, _ FileFacts) error {
		once.Do(cancel)
		mu.Lock()
		written++
		mu.Unlock()
		return nil
	})

	stats, err := runnerFor(t, root, Options{Workers: 2}).Run(ctx, sink)
	if err != nil {
		t.Fatalf("cancellation should end the run cleanly, got: %v", err)
	}

	rec := stats.Reconciliation
	if rec.Complete() {
		t.Fatalf("a run that stopped after ~%d of %d files claimed to be complete (%s)",
			written, len(files), rec)
	}
	if rec.State != IndexStatePartial {
		t.Errorf("State = %q, want %q", rec.State, IndexStatePartial)
	}
	if !rec.Cancelled {
		t.Error("Cancelled = false on a cancelled run")
	}
	if rec.Discovered != int64(len(files)) {
		t.Errorf("Discovered = %d, want %d", rec.Discovered, len(files))
	}
	// The count is exact, not an estimate: it is the arithmetic complement of
	// what the three terminal outcomes recorded.
	if want := rec.Discovered - rec.Accounted; rec.Unaccounted != want || rec.Unaccounted <= 0 {
		t.Errorf("Unaccounted = %d, want %d and > 0 (%s)", rec.Unaccounted, want, rec)
	}
	if !strings.Contains(rec.Warning(), fmt.Sprintf("%d of %d", rec.Unaccounted, rec.Discovered)) {
		t.Errorf("Warning() = %q, want it to name the shortfall count", rec.Warning())
	}
	t.Logf("shortfall: %s | %s", rec, rec.Warning())
}

// TestSinkFailureRunReportsPartial covers the other loss path: the pool is
// aborted mid-flight, so the files in flight reach no terminal outcome. The
// error already tells the caller not to publish; the reconciliation says by
// how much the run fell short, which is what a status line has to show.
func TestSinkFailureRunReportsPartial(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 120; i++ {
		files[fmt.Sprintf("pkg/f%03d.go", i)] = fmt.Sprintf("package pkg\n\nfunc F%03d() {}\n", i)
	}
	writeTree(t, root, files)

	sentinel := errors.New("disk on fire")
	c := &collector{err: sentinel, failN: 5}

	stats, err := runnerFor(t, root, Options{Workers: 4}).Run(context.Background(), c)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, sentinel)
	}

	rec := stats.Reconciliation
	if rec.Complete() {
		t.Fatalf("an aborted run reported complete: %s", rec)
	}
	if rec.Unaccounted <= 0 {
		t.Errorf("Unaccounted = %d, want a positive shortfall (%s)", rec.Unaccounted, rec)
	}
}

// TestReconciliationIsIndependentOfPoolWidth pins the determinism requirement:
// the same tree must reconcile identically at every width, so a partial verdict
// can never be an artefact of how many workers happened to be running.
func TestReconciliationIsIndependentOfPoolWidth(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"blob.zzzz": "no grammar", "bad.go": "package p\nfunc X( {"}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("src/g%02d.go", i)] = fmt.Sprintf("package src\n\nfunc G%02d() {}\n", i)
	}
	writeTree(t, root, files)

	var first string
	for _, workers := range []int{1, 3, 8} {
		var c collector
		stats, err := runnerFor(t, root, Options{Workers: workers}).Run(context.Background(), &c)
		if err != nil {
			t.Fatalf("workers=%d: Run: %v", workers, err)
		}
		got := stats.Reconciliation.String()
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("workers=%d reconciled to %s, want %s", workers, got, first)
		}
	}
}
