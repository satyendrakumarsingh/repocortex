package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ConfigMind: which configuration keys this repository declares, which it
// reads, and where the two disagree.
//
// # No value is ever read, here or anywhere below here
//
// Not masked, not truncated — never read at all. registry.ConfigKeyUsage has no
// field that could hold one, and neither does anything in this file. That is a
// stronger guarantee than redaction, because a redaction list can be wrong
// about a key nobody anticipated and a struct with nowhere to put a value
// cannot leak one. `sensitive_by_name` is a judgement about the NAME, which
// requires reading nothing.
//
// # The inventory is the boring half
//
// "You declare 47 settings" is a number. "14 settings your code reads are
// declared nowhere" is a deployment about to fail at start-up, and "9 you
// declare are read by nothing" is either dead configuration or a lookup this
// scan cannot see. So the two mismatches lead and the full inventory follows,
// exactly as `cortex config` orders them.
//
// # Where the confidence tier comes from, and where it honestly cannot
//
// A key's readers are counted in config_usages, which records a site and a
// binding status but no per-site confidence — so this screen does not invent
// one. What it does carry a tier for is the CONFIGURES edge: an infrastructure
// unit (a compose service, a Dockerfile, a Kubernetes-style manifest the infra
// reader understands) declaring that it is configured by a key. Those are real
// edges with a real tier and a real file:line, and they are the rows this
// screen can prove. The read COUNTS are stated beside them with an omission
// saying their per-site evidence was not loaded, rather than each read being
// dressed up as an edge.

// ConfigReader is the port /api/config needs.
//
// Declared here, at the consumer (CLAUDE.md §1), and deliberately NOT folded
// into the composed Registry: this is one method, serving one screen, and
// widening the port every existing implementation must satisfy in order to add
// a screen is the ISP violation §3 names. Resolved per request the way
// PackReader is, so a caller wired before this screen existed still works —
// and when it genuinely cannot be resolved the answer is guidance naming the
// missing wiring, never an empty inventory that reads like a repository with no
// configuration.
type ConfigReader interface {
	ConfigReport(ctx context.Context, runID int64) ([]registry.ConfigKeyUsage, error)
}

// Compile-time proof the production adapter satisfies the port.
var _ ConfigReader = (*registry.Registry)(nil)

// configReader resolves the port for this request. See packReader for the
// reasoning behind the fallback.
func (s *Server) configReader() ConfigReader {
	if s.deps.ConfigKeys != nil {
		return s.deps.ConfigKeys
	}
	if cr, ok := s.deps.Registry.(ConfigReader); ok {
		return cr
	}
	return nil
}

// Config findings. Machine-readable, because a client renders a start-up
// failure differently from dead configuration.
const (
	// ConfigReadNotDeclared: code reads it and no configuration file here
	// declares it. The finding that catches a deployment failing at start-up.
	ConfigReadNotDeclared = "read_but_not_declared"

	// ConfigDeclaredNotRead: declared and read by nothing this scan could see.
	ConfigDeclaredNotRead = "declared_but_never_read"

	// ConfigDeclaredAndRead: both halves present. Not a finding; carried so the
	// inventory has one vocabulary rather than a finding field that is
	// sometimes empty.
	ConfigDeclaredAndRead = "declared_and_read"
)

// ConfigConsumer is one infrastructure unit that declares it is configured by a
// key — a CONFIGURES edge, with its tier and its evidence.
type ConfigConsumer struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Path     string `json:"file_path,omitempty"`

	// Relation is the recorded edge kind — CONFIGURED_BY, which pkg/model spells
	// that way — stated so the row is legible without reading the field it
	// arrived in.
	Relation string `json:"relation"`

	Confidence string   `json:"confidence"`
	Evidence   Evidence `json:"evidence"`
}

// ConfigKeyRow is one configuration key.
type ConfigKeyRow struct {
	Name string `json:"name"`

	// SymbolID is the declaration's node. Zero for a key nothing here declares.
	SymbolID int64 `json:"symbol_id,omitempty"`

	Declared bool `json:"declared"`

	// Readers is how many code sites read this key. A COUNT, not a list: see
	// the file comment for why the sites are not enumerated with evidence.
	Readers int `json:"readers"`

	// Sensitive is true when the key's NAME indicates a credential. No value
	// was read to determine it — the field is named for what it measures.
	Sensitive bool `json:"sensitive_by_name"`

	// Finding is the machine key; see the constants above.
	Finding string `json:"finding"`

	// Evidence is the declaration site for a declared key, and one recorded
	// read site for a key nothing declares. EvidenceIs says which, because a
	// file:line that means two different things depending on a boolean
	// elsewhere in the row is a file:line a reader cannot use.
	Evidence   Evidence `json:"evidence"`
	EvidenceIs string   `json:"evidence_is"`

	// ConfiguredBy is the infrastructure units that name this key, each with a
	// tier and a real location. Empty is the normal case for a repository with
	// no compose or container manifests.
	ConfiguredBy []ConfigConsumer `json:"configured_by"`
}

