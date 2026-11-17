package mcp

// What the index was built from, and what it cannot tell you.
//
// The trap, which code-review-graph documented after shipping it: a boolean
// named is_stale=false reads as "this answer reflects your working tree". It
// does not. Comparing the build's commit against HEAD says nothing about
// staged, unstaged or untracked files, and an agent that believes otherwise
// will act on an answer describing code the user already changed.
//
// So HeadMatchesBuild is narrowly named — it claims exactly the commit
// comparison and nothing more — and Caveat is ALWAYS populated, including on
// the happy path, because the happy path is where the overclaim happens.

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/autorun"
)

// Provenance describes the index behind an answer.
type Provenance struct {
	// BuiltAtSHA is the commit the index was built from. Empty when the
	// project is not a git repository.
	BuiltAtSHA string `json:"built_at_sha,omitempty"`

	// HeadSHA is the repository's current HEAD. Empty when unknown.
	HeadSHA string `json:"head_sha,omitempty"`

	// HeadMatchesBuild compares COMMITS ONLY. It is deliberately not called
	// "fresh" or "is_stale".
	HeadMatchesBuild bool `json:"head_matches_build"`

	// AgeSeconds is how long ago the index was built.
	AgeSeconds int64 `json:"age_seconds"`

	// LastResolvedAt is the RFC3339 timestamp when references were resolved.
	LastResolvedAt string `json:"last_resolve_at,omitempty"`

	// ExactShare is the fraction of resolved references proven by a compiler (0.0 to 1.0).
	ExactShare float64 `json:"exact_share,omitempty"`

	// LastAutoIndex is the last background re-index a git hook started, and
	// whether it succeeded. A failed one means the index may be behind the
	// code even when HeadMatchesBuild is true: the hook ran and the update
	// did not land.
	LastAutoIndex *autorun.Run `json:"last_auto_index,omitempty"`

	// Caveat is always set. It states what the comparison above does not cover.
	Caveat string `json:"caveat"`
}

const uncommittedCaveat = "This compares commits only. Staged, unstaged and untracked changes are " +
	"invisible to the index and are NOT reflected here — a matching commit does not mean the " +
	"working tree matches."

// ProvenanceFor builds the envelope.
//
// An empty headSHA never yields a match: not knowing HEAD and HEAD being equal
// are different states, and collapsing them would report freshness RepoCortex
// cannot establish.
func ProvenanceFor(builtAtSHA, headSHA string, ageSeconds int64) *Provenance {
	matches := builtAtSHA != "" && headSHA != "" && builtAtSHA == headSHA

	caveat := uncommittedCaveat
	switch {
	case headSHA == "":
		caveat = "HEAD could not be determined, so index freshness is unknown. " + uncommittedCaveat
	case builtAtSHA == "":
		caveat = "The index recorded no commit, so it cannot be compared against HEAD. " + uncommittedCaveat
	case !matches:
		caveat = fmt.Sprintf(
			"The index was built from %s but HEAD is %s. Results may describe code that has changed; "+
				"run `repocortex index` to reindex. %s",
			shortSHA(builtAtSHA), shortSHA(headSHA), uncommittedCaveat)
	}

	return &Provenance{
		BuiltAtSHA:       builtAtSHA,
		HeadSHA:          headSHA,
		HeadMatchesBuild: matches,
		AgeSeconds:       ageSeconds,
		Caveat:           caveat,
	}
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
