package parse

import (
	"context"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// stalledByAMacro is five lines cut from google/benchmark's
// test/benchmark_test.cc. The grammar's first two passes die at the macro in
// front of the function's name, and the third full-parse retry pass of the
// parser library then spends the whole wall-clock ceiling inside one dispatch,
// at iteration 2,299 of 295,950 for the whole file, where nothing counts it.
const stalledByAMacro = "#endif\n" +
	"int BENCHMARK_NOINLINE Factorial(int n) {\n" +
	"BENCHMARK_TEMPLATE1_CAPTURE(BM_template1_capture, void, foo, 24UL);\n" +
	"BENCHMARK(BM_BenchmarkName);\n" +
	"template <typename type>\n"

// Before the first parse of a C-family file had a shorter allowance than the
// ceiling, this input took the full minute and was recorded as incomplete on a
// wall-clock verdict. It now takes the allowance (two seconds for a file this
// small), and the macro mask, which parses it in milliseconds, reads it.
func TestAStalledParseIsRescuedByTheMask(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the first-parse allowance")
	}
	start := time.Now()
	res, err := cppParser(t, "cpp").Parse(context.Background(), "x.cc", []byte(stalledByAMacro))
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > 20*time.Second*probeScale {
		t.Errorf("took %s: the ceiling was waited out", took.Round(time.Millisecond))
	}
	if res.Reason == model.ParseReasonBudgetExceeded || res.Reason == model.ParseReasonStoppedEarly {
		t.Errorf("reason %q (%v): the file was recorded as incomplete", res.Reason, res.Err)
	}
	if res.MacrosMasked != 1 {
		t.Errorf("MacrosMasked = %d, want 1", res.MacrosMasked)
	}
}

func TestFirstParseAllowance(t *testing.T) {
	const ms = 1000 // microseconds in a millisecond
	cases := []struct {
		name     string
		cfg      parserConfig
		language string
		size     int
		want     uint64
	}{
		{"a small C++ file", defaultParserConfig, "cpp", 10 << 10, (2*1000 + 10*10) * ms * probeScale},
		{"no source", defaultParserConfig, "c", 0, 2000 * ms * probeScale},
		{"a megabyte of C#", defaultParserConfig, "c_sharp", 1 << 20, min(60*1000, (2*1000+10*1024)*probeScale) * ms},
		{"capped by the ceiling", defaultParserConfig, "c", 100 << 20, uint64(DefaultParseCeiling.Microseconds())},
		{"a language without a mask gets the size-scaled allowance", defaultParserConfig, "go", 10 << 10, uint64((budgetBase + 10*budgetPerKiB).Microseconds())},
		{"a short ceiling is the allowance", parserConfig{ceiling: 500 * time.Millisecond}, "cpp", 10 << 10, 500 * ms},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.probeMicros(tc.language, tc.size); got != tc.want {
				t.Errorf("allowance = %d us, want %d us", got, tc.want)
			}
		})
	}
}
