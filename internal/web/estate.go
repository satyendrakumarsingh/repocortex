package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/multirepo"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// The system graph: the estate, federated.
//
// # The question one repository cannot answer
//
// Every other screen here answers something about ONE repository. The question
// that matters most in a microservice estate is not one of them: "if I change
// this response type, which other repositories break?" needs the frontend's
// outbound call and the backend's route in the same answer, and those live in
// two databases that know nothing of each other. The facts are already indexed
// on both sides; the only thing missing is the edge between them.
//
// # Federated, never merged
//
// Each member keeps its own index and is opened READ-ONLY, exactly as `repocortex
// diff --from-index` does and for the same reasons: members are indexed at
// different commits on different schedules, one committed run per database is
// an invariant the index swap depends on, and a combined database would have to
// be rebuilt whenever any member changed.
//
// # The confidence here is weaker than anywhere else in this product
//
// Matching '/users/{id}' against '/users/:id' against '/users/%s' is a guess no
// compiler proved, so NO fact this screen produces is ever `exact` and every
// edge's confidence is the MINIMUM over every leg of the path including the
// match itself. Each leg travels with the edge so the minimum can be checked
// rather than believed, and internal/multirepo enforces the ceiling — this
// package neither computes it nor could raise it.
//
// # A member that could not be read is DATA, not an error
//
// A federated view that silently omits a member reports a smaller system than
// the one that exists, and reads as completeness. So an unreadable member is
// still a row, `complete` is false, and every count is stated as a floor.

// estateFile is the conventional manifest looked for when ?estate= is absent.
// The name matches the one `repocortex system --members` documents.
const estateFile = "repocortex.system.yml"

// Bounds. Each member costs one opened database and several whole-run reads, so
// the member count is the number that has to stay small.
const (
	maxEstateMembers = 25
	maxEstateRows    = 200
)

