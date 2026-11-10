package context

// Request is one context assembly.
type Request struct {
	// Task is the plain-English description of the work. Required; a blank
	// task is an error rather than an empty pack, because it is always a
	// caller bug and returning "nothing matched" would hide it.
	Task string

	// Budget bounds the result. The zero value means "use the defaults".
	Budget Budget

	// Tuning overrides the ranking policy. nil means DefaultTuning. Exposed so
	// the weights are configurable from one place rather than being recompiled,
	// and so a test can prove a weight actually does what its comment claims.
	Tuning *Tuning
}

// Item is one unit of context, with the reason it earned its tokens.
type Item struct {
	// Kind is the entity kind: class, interface, method, function, table, test
	// or file. It uses model.EntityKind values so an agent sees the same
	// vocabulary the rest of RepoCortex speaks.
	Kind string `json:"kind"`
	Name string `json:"name"`
	FQN  string `json:"fqn,omitempty"`
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`

	// Relevance is the composite score. Comparable within one pack only: it is
	// not a probability and carries no meaning across tasks.
	Relevance float64 `json:"relevance"`

	// Why is never empty. An item an agent cannot justify to the human reading
	// its output is an item that should not have been sent.
	Why string `json:"why"`

	// Confidence describes the RELATIONSHIP that justified inclusion, not the
	// existence of the symbol: the minimum confidence along the graph path for
	// an expanded item, "low" for a naming-convention or text match. It follows
	// model.Confidence, so "exact" still means a compiler proved it.
	Confidence string `json:"confidence,omitempty"`

	// Excerpt is bounded, untrusted repository source. Present only on the few
	// items where reading the code is cheaper than fetching it.
	Excerpt string `json:"excerpt,omitempty"`
}

// Exclusion is a candidate that was considered and left out.
//
// Reporting these is a product requirement, not diagnostics. An agent (or a
// human reviewing the agent) can only trust a bounded answer if the boundary is
// visible; "we left out MarketingReport because nothing connects it to the
// callback flow" is a checkable claim, while silence is not.
type Exclusion struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// Pack is the assembled context.
type Pack struct {
	Task string `json:"task"`

	// Interpretation is what RepoCortex understood the task to be. It exists so a
	// wrong answer can be diagnosed in one glance: almost every bad pack comes
	// from a misread task, not from bad ranking.
	Interpretation string `json:"interpretation"`

	Items    []Item      `json:"items,omitempty"`
	Excluded []Exclusion `json:"excluded,omitempty"`

	// Notes carry coverage caveats: truncation, bounded traversals, areas the
	// index does not cover. Never empty when something was cut.
	Notes []string `json:"notes,omitempty"`

	// TokenEstimate is an ESTIMATE, computed as bytes/4 over the pack's text.
	// Real tokenisers disagree with it by 10-20% depending on how much of the
	// content is code, and it is reported so a caller can budget, never as a
	// billing figure.
	TokenEstimate int `json:"token_estimate"`
}

// bytesPerToken is the divisor behind TokenEstimate.
//
// Four bytes per token is the widely used rule of thumb for English text under
// BPE tokenisers. Code tokenises slightly worse (punctuation and identifiers
// fragment), so this estimate tends to run low on excerpt-heavy packs. It is
// used only to decide when to stop adding material, where being approximately
// right is enough and a real tokeniser would be a dependency and a per-model
// answer.
const bytesPerToken = 4

// EstimateTokens returns the approximate token cost of the whole pack,
// reporting included.
func (p Pack) EstimateTokens() int {
	n := p.contentBytes()
	for _, ex := range p.Excluded {
		n += len(ex.Name) + len(ex.Path) + len(ex.Reason)
	}
	for _, note := range p.Notes {
		n += len(note)
	}
	return n / bytesPerToken
}

// ContentTokens estimates only the material the agent has to READ: the task,
// RepoCortex's reading of it, and the items.
//
// This, not EstimateTokens, is what Budget.MaxTokensEstimate bounds, and the
// distinction is deliberate rather than convenient. Notes and exclusions are
// RepoCortex's own reporting about the answer, they GROW as the budget bites, and
// trimming them to fit a budget would mean the tighter the budget the less
// honest the pack — exactly backwards. They are bounded separately, by
// Thresholds.MaxExclusions.
func (p Pack) ContentTokens() int { return p.contentBytes() / bytesPerToken }

func (p Pack) contentBytes() int {
	n := len(p.Task) + len(p.Interpretation)
	for _, it := range p.Items {
		n += len(it.Kind) + len(it.Name) + len(it.FQN) + len(it.Path) + len(it.Why) + len(it.Excerpt)
	}
	return n
}

// itemLines counts the excerpt lines an item contributes to the line budget.
func itemLines(it Item) int {
	if it.Excerpt == "" {
		return 0
	}
	n := 1
	for _, r := range it.Excerpt {
		if r == '\n' {
			n++
		}
	}
	return n
}
