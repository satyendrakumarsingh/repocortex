package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The shared machinery behind the two TASK screens — the compiled contract and
// the verify report.
//
// # Why this file exists at all
//
// Both screens are views over packages that already answer their question:
// internal/compile turns an intent plus a context pack into a Contract, and
// internal/verify marks a change against one. Neither knows about SQLite, and
// both take their sources through small ports declared at the consumer
// (compile.Ports, verify.ArchitectureSource). This file is the ADAPTER between
// those ports and the read surfaces the portal already holds — nothing more.
// The same three adapters exist in internal/cli for the two commands; they are
// six lines each and deliberately duplicated rather than exported from the CLI,
// because a portal that imported the command layer to reach a closure would
// have inverted the arrow CLAUDE.md §1 is about.
//
// # What is NOT duplicated
//
// No scope selection, no risk rule, no disposition, no standing. Every
// judgement stays in compile and verify, where it is tested, and this package
// renders it. The moment this file starts deciding whether a finding is a fact
// there are two answers to that question and CI is gating on the wrong one.

// ---------------------------------------------------------------- ports

// PackReader is the read surface internal/context needs to assemble a context
// pack.
//
// Declared here, at the consumer, and structurally identical to devctx.Registry
// — restated rather than aliased so this package's dependency is visible in its
// own vocabulary, and so a change on either side shows up as a compile failure
// at the assignment below rather than as a silently widened port.
//
// It is a SUPERSET of BulkReader (Files and Symbols appear in both) and is kept
// separate anyway: BulkReader is the whole-run scan the graph screens pay for
// once per run, and folding full-text search and file excerpting into it would
// make that cost look like it belonged to the graph.
type PackReader interface {
	CatalogueFingerprint(ctx context.Context, runID int64) (registry.Fingerprint, error)
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	SearchFiles(ctx context.Context, query string, limit int) ([]registry.FileMatch, error)
	FileExcerpt(ctx context.Context, runID int64, path string, startLine, lines int) (string, error)
}

// Compile-time proof the production adapter satisfies the port, and that the
// port is exactly what internal/context asks for.
var (
	_ PackReader            = (*registry.Registry)(nil)
	_ devctx.Registry       = PackReader(nil)
	_ devctx.Graph          = capability.Graph(nil)
	_ architecture.Registry = (*registry.Registry)(nil)
)

// packReader resolves the port for this request.
//
// Deps.Packs is the wired answer. The fallback exists because the port was
// added after the four original screens: a caller built before it — a test, an
// embedder — passes only Registry, and the production Registry already
// satisfies PackReader. Asserting is honest here precisely because the failure
// is reported rather than papered over: a nil result produces guidance naming
// the missing wiring, not an empty contract that reads like a thin repository.
func (s *Server) packReader() PackReader {
	if s.deps.Packs != nil {
		return s.deps.Packs
	}
	if pr, ok := s.deps.Registry.(PackReader); ok {
		return pr
	}
	return nil
}

// ---------------------------------------------------------------- adapters

// archView adapts the declared architecture to compile's port.
//
// arch.Model is pure declarative data with no I/O, so this is a projection of
// configuration and not a query.
type archView struct {
	resolver *arch.Resolver
	model    arch.Model
}

func (v archView) ComponentOf(path string) string { return v.resolver.ComponentOf(path) }
func (v archView) Rules() []arch.Rule             { return v.model.Rules }

// archSource adapts capability/architecture to verify's port: the POST-change
// check, run against the index as it currently stands.
type archSource struct {
	reg   architecture.Registry
	model arch.Model
}

func (a archSource) Check(ctx context.Context, runID int64, min model.Confidence) (architecture.Report, error) {
	return architecture.Check(ctx, a.reg, runID, a.model, min)
}

// impactView adapts capability/impact and capability/tests to compile's port.
//
// Mirrors internal/cli's adapter exactly, including the two decisions that
// matter: an unresolvable target is an UNRESOLVED Reach rather than an error
// (one unindexed scope symbol must not stop a whole contract compiling), and a
// test reached like a caller is MARKED rather than dropped (leaving it unmarked
// makes verify report every covering test as a caller the change forgot).
type impactView struct {
	deps  capability.Deps
	runID int64
}

