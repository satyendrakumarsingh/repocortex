package report

import (
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func impacted(n int) []impact.Impacted {
	out := make([]impact.Impacted, n)
	for i := range out {
		out[i] = impact.Impacted{
			Symbol:     capability.Symbol{Name: fmt.Sprintf("Sym%02d", i), FilePath: "a.go"},
			Depth:      1,
			Confidence: model.ConfidenceHigh,
		}
	}
	return out
}

func TestRenderPR(t *testing.T) {
	tests := []struct {
		name    string
		in      PRInput
		want    []string
		notWant []string
	}{
		{
			name: "nothing ran",
			in:   PRInput{},
			want: []string{PRMarker, "| Blast radius | not run |", "| Architecture | not run |", "| Blindspots | not run |", "Reported by [RepoCortex]"},
		},
		{
			name: "ran and found nothing is not 'not run'",
			in:   PRInput{Missed: &missed.Report{}, Arch: &architecture.Report{RulesEvaluated: 3}},
			want: []string{"| Blindspots | 0 (0 high) |", "| Architecture | 0 rule violations, 0 data-rule violations |"},
		},
		{
			name: "no rules declared is not zero violations",
			in:   PRInput{Arch: &architecture.Report{}},
			want: []string{"| Architecture | no rules declared |"},
		},
		{
			name: "stale index is called out",
			in: PRInput{Impact: &impact.DiffReport{
				BaseRev: "main", HeadRev: "HEAD", IndexedRevision: "abc123", RevisionMatchesRange: false,
			}},
			want: []string{"**Stale index.**", "built at abc123"},
		},
		{
			name: "rows are bounded and the overflow is counted",
			in: PRInput{MaxRows: 20, Impact: &impact.DiffReport{
				RevisionMatchesRange: true, SeedsFound: 1, Impacted: impacted(25),
				Risk: impact.Risk{Level: "high", Score: 80},
			}},
			want:    []string{"**high** risk (score 80): 25 symbols reached from 1 changed symbols", "Sym19", "…and 5 more"},
			notWant: []string{"Sym20"},
		},
		{
			name: "table cells escape pipes",
			in: PRInput{Missed: &missed.Report{Blindspots: []missed.Blindspot{
				{Kind: missed.BlindspotUnresolvedCall, Severity: "high", Target: "a|b"},
			}}},
			want: []string{`a\|b`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderPR(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
			if !strings.HasPrefix(got, PRMarker) {
				t.Error("comment must start with the sticky marker")
			}
		})
	}
}

func TestRenderPRSafety(t *testing.T) {
	rep := &impact.DiffReport{
		RevisionMatchesRange: true, Risk: impact.Risk{Level: "high"},
		Impacted: []impact.Impacted{{
			Symbol: capability.Symbol{Name: "<!-- x -->evil\r\n## All clear", FilePath: "a`b.go"}, Depth: 1,
		}},
		Unknown: []impact.Unknown{{Kind: "k", Detail: "[link](http://x) ![img](http://x) @someone\n## Injected"}},
	}
	got := RenderPR(PRInput{Impact: rep})
	for _, bad := range []string{"<!-- x -->", "\n## All clear", "\n## Injected", "[link](", "![img](", "@someone", "a`b.go"} {
		if strings.Contains(got, bad) {
			t.Errorf("raw %q survived in:\n%s", bad, got)
		}
	}
	if strings.Count(got, PRMarker) != 1 {
		t.Error("marker must appear exactly once")
	}
	if !strings.Contains(got, "&#64;\u200bsomeone") || !strings.Contains(got, "a\\`b.go") {
		t.Errorf("expected escapes missing in:\n%s", got)
	}
}

func TestRenderPRBounds(t *testing.T) {
	t.Run("unknown and data bullets bounded", func(t *testing.T) {
		var us []impact.Unknown
		for i := 0; i < 30; i++ {
			us = append(us, impact.Unknown{Kind: "k", Detail: fmt.Sprintf("d%02d", i)})
		}
		var dv []architecture.DataViolation
		for i := 0; i < 25; i++ {
			dv = append(dv, architecture.DataViolation{RuleID: "r"})
		}
		got := RenderPR(PRInput{MaxRows: 5, Impact: &impact.DiffReport{Unknown: us}, Arch: &architecture.Report{DataViolations: dv}})
		for _, w := range []string{"d04", "…and 25 more", "…and 20 more", "#### Data-rule violations\n\n"} {
			if !strings.Contains(got, w) {
				t.Errorf("missing %q in:\n%s", w, got)
			}
		}
		if strings.Contains(got, "d05") {
			t.Error("unknown list not bounded")
		}
	})
	t.Run("arch and blindspot overflow", func(t *testing.T) {
		var vs []architecture.Violation
		var bs []missed.Blindspot
		for i := 0; i < 8; i++ {
			vs = append(vs, architecture.Violation{RuleID: "r", Severity: "high"})
			bs = append(bs, missed.Blindspot{Kind: missed.BlindspotOrphanTable, Severity: "high", Target: "t"})
		}
		got := RenderPR(PRInput{MaxRows: 3, Arch: &architecture.Report{RulesEvaluated: 1, Violations: vs}, Missed: &missed.Report{Blindspots: bs}})
		if strings.Count(got, "…and 5 more") != 2 {
			t.Errorf("want two overflow lines in:\n%s", got)
		}
	})
	t.Run("MaxRows<=0 uses default", func(t *testing.T) {
		got := RenderPR(PRInput{MaxRows: -1, Impact: &impact.DiffReport{RevisionMatchesRange: true, Impacted: impacted(DefaultPRRows + 2)}})
		if !strings.Contains(got, "…and 2 more") {
			t.Errorf("default bound not applied:\n%s", got)
		}
	})
	t.Run("unknown revision", func(t *testing.T) {
		got := RenderPR(PRInput{Impact: &impact.DiffReport{BaseRev: "a", HeadRev: "b"}})
		if !strings.Contains(got, "built at unknown") {
			t.Errorf("missing unknown revision in:\n%s", got)
		}
	})
	t.Run("dispatch note", func(t *testing.T) {
		got := RenderPR(PRInput{Impact: &impact.DiffReport{RevisionMatchesRange: true, Impacted: impacted(1), Dispatch: make([]impact.Dispatched, 3)}})
		if !strings.Contains(got, "_3 more reached only through interface dispatch (not listed)._") {
			t.Errorf("dispatch note missing:\n%s", got)
		}
	})
	t.Run("total cap", func(t *testing.T) {
		var us []impact.Unknown
		for i := 0; i < 400; i++ {
			us = append(us, impact.Unknown{Kind: "k", Detail: strings.Repeat("x", 250)})
		}
		got := RenderPR(PRInput{MaxRows: 1000, Impact: &impact.DiffReport{Unknown: us}})
		if len(got) > MaxPRCommentChars+len(prTruncatedNote) {
			t.Errorf("body %d chars exceeds cap", len(got))
		}
		if !strings.HasSuffix(got, prTruncatedNote) || !strings.HasPrefix(got, PRMarker) {
			t.Error("truncated body must start with marker and end with note")
		}
	})
}

func TestRenderPRDataRulesOnly(t *testing.T) {
	got := RenderPR(PRInput{Arch: &architecture.Report{
		DataViolations: []architecture.DataViolation{{RuleID: "r", Severity: "high"}},
	}})
	if !strings.Contains(got, "| Architecture | 0 rule violations, 1 data-rule violations, worst high |") {
		t.Errorf("data-only summary wrong:\n%s", got)
	}
	if strings.Contains(got, "no rules declared") {
		t.Error("data rules exist; must not say no rules declared")
	}
}

func TestRenderPRFieldCapAndAutolinks(t *testing.T) {
	huge := strings.Repeat("y", 70000)
	got := RenderPR(PRInput{
		Impact: &impact.DiffReport{Unknown: []impact.Unknown{{Kind: "k", Detail: huge}}},
		Arch:   &architecture.Report{RulesEvaluated: 1},
		Missed: &missed.Report{Blindspots: []missed.Blindspot{{Kind: missed.BlindspotOrphanTable, Severity: "high", Target: "https://x.io www.x.io"}}},
	})
	for _, w := range []string{"| Architecture |", "### Blindspots", "…"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q", w)
		}
	}
	if len(got) > 5000 {
		t.Errorf("huge field not capped: %d chars", len(got))
	}
	for _, bad := range []string{"https://x.io", "www.x.io"} {
		if strings.Contains(got, bad) {
			t.Errorf("autolinkable %q survived", bad)
		}
	}
}

func TestRenderPRDispatchOnly(t *testing.T) {
	got := RenderPR(PRInput{Impact: &impact.DiffReport{RevisionMatchesRange: true, Dispatch: make([]impact.Dispatched, 2)}})
	if !strings.Contains(got, "_2 more reached only through interface dispatch (not listed)._") {
		t.Errorf("dispatch footnote missing:\n%s", got)
	}
}

// mixed builds an Impacted list with the given number of symbols per tier, in
// a fixed order so the rendered output is deterministic.
func mixed(counts map[model.Confidence]int) []impact.Impacted {
	var out []impact.Impacted
	for _, c := range []model.Confidence{
		model.ConfidenceExact, model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow, "",
	} {
		for i := 0; i < counts[c]; i++ {
			out = append(out, impact.Impacted{
				Symbol:     capability.Symbol{Name: fmt.Sprintf("%s%02d", c, i), FilePath: "a.go"},
				Depth:      1,
				Confidence: c,
			})
		}
	}
	return out
}

func TestRenderPRConfidenceBreakdown(t *testing.T) {
	ran := func(im []impact.Impacted, dispatch int) *impact.DiffReport {
		r := &impact.DiffReport{RevisionMatchesRange: true, SeedsFound: 1, Impacted: im,
			Risk: impact.Risk{Level: "high", Score: 80}}
		for i := 0; i < dispatch; i++ {
			r.Dispatch = append(r.Dispatch, impact.Dispatched{
				Impacted: impact.Impacted{Symbol: capability.Symbol{Name: fmt.Sprintf("D%d", i)}}})
		}
		return r
	}
	tests := []struct {
		name    string
		in      PRInput
		want    []string
		notWant []string
	}{
		{
			name:    "not run stays not run",
			in:      PRInput{},
			want:    []string{"| Blast radius | not run |"},
			notWant: []string{"% exact", "How much of the blast radius is proven"},
		},
		{
			name:    "ran and reached nothing has no shares",
			in:      PRInput{Impact: ran(nil, 0)},
			want:    []string{"0 symbols reached from 1 changed symbols |"},
			notWant: []string{"% exact", "How much of the blast radius is proven"},
		},
		{
			name: "mostly proven",
			in:   PRInput{Impact: ran(mixed(map[model.Confidence]int{model.ConfidenceExact: 18, model.ConfidenceHigh: 2}), 0)},
			want: []string{
				"20 symbols reached from 1 changed symbols; 90% exact, 10% high, 0% medium |",
				"### How much of the blast radius is proven",
				"| exact (compiler-proven) | 18 | 90% |",
				"| high | 2 | 10% |",
				"| medium | 0 | 0% |",
				"Shares are of the 20 symbols reached through recorded references",
			},
			notWant: []string{"| low |", "No part of this blast radius is compiler-proven"},
		},
		{
			name: "nothing proven says how to prove it",
			in: PRInput{Impact: ran(mixed(map[model.Confidence]int{
				model.ConfidenceHigh: 1, model.ConfidenceMedium: 1, model.ConfidenceLow: 1}), 0)},
			want: []string{
				"0% exact, 33% high, 33% medium, 33% low",
				"| low | 1 | 33% |",
				"No part of this blast radius is compiler-proven",
				"REPOCORTEX_ACCURACY: auto",
			},
		},
		{
			name: "dispatch is excluded and the exclusion is stated",
			in:   PRInput{Impact: ran(mixed(map[model.Confidence]int{model.ConfidenceExact: 2}), 3)},
			want: []string{
				"5 symbols reached from 1 changed symbols; 100% exact",
				"3 reached only through interface dispatch are not counted",
			},
		},
		{
			name: "an unrecognised confidence is shown, not dropped",
			in:   PRInput{Impact: ran(mixed(map[model.Confidence]int{model.ConfidenceExact: 1, "": 1}), 0)},
			want: []string{"| unlabelled | 1 | 50% |"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderPR(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
			if again := RenderPR(tc.in); again != got {
				t.Error("RenderPR is not deterministic")
			}
		})
	}
}

// TestRenderPRConfidenceBreakdownIsNotAnInjectionPath: confidence strings come
// from JSON input. An unknown one is counted as unlabelled and never echoed
// into the breakdown or the summary row.
func TestRenderPRConfidenceBreakdownIsNotAnInjectionPath(t *testing.T) {
	const evil = "exact|@octocat https://evil.example <b>x</b>"
	im := mixed(map[model.Confidence]int{model.ConfidenceExact: 1})
	im = append(im, impact.Impacted{
		Symbol: capability.Symbol{Name: "Z", FilePath: "z.go"}, Depth: 1, Confidence: model.Confidence(evil)})
	got := RenderPR(PRInput{Impact: &impact.DiffReport{RevisionMatchesRange: true, SeedsFound: 1, Impacted: im}})
	start := strings.Index(got, "### How much of the blast radius is proven")
	end := strings.Index(got, "### Blast radius")
	if start < 0 || end < start {
		t.Fatalf("breakdown or impact table missing:\n%s", got)
	}
	if section := got[start:end]; strings.Contains(section, "octocat") || strings.Contains(section, "evil") {
		t.Errorf("untrusted confidence leaked into the breakdown:\n%s", section)
	}
	if !strings.Contains(got, "| unlabelled | 1 | 50% |") {
		t.Errorf("unknown confidence not bucketed as unlabelled:\n%s", got)
	}
	if strings.Contains(got, "<b>") || strings.Contains(got, "@octocat") {
		t.Errorf("raw untrusted text in output:\n%s", got)
	}
}
