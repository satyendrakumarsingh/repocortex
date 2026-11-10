package infra

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The YAML documents this package reads are written by whoever owns the
// repository being indexed, which makes them attacker-influenced input to a tool
// that runs unattended. Two consequences, both handled here rather than in each
// extractor:
//
//   - Aliases are followed, because `<<: *defaults` is ordinary in compose
//     files and refusing to follow one would silently under-report a service's
//     ports. Following them is also how a small document expands into a huge
//     one — the billion-laughs shape — so every traversal spends from a shared
//     node budget and stops when it runs out.
//
//   - yaml.v3 has panicked on malformed input within living memory
//     (CVE-2022-28948). A truncated manifest must produce a Problem a human can
//     read, not a stack trace that takes an index run down with it.
const (
	// yamlNodeBudget bounds nodes visited per FILE, across every document in it.
	yamlNodeBudget = 500000
	// yamlMaxDepth bounds nesting. Nothing in a manifest is legitimately this
	// deep; a document claiming to be is generated or hostile.
	yamlMaxDepth = 64
	// aliasChain bounds how many alias hops one reference may take.
	aliasChain = 100
)

// walk carries the shared budget for one file's traversal.
type walk struct{ budget int }

func newWalk() *walk { return &walk{budget: yamlNodeBudget} }

func (w *walk) spend() bool {
	if w.budget <= 0 {
		return false
	}
	w.budget--
	return true
}

// exhausted reports whether the budget ran out, so a caller can say the file was
// read only in part rather than pretending it was read whole.
func (w *walk) exhausted() bool { return w.budget <= 0 }

// deref follows aliases to the node they name.
func (w *walk) deref(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if i >= aliasChain || !w.spend() {
			return nil
		}
		n = n.Alias
	}
	return n
}

// entry is one key/value pair of a mapping, with the line the KEY was written
// on — which is the line a report should cite, because that is where a reader
// looks.
type entry struct {
	key  string
	line int
	val  *yaml.Node
}

// entries returns a mapping's pairs, expanding YAML merge keys (`<<`).
//
// Merged pairs come last and are not deduplicated: this package builds
// inventories, and a duplicate key is dropped by the callers that care (see
// keySet) rather than by pretending YAML's override rules were applied here.
func (w *walk) entries(n *yaml.Node) []entry {
	n = w.deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out []entry
	var merged []entry
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !w.spend() {
			return append(out, merged...)
		}
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			continue
		}
		if k.Value == "<<" {
			for _, m := range w.mergeSources(v) {
				merged = append(merged, w.entries(m)...)
			}
			continue
		}
		out = append(out, entry{key: k.Value, line: k.Line, val: v})
	}
	return append(out, merged...)
}

// mergeSources resolves the right-hand side of a merge key, which YAML allows to
// be either one mapping or a sequence of them.
func (w *walk) mergeSources(v *yaml.Node) []*yaml.Node {
	v = w.deref(v)
	if v == nil {
		return nil
	}
	if v.Kind == yaml.SequenceNode {
		out := make([]*yaml.Node, 0, len(v.Content))
		for _, c := range v.Content {
			if d := w.deref(c); d != nil && d.Kind == yaml.MappingNode {
				out = append(out, d)
			}
		}
		return out
	}
	if v.Kind == yaml.MappingNode {
		return []*yaml.Node{v}
	}
	return nil
}

// field returns the value node for one key of a mapping, or nil.
func (w *walk) field(n *yaml.Node, key string) *yaml.Node {
	for _, e := range w.entries(n) {
		if e.key == key {
			return w.deref(e.val)
		}
	}
	return nil
}

// path walks a chain of mapping keys — spec.template.spec — and returns nil the
// moment one is missing, which is how an unexpected shape stops being a panic.
func (w *walk) path(n *yaml.Node, keys ...string) *yaml.Node {
	cur := n
	for _, k := range keys {
		cur = w.field(cur, k)
		if cur == nil {
			return nil
		}
	}
	return cur
}

