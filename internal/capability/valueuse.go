package capability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// A function used as a value is invisible to a call graph.
//
// `.then(handleResponse)`, `register("x", handler)`, a table of callbacks and a
// method reference are all uses of the function and none is a call, so no edge
// records them. The answer an index gives for such a function is silently
// short: koa's `respond` is reached by its whole test suite through a function
// passed by name, and the index reported one caller, no test and certainty high.
//
// The graph cannot see this. The text can, because the identifier is still
// there. The dead-code report already uses that evidence (its "name appears
// elsewhere" reason); this applies it to the questions about callers, impact
// and tests. It is a SIGNAL OF DOUBT and never a finding: a name that occurs
// outside its definition and its recorded references MAY be passed as a value,
// or may be an unrelated symbol that shares the name. The answer says "may".

// TextReader is the read surface the check needs. Like BindingReader it is not
// part of Registry: a registry that does not offer it yields an empty ValueUse,
// which every consumer reads as "not measured" and never as "nothing found".
type TextReader interface {
	UnrecordedMentions(ctx context.Context, runID int64, name string, defs []registry.DefSpan) (registry.UnrecordedMentions, error)
	IncomingEdgeCounts(ctx context.Context, runID int64, ids []int64) (map[int64]int, error)
}

var _ TextReader = (*registry.Registry)(nil)

const (
	// MaxPathChecks bounds how many symbols ON THE PATH are text-checked besides
	// the target. The check reads the stored text of up to a dozen files per
	// name, so it is bounded by a small fixed number and not by the size of the
	// answer: on a hub with a thousand callers it costs the same as on a leaf.
	MaxPathChecks = 4

	// fewCallers is how many recorded incoming relationships a path symbol may
	// have and still be checked. A symbol with many callers is not one whose
	// only route in is a value.
	fewCallers = 2

	// maxPathCandidates is how many reached symbols are looked at to find the
	// ones with few callers, strongest path first.
	maxPathCandidates = 200

	// maxNameDefs is how many definitions of a name are read to learn where the
	// name is defined.
	maxNameDefs = 500

	// TextBudget is the time the whole check may take. Each name costs a read of
	// up to a dozen files from the full-text index, which measured about 90ms a
	// name when those files are 2,000 lines long (tests/scale), so five names are
	// near half a second. A check that has used its budget stops and says how
	// many names it reached: an answer that is late is worth less than one that
	// says it looked at fewer. It is the user's latency, so a slow test machine
	// is answered in the test, which accepts a check that stopped on time.
	TextBudget = 600 * time.Millisecond
)

