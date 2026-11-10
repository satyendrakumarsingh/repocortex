package compile

import (
	"strings"
	"testing"

	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

// FuzzCompile hunts the unsourced requirement.
//
// The intent is typed by a person and the pack is built from a repository
// RepoCortex did not write, so both arrive as arbitrary text — including text
// that is not valid UTF-8, which is where a rune-indexing normaliser goes
// wrong. But the property this package exists to enforce is the one about
// EVIDENCE: a constraint or a risk with nothing behind it is an opinion, and an
// opinion typeset as a requirement is how an agent ends up confidently obeying
// something no one in the repository ever decided. There is a single exit
// filter for that rule, and this target holds it against inputs no test case
// would think to write.
//
// Determinism is checked alongside it, because a contract that changes between
// two runs of the same input cannot be cached, diffed or trusted, and nothing
// in the pipeline would notice.
func FuzzCompile(f *testing.F) {
	for _, s := range [][5]string{
		{"add idempotency to payment callbacks", "PaymentController.java", kindEndpoint, "handle", "called by the callback route"},
		{"fix the bug", "internal/store/db.go", kindTable, "transactions", "writes the table"},
		{"x", "src/Foo.test.ts", kindTest, "Foo", "covers this area"},
		{"   spaced   out   ", "config/app.yml", kindConfiguration, "app", "declares the setting"},
		{"unicode ünïcödé", "pkg/model/model.go", kindFile, "model.go", ""},
		// The inputs that break a normaliser: nothing, whitespace only, and
		// bytes that are not valid UTF-8.
		{"", "", "", "", ""},
		{" \t\n", "a", kindInterface, "b", "c"},
		{"\xff\xfe", "\xff", "\xff", "\xff", "\xff"},
		{".", "/", kindFile, "", ""},
		{"ends with a dot.", "tests/x_test.go", "", "", ""},
	} {
		f.Add(s[0], s[1], s[2], s[3], s[4], 0.5, int8(3))
	}

	f.Fuzz(func(t *testing.T, intent, path, kind, name, why string, relevance float64, maxScope int8) {
		// Two items rather than one, so the total ordering in selectItems is
		// exercised: ties on relevance are resolved by path, then name, then
		// kind, and a partial comparator there is exactly how "same input,
		// different brief" gets in.
		pack := devctx.Pack{
			Task:           intent,
			Interpretation: why,
			Items: []devctx.Item{
				{Kind: kind, Name: name, Path: path, Why: why, Relevance: relevance, Confidence: confidenceLow},
				{Kind: kind, Name: name, Path: path, Why: why, Relevance: relevance},
			},
			Excluded: []devctx.Exclusion{{Name: name, Path: path, Reason: why}},
			Notes:    []string{why},
		}

		c, err := Compile(intent, pack, Options{MaxScope: int(maxScope)})
		if strings.TrimSpace(intent) == "" {
			if err == nil {
				t.Fatalf("compiled a contract for a blank intent %q", intent)
			}
			return
		}
		if err != nil {
			t.Fatalf("Compile(%q) = %v; only a blank intent is refused", intent, err)
		}

		// The central invariant. Every rule is written to cite its trigger, and
		// the exit filter is what stops a future rule from forgetting to.
		for _, con := range c.Constraints {
			if strings.TrimSpace(con.Text) == "" || con.Evidence == "" {
				t.Fatalf("constraint with no evidence: %#v from intent %q path %q kind %q", con, intent, path, kind)
			}
		}
		for _, r := range c.Risks {
			if strings.TrimSpace(r.Text) == "" || r.Evidence == "" {
				t.Fatalf("risk with no evidence: %#v from intent %q path %q kind %q", r, intent, path, kind)
			}
			if severityRank(r.Severity) == 0 {
				t.Fatalf("risk severity %q ranks below every real severity, so --fail-on cannot see it: %#v", r.Severity, r)
			}
		}

		// The intent is carried verbatim: a reader compares it against
		// Interpretation to see whether a vague ask was read the way it was
		// meant, and a cleaned-up copy hides the vagueness that caused a
		// misread.
		if c.Intent != intent {
			t.Fatalf("intent rewritten: %q became %q", intent, c.Intent)
		}
		// A contract that names no gaps claims to be complete, which this
		// package is never in a position to claim.
		if len(c.Unknowns) == 0 {
			t.Fatalf("no unknowns in a contract, from intent %q", intent)
		}
		// The scope cap is the boundary the package exists to draw; a negative
		// or zero MaxScope means the default, never "unbounded".
		limit := int(maxScope)
		if limit <= 0 {
			limit = defaultMaxScope
		}
		if len(c.Scope) > limit {
			t.Fatalf("%d scope entries against a cap of %d", len(c.Scope), limit)
		}

		md := c.Markdown()
		if !strings.HasPrefix(md, "# Task Contract\n") {
			t.Fatalf("contract lost its heading: %q", md)
		}
		// An empty section reads as "we looked and found nothing", a claim this
		// package cannot make, so a header is never written without a body.
		for _, line := range strings.Split(md, "\n") {
			if !strings.HasPrefix(line, "## ") {
				continue
			}
			if strings.TrimSpace(strings.TrimPrefix(line, "## ")) == "" {
				t.Fatalf("empty section header in %q", md)
			}
		}
		if strings.HasSuffix(md, "\n\n") {
			t.Fatalf("contract ends on an empty section: %q", md)
		}
		if c.TokenEstimate != len(md)/bytesPerToken {
			t.Fatalf("token estimate %d against a %d-byte brief", c.TokenEstimate, len(md))
		}

		// Same input, byte-identical brief. Ordering included.
		again, err := Compile(intent, pack, Options{MaxScope: int(maxScope)})
		if err != nil {
			t.Fatalf("second Compile of the same input failed: %v", err)
		}
		if again.Markdown() != md {
			t.Fatalf("two compilations of the same input differ:\n%q\n%q", md, again.Markdown())
		}
	})
}
