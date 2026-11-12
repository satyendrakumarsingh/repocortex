package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The task contract: the brief an agent should have been handed.
//
// # Why this is a screen and not a command's output
//
// `repocortex compile` already prints a contract. What a page adds is the thing
// prose cannot do: it puts the UNKNOWNS at the top, in the reader's way, at the
// same weight as the scope. That ordering is the whole argument. Most bad agent
// output comes from an under-specified task, and the part of a brief that is
// under-specified is exactly the part a Markdown document buries at the bottom
// under six sections the reader has already agreed with. A contract whose gaps
// are collapsed behind a disclosure triangle is a contract that claims to be
// complete.
//
// # Nothing here decides anything
//
// Scope selection, constraint derivation, risk severity, acceptance and the
// unknowns themselves are internal/compile's, unchanged, with their Evidence
// strings carried verbatim. This handler assembles the pack, calls
// CompileWith, and states what it left out.
//
// # What it leaves out, and says so
//
// Three things, every time: the pack's own exclusions (candidates considered
// and rejected, with the reason), the scope items dropped by the scope budget,
// and the impact floor's hidden count. All three land in the omission block
// with a remedy, because a brief that quietly narrowed itself is worse than a
// wide one.

// ContractResponse is the compiled-brief screen.
type ContractResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Intent is the request as asked, echoed so a reader comparing it against
	// Contract.Interpretation can see a misread before acting on the brief.
	Intent string `json:"intent"`

	// Status is ok | no_intent | unavailable.
	Status string `json:"status"`

	Help *Guidance `json:"guidance,omitempty"`

	Filters ContractFilters `json:"filters"`

	// Contract is the whole compiled brief, with internal/compile's own types
	// intact. Remapping it here would be the obvious thing to do and would
	// quietly drop the Evidence strings that make every line checkable.
	Contract *compile.Contract `json:"contract,omitempty"`

	// Unknowns is Contract.Unknowns HOISTED to the top level.
	//
	// Duplicated on purpose. It is the most important section of a contract and
	// a client that renders the struct field-by-field would put it after Risks,
	// which is where a reader stops looking. Hoisting it makes the prominent
	// rendering the DEFAULT rather than something a UI has to remember.
	Unknowns []string `json:"unknowns"`

	// Interpretation is hoisted for the same reason, one line above the scope:
	// almost every bad pack comes from a misread task rather than bad ranking.
	Interpretation string `json:"interpretation,omitempty"`

	Counts ContractCounts `json:"counts"`

	// PackNotes are internal/context's own coverage caveats, verbatim.
	PackNotes []string `json:"pack_notes,omitempty"`

	// Excluded is what the context engine considered and left out, with the
	// reason it gave. Bounded, and the remainder is counted.
	Excluded []devctx.Exclusion `json:"pack_excluded,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// ContractFilters echoes the bounds actually applied.
type ContractFilters struct {
	MaxScope int `json:"max_scope"`
}

// ContractCounts is the arithmetic a reader checks the brief against.
type ContractCounts struct {
	Scope       int `json:"scope"`
	OutOfScope  int `json:"out_of_scope"`
	Constraints int `json:"constraints"`
	Acceptance  int `json:"acceptance"`
	Risks       int `json:"risks"`
	Unknowns    int `json:"unknowns"`
	BlastRadius int `json:"blast_radius"`
	Tests       int `json:"tests"`

	// PackItems is how many units the context engine admitted before the scope
	// budget cut them. Stated beside Scope so the cut is visible as a number.
	PackItems     int `json:"pack_items"`
	TokenEstimate int `json:"token_estimate"`

	// BlastOutsideScope counts reached symbols that live in a file the scope
	// does not name. It is the single number this screen exists to surface:
	// scope bounds the edit, blast radius bounds the consequence, and the gap
	// between them is what an agent is about to walk into.
	BlastOutsideScope int `json:"blast_radius_outside_scope"`

	// UnresolvedTargets counts scope symbols the impact analysis could not find
	// in the index. Their blast radius is UNKNOWN, not empty.
	UnresolvedTargets int `json:"impact_targets_unresolved"`
}

// contractStatus values.
const (
	contractOK          = "ok"
	contractNoIntent    = "no_intent"
	contractUnavailable = "unavailable"
)

// maxScopeCeiling bounds ?max_scope=. Well above compile's default of 12: the
// point of the control is to widen a brief a little, never to turn it back into
// a search result.
const maxScopeCeiling = 60

func (s *Server) handleContract(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	intent, hasIntent := intentParam(r)
	maxScope := intParam(r, "max_scope", 0, 0, maxScopeCeiling)

	resp := ContractResponse{
		Meta: meta, Legend: Legend(), Intent: intent,
		Filters:  ContractFilters{MaxScope: maxScope},
		Unknowns: []string{},
	}
	if !ok {
		resp.Status = contractNoIntent
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if !hasIntent {
		resp.Status = contractNoIntent
		resp.Help = &Guidance{
			Title: "Describe the task in one line",
			Detail: "A contract is compiled FROM a request. It states what is in scope and why, what is " +
				"deliberately not, which rules the change must respect, what touching the scope reaches, " +
				"and — the part that matters most — what it could not work out from the index.",
			Steps: []string{
				`Try: "Add idempotency to payment callback processing"`,
				"Everything is derived from this index. A constraint with no evidence is an opinion, and this refuses to emit one.",
				"repocortex compile \"<your task>\" --json  — the same contract on the command line",
			},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	contract, pack, err := s.buildContract(ctx, run.ID, intent, maxScope)
	switch {
	case errors.Is(err, errNoPackReader):
		resp.Status = contractUnavailable
		resp.Help = &Guidance{
			Title: "This build cannot compile a contract",
			Detail: "The portal was constructed without the context-pack port. The graph screens read the " +
				"index directly; a contract additionally needs full-text file search and file excerpting, " +
				"and this server was handed a narrower reader than that.",
			Steps: []string{"repocortex compile \"<your task>\" — the command has the same sources"},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}

	var om Omissions
	resp.Status = contractOK
	resp.Contract = &contract
	resp.Interpretation = contract.Interpretation
	resp.Unknowns = contract.Unknowns
	if resp.Unknowns == nil {
		// Never null. An absent unknowns list and an empty one must not
		// serialise the same, and of the two the empty array is the one a
		// client can render as "the compiler named no gaps" rather than
		// skipping the section entirely.
		resp.Unknowns = []string{}
	}
	resp.PackNotes = pack.Notes

	scopePaths := make(map[string]struct{}, len(contract.Scope))
	for _, sc := range contract.Scope {
		scopePaths[sc.Path] = struct{}{}
	}
	outside, unresolved := 0, 0
	for _, b := range contract.BlastRadius {
		if _, in := scopePaths[b.Path]; !in {
			outside++
		}
	}
	for _, sc := range contract.Scope {
		if sc.Symbol == "" {
			unresolved++
		}
	}

	resp.Counts = ContractCounts{
		Scope: len(contract.Scope), OutOfScope: len(contract.OutOfScope),
		Constraints: len(contract.Constraints), Acceptance: len(contract.Acceptance),
		Risks: len(contract.Risks), Unknowns: len(contract.Unknowns),
		BlastRadius: len(contract.BlastRadius), Tests: len(contract.Tests),
		PackItems: len(pack.Items), TokenEstimate: contract.TokenEstimate,
		BlastOutsideScope: outside, UnresolvedTargets: unresolved,
	}

	resp.Excluded = capped(pack.Excluded, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Context candidates beyond the first %d that were considered and rejected are not listed. "+
			"Each carries the reason the ranking gave.", maxRows), "")

	// The scope budget's own cut. compile reports it as an Unknown too, which
	// is where a reader of the brief meets it; it is repeated here so the
	// omission block is a complete account of what this RESPONSE left out
	// rather than only of what the handler trimmed.
	if dropped := len(pack.Items) - len(contract.Scope); dropped > 0 {
		om.Hide(ReasonRowBudget, dropped,
			fmt.Sprintf("Context items the scope budget cut. %d unit(s) ranked below the %d that made the "+
				"brief. A contract listing everything has bounded nothing — but the boundary is a choice, "+
				"and this is where it fell.", dropped, len(contract.Scope)),
			"raise ?max_scope= (up to "+fmt.Sprint(maxScopeCeiling)+")")
	}
	impactFloor := "the default standard of proof"
	if contract.ImpactFloor != "" {
		impactFloor = "the " + tierWord(model.Confidence(contract.ImpactFloor)) + " standard of proof"
	}
	om.Hide(ReasonBelowConfidenceFloor, contract.ImpactHidden,
		fmt.Sprintf("Symbols an unbounded impact walk would have reached but %s kept out of the blast "+
			"radius. They are still in the index; the standard is inherited from the analysis so that a later "+
			"`repocortex verify` accuses nobody of missing a caller across an edge this brief never walked.",
			impactFloor),
		"lower the impact standard of proof with `repocortex compile --json`, then verify against that contract")
	if unresolved > 0 {
		om.Hide(ReasonUnresolvedReferences, unresolved,
			"Scope entries that name a FILE rather than a symbol. No impact analysis ran for those, so "+
				"the blast radius below is silent about them — silent, not empty.",
			"name the symbol in the task text")
	}
	if len(pack.Items) == 0 {
		om.Note(ReasonUnresolvedReferences,
			"The context engine admitted nothing for this task. The contract below is therefore almost "+
				"entirely unknowns, which is the honest answer rather than a thin brief that looks complete.",
			"try the vocabulary the repository actually uses — check the graph explorer for names")
	}

	switch {
	case len(contract.Unknowns) > 0:
		resp.Next = NextAction{
			Label: fmt.Sprintf("Answer the %d unknown(s) before starting", len(contract.Unknowns)),
			Detail: "Each one is a question the index could not answer. An agent handed this brief will " +
				"invent an answer to every one of them, and the invented answer will be indistinguishable " +
				"from a derived one in the output.",
			Screen: "contract",
		}
	case outside > 0:
		resp.Next = NextAction{
			Label:  fmt.Sprintf("%d reached symbol(s) sit outside the declared scope", outside),
			Detail: "Scope bounds the edit; blast radius bounds the consequence. Review the gap before the work starts.",
			Screen: "blast",
		}
	default:
		resp.Next = NextAction{
			Label:   "Hand this to the agent, then mark the result",
			Detail:  "`repocortex verify` checks the change that follows against this contract. The verify screen does the same over this index.",
			Command: "repocortex compile \"" + truncateIntent(intent) + "\" --json > contract.json",
			Screen:  "verify",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// truncateIntent shortens an intent for display inside a suggested command.
func truncateIntent(s string) string {
	const n = 60
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
