package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// accuracy.indexers.max_parallel was parsed and then ignored. With it set the
// indexers overlap up to the limit; without it they run one at a time, as
// before; either way the results come back in plan order.
func TestExecuteHonoursMaxParallel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		parallel int
		wantMax  int32
	}{
		{"default stays sequential", 0, 1},
		{"two at once", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeRepo(t, map[string]string{
				"a/go.mod": "module a", "b/go.mod": "module b", "c/go.mod": "module c",
			})
			r := newFakeRunner("go")
			r.install("scip-go")
			var running, peak int32
			var mu sync.Mutex
			r.handle("scip-go", func(c Command) (Result, error) {
				if flagValue(c.Args, "--output") == "" {
					return Result{Stdout: []byte("scip-go 1.2.3\n")}, nil // version probe
				}
				n := atomic.AddInt32(&running, 1)
				mu.Lock()
				if n > peak {
					peak = n
				}
				mu.Unlock()
				time.Sleep(60 * time.Millisecond)
				atomic.AddInt32(&running, -1)
				if err := os.WriteFile(flagValue(c.Args, "--output"), minimalSCIPIndex, 0o600); err != nil {
					return Result{ExitCode: 1}, err
				}
				return Result{}, nil
			})

			cfg := testConfig(root, config.AccuracyAuto)
			cfg.IndexerMaxParallel = tc.parallel
			plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r})
			if err != nil {
				t.Fatalf("NewPlanWith: %v", err)
			}
			res, err := Execute(context.Background(), plan, Options{Runner: r, OutputDir: t.TempDir()})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(res.Indexes) != 3 {
				t.Fatalf("got %d indexes, want 3 (skipped: %+v)", len(res.Indexes), res.Skipped)
			}
			if peak != tc.wantMax {
				t.Errorf("peak concurrency = %d, want %d", peak, tc.wantMax)
			}
			for i, want := range []string{"a", "b", "c"} {
				if res.Indexes[i].Project.Dir != want {
					t.Errorf("result %d is %s, want %s: results must keep plan order", i, res.Indexes[i].Project.Dir, want)
				}
			}
		})
	}
}

// In exact mode the first failing indexer cancels the rest. An earlier one in
// plan order that was still running then fails with "context canceled", and
// returning that hid the real failure: exact mode must report the indexer
// that actually failed.
func TestParallelExactModeReportsTheRealFailure(t *testing.T) {
	t.Parallel()
	root := writeRepo(t, map[string]string{"a/go.mod": "module a", "b/go.mod": "module b"})
	r := newFakeRunner("go")
	r.install("scip-go")
	r.ctxHandlers = map[string]func(context.Context, Command) (Result, error){
		"scip-go": func(ctx context.Context, c Command) (Result, error) {
			out := flagValue(c.Args, "--output")
			if out == "" {
				return Result{Stdout: []byte("scip-go 1.2.3\n")}, nil
			}
			if filepath.Base(c.Dir) == "b" {
				time.Sleep(50 * time.Millisecond) // a is already running
				return Result{ExitCode: 1, Stderr: []byte("b is broken")}, errors.New("scip-go: b is broken")
			}
			// a is slow; when b fails it is killed, as a real subprocess is.
			select {
			case <-ctx.Done():
				return Result{ExitCode: -1}, ctx.Err()
			case <-time.After(2 * time.Second):
				return Result{}, os.WriteFile(out, minimalSCIPIndex, 0o600)
			}
		},
	}
	cfg := testConfig(root, config.AccuracyExact)
	cfg.IndexerMaxParallel = 2
	plan, err := NewPlanWith(context.Background(), root, cfg, Options{Runner: r})
	if err != nil {
		t.Fatalf("NewPlanWith: %v", err)
	}
	_, err = Execute(context.Background(), plan, Options{Runner: r, OutputDir: t.TempDir()})
	if err == nil {
		t.Fatal("exact mode succeeded although an indexer failed")
	}
	if errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("err = %v; want the real failure of b, not a cancellation", err)
	}
}
