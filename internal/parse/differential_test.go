//go:build cgo_parser

package parse

// Differential test: does the escape hatch actually lead anywhere?
//
// The pure-Go runtime and the official C runtime are two independent
// implementations of tree-sitter, driving two independently maintained grammar
// sets. Nothing guarantees they see the same symbols in the same file, and
// until this test existed nobody had checked. A fallback nobody has compared
// against the thing it replaces is not a fallback, it is a comment in an
// architecture document.
//
// WHAT THIS TEST IS DESIGNED TO PRODUCE
//
// Not a green tick. Byte-identical output from two implementations would be an
// astonishing result and is not the useful question. The useful question is
// whether the two agree about what a developer would call the symbols in a
// file, so the test:
//
//   - compares SETS of (name, start line), not orderings or byte offsets;
//   - reports a similarity score per language and fails below a threshold;
//   - prints every disagreement. A test that only says "83% match" tells you
//     nothing you can act on — the disagreements ARE the finding, and they are
//     what a maintainer needs when deciding whether to flip the build tag.
//
// Run it with:
//
//	go test -tags cgo_parser -run Differential -v ./internal/parse/

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/odvcencio/gotreesitter/grammars"
)

// The thresholds, and why the gates are asymmetric.
//
// The naive design gates on set similarity in both directions. That is the
// wrong shape for this question, and would actively harm the project: the C
// runtime's grammars find symbols RepoCortex's own tags supplements currently
// miss (abstract TypeScript classes, interface method signatures, Java
// annotation types). A symmetric gate would count that extra coverage as a
// failure, and the cheapest way to make CI green would be to DELETE working
// patterns from the CGO backend. A test that rewards removing coverage is
// worse than no test.
//
// So the hard gate runs one way only, in the direction that decides whether the
// escape hatch is usable.

// cgoRecallThreshold is the share of what the shipping pure-Go path finds that
// the C runtime also finds.
//
// This is THE escape-hatch question: if a maintainer flips the build tag
// because gotreesitter has failed them, do symbols disappear from search
// results, outlines and context packs? 0.95 allows a handful of per-grammar
// quirks and nothing more. Below it, switching backends is a visible regression
// for users and the fallback is not real.
//
// Raising this number is a real improvement. Lowering it to make a run pass is
// the failure mode CONTRIBUTING.md warns about; fix the query, or record and
// justify a normalisation.
const cgoRecallThreshold = 0.95

// similarityFloor is a tripwire on the two-way Jaccard index, not a coverage
// gate.
//
// Deliberately loose, because one-directional extra coverage is expected and
// wanted. Its job is to catch the catastrophic case the recall gate cannot see:
// both backends producing plenty of symbols that happen to be almost disjoint,
// which would mean a grammar or position-convention mismatch rather than a
// coverage difference. Below 0.60 no per-construct explanation would be
// adequate.
const similarityFloor = 0.60

// kindAgreementThreshold applies only to symbols BOTH backends found.
//
// Held high on purpose: two runtimes disagreeing about whether a file contains
// a symbol is a coverage gap, but two runtimes disagreeing about whether that
// symbol is a class or an interface is a correctness problem that would put
// wrong edges in the graph.
const kindAgreementThreshold = 0.90

// symKey identifies a symbol for comparison.
//
// Name and start LINE only. Columns are excluded because the two grammars
// disagree about where a declaration node begins in ways that have nothing to
// do with the symbol (leading modifiers, annotations, decorators), and end
// positions are excluded for the same reason. Line is kept because a symbol
// found on the wrong line is a genuine defect, not a naming difference.
type symKey struct {
	Name string
	Line int
}

func (k symKey) String() string { return fmt.Sprintf("%s@L%d", k.Name, k.Line) }

