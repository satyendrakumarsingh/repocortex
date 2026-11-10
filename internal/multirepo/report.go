package multirepo

import "github.com/satyendrakumarsingh/repocortex/pkg/model"

// MatchKind names HOW a call was matched to a route. It is reported next to
// every cross-repo edge because the three are not equally strong evidence and
// a reader deciding whether to act on an edge needs to know which one they
// have.
//
// None of them is ever "exact". See capHeuristic.
type MatchKind string

const (
	// MatchLiteralPath is a fully literal call path against a route with no
	// parameters. The strongest claim available from source text, and still a
	// guess: nothing proved that the two strings denote one route.
	MatchLiteralPath MatchKind = "literal_path"

	// MatchPathParameter needed a wildcard to absorb a segment. The route is
	// right; which resource the call names is not proven, and a parameterised
	// route is by construction reachable from many callers.
	MatchPathParameter MatchKind = "path_parameter"

	// MatchLiteralPrefix is a call whose target was assembled at run time,
	// matched only by the whole literal segments it revealed. All that is known
	// is that the target starts where the route starts.
	MatchLiteralPrefix MatchKind = "literal_prefix"
)

// Leg is one hop of a cross-repository path, with the confidence it
// contributes.
//
// Legs exist so the minimum can be AUDITED rather than believed. A single
// number on an edge invites the reader to trust it; the same number beside the
// three legs that produced it invites them to check which hop was weakest, and
// that is usually the one they need to go and look at.
type Leg struct {
	Name       string           `json:"name"`
	Confidence model.Confidence `json:"confidence"`
	Detail     string           `json:"detail"`
	// Heuristic marks a leg no compiler proved. The route match is always one.
	Heuristic bool `json:"heuristic"`
}

// CrossRepoEdge is one outbound call in one member joined to one endpoint in
// another.
type CrossRepoEdge struct {
	FromMember string `json:"from_member"`
	// FromSymbol is the declaration containing the call. Empty when the call
	// sits outside every declaration — a module-level prefetch — which is real
	// and common rather than a failure, and is reported as such rather than
	// being attributed to the enclosing file.
	FromSymbol string `json:"from_symbol,omitempty"`
	FromFile   string `json:"from_file"`
	FromLine   int    `json:"from_line"`
	// Client names the calling convention that found the call (fetch, axios,
	// go-net-http), so a reader can say how it was found rather than treating
	// every call as equally certain.
	Client  string `json:"client,omitempty"`
	Method  string `json:"method,omitempty"`
	Path    string `json:"path"`
	Dynamic bool   `json:"dynamic"`

	ToMember       string `json:"to_member"`
	ToEndpoint     string `json:"to_endpoint"`
	ToEndpointFile string `json:"to_endpoint_file"`
	ToEndpointLine int    `json:"to_endpoint_line"`
	// ToHandler is empty when the framework's syntax did not reveal one. The
	// endpoint is still real and the edge is still an edge; the path simply
	// stops at the route instead of reaching a function, and Note says so.
	ToHandler     string `json:"to_handler,omitempty"`
	ToHandlerFile string `json:"to_handler_file,omitempty"`

	Match MatchKind `json:"match"`
	// Confidence is the MINIMUM over Legs. Never "exact".
	Confidence model.Confidence `json:"confidence"`
	Legs       []Leg            `json:"legs"`
	// Note carries a limitation specific to this edge, empty when there is
	// none.
	Note string `json:"note,omitempty"`
}

// UnjoinedCall is an outbound call that matched no endpoint in any member.
//
// Usually a call into a third party, or into a service nobody added to the
// estate file. Both are worth saying; neither is a row worth dropping, and
// dropping them would also destroy the denominator of "how much of this
// system's traffic did we account for".
type UnjoinedCall struct {
	Member   string `json:"member"`
	Caller   string `json:"caller,omitempty"`
	FilePath string `json:"file"`
	Line     int    `json:"line"`
	Method   string `json:"method,omitempty"`
	Path     string `json:"path"`
	Dynamic  bool   `json:"dynamic"`
	Client   string `json:"client,omitempty"`
	Reason   string `json:"reason"`
}