// EstateResponse is the whole system-graph screen.
type EstateResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Manifest is the estate file this screen tried to read, echoed whether or
	// not it was found.
	Manifest FileRef `json:"manifest"`

	// Federated is false when no estate could be assembled. Every list below is
	// then empty for that reason and no other.
	Federated bool `json:"federated"`

	// Complete is multirepo's own verdict: false means at least one member
	// could not be read and every count here is a FLOOR rather than a total.
	// It is the single most important boolean on the screen.
	Complete bool `json:"complete"`

	// Members is printed first and never omitted, including the members whose
	// index is absent, uncommitted or unreadable.
	Members []multirepo.MemberStatus `json:"members"`

	// Edges are the cross-repository calls, each carrying its own confidence
	// tier, its own legs, and file:line evidence on both ends.
	Edges []multirepo.CrossRepoEdge `json:"cross_repo_edges"`

	SharedTables []multirepo.SharedTable `json:"shared_tables"`

	// Unjoined is everything the federation could NOT join — the half of the
	// report that keeps the other half honest.
	Unjoined multirepo.Unjoined `json:"unjoined"`

	Provenance multirepo.Provenance `json:"provenance"`

	Counts EstateCounts `json:"counts"`

	// Caveats are the standing limitations of the method, as opposed to the
	// per-run conditions in Provenance.Notes.
	Caveats []string `json:"caveats"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// EstateCounts is the arithmetic of the federation.
type EstateCounts struct {
	MembersDeclared  int `json:"members_declared"`
	MembersAvailable int `json:"members_available"`

	CrossRepoEdges int `json:"cross_repo_edges"`
	SharedTables   int `json:"shared_tables"`

	// WriteWriteTables is the finding inside SharedTables: two services writing
	// one table share an invariant neither repository states.
	WriteWriteTables int `json:"write_write_tables"`

	UnjoinedCalls  int `json:"unjoined_calls"`
	AmbiguousCalls int `json:"ambiguous_calls"`
	// UnconsumedEndpoints are routes no member of THIS system calls. A lead to
	// check, never a verdict that the route is dead.
	UnconsumedEndpoints int `json:"unconsumed_endpoints"`

	Scope string `json:"scope"`
}

func (s *Server) handleEstate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, _, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := EstateResponse{
		Meta: meta, Legend: Legend(),
		Members: []multirepo.MemberStatus{}, Edges: []multirepo.CrossRepoEdge{},
		SharedTables: []multirepo.SharedTable{},
		Unjoined: multirepo.Unjoined{
			Calls: []multirepo.UnjoinedCall{}, AmbiguousCalls: []multirepo.AmbiguousCall{},
			Endpoints: []multirepo.UnconsumedEndpoint{}, UnavailableMembers: []string{},
		},
		Caveats: []string{},
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label:   "Index this repository first",
			Detail:  "This repository is a member of its own estate; without an index it contributes nothing to one.",
			Command: "repocortex index",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Manifest = s.repoFile(r, "estate", estateFile)
	var om Omissions

	if resp.Manifest.Problem != "" {
		resp.Help = estateMissingGuidance(resp.Manifest)
		resp.Next = estateNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if _, err := os.Stat(resp.Manifest.Absolute); err != nil { //nolint:gosec // G703: Absolute was contained under RepoRoot by repoFile
		resp.Manifest.Problem = fmt.Sprintf("%s is not there.", resp.Manifest.Path)
		resp.Help = estateMissingGuidance(resp.Manifest)
		resp.Next = estateNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	declared, err := multirepo.LoadEstate(resp.Manifest.Absolute)
	if err != nil {
		resp.Manifest.Found = true
		resp.Manifest.Problem = err.Error()
		resp.Help = &Guidance{
			Title:  fmt.Sprintf("%s is not a readable estate file", resp.Manifest.Path),
			Detail: err.Error(),
			Steps: []string{
				"members:\n  - name: web\n    index: ../web/.repocortex/index.db\n  - name: orders\n    index: ../orders/.repocortex/index.db",
				"Paths are resolved relative to the FILE, so the estate means the same thing from every directory.",
			},
		}
		resp.Next = estateNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Manifest.Found = true
	resp.Counts.MembersDeclared = len(declared)

	if len(declared) < 2 {
		resp.Help = &Guidance{
			Title: fmt.Sprintf("%s names %d member(s); a system graph needs at least two",
				resp.Manifest.Path, len(declared)),
			Detail: "A system graph is the edges BETWEEN repositories. One repository is already " +
				"answered by the cross-stack and database screens here.",
			Steps: []string{
				"Add every service in the estate to " + estateFile + ".",
				"A member silently missing from that list is the exact failure this screen exists to prevent, which is why the list is a reviewed file rather than a set of flags.",
			},
		}
		resp.Next = estateNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	sort.Slice(declared, func(i, j int) bool { return declared[i].Name < declared[j].Name })
	running := declared
	if len(declared) > maxEstateMembers {
		running = declared[:maxEstateMembers]
		om.Hide(ReasonNodeBudget, len(declared)-maxEstateMembers,
			fmt.Sprintf("Member(s) beyond the first %d (by name) were not opened at all. Every "+
				"count on this screen is missing whatever they would have contributed, and an edge "+
				"into one of them appears here as an unjoined call.", maxEstateMembers),
			"repocortex system --members "+resp.Manifest.Path)
	}

	cat := newMemberCatalogue()
	defer cat.closeAll()

	members := make([]multirepo.Member, 0, len(running))
	for _, e := range running {
		members = append(members, cat.open(ctx, e))
	}

	fed, err := multirepo.New(multirepo.WithMaxEdges(maxEstateRows),
		multirepo.WithMaxUnjoined(maxEstateRows), multirepo.WithMaxSharedTables(maxEstateRows))
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: build federator: %w", err))
		return
	}
	rep, err := fed.Build(ctx, members)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: federate %d member(s): %w", len(members), err))
		return
	}

	resp.Federated = true
	resp.Complete = rep.Complete()
	resp.Members = rep.Members
	resp.Provenance = rep.Provenance
	resp.Caveats = rep.Caveats
	if rep.Edges != nil {
		resp.Edges = rep.Edges
	}
	if rep.SharedTables != nil {
		resp.SharedTables = rep.SharedTables
	}
	resp.Unjoined = normaliseUnjoined(rep.Unjoined)
	resp.Counts = estateCounts(resp, len(declared))

	for section, n := range rep.Unjoined.Truncated {
		om.Hide(ReasonRowBudget, n,
			fmt.Sprintf("The %s section was capped at %d row(s) and %d more exist.",
				section, maxEstateRows, n),
			"repocortex system --members "+resp.Manifest.Path+" --max-unjoined 1000")
	}
	if !resp.Complete {
		om.Hide(ReasonUnresolvedReferences, len(rep.Unjoined.UnavailableMembers),
			fmt.Sprintf("Member(s) could not be read: %v. Every count on this screen is a LOWER BOUND "+
				"rather than a total, and an edge into one of them cannot appear at all — it is not "+
				"drawn weakly, it is absent.", rep.Unjoined.UnavailableMembers),
			"read each member's row above; every one names the command that fixes it")
	}
	if n := len(rep.Unjoined.AmbiguousCalls); n > 0 {
		om.Hide(ReasonUnresolvedReferences, n,
			"Call(s) matched an endpoint in more than one member and were REFUSED rather than "+
				"assigned: there is no router spanning two repositories, so those are two answers "+
				"and not one. Picking either would be an invented dependency.",
			"disambiguate the route prefixes between the members named on each row")
	}

	resp.Next = estateNextAction(resp)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// normaliseUnjoined keeps every list non-null: on this screen an empty list is
// a finding, and a client must not have to tell null from empty to read it.
func normaliseUnjoined(u multirepo.Unjoined) multirepo.Unjoined {
	if u.Calls == nil {
		u.Calls = []multirepo.UnjoinedCall{}
	}
	if u.AmbiguousCalls == nil {
		u.AmbiguousCalls = []multirepo.AmbiguousCall{}
	}
	if u.Endpoints == nil {
		u.Endpoints = []multirepo.UnconsumedEndpoint{}
	}
	if u.UnavailableMembers == nil {
		u.UnavailableMembers = []string{}
	}
	return u
}

func estateCounts(resp EstateResponse, declared int) EstateCounts {
	c := EstateCounts{
		MembersDeclared:     declared,
		CrossRepoEdges:      len(resp.Edges),
		SharedTables:        len(resp.SharedTables),
		UnjoinedCalls:       len(resp.Unjoined.Calls),
		AmbiguousCalls:      len(resp.Unjoined.AmbiguousCalls),
		UnconsumedEndpoints: len(resp.Unjoined.Endpoints),
	}
	for _, m := range resp.Members {
		if m.Available {
			c.MembersAvailable++
		}
	}
	for _, t := range resp.SharedTables {
		if t.WriteWrite {
			c.WriteWriteTables++
		}
	}
	if resp.Complete {
		c.Scope = fmt.Sprintf("Every one of the %d declared member(s) was read.", declared)
	} else {
		c.Scope = fmt.Sprintf("Only %d of %d declared member(s) could be read, so every count here "+
			"is a lower bound rather than a total.", c.MembersAvailable, declared)
	}
	return c
}

func estateMissingGuidance(ref FileRef) *Guidance {
	detail := fmt.Sprintf(
		"A system graph is the edges BETWEEN repositories, and nothing in one repository's index "+
			"says which other repositories exist. That list is a property of the ESTATE — the same "+
			"for everyone on the team, changed when a service is added, and worth reviewing — so it "+
			"lives in a committed file rather than being inferred. This screen is empty because no "+
			"such file has been written, not because the estate has no cross-repository edges. %s",
		ref.Problem)
	if ref.Defaulted {
		detail += " " + ref.DefaultedFrom
	}
	return &Guidance{
		Title:  fmt.Sprintf("No estate manifest at %s", ref.Path),
		Detail: detail,
		Steps: []string{
			"Write " + estateFile + ":\n  members:\n    - name: web\n      index: ../web/.repocortex/index.db\n    - name: orders\n      index: ../orders/.repocortex/index.db",
			"Index each member in its own checkout: repocortex index --project <that checkout>",
			"repocortex system --members " + estateFile + " — the same federation from the command line",
		},
	}
}

func estateNextAction(resp EstateResponse) NextAction {
	switch {
	case !resp.Federated:
		return NextAction{
			Label:   "Declare the estate",
			Detail:  "Until it is declared, every cross-repository call in this repository is an unbound call.",
			Command: "repocortex system --members " + estateFile,
		}
	case !resp.Complete:
		return NextAction{
			Label: fmt.Sprintf("%d member(s) could not be read", len(resp.Unjoined.UnavailableMembers)),
			Detail: "Every count on this screen is a lower bound. Each member row names the command that " +
				"fixes it, and the person who has to run it is usually not the person reading this.",
			Screen: "system",
		}
	case resp.Counts.WriteWriteTables > 0:
		return NextAction{
			Label: fmt.Sprintf("%d table(s) are written by more than one member", resp.Counts.WriteWriteTables),
			Detail: "Two services writing one table share an invariant that neither repository " +
				"states. That is the highest-value finding this screen produces.",
			Screen: "db",
		}
	case resp.Counts.UnjoinedCalls > 0:
		return NextAction{
			Label:  fmt.Sprintf("%d outbound call(s) matched no endpoint in this system", resp.Counts.UnjoinedCalls),
			Detail: "Each is either a third party or a service nobody added to the estate file. Those are opposite conclusions.",
			Screen: "crossstack",
		}
	default:
		return NextAction{
			Label:  "Read every edge at its weakest leg",
			Detail: "No fact on this screen is ever `exact`: a route match compares two strings written by two teams in two syntaxes.",
			Screen: "crossstack",
		}
	}
}

// ---------------------------------------------------------------- member indexes

// memberCatalogue opens each distinct member index at most once.
//
// Two members may name the same database — two views of one repository is a
// legitimate thing to federate — and opening it twice would mean two connection
// pools over one SQLite file for no gain. The same reason internal/cli's own
// catalogue exists.
//
// # Why this package opens a database at all
//
// It is the one place in the portal that does, and it is deliberate rather than
// an oversight of the Registry port: the port serves THIS repository's index,
// and the members of an estate are by definition other files this server was
// never given. They are opened READ-ONLY and never migrated — a reader must
// never mutate somebody else's index, that index may be held by a `repocortex
// watch` in the other checkout, and a schema mismatch is reported as a member
// row rather than fixed behind the operator's back.
//
// The paths come from a manifest confined to the indexed repository (see
// repoFile) and are the repository authors' own declaration, at the same trust
// level as .repocortex.yml. The manifest is confined; the members it names are
// not, because a sibling checkout is the entire point.
type memberCatalogue struct {
	byPath map[string]*openMember
	order  []*openMember
}