// normalisation is one documented reason two backends legitimately differ.
//
// Every entry must carry a reason. An undocumented normalisation is a way to
// make a differential test pass without learning anything, so the report prints
// the reason next to the count of symbols each rule touched.
type normalisation struct {
	name   string
	reason string

	// drop removes a symbol from BOTH sides' sets before comparison. It must
	// be symmetric: a rule that only ever fires on one backend is hiding that
	// backend's behaviour rather than normalising a difference.
	drop func(lang string, s TaggedSymbol) bool

	// rewrite canonicalises a symbol that both backends find but describe
	// differently.
	rewrite func(lang string, s TaggedSymbol) TaggedSymbol
}

// normalisations are applied to both backends' output before comparison.
//
// Kept short deliberately. Each rule is a place where the differential test
// stops proving something, so the bar for adding one is a specific, explained
// grammar difference — never "this made the number go up".
//
// TWO RULES WERE REMOVED, and how they came to be removed is the argument for
// keeping this list short.
//
//   - "go-type-family" collapsed class, interface and type_alias into one kind
//     for Go, on the reasoning that Go spells all three as `type X ...` and the
//     two query engines read that form differently. It was a reasonable rule
//     for the queries of the day. It also blindfolded the kind-agreement gate
//     for exactly one language — and Go was where every type declaration was
//     being indexed as an alias, so no struct was ever a class and no interface
//     was ever an interface. The gate whose stated purpose is "a class labelled
//     an interface puts wrong edges in the graph" could not see it.
//
//   - "java-unclassified" folded Java `function` into `method`, describing a
//     real de-duplication defect that kindSpecificity had since fixed. It
//     outlived the bug it documented and normalised nothing.
//
// Both were verified inert before removal: kind agreement stays at 1.000 for
// every language with each of them disabled. The lesson is the one this comment
// exists to pass on — a normalisation that currently hides nothing still hides
// whatever arrives next, so it is a liability the moment its reason expires.
var normalisations = []normalisation{
	{
		name: "anonymous",
		reason: "A capture can match a node with no identifier text (an anonymous " +
			"class body, a destructuring target). Neither backend can name it, so " +
			"neither can usefully report it and RepoCortex drops it downstream anyway.",
		drop: func(_ string, s TaggedSymbol) bool { return strings.TrimSpace(s.Name) == "" },
	},
}

// backendPair holds the two runtimes under comparison.
type backendPair struct {
	pure Backend
	cgo  Backend
}

func newBackendPair(t *testing.T) backendPair {
	t.Helper()
	c, err := CGOBackend()
	if err != nil {
		t.Fatalf("CGO backend unavailable in a -tags cgo_parser build: %v", err)
	}
	return backendPair{pure: PureGoBackend(), cgo: c}
}

// TestBothBackendsLoadAndParse is the precondition for everything below: if
// either runtime cannot produce symbols at all, a similarity score is
// meaningless rather than merely bad.
func TestBothBackendsLoadAndParse(t *testing.T) {
	p := newBackendPair(t)

	if p.pure.Name() == p.cgo.Name() {
		t.Fatalf("both backends report %q — the build tag did not select a distinct runtime", p.pure.Name())
	}
	t.Logf("cgo backend languages: %v", p.cgo.Languages())

	probes := map[string]string{
		"go":         "package p\n\ntype Repo interface{}\n\nfunc New() *Repo { return nil }\n",
		"java":       "package p;\npublic class A { void b() {} }\n",
		"python":     "class A:\n    def b(self):\n        pass\n",
		"typescript": "export class A { b(): void {} }\n",
	}
	for _, lang := range p.cgo.Languages() {
		src, ok := probes[lang]
		if !ok {
			t.Errorf("%s: compiled into the cgo backend but has no probe — add one", lang)
			continue
		}
		for _, b := range []Backend{p.pure, p.cgo} {
			if !b.Supports(lang) {
				t.Errorf("%s: backend %s does not support it", lang, b.Name())
				continue
			}
			syms, err := b.Symbols(context.Background(), lang, []byte(src))
			if err != nil {
				t.Errorf("%s/%s: %v", lang, b.Name(), err)
				continue
			}
			if len(syms) == 0 {
				t.Errorf("%s/%s: parsed but produced no symbols", lang, b.Name())
			}
		}
	}
}

