package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// The tests below run a real subprocess, because the two properties they check —
// that a hang is killed and that a flood is bounded — are properties of
// ExecRunner's interaction with the operating system, and a fake would only
// prove that the fake behaves. The subprocess is this test binary re-invoked
// (the os/exec pattern), so nothing outside the repository is required: no
// shell, no network, no third-party tool.

// helperEnv marks the child process and selects its behaviour.
const (
	helperEnv     = "REPOCORTEX_INDEXER_HELPER"
	helperModeEnv = "REPOCORTEX_INDEXER_HELPER_MODE"
)

// TestHelperProcess is not a test. It is the body of the child process the
// tests in this file spawn; it exits immediately unless the marker is set.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	switch os.Getenv(helperModeEnv) {
	case "hang":
		// Sleep rather than block on an empty select: a deadlocked Go program
		// panics, which would exit and defeat the point.
		time.Sleep(5 * time.Minute)
		os.Exit(0)

	case "flood":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for i := 0; i < 128; i++ { // 8 MiB, far past any cap under test
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(0)
			}
		}
		os.Exit(0)

	case "fail":
		os.Stderr.WriteString("first line of trouble\nsecond line\n")
		os.Exit(7)

	case "banner":
		os.Stdout.WriteString("fake-tool 9.9.9 (build abc)\nextra line\n")
		os.Exit(0)

	default:
		os.Exit(0)
	}
}

func helperCommand(t *testing.T, mode string) Command {
	t.Helper()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return Command{
		Path: self,
		Args: []string{"-test.run=^TestHelperProcess$", "-test.v=false"},
		Env:  append(os.Environ(), helperEnv+"=1", helperModeEnv+"="+mode),
	}
}

// TestRunnerKillsAHangingSubprocess is the guarantee that a build which never
// returns cannot take RepoCortex with it.
func TestRunnerKillsAHangingSubprocess(t *testing.T) {
	t.Parallel()
	cmd := helperCommand(t, "hang")
	cmd.Timeout = 300 * time.Millisecond

	start := time.Now()
	res, err := ExecRunner{}.Run(context.Background(), cmd)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !res.TimedOut {
		t.Errorf("Result.TimedOut = false, want true")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not unwrap to DeadlineExceeded: %v", err)
	}
	// A killed run is something RepoCortex stopped, not a fault to report as a
	// bug, and it must never look retryable.
	if k := errs.KindOf(err); k != errs.KindCancelled {
		t.Errorf("KindOf = %q, want %q", k, errs.KindCancelled)
	}
	if errs.IsRetryable(err) {
		t.Error("a timed-out build must not be reported as retryable")
	}
	// waitDelay is 5s; the kill itself should be immediate.
	if elapsed > 15*time.Second {
		t.Errorf("took %s to give up on a 300ms timeout", elapsed)
	}
}

// TestRunnerBoundsOutput proves the memory bound: a subprocess writing 8 MiB
// with a 64 KiB cap must leave 64 KiB in memory, not 8 MiB, and must still be
// allowed to finish rather than being blocked on a full pipe.
func TestRunnerBoundsOutput(t *testing.T) {
	t.Parallel()
	cmd := helperCommand(t, "flood")
	cmd.MaxOutputBytes = 64 << 10
	cmd.Timeout = 60 * time.Second

	res, err := ExecRunner{}.Run(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := int64(len(res.Stdout)); got > cmd.MaxOutputBytes {
		t.Fatalf("captured %d bytes, cap was %d", got, cmd.MaxOutputBytes)
	}
	if res.StdoutDropped == 0 {
		t.Fatal("expected dropped bytes to be counted")
	}
	if !res.Truncated() {
		t.Error("Truncated() = false after dropping bytes")
	}
	// The flood must not have been turned into a failure: the process wrote
	// everything it wanted to and exited normally.
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (a chatty process is not a failing one)", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("a bounded writer must not stall the child into a timeout")
	}
}