func (v impactView) Reach(ctx context.Context, target string) (compile.Reach, error) {
	rep, err := impact.Analyze(ctx, v.deps, v.runID, target)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		if errors.Is(err, capability.ErrTargetNotFound) || errors.As(err, &amb) {
			return compile.Reach{Resolved: false}, nil
		}
		return compile.Reach{}, err
	}

	r := compile.Reach{
		Resolved:  true,
		Target:    rep.Target.Name,
		Floor:     string(rep.ConfidenceFloor),
		Hidden:    rep.LowConfidenceHidden,
		Truncated: rep.Truncated,
	}
	seen := make(map[string]struct{})
	add := func(im impact.Impacted, evidence string) {
		key := im.FilePath + "#" + im.Name
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		r.Callers = append(r.Callers, compile.Reached{
			Symbol:     im.Name,
			Path:       im.FilePath,
			Kind:       im.Kind,
			Depth:      im.Depth,
			Confidence: string(im.Confidence),
			IsTest:     capability.IsTestSymbol(im.Kind, im.Name, im.FilePath),
			Evidence:   evidence,
		})
	}
	// Strongest claim first, so the dedup above keeps the stronger one.
	for _, group := range [][]impact.Impacted{rep.Direct, rep.Indirect} {
		for _, im := range group {
			add(im, firstEvidence(im))
		}
	}
	for _, d := range rep.Dispatch {
		// A dispatch reach HAS no call site — that is the whole claim — so it
		// carries the analysis's own sentence rather than an empty file:line
		// that would read as an ordinary caller nobody managed to locate.
		add(d.Impacted, d.Detail)
	}

	tr, err := tests.FindTests(ctx, v.deps, v.runID, target)
	if err != nil {
		var amb *capability.AmbiguousTargetError
		if errors.Is(err, capability.ErrTargetNotFound) || errors.As(err, &amb) {
			return r, nil
		}
		return compile.Reach{}, err
	}
	for _, t := range tr.Tests {
		r.Tests = append(r.Tests, compile.TestRef{
			Name:       t.Name,
			Path:       t.FilePath,
			Signal:     string(t.Strongest),
			Confidence: string(t.Confidence),
		})
	}
	return r, nil
}

func firstEvidence(im impact.Impacted) string {
	if len(im.Evidence) == 0 {
		return ""
	}
	e := im.Evidence[0]
	return fmt.Sprintf("%s:%d", e.FilePath, e.Range.Start.Line)
}

// compilePorts builds the ports a contract is compiled against.
func (s *Server) compilePorts(runID int64) compile.Ports {
	m := s.deps.Config.Architecture
	return compile.Ports{
		Architecture: archView{resolver: arch.NewResolver(m), model: m},
		Impact:       impactView{deps: s.deps.Caps, runID: runID},
	}
}

// ---------------------------------------------------------------- contract

// maxIntentLength bounds the request a query string may carry.
//
// Not a security control — the CSP and the loopback guard are — but a bound on
// what gets echoed back into a rendered page and into a token estimate. An
// intent longer than this is not a task, it is a paste.
const maxIntentLength = 2000

// maxChangedPaths bounds the change set a verify request may name.
const maxChangedPaths = 500

// intentParam reads and bounds the task text.
func intentParam(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("intent"))
	if raw == "" {
		return "", false
	}
	if len(raw) > maxIntentLength {
		raw = raw[:maxIntentLength]
	}
	return raw, true
}

// buildContract assembles a pack for intent and compiles it into a contract.
//
// The pack is built by internal/context and the contract by internal/compile;
// this function chooses neither the scope nor the ranking. It exists so the two
// screens that need a contract — the brief itself, and the report that marks a
// change against it — cannot end up compiling two different contracts for the
// same words.
func (s *Server) buildContract(ctx context.Context, runID int64, intent string, maxScope int) (compile.Contract, devctx.Pack, error) {
	pr := s.packReader()
	if pr == nil {
		return compile.Contract{}, devctx.Pack{}, errNoPackReader
	}
	pack, err := s.assembler.Assemble(ctx, pr, s.deps.Caps.Graph, runID, devctx.Request{
		Task:   intent,
		Budget: devctx.BudgetFrom(s.deps.Config),
	})
	if err != nil {
		return compile.Contract{}, devctx.Pack{}, fmt.Errorf("web: assemble context for %q: %w", intent, err)
	}
	contract, err := compile.CompileWith(ctx, intent, pack,
		compile.Options{MaxScope: maxScope}, s.compilePorts(runID))
	if err != nil {
		return compile.Contract{}, pack, fmt.Errorf("web: compile contract: %w", err)
	}
	return contract, pack, nil
}

// errNoPackReader is returned when the context-pack port was never wired.
//
// Its own error, and reported as guidance rather than as a 500: the server is
// healthy and the index is fine — the operator's build did not connect the one
// port these two screens need, and telling them that is more useful than a
// stack trace.
var errNoPackReader = errors.New("web: no context-pack reader is wired; " +
	"the contract and verify screens read internal/context, which needs the full registry")
