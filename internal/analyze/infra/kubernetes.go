package infra

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Kubernetes is where the deployment facts actually live for most systems that
// have any: the image a service runs, the port it listens on, the environment it
// is handed, and — the part no source file states — which Secret supplies which
// key.
//
// Every document becomes at least one Unit, including the kinds this package has
// no special reading for. An inventory that lists a ServiceAccount by name and
// says nothing else about it is honest; omitting it would suggest the file
// declared less than it did.

// podSpecPath maps a workload kind to where its pod template lives. Kinds absent
// from this map are still reported — as an object of that kind — they simply
// have no containers to read.
var podSpecPath = map[string][]string{
	"Pod":         {"spec"},
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"ReplicaSet":  {"spec", "template", "spec"},
	"Job":         {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
	// Argo Rollouts reuse the Deployment shape, and a repository that uses them
	// has no Deployments at all — omitting it would report such a system as
	// having no workloads.
	"Rollout": {"spec", "template", "spec"},
}

// parseGenericYAML decides whether a plain .yaml file is a Kubernetes manifest
// and reads it if so.
//
// The second return is false for YAML that is simply not infrastructure — a CI
// config, a linter's settings — which is the common case and must not appear in
// a deployment report. It is TRUE for a file that looks like a manifest and
// could not be read: that is a finding, not a non-event.
func parseGenericYAML(path string, src []byte) (File, bool) {
	docs, problems := documents(src)
	f := File{Path: path, Format: FormatKubernetes, Problems: problems}

	w := newWalk()
	any := false
	for _, doc := range docs {
		if isKubernetesDoc(w, doc) {
			any = true
			break
		}
	}
	if !any {
		// Nothing parsed as Kubernetes. If the TEXT says otherwise — the file
		// contains apiVersion and kind but the parser could not get to them —
		// then this is a manifest we failed to read, and saying "no manifests
		// found" about it would be the exact error this package exists to avoid.
		if len(problems) > 0 && looksKubernetes(src) {
			return f, true
		}
		return File{}, false
	}
	readKubernetes(&f, w, docs)
	return f, true
}

func isKubernetesDoc(w *walk, doc *yaml.Node) bool {
	if doc == nil || doc.Kind != yaml.MappingNode {
		return false
	}
	return text(w.field(doc, "apiVersion")) != "" && text(w.field(doc, "kind")) != ""
}

// readKubernetes fills a File from already-decoded documents. Shared with the
// Helm reader, which reaches the same place once a template turns out to contain
// no template actions.
func readKubernetes(f *File, w *walk, docs []*yaml.Node) {
	for _, doc := range docs {
		if len(f.Units) >= maxUnits {
			f.Problems = append(f.Problems, Problem{Reason: ReasonLimit})
			break
		}
		if !isKubernetesDoc(w, doc) {
			continue
		}
		kind := text(w.field(doc, "kind"))
		name, ok := cleanName(text(w.path(doc, "metadata", "name")))
		if !ok {
			// A nameless object is a manifest that will not apply. Named
			// "(unnamed)" rather than skipped, so the count of objects in the
			// file stays true.
			name = "(unnamed)"
		}
		if ns := text(w.path(doc, "metadata", "namespace")); ns != "" {
			if n, ok := cleanName(ns + "/" + name); ok {
				name = n
			}
		}
		line := doc.Line

		switch kind {
		case "ConfigMap", "Secret":
			f.KeySets = append(f.KeySets, configKeySet(w, kind, name, line, doc))
			continue
		case "Service":
			f.Units = append(f.Units, serviceUnit(w, kind, name, line, doc))
			continue
		}

		spec := podSpecPath[kind]
		if spec == nil {
			// Every other kind: recorded by name and kind. Ingress, HPA,
			// ServiceAccount, a CRD nobody here has heard of.
			f.Units = append(f.Units, Unit{Name: name, Type: kind, Line: line})
			continue
		}
		pod := w.path(doc, spec...)
		if pod == nil {
			f.Units = append(f.Units, Unit{Name: name, Type: kind, Line: line})
			continue
		}
		before := len(f.Units)
		for _, listKey := range []string{"initContainers", "containers"} {
			for _, c := range w.items(w.field(pod, listKey)) {
				if len(f.Units) >= maxUnits {
					break
				}
				f.Units = append(f.Units, containerUnit(w, kind, name, listKey, c))
			}
		}
		if len(f.Units) == before {
			// A workload whose pod template declares no containers. Reported as
			// the workload it is; the alternative is a Deployment that vanishes
			// from the inventory.
			f.Units = append(f.Units, Unit{Name: name, Type: kind, Line: line})
		}
	}

	if w.exhausted() {
		f.Problems = append(f.Problems, Problem{
			Reason: ReasonLimit,
			Detail: "document exceeded the node budget; read in part only",
		})
	}
}

// containerUnit reads one container. Its name is `workload/container`, because
// "which service does this code run in" is answered by the pair and not by
// either half: two workloads routinely run a container called `app`.
func containerUnit(w *walk, kind, workload, listKey string, c *yaml.Node) Unit {
	cname, ok := cleanName(text(w.field(c, "name")))
	if !ok {
		cname = "(unnamed)"
	}
	typ := kind
	if listKey == "initContainers" {
		// An init container runs to completion before the workload starts. It
		// is not the thing serving traffic, and conflating the two would put an
		// image nobody's request ever reaches at the centre of the report.
		typ = kind + "/init"
	}

	u := Unit{
		Name:    workload + "/" + cname,
		Type:    typ,
		Line:    c.Line,
		Image:   w.str(w.field(c, "image")),
		Command: k8sCommand(w, c),
	}

	for _, p := range w.items(w.field(c, "ports")) {
		if len(u.Ports) >= 128 {
			break
		}
		port := Port{
			Name:     w.str(w.field(p, "name")),
			Protocol: strings.ToLower(text(w.field(p, "protocol"))),
			Line:     p.Line,
		}
		if v, ok := num(w.field(p, "containerPort")); ok {
			port.Container = v
		}
		if v, ok := num(w.field(p, "hostPort")); ok {
			port.Published = v
		}
		if port.Container == 0 && port.Published == 0 {
			port.Raw = w.str(w.field(p, "containerPort"))
			if port.Raw == "" {
				continue
			}
		}
		u.Ports = append(u.Ports, port)
	}

	u.Env = k8sEnv(w, w.field(c, "env"))
	u.EnvFrom = k8sEnvFrom(w, w.field(c, "envFrom"))
	u.Limits = k8sLimits(w, w.field(c, "resources"))
	return u
}

// k8sEnv reads `env:`.
//
// The `value` field is never touched. What IS read is `valueFrom`, because where
// a value comes from is a deployment fact of its own: a key fed by
// secretKeyRef tells you that rotating that Secret restarts this container, and
// that fact appears in no source file.
func k8sEnv(w *walk, n *yaml.Node) []EnvKey {
	var out []EnvKey
	for _, item := range w.items(n) {
		if len(out) >= maxKeys {
			break
		}
		raw := text(w.field(item, "name"))
		if raw == "" {
			continue
		}
		from, ref := SourceLiteral, ""
		if vf := w.field(item, "valueFrom"); vf != nil {
			switch {
			case w.field(vf, "secretKeyRef") != nil:
				from = SourceSecret
				ref = keyRef(w, w.field(vf, "secretKeyRef"))
			case w.field(vf, "configMapKeyRef") != nil:
				from = SourceConfigMap
				ref = keyRef(w, w.field(vf, "configMapKeyRef"))
			case w.field(vf, "fieldRef") != nil:
				from = SourceDownward
				ref = text(w.path(vf, "fieldRef", "fieldPath"))
			case w.field(vf, "resourceFieldRef") != nil:
				from = SourceDownward
				ref = text(w.path(vf, "resourceFieldRef", "resource"))
			}
		}
		if k, ok := newEnvKey(raw, item.Line, from, ref); ok {
			out = append(out, k)
		}
	}
	return out
}

// keyRef renders a secretKeyRef/configMapKeyRef as `object:key`. Both halves are
// names.
func keyRef(w *walk, n *yaml.Node) string {
	name := text(w.field(n, "name"))
	key := text(w.field(n, "key"))
	switch {
	case name != "" && key != "":
		return name + ":" + key
	case name != "":
		return name
	}
	return key
}

func k8sEnvFrom(w *walk, n *yaml.Node) []SourceRef {
	var out []SourceRef
	for _, item := range w.items(n) {
		if len(out) >= 128 {
			break
		}
		for _, r := range []struct{ field, kind string }{
			{"secretRef", "Secret"}, {"configMapRef", "ConfigMap"},
		} {
			ref := w.field(item, r.field)
			if ref == nil {
				continue
			}
			name, ok := cleanName(text(w.field(ref, "name")))
			if !ok {
				continue
			}
			out = append(out, SourceRef{
				Kind: r.kind, Name: name,
				Prefix:   safe(text(w.field(item, "prefix"))),
				Optional: boolOf(w.field(ref, "optional")),
				Line:     item.Line,
			})
		}
	}
	return out
}

func k8sLimits(w *walk, n *yaml.Node) []Limit {
	if n == nil {
		return nil
	}
	byName := map[string]*Limit{}
	var order []string
	take := func(section string, set func(*Limit, string)) {
		for _, e := range w.entries(w.field(n, section)) {
			name, ok := cleanName(e.key)
			if !ok {
				continue
			}
			l, seen := byName[name]
			if !seen {
				l = &Limit{Resource: name}
				byName[name] = l
				order = append(order, name)
			}
			set(l, w.str(e.val))
		}
	}
	take("requests", func(l *Limit, v string) { l.Request = v })
	take("limits", func(l *Limit, v string) { l.Limit = v })

	out := make([]Limit, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

func k8sCommand(w *walk, c *yaml.Node) string {
	var parts []string
	for _, key := range []string{"command", "args"} {
		for _, item := range w.items(w.field(c, key)) {
			parts = append(parts, text(item))
			if len(parts) >= 64 {
				break
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	// Joined first, then redacted: a credential split across argv entries
	// (`--token`, `ghp_…`) is only recognisable once the argument sits next to
	// the flag that names it.
	return safe(strings.Join(parts, " "))
}

// serviceUnit reads a Service: the ports are the ones the rest of the cluster
// reaches this workload on, which is a different set from the container's own.
func serviceUnit(w *walk, kind, name string, line int, doc *yaml.Node) Unit {
	u := Unit{Name: name, Type: kind, Line: line}
	for _, p := range w.items(w.path(doc, "spec", "ports")) {
		if len(u.Ports) >= 128 {
			break
		}
		port := Port{
			Name:     w.str(w.field(p, "name")),
			Protocol: strings.ToLower(text(w.field(p, "protocol"))),
			Line:     p.Line,
		}
		if v, ok := num(w.field(p, "port")); ok {
			port.Published = v
		}
		if v, ok := num(w.field(p, "targetPort")); ok {
			port.Container = v
		} else if s := w.str(w.field(p, "targetPort")); s != "" {
			// A named targetPort refers to a port name on the container. Kept as
			// written; resolving it means joining across documents, and a wrong
			// join here would report a service pointing at a port it does not.
			port.Raw = s
		}
		if v, ok := num(w.field(p, "nodePort")); ok && port.Published == 0 {
			port.Published = v
		}
		if port.Container == 0 && port.Published == 0 && port.Raw == "" {
			continue
		}
		u.Ports = append(u.Ports, port)
	}
	return u
}

// configKeySet reads a ConfigMap or a Secret.
//
// For a Secret this is the sharpest instance of the whole package's rule:
// `data` maps key names to base64-encoded credentials, and this reads the
// left-hand side of that map and nothing else. The right-hand side is in the
// parse tree and is never passed to a function, assigned to a field, or
// rendered.
func configKeySet(w *walk, kind, name string, line int, doc *yaml.Node) KeySet {
	ks := KeySet{Name: name, Type: kind, Line: line, Secret: kind == "Secret"}
	for _, section := range []string{"data", "stringData", "binaryData"} {
		for _, e := range w.entries(w.field(doc, section)) {
			if len(ks.Keys) >= maxKeys {
				return ks
			}
			from := SourceConfigMap
			if kind == "Secret" {
				from = SourceSecret
			}
			if k, ok := newEnvKey(e.key, e.line, from, name); ok {
				ks.Keys = append(ks.Keys, k)
			}
		}
	}
	return ks
}