// TestRunnerReportsExitCodeAsData: a tool that ran and disagreed is a result,
// not an error from the runner.
func TestRunnerReportsExitCodeAsData(t *testing.T) {
	t.Parallel()
	res, err := ExecRunner{}.Run(context.Background(), helperCommand(t, "fail"))
	if err != nil {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Diagnostic(), "first line of trouble") {
		t.Errorf("diagnostic lost the useful line: %q", res.Diagnostic())
	}
}

// TestRunnerRefusesRelativeExecutable is a security test, not an input
// validation test. exec.Cmd resolves a relative Path against Cmd.Dir, and
// Cmd.Dir is the untrusted repository — so a repo shipping ./npm would be run in
// place of the real one.
func TestRunnerRefusesRelativeExecutable(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{"npm": "#!/bin/sh\necho pwned\n"})
	_, err := ExecRunner{}.Run(context.Background(), Command{Path: "npm", Dir: repo})
	if !errors.Is(err, ErrRelativeExecutable) {
		t.Fatalf("err = %v, want ErrRelativeExecutable", err)
	}
	_, err = ExecRunner{}.Run(context.Background(), Command{Path: "./npm", Dir: repo})
	if !errors.Is(err, ErrRelativeExecutable) {
		t.Fatalf("err = %v, want ErrRelativeExecutable for ./npm", err)
	}
	_, err = ExecRunner{}.Run(context.Background(), Command{})
	if !errors.Is(err, ErrNoExecutable) {
		t.Fatalf("err = %v, want ErrNoExecutable", err)
	}
}

func TestRunnerRefusesNULInArguments(t *testing.T) {
	t.Parallel()
	cmd := helperCommand(t, "banner")
	cmd.Args = append(cmd.Args, "a\x00b")
	_, err := ExecRunner{}.Run(context.Background(), cmd)
	if !errors.Is(err, ErrBadArgument) {
		t.Fatalf("err = %v, want ErrBadArgument", err)
	}
}

func TestRunnerHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := helperCommand(t, "hang")
	cmd.Timeout = time.Minute
	_, err := ExecRunner{}.Run(ctx, cmd)
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	if k := errs.KindOf(err); k != errs.KindCancelled {
		t.Errorf("KindOf = %q, want %q", k, errs.KindCancelled)
	}
}

func TestBoundedWriterCountsWithoutGrowing(t *testing.T) {
	t.Parallel()
	w := &boundedWriter{max: 10}
	for i := 0; i < 100; i++ {
		n, err := w.Write([]byte("0123456789"))
		if err != nil || n != 10 {
			// A short write or an error would make exec abandon the stream and
			// could make a well-behaved tool abort its own build.
			t.Fatalf("Write = %d, %v; want 10, nil", n, err)
		}
	}
	if got := len(w.bytes()); got != 10 {
		t.Fatalf("buffered %d bytes, want 10", got)
	}
	if got := w.droppedBytes(); got != 990 {
		t.Fatalf("dropped = %d, want 990", got)
	}
}

// TestGoEnvDisarmsChecksumOptOuts: `go install` of a pinned version is only
// trustworthy because the module checksum database verifies it, and every
// variable below can switch that off. They are inherited from the user's shell,
// so they have to be cleared rather than assumed absent.
func TestGoEnvDisarmsChecksumOptOuts(t *testing.T) {
	t.Setenv("GOFLAGS", "-toolexec=/bin/evil")
	t.Setenv("GONOSUMDB", "*")
	t.Setenv("GOPRIVATE", "github.com/*")
	t.Setenv("GOINSECURE", "*")
	t.Setenv("GOTOOLCHAIN", "auto")

	env := goEnv()
	want := map[string]string{
		"GOFLAGS":     "",
		"GONOSUMDB":   "",
		"GOPRIVATE":   "",
		"GOINSECURE":  "",
		"GOTOOLCHAIN": runtime.Version(),
		"GOSUMDB":     "sum.golang.org",
	}
	got := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			if _, interesting := want[k]; interesting {
				if _, dup := got[k]; dup {
					// envWith must replace, not append: the last value wins in
					// execve, but a duplicate is a trap for anyone reading it.
					t.Errorf("%s appears more than once in the child environment", k)
				}
				got[k] = v
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("child env %s = %q, want %q", k, got[k], v)
		}
	}
}
