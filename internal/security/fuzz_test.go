package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FuzzRedact drives the redactor with arbitrary bytes.
//
// CONTRIBUTING.md requires a fuzz target for anything reading untrusted input,
// and Redact reads the most untrusted input in the system: the contents of a
// cloned repository, on their way to a third-party model. Three properties are
// asserted, each guarding a distinct failure:
//
//  1. No panic. A panic here takes down the MCP server mid-response.
//
//  2. No hang. stdlib regexp is RE2, so catastrophic backtracking should be
//     impossible — this target is what makes that a fact about the code rather
//     than a claim about the library, and it fails loudly if someone swaps in
//     an engine that backtracks.
//
//  3. The output is exactly the input with each finding's span replaced by its
//     placeholder — recomputed here from Line/Col/Length ALONE. That is the
//     strong one: it proves the reported position really locates the redacted
//     bytes (a caller building model.Evidence depends on it), that findings do
//     not overlap, and that no secret was widened, narrowed or left behind.
func FuzzRedact(f *testing.F) {
	seeds := []string{
		"",
		"DB_PASSWORD=hunter2\n",
		`aws_key = "AKIAIOSFODNN7EXAMPLE"`,
		"postgres://user:pw@host:5432/db",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----", // unterminated armour
		"token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij",
		"password=[REDACTED:generic-password]", // already redacted
		"pwd=0010 =0001",                       // a placeholder read as the next key
		"[REDACTED:generic-password]=x",
		"token:AeyJ00000000.A0000000.A", // a value that only partly redacts
		"[REDACTED:", "[REDACTED:]", "]REDACTED[",

		// Shapes chosen to be hostile rather than realistic.
		strings.Repeat("password=", 500),
		strings.Repeat("a:", 1000),
		strings.Repeat(`"`, 500) + "password=x",
		"password=" + strings.Repeat("A", 2000),
		strings.Repeat("-", 1000) + "BEGIN PRIVATE KEY",
		strings.Repeat("://:@", 300),
		"password=\x00\x00\x00\n",
		"password=\xff\xfe invalid utf8",
		"password:\r\nsecret:\r\n",
		strings.Repeat("eyJ", 400),
		strings.Repeat("AKIA", 400),
		"\n\n\n\n=\n:\n-----\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, text string) {
		// Bound the corpus so a pathological input is reported as a hang in the
		// redactor rather than as the fuzzer exploring a megabyte of text.
		if len(text) > 64*1024 {
			t.Skip()
		}

		start := time.Now()
		out, findings := Redact(text)
		elapsed := time.Since(start)

		// Redaction is two linear regexp passes. Anything near a second means
		// the engine is backtracking, which would be a denial of service
		// reachable from a committed file.
		if elapsed > 2*time.Second {
			t.Fatalf("Redact took %v on %d bytes", elapsed, len(text))
		}

		lineStarts := indexLines(text)
		var want strings.Builder
		prev := 0
		for i, fnd := range findings {
			if fnd.Length <= 0 {
				t.Fatalf("finding %d has length %d", i, fnd.Length)
			}
			if fnd.Line < 1 || fnd.Line > len(lineStarts) {
				t.Fatalf("finding %d line %d is outside 1..%d", i, fnd.Line, len(lineStarts))
			}
			off := lineStarts[fnd.Line-1] + fnd.Col
			if off < prev {
				t.Fatalf("finding %d overlaps its predecessor (offset %d < %d)", i, off, prev)
			}
			if off+fnd.Length > len(text) {
				t.Fatalf("finding %d runs past the end of the input", i)
			}
			want.WriteString(text[prev:off])
			want.WriteString(fnd.Placeholder())
			prev = off + fnd.Length
		}
		want.WriteString(text[prev:])

		if got := want.String(); got != out {
			t.Fatalf("output does not match the reported findings\n got: %q\nwant: %q\nfindings: %v", out, got, findings)
		}

		// Redaction is applied at more than one layer, so a second pass must
		// change nothing at all.
		again, second := Redact(out)
		if again != out {
			t.Fatalf("not idempotent:\nfirst:  %q\nsecond: %q", out, again)
		}
		if len(second) != 0 {
			t.Fatalf("second pass reported %v on already-redacted text %q", second, out)
		}

		// Detect must agree with Redact, or a caller that only asks "is there a
		// secret here" gets a different answer from the one that redacts.
		if d := Detect(text); len(d) != len(findings) {
			t.Fatalf("Detect found %d, Redact found %d", len(d), len(findings))
		}
	})
}

// FuzzValidatePath checks that no arrangement of separators, dots and escapes
// gets a path past the root. The property is absolute: whatever comes back must
// live inside the evaluated root.
func FuzzValidatePath(f *testing.F) {
	for _, s := range []string{
		"a.go", "./a.go", "../a.go", "../../etc/passwd", "/etc/passwd",
		"a/../../b", "a//b", "a/./b", "...", "....//", `a\..\..\b`,
		"a\x00b", "", " ", "a/", "/", "C:/x", `\\host\share`,
		strings.Repeat("../", 200) + "etc/passwd",
		strings.Repeat("a/", 500) + "b",
	} {
		f.Add(s)
	}

	root := f.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o600); err != nil {
		f.Fatal(err)
	}
	evaluated, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, requested string) {
		if len(requested) > 4096 {
			t.Skip()
		}
		got, err := ValidatePath(root, requested)
		if err != nil {
			if got != "" {
				t.Fatalf("ValidatePath returned both a path %q and an error %v", got, err)
			}
			return
		}
		if !within(evaluated, got) {
			t.Fatalf("ValidatePath(%q) = %q, which is outside %q", requested, got, evaluated)
		}
		if strings.Contains(got, "\x00") {
			t.Fatalf("ValidatePath(%q) returned a path containing NUL", requested)
		}
	})
}

func indexLines(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}
