package infra

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Helm is the format this package is least able to read, and says so.
//
// A chart's templates are not YAML. They are Go templates that PRODUCE YAML, and
// rendering them requires Helm's engine, the chart's values, and whatever
// `--set` flags the deploy pipeline passes. Two options were available:
//
//   - Guess. Strip the `{{ … }}` actions and parse what is left. This yields
//     service names like `-api` and images like `:`, and a report full of
//     objects that do not exist is worse than a report that says it could not
//     read the file — because the second sends someone to look and the first
//     does not.
//
//   - Say so. A template is reported as present and unrendered, with its path.
//
// This does the second. What it CAN read honestly is the chart's identity
// (Chart.yaml) and its configuration surface (values.yaml key names), which is
// the half of a chart that is plain data — and the values keys are exactly the
// answer to "which config key does this deployment actually set".

func parseChart(path string, src []byte) File {
	f := File{Path: path, Format: FormatHelm}
	docs, problems := documents(src)
	f.Problems = append(f.Problems, problems...)
	if len(docs) == 0 {
		return f
	}
	w := newWalk()
	root := docs[0]

	name, ok := cleanName(text(w.field(root, "name")))
	if !ok {
		return f
	}
	u := Unit{Name: name, Type: "Chart", Line: root.Line}
	// A chart's appVersion names the application it deploys, which is the one
	// fact in Chart.yaml that connects it to a repository's own code.
	if v := w.str(w.field(root, "appVersion")); v != "" {
		u.Image = v
	}
	for _, d := range w.items(w.field(root, "dependencies")) {
		if dep, ok := cleanName(text(w.field(d, "name"))); ok {
			u.DependsOn = append(u.DependsOn, dep)
		}
		if len(u.DependsOn) >= 128 {
			break
		}
	}
	f.Units = append(f.Units, u)
	return f
}

// parseValues reads a values file as a set of dotted key names.
//
// Values files hold credentials as often as any other configuration file — a
// chart with `postgresql.auth.password` in its values is the normal case, not
// the pathological one — so this reads key paths and stops. It is the same
// treatment internal/analyze/config gives a YAML config file, applied to the
// file Helm actually reads.
func parseValues(path string, src []byte) File {
	f := File{Path: path, Format: FormatHelm}
	docs, problems := documents(src)
	f.Problems = append(f.Problems, problems...)
	if len(docs) == 0 {
		return f
	}
	w := newWalk()

	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	name, ok := cleanName(base)
	if !ok {
		name = "values"
	}

	ks := KeySet{Name: name, Type: "helm-values", Line: docs[0].Line}
	flattenKeys(w, docs[0], "", 0, &ks)
	if len(ks.Keys) > 0 {
		f.KeySets = append(f.KeySets, ks)
	}
	if w.exhausted() {
		f.Problems = append(f.Problems, Problem{
			Reason: ReasonLimit,
			Detail: "document exceeded the node budget; read in part only",
		})
	}
	return f
}

// flattenKeys walks a mapping into dotted paths, bounded in both depth and
// count.
//
// Both bounds are load-bearing rather than decorative: without them a generated
// document of 20,000 nested keys builds ever-longer dotted paths and runs the
// secret-name matcher over each, which is quadratic in the document. That exact
// shape hung the test suite in internal/analyze/config before its bounds were
// added; this package inherits the lesson rather than the incident.
func flattenKeys(w *walk, n *yaml.Node, prefix string, depth int, ks *KeySet) {
	if depth > yamlMaxDepth || len(ks.Keys) >= maxKeys {
		return
	}
	for _, e := range w.entries(n) {
		if len(ks.Keys) >= maxKeys {
			return
		}
		seg, ok := cleanName(e.key)
		if !ok {
			// A segment that is not nameable makes the whole subtree
			// unnameable: a path a caller cannot write is not a path, and
			// joining the raw text in anyway is how a value reaches a key name.
			continue
		}
		path := seg
		if prefix != "" {
			path = prefix + "." + seg
		}
		// The JOINED path is re-validated, not just the segment.
		//
		// FuzzParse found this: a self-referential anchor (`spec: &s` with
		// `containers: *s` beneath it) makes each level append the same segment
		// again, and after enough levels the dotted path is longer than any name
		// can be. Emitting it produces a key nobody can look up, and — the
		// reason this matters beyond tidiness — a name that this package would
		// itself refuse if handed it back is no longer a name it has checked.
		// Both the key and its subtree are dropped, since every path below is
		// longer still.
		if !plausibleName(path) {
			continue
		}
		v := w.deref(e.val)
		if v != nil && v.Kind == yaml.MappingNode {
			// The parent is reported too: `postgresql.auth` is a real prefix
			// that a --set overrides as a block.
			ks.Keys = append(ks.Keys, EnvKey{
				Name: path, Line: e.line, From: SourceLiteral, Secret: isSecretName(path),
			})
			flattenKeys(w, v, path, depth+1, ks)
			continue
		}
		ks.Keys = append(ks.Keys, EnvKey{
			Name: path, Line: e.line, From: SourceLiteral, Secret: isSecretName(path),
		})
	}
}

// parseHelmTemplate reports a template rather than rendering one.
//
// A template that happens to contain no actions is ordinary YAML and is read as
// Kubernetes — some charts carry a plain manifest in templates/ — but the
// format stays "helm", because that is where the file lives and what a reader
// needs to know to change it.
func parseHelmTemplate(path string, src []byte) File {
	f := File{Path: path, Format: FormatHelm}
	if looksTemplated(src) {
		f.Problems = append(f.Problems, Problem{
			Reason: ReasonTemplated,
			Detail: "Go template actions; rendering needs helm and a values file",
		})
		return f
	}
	docs, problems := documents(src)
	f.Problems = append(f.Problems, problems...)
	readKubernetes(&f, newWalk(), docs)
	return f
}
