package context

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// Budget bounds what one context pack may cost the agent that reads it.
//
// Every field is a ceiling, never a target. The assembler stops as soon as
// marginal value falls off (see Thresholds.MarginalFraction), which is usually
// well before any of these limits bind; the limits exist so that a pathological
// task — one naming a symbol every file touches — still produces something an
// agent can read rather than a dump of the repository.
type Budget struct {
	// MaxFiles bounds the number of DISTINCT file paths across all items.
	// Files, not items, because reading a second symbol from a file the agent
	// already has open is nearly free while opening a new file is not.
	MaxFiles int

	// MaxSymbols bounds the number of items.
	MaxSymbols int

	// MaxLines bounds total excerpt lines across the pack.
	MaxLines int

	// MaxTokensEstimate bounds the pack's ESTIMATED token cost. See
	// EstimateTokens: the estimate is bytes/4 and is not exact.
	MaxTokensEstimate int

	// MaxGraphDepth bounds how many hops from a seed the expansion may travel.
	// Depth is the cheapest knob to get wrong: each additional hop multiplies
	// the reachable set, and by hop four "related to the task" has usually
	// decayed into "in the same program".
	MaxGraphDepth int

	// HubDegreeLimit refuses to expand THROUGH a symbol wider than this many
	// edges. Zero, the default, disables it, and withDefaults deliberately does
	// NOT fill it from DefaultBudget: unlike every other field here, an unset
	// value is a complete instruction ("do not prune"), not a forgotten one.
	//
	// Depth is the knob that multiplies the reachable set; this is the knob
	// that stops one wide node from doing the multiplying on its own.
	HubDegreeLimit int
}

// DefaultMaxTokensEstimate is the default token ceiling for one pack.
//
// 8,000 estimated tokens is roughly 4% of a 200k window. That is deliberately
// frugal: a context pack is what the agent reads BEFORE it starts working, and
// it must leave room for the file reads, edits and test output that follow.
// config.Config has no equivalent field, so this constant is the only source.
const DefaultMaxTokensEstimate = 8000

// DefaultBudget returns the budget implied by config.Defaults.
func DefaultBudget() Budget { return BudgetFrom(config.Defaults()) }

// BudgetFrom derives a Budget from resolved configuration.
//
// The context.* keys in .cortex.yml have been parsed and validated since M0
// but nothing consumed them; this is the consumer. Repository config is
// untrusted, but these values can only make a response SMALLER or larger within
// limits the config package already validates (MaxGraphDepth is capped at 10
// there precisely because an unbounded traversal is a denial of service).
func BudgetFrom(cfg config.Config) Budget {
	return Budget{
		MaxFiles:          cfg.MaxFiles,
		MaxSymbols:        cfg.MaxSymbols,
		MaxLines:          cfg.MaxLines,
		MaxTokensEstimate: DefaultMaxTokensEstimate,
		MaxGraphDepth:     cfg.MaxGraphDepth,
	}
}

// withDefaults fills unset fields from DefaultBudget.
//
// Zero means "unset", not "zero allowed": a caller that constructs a Budget by
// hand and forgets a field should get a sane pack, not an empty one.
func (b Budget) withDefaults() Budget {
	d := DefaultBudget()
	if b.MaxFiles <= 0 {
		b.MaxFiles = d.MaxFiles
	}
	if b.MaxSymbols <= 0 {
		b.MaxSymbols = d.MaxSymbols
	}
	if b.MaxLines <= 0 {
		b.MaxLines = d.MaxLines
	}
	if b.MaxTokensEstimate <= 0 {
		b.MaxTokensEstimate = d.MaxTokensEstimate
	}
	if b.MaxGraphDepth <= 0 {
		b.MaxGraphDepth = d.MaxGraphDepth
	}
	return b
}

