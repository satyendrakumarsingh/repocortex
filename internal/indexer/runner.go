package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds one subprocess when the caller sets none.
//
// Ten minutes is not a guess at how long a build takes — it is how long
// RepoCortex is willing to be unresponsive before deciding the build is not
// coming back. config.Config.IndexerTimeout overrides it, and that is the knob
// a user with a genuinely slow monorepo should reach for.
const DefaultTimeout = 10 * time.Minute

// DefaultMaxOutputBytes bounds captured stdout and stderr, per stream, per
// process.
//
// The bound is not politeness. Gradle in debug mode, a Maven plugin looping, or
// a hostile build script running `yes` all produce output at pipe speed
// forever; buffering it whole would turn a bad build into an OOM kill of
// RepoCortex itself. Past the cap bytes are counted and discarded rather than
// blocking the pipe, so the subprocess keeps making progress and its exit code
// is still observed.
const DefaultMaxOutputBytes int64 = 1 << 20

// waitDelay is how long Wait will tolerate a child holding the output pipes
// open after the context has killed it.
//
// Maven and Gradle fork daemons that inherit stdout. Without a WaitDelay the
// kill lands on the parent, the daemon keeps the pipe open, and Wait blocks
// forever — the exact hang the timeout was supposed to prevent.
const waitDelay = 5 * time.Second

// Errors returned when a Command is malformed. They are sentinels because
// they describe a programming mistake with a security consequence, and a test
// asserting on message text would not survive a reword.
var (
	// ErrNoExecutable means Command.Path was empty.
	ErrNoExecutable = errors.New("indexer: command has no executable")

	// ErrRelativeExecutable means Command.Path was not absolute.
	//
	// This is a security check, not tidiness. exec.Cmd resolves a relative Path
	// against Cmd.Dir, and Cmd.Dir is the repository being indexed. A repo
	// shipping ./go or ./npm would otherwise be executed in place of the real
	// tool. Resolve through Runner.LookPath first; it returns an absolute path
	// or an error.
	ErrRelativeExecutable = errors.New("indexer: executable path must be absolute")

	// ErrBadArgument means an argument contained a NUL byte, which cannot be
	// passed through execve and is a sign the value came from somewhere it
	// should not have.
	ErrBadArgument = errors.New("indexer: argument contains a NUL byte")
)

// Command is one subprocess this package is willing to run.
//
// Note what is absent: there is no field holding a shell line, and no field a
// caller could fill with "mvn clean install && curl ...". The executable and
// each argument are separate values that go straight to execve. This is the
// single most important shape decision in the package — argv injection is not
// possible if there is no string to inject into.
type Command struct {
	// Path is the absolute path to the executable. Always the result of
	// Runner.LookPath, never a name and never repository-derived.
	Path string

	// Args are the arguments after argv[0]. Every element is either a compile-
	// time constant or a value this package constructed and validated.
	Args []string

	// Dir is the working directory. This is how repository-derived paths cross
	// into a subprocess: as a directory rather than as an argument, so there is
	// nothing to quote and nothing to escape.
	Dir string

	// Env is the child environment. Nil inherits this process's environment,
	// which is what build tools need; call sites that must constrain it (see
	// goEnv) build it explicitly.
	Env []string

	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration

	// MaxOutputBytes bounds each captured stream. Zero means
	// DefaultMaxOutputBytes; negative means unbounded, which nothing in this
	// package asks for.
	MaxOutputBytes int64
}

// Argv returns the full argument vector, for audit records and error messages.
func (c Command) Argv() []string {
	argv := make([]string, 0, len(c.Args)+1)
	argv = append(argv, c.Path)
	return append(argv, c.Args...)
}

// String renders the command for humans. It is not shell-quoted on purpose:
// producing something that looks copy-pasteable into a shell would invite
// someone to build the same command as a string somewhere else.
func (c Command) String() string { return strings.Join(c.Argv(), " ") }

func (c Command) validate() error {
	if c.Path == "" {
		return ErrNoExecutable
	}
	if !filepath.IsAbs(c.Path) {
		return fmt.Errorf("%w: %q", ErrRelativeExecutable, c.Path)
	}
	for _, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return ErrBadArgument
		}
	}
	if c.Dir != "" && !filepath.IsAbs(c.Dir) {
		return fmt.Errorf("indexer: working directory must be absolute: %q", c.Dir)
	}
	return nil
}

