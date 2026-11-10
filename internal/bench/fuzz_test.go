package bench

import (
	"strings"
	"testing"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

// FuzzLoadCases hunts the vacuous green.
//
// A case file is hand-edited, pasted, piped in and occasionally truncated
// mid-write, and this loader runs in CI. Two failures matter here and they are
// not the same. The first is a decoder that panics or hangs on malformed input:
// yaml.v3 has panicked on malformed documents within living memory, which is
// why decodeCases carries a recover, and a benchmark loader that takes CI down
// because someone pasted junk is a denial of service.
//
// The second is worse because it is silent. A case that survives Validate but
// carries nothing to measure contributes a green row and asserts nothing, and a
// suite of them reports a high pass rate while measuring literally nothing —
// the number is high, so nobody looks. So this asserts what a validated case
// must carry, and then scores it against an empty pack to hold the whole
// pipeline (Score, Summarise, Markdown) to not-panicking on decoder output
// nobody wrote by hand.
func FuzzLoadCases(f *testing.F) {
	for _, s := range []string{
		"- id: a\n  task: do the thing\n  expect: [\"pay/Controller.java\"]\n  forbid: [\"marketing/\"]\n  max_tokens: 100\n",
		"cases:\n  - id: a\n    task: t\n    expect: [x]\n",
		`[{"id":"a","task":"t","expect":["x"],"forbid":["y"],"max_tokens":10}]`,
		`{"cases":[{"id":"a","task":"t","expect":["x"]}]}`,
		// The shapes that decode but measure nothing.
		"- id: a\n  task: t\n  expect: []\n",
		"- id: \"  \"\n  task: t\n  expect: [x]\n",
		"- id: a\n  task: t\n  expect: [\"\"]\n",
		"- id: a\n  task: t\n  expects: [x]\n",
		"- id: a\n  task: t\n  expect: [x]\n- id: a\n  task: t2\n  expect: [y]\n",
		// Malformed, truncated, and the BOM a Windows editor prepends.
		"\xef\xbb\xbf- id: a\n  task: t\n  expect: [x]\n",
		"[{\"id\":",
		"- id: a\n\t  bad: tab\n",
		"&a [*a]",
		"[[[[[[[[[[",
		"",
		"   ",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		cases, err := LoadCases([]byte(src))
		if err != nil {
			if cases != nil {
				t.Fatalf("error %v alongside %d cases", err, len(cases))
			}
			return
		}
		// Zero cases is a load error, not an empty result: a suite built from
		// none reports 0/0 passed and reads as green.
		if len(cases) == 0 {
			t.Fatalf("no error and no cases, from %q", src)
		}
		if len(src) > maxCaseFileBytes {
			t.Fatalf("accepted a %d-byte file, over the %d limit", len(src), maxCaseFileBytes)
		}

		if err := Validate(cases); err != nil {
			// Validate is allowed to reject; what it must not do is accept a
			// case that cannot measure anything, which is checked below.
			return
		}
		seen := make(map[string]bool, len(cases))
		for i, c := range cases {
			id := strings.TrimSpace(c.ID)
			if id == "" {
				t.Fatalf("case %d passed Validate with no id, from %q", i, src)
			}
			if seen[id] {
				t.Fatalf("case %d passed Validate with duplicate id %q", i, id)
			}
			seen[id] = true
			if strings.TrimSpace(c.Task) == "" {
				t.Fatalf("case %q passed Validate with no task", id)
			}
			if len(c.Expect) == 0 {
				t.Fatalf("case %q passed Validate with nothing to expect: it would pass without measuring anything", id)
			}
			for _, want := range c.Expect {
				if strings.TrimSpace(want) == "" {
					t.Fatalf("case %q passed Validate with a blank expectation", id)
				}
			}
		}

		// An empty pack cannot satisfy a non-empty Expect list, so every
		// validated case must score as a failure. A case that PASSES against
		// nothing is the vacuous green this benchmark exists to make
		// impossible.
		results := make([]Result, 0, len(cases))
		for _, c := range cases {
			r := Score(c, devctx.Pack{})
			if r.Passed {
				t.Fatalf("case %q passed against an empty pack: %#v", c.ID, r)
			}
			if r.Recall < 0 || r.Recall > 1 {
				t.Fatalf("recall %v outside 0..1 for case %q", r.Recall, c.ID)
			}
			if len(r.Found)+len(r.Missing) != len(c.Expect) {
				t.Fatalf("found+missing (%d+%d) does not partition expect (%d) for case %q",
					len(r.Found), len(r.Missing), len(c.Expect), c.ID)
			}
			results = append(results, r)
		}

		suite := Summarise(results)
		if suite.Cases != len(results) {
			t.Fatalf("summary counts %d cases against %d results", suite.Cases, len(results))
		}
		if suite.Passed != 0 {
			t.Fatalf("%d cases passed against an empty pack", suite.Passed)
		}
		// Markdown is what a human reads in CI. A row that breaks the table
		// shifts every column after it and misreports the numbers while still
		// rendering, so the escaping has to survive whatever a case file says.
		md := suite.Markdown()
		if !strings.HasPrefix(md, "# AgentBench\n") {
			t.Fatalf("report lost its heading: %q", md)
		}
		for _, line := range strings.Split(md, "\n") {
			if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| --- ") {
				continue
			}
			if got := strings.Count(line, "|") - strings.Count(line, `\|`); got != 6 {
				t.Fatalf("table row has %d unescaped pipes, want 6: %q", got, line)
			}
		}
	})
}