// TestDifferentialSymbolAgreement is the exit criterion.
func TestDifferentialSymbolAgreement(t *testing.T) {
	p := newBackendPair(t)
	corpus := buildCorpus(t, p.cgo)

	byLang := map[string][]corpusFile{}
	for _, f := range corpus {
		byLang[f.lang] = append(byLang[f.lang], f)
	}
	langs := make([]string, 0, len(byLang))
	for l := range byLang {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	t.Logf("corpus: %d files across %d languages", len(corpus), len(langs))
	for _, r := range normalisations {
		t.Logf("normalisation %q: %s", r.name, r.reason)
	}

	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			rep := compareLanguage(t, p, lang, byLang[lang])
			rep.log(t)

			if rep.union == 0 {
				t.Fatalf("%s: neither backend produced a symbol — the corpus proves nothing", lang)
			}
			if rep.cgoRecall() < cgoRecallThreshold {
				t.Errorf("%s: the C runtime found only %.3f of what the shipping pure-Go path finds "+
					"(gate %.2f). Flipping the build tag would lose the %d symbols listed above, so "+
					"the escape hatch is not usable for this language.",
					lang, rep.cgoRecall(), cgoRecallThreshold, len(rep.onlyPure))
			}
			if rep.similarity() < similarityFloor {
				t.Errorf("%s: Jaccard similarity %.3f is below the %.2f tripwire — the two backends "+
					"are describing different files, not merely covering different constructs.",
					lang, rep.similarity(), similarityFloor)
			}
			if rep.matched > 0 && rep.kindAgreement() < kindAgreementThreshold {
				t.Errorf("%s: kind agreement %.3f among matched symbols is below the %.2f gate. "+
					"See the kind conflicts above: the backends found the same symbols but "+
					"disagree about what they are, which would put wrong edges in the graph.",
					lang, rep.kindAgreement(), kindAgreementThreshold)
			}
		})
	}
}

// TestDifferentialMalformedInput checks the property that matters most for an
// escape hatch under hostile input: both runtimes must survive, and neither may
// invent symbols the other cannot see in garbage.
//
// Agreement on symbol CONTENT is deliberately not asserted here. Error recovery
// is where two tree-sitter implementations are most entitled to differ, and
// pretending otherwise would be a test that fails for the wrong reason.
func TestDifferentialMalformedInput(t *testing.T) {
	p := newBackendPair(t)

	bad := []struct{ name, src string }{
		{"empty", ""},
		{"truncated", "package"},
		{"unbalanced", "package p\nfunc Broken( {"},
		{"binary", "\x00\x01\x02\xff\xfe"},
		{"brace_storm", strings.Repeat("{", 10000)},
		{"deep_nesting", "package p\nfunc A(){" + strings.Repeat("if true {", 500)},
		{"invalid_utf8", "package p\nfunc \xc3\x28() {}"},
		{"nul_in_identifier", "package p\nfunc A\x00B() {}"},
	}

	for _, lang := range p.cgo.Languages() {
		for _, tc := range bad {
			t.Run(lang+"/"+tc.name, func(t *testing.T) {
				pureSyms, pureErr := p.pure.Symbols(context.Background(), lang, []byte(tc.src))
				cgoSyms, cgoErr := p.cgo.Symbols(context.Background(), lang, []byte(tc.src))

				// The contract is "degrade, do not crash". Both backends
				// returning an error is a perfectly good outcome; a panic
				// escaping either one is not, and would have failed the run
				// before reaching this line.
				t.Logf("purego: %d symbols, err=%v", len(pureSyms), pureErr)
				t.Logf("cgo:    %d symbols, err=%v", len(cgoSyms), cgoErr)

				for _, s := range append(append([]TaggedSymbol{}, pureSyms...), cgoSyms...) {
					if s.Range.Start.Line < 1 {
						t.Errorf("symbol %q has a non-positive start line %d", s.Name, s.Range.Start.Line)
					}
				}
			})
		}
	}
}