// The response budget is allocated by RELEVANCE, not by count.
//
// MaxFiles and MaxSymbols are counts, and a count is an equal claim: under the
// policy this replaces, the first three items each received the same fixed
// window of source whether the first outscored the third by 5% or by 500%, and
// an item that had no source to show freed nothing for the items behind it. The
// symptom is easy to measure and embarrassing to read — a weak item shipping its
// whole body while the item the task is actually about is clipped mid-method.
//
// The replacement is three rules, in this order:
//
//	CLIFF     an item far below the strongest is named without source. Applied
//	          over the WHOLE candidate list before any item cap, because a cliff
//	          applied afterwards lets cliff-bound items occupy the cap and hands
//	          their share to nobody.
//	FLOOR     every eligible item gets an equal share of BodyFloorShare of the
//	          envelope, so a diffuse question ("how does settlement work") still
//	          comes back as a spread rather than one file.
//	REMAINDER the rest is split in proportion to score*worth, so a precise
//	          question ("fix handleCallback") concentrates on what it named.
//
// Never cliff everything: an empty answer costs the agent a whole round trip and
// teaches it nothing, so the strongest item is eligible by construction.

// bodyEnvelope is the number of characters of source one pack may spend.
func bodyEnvelope(b Budget, th Thresholds) int {
	env := int(th.BodyShare * float64(b.MaxTokensEstimate) * bytesPerToken)
	if env < 0 {
		return 0
	}
	return env
}

// markBodyCliff computes each candidate's claim on the response budget and
// withholds source from the ones far below the strongest.
//
// It must be called with the FULL admitted list — every candidate that survived
// the marginal-value cut, before MaxSymbols or MaxFiles have removed anything.
// A cliffed candidate stays in the pack and stays named; only its bytes are
// withheld, and withheld carries the reason so nothing is lost silently.
func markBodyCliff(kept []*candidate, w Weights, th Thresholds) {
	var top float64
	for _, c := range kept {
		c.bodyWeight = c.score * c.bodyWorth(w)
		if c.bodyWeight > top {
			top = c.bodyWeight
		}
	}
	if top <= 0 {
		return
	}
	cliff := th.BodyCliffFraction * top
	for _, c := range kept {
		// >= keeps the strongest item eligible even at a cliff fraction of 1.
		if c.bodyWeight >= cliff || !c.canShowSource() {
			continue
		}
		c.withheld = fmt.Sprintf(
			"source withheld: its relevance to the response budget is %.0f%% of the strongest "+
				"item's, below this pack's %.0f%% cliff, so its bytes would cost more than they teach",
			100*c.bodyWeight/top, 100*th.BodyCliffFraction)
	}
}

// allocateBodies splits the envelope over the items that will actually ship.
//
// cands must be ordered strongest-first and must already have been through
// markBodyCliff. The returned slice is parallel to cands: a zero means no body,
// and the reason is on the candidate's withheld field.
//
// Two things are deliberately absent. There is no redistribution pass for
// allocation an item cannot use (a six-line method granted 900 characters spends
// 240 of them): the unspent envelope simply is not spent, which makes the pack
// cheaper and is the direction we want to err in. And there is no count of items
// allowed to carry source — how many can is a consequence of MinBodyChars and
// the envelope, not a separate number that could disagree with them.
func allocateBodies(cands []*candidate, th Thresholds, envelope int) []int {
	grants := make([]int, len(cands))
	if envelope <= 0 || len(cands) == 0 {
		return grants
	}

	// Eligible, strongest-first. Index into cands is carried so the result stays
	// parallel to the input.
	idx := make([]int, 0, len(cands))
	for i, c := range cands {
		if c.withheld == "" && c.bodyWeight > 0 && c.canShowSource() {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return grants
	}

	// Dust helps nobody: when the floor cannot reach MinBodyChars for everyone,
	// withhold from the weakest rather than give every item three broken lines.
	// One always survives — never cliff every candidate.
	floorPool := int(th.BodyFloorShare * float64(envelope))
	n := len(idx)
	for n > 1 && floorPool/n < th.MinBodyChars {
		n--
	}
	for _, i := range idx[n:] {
		cands[i].withheld = fmt.Sprintf(
			"source withheld: this pack's ~%d-character source budget stretched to the %s "+
				"above it, and splitting it further would leave every item under %d characters, "+
				"which teaches less than the summary line already did",
			envelope, plural(n, "stronger item"), th.MinBodyChars)
	}
	idx = idx[:n]

	floorEach := floorPool / n
	var sum float64
	for _, i := range idx {
		sum += cands[i].bodyWeight
	}
	remainder := envelope - floorEach*n
	if remainder < 0 {
		remainder = 0
	}

	for _, i := range idx {
		g := floorEach
		if sum > 0 {
			g += int(float64(remainder) * cands[i].bodyWeight / sum)
		}
		grants[i] = g
	}
	return grants
}