// Result is what a subprocess did.
//
// A non-zero ExitCode is data, not an error: a Maven build failing on the
// repository's own code is a fact the caller reports, not a fault in RepoCortex.
// Runner returns an error only when the process could not be run, was killed by
// the timeout, or the context was cancelled.
type Result struct {
	Argv     []string
	Dir      string
	ExitCode int
	Duration time.Duration

	// Stdout and Stderr hold at most MaxOutputBytes each. What is kept is the
	// HEAD of the stream: a tool that cannot start says so in its first line,
	// and that is the failure this package has to diagnose most often.
	Stdout []byte
	Stderr []byte

	// StdoutDropped and StderrDropped count bytes discarded past the cap.
	// Non-zero means what you are reading is a prefix; say so before quoting
	// it at a user.
	StdoutDropped int64
	StderrDropped int64

	// TimedOut reports that the process was killed because Timeout elapsed,
	// rather than exiting on its own.
	TimedOut bool
}

// Truncated reports whether either stream lost bytes to the cap.
func (r Result) Truncated() bool { return r.StdoutDropped > 0 || r.StderrDropped > 0 }

// Diagnostic renders the most useful few lines of a failure for a CLI message.
// It is bounded twice — once by the capture cap and once here — because a
// thousand lines of Gradle output in an error message is not a diagnostic.
func (r Result) Diagnostic() string {
	s := strings.TrimSpace(string(r.Stderr))
	if s == "" {
		s = strings.TrimSpace(string(r.Stdout))
	}
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 10 {
		lines = lines[:10]
	}
	out := strings.Join(lines, "\n")
	if r.Truncated() || len(lines) < len(strings.Split(s, "\n")) {
		out += "\n… (output truncated)"
	}
	return out
}

// Runner is the only way this package reaches the operating system.
//
// It exists for two reasons and they point the same way. Testing: no unit test
// in this package needs a JVM, a network or a Docker daemon, because a fake
// Runner makes planning and execution a pure function. Review: every subprocess
// RepoCortex can start is reachable from the two methods below, so an auditor has
// one place to look rather than a grep for exec.Command.
type Runner interface {
	// LookPath resolves an executable name against PATH and returns an
	// absolute path. It must never return a relative path — see
	// ErrRelativeExecutable.
	LookPath(file string) (string, error)

	// Run executes the command. It returns an error only when the process
	// could not be started, was killed by the timeout, or the context ended.
	Run(ctx context.Context, c Command) (Result, error)
}

// ExecRunner is the real Runner.
type ExecRunner struct {
	// Timeout applies to commands that do not set their own. Zero means
	// DefaultTimeout.
	Timeout time.Duration

	// MaxOutputBytes applies to commands that do not set their own. Zero means
	// DefaultMaxOutputBytes.
	MaxOutputBytes int64
}

// LookPath resolves file against PATH.
//
// The absolute-path conversion at the end is not cosmetic. A PATH entry may be
// relative, in which case LookPath returns a relative result that exec would
// later resolve against the repository directory. Go 1.19+ already refuses the
// "." case; this closes the rest.
func (r ExecRunner) LookPath(file string) (string, error) {
	p, err := exec.LookPath(file)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// Run executes c with the timeout and output bound applied.
func (r ExecRunner) Run(ctx context.Context, c Command) (Result, error) {
	if err := c.validate(); err != nil {
		return Result{Argv: c.Argv(), Dir: c.Dir}, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = r.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxOut := c.MaxOutputBytes
	if maxOut == 0 {
		maxOut = r.MaxOutputBytes
	}
	if maxOut == 0 {
		maxOut = DefaultMaxOutputBytes
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, c.Path, c.Args...) //nolint:gosec // argv is explicit and never repository-derived; see Command
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	stdout := &boundedWriter{max: maxOut}
	stderr := &boundedWriter{max: maxOut}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Argv:          c.Argv(),
		Dir:           c.Dir,
		Duration:      time.Since(start),
		Stdout:        stdout.bytes(),
		Stderr:        stderr.bytes(),
		StdoutDropped: stdout.droppedBytes(),
		StderrDropped: stderr.droppedBytes(),
	}

	// Order matters here. A timed-out process also exits non-zero (killed), so
	// the deadline has to be checked before the exit code is believed.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("indexer: %s timed out after %s: %w",
			filepath.Base(c.Path), timeout, context.DeadlineExceeded)
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, ctx.Err()
	}

	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		// The tool ran and disagreed. That is a result, not a failure of ours.
		res.ExitCode = ee.ExitCode()
		return res, nil
	default:
		res.ExitCode = -1
		return res, fmt.Errorf("indexer: run %s: %w", filepath.Base(c.Path), err)
	}
}