// AmbiguousCall matched more than one endpoint and was therefore refused.
//
// Refused rather than resolved: two members that both plausibly answer a
// request are not two candidates a router would choose between, because there
// is no router spanning them. Picking one would be the invented dependency this
// package exists to avoid.
type AmbiguousCall struct {
	Member     string   `json:"member"`
	Caller     string   `json:"caller,omitempty"`
	FilePath   string   `json:"file"`
	Line       int      `json:"line"`
	Method     string   `json:"method,omitempty"`
	Path       string   `json:"path"`
	Candidates []string `json:"candidates"` // "member: SIGNATURE", sorted
}

// UnconsumedEndpoint is a route no member of this system calls.
//
// "No member of THIS system" is the whole caveat and the field carries it: an
// endpoint may be called by a mobile client, a cron job, a partner, or a
// repository nobody put in the estate file. It is a lead to check, never a
// verdict that the route is dead.
type UnconsumedEndpoint struct {
	Member   string `json:"member"`
	Endpoint string `json:"endpoint"`
	FilePath string `json:"file"`
	Line     int    `json:"line"`
	Handler  string `json:"handler,omitempty"`
}

// TableParticipant is one member's relationship with a shared table.
type TableParticipant struct {
	Member   string `json:"member"`
	Accesses int    `json:"accesses"`
	Reads    int    `json:"reads"`
	Writes   int    `json:"writes"`
	// Uncertain counts accesses whose statement looked like a run-time
	// fragment. Any of them weakens the coupling claim.
	Uncertain int `json:"uncertain"`
	// Declared is true when this member also DECLARES the table — it owns the
	// schema, rather than merely naming it in a query.
	Declared bool `json:"declared"`
}

// SharedTable is one table named by two or more members.
type SharedTable struct {
	Table string `json:"table"`
	// Participants is sorted by member name.
	Participants []TableParticipant `json:"participants"`
	// Writers names the members that write it, sorted. Two or more is the
	// finding: two services writing one table share an invariant that neither
	// repository states.
	Writers []string `json:"writers"`
	Readers []string `json:"readers"`
	// Owners names the members that declare the table. Zero owners means every
	// member merely queries it and nobody in this system owns the schema.
	Owners     []string         `json:"owners"`
	WriteWrite bool             `json:"write_write"`
	Confidence model.Confidence `json:"confidence"`
	Note       string           `json:"note,omitempty"`
}

// MemberStatus is what the report can say about one member, whether or not it
// could be read.
//
// This section is printed FIRST and never omitted. A federated view that
// silently drops a member reports a smaller system than the one that exists,
// and the reader has no symptom to notice.
type MemberStatus struct {
	Name      string `json:"name"`
	IndexPath string `json:"index_path"`
	Available bool   `json:"available"`
	// Unavailable is written for the person who has to fix it, and names the
	// command that would. Empty when Available.
	Unavailable string `json:"unavailable,omitempty"`

	RunID           int64   `json:"run_id,omitempty"`
	GitRevision     string  `json:"git_revision,omitempty"`
	ProducerVersion string  `json:"producer_version,omitempty"`
	Status          string  `json:"status,omitempty"`
	IndexState      string  `json:"index_state,omitempty"`
	AccuracyMode    string  `json:"accuracy_mode,omitempty"`
	IndexedUnix     int64   `json:"indexed_unix,omitempty"`
	AgeHours        float64 `json:"age_hours,omitempty"`

	Endpoints     int `json:"endpoints"`
	OutboundCalls int `json:"outbound_calls"`
	// LocallyAnswered is outbound calls this member answers itself. Excluded
	// from cross-repo matching — a self-call is not a cross-repo dependency —
	// and counted so the arithmetic of the call totals closes.
	LocallyAnswered int `json:"locally_answered"`
	Tables          int `json:"tables"`

	// Warnings are the conditions under which this member's contribution should
	// not be read as current: a degraded run, a partial index, an unstamped or
	// mismatched producer build.
	Warnings []string `json:"warnings"`
}

