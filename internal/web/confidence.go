package web

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// The confidence legend, served as data.
//
// # Why the client does not own this
//
// The portal's whole differentiation is that an edge's rendering IS its
// evidential status: a dashed line means "measured at 5.7–37.5% precision", not
// "looks tentative". If the mapping from tier to stroke lived only in the
// stylesheet, a later restyle could quietly make `medium` look like `high`, and
// nothing would fail. Serving it — including the measured precision ranges from
// docs/accuracy.md — means the legend the reader sees and the strokes the canvas
// draws come from one source, and a test can assert the numbers are still the
// measured ones.
//
// The precision figures are not estimates. They are the adjudication of 30,975
// heuristic CALLS edges against compiler-backed indexes (scip-go 0.2.7,
// scip-typescript 0.3.14, scip-python 0.6.6) over five repositories in three
// languages. See docs/accuracy.md, which also documents how to reproduce them.

// tierWords is the plain word each tier id reads as on screen. The ids stay in
// every field, attribute and parameter; prose the server writes uses the word,
// the same one assets/js/tiers.mjs gives the client, and a test holds the two
// copies together.
var tierWords = map[model.Confidence]string{
	model.ConfidenceExact:  "proven",
	model.ConfidenceHigh:   "very likely",
	model.ConfidenceMedium: "uncertain",
	model.ConfidenceLow:    "a guess",
}

// tierWord is the word for a tier id in a sentence a reader sees. An id the
// legend does not know reads as unmeasured, which is what it is.
func tierWord(c model.Confidence) string {
	if w, ok := tierWords[c]; ok {
		return w
	}
	return "unmeasured"
}

// Tier is one confidence level with everything needed to render and interpret
// it.
type Tier struct {
	Confidence string `json:"confidence"`
	Rank       int    `json:"rank"`

	// Stroke is the canvas rendering: solid | solid-light | dashed | ghosted.
	Stroke string `json:"stroke"`

	// Glyph marks the tier in text contexts. Only `exact` earns one.
	Glyph string `json:"glyph,omitempty"`

	// DefaultVisible is false for `low`, which is off until asked for. The
	// count of what that hides is reported in every response's omission block,
	// so "off by default" never becomes "absent without saying so".
	DefaultVisible bool `json:"default_visible"`

	// Precision is the measured range, as prose. Empty for `exact`, which is
	// not measured against a compiler because it came from one.
	Precision string `json:"precision,omitempty"`

	// Meaning is the one sentence a reader needs to know what acting on this
	// tier costs them.
	Meaning string `json:"meaning"`

	// Advice is what to DO with an edge at this tier.
	Advice string `json:"advice"`
}

// Legend is the whole tier vocabulary, strongest first.
func Legend() []Tier {
	return []Tier{
		{
			Confidence:     string(model.ConfidenceExact),
			Rank:           model.ConfidenceExact.Rank(),
			Stroke:         "solid",
			Glyph:          "✓",
			DefaultVisible: true,
			Meaning: "A compiler proved it. Sourced from a SCIP index produced by a real " +
				"typechecker, and the store refuses to record this tier from any other source.",
			Advice: "Safe to act on without verification.",
		},
		{
			Confidence:     string(model.ConfidenceHigh),
			Rank:           model.ConfidenceHigh.Rank(),
			Stroke:         "solid-light",
			DefaultVisible: true,
			Precision:      "86.8–97.8% measured",
			Meaning: "Resolved through an explicit import plus an unambiguous receiver type. " +
				"Measured between 86.8% and 96.1% across Go, TypeScript and Python, and 97.8% " +
				"on the compiler-backed comparison in docs/accuracy.md.",
			Advice: "Worth acting on. Roughly one edge in eight is still wrong, so verify before " +
				"anything irreversible.",
		},
		{
			Confidence:     string(model.ConfidenceMedium),
			Rank:           model.ConfidenceMedium.Rank(),
			Stroke:         "dashed",
			DefaultVisible: true,
			Precision:      "5.7–37.5% measured",
			Meaning: "Type inference picked a preferred candidate where more than one existed. " +
				"This is the tier to distrust: it is not rare (6,473 of the edges checked against a compiler) and " +
				"it is mostly wrong.",
			Advice: "Shown, never acted on. Treat a medium edge as a question, not a finding.",
		},
		{
			Confidence:     string(model.ConfidenceLow),
			Rank:           model.ConfidenceLow.Rank(),
			Stroke:         "ghosted",
			DefaultVisible: false,
			Precision:      "0.0–2.8% measured",
			Meaning: "A matching name in an imported package, with no type confirmation. " +
				"About 97 in every 100 name a relationship that does not exist.",
			Advice: "A hint that a name exists, nothing more. Off by default; the count of what " +
				"that hides is stated in every view.",
		},
	}
}

// DefaultFloor is the confidence floor the portal applies when the reader has
// not chosen one.
//
// medium, matching impact.DefaultMinConfidence — the portal must not present a
// wider blast radius than `cortex` itself would, or the two disagree about
// the same repository. `low` is one control away and its exclusion is always
// counted.
const DefaultFloor = model.ConfidenceMedium

// tierOf maps a confidence to its legend entry, tolerating the empty/unknown
// value a pre-validation row could carry.
func tierOf(c model.Confidence) string {
	if !c.Valid() {
		return "unknown"
	}
	return string(c)
}

// confidenceOrder is the fixed reporting order for every histogram in this
// package, so a stacked bar's segments never reorder between requests.
var confidenceOrder = []model.Confidence{
	model.ConfidenceExact, model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow,
}

// Histogram counts items per confidence tier.
//
// A map would serialise in a nondeterministic order and force the client to
// know the tier list; a slice of counted tiers keeps the stacked bar stable and
// self-describing. `unknown` is carried rather than folded into `low`: a row
// with no valid confidence is a defect in the index, not a weak fact, and
// hiding it inside a legitimate tier would make it unfindable.
type Histogram struct {
	Exact   int `json:"exact"`
	High    int `json:"high"`
	Medium  int `json:"medium"`
	Low     int `json:"low"`
	Unknown int `json:"unknown"`
	Total   int `json:"total"`
}

func (h *Histogram) add(c model.Confidence) {
	h.Total++
	switch c {
	case model.ConfidenceExact:
		h.Exact++
	case model.ConfidenceHigh:
		h.High++
	case model.ConfidenceMedium:
		h.Medium++
	case model.ConfidenceLow:
		h.Low++
	default:
		h.Unknown++
	}
}

// AtOrAbove counts the entries at least as strong as floor. Used to state what
// a floor keeps and what it hides in the same breath.
func (h Histogram) AtOrAbove(floor model.Confidence) int {
	n := 0
	for _, c := range confidenceOrder {
		if !c.AtLeast(floor) {
			continue
		}
		switch c {
		case model.ConfidenceExact:
			n += h.Exact
		case model.ConfidenceHigh:
			n += h.High
		case model.ConfidenceMedium:
			n += h.Medium
		case model.ConfidenceLow:
			n += h.Low
		}
	}
	return n
}

// Below counts the entries weaker than floor, `unknown` included: a row whose
// confidence the index never set cannot be shown to clear a floor, so it is
// hidden by one and must be counted as hidden.
func (h Histogram) Below(floor model.Confidence) int {
	return h.Total - h.AtOrAbove(floor)
}
