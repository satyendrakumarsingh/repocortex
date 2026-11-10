package mcp

// How a number was arrived at, stated in the response rather than in the docs.
//
// RepoCortex publishes precision figures for its confidence tiers and token
// savings for its context packs. Those two numbers have completely different
// standing: one is adjudicated against a compiler index, the other divides
// bytes by four. Presenting them in the same envelope without saying which is
// which is the kind of quiet overclaim this project exists not to make.
//
// The tests are the enforcement. BasisTokenCount may never be
// BasisMeasured while internal/context/pack.go:92 uses bytesPerToken = 4.

// Basis labels a number's provenance.
type Basis string

const (
	// BasisMeasured: adjudicated against ground truth external to RepoCortex —
	// today, a compiler-backed SCIP index.
	BasisMeasured Basis = "measured"

	// BasisInferred: derived from RepoCortex's own heuristics. Internally
	// consistent, externally unvalidated.
	BasisInferred Basis = "inferred"

	// BasisEstimated: an approximation with a known, documented error, such as
	// the bytes/4 token estimate.
	BasisEstimated Basis = "estimated"
)

// BasisTokenCount is the basis of every token number RepoCortex emits.
//
// internal/context/pack.go:92 uses bytesPerToken = 4 and its own comment says
// the estimate "tends to run low on excerpt-heavy packs". Until a real BPE
// tokenizer replaces it, a token count is an estimate and says so. Changing
// this to BasisMeasured requires replacing that heuristic first; the test
// enforces the pairing.
const BasisTokenCount = BasisEstimated