// ConfigCounts is the headline arithmetic.
type ConfigCounts struct {
	Keys int `json:"keys"`

	ReadNotDeclared int `json:"read_but_not_declared"`
	DeclaredNotRead int `json:"declared_but_never_read"`
	DeclaredAndRead int `json:"declared_and_read"`

	SensitiveByName int `json:"sensitive_by_name"`

	// ConfiguresEdges is how many CONFIGURES relationships survived the floor —
	// the only relationships on this screen that carry a tier.
	ConfiguresEdges int `json:"configures_edges"`
}

// ConfigFilters echoes the bounds actually applied.
type ConfigFilters struct {
	MinConfidence string `json:"min_confidence"`
	MaxKeys       int    `json:"max_keys_per_section"`
}

// ConfigResponse is the whole ConfigMind screen.
type ConfigResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Filters ConfigFilters `json:"filters"`

	// The two mismatches first, then the inventory. Three fields rather than
	// one list with a status column, because they are three different claims
	// and a client that renders only the first two is still correct.
	ReadNotDeclared []ConfigKeyRow `json:"read_but_not_declared"`
	DeclaredNotRead []ConfigKeyRow `json:"declared_but_never_read"`
	Keys            []ConfigKeyRow `json:"keys"`

	Counts ConfigCounts `json:"counts"`

	// ValuePolicy is the guarantee, stated in the payload so a client cannot
	// imply otherwise and a reader does not have to take it on trust.
	ValuePolicy string `json:"value_policy"`

	// Coverage names the declaration sources that are NOT read.
	Coverage string `json:"coverage_limit"`

	Help *Guidance  `json:"guidance,omitempty"`
	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

const (
	configValuePolicy = "No configuration VALUE was read to produce this report. Not masked, not " +
		"truncated — never read: no structure behind this endpoint has a field that could hold one. " +
		"`sensitive_by_name` is a judgement about the key's NAME and required reading nothing."

	configCoverageLimit = "Declarations are read from .env, YAML, JSON, TOML and .properties files; " +
		"usage from os.Getenv, @Value, process.env and similar lookups. Kubernetes, Helm, Terraform " +
		"and CI definitions are NOT read — that is a limit of this index, not a finding about your code."
)

