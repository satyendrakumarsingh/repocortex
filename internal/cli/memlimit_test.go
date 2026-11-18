package cli

import (
	"io"
	"log/slog"
	"runtime/debug"
	"testing"
)

func TestParseMemoryLimit(t *testing.T) {
	for in, want := range map[string]int64{
		"off": 0, "0": 0, "none": 0, "1048576": 1 << 20, "512MiB": 512 << 20, "1GiB": 1 << 30,
		"500MB": 500_000_000, "64 KiB": 64 << 10, "2gb": 2_000_000_000,
	} {
		got, err := parseMemoryLimit(in)
		if err != nil || got != want {
			t.Errorf("parseMemoryLimit(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "-1", "12 parsecs", "99999999999999GiB"} {
		if _, err := parseMemoryLimit(bad); err == nil {
			t.Errorf("parseMemoryLimit(%q) accepted", bad)
		}
	}
}

// GOMEMLIMIT wins over everything: the runtime has applied it already and the
// index run must not replace it. Otherwise the flag, the environment variable
// and the default apply in that order; the default only to a run of ordinary
// size, and the previous limit is put back.
func TestIndexMemoryLimitPrecedence(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	current := func() int64 { return debug.SetMemoryLimit(-1) }
	original := current()
	defer debug.SetMemoryLimit(original)

	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(memoryLimitEnv, "")

	// The default: nothing yet, then applied once the run turns out to be small.
	m, err := newIndexMemoryLimit("", log)
	if err != nil || current() != original {
		t.Fatalf("before discovery: limit %d, err %v", current(), err)
	}
	m.Discovered(500, 17<<20)
	if current() != DefaultIndexMemoryLimit {
		t.Errorf("a small run: limit %d, want the default %d", current(), DefaultIndexMemoryLimit)
	}
	m.Restore()
	if current() != original {
		t.Errorf("limit not restored: %d, want %d", current(), original)
	}

	// A large run keeps no default: the live heap is over it.
	m, _ = newIndexMemoryLimit("", log)
	m.Discovered(14000, 117<<20)
	if current() != original {
		t.Errorf("a large run was limited to %d", current())
	}
	m.Restore()

	t.Setenv(memoryLimitEnv, "2GiB")
	m, _ = newIndexMemoryLimit("", log)
	if current() != 2<<30 {
		t.Errorf("env: limit %d", current())
	}
	m.Restore()

	// An explicit limit applies at once and to a run of any size.
	m, _ = newIndexMemoryLimit("1GiB", log)
	m.Discovered(14000, 117<<20)
	if current() != 1<<30 {
		t.Errorf("flag over env: limit %d", current())
	}
	m.Restore()

	m, _ = newIndexMemoryLimit("off", log)
	m.Discovered(10, 1<<20)
	if current() != original {
		t.Errorf("off changed the limit to %d", current())
	}
	m.Restore()

	t.Setenv("GOMEMLIMIT", "3GiB")
	m, _ = newIndexMemoryLimit("1GiB", log)
	m.Discovered(10, 1<<20)
	if current() != original {
		t.Errorf("GOMEMLIMIT set: the run replaced the limit with %d", current())
	}
	m.Restore()

	t.Setenv("GOMEMLIMIT", "")
	if _, err := newIndexMemoryLimit("bogus", log); err == nil {
		t.Error("a bad --memory-limit was accepted")
	}
}
