package search

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"handleCallback", []string{"handle", "callback"}},
		{"TransactionService", []string{"transaction", "service"}},
		{"handle_callback", []string{"handle", "callback"}},
		{"HANDLE_CALLBACK", []string{"handle", "callback"}},
		{"HTTPServer", []string{"http", "server"}},
		{"updateStatusByID", []string{"update", "status", "by", "id"}},
		{"src/payment/CallbackController.java", []string{"src", "payment", "callback", "controller", "java"}},
		{"simple", []string{"simple"}},
		{"", nil},
		{"a", nil}, // single chars are noise
		{"_", nil}, // separators only
		{"XMLHTTPRequest", []string{"xmlhttp", "request"}},
	}
	for _, c := range cases {
		got := SplitIdentifier(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("SplitIdentifier(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExpandIdentifiers(t *testing.T) {
	got := ExpandIdentifiers("void handleCallback(String txnId) { transactionService.updateStatus(txnId); }")
	for _, want := range []string{"handle", "callback", "transaction", "service", "update", "status"} {
		if !strings.Contains(got, want) {
			t.Errorf("expansion missing %q; got %q", want, got)
		}
	}
}

func TestExpandIdentifiersDeduplicates(t *testing.T) {
	got := ExpandIdentifiers("callbackHandler callbackHandler callbackHandler")
	if n := strings.Count(got, "callback"); n != 1 {
		t.Errorf("expected 'callback' once, got %d in %q", n, got)
	}
}

// FuzzSplitIdentifier ensures hostile or malformed source can never panic or
// hang the indexer. Parser-adjacent code runs over untrusted repositories.
func FuzzSplitIdentifier(f *testing.F) {
	for _, s := range []string{"handleCallback", "", "_", "ÜberService", "a_B_c", "😀Emoji", strings.Repeat("aB", 500)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		parts := SplitIdentifier(s)
		for _, p := range parts {
			if utf8.RuneCountInString(p) < 2 {
				t.Fatalf("emitted sub-2-char fragment %q from %q", p, s)
			}
			if p != strings.ToLower(p) {
				t.Fatalf("emitted non-lowercased fragment %q from %q", p, s)
			}
		}
		_ = ExpandIdentifiers(s)
	})
}