// items returns a sequence's elements, dereferenced. A scalar or mapping is not
// a sequence and returns nothing; the callers that accept both shapes check
// Kind themselves.
func (w *walk) items(n *yaml.Node) []*yaml.Node {
	n = w.deref(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		if !w.spend() {
			break
		}
		if d := w.deref(c); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// text returns a scalar's text, or "" for anything else.
//
// It reads Value directly and never a tag-resolved Go value: this is the point
// where the parser would otherwise hand back a decoded credential, and there is
// nowhere in this package's types to put one anyway.
func text(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// str is text plus this package's redaction and bounds, for anything that will
// be printed.
func (w *walk) str(n *yaml.Node) string { return safe(text(n)) }

// num reads a scalar as a PORT NUMBER, reporting whether it was one. YAML
// numbers arrive as text; a port written as "8080" and one written as 8080 are
// the same port and must report the same way.
//
// Zero is refused along with everything outside 1..65535. Nothing listens on
// port 0, so a manifest that says so is either using it as "unset" or is
// malformed — and either way a Port carrying nothing but zeroes is a row that
// claims a service exposes something it does not. FuzzParse found exactly that
// from a compose file reading `ports: ["0"]`.
func num(n *yaml.Node) (int, bool) {
	s := strings.TrimSpace(text(n))
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 65535 {
		return 0, false
	}
	return v, true
}

func boolOf(n *yaml.Node) bool { return strings.EqualFold(text(n), "true") }

// documents decodes every document in a YAML stream.
//
// It returns what it managed to read PLUS the problems it hit, rather than one
// or the other. A three-document bundle whose second document is malformed still
// declares two workloads, and reporting nothing for it would hide them; reporting
// them without the problem would hide the one that is broken.
func documents(src []byte) (docs []*yaml.Node, problems []Problem) {
	defer func() {
		if r := recover(); r != nil {
			// See the note at the top of this file. The panic value can quote
			// document text, so it goes through the redactor like any other
			// parser message.
			problems = append(problems, Problem{
				Reason: ReasonPanic,
				Detail: safe(fmt.Sprintf("yaml parser panicked: %v", r)),
			})
		}
	}()

	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	for i := 0; ; i++ {
		if i >= maxDocs {
			problems = append(problems, Problem{
				Reason: ReasonLimit,
				Detail: fmt.Sprintf("stopped after %d documents", maxDocs),
			})
			return docs, problems
		}
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, problems
		}
		if err != nil {
			problems = append(problems, Problem{
				Reason: ReasonMalformedYAML,
				// Clamped to the file: the line is read out of a message rather
				// than reported structurally, and a citation pointing past the
				// end of the file sends a reader somewhere that does not exist.
				Line: clampLine(yamlErrLine(err), src),
				// REDACTED: yaml.v3's messages quote the text they choked on,
				// and in these files that text can be a credential.
				Detail: safe(err.Error()),
			})
			// The decoder does not resynchronise after a syntax error; a second
			// Decode returns the same failure forever.
			return docs, problems
		}
		if doc.Kind == yaml.DocumentNode {
			if len(doc.Content) == 0 {
				continue // `---` with nothing under it
			}
			docs = append(docs, doc.Content[0])
			continue
		}
		docs = append(docs, &doc)
	}
}

func clampLine(line int, src []byte) int {
	if line < 1 || line > strings.Count(string(src), "\n")+1 {
		return 0
	}
	return line
}

// yamlErrLine pulls the line number out of a yaml.TypeError or a syntax error
// message, so a Problem can cite a location. Zero when there is none — a cited
// line that does not exist is worse than no line.
func yamlErrLine(err error) int {
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		return parseLinePrefix(te.Errors[0])
	}
	return parseLinePrefix(err.Error())
}

func parseLinePrefix(msg string) int {
	const marker = "line "
	i := strings.Index(msg, marker)
	if i < 0 {
		return 0
	}
	rest := msg[i+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
