package mcp

// Never hand back a "reduced" form that is larger than what it replaced.
//
// Summarisers and truncators acquire prefixes, suffixes and explanatory notes
// over time. On a small input those additions can exceed what they removed, and
// the tool then spends MORE of the agent's context to say less. The failure is
// silent: nothing errors, the response is simply worse.
//
// An empty reduction is treated as a failed reduction rather than as an answer
// of nothing. A summariser that produces "" has not concluded the content is
// empty; it has broken.

// NeverWorse returns reduced only when it is strictly smaller and non-empty.
func NeverWorse(full, reduced string) string {
	if reduced == "" {
		return full
	}
	if len(reduced) >= len(full) {
		return full
	}
	return reduced
}