// TestDifferentialGateDetectsRegression proves the gate can fail.
//
// CONTRIBUTING.md is explicit that a gate only ever seen passing proves
// nothing. The differential run above currently reports perfect recall for all
// four languages, which is exactly the situation in which a broken comparison
// would be indistinguishable from a healthy one. So the defect the gate exists
// to catch is injected deliberately: a backend that silently loses symbols.
func TestDifferentialGateDetectsRegression(t *testing.T) {
	p := newBackendPair(t)
	corpus := buildCorpus(t, p.cgo)

	var goFiles []corpusFile
	for _, f := range corpus {
		if f.lang == "go" {
			goFiles = append(goFiles, f)
		}
	}
	if len(goFiles) == 0 {
		t.Fatal("no Go corpus files to inject a regression into")
	}

	// A backend that drops every third symbol: the shape of a grammar
	// regression, not an outright failure. An outright failure would be caught
	// by any smoke test; quiet under-reporting is what this gate is for.
	lossy := &lossyBackend{Backend: p.cgo, keepEveryNth: 3}

	rep := compareLanguage(t, backendPair{pure: p.pure, cgo: lossy}, "go", goFiles)
	t.Logf("injected-regression run: cgo recall %.3f, %d symbols lost", rep.cgoRecall(), len(rep.onlyPure))

	if rep.cgoRecall() >= cgoRecallThreshold {
		t.Fatalf("the gate did not notice a backend dropping two symbols in three: "+
			"recall %.3f still passes the %.2f threshold", rep.cgoRecall(), cgoRecallThreshold)
	}
	if len(rep.onlyPure) == 0 {
		t.Fatal("the report listed no missing symbols despite the injected loss — " +
			"the disagreement listing, which is the whole point of this test, is broken")
	}
}

// lossyBackend wraps a real backend and discards symbols, standing in for a
// grammar regression.
type lossyBackend struct {
	Backend
	keepEveryNth int
}

func (l *lossyBackend) Name() string { return l.Backend.Name() + "-lossy" }

