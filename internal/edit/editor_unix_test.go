//go:build unix

package edit

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// prepareWithin runs Prepare and fails the test, instead of hanging it, if
// Prepare blocks — which is what opening a FIFO with no writer does.
func prepareWithin(t *testing.T, ed *Editor, fe FileEdit) ([]Prepared, []Problem, error) {
	t.Helper()
	type result struct {
		p   []Prepared
		pr  []Problem
		err error
	}
	ch := make(chan result, 1)
	go func() {
		p, pr, err := ed.Prepare(context.Background(), []FileEdit{fe})
		ch <- result{p, pr, err}
	}()
	limit := 10 * time.Second
	if dl, ok := t.Deadline(); ok && time.Until(dl)/2 < limit {
		limit = time.Until(dl) / 2
	}
	select {
	case r := <-ch:
		return r.p, r.pr, r.err
	case <-time.After(limit):
		t.Fatalf("Prepare blocked for %v on a FIFO", limit)
		return nil, nil, nil
	}
}

func TestPrepareRefusesAFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "a.go"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	prepared, problems, err := prepareWithin(t, New(root), editFor("a.go", storeSrc, map[int][]int{3: {-1}}))
	if err != nil || len(prepared) != 0 || len(problems) != 1 {
		t.Fatalf("err=%v prepared=%d problems=%+v, want one problem", err, len(prepared), problems)
	}
}

// TestPrepareRefusesAFIFOSwappedInAfterTheCheck: the path passes the Lstat
// check as a regular file and is a FIFO by the time it is opened. The open
// must not block, and the opened handle must be refused.
func TestPrepareRefusesAFIFOSwappedInAfterTheCheck(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", storeSrc)
	ed := New(root)
	ed.fault = func(s faultStage, rel string) error {
		if s != stageChecked {
			return nil
		}
		p := filepath.Join(root, rel)
		if err := os.Remove(p); err != nil {
			return err
		}
		return syscall.Mkfifo(p, 0o644)
	}
	prepared, problems, err := prepareWithin(t, ed, editFor("a.go", storeSrc, map[int][]int{3: {-1}}))
	if err != nil || len(prepared) != 0 || len(problems) != 1 {
		t.Fatalf("err=%v prepared=%d problems=%+v, want one problem", err, len(prepared), problems)
	}
}
