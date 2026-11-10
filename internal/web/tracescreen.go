package web

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/runtime"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/internal/trace"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Runtime correlation: what actually RAN, against what could.
//
// # The one question static analysis cannot answer
//
// Every other screen in this portal describes what the code CAN do. An endpoint
// the index knows and no span ever reached is either dead or untested, and both
// are worth knowing before a refactor — but nothing in a graph can tell you
// which endpoints ran. Only a trace can, and only for the window it covers.
//
// # An exported file, never a live collector
//
// A service that must be running for RepoCortex to work is a service RepoCortex has
// no business requiring, and the export is the artefact CI already produces. So
// this reads a file inside the repository, named by ?export= or defaulting to
// the conventional name, and parses it with internal/trace.
//
// # Spans are not persisted, and this screen is not a cache
//
// Runtime data ages out in hours while an index describes a commit. Storing
// them together would produce a graph whose edges are true at different times.
// The correlation is recomputed per request from the file, and the file's own
// window is what every "never reached" claim below is scoped to.
//
// # Every match says how it was made
//
// runtime.Observed carries its MatchKind: an explicit code.function attribute
// named the symbol outright, an http.route matching an endpoint is strong, and
// a span name matching a symbol is weak and only tried when nothing better
// exists. That field is the difference between a correlation and a guess, and
// it travels to the client on every row.

// maxTraceExportBytes bounds the export this screen will read.
//
// Smaller than internal/trace's own 64 MB ceiling on purpose: that bound
// protects the indexer, and this one protects a synchronous HTTP handler that a
// reader is watching a spinner for.
const maxTraceExportBytes = 16 << 20

// traceExportFile is the conventional name looked for when ?export= is absent.
const traceExportFile = "cortex.trace.json"

// maxTraceRows bounds each listing on this screen.
const maxTraceRows = 200

// TraceCoverage is what the index could have seen run, against what the export
// showed running.
//
// This block is why an empty trace screen is not an empty screen. "No endpoint
// was exercised" and "this index knows of no endpoints at all" are opposite
// facts and they produce the same empty list.
type TraceCoverage struct {
	// IndexedEndpoints is how many routes this index extracted — the
	// denominator of every coldness claim on this screen.
	IndexedEndpoints int `json:"indexed_endpoints"`

	// IndexedSymbols is how many symbols a span could have been correlated to
	// at all.
	IndexedSymbols int `json:"indexed_symbols"`

	// Exercised, ColdEndpoints and Unmatched are the export's side.
	Exercised     int `json:"exercised_entities"`
	ColdEndpoints int `json:"cold_endpoints"`
	Unmatched     int `json:"unmatched_operations"`

	// Detail is the sentence that stops "cold" being read as "dead".
	Detail string `json:"detail"`
}