// boundedWriter keeps a prefix and counts the rest.
//
// It never returns an error or a short write. Reporting a short write would
// make exec's copier abandon the stream and, worse, could make a tool that
// checks its own write errors abort a build RepoCortex merely declined to read
// all of. Discarding is deliberate, so it is reported as success and counted.
//
// The mutex looks redundant given exec uses one goroutine per stream, but it
// costs nothing on this path and makes the type safe to assign to both Stdout
// and Stderr, which is a mistake waiting to happen.
type boundedWriter struct {
	mu      sync.Mutex
	max     int64
	buf     bytes.Buffer
	dropped int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.max < 0 {
		w.buf.Write(p)
		return len(p), nil
	}
	room := w.max - int64(w.buf.Len())
	if room > 0 {
		n := int64(len(p))
		if n > room {
			n = room
		}
		w.buf.Write(p[:n])
		w.dropped += int64(len(p)) - n
		return len(p), nil
	}
	w.dropped += int64(len(p))
	return len(p), nil
}

func (w *boundedWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		return nil
	}
	return append([]byte(nil), w.buf.Bytes()...)
}

func (w *boundedWriter) droppedBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// envWith returns base with each key=value in kv set, replacing any existing
// entry for that key.
//
// Building the child environment explicitly matters for the Go toolchain in
// particular: GOFLAGS, GOPRIVATE and GONOSUMDB can each disable module checksum
// verification, and a machine that has one of them set for unrelated reasons
// must not silently lose the verification this package depends on.
func envWith(base []string, kv ...string) []string {
	out := make([]string, 0, len(base)+len(kv))
	drop := make(map[string]bool, len(kv))
	for _, e := range kv {
		if k, _, ok := strings.Cut(e, "="); ok {
			drop[k] = true
		}
	}
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && drop[k] {
			continue
		}
		out = append(out, e)
	}
	return append(out, kv...)
}

// goEnv is the environment for any `go` subprocess.
//
// Every entry is a defence:
//
//   - GOTOOLCHAIN pinned to the toolchain RepoCortex is ITSELF running. A go.mod
//     may name a `toolchain` line, and the default GOTOOLCHAIN=auto honours it
//     by DOWNLOADING that toolchain and executing it — so a cloned repository,
//     or a dependency of an indexer, could choose which compiler binary runs on
//     the machine.
//
//     This was `local`, which is stricter still, and it did not work: `local`
//     means the `go` on PATH, which may be older than the running process (here
//     1.24.2 on PATH versus 1.25.13 running). Installing scip-go, whose go.mod
//     requires >= 1.25.0, failed with "requires go >= 1.25.0 (running go
//     1.24.2; GOTOOLCHAIN=local)".
//
//     Pinning to our own version keeps the security property — no third party
//     chooses the compiler — while letting installs that legitimately need a
//     current Go succeed. A module demanding something NEWER than ours still
//     fails loudly, which is the correct outcome.
//
//   - GOFLAGS empty. GOFLAGS can carry -mod=mod, -insecure or an arbitrary
//     -toolexec=PROGRAM, which is a straightforward code-execution hook.
//
//   - GOPRIVATE, GONOSUMDB, GONOSUMCHECK and GOINSECURE cleared, GOSUMDB
//     pinned. These are the switches that turn module checksum verification
//     off, and that verification is the entire reason `go install` of a pinned
//     version can be trusted.
//
// They are cleared rather than merely defaulted because they are inherited from
// the user's environment, where they may be set for unrelated reasons.
func goEnv(extra ...string) []string {
	base := envWith(os.Environ(),
		"GOTOOLCHAIN="+runtime.Version(),
		"GOFLAGS=",
		"GOPRIVATE=",
		"GONOSUMDB=",
		"GONOSUMCHECK=",
		"GOINSECURE=",
		"GOSUMDB=sum.golang.org",
	)
	return envWith(base, extra...)
}