type openMember struct {
	source  multirepo.MemberSource
	runID   int64
	failure string
	closeFn func()
}

func newMemberCatalogue() *memberCatalogue {
	return &memberCatalogue{byPath: map[string]*openMember{}}
}

// open returns one member, with Unavailable set instead of an error when it
// could not be read. Every failure here is DATA: a missing index is the single
// most likely reason a system graph is wrong, and returning early would hide
// every other member's findings behind the one that could not be read.
func (c *memberCatalogue) open(ctx context.Context, e multirepo.EstateMember) multirepo.Member {
	m := multirepo.Member{Name: e.Name, IndexPath: e.Index}

	abs, err := filepath.Abs(e.Index)
	if err != nil {
		m.Unavailable = fmt.Sprintf("cannot resolve %s: %v", e.Index, err)
		return m
	}
	idx, seen := c.byPath[abs]
	if !seen {
		idx = openMemberIndex(ctx, abs)
		c.byPath[abs] = idx
		if idx.closeFn != nil {
			c.order = append(c.order, idx)
		}
	}
	if idx.failure != "" {
		m.Unavailable = idx.failure
		return m
	}
	m.Source, m.RunID = idx.source, idx.runID
	return m
}

func (c *memberCatalogue) closeAll() {
	for _, idx := range c.order {
		idx.closeFn()
	}
}