// TraceResponse is the whole runtime-correlation screen.
type TraceResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Export is the file this screen tried to read, echoed whether or not it
	// was found.
	Export FileRef `json:"export"`

	// Correlated is false when no export was read. Every list below is then
	// empty for that reason and no other.
	Correlated bool `json:"correlated"`

	// Format is the export format internal/trace detected: otlp-json | jaeger.
	Format string `json:"format,omitempty"`

	Spans    int      `json:"spans"`
	Traces   int      `json:"traces"`
	Services []string `json:"services"`

	// Exercised are indexed entities a span reached, each with the MatchKind
	// that tied it to code.
	Exercised []runtime.Observed `json:"exercised"`

	// ColdEndpoints are endpoints the index knows and no span reached — the
	// most actionable output here, and the one most easily misread.
	ColdEndpoints []runtime.Observed `json:"cold_endpoints"`

	// Unmatched are span operation names that matched nothing indexed. A large
	// number means the correlation is weak, not that the code is absent.
	Unmatched []string `json:"unmatched"`

	Coverage TraceCoverage `json:"coverage"`

	// Caveats are the standing limits of the method, printed every time.
	Caveats []string `json:"caveats"`

	Help *Guidance `json:"guidance,omitempty"`

	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := TraceResponse{
		Meta: meta, Legend: Legend(),
		Exercised: []runtime.Observed{}, ColdEndpoints: []runtime.Observed{},
		Unmatched: []string{}, Services: []string{},
		Caveats: traceCaveats(),
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label:   "Index this repository first",
			Detail:  "A trace is correlated AGAINST an index. Without one there is nothing to correlate to.",
			Command: "cortex index",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	// The index's own side is filled in first, so a screen with no export still
	// reports what could have been observed.
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp.Coverage = traceCoverage(idx)

	resp.Export = s.repoFile(r, "export", traceExportFile)
	var om Omissions

	src, readErr := readRepoFile(&resp.Export, maxTraceExportBytes)
	if readErr != nil {
		resp.Help = traceMissingGuidance(resp.Export, resp.Coverage)
		resp.Next = traceNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	spans, format, err := trace.Parse(src, maxTraceExportBytes)
	if err != nil {
		resp.Export.Found = true
		resp.Export.Problem = err.Error()
		resp.Help = &Guidance{
			Title:  fmt.Sprintf("%s is not a trace export this portal can read", resp.Export.Path),
			Detail: err.Error() + " Two formats are understood: OpenTelemetry OTLP/JSON and Jaeger UI JSON.",
			Steps: []string{
				"Export from your collector as OTLP/JSON, or save a trace from the Jaeger UI.",
				"cortex trace " + resp.Export.Path + " — the same correlation from the command line",
			},
		}
		resp.Next = traceNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Format = string(format)
	if len(spans) == 0 {
		resp.Help = &Guidance{
			Title: fmt.Sprintf("%s parsed as %s and contains no spans", resp.Export.Path, format),
			Detail: "The file is a valid export of an empty window. Nothing ran during it, which is " +
				"a fact about the window rather than about the code.",
			Steps: []string{"Export a window in which the service actually served traffic."},
		}
		resp.Next = traceNextAction(resp)
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	rep, err := runtime.Correlate(ctx, s.deps.Registry, run.ID, spans)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: correlate %s: %w", resp.Export.Path, err))
		return
	}

	resp.Correlated = true
	resp.Spans, resp.Traces = rep.Spans, rep.Traces
	if rep.Services != nil {
		resp.Services = rep.Services
	}
	resp.Exercised = capped(rep.Exercised, maxTraceRows, &om, ReasonRowBudget,
		fmt.Sprintf("Exercised entities beyond the first %d are not listed. coverage.exercised_entities is the full figure.", maxTraceRows),
		"cortex trace "+resp.Export.Path)
	resp.ColdEndpoints = capped(rep.ColdEndpoints, maxTraceRows, &om, ReasonRowBudget,
		fmt.Sprintf("Cold endpoints beyond the first %d are not listed. coverage.cold_endpoints is the full figure.", maxTraceRows),
		"cortex trace "+resp.Export.Path)
	if rep.Unmatched != nil {
		resp.Unmatched = rep.Unmatched
	}

	resp.Coverage.Exercised = len(rep.Exercised)
	resp.Coverage.ColdEndpoints = len(rep.ColdEndpoints)
	resp.Coverage.Unmatched = len(rep.Unmatched)
	resp.Coverage.Detail = fmt.Sprintf(
		"%d of this index's %d endpoint(s) were never reached in THIS export. Cold here means cold "+
			"in this window only: an endpoint absent from one trace window is not dead code, and no "+
			"trace can prove absence.", len(rep.ColdEndpoints), resp.Coverage.IndexedEndpoints)

	if len(rep.Unmatched) > 0 {
		om.Hide(ReasonUnresolvedReferences, len(rep.Unmatched),
			"Span operation(s) matched nothing indexed. That means the correlation is weak here, "+
				"not that the code is absent: most services do not set OpenTelemetry's code.* "+
				"attributes, and without them a span name is all there is to match on.",
			"instrument with code.function / code.namespace attributes")
	}

	resp.Next = traceNextAction(resp)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// readRepoFile opens a resolved FileRef and records why it could not be read.
//
// It mutates the ref rather than returning a second error type, so a caller
// that forgets to check the error still ships a response saying what happened.
func readRepoFile(ref *FileRef, maxBytes int64) ([]byte, error) {
	if ref.Problem != "" {
		return nil, fmt.Errorf("web: %s", ref.Problem)
	}
	info, err := os.Stat(ref.Absolute) //nolint:gosec // G703: Absolute was contained under RepoRoot by repoFile
	if err != nil {
		ref.Problem = fmt.Sprintf("%s is not there.", ref.Path)
		return nil, fmt.Errorf("web: stat %s: %w", ref.Path, err)
	}
	if info.IsDir() {
		ref.Problem = fmt.Sprintf("%s is a directory, not a file.", ref.Path)
		return nil, fmt.Errorf("web: %s is a directory", ref.Path)
	}
	if info.Size() > maxBytes {
		ref.Problem = fmt.Sprintf("%s is %d bytes; this screen reads at most %d.",
			ref.Path, info.Size(), maxBytes)
		return nil, fmt.Errorf("web: %s exceeds %d bytes", ref.Path, maxBytes)
	}
	src, _, err := security.ReadFileContained(ref.root, ref.Path, security.WithMaxBytes(maxBytes))
	if err != nil {
		ref.Problem = fmt.Sprintf("%s could not be read: %v", ref.Path, err)
		return nil, fmt.Errorf("web: read %s: %w", ref.Path, err)
	}
	ref.Found = true
	return src, nil
}

// traceCoverage reports what the INDEX could have seen run.
func traceCoverage(idx *structuralIndex) TraceCoverage {
	c := TraceCoverage{IndexedSymbols: len(idx.syms)}
	for _, sym := range idx.syms {
		if strings.EqualFold(string(sym.Kind), string(model.KindEndpoint)) {
			c.IndexedEndpoints++
		}
	}
	c.Detail = fmt.Sprintf(
		"This index holds %d endpoint(s) and %d symbol(s) a span could have been correlated to. "+
			"Nothing below is a claim about code this index does not know.",
		c.IndexedEndpoints, c.IndexedSymbols)
	return c
}

func traceMissingGuidance(ref FileRef, cov TraceCoverage) *Guidance {
	title := fmt.Sprintf("No trace export at %s", ref.Path)
	if ref.Defaulted {
		title = fmt.Sprintf("No trace export has been placed at %s", ref.Path)
	}
	detail := fmt.Sprintf(
		"This screen answers what RAN, and only a trace knows that. Nothing has been exported into "+
			"this repository yet, so the %d endpoint(s) this index knows are neither hot nor cold "+
			"here — they are UNOBSERVED, which is a third thing and the reason this screen is empty "+
			"rather than showing them all as dead. %s",
		cov.IndexedEndpoints, ref.Problem)
	if ref.Defaulted {
		detail += " " + ref.DefaultedFrom
	}
	return &Guidance{
		Title:  title,
		Detail: detail,
		Steps: []string{
			"Export a window from your collector as OTLP/JSON, or save a trace from the Jaeger UI.",
			"Save it in the repository as " + traceExportFile + ", or pass ?export=<repo-relative path>",
			"cortex trace <export.json> — the same correlation from the command line",
		},
	}
}

// traceCaveats are the standing limits of the method, printed every time.
func traceCaveats() []string {
	return []string{
		"Correlation is inferred from names and every row says how it was made. code-attribute named the " +
			"symbol outright; route matched an extracted endpoint; name matched a span name to a " +
			"symbol and is weak — a span called \"handle\" says nothing about which of nine handlers ran.",
		"Cold means cold IN THIS EXPORT. An endpoint absent from one trace window is not dead code, " +
			"and no trace can prove absence.",
		"Spans are not persisted. Runtime data ages out in hours while an index describes a commit, " +
			"and storing them together would produce a graph whose edges are true at different times.",
		"This is a window, not a census. A sampled tracing setup shows a fraction of what ran, and " +
			"nothing in the export says which fraction.",
	}
}

func traceNextAction(resp TraceResponse) NextAction {
	switch {
	case !resp.Correlated && resp.Coverage.IndexedEndpoints == 0:
		return NextAction{
			Label: "This index extracted no endpoints, so there is nothing to correlate against",
			Detail: "A trace would still match symbols by name, but the route half of the " +
				"correlation — the strong half — needs extracted routes.",
			Screen: "overview#blind-spots",
		}
	case !resp.Correlated:
		return NextAction{
			Label:   "Export a trace window and drop it in the repository",
			Detail:  fmt.Sprintf("%d endpoint(s) here are currently unobserved rather than cold.", resp.Coverage.IndexedEndpoints),
			Command: "cortex trace " + traceExportFile,
		}
	case resp.Coverage.ColdEndpoints > 0:
		return NextAction{
			Label: fmt.Sprintf("%d endpoint(s) were never reached in this window", resp.Coverage.ColdEndpoints),
			Detail: "Check the window before concluding anything: each is either dead, untested, or " +
				"simply not exercised during the export. Open the blast radius before deleting one.",
			Screen: "blast",
		}
	case resp.Coverage.Unmatched > resp.Coverage.Exercised:
		return NextAction{
			Label:  fmt.Sprintf("%d span operation(s) matched nothing indexed", resp.Coverage.Unmatched),
			Detail: "More spans matched nothing than matched something, so this correlation is weak. Instrumenting code.function attributes fixes it at the source.",
			Screen: "overview",
		}
	default:
		return NextAction{
			Label:  "Read every row beside its match kind",
			Detail: "A route match and a span-name match are not the same evidence, and only one of them survives a rename.",
			Screen: "crossstack",
		}
	}
}