// maxConsumersPerKey bounds one key's CONFIGURES list.
const maxConsumersPerKey = 25

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	resp := ConfigResponse{
		Meta: meta, Legend: Legend(),
		Filters:         ConfigFilters{MinConfidence: string(floor), MaxKeys: maxRows},
		ReadNotDeclared: []ConfigKeyRow{}, DeclaredNotRead: []ConfigKeyRow{}, Keys: []ConfigKeyRow{},
		ValuePolicy: configValuePolicy, Coverage: configCoverageLimit,
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label: "Index this repository", Command: "cortex index",
			Detail: "Configuration keys are extracted during indexing; there is nothing committed to read.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	reader := s.configReader()
	if reader == nil {
		resp.Help = &Guidance{
			Title: "This portal was started without a configuration read port",
			Detail: "The registry it was given does not implement ConfigReport, so this screen has no " +
				"source. That is missing wiring, not a repository without configuration — reporting " +
				"zero keys here would be a claim nothing checked.",
			Steps: []string{
				"cortex ui — the shipped portal wires the full registry",
				"cortex config — the same report in the terminal",
			},
		}
		resp.Next = NextAction{
			Label: "Read it in the terminal instead", Command: "cortex config --all",
			Detail: "Same source, no portal wiring required.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	keys, err := reader.ConfigReport(ctx, run.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: configuration report for run %d: %w", run.ID, err))
		return
	}
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	consumers, hiddenEdges := idx.configConsumers(floor)

	rows := make([]ConfigKeyRow, 0, len(keys))
	var readSites int
	for _, k := range keys {
		row := ConfigKeyRow{
			Name: k.Name, SymbolID: k.SymbolID, Declared: k.Declared,
			Readers: k.Readers, Sensitive: k.Secret,
			Evidence:     Evidence{FilePath: k.FilePath, Line: k.Line},
			ConfiguredBy: []ConfigConsumer{},
		}
		switch {
		case !k.Declared:
			row.Finding = ConfigReadNotDeclared
			row.EvidenceIs = "a recorded read site — nothing in this repository declares this key"
		case k.Readers == 0:
			row.Finding = ConfigDeclaredNotRead
			row.EvidenceIs = "the declaration"
		default:
			row.Finding = ConfigDeclaredAndRead
			row.EvidenceIs = "the declaration"
		}
		readSites += k.Readers
		if k.SymbolID > 0 {
			if cs := consumers[k.SymbolID]; len(cs) > 0 {
				row.ConfiguredBy = capped(cs, maxConsumersPerKey, &om, ReasonRowBudget,
					fmt.Sprintf("%s: infrastructure units beyond the first %d are not listed.",
						k.Name, maxConsumersPerKey), "")
				resp.Counts.ConfiguresEdges += len(cs)
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	var missing, unread, both []ConfigKeyRow
	for _, row := range rows {
		switch row.Finding {
		case ConfigReadNotDeclared:
			missing = append(missing, row)
		case ConfigDeclaredNotRead:
			unread = append(unread, row)
		default:
			both = append(both, row)
		}
		if row.Sensitive {
			resp.Counts.SensitiveByName++
		}
	}
	resp.Counts.Keys = len(rows)
	resp.Counts.ReadNotDeclared = len(missing)
	resp.Counts.DeclaredNotRead = len(unread)
	resp.Counts.DeclaredAndRead = len(both)

	resp.ReadNotDeclared = capped(missing, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Keys read but not declared beyond the first %d (alphabetical) are not listed. "+
			"counts.read_but_not_declared is the full figure.", maxRows), "cortex config")
	resp.DeclaredNotRead = capped(unread, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Keys declared but never read beyond the first %d are not listed.", maxRows),
		"cortex config --all")
	resp.Keys = capped(rows, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("The full inventory is shortened to the first %d key(s). counts.keys is the full "+
			"figure.", maxRows), "cortex config --all")
	for _, p := range []*[]ConfigKeyRow{&resp.ReadNotDeclared, &resp.DeclaredNotRead, &resp.Keys} {
		if *p == nil {
			*p = []ConfigKeyRow{}
		}
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenEdges,
		fmt.Sprintf("CONFIGURES relationships between an infrastructure unit and a key, recorded weaker "+
			"than %s. Each is a workload this screen does not show as depending on a key.", tierWord(floor)),
		"set the standard of proof to `low`")
	if readSites > 0 {
		om.Hide(ReasonEvidenceBudget, readSites,
			"Individual code sites that read a configuration key. This index records them as a count "+
				"and one example location per key, not as edges with per-site evidence, so the readers "+
				"figure is stated without a tier rather than each read being presented as a proven "+
				"relationship.",
			"cortex config --all, then grep the key name")
	}

	switch {
	case len(rows) == 0:
		resp.Help = &Guidance{
			Title: "This index recorded no configuration keys",
			Detail: configCoverageLimit + " An empty screen here means no declaration file and no lookup " +
				"was recognised, not that this service is configuration-free.",
			Steps: []string{
				"cortex index — configuration keys are extracted during indexing",
				"cortex config — the same report in the terminal, with the coverage note",
				"If your settings live only in Helm charts or CI definitions, they are outside what this index reads",
			},
		}
		resp.Next = NextAction{
			Label:   "Confirm the declaration sources are ones this index reads",
			Detail:  "Kubernetes, Helm, Terraform and CI definitions are not read, so a service configured entirely through them reports nothing here by construction.",
			Command: "cortex config",
		}
	case len(missing) > 0:
		resp.Next = NextAction{
			Label: fmt.Sprintf("%d key(s) are read and declared nowhere", len(missing)),
			Detail: "A service that needs these and finds nothing fails at start-up. Each can also be a " +
				"key assembled at run time or supplied by the platform, which is why this is a prompt to " +
				"check rather than a verdict.",
			Command: "cortex config",
		}
	case len(unread) > 0:
		resp.Next = NextAction{
			Label:   fmt.Sprintf("%d key(s) are declared and read by nothing", len(unread)),
			Detail:  "Either dead configuration, or a lookup this scan cannot see — a key read through a computed name will not be found.",
			Command: "cortex config --all",
		}
	default:
		resp.Next = NextAction{
			Label:   "Every declared key has a reader, and every read key is declared",
			Detail:  fmt.Sprintf("%d key(s), %d labelled sensitive by name.", len(rows), resp.Counts.SensitiveByName),
			Command: "cortex config --all",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// configConsumers collects the CONFIGURES edges pointing at each key node.
//
// One linear scan over the cached compact edges, no traversal — the edge IS the
// fact. Keyed by the key's symbol id, which is why a key nothing declares (and
// so has no node) can never acquire one: an infrastructure unit naming a key
// that no configuration file declares produced no edge at index time either.
//
// The returned int is how many qualifying edges fell below the floor.
func (idx *structuralIndex) configConsumers(floor model.Confidence) (map[int64][]ConfigConsumer, int) {
	out := map[int64][]ConfigConsumer{}
	hidden := 0
	for i := range idx.edges {
		e := idx.edges[i]
		if e.Kind != model.EdgeConfigures {
			continue
		}
		if sym, ok := idx.syms[e.Dst]; !ok || sym.Kind != model.KindConfiguration {
			continue
		}
		if !e.Confidence.AtLeast(floor) {
			hidden++
			continue
		}
		src := idx.syms[e.Src]
		name := src.Name
		if name == "" {
			name = fmt.Sprintf("symbol#%d", e.Src)
		}
		out[e.Dst] = append(out[e.Dst], ConfigConsumer{
			SymbolID: e.Src, Name: name, Kind: string(src.Kind),
			Path: idx.symbolPath(e.Src), Relation: string(model.EdgeConfigures),
			Confidence: tierOf(e.Confidence),
			Evidence:   Evidence{FilePath: e.File, Line: int(e.Line)},
		})
	}
	for id := range out {
		rows := out[id]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Name != rows[j].Name {
				return rows[i].Name < rows[j].Name
			}
			return rows[i].SymbolID < rows[j].SymbolID
		})
	}
	return out, hidden
}
