package parse

import (
	"context"
	"strings"
	"testing"
)

// A Swift file with a misread function-type attribute is retried with the
// attribute blanked; one that also has a hundred errors the mask cannot reach
// is not: the retry parses the whole file again for nothing (see
// maskRetryErrorLimit).
func TestSwiftMaskRetryIsSkippedForAFileWithManyErrors(t *testing.T) {
	const attr = "typealias Handler = @escaping @Sendable (_ data: Int) -> Void\nfunc keep(_ h: @Sendable (_ d: Int) -> Void) {}\n"
	heavy := attr + strings.Repeat("let a = = 1\nfunc f() { if { } }\n", 200)

	var seen []maskRetry
	maskRetryObserver = func(m maskRetry) { seen = append(seen, m) }
	defer func() { maskRetryObserver = nil }()
	p := NewRegistry().ForLanguage("swift")

	parse := func(src string) {
		t.Helper()
		seen = nil
		if _, err := p.Parse(context.Background(), "a.swift", []byte(src)); err != nil {
			t.Fatal(err)
		}
	}

	parse(attr)
	if len(seen) != 1 {
		t.Fatalf("a small file with the attribute was retried %d times, want once", len(seen))
	}

	saved := maskRetryErrorLimit["swift"]
	maskRetryErrorLimit["swift"] = 1 << 30
	parse(heavy)
	maskRetryErrorLimit["swift"] = saved
	if len(seen) != 1 || seen[0].PlainErrors < saved {
		t.Fatalf("the heavy file must reach the limit for this test to mean anything: %+v", seen)
	}

	parse(heavy)
	if len(seen) != 0 {
		t.Errorf("a file with %d errors was retried: %+v", seen[0].PlainErrors, seen)
	}
}
