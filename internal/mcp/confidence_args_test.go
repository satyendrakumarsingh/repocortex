package mcp

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestParseMinConfidenceAcceptsEveryTier(t *testing.T) {
	cases := map[string]model.Confidence{
		"exact":  model.ConfidenceExact,
		"high":   model.ConfidenceHigh,
		"medium": model.ConfidenceMedium,
		"low":    model.ConfidenceLow,
	}
	for raw, want := range cases {
		got, err := parseMinConfidence(raw)
		if err != nil {
			t.Errorf("parseMinConfidence(%q): unexpected error %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("parseMinConfidence(%q) = %q, want %q", raw, got, want)
		}
	}
}

// An empty value means "the caller did not ask for a threshold", which is not
// an error. It must map to low, which admits everything.
func TestParseMinConfidenceTreatsEmptyAsLow(t *testing.T) {
	got, err := parseMinConfidence("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != model.ConfidenceLow {
		t.Errorf("got %q, want %q", got, model.ConfidenceLow)
	}
}

// A misspelling must be refused, not silently downgraded. An agent that asks
// for "hgih" and receives every low-confidence edge has been lied to.
func TestParseMinConfidenceRejectsAnUnknownTier(t *testing.T) {
	if _, err := parseMinConfidence("probable"); err == nil {
		t.Fatal("expected an error for an unknown tier, got nil")
	}
}

func TestParseMinConfidenceIsCaseInsensitiveAndTrims(t *testing.T) {
	got, err := parseMinConfidence("  HIGH ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != model.ConfidenceHigh {
		t.Errorf("got %q, want %q", got, model.ConfidenceHigh)
	}
}
