package pipeline

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Turning a deployment manifest into graph nodes.
//
// This is the layer that decides how a compose service relates to a
// configuration key, and it lives here rather than in internal/analyze/infra
// for one reason: it is a statement about TWO extractors at once. A key name a
// Deployment sets and a key name a .env declares are the same setting, so they
// must become the same kind of node under the same FQN rule — and that rule
// (analyze.ConfigFQN) belongs to the package that already owns configuration
// nodes. Duplicating it inside the infra extractor would be a second definition
// of one identity, which is the failure model.DefaultFQN's doc comment records
// in detail: two ends of one naming rule drifted apart and every access
// resolved to nothing.
//
// # What becomes a node
//
//	unit          a deployment_unit symbol, FQN unit:<manifest>#<name>
//	env key name  a configuration symbol, FQN config:<NAME> — the SAME node
//	              kind and FQN a .env declaration produces
//
// # What becomes an edge, here, during indexing
//
//	unit -CONFIGURED_BY-> key    the unit sets this key
//	unit -DEPENDS_ON->    unit   compose depends_on
//
// Both are INTRA-FILE: a compose file names its own services and its own keys,
// so both ends exist in the file being written and the edge is complete now.
// The cross-file join — a unit to the endpoints built from its context — cannot
// be, and is done by registry.BindInfraUnits after every file has been seen.
// This is the same split writeAnalysis makes between an exposure and an access,
// for the same reason.
//
// # Values
//
// Nothing below reads or carries one. infra.EnvKey has no value field, so
// there is no expression here that could name one.

// infraSymbols builds the nodes one manifest declares, in a deterministic
// order.
//
// Order is declaration order — the extractor's, which is file order — and never
// map order. Symbol row ids are assigned in the order they are written, so
// ranging a map here would hand two identical runs different ids, and ids reach
// the digest indirectly through every edge that references them.
func infraSymbols(f infra.File) []model.Symbol {
	var out []model.Symbol

	add := func(s model.Symbol) {
		s.ID = infraPlaceholderID(len(out))
		out = append(out, s)
	}

	for _, u := range f.Units {
		add(model.Symbol{
			Kind: infra.KindUnit,
			Name: u.Name,
			FQN:  infra.UnitFQN(f.Path, u.Name),
			Range: model.Range{
				Start: model.Position{Line: u.Line},
				End:   model.Position{Line: u.Line},
			},
			// Signature is the field that already means "how this thing is
			// declared", so it carries the unit's own vocabulary and image the
			// way a dependency node carries its ecosystem and version. It is
			// assembled from an image reference and a type name, never from a
			// manifest's values.
			Signature: infraSignature(u),
			// A deployment unit is the public face of a repository at run time.
			// Marking it unexported would hide it from every report that
			// filters on exported symbols, which is most of them.
			Exported: true,
			// The manifest states this outright. It is not inferred, and it is
			// not compiler-proven either — nothing compiled a compose file.
			Confidence: model.ConfidenceHigh,
			Source:     model.SourceConfig,
		})
		for _, k := range u.Env {
			add(configKeySymbol(k))
		}
	}
	for _, ks := range f.KeySets {
		for _, k := range ks.Keys {
			add(configKeySymbol(k))
		}
	}

	return dedupeByFQN(out)
}

// configKeySymbol makes one key NAME a configuration node.
//
// Same kind and same FQN as internal/analyze/config produces for a key declared
// in a .env, which is what lets registry.BindConfigUsages bind
// os.Getenv("DATABASE_URL") in source to the Deployment that supplies it. Two
// node kinds for one setting would have made that join impossible to express.
func configKeySymbol(k infra.EnvKey) model.Symbol {
	return model.Symbol{
		Kind: model.KindConfiguration,
		Name: k.Name,
		FQN:  analyze.ConfigFQN(k.Name),
		Range: model.Range{
			Start: model.Position{Line: k.Line},
			End:   model.Position{Line: k.Line},
		},
		Exported:   true,
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceConfig,
	}
}