// Provenance is what the report knows about WHEN and BY WHAT the members were
// indexed.
//
// A system graph assembled from a six-week-old index of one service is a
// picture of a system that no longer exists, and only the tool can know that.
type Provenance struct {
	// RunningVersion is the build producing this report, for comparison with
	// the builds that produced the indexes.
	RunningVersion string `json:"running_version,omitempty"`
	// ProducerVersions is the distinct set stamped across available members,
	// sorted. An empty string in it means at least one run predates the stamp.
	ProducerVersions []string `json:"producer_versions"`
	// MixedProducers is true when the members were not all indexed by one
	// build. Resolution rules change between builds, so a mixed estate holds
	// edges that would not all be produced by any single binary.
	MixedProducers bool `json:"mixed_producers"`

	OldestIndexedUnix int64 `json:"oldest_indexed_unix,omitempty"`
	NewestIndexedUnix int64 `json:"newest_indexed_unix,omitempty"`
	// SkewHours is the spread between the oldest and newest member index. The
	// system graph describes no single moment; this is how wide that moment is.
	SkewHours float64 `json:"skew_hours"`

	// Notes are the sentences a reader must see before acting on anything
	// above.
	Notes []string `json:"notes"`
}

// Unjoined is everything the federation could not join, which is the half of
// the report that keeps the other half honest.
type Unjoined struct {
	Calls          []UnjoinedCall       `json:"calls"`
	AmbiguousCalls []AmbiguousCall      `json:"ambiguous_calls"`
	Endpoints      []UnconsumedEndpoint `json:"endpoints"`
	// UnavailableMembers names members whose index could not be read at all.
	// Every count in the report is missing whatever they would have contributed.
	UnavailableMembers []string `json:"unavailable_members"`

	// Truncated records what the bounds cut, per section, so a capped list is
	// never mistaken for a complete one.
	Truncated map[string]int `json:"truncated,omitempty"`
}

// Report is the system graph: a view across several indexes at one moment.
type Report struct {
	Members      []MemberStatus  `json:"members"`
	Edges        []CrossRepoEdge `json:"cross_repo_edges"`
	SharedTables []SharedTable   `json:"shared_tables"`
	Unjoined     Unjoined        `json:"unjoined"`
	Provenance   Provenance      `json:"provenance"`
	// Caveats are the standing limitations of the method itself, as opposed to
	// the per-run conditions in Provenance.Notes. They do not vary with the
	// data and they are printed every time, because the reading rule they carry
	// is what the numbers mean.
	Caveats []string `json:"caveats"`
}

// Complete reports whether every member contributed.
//
// The single most important boolean in the document: false means every count
// below it is a floor rather than a total.
func (r *Report) Complete() bool { return len(r.Unjoined.UnavailableMembers) == 0 }

// capHeuristic is the ceiling on everything this package produces.
//
// `exact` means a compiler proved it. Every path here traverses a route match
// that was decided by comparing two strings written by two teams in two
// syntaxes, so no chain through it can be compiler-proven however strong its
// other legs are. This is deliberately a function rather than a consequence of
// the arithmetic: the ceiling has to hold when someone later adds a leg that
// can carry `exact`, and a ceiling that only holds by accident is not one.
func capHeuristic(c model.Confidence) model.Confidence {
	if !c.Valid() || c == model.ConfidenceExact {
		return model.ConfidenceHigh
	}
	return c
}

// minConfidence returns the weakest of the given confidences, which is what a
// chain is worth.
//
// A chain through one heuristic hop is a lead, not a fact, and reporting the
// strongest leg would let a compiler-proven EXPOSES edge launder a guessed
// route match into something a reader would act on.
func minConfidence(cs ...model.Confidence) model.Confidence {
	out := model.ConfidenceExact
	for _, c := range cs {
		if !c.Valid() {
			continue
		}
		if c.Rank() < out.Rank() {
			out = c
		}
	}
	return capHeuristic(out)
}

// demote weakens a confidence by exactly one tier, floored at low.
//
// One tier, not a jump to low: weaker evidence is not the same as no evidence,
// and collapsing the two would make the ranking that thresholds exist to read
// (model.Confidence.AtLeast) meaningless. Mirrors registry.demote.
func demote(c model.Confidence) model.Confidence {
	switch c {
	case model.ConfidenceExact, model.ConfidenceHigh:
		return model.ConfidenceMedium
	default:
		return model.ConfidenceLow
	}
}
