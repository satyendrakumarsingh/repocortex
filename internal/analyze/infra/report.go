package infra

import (
	"sort"
	"strings"
)

// Summary is the aggregate view over every manifest that was read.
//
// It leads with the counts a reader checks first and then with the two findings
// that are worth acting on — a Secret or ConfigMap a workload consumes that this
// repository never declares, and the files that could not be read. Both are
// mismatches rather than inventory, which is the same shape `repocortex config`
// and `repocortex deps` settled on: the inventory is the boring half.
type Summary struct {
	Files      int            `json:"files"`
	ByFormat   map[Format]int `json:"by_format"`
	Units      int            `json:"units"`
	EnvKeys    int            `json:"env_keys"`
	SecretKeys int            `json:"secret_keys"`
	KeySets    int            `json:"key_sets"`
	Problems   int            `json:"problems"`

	// Unresolved are envFrom references — whole Secrets and ConfigMaps injected
	// into a container — that no manifest in this scan declares.
	//
	// Not an error. A Secret is very often created out of band by a sealed-secret
	// controller, a cloud operator or a human running kubectl, and that is
	// precisely why it is worth reporting: those keys are part of the running
	// system's configuration and nothing in the repository can tell you their
	// names.
	Unresolved []Unresolved `json:"unresolved,omitempty"`
}

// Unresolved is one envFrom reference with no declaring object in the scan.
type Unresolved struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Unit and Path locate the consumer, so a reader knows what breaks if the
	// object is missing at deploy time.
	Unit string `json:"unit"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Summarize aggregates files that have already been parsed.
//
// Pure over its input: the caller does the walking and the reading, this does
// the arithmetic. Keeping I/O out is what lets the same function serve the CLI,
// a test, and — when the persistence hop lands — the indexing pipeline.
func Summarize(files []File) Summary {
	s := Summary{ByFormat: map[Format]int{}}

	declared := make(map[string]struct{})
	for _, f := range files {
		for _, ks := range f.KeySets {
			declared[refKey(ks.Type, ks.Name)] = struct{}{}
		}
	}

	for _, f := range files {
		s.Files++
		s.ByFormat[f.Format]++
		s.Problems += len(f.Problems)
		s.KeySets += len(f.KeySets)

		for _, ks := range f.KeySets {
			for _, k := range ks.Keys {
				s.EnvKeys++
				if k.Secret {
					s.SecretKeys++
				}
			}
		}
		for _, u := range f.Units {
			s.Units++
			for _, k := range u.Env {
				s.EnvKeys++
				if k.Secret {
					s.SecretKeys++
				}
			}
			for _, ref := range u.EnvFrom {
				if _, ok := declared[refKey(ref.Kind, ref.Name)]; ok {
					continue
				}
				s.Unresolved = append(s.Unresolved, Unresolved{
					Kind: ref.Kind, Name: ref.Name,
					Unit: u.Name, Path: f.Path, Line: ref.Line,
				})
			}
		}
	}

	// Deterministic: the same scan must produce the same report, or the output
	// cannot be diffed between runs.
	sort.Slice(s.Unresolved, func(i, j int) bool {
		a, b := s.Unresolved[i], s.Unresolved[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Line < b.Line
	})
	return s
}

// refKey namespaces a key set by its kind, because a ConfigMap and a Secret may
// share a name and consuming the wrong one is a different deployment.
//
// Names carry an optional `namespace/` prefix from readKubernetes; an envFrom
// reference never does, since it resolves within the workload's own namespace.
// Both spellings are registered so a reference matches the object that declares
// it without pretending this scanner knows the namespace a manifest will be
// applied into.
func refKey(kind, name string) string {
	return strings.ToLower(kind) + "/" + strings.ToLower(baseName(name))
}

func baseName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}
