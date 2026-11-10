package errs

import (
	"errors"
	"fmt"
	"testing"
)

// stubCode is a stand-in for *sqlite.Error, whose fields are unexported and so
// cannot be constructed from outside the driver. It exercises the duck-typed
// assertion in classifySQLite; TestRealSQLiteBusyIsTransient in the external
// test file proves the driver actually presents errors this way.
type stubCode int

func (c stubCode) Error() string { return fmt.Sprintf("sqlite error (%d)", int(c)) }
func (c stubCode) Code() int     { return int(c) }

func TestSQLiteCodeClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		code int
		want Kind
	}{
		{"SQLITE_BUSY", sqliteBusy, KindTransient},
		{"SQLITE_LOCKED", sqliteLocked, KindTransient},
		{"SQLITE_PROTOCOL", sqliteProtocol, KindTransient},
		{"SQLITE_BUSY_SNAPSHOT", sqliteBusy | (2 << 8), KindTransient},
		{"SQLITE_LOCKED_SHAREDCACHE", sqliteLocked | (1 << 8), KindTransient},
		{"SQLITE_INTERRUPT", sqliteInterrupt, KindCancelled},
		{"SQLITE_CORRUPT", sqliteCorrupt, KindCorrupt},
		{"SQLITE_NOTADB", sqliteNotADB, KindCorrupt},
		{"SQLITE_CONSTRAINT", sqliteConstraint, KindConflict},
		{"SQLITE_CONSTRAINT_CHECK", sqliteConstraint | (11 << 8), KindConflict},
		{"SQLITE_OK", 0, KindInternal},     // unclassified -> a bug, not benign
		{"SQLITE_IOERR", 10, KindInternal}, // deliberately not retryable
		{"SQLITE_FULL", 13, KindInternal},  // a full disk does not clear on retry
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("registry: commit run 7: %w", stubCode(tc.code))
			if got := KindOf(err); got != tc.want {
				t.Fatalf("KindOf(code %d) = %q, want %q", tc.code, got, tc.want)
			}
			if got, want := IsRetryable(err), tc.want == KindTransient; got != want {
				t.Errorf("IsRetryable(code %d) = %v, want %v", tc.code, got, want)
			}
		})
	}
}

func TestNonSQLiteErrorIsNotMisread(t *testing.T) {
	t.Parallel()

	if got := classifySQLite(errors.New("plain")); got != "" {
		t.Errorf("classifySQLite(plain) = %q, want empty", got)
	}
}

func TestExplicitKindOverridesDriverCode(t *testing.T) {
	t.Parallel()

	// Proof the gate can fail: the same busy code, deliberately classified as
	// corrupt by the layer above, must stop being retryable.
	busy := stubCode(sqliteBusy)
	if !IsRetryable(fmt.Errorf("x: %w", busy)) {
		t.Fatal("test premise broken: a bare busy code should be retryable")
	}
	if IsRetryable(Wrap(KindCorrupt, "registry.validateRun", busy)) {
		t.Error("an explicit annotation did not override the driver code")
	}
}
