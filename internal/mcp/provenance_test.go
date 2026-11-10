package mcp

import (
	"strings"
	"testing"
)

func TestProvenanceMatchingHeadIsNotStale(t *testing.T) {
	p := ProvenanceFor("abc123", "abc123", 30)
	if !p.HeadMatchesBuild {
		t.Error("identical SHAs must report HeadMatchesBuild")
	}
	if p.Caveat == "" {
		t.Error("even a matching build must carry the uncommitted-changes caveat")
	}
}

func TestProvenanceDifferentHeadIsStale(t *testing.T) {
	p := ProvenanceFor("abc123", "def456", 30)
	if p.HeadMatchesBuild {
		t.Error("different SHAs must not report HeadMatchesBuild")
	}
	if !strings.Contains(strings.ToLower(p.Caveat), "reindex") {
		t.Errorf("a stale build must tell the agent to reindex, got %q", p.Caveat)
	}
}

// The lesson code-review-graph learned the hard way: comparing commits says
// NOTHING about staged, unstaged or untracked files, and a field named
// "is_stale: false" implies otherwise. The caveat must always be present.
func TestProvenanceNeverClaimsACleanWorktree(t *testing.T) {
	p := ProvenanceFor("abc123", "abc123", 0)
	lower := strings.ToLower(p.Caveat)
	if !strings.Contains(lower, "uncommitted") && !strings.Contains(lower, "unstaged") {
		t.Errorf("the caveat must state that uncommitted changes are invisible, got %q", p.Caveat)
	}
}

// An unknown HEAD — not a git repository, or git unavailable — must not be
// rendered as a match.
func TestProvenanceUnknownHeadIsNotAMatch(t *testing.T) {
	p := ProvenanceFor("abc123", "", 10)
	if p.HeadMatchesBuild {
		t.Error("an unknown HEAD must never report a match")
	}
}

// A run indexed on a non-repository has no BuiltAtSHA. The generic
// mismatch wording ("built from  but HEAD is ...") would render with a
// blank commit; this must be worded as its own case instead.
func TestProvenanceEmptyBuiltAtSHAIsWordedDistinctly(t *testing.T) {
	p := ProvenanceFor("", "abc123", 10)
	if p.HeadMatchesBuild {
		t.Error("an empty build SHA must never report a match")
	}
	if strings.Contains(p.Caveat, "built from  but") {
		t.Errorf("caveat renders the empty build SHA into the mismatch template, got %q", p.Caveat)
	}
	lower := strings.ToLower(p.Caveat)
	if !strings.Contains(lower, "no commit") {
		t.Errorf("caveat should explain the index recorded no commit, got %q", p.Caveat)
	}
}

func TestProvenanceCarriesAge(t *testing.T) {
	p := ProvenanceFor("abc123", "abc123", 4242)
	if p.AgeSeconds != 4242 {
		t.Errorf("AgeSeconds = %d, want 4242", p.AgeSeconds)
	}
}