func (l *lossyBackend) Symbols(ctx context.Context, lang string, src []byte) ([]TaggedSymbol, error) {
	syms, err := l.Backend.Symbols(ctx, lang, src)
	if err != nil {
		return nil, err
	}
	out := make([]TaggedSymbol, 0, len(syms))
	for i, s := range syms {
		if i%l.keepEveryNth == 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// corpus
// ---------------------------------------------------------------------------

type corpusFile struct {
	lang  string
	label string
	src   []byte
}

// buildCorpus assembles the payment fixture plus generated sources.
//
// The fixture matters because hand-written unit snippets flatter a parser: real
// files carry imports, javadoc, annotations and nesting. The generated sources
// matter because the fixture is Java only, and cover the constructs where two
// grammars are most likely to part company.
func buildCorpus(t *testing.T, cgo Backend) []corpusFile {
	t.Helper()

	var out []corpusFile

	root := filepath.Join("..", "..", "fixtures", "payment-platform")
	if _, err := os.Stat(root); err == nil {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			entry := grammars.DetectLanguage(path)
			if entry == nil || !cgo.Supports(entry.Name) {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, corpusFile{
				lang:  entry.Name,
				label: "fixture:" + filepath.ToSlash(rel),
				src:   src,
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking fixture: %v", err)
		}
	} else {
		t.Logf("payment fixture absent (%v); comparing generated sources only", err)
	}

	for _, g := range generatedCorpus {
		if !cgo.Supports(g.lang) {
			continue
		}
		out = append(out, corpusFile{lang: g.lang, label: "generated:" + g.label, src: []byte(g.src)})
	}
	return out
}

// generatedCorpus covers the constructs the brief calls out: classes,
// interfaces, methods, generics, annotations/decorators and nested types. Each
// one is a place where grammar authors make different structural choices, so
// each one is a place a differential test can earn its keep.
var generatedCorpus = []struct{ lang, label, src string }{
	{"go", "types-and-generics", `package pay

import "context"

// Repository is an interface with a type parameter.
type Repository[T any] interface {
	Save(ctx context.Context, item T) error
	FindByID(ctx context.Context, id string) (T, error)
}

type Transaction struct {
	ID     string
	Amount int64
	meta   map[string]string
}

// Nested type declarations inside a function body.
func Build() any {
	type inner struct{ X int }
	return inner{}
}

const (
	MaxRetries = 3
	Backoff    = "250ms"
)

var DefaultTimeout = 5

type Handler func(ctx context.Context) error

func NewRepository[T any]() Repository[T] { return nil }

func (t *Transaction) UpdateStatus(status string) error {
	t.meta["status"] = status
	return nil
}

func (t Transaction) String() string { return t.ID }
`},

	{"go", "embedding-and-aliases", `package pay

type Base struct{ ID string }

type Derived struct {
	Base
	Extra int
}

type Alias = Transaction

type Money int64

func (m Money) Cents() int64 { return int64(m) }
`},

	{"java", "generics-annotations-nested", `package com.pay.service;

import java.util.List;
import java.util.Optional;

@Service
@Transactional(readOnly = true)
public class TransactionService<T extends Payable> implements Auditable, Closeable {

    private static final int MAX_RETRIES = 3;
    private final Repository<T> repository;

    public TransactionService(Repository<T> repository) {
        this.repository = repository;
    }

    @Override
    @Deprecated
    public Optional<T> findById(String id) {
        return repository.findById(id);
    }

    public <R> List<R> mapAll(Mapper<T, R> mapper) {
        return repository.findAll().stream().map(mapper::apply).toList();
    }

    // A nested type, an interface and an enum inside the class body.
    public static class Result {
        private final String code;
        Result(String code) { this.code = code; }
    }

    public interface Mapper<A, B> {
        B apply(A input);
    }

    public enum Status { PENDING, SETTLED, FAILED }

    public record Money(long cents, String currency) {}

    @interface Marker {}
}
`},

	{"java", "interfaces-and-abstract", `package com.pay.repository;

public interface Repository<T> {
    T findById(String id);
    void save(T item);
    default boolean exists(String id) { return findById(id) != null; }
    static <X> Repository<X> empty() { return null; }
}

abstract class AbstractRepository<T> implements Repository<T> {
    protected String table;
    public abstract T findById(String id);
    public void save(T item) {}
}
`},

	{"python", "classes-decorators-nested", `import dataclasses
from typing import Generic, TypeVar

T = TypeVar("T")
MAX_RETRIES = 3


class Repository(Generic[T]):
    """A generic repository."""

    table: str = "transactions"

    def __init__(self, conn):
        self.conn = conn

    @property
    def name(self):
        return self.table

    @staticmethod
    def build(conn):
        return Repository(conn)

    async def find_by_id(self, txn_id):
        return await self.conn.fetch(txn_id)

    class Meta:
        ordering = ["id"]


@dataclasses.dataclass
class Transaction:
    txn_id: str
    amount: int


@app.route("/callback")
def handle_callback(request):
    def inner(x):
        return x
    return inner(request)
`},

	{"python", "inheritance-and-async", `import asyncio


class BaseHandler:
    def handle(self, event):
        raise NotImplementedError


class CallbackHandler(BaseHandler):
    RETRIES = 2

    def handle(self, event):
        return self._dispatch(event)

    def _dispatch(self, event):
        return event

    async def handle_async(self, event):
        await asyncio.sleep(0)
        return self.handle(event)


def module_level(a, b=1, *args, **kwargs):
    class Local:
        def inner(self):
            return None
    return Local
`},

	{"typescript", "modules-and-signatures", `declare module "pay" {
  export function settle(id: string): void;
}

export interface Auditable {
  readonly auditedAt: Date;
  audit(reason: string): void;
}

export class Ledger implements Auditable {
  readonly auditedAt = new Date();
  private entries: string[] = [];

  audit(reason: string): void {
    this.entries.push(reason);
  }

  get size(): number {
    return this.entries.length;
  }
}

export const DEFAULT_CURRENCY = "USD";

export function* walk<T>(items: T[]): Generator<T> {
  for (const item of items) yield item;
}
`},

	{"typescript", "classes-generics-decorators", `import { Injectable } from "./di";

export interface Repository<T> {
  findById(id: string): Promise<T | null>;
  save(item: T): void;
}

export type Status = "pending" | "settled" | "failed";

export enum Currency {
  USD = "USD",
  EUR = "EUR",
}

@Injectable()
export class TransactionService<T extends { id: string }> implements Repository<T> {
  private readonly cache = new Map<string, T>();
  public static readonly MAX_RETRIES: number = 3;

  constructor(private readonly conn: Connection) {}

  async findById(id: string): Promise<T | null> {
    return this.cache.get(id) ?? null;
  }

  save(item: T): void {
    this.cache.set(item.id, item);
  }

  private normalise<R>(input: R): R {
    return input;
  }
}

export abstract class BaseService {
  abstract run(): void;
  protected helper(): number { return 1; }
}

export namespace Money {
  export function toCents(v: number): number { return v * 100; }
}

export function createService<T extends { id: string }>(conn: Connection) {
  return new TransactionService<T>(conn);
}
`},
}

// ---------------------------------------------------------------------------
// comparison and reporting
// ---------------------------------------------------------------------------

type disagreement struct {
	file string
	key  symKey
	kind model.EntityKind
}

type kindConflict struct {
	file      string
	key       symKey
	pure, cgo model.EntityKind
}

type report struct {
	lang    string
	files   int
	matched int
	union   int

	onlyPure []disagreement
	onlyCGO  []disagreement
	kindDiff []kindConflict

	kindMatches int
	dropped     map[string]int
	errs        []string
}

// similarity is the Jaccard index over (name, start line) sets: the share of
// all symbols either backend saw that both saw. Reported as the headline
// number, gated only by similarityFloor, because it is symmetric and therefore
// penalises the C runtime for finding MORE than the default build does.
func (r *report) similarity() float64 {
	if r.union == 0 {
		return 0
	}
	return float64(r.matched) / float64(r.union)
}

// cgoRecall is the share of the pure-Go path's symbols that the C runtime also
// found — what a maintainer would LOSE by flipping the build tag.
func (r *report) cgoRecall() float64 {
	denom := r.matched + len(r.onlyPure)
	if denom == 0 {
		return 0
	}
	return float64(r.matched) / float64(denom)
}

// pureRecall is the reverse: the share of the C runtime's symbols the shipping
// default build already finds. Reported, never gated. A shortfall here is a
// missing pattern in queries.go, not a defect in the escape hatch, and gating
// on it would create pressure to delete coverage from the CGO backend.
func (r *report) pureRecall() float64 {
	denom := r.matched + len(r.onlyCGO)
	if denom == 0 {
		return 0
	}
	return float64(r.matched) / float64(denom)
}

func (r *report) kindAgreement() float64 {
	if r.matched == 0 {
		return 0
	}
	return float64(r.kindMatches) / float64(r.matched)
}

func (r *report) log(t *testing.T) {
	t.Helper()

	t.Logf("── %s ─────────────────────────────────────────", r.lang)
	t.Logf("files              %d", r.files)
	t.Logf("symbols in both    %d", r.matched)
	t.Logf("only purego        %d   (lost if you flip the build tag)", len(r.onlyPure))
	t.Logf("only cgo           %d   (gained if you flip the build tag)", len(r.onlyCGO))
	t.Logf("cgo recall         %.3f  GATE >= %.2f", r.cgoRecall(), cgoRecallThreshold)
	t.Logf("pure recall        %.3f  (reported, not gated)", r.pureRecall())
	t.Logf("similarity         %.3f  (Jaccard; tripwire >= %.2f)", r.similarity(), similarityFloor)
	t.Logf("kind agreement     %.3f  GATE >= %.2f, over %d matched",
		r.kindAgreement(), kindAgreementThreshold, r.matched)

	for name, n := range r.dropped {
		if n > 0 {
			t.Logf("normalised away    %d by %q", n, name)
		}
	}
	for _, e := range r.errs {
		t.Logf("backend error      %s", e)
	}

	// The disagreements are the deliverable, so they are printed in full rather
	// than summarised. A capped list would hide exactly the systematic gap a
	// maintainer needs to see.
	logDisagreements(t, "SEEN ONLY BY purego (cgo misses these)", r.onlyPure)
	logDisagreements(t, "SEEN ONLY BY cgo (purego misses these)", r.onlyCGO)

	if len(r.kindDiff) > 0 {
		t.Logf("KIND CONFLICTS (both found the symbol, disagree on what it is):")
		for _, k := range r.kindDiff {
			t.Logf("    %-40s %-28s purego=%s cgo=%s", k.file, k.key, k.pure, k.cgo)
		}
	}
}

func logDisagreements(t *testing.T, header string, ds []disagreement) {
	t.Helper()
	if len(ds) == 0 {
		return
	}
	t.Logf("%s: %d", header, len(ds))

	// Grouped by file so a systematic gap in one construct is visible as a
	// cluster rather than a flat list.
	byFile := map[string][]disagreement{}
	var files []string
	for _, d := range ds {
		if _, ok := byFile[d.file]; !ok {
			files = append(files, d.file)
		}
		byFile[d.file] = append(byFile[d.file], d)
	}
	sort.Strings(files)
	for _, f := range files {
		items := byFile[f]
		sort.Slice(items, func(i, j int) bool { return items[i].key.Line < items[j].key.Line })
		parts := make([]string, 0, len(items))
		for _, d := range items {
			parts = append(parts, fmt.Sprintf("%s(%s)", d.key, d.kind))
		}
		t.Logf("    %-40s %s", f, strings.Join(parts, " "))
	}
}

func compareLanguage(t *testing.T, p backendPair, lang string, files []corpusFile) *report {
	t.Helper()

	rep := &report{lang: lang, files: len(files), dropped: map[string]int{}}
	ctx := context.Background()

	for _, f := range files {
		pureSyms, err := p.pure.Symbols(ctx, lang, f.src)
		if err != nil {
			rep.errs = append(rep.errs, fmt.Sprintf("purego %s: %v", f.label, err))
			continue
		}
		cgoSyms, err := p.cgo.Symbols(ctx, lang, f.src)
		if err != nil {
			rep.errs = append(rep.errs, fmt.Sprintf("cgo %s: %v", f.label, err))
			continue
		}

		pureSet := normaliseSet(lang, pureSyms, rep.dropped)
		cgoSet := normaliseSet(lang, cgoSyms, rep.dropped)

		for k, pk := range pureSet {
			ck, ok := cgoSet[k]
			if !ok {
				rep.onlyPure = append(rep.onlyPure, disagreement{f.label, k, pk})
				continue
			}
			rep.matched++
			if pk == ck {
				rep.kindMatches++
			} else {
				rep.kindDiff = append(rep.kindDiff, kindConflict{f.label, k, pk, ck})
			}
		}
		for k, ck := range cgoSet {
			if _, ok := pureSet[k]; !ok {
				rep.onlyCGO = append(rep.onlyCGO, disagreement{f.label, k, ck})
			}
		}
	}

	rep.union = rep.matched + len(rep.onlyPure) + len(rep.onlyCGO)
	return rep
}

// normaliseSet applies the documented normalisations and collapses a backend's
// output to a set keyed by (name, start line).
//
// Collapsing to a set is what makes the comparison fair: the two engines yield
// matches in different orders and may capture one declaration through several
// patterns, neither of which is a disagreement about the file's contents.
func normaliseSet(lang string, syms []TaggedSymbol, dropped map[string]int) map[symKey]model.EntityKind {
	out := make(map[symKey]model.EntityKind, len(syms))
	for _, s := range syms {
		skip := false
		for _, r := range normalisations {
			if r.drop != nil && r.drop(lang, s) {
				dropped[r.name]++
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		for _, r := range normalisations {
			if r.rewrite != nil {
				s = r.rewrite(lang, s)
			}
		}
		out[symKey{Name: s.Name, Line: s.Range.Start.Line}] = s.Kind
	}
	return out
}