// infraSignature renders how a unit is declared, without reading a value.
func infraSignature(u infra.Unit) string {
	if u.Image == "" {
		return u.Type
	}
	return u.Type + " " + u.Image
}

// infraPlaceholderID mirrors analyze's placeholder contract: a negative id well
// below the ordinary range, for a symbol that does not exist yet.
func infraPlaceholderID(ordinal int) int64 { return -int64(ordinal) - 900000 }

// dedupeByFQN collapses repeats, keeping the first.
//
// Required rather than tidy. A compose file where two services both set DB_HOST
// declares the key twice, and the sink's own duplicate collapse keys on (fqn,
// line) — two different lines, so it would let both through and the manifest
// would contribute two `config:DB_HOST` nodes. Every reader that looks a key up
// by name would then get whichever one it happened to find.
func dedupeByFQN(in []model.Symbol) []model.Symbol {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, dup := seen[s.FQN]; dup {
			continue
		}
		seen[s.FQN] = struct{}{}
		out = append(out, s)
	}
	return out
}

// infraRows converts one manifest into the registry rows and edges it implies.
//
// symbolID resolves an FQN to the row id the sink has just written, or 0 when
// the symbol did not land — which is a gap to record, never a reason to invent
// a reference.
func infraRows(
	f infra.File,
	symbolID func(fqn string) int64,
) (units []registry.InfraUnitRecord, keySets []registry.InfraKeySetRecord, problems []registry.InfraProblem) {
	for _, u := range f.Units {
		rec := registry.InfraUnitRecord{
			InfraUnit: registry.InfraUnit{
				SymbolID:     symbolID(infra.UnitFQN(f.Path, u.Name)),
				Name:         u.Name,
				Type:         u.Type,
				Format:       string(f.Format),
				Image:        u.Image,
				BuildContext: u.BuildContext,
				Dockerfile:   u.Dockerfile,
				Final:        u.Final,
				Line:         u.Line,
			},
		}
		for _, p := range u.Ports {
			line := p.Line
			if line == 0 {
				// The format stated nothing better than the unit's own header.
				// The unit's line is the honest fallback: it is where a reader
				// goes to change the port, and a zero would render as "line 0",
				// which is a location that does not exist.
				line = u.Line
			}
			rec.Ports = append(rec.Ports, registry.InfraPort{
				Container: p.Container, Published: p.Published,
				Protocol: p.Protocol, Name: p.Name, Raw: p.Raw, Line: line,
			})
		}
		for _, k := range u.Env {
			rec.Keys = append(rec.Keys, registry.InfraKey{
				SymbolID: symbolID(analyze.ConfigFQN(k.Name)),
				Name:     k.Name,
				From:     string(k.From),
				Ref:      k.Ref,
				Secret:   k.Secret,
				Line:     k.Line,
			})
		}
		for _, e := range u.EnvFrom {
			rec.EnvFrom = append(rec.EnvFrom, registry.InfraEnvFrom{
				Kind: e.Kind, Name: e.Name, Prefix: e.Prefix,
				Optional: e.Optional, Line: e.Line,
			})
		}
		for _, l := range u.Limits {
			rec.Limits = append(rec.Limits, registry.InfraLimit{
				Resource: l.Resource, Request: l.Request, Limit: l.Limit,
			})
		}
		units = append(units, rec)
	}

	for _, ks := range f.KeySets {
		set := registry.InfraKeySetRecord{Name: ks.Name, Type: ks.Type, Line: ks.Line}
		for _, k := range ks.Keys {
			set.Keys = append(set.Keys, registry.InfraKey{
				KeySet:     ks.Name,
				KeySetType: ks.Type,
				SymbolID:   symbolID(analyze.ConfigFQN(k.Name)),
				Name:       k.Name,
				From:       string(k.From),
				Ref:        k.Ref,
				// A key in a Secret is a credential by construction, whatever
				// the individual key is called.
				Secret: k.Secret || ks.Secret,
				Line:   k.Line,
			})
		}
		keySets = append(keySets, set)
	}

	for _, p := range f.Problems {
		reason := p.Reason
		if reason == "" {
			// A skip the walker recorded without naming a reason. Named here
			// rather than dropped: a nameless problem is still a manifest
			// nobody read, and dropping it is how "unreadable" becomes
			// "absent".
			reason = "unreadable"
		}
		// p.Detail is DROPPED here, on purpose and not for brevity. It is the
		// parser's own message, and a parser quotes the text it failed on:
		// FuzzInfraPersistence planted a value in a manifest and go-yaml
		// returned it inside "unknown anchor '<value>' referenced". Redaction
		// matches credential shapes and cannot know an arbitrary token was
		// somebody's password, so the message is not persisted at all. The
		// reason and the line are what a reader acts on; `cortex infra
		// --disk` still shows the full message without storing it.
		problems = append(problems, registry.InfraProblem{
			Format: string(f.Format), Reason: reason, Line: p.Line,
		})
	}
	return units, keySets, problems
}

