package infra

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// A compose file is the densest statement of a system's runtime shape that most
// repositories contain: every service, the image or build context behind it,
// the ports it publishes, what it is configured with and what it waits for. It
// is also the one file that maps a SOURCE DIRECTORY to a service name, via
// build.context — the link that answers "which service does this code run in"
// without inferring anything.

func parseCompose(path string, src []byte) File {
	f := File{Path: path, Format: FormatCompose}

	docs, problems := documents(src)
	f.Problems = append(f.Problems, problems...)
	if len(docs) == 0 {
		return f
	}

	w := newWalk()
	root := docs[0] // compose is single-document; extra documents are not compose

	services := w.field(root, "services")
	for _, e := range w.entries(services) {
		if len(f.Units) >= maxUnits {
			f.Problems = append(f.Problems, Problem{Reason: ReasonLimit})
			break
		}
		name, ok := cleanName(e.key)
		if !ok {
			continue
		}
		f.Units = append(f.Units, composeService(w, name, e.line, e.val))
	}

	// Top-level `secrets:` and `configs:` name the credentials a deployment
	// declares. Only the NAMES are read: a compose secret points at a file or an
	// external store, and this package opens neither.
	for _, section := range []struct {
		key, typ string
		secret   bool
	}{
		{"secrets", "compose-secret", true},
		{"configs", "compose-config", false},
	} {
		for _, e := range w.entries(w.field(root, section.key)) {
			name, ok := cleanName(e.key)
			if !ok {
				continue
			}
			f.KeySets = append(f.KeySets, KeySet{
				Name: name, Type: section.typ, Line: e.line, Secret: section.secret,
			})
		}
	}

	if w.exhausted() {
		f.Problems = append(f.Problems, Problem{
			Reason: ReasonLimit,
			Detail: "document exceeded the node budget; read in part only",
		})
	}
	return f
}

func composeService(w *walk, name string, line int, n *yaml.Node) Unit {
	u := Unit{Name: name, Type: "service", Line: line}

	u.Image = w.str(w.field(n, "image"))

	switch b := w.field(n, "build"); {
	case b == nil:
	case b.Kind == yaml.ScalarNode:
		u.BuildContext = w.str(b)
	default:
		u.BuildContext = w.str(w.field(b, "context"))
		u.Dockerfile = w.str(w.field(b, "dockerfile"))
	}

	u.Ports = composePorts(w, w.field(n, "ports"), false)
	u.Ports = append(u.Ports, composePorts(w, w.field(n, "expose"), true)...)
	u.Env = composeEnv(w, w.field(n, "environment"))
	u.EnvFiles = composeEnvFiles(w, w.field(n, "env_file"))
	u.DependsOn = composeDependsOn(w, w.field(n, "depends_on"))
	u.Command = composeCommand(w, n)
	u.Limits = composeLimits(w, n)
	return u
}

// composePorts reads both the string form ("8080:80/tcp", "127.0.0.1:8080:80")
// and the long form ({target, published, protocol}).
//
// containerOnly is set for `expose:`, which publishes nothing — the distinction
// between "the container listens here" and "the host can reach it here" is the
// one a reader is actually asking about.
func composePorts(w *walk, n *yaml.Node, containerOnly bool) []Port {
	var out []Port
	for _, item := range w.items(n) {
		if len(out) >= 256 {
			break
		}
		if item.Kind == yaml.MappingNode {
			p := Port{
				Name:     w.str(w.field(item, "name")),
				Protocol: strings.ToLower(text(w.field(item, "protocol"))),
				Line:     item.Line,
			}
			if v, ok := num(w.field(item, "target")); ok {
				p.Container = v
			}
			if v, ok := num(w.field(item, "published")); ok && !containerOnly {
				p.Published = v
			}
			if p.Container == 0 && p.Published == 0 {
				p.Raw = w.str(w.field(item, "target"))
				if p.Raw == "" {
					// A long-form entry that names no port at all. Dropped
					// rather than emitted as a row of zeroes, which would read
					// as "this service exposes a port" about nothing.
					continue
				}
			}
			out = append(out, p)
			continue
		}
		if s := text(item); s != "" {
			p := parsePortSpec(s, containerOnly)
			p.Line = item.Line
			out = append(out, p)
		}
	}
	return out
}

