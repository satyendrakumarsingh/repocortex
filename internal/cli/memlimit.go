package cli

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// DefaultIndexMemoryLimit is the soft heap limit `repocortex index` runs
// under when nothing else sets one and the repository is of ordinary size.
//
// A soft limit (runtime/debug.SetMemoryLimit) does not stop the process at a
// size; it makes the collector work harder as the heap approaches it, so
// garbage that would have waited for the next cycle is returned sooner. An
// index run is a stream of parse trees that die young next to a graph that
// lives, which is the case it suits.
//
// MEASURED, a full index of 561 C files (17.5 MiB of source, PostgreSQL's
// parser, 4 workers), median of three, on a 14-core Apple machine:
//
//	limit      wall     peak RSS
//	none       15.5 s   1,114 MB
//	1 GiB      15.5 s   1,029 MB   -8%
//	768 MiB    15.3 s     918 MB   -18%   (750 MiB measured)
//	500 MiB    16.1 s     844 MB   -24%   +4% wall
//
// 768 MiB is where the saving is still free: a fifth of the peak for no
// measurable time, where 500 MiB buys six points more for a few percent of
// wall clock.
const DefaultIndexMemoryLimit int64 = 768 << 20

// DefaultMemoryLimitMaxSource is the most source a run may discover for the
// default limit to apply. Past it the live heap is bigger than the limit, and
// a limit below the live set only makes the collector run flat out: 14,000
// files and 117 MiB of source took 283 s under 768 MiB against 254 s without
// (+11%), with no lower peak (1,818 MB against 1,414 MB, one run each). The cut
// is 64 MiB, between that and the 17.5 MiB that gained. An explicit limit is
// always applied.
const DefaultMemoryLimitMaxSource int64 = 64 << 20

// memoryLimitEnv names the environment variable that sets the index memory
// limit: a size such as 1GiB or 512MB, a plain number of bytes, or "off".
const memoryLimitEnv = "REPOCORTEX_MEMORY_LIMIT"

// indexMemoryLimit is the soft memory limit of one index run.
type indexMemoryLimit struct {
	previous int64
	applied  bool
	// deferred is true when only the default is in play, which waits to see
	// how much source the run found; see DefaultMemoryLimitMaxSource.
	deferred bool
	log      *slog.Logger
}

// newIndexMemoryLimit resolves the limit for an index run and, when it is an
// explicit one, applies it. flag is the value of --memory-limit ("" when not
// given). Precedence: GOMEMLIMIT (left alone: the runtime has applied it),
// then the flag, then REPOCORTEX_MEMORY_LIMIT, then the default, which is
// applied by Discovered.
func newIndexMemoryLimit(flag string, log *slog.Logger) (*indexMemoryLimit, error) {
	m := &indexMemoryLimit{log: log}
	if os.Getenv("GOMEMLIMIT") != "" {
		return m, nil
	}
	spec := flag
	if spec == "" {
		spec = os.Getenv(memoryLimitEnv)
	}
	if spec == "" {
		m.deferred = true
		return m, nil
	}
	n, err := parseMemoryLimit(spec)
	if err != nil {
		return m, err
	}
	m.set(n)
	return m, nil
}

func (m *indexMemoryLimit) set(limit int64) {
	if limit <= 0 || m.applied {
		return
	}
	m.previous = debug.SetMemoryLimit(limit)
	m.applied = true
	m.log.Debug("soft memory limit for this run", "bytes", limit)
}

// Discovered is told how much source the run found, and applies the default
// limit when that is within DefaultMemoryLimitMaxSource.
func (m *indexMemoryLimit) Discovered(_ int, sourceBytes int64) {
	if m == nil || !m.deferred || sourceBytes > DefaultMemoryLimitMaxSource {
		return
	}
	m.set(DefaultIndexMemoryLimit)
}

// Restore puts the previous limit back.
func (m *indexMemoryLimit) Restore() {
	if m != nil && m.applied {
		debug.SetMemoryLimit(m.previous)
		m.applied = false
	}
}

// parseMemoryLimit reads "off" (0), a byte count, or a count with a KB, MB,
// GB (powers of 1000) or KiB, MiB, GiB (powers of 1024) suffix.
func parseMemoryLimit(s string) (int64, error) {
	t := strings.TrimSpace(s)
	switch strings.ToLower(t) {
	case "off", "none", "0":
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30},
		{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1000 * 1000 * 1000},
		{"b", 1},
	}
	lower := strings.ToLower(t)
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			mult = u.mult
			lower = strings.TrimSpace(strings.TrimSuffix(lower, u.suffix))
			break
		}
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("memory limit %q: want a size such as 1GiB or 512MB, a number of bytes, or off", s)
	}
	return n * mult, nil
}
