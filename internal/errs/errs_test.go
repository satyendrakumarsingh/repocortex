package errs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// errNoCommittedRun mirrors registry.ErrNoCommittedRun. Copied rather than
// imported so this package stays a leaf and the test cannot create an import
// cycle when the registry adopts errs.
var errNoCommittedRun = errors.New("registry: no committed index run")

func TestKindRoundTripThroughFmtErrorf(t *testing.T) {
	t.Parallel()

	base := Wrap(KindTransient, "store.Exec", errors.New("database is locked"))

	// Four layers of the plain wrapping the rest of the codebase already
	// uses. The kind has to survive all of it, or migration means converting
	// every intermediate wrap too.
	err := error(base)
	err = fmt.Errorf("registry: commit run 7: %w", err)
	err = fmt.Errorf("publish index revision: %w", err)
	err = fmt.Errorf("indexing failed: %w", err)

	if got := KindOf(err); got != KindTransient {
		t.Fatalf("KindOf through 3 wraps = %q, want %q", got, KindTransient)
	}
	if !IsRetryable(err) {
		t.Error("IsRetryable = false, want true for a transient cause")
	}
	if !Is(err, KindTransient) {
		t.Error("Is(err, KindTransient) = false")
	}
	if want := "indexing failed: publish index revision: registry: commit run 7: store.Exec: database is locked"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestOutermostKindWins(t *testing.T) {
	t.Parallel()

	// A caller that knows better than the driver must be able to say so:
	// contention inside a validation pass is still a validation failure to
	// whoever called it.
	inner := Wrap(KindTransient, "store.Exec", errors.New("locked"))
	outer := Wrap(KindCorrupt, "registry.validateRun", fmt.Errorf("check: %w", inner))

	if got := KindOf(outer); got != KindCorrupt {
		t.Fatalf("KindOf = %q, want %q (outermost annotation must win)", got, KindCorrupt)
	}
	if IsRetryable(outer) {
		t.Error("IsRetryable = true; a re-classified error must not stay retryable")
	}
}

func TestZeroKindIsTransparent(t *testing.T) {
	t.Parallel()

	// Attaching Op/Fields without asserting a class must not swallow the
	// classification underneath.
	inner := Wrap(KindNotFound, "registry.run", sql.ErrNoRows)
	outer := &Error{Op: "cli.index", Err: inner}

	if got := KindOf(outer); got != KindNotFound {
		t.Fatalf("KindOf = %q, want %q", got, KindNotFound)
	}
}

func TestWrappedSentinelStaysMatchable(t *testing.T) {
	t.Parallel()

	// The whole migration depends on this: adopting a kind must not break a
	// single existing errors.Is against a sentinel.
	err := error(Wrap(KindNotFound, "registry.CurrentRun", errNoCommittedRun))
	err = fmt.Errorf("load context: %w", err)

	if !errors.Is(err, errNoCommittedRun) {
		t.Fatal("errors.Is lost the sentinel through *Error")
	}
	if got := KindOf(err); got != KindNotFound {
		t.Errorf("KindOf = %q, want %q", got, KindNotFound)
	}
}

func TestErrorsAsExtractsError(t *testing.T) {
	t.Parallel()

	want := Wrap(KindConflict, "registry.ReplaceFile", errors.New("run is committed")).
		WithField("run_id", int64(7))
	err := fmt.Errorf("update file: %w", fmt.Errorf("replace: %w", want))

	var got *Error
	if !errors.As(err, &got) {
		t.Fatal("errors.As did not find *Error")
	}
	if got != want {
		t.Fatalf("errors.As returned %p, want %p", got, want)
	}
	if got.Fields["run_id"] != int64(7) {
		t.Errorf("Fields[run_id] = %v, want 7", got.Fields["run_id"])
	}
	if got.Op != "registry.ReplaceFile" {
		t.Errorf("Op = %q", got.Op)
	}
}

func TestErrorsIsAgainstKindPrototype(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("commit: %w", Wrap(KindTransient, "store.Exec", errors.New("locked")))

	if !errors.Is(err, &Error{Kind: KindTransient}) {
		t.Error("prototype match on Kind failed")
	}
	if !errors.Is(err, &Error{Kind: KindTransient, Op: "store.Exec"}) {
		t.Error("prototype match on Kind+Op failed")
	}
	if errors.Is(err, &Error{Kind: KindTransient, Op: "other.Op"}) {
		t.Error("prototype matched despite a different Op")
	}
	if errors.Is(err, &Error{Kind: KindCorrupt}) {
		t.Error("prototype matched despite a different Kind")
	}
}

func TestCancellationMapsToCancelled(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer stop()

	cases := []struct {
		name string
		err  error
	}{
		{"context.Canceled", context.Canceled},
		{"context.DeadlineExceeded", context.DeadlineExceeded},
		{"live cancelled ctx", cancelled.Err()},
		{"live expired ctx", deadline.Err()},
		{"wrapped", fmt.Errorf("discovery: %w", context.Canceled)},
		{"wrapped twice", fmt.Errorf("index: %w", fmt.Errorf("walk: %w", context.DeadlineExceeded))},
		{"through *Error", Wrap("", "pipeline.Run", context.Canceled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.err); got != KindCancelled {
				t.Fatalf("KindOf = %q, want %q", got, KindCancelled)
			}
			if IsRetryable(tc.err) {
				t.Error("cancellation must not be retryable — the caller asked to stop")
			}
		})
	}
}

func TestRetryableClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind Kind
		want bool
	}{
		{KindTransient, true},
		{KindInvalid, false},
		{KindNotFound, false},
		{KindConflict, false},
		{KindCorrupt, false},
		{KindUnsupported, false},
		{KindCancelled, false},
		{KindInternal, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := fmt.Errorf("outer: %w", New(tc.kind, "op", "boom"))
			if got := IsRetryable(err); got != tc.want {
				t.Fatalf("IsRetryable(%s) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

func TestUnclassifiedIsInternal(t *testing.T) {
	t.Parallel()

	// An error we cannot classify must read as a bug, never as benign and
	// never as retryable.
	err := fmt.Errorf("wrapped: %w", errors.New("something we have never seen"))
	if got := KindOf(err); got != KindInternal {
		t.Fatalf("KindOf = %q, want %q", got, KindInternal)
	}
	if IsRetryable(err) {
		t.Error("an unclassified error must not be retryable")
	}
}

func TestNilSafety(t *testing.T) {
	t.Parallel()

	if got := KindOf(nil); got != "" {
		t.Errorf("KindOf(nil) = %q, want the zero Kind", got)
	}
	if IsRetryable(nil) {
		t.Error("IsRetryable(nil) = true")
	}
	if Is(nil, KindInternal) || Is(nil, "") {
		t.Error("Is(nil, ...) must always be false")
	}

	var e *Error
	if e.Error() != "<nil>" {
		t.Errorf("(*Error)(nil).Error() = %q", e.Error())
	}
	if e.Unwrap() != nil {
		t.Error("(*Error)(nil).Unwrap() != nil")
	}
	if e.Is(&Error{}) {
		t.Error("(*Error)(nil).Is(...) = true")
	}
	if e.WithField("k", "v") != nil {
		t.Error("(*Error)(nil).WithField must stay nil")
	}
	if got := KindOf(e); got != KindInternal {
		// A typed-nil *Error is itself a bug; report it as one rather than
		// panicking.
		t.Errorf("KindOf(typed nil) = %q, want %q", got, KindInternal)
	}
}

func TestWrapNilNeverReturnsNil(t *testing.T) {
	t.Parallel()

	// The typed-nil trap: if Wrap returned a nil *Error it would compare
	// non-nil once assigned to an `error`, and every caller downstream would
	// fail on a success.
	// Assert on the CONCRETE return: comparing the interface to nil is
	// vacuously false because Wrap's return type is *Error, which staticcheck
	// (SA4023) correctly flags as a comparison that can never be true. The
	// property actually worth testing is that Wrap never returns a nil
	// pointer, since that is what would become a non-nil interface downstream.
	wrapped := Wrap(KindInternal, "registry.CommitRun", nil)
	if wrapped == nil {
		t.Fatal("Wrap returned a nil *Error; assigned to an error it would compare non-nil")
	}
	err := error(wrapped)
	if got := err.Error(); got != "registry.CommitRun: <nil cause>" {
		t.Errorf("message = %q; the mistake must be visible", got)
	}
}

func TestNotFoundFromStdlibLeaves(t *testing.T) {
	t.Parallel()

	if got := KindOf(fmt.Errorf("read run: %w", sql.ErrNoRows)); got != KindNotFound {
		t.Errorf("sql.ErrNoRows = %q, want %q", got, KindNotFound)
	}
	if got := KindOf(fmt.Errorf("stat: %w", fs.ErrNotExist)); got != KindNotFound {
		t.Errorf("fs.ErrNotExist = %q, want %q", got, KindNotFound)
	}

	// A real *fs.PathError wrapping syscall.ENOENT: the sentinel is reachable
	// only through the Is hook, which is the case worth guarding.
	_, err := os.Open(filepath.Join(t.TempDir(), "no-such-index.db"))
	if err == nil {
		t.Fatal("expected a missing-file error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("test premise broken: %v is not fs.ErrNotExist", err)
	}
	if got := KindOf(fmt.Errorf("open index: %w", err)); got != KindNotFound {
		t.Errorf("KindOf(*fs.PathError) = %q, want %q", got, KindNotFound)
	}
}

func TestJoinedErrorsAreWalked(t *testing.T) {
	t.Parallel()

	joined := errors.Join(errors.New("plain"), New(KindCorrupt, "doctor.verify", "dangling edges"))
	if got := KindOf(fmt.Errorf("verify: %w", joined)); got != KindCorrupt {
		t.Fatalf("KindOf(joined) = %q, want %q", got, KindCorrupt)
	}

	// Multiple %w verbs in one fmt.Errorf take the same path.
	multi := fmt.Errorf("a: %w, b: %w", errors.New("plain"), New(KindInvalid, "config.Validate", "bad mode"))
	if got := KindOf(multi); got != KindInvalid {
		t.Fatalf("KindOf(multi-%%w) = %q, want %q", got, KindInvalid)
	}
}

func TestErrorMessageShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"op and cause", Wrap(KindInvalid, "config.Validate", errors.New("bad mode")), "config.Validate: bad mode"},
		{"no op", Wrap(KindInvalid, "", errors.New("bad mode")), "bad mode"},
		{"new", New(KindNotFound, "registry.CurrentRun", "no committed index run"), "registry.CurrentRun: no committed index run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFieldsNeverAppearInMessage(t *testing.T) {
	t.Parallel()

	// The structural half of "Fields must not carry secrets": even a field
	// that should never have been set cannot reach a log line or the CLI on
	// its own.
	err := Wrap(KindInvalid, "config.Load", errors.New("bad value")).
		WithField("config_key", "accuracy.mode").
		WithField("oops", "s3cret-token")

	if msg := err.Error(); msg != "config.Load: bad value" {
		t.Fatalf("Error() = %q; Fields must not be rendered", msg)
	}
	if msg := fmt.Sprintf("%v", error(err)); msg != "config.Load: bad value" {
		t.Fatalf("%%v = %q; Fields must not be rendered", msg)
	}
}

func TestWithFieldChains(t *testing.T) {
	t.Parallel()

	err := New(KindConflict, "registry.CommitRun", "already committed").
		WithField("run_id", int64(3)).
		WithField("index_run_status", "ready")

	if len(err.Fields) != 2 || err.Fields["run_id"] != int64(3) {
		t.Fatalf("Fields = %v", err.Fields)
	}
	if KindOf(err) != KindConflict {
		t.Error("WithField changed the kind")
	}
}

// --- benchmarks -----------------------------------------------------------

// deepChain builds the 5-deep chain a realistic retry loop inspects: a driver
// error, a kind annotation, and three plain fmt.Errorf wraps on top.
func deepChain() error {
	err := error(Wrap(KindTransient, "store.Exec", stubCode(sqliteBusy)))
	err = fmt.Errorf("registry: commit run 7: %w", err)
	err = fmt.Errorf("publish index revision: %w", err)
	err = fmt.Errorf("indexing failed: %w", err)
	return err
}

// unclassifiedChain is the worst case: nothing to find, so the walk runs to
// the end of the chain.
func unclassifiedChain() error {
	err := errors.New("root")
	for i := 0; i < 4; i++ {
		err = fmt.Errorf("layer %d: %w", i, err)
	}
	return err
}

func BenchmarkKindOf(b *testing.B) {
	err := deepChain()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkKind = KindOf(err)
	}
}

func BenchmarkKindOfUnclassified(b *testing.B) {
	err := unclassifiedChain()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkKind = KindOf(err)
	}
}

func BenchmarkIsRetryable(b *testing.B) {
	err := deepChain()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = IsRetryable(err)
	}
}

func BenchmarkIsRetryableUnclassified(b *testing.B) {
	err := unclassifiedChain()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = IsRetryable(err)
	}
}

var (
	sinkKind Kind
	sinkBool bool
)