// parsePortSpec reads the colon-separated short form.
//
// The container port is always the LAST field, which is what makes this
// tractable across "80", "8080:80" and "127.0.0.1:8080:80" without knowing which
// shape was written.
func parsePortSpec(s string, containerOnly bool) Port {
	spec, proto := s, ""
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		spec, proto = spec[:i], strings.ToLower(spec[i+1:])
	}
	if proto != "tcp" && proto != "udp" {
		proto = ""
	}
	parts := strings.Split(spec, ":")
	p := Port{Protocol: proto}
	container, cok := atoiPort(parts[len(parts)-1])
	if !cok {
		// A range ("8000-8010:80") or a variable ("${PORT}:80"). Kept verbatim
		// rather than dropped: it is still an open port.
		p.Raw = safe(s)
		return p
	}
	p.Container = container
	if !containerOnly && len(parts) >= 2 {
		if published, ok := atoiPort(parts[len(parts)-2]); ok {
			p.Published = published
		} else {
			p.Raw = safe(s)
		}
	}
	return p
}

// atoiPort accepts 1..65535 only; see num in yamlnode.go for why zero is not a
// port.
func atoiPort(s string) (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < 1 || v > 65535 {
		return 0, false
	}
	return v, true
}

// composeEnv reads `environment:` in both shapes.
//
// Mapping form: only the KEY node is touched. The value node is right there in
// the tree and is never passed to anything.
//
// Sequence form: "KEY=value" is cut at the FIRST `=` and the right-hand side is
// discarded in the same expression. A bare "KEY" with no `=` is compose's
// pass-through form, which takes the value from whatever ran docker — worth
// distinguishing, because such a key is configured somewhere this repository
// cannot show you.
func composeEnv(w *walk, n *yaml.Node) []EnvKey {
	var out []EnvKey
	n = w.deref(n)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.MappingNode {
		for _, e := range w.entries(n) {
			if len(out) >= maxKeys {
				break
			}
			if k, ok := newEnvKey(e.key, e.line, SourceLiteral, ""); ok {
				out = append(out, k)
			}
		}
		return out
	}
	for _, item := range w.items(n) {
		if len(out) >= maxKeys {
			break
		}
		raw := text(item)
		if raw == "" {
			continue
		}
		name, _, found := strings.Cut(raw, "=")
		from := SourceLiteral
		if !found {
			from = SourceHost
		}
		if k, ok := newEnvKey(name, item.Line, from, ""); ok {
			out = append(out, k)
		}
	}
	return out
}

// composeEnvFiles records the paths, and does not open them.
//
// Opening one would mean reading a .env — a file whose entire content
// internal/security classifies as sensitive. The path is the useful fact
// ("this service is configured by deploy/prod.env"); the contents belong to
// internal/analyze/config, which reads names and never values.
func composeEnvFiles(w *walk, n *yaml.Node) []string {
	n = w.deref(n)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		if s := w.str(n); s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, item := range w.items(n) {
		if len(out) >= 64 {
			break
		}
		s := ""
		if item.Kind == yaml.MappingNode {
			s = w.str(w.field(item, "path"))
		} else {
			s = w.str(item)
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func composeDependsOn(w *walk, n *yaml.Node) []string {
	n = w.deref(n)
	if n == nil {
		return nil
	}
	var out []string
	if n.Kind == yaml.MappingNode {
		for _, e := range w.entries(n) {
			if name, ok := cleanName(e.key); ok {
				out = append(out, name)
			}
			if len(out) >= 256 {
				break
			}
		}
		return out
	}
	for _, item := range w.items(n) {
		if name, ok := cleanName(text(item)); ok {
			out = append(out, name)
		}
		if len(out) >= 256 {
			break
		}
	}
	return out
}

// composeCommand prefers entrypoint, for the reason CMD gives way to ENTRYPOINT
// in a Dockerfile: it names the process, and the command is its arguments.
func composeCommand(w *walk, n *yaml.Node) string {
	for _, key := range []string{"entrypoint", "command"} {
		v := w.field(n, key)
		if v == nil {
			continue
		}
		if v.Kind == yaml.ScalarNode {
			return w.str(v)
		}
		parts := make([]string, 0, len(v.Content))
		for _, item := range w.items(v) {
			parts = append(parts, text(item))
			if len(parts) >= 64 {
				break
			}
		}
		if len(parts) > 0 {
			// Joined and THEN redacted, so a split token like ["--token",
			// "ghp_…"] is redacted as the credential it is rather than passing
			// through as two innocent-looking strings.
			return safe(strings.Join(parts, " "))
		}
	}
	return ""
}

// composeLimits reads deploy.resources.{limits,reservations}.
func composeLimits(w *walk, n *yaml.Node) []Limit {
	res := w.path(n, "deploy", "resources")
	if res == nil {
		return nil
	}
	byName := map[string]*Limit{}
	order := []string{}
	take := func(node *yaml.Node, set func(*Limit, string)) {
		for _, e := range w.entries(node) {
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
	take(w.field(res, "limits"), func(l *Limit, v string) { l.Limit = v })
	take(w.field(res, "reservations"), func(l *Limit, v string) { l.Request = v })

	out := make([]Limit, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}