func openMemberIndex(ctx context.Context, path string) *openMember {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &openMember{failure: fmt.Sprintf(
			"no index at %s. Build one in that checkout: 'repocortex index --project <that checkout>'.",
			path)}
	} else if err != nil {
		return &openMember{failure: fmt.Sprintf("cannot stat %s: %v", path, err)}
	}

	db, err := store.Open(ctx, store.Options{Path: path, ReadOnly: true})
	if err != nil {
		return &openMember{failure: fmt.Sprintf(
			"cannot open %s: %v. Re-index that checkout with this build of repocortex.", path, err)}
	}
	if err := store.CheckVersion(ctx, db.DB); err != nil {
		_ = db.Close()
		// Its own message: that member was indexed by a build whose schema this
		// one does not read, and re-indexing it is the fix. Never migrated here
		// — a read must not rewrite somebody else's index.
		return &openMember{failure: fmt.Sprintf(
			"%s is at a schema this build does not read: %v. Re-index that checkout.", path, err)}
	}

	reg := registry.New(db)
	run, err := reg.CurrentRun(ctx)
	if errors.Is(err, registry.ErrNoCommittedRun) {
		_ = db.Close()
		// Distinguished from "no index" deliberately: the two look identical to
		// somebody staring at a member with no edges, and each has a different
		// fix.
		return &openMember{failure: fmt.Sprintf(
			"%s holds no committed run, so it exposes no facts. Run 'repocortex index' in that checkout.",
			path)}
	}
	if err != nil {
		_ = db.Close()
		return &openMember{failure: fmt.Sprintf("cannot read the committed run in %s: %v", path, err)}
	}

	return &openMember{
		source:  reg,
		runID:   run.ID,
		closeFn: func() { _ = db.Close() },
	}
}
