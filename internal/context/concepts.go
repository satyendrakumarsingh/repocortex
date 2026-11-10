package context

import "strings"

// conceptRule defines a trigger condition and the domain terms it maps to.
//
// In real software engineering, task descriptions and questions are phrased in
// terms of symptoms, behaviors, or human prose ("unreliable edges", "never print
// a password", "indexing twice produces different results") that diverge from
// exact codebase identifiers ("DefaultMinConfidence", "Redact", "RegistryDigest").
//
// Algorithmic concept bridging maps these engineering concepts deterministically
// without requiring an external model call or vector database.
type conceptRule struct {
	// triggers are phrases (all lowercase) that indicate this concept.
	// If any phrase appears in the task text, the rule fires.
	triggers []string

	// requiredWords, when non-empty, requires ALL of these words to appear in the task
	// (in any order) for the rule to fire.
	requiredWords []string

	// terms are the code domain terms to expand into the Intent.
	terms []string
}

// conceptRules is the deterministic catalog of domain concept bridges.
var conceptRules = []conceptRule{
	// Impact analysis / confidence floor / blast radius
	{
		triggers: []string{"blast radius", "unreliable edges", "confidence floor", "unreliable edge"},
		terms:    []string{"confidence", "floor", "impact", "defaultminconfidence"},
	},
	// Determinism / index digest / two-phase swap
	{
		triggers:      []string{"same code twice", "different results", "index twice", "indexing twice", "two-phase swap"},
		requiredWords: []string{"twice"},
		terms:         []string{"digest", "registrydigest", "determinism"},
	},
	// Secret and credential redaction / masking / sanitization
	{
		triggers:      []string{"print a password", "print password", "leak password", "secret redaction", "mask password"},
		requiredWords: []string{"password"},
		terms:         []string{"redact", "redaction"},
	},
	// Resolution coverage / unexplained absence / unused code
	{
		triggers: []string{"returned nothing", "code is unused", "empty answer", "resolution coverage", "absence"},
		terms:    []string{"resolution", "resolutionstats", "coverage"},
	},
	// Test discovery / test gap
	{
		triggers: []string{"tests that exercise", "tests cover", "test coverage", "tests that cover"},
		terms:    []string{"findtests", "testgap"},
	},
}

// expandConcepts analyzes the normalized task text and injects domain concept terms.
func expandConcepts(taskLower string, note func(string, float64)) {
	taskWords := strings.Fields(taskLower)
	wordSet := make(map[string]bool, len(taskWords))
	for _, w := range taskWords {
		wordSet[w] = true
	}

	for _, rule := range conceptRules {
		matched := false
		for _, trigger := range rule.triggers {
			if strings.Contains(taskLower, trigger) {
				matched = true
				break
			}
		}
		if !matched && len(rule.requiredWords) > 0 {
			allFound := true
			for _, rw := range rule.requiredWords {
				if !wordSet[rw] && !strings.Contains(taskLower, rw) {
					allFound = false
					break
				}
			}
			if allFound {
				matched = true
			}
		}

		if matched {
			for _, term := range rule.terms {
				note(term, weightConcept)
			}
		}
	}
}
