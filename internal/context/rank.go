package context

import (
	"sort"
	"strconv"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Weights are the ONLY place a ranking coefficient appears.
//
// Spec §17 warns specifically about scoring logic scattered across files: once
// three packages each add "a small boost", nobody can explain a ranking or
// change it safely. Every number below is applied exactly once, in score, and
// nothing else in this package multiplies a signal by a constant.
//
// The absolute values matter less than the ORDER, which encodes a claim about
// evidence quality:
//
//		SymbolNameMatch > GraphProximity > TestRelation > DataAccess > Lexical
//
//	  - SymbolNameMatch is highest because the task naming a symbol is the only
//	    signal that reflects the author's own intent rather than our inference.
//
//	  - GraphProximity is second and must stay above Lexical. That ordering is
//	    the whole reason the golden test passes: in the payment fixture the decoy
//	    file scores BETTER lexically on "payment" than the real controller does,
//	    and only structure tells them apart.
//
//	  - TestRelation is high because existing coverage is disproportionately
//	    valuable for a change task — it tells the agent what must keep passing —
//	    and because nothing else in the pipeline would surface it.
//
//	  - DataAccess is high for the same reason for persistence: an idempotency
//	    change lives at the write, so the tables on the path are context, not
//	    trivia.
//
//	  - Lexical is deliberately the weakest evidence that can admit anything. A
//	    word in a comment is not a dependency.
//
// ModuleAffinity and Recency are MODIFIERS: they reorder material that is
// already relevant and can never admit anything on their own (see score).
// Recency is weakest of all because file mtime is noisy — `git checkout`
// rewrites it wholesale, which is exactly why the indexer compares content
// hashes instead.
type Weights struct {
	SymbolNameMatch float64
	GraphProximity  float64
	Lexical         float64
	TestRelation    float64
	DataAccess      float64
	ModuleAffinity  float64
	Recency         float64

	// ConfidenceShare is the fraction of the GraphProximity contribution that
	// is scaled by the path's minimum confidence. At 0.6, a path built entirely
	// from low-confidence guesses still counts for 40% + 15% = 55% of a proven
	// one — discounted, never erased, because under-reporting a real dependency
	// is the more expensive error.
	ConfidenceShare float64

	// MemberBonus rewards a unit that contributed several relevant members: a
	// class where three methods sit on the path is more central to the task
	// than one where a single field was touched. Capped, because a large class
	// would otherwise win on size alone.
	MemberBonus    float64
	MaxMemberBonus float64

	// The Worth coefficients below are the ONLY weights not applied in score,
	// and the exception is deliberate rather than a crack in the rule: they
	// decide BYTES, not rank, and each is still applied exactly once, in
	// bodyWorth. Keeping them here keeps the promise that made this struct worth
	// having — one file to read before you can explain any number this package
	// produced.
	//
	// Worth exists because rank and byte-value are not the same question. A
	// candidate can legitimately rank on a name collision while its source
	// teaches nothing, so the allocator re-applies the relevance penalty a
	// second time before handing out the response budget. Every coefficient is
	// a multiplier in (0,1]; they compose, and nothing raises worth above 1.

	// WorthGenerated discounts generated, vendored or minified source (see
	// looksGenerated). A generated PaymentRequestBuilder matches "payment" as
	// well as a hand-written one and is ranked accordingly, but its body is
	// templated: reading it teaches an agent about the generator, not about the
	// change. 0.2 rather than 0 because a schema-generated DTO still shows the
	// field names, which is occasionally the answer.
	WorthGenerated float64

	// WorthTextOnly discounts a unit admitted on text alone. That evidence is a
	// word that may well be in a comment — the decoy case this package exists to
	// get right — so its bytes are the least likely in the pack to repay their
	// cost. Half, not less, because the words could equally be in the code.
	WorthTextOnly float64
}

// Thresholds are the cut-offs that decide what is even considered.
type Thresholds struct {
	// SeedName is the minimum name signal to become a seed. At 0.35 a single
	// identifier-part match on a full-weight noun (0.7/1.5 = 0.47) qualifies
	// and a stem-only match on a demoted verb does not.
	SeedName float64

	// MinLexicalTerms is how many DISTINCT task terms a file must match to earn
	// a place on lexical evidence alone. Two, because one is exactly the decoy
	// case the golden test encodes.
	MinLexicalTerms int

	// MarginalFraction stops the pack when value falls off rather than when the
	// budget fills: an item scoring under this fraction of the best item is
	// noise next to what is already included, and including it costs the agent
	// capacity to think.
	MarginalFraction float64

	// AbsoluteFloor drops items that are weak in absolute terms even when
	// everything else is weaker still, so a task that matched almost nothing
	// returns almost nothing instead of promoting its best rubbish.
	AbsoluteFloor float64

	MaxSeeds int

	// LexicalSeeds is how many seed slots are RESERVED for corroborated text
	// evidence, taken from the weakest name seeds when the set is full.
	//
	// It is not a ranking weight and must not be tuned like one. It exists
	// because the seed budget is otherwise monopolised by marginal name
	// matches: measured on the self bench, all five cases filled every slot
	// with name seeds and discarded 53-98 corroborated lexical files. Set to 0
	// this package behaves exactly as it did before lexical seeding existed.
	//
	// Measured sensitivity on the self bench, so the next person does not have
	// to rediscover it: 3 and 6 both score 2/5 at 50% mean recall; 12 falls
	// back to 1/5 at 30%. There is a plateau at small values and a cliff, and
	// the cliff has an explanation — past a point the quota displaces name
	// seeds that were carrying real evidence. 6 sits inside the plateau rather
	// than on its edge, which is the reason to prefer it over 3. Raising it
	// toward 12 is known to be wrong.
	LexicalSeeds  int
	MaxGraphNodes int
	MaxQueryTerms int
	LexicalLimit  int

	// ExcerptLines caps how much source ANY ONE item may show. Source is the
	// last thing added and the first thing cut: a symbol summary costs a line
	// and tells the agent where to look, while a source range costs twenty and
	// tells it something it can fetch itself.
	//
	// It is a ceiling, not the allocator — a 200-line method is not the fastest
	// way to teach one idea no matter how much of the envelope it won. Which
	// items get source, and how much each gets, is decided by weight in
	// allocateBodies.
	//
	// There is deliberately no MaxExcerpts beside it any more. A count of items
	// allowed to carry source WAS the allocation-by-count defect: it handed the
	// first three items the whole envelope regardless of how much stronger the
	// first was than the third, and an item it skipped freed nothing for the
	// items behind it. How many items can carry source now falls out of the
	// envelope itself — BodyFloorShare and MinBodyChars together admit at most
	// BodyFloorShare*envelope/MinBodyChars of them — so the bound is a
	// consequence of the budget rather than a second, unrelated number.
	ExcerptLines int

	// BodyShare is the fraction of the content token budget that source bodies
	// may spend, converted to characters at bytesPerToken.
	//
	// It is a minority share on purpose. A summary line is the part of a pack an
	// agent CANNOT reconstruct — it is our reading of the repository — while a
	// source range is something it can fetch itself in one call with the path
	// and line we already gave it.
	//
	// 0.06 was chosen by measurement, and against the first instinct. The
	// allocator was first shipped at 0.10, which took `repocortex bench
	// fixtures/bench/payment.yml` from 1,640 / 1,246 / 988 tokens to
	// 2,200 / 1,607 / 1,427 — about 34% more — while recall stayed at 100% on
	// all three cases, exactly as it had been before. The extra characters
	// bought nothing the harness could see.
	//
	// The harness is saturated, so this is NOT evidence that wider source
	// coverage is worthless; it is the absence of evidence that it is worth
	// paying for. With the objective being useful understanding per token, the
	// burden of proof sits on the more expensive default, and it is unmet. Raise
	// it when a benchmark exists that can tell the two apart — one that scores
	// whether the agent's ANSWER improved, not whether the right files were
	// named.
	BodyShare float64

	// BodyFloorShare is the fraction of the body envelope divided EQUALLY among
	// eligible items before the remainder is split by weight.
	//
	// This is the knob that decides whether a diffuse question gets a spread or
	// a spotlight. At 0 the leader takes nearly everything, which is right for
	// "fix handleCallback" and wrong for "how does payment settlement work". At
	// 1 every item gets the same share regardless of relevance, which is exactly
	// the allocation-by-count defect this replaces. 0.35 was chosen against the
	// golden task: it leaves every one of nine items ~124 characters — two or
	// three lines, enough to see a signature — while the top item still wins
	// about six times the weakest item's allocation.
	BodyFloorShare float64

	// BodyCliffFraction is the relative cliff: an item whose body weight is
	// below this fraction of the STRONGEST item's weight is named in the pack
	// but carries no source.
	//
	// It is the same 0.15 as MarginalFraction, and sharing the number is the
	// point: an item that far below the leader is already at the edge of being
	// worth listing, so its bytes are past that edge. Two numbers for one
	// judgement would drift apart and neither would be explainable.
	//
	// The cliff is applied over the WHOLE candidate list, before the item cap.
	// Applied after, the cap fills with cliff-bound items and the envelope they
	// free is never handed to the items behind them.
	BodyCliffFraction float64

	// MinBodyChars is the smallest excerpt worth printing: about three lines at
	// the fixture's measured ~40 characters per line of Java. Below that an
	// excerpt shows a signature and half a statement, which tells an agent less
	// than the item's own summary already did while still costing tokens. When
	// the envelope cannot give every eligible item this much, the allocator
	// withholds source from the weakest rather than dusting everyone with
	// fragments.
	MinBodyChars int

	// MaxExclusions bounds how many rejections are reported. The list is a
	// trust feature, not an audit log; naming the near-misses is useful,
	// enumerating every file in the repository is not.
	MaxExclusions int
}

// Tuning is the complete ranking policy.
type Tuning struct {
	Weights
	Thresholds
}

// DefaultTuning returns the policy the golden test is calibrated against.
func DefaultTuning() Tuning {
	return Tuning{
		Weights: Weights{
			SymbolNameMatch: 3.0,
			GraphProximity:  2.2,
			Lexical:         0.9,
			TestRelation:    1.2,
			DataAccess:      1.0,
			ModuleAffinity:  0.5,
			Recency:         0.3,
			ConfidenceShare: 0.6,
			MemberBonus:     0.15,
			MaxMemberBonus:  0.45,
			WorthGenerated:  0.2,
			WorthTextOnly:   0.5,
		},
		Thresholds: Thresholds{
			SeedName:          0.35,
			MinLexicalTerms:   2,
			MarginalFraction:  0.15,
			AbsoluteFloor:     0.30,
			MaxSeeds:          24,
			LexicalSeeds:      6,
			MaxGraphNodes:     2000,
			MaxQueryTerms:     8,
			LexicalLimit:      50,
			ExcerptLines:      14,
			MaxExclusions:     12,
			BodyShare:         0.06,
			BodyFloorShare:    0.35,
			BodyCliffFraction: 0.15,
			MinBodyChars:      120,
		},
	}
}

// admission records why a candidate is in the running, in descending order of
// evidence quality. The zero value means "not admitted".
type admission int

const (
	admitNone admission = iota
	admitLexical
	admitData
	admitTest
	admitGraph
	admitNamed
)

func (a admission) String() string {
	switch a {
	case admitNamed:
		return "named by the task"
	case admitGraph:
		return "on a dependency path from what the task named"
	case admitTest:
		return "covers included code"
	case admitData:
		return "persistence touched by included code"
	case admitLexical:
		return "text match"
	default:
		return "not admitted"
	}
}

// candidate is one unit of context under consideration.
//
// The unit is a top-level declaration — a class, an interface, a free function,
// a table — not an individual method. That choice is the pack's main
// compression: an agent that is told "TransactionService, because
// updateStatus() is one hop from handleCallback" can open the file, whereas a
// pack listing updateStatus, repository, notificationService and
// findTransaction as four separate items has spent four times the tokens to say
// the same thing.
type candidate struct {
	unitID  int64
	kind    string
	name    string
	fqn     string
	path    string
	line    int
	via     admission
	relates model.Confidence

	// Signals, each normalised to [0,1] before weighting.
	nameScore  float64
	nameTerms  []string
	proximity  float64
	pathConf   model.Confidence
	depth      int
	lexical    float64
	lexTerms   []string
	testFor    string
	tables     []tableRef
	tableOwner string
	affinity   float64
	recency    float64
	members    int

	score float64

	// Body allocation, filled in after admission by markBodyCliff and
	// allocateBodies. bodyWeight is score*worth — the claim this item has on the
	// response budget, which is not the same as its claim on a place in the pack.
	// bodyChars is what it was granted; withheld is non-empty when policy, not
	// the absence of source, is why it has none.
	bodyWeight float64
	bodyChars  int
	withheld   string
}

// canShowSource reports whether this candidate has a source range to show at
// all. A synthetic file- or table-level candidate has a path but no symbol
// range, so it is not a claimant on the response budget and must never be
// reported as having had source withheld — there was none to withhold.
func (c *candidate) canShowSource() bool { return c.unitID > 0 }

// bodyWorth is how much this candidate's BYTES are expected to teach, in (0,1].
//
// It is the second application of the relevance penalty, and it exists because
// "belongs in the answer" and "its source belongs in the answer" are different
// questions. Generated code ranks honestly on a name collision and still teaches
// nothing when you read it; a unit admitted because a task word appears
// somewhere in the file may have matched a comment. Both should be NAMED — an
// agent can decide for itself whether to open them — and neither should be
// handed a share of the response budget equal to the file the task is about.
func (c *candidate) bodyWorth(w Weights) float64 {
	worth := 1.0
	if looksGenerated(c.path) {
		worth *= w.WorthGenerated
	}
	if c.via == admitLexical {
		worth *= w.WorthTextOnly
	}
	return worth
}

// generatedMarkers are path fragments that mean "a tool wrote this".
//
// Path-only, lowercased, and deliberately conservative: a false positive costs
// an agent the source of a file it might have wanted, so the list holds only
// conventions that are near-universal in the ecosystems they come from. It is a
// classification, not a coefficient — the number it selects lives in Weights.
//
// Discovery-time excludes (config.Defaults) already keep most of vendor/ and
// node_modules/ out of the index entirely; the entries here are the ones that
// survive indexing because they sit inside the source tree.
var generatedMarkers = []string{
	"/generated/", "/gen/", "/.gen/", "/vendor/", "/node_modules/",
	".pb.go", "_pb2.py", "_pb.go", ".pb.cc", ".pb.h",
	"_generated.", ".generated.", ".g.dart", ".g.cs", ".designer.cs",
	".min.js", ".min.css",
}

// looksGenerated reports whether a path names machine-written source. The path
// is matched with a leading slash so a directory marker matches at the root as
// well as in the middle, and so "regenerated/" is not mistaken for "generated/".
func looksGenerated(p string) bool {
	lower := "/" + strings.ToLower(p)
	for _, m := range generatedMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// score folds every signal into one number, and is the only multiplication of a
// signal by a weight in this package.
//
// The split between base and modifiers is what makes "cannot admit on its own"
// enforceable rather than a comment: a candidate whose base is zero scores zero
// no matter how recently its file was touched or how close it sits to a seed's
// directory.
func (c *candidate) computeScore(w Weights) {
	// A depth-0 unit was reached by no edge at all — the task named it — so
	// there is no path confidence to discount and applying one would penalise
	// the strongest evidence in the pipeline.
	confidenceMix := 1.0
	if c.depth > 0 {
		confidenceMix = (1 - w.ConfidenceShare) + w.ConfidenceShare*confidenceFactor(c.pathConf)
	}

	memberBonus := w.MemberBonus * float64(max(0, c.members-1))
	if memberBonus > w.MaxMemberBonus {
		memberBonus = w.MaxMemberBonus
	}

	base := w.SymbolNameMatch*c.nameScore +
		w.GraphProximity*c.proximity*confidenceMix +
		w.Lexical*c.lexical
	if c.testFor != "" {
		base += w.TestRelation
	}
	if c.tableOwner != "" {
		base += w.DataAccess
	}
	if base > 0 {
		base += memberBonus
	}

	if base <= 0 {
		c.score = 0
		return
	}
	c.score = base + w.ModuleAffinity*c.affinity + w.Recency*c.recency
}

// admit decides whether a candidate belongs in the pack at all.
//
// This is a GATE, not a weight, and it is the single most important decision in
// the package. Ranking alone cannot solve the decoy problem: a file that
// matches a task word really is more relevant than a file that matches nothing,
// so under a pure ranking it is always somewhere in the ordering and a generous
// budget will eventually include it. Requiring corroboration — a dependency
// path, a test relationship, a data dependency, or several distinct task terms
// rather than one — is what turns "ranked lower" into "left out, and here is
// why".
func admit(c *candidate, th Thresholds) admission {
	switch {
	case c.nameScore > 0:
		return admitNamed
	case c.proximity > 0:
		return admitGraph
	case c.testFor != "":
		return admitTest
	case c.tableOwner != "":
		return admitData
	case len(c.lexTerms) >= th.MinLexicalTerms:
		return admitLexical
	}
	return admitNone
}

// rejectionReason explains a near-miss in terms an engineer can check.
func rejectionReason(c *candidate, th Thresholds) string {
	var b strings.Builder
	if len(c.lexTerms) > 0 {
		b.WriteString("matches " + quoteList(c.lexTerms) + " textually")
		if len(c.lexTerms) < th.MinLexicalTerms {
			b.WriteString(" (only " + plural(len(c.lexTerms), "task term") + "; ")
			b.WriteString(plural(th.MinLexicalTerms, "term"))
			b.WriteString(" required without a structural connection)")
		}
		b.WriteString(", but ")
	}
	b.WriteString("no dependency path connects it to anything the task names, ")
	b.WriteString("it covers no included code, and it shares no table with the flow")
	return b.String()
}

func quoteList(xs []string) string {
	if len(xs) == 0 {
		return "no term"
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = `"` + x + `"`
	}
	sort.Strings(out)
	switch len(out) {
	case 1:
		return out[0]
	case 2:
		return out[0] + " and " + out[1]
	default:
		return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
	}
}

func plural(n int, noun string) string {
	s := noun
	if n != 1 {
		s += "s"
	}
	return strconv.Itoa(n) + " " + s
}