// NameUse is the finding for one symbol.
type NameUse struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`

	// OnPath is false for the target itself and true for a symbol the answer
	// reaches the target through.
	OnPath bool `json:"on_path,omitempty"`

	// Places is how many places hold the name outside every definition of it and
	// every reference the index recorded. A lower bound when LowerBound.
	Places     int  `json:"places"`
	LowerBound bool `json:"count_is_lower_bound,omitempty"`

	// Sites are the first few of them.
	Sites []registry.Place `json:"sites,omitempty"`
}

// ValueUse is what the text check found for one answer.
type ValueUse struct {
	// Checked is how many symbols' text was checked, the target included.
	Checked int `json:"checked"`

	// PathCandidates is how many symbols on the path had few enough callers to
	// be worth checking; Checked can be smaller than 1 + PathCandidates when
	// the bound was reached.
	PathCandidates int `json:"path_candidates,omitempty"`

	// OutOfTime is true when the check stopped at TextBudget with names left.
	OutOfTime bool `json:"out_of_time,omitempty"`

	// Uses are the symbols whose name occurs elsewhere, the target first.
	Uses []NameUse `json:"uses,omitempty"`
}

// Any reports whether a name was found outside its recorded references.
func (v ValueUse) Any() bool { return len(v.Uses) > 0 }

// OnTarget is the finding for the target itself, if there is one.
func (v ValueUse) OnTarget() (NameUse, bool) {
	for _, u := range v.Uses {
		if !u.OnPath {
			return u, true
		}
	}
	return NameUse{}, false
}

// Places is the sum over every symbol with a finding.
func (v ValueUse) Places() int {
	n := 0
	for _, u := range v.Uses {
		n += u.Places
	}
	return n
}

func placesWord(u NameUse) string {
	if u.LowerBound {
		return fmt.Sprintf("at least %d other place(s)", u.Places)
	}
	return fmt.Sprintf("%d other place(s)", u.Places)
}

// Lead is the clause a first line opens with. Empty when nothing was found.
//
// The wording is fixed because an agent reads it and a test holds it: the name,
// how many places, that they are not recorded as calls, and the consequence.
func (v ValueUse) Lead() string {
	var parts []string
	if u, ok := v.OnTarget(); ok {
		parts = append(parts, fmt.Sprintf("%s: its name appears in %s that are not recorded as calls: "+
			"it may be passed as a value", u.Name, placesWord(u)))
	}
	var path []NameUse
	for _, u := range v.Uses {
		if u.OnPath {
			path = append(path, u)
		}
	}
	switch len(path) {
	case 0:
	case 1:
		parts = append(parts, fmt.Sprintf("%s, on the path to it, is named in %s not recorded as calls "+
			"(it may be passed as a value), so what reaches %s is not listed",
			path[0].Name, placesWord(path[0]), path[0].Name))
	default:
		names := make([]string, 0, 3)
		for i, u := range path {
			if i == 3 {
				break
			}
			names = append(names, u.Name)
		}
		more := ""
		if len(path) > 3 {
			more = fmt.Sprintf(" and %d more", len(path)-3)
		}
		parts = append(parts, fmt.Sprintf("%d symbols on the path to it (%s%s) are named in %d other place(s) not "+
			"recorded as calls (they may be passed as values), so what reaches them is not listed",
			len(path), strings.Join(names, ", "), more, sumPlaces(path)))
	}
	return strings.Join(parts, "; ")
}

func sumPlaces(us []NameUse) int {
	n := 0
	for _, u := range us {
		n += u.Places
	}
	return n
}

// Detail is the full-length account, with the sites, for a note.
func (v ValueUse) Detail() string {
	if !v.Any() {
		return ""
	}
	var b strings.Builder
	for _, u := range v.Uses {
		who := "the target"
		if u.OnPath {
			who = fmt.Sprintf("%s (%s:%d), which the answer reaches the target through,", u.Name, u.Path, u.Line)
		}
		fmt.Fprintf(&b, "The name of %s occurs in %s that are neither a definition of it nor a reference the index "+
			"recorded", who, placesWord(u))
		if len(u.Sites) > 0 {
			sites := make([]string, 0, len(u.Sites))
			for _, s := range u.Sites {
				sites = append(sites, fmt.Sprintf("%s:%d", s.Path, s.Line))
			}
			fmt.Fprintf(&b, " (for example %s)", strings.Join(sites, ", "))
		}
		b.WriteString(". A function passed as a value, registered by name or named in a table is used without being " +
			"called, and leaves no call edge; this may be one. Search for the name before concluding that nothing reaches it. ")
	}
	fmt.Fprintf(&b, "Checked %d symbol(s) for this", v.Checked)
	if v.OutOfTime {
		b.WriteString(", and stopped at its time budget")
	}
	if v.PathCandidates > v.Checked-1 && v.Checked > 0 {
		fmt.Fprintf(&b, "; %d other symbol(s) on the path have few callers and were not checked", v.PathCandidates-(v.Checked-1))
	}
	b.WriteString(".")
	return b.String()
}

// callable reports whether a kind can be passed around as a value.
func callable(kind string) bool { return kind == "function" || kind == "method" }

// constructorLike reports whether a qualified name ends in the same word twice,
// `LoggerConfiguration.LoggerConfiguration`: a constructor, whose name is the
// name of its type and so occurs wherever the type is written. It is never
// passed as a value, and counting its mentions would count every use of the type.
func constructorLike(fqn string) bool {
	rest := fqn
	if i := strings.Index(fqn, ":"); i >= 0 {
		rest = fqn[i+1:]
	}
	segs := strings.Split(rest, ".")
	n := len(segs)
	return n >= 2 && segs[n-1] == segs[n-2]
}

// ValueUse checks tgt, and the few symbols on its path that have few recorded
// callers, for a name that occurs in code the index does not record as a
// reference. reached is the symbols the answer walked through, strongest path
// first.
//
// Bounded: at most 1 + MaxPathChecks names are looked up in the text index,
// whatever the size of reached.
func (d Deps) ValueUse(ctx context.Context, runID int64, tgt Target, idx *SymbolIndex, reached []int64) (ValueUse, error) {
	reader, ok := d.Registry.(TextReader)
	if !ok || tgt.SymbolID == 0 {
		return ValueUse{}, nil
	}
	var out ValueUse
	done := map[string]bool{}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, TextBudget)
	defer cancel()
	// expired is whether err is this check's own deadline and not the caller's
	// cancellation: the first returns what was found, the second is an error.
	expired := func(err error) bool {
		if parent.Err() != nil {
			return false
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return true
		}
		if err != nil && (strings.Contains(err.Error(), "interrupted") || strings.Contains(err.Error(), "context deadline exceeded")) {
			return true
		}
		return false
	}

	check := func(id int64, name, path string, line int, onPath bool) error {
		done[name] = true
		defs, err := d.Registry.FindSymbolsByName(ctx, runID, name, "", maxNameDefs)
		if err != nil {
			return fmt.Errorf("capability: definitions of %q: %w", name, err)
		}
		spans := make([]registry.DefSpan, 0, len(defs))
		for _, h := range defs {
			// A function spelled like a type is a constructor or a return type
			// read as a definition. The name occurs wherever the type is written,
			// and counting those would count every use of the type.
			if h.Kind == "class" || h.Kind == "interface" || h.Kind == "type_alias" {
				return nil
			}
			spans = append(spans, registry.DefSpan{Path: h.FilePath, From: h.StartLine, To: max(h.EndLine, h.StartLine)})
			if h.ID == id && line == 0 {
				line = h.StartLine
			}
		}
		m, err := reader.UnrecordedMentions(ctx, runID, name, spans)
		if err != nil {
			return err
		}
		out.Checked++
		if !m.Known || m.Total == 0 {
			return nil
		}
		out.Uses = append(out.Uses, NameUse{
			SymbolID: id, Name: name, Path: path, Line: line, OnPath: onPath,
			Places: m.Total, LowerBound: !m.Complete, Sites: m.Sample,
		})
		return nil
	}

	if callable(tgt.Kind) && !constructorLike(tgt.FQN) {
		if err := check(tgt.SymbolID, tgt.Name, tgt.FilePath, tgt.Line, false); err != nil {
			if expired(err) {
				out.OutOfTime = true
				return out, nil
			}
			return out, err
		}
	}

	// The path symbols with few callers, strongest path first. Tests are left
	// out: a test is the end of a path, and its not being called is its normal
	// state.
	var cands []Symbol
	if idx == nil {
		reached = nil
	}
	for _, id := range reached {
		if len(cands) >= maxPathCandidates {
			break
		}
		sym, ok := idx.Lookup(id)
		if !ok || id == tgt.SymbolID || !callable(sym.Kind) || constructorLike(sym.FQN) ||
			IsTestSymbol(sym.Kind, sym.Name, sym.FilePath) {
			continue
		}
		cands = append(cands, sym)
	}
	if len(cands) == 0 {
		return out, nil
	}
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.ID
	}
	counts, err := reader.IncomingEdgeCounts(ctx, runID, ids)
	if err != nil {
		if expired(err) {
			out.OutOfTime = true
			return out, nil
		}
		return out, err
	}
	checked := 0
	for _, c := range cands {
		if counts[c.ID] > fewCallers {
			continue
		}
		// Several definitions share a name and the text check is by name, so one
		// look serves them all.
		if done[c.Name] {
			continue
		}
		out.PathCandidates++
		if checked >= MaxPathChecks {
			continue
		}
		checked++
		if err := check(c.ID, c.Name, c.FilePath, 0, true); err != nil {
			if expired(err) {
				out.OutOfTime = true
				return out, nil
			}
			return out, err
		}
		if err := ctx.Err(); err != nil {
			if expired(err) {
				out.OutOfTime = true
				return out, nil
			}
			return out, err
		}
	}
	return out, nil
}