// infraEdges builds the intra-file relationships a manifest states.
//
// Deduplicated on the full edge identity before returning, because the schema's
// UNIQUE constraint is (run, src, dst, kind, evidence_file, evidence_line) and
// a plain INSERT that collides fails the whole file's savepoint. A Dockerfile
// line reading `ENV TOKEN=a TOKEN=b` produces exactly that collision: one key
// name, one line, two declarations.
func infraEdges(f infra.File, symbolID func(fqn string) int64) []model.Edge {
	var out []model.Edge
	seen := map[edgeKey]struct{}{}
	emit := func(e model.Edge) {
		if e.SrcID == 0 || e.DstID == 0 {
			return
		}
		k := edgeKey{e.SrcID, e.DstID, string(e.Kind), e.Evidence.Range.Start.Line}
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}

	for _, u := range f.Units {
		unitID := symbolID(infra.UnitFQN(f.Path, u.Name))
		if unitID == 0 {
			continue
		}
		for _, k := range u.Env {
			// CONFIGURED_BY, from the unit to the key: "this workload is
			// configured by DATABASE_URL". The same edge kind a function
			// reading that key acquires, pointing at the same node — which is
			// what makes "what does changing this key restart" one query over
			// one edge type rather than a special case.
			emit(model.Edge{
				SrcID: unitID, DstID: symbolID(analyze.ConfigFQN(k.Name)),
				Kind:       model.EdgeConfigures,
				Confidence: model.ConfidenceHigh,
				Evidence: model.Evidence{
					FilePath: f.Path,
					Range:    model.Range{Start: model.Position{Line: k.Line}},
					Source:   model.SourceConfig,
					Detail:   "declared " + string(k.From),
				},
			})
		}
		for _, dep := range u.DependsOn {
			// depends_on names a service in THIS file. One that names something
			// else — an override file's service, a typo — resolves to no
			// symbol and emits nothing, which is the honest outcome: the
			// dependency is real but its target is not something this index
			// contains, and pointing the edge at the nearest similar name
			// would be a fabricated deployment ordering.
			emit(model.Edge{
				SrcID: unitID, DstID: symbolID(infra.UnitFQN(f.Path, dep)),
				Kind:       model.EdgeDependsOn,
				Confidence: model.ConfidenceHigh,
				Evidence: model.Evidence{
					FilePath: f.Path,
					Range:    model.Range{Start: model.Position{Line: u.Line}},
					Source:   model.SourceConfig,
					Detail:   "depends_on",
				},
			})
		}
	}
	return out
}

type edgeKey struct {
	src, dst int64
	kind     string
	line     int
}

// InfraBindSummary is what a caller reports after the deployment join.
type InfraBindSummary = registry.UnitBindStats

// bindInfra runs the cross-file half of the deployment join.
//
// Split out of Resolve so the ordering argument is stated once: it must run
// after every file has been indexed, because it matches a manifest's build
// context against the endpoints declared beneath it, and on a run in progress
// "the endpoints beneath it" is a moving target.
func bindInfra(ctx context.Context, reg *registry.Registry, runID int64) (InfraBindSummary, error) {
	st, err := reg.BindInfraUnits(ctx, runID)
	if err != nil {
		return st, fmt.Errorf("bind deployment units: %w", err)
	}
	return st, nil
}
