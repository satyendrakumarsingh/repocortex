package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/deps"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// DepMind: what this project declares it depends on, whether anything imports
// it, and what RepoCortex can actually do for each language in the tree.
//
// # Manifests, not lock files
//
// registry.DependencyReport reads dependency symbols, which the analysis layer
// extracts from go.mod, package.json, pom.xml, build.gradle, requirements.txt,
// pyproject.toml and Cargo.toml. A lock file is the resolved transitive
// closure — hundreds of packages no line of this repository mentions — and
// "your lock file contains golang.org/x/sys" is not a sentence anyone acts on.
//
// # NO VULNERABILITY DATA
//
// Versions are reported accurately so a real scanner can consume them. An
// embedded advisory list would be stale the day it was built, and inventing
// vulnerability information is exactly the overclaim this product exists to
// avoid.
//
// # An unused dependency is a PROMPT, not a verdict
//
// A package can be loaded by reflection, required by a build plugin, or
// imported under a name that differs from its distribution name. The response
// says so in the row's own caveat rather than in a footnote a client can drop.
//
// # Why the language tiers are on this screen
//
// `repocortex languages` reports what the parser can do per language: typed,
// resolved or parsed. That is the same question DepMind answers about packages,
// asked about the toolchain — "how much of this can RepoCortex actually see" —
// and it is what makes an empty dependency answer interpretable. A repository
// whose main language sits at `parsed` binds no cross-file reference at all, so
// the importer counts below are zero for a reason that has nothing to do with
// its manifests. The tiers come from parse.LanguagesInTier at run time, not
// from a table, so this screen and the CLI cannot disagree.

// DependencyReader is the port /api/deps needs for the declared side.
//
// Declared at the consumer and kept out of the composed Registry for the same
// ISP reason ConfigReader is; see that type's comment.
type DependencyReader interface {
	DependencyReport(ctx context.Context, runID int64, matches registry.ImportMatcher) ([]registry.DependencyUsage, error)
}

// ReasonImportsNotEnumerable is why the reverse direction is unanswerable.
//
// Its own reason rather than a row budget, and the difference is not cosmetic:
// a row budget is a bound this handler chose and can raise, while this is a
// question the read port cannot answer at all. Labelling it `row_budget` would
// have told a reader the rows exist and were shortened. It carries no entry in
// OmissionLabel's table, so it renders through the documented fallback — the
// underscores become spaces — which is why the key is written as a phrase.
const ReasonImportsNotEnumerable = "import_paths_not_enumerable"

// ImportEnumerator is the port the REVERSE question needs: every import path
// the run recorded, so a path matching no declared dependency can be named.
//
// The index has always held the answer — internal/acquire records every import
// of every file — and for a while nothing exported a read method for it, so
// this port was declared against a method that did not exist. That was
// deliberate: leaving it undeclared would have made the gap invisible, while
// declaring it let the response say precisely what was missing. The method now
// exists (registry.DistinctImports) and the assertion below is what turned the
// screen on.
//
// It returns paths and file counts, NOT the mismatch. Deciding whether
// `github.com/go-git/go-git/v5/plumbing` uses the module
// `github.com/go-git/go-git/v5` is ecosystem knowledge, and the registry must
// not acquire it (see registry.ImportMatcher). So the intersection happens
// here, in the delivery adapter that already supplies the matcher.
type ImportEnumerator interface {
	DistinctImports(ctx context.Context, runID int64) (map[string]int, error)
}

// Compile-time proof the production adapter satisfies both sides of the join.
var (
	_ DependencyReader = (*registry.Registry)(nil)
	_ ImportEnumerator = (*registry.Registry)(nil)
)

// dependencyReader resolves the port for this request. See packReader.
func (s *Server) dependencyReader() DependencyReader {
	if s.deps.Dependencies != nil {
		return s.deps.Dependencies
	}
	if dr, ok := s.deps.Registry.(DependencyReader); ok {
		return dr
	}
	return nil
}

// importMatcher adapts the analysis layer's ecosystem rules to the port the
// registry declares.
//
// Identical to internal/cli's, and imported at the same layer for the same
// reason: knowing that `github.com/go-git/go-git/v5/plumbing` uses the module
// `github.com/go-git/go-git/v5`, or that a Rust crate spells its hyphens as
// underscores, is ecosystem knowledge. It belongs in internal/analyze/deps, and
// the registry takes it as a parameter precisely so that a delivery adapter —
// the CLI or this portal — is the one that supplies it. Passing nil instead
// would leave Importers at zero on every row, which registry.DependencyUsage
// documents as "not measured" and this screen would have had to report as
// "nothing imports this": the exact false claim the port exists to prevent.
func importMatcher(name, ecosystem, importPath string) bool {
	return deps.Matches(deps.Dependency{
		Name:      name,
		Ecosystem: deps.Ecosystem(ecosystem),
	}, importPath)
}

// DependencyRow is one declared dependency.
type DependencyRow struct {
	SymbolID int64 `json:"symbol_id,omitempty"`

	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Ecosystem string `json:"ecosystem,omitempty"`
	Scope     string `json:"scope,omitempty"`

	// Indirect is a dependency the manifest itself marks transitive. Reporting
	// one as unused would restate a fact the manifest already makes, so those
	// are excluded from the unused list and still carried in the inventory.
	Indirect bool `json:"indirect"`

	// Importers is how many indexed files import it. Measured, never assumed:
	// an ImportMatcher was supplied, which is what makes zero mean "no importer
	// found" rather than "not measured".
	Importers int  `json:"importers"`
	Measured  bool `json:"importers_measured"`

	// Evidence is the manifest line that declares it.
	Evidence Evidence `json:"evidence"`

	// Caveat is present on an unused row and says why it is a prompt.
	Caveat string `json:"caveat,omitempty"`
}

// LanguagePresence is one language as it appears in THIS repository.
type LanguagePresence struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
	Symbols  int    `json:"symbols"`
}

// LanguageTier is one parser tier, and what sits in it.
type LanguageTier struct {
	// Tier is typed | resolved | parsed.
	Tier string `json:"tier"`

	// WhatYouGet is parse.Tier.Description, verbatim, so this screen and
	// `repocortex languages` cannot phrase the same capability two ways.
	WhatYouGet string `json:"what_you_get"`

	// Supported is every language the shipping binary places at this tier.
	Supported []string `json:"supported_languages"`
	Count     int      `json:"supported_count"`

	// Present is the subset actually found in this repository, with its weight.
	// The pair is the point: a tier with 30 supported languages and none in the
	// tree tells the reader nothing, and a `parsed` tier holding this
	// repository's main language tells them why the graph is thin.
	Present      []LanguagePresence `json:"present_in_this_repository"`
	FilesHere    int                `json:"files_in_this_repository"`
	SymbolsHere  int                `json:"symbols_in_this_repository"`
	SymbolShare  float64            `json:"symbol_share_pct"`
	PresentCount int                `json:"present_count"`
}

// DepsCounts is the headline arithmetic.
type DepsCounts struct {
	Declared int `json:"declared"`
	Direct   int `json:"declared_direct"`
	Indirect int `json:"declared_indirect"`

	// NotImported counts DIRECT declarations with no importer. Indirect ones
	// are excluded — see DependencyRow.Indirect.
	NotImported int `json:"declared_but_not_imported"`
	Imported    int `json:"declared_and_imported"`

	// ImportedNotDeclared is the reverse direction: distinct import paths no
	// declared dependency matches. It is -1, never 0, when the subtraction could
	// not be performed — a 0 would assert that every import matches a
	// declaration, which nothing would have established.
	//
	// It is the FULL count and not the length of the rows served beside it, so
	// a row budget can shorten the table without moving the headline.
	ImportedNotDeclared int `json:"imported_but_not_declared"`
}

// ReverseDirection is the "imported but never declared" answer, or the honest
// statement that it could not be computed.
//
// A struct rather than a bare list, because an empty list is a CLAIM — that
// every import path in this repository matches something a manifest declares —
// and a portal that could not perform the subtraction has not checked it.
// Available is the boolean a client gates on. Detail carries the caveat when
// the answer IS available and the reason when it is not; a client that renders
// only the rows renders the least readable half of this block.
type ReverseDirection struct {
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
	Remedy    string `json:"remedy,omitempty"`

	// Rows is populated only when Available. Always an array, never null.
	Rows []ImportRow `json:"rows"`
}

// ImportRow is one import path that matches no declared dependency.
type ImportRow struct {
	Path  string `json:"import_path"`
	Files int    `json:"files_importing"`
}

// DepsResponse is the whole DepMind screen.
type DepsResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// The mismatch first, then the inventory — the order `repocortex deps` uses,
	// because the inventory is the boring half.
	NotImported []DependencyRow `json:"declared_but_not_imported"`
	Declared    []DependencyRow `json:"declared"`

	ImportedNotDeclared ReverseDirection `json:"imported_but_not_declared"`

	LanguageTiers []LanguageTier `json:"language_tiers"`

	Counts DepsCounts `json:"counts"`

	// VulnerabilityPolicy is stated in the payload so a client cannot imply
	// this screen is a security scan.
	VulnerabilityPolicy string `json:"vulnerability_policy"`

	// Coverage names the manifests that are read.
	Coverage string `json:"coverage_limit"`

	Help *Guidance  `json:"guidance,omitempty"`
	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

const (
	depsVulnerabilityPolicy = "NO VULNERABILITY DATA is reported. Versions are published accurately so a " +
		"real scanner can consume them; an embedded advisory list would be stale the day it was built."

	depsCoverageLimit = "Manifests, not lock files: go.mod, package.json, pom.xml, build.gradle, " +
		"requirements.txt, pyproject.toml, Cargo.toml. A lock file is the resolved transitive closure " +
		"and says nothing about what this repository's authors declared."

	unusedDependencyCaveat = "A prompt, not a verdict: a package can be loaded by reflection, required " +
		"by a build plugin, or imported under a name that differs from its distribution name."
)

func (s *Server) handleDeps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := DepsResponse{
		Meta: meta, Legend: Legend(),
		NotImported: []DependencyRow{}, Declared: []DependencyRow{},
		LanguageTiers:       []LanguageTier{},
		VulnerabilityPolicy: depsVulnerabilityPolicy, Coverage: depsCoverageLimit,
		Counts: DepsCounts{ImportedNotDeclared: -1},
	}
	if !ok {
		resp.Help = meta.Help
		resp.ImportedNotDeclared = reverseUnavailable(reverseNoRun)
		resp.LanguageTiers = languageTiers(nil)
		resp.Next = NextAction{
			Label: "Index this repository", Command: "repocortex index",
			Detail: "Dependencies are extracted from manifests during indexing; there is nothing committed to read.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	var om Omissions

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	resp.LanguageTiers = languageTiers(idx)

	reader := s.dependencyReader()
	if reader == nil {
		// The reverse direction dies with the declared side: "imported but never
		// declared" is a subtraction, and without the set being subtracted the
		// remainder is every import in the repository.
		resp.ImportedNotDeclared = reverseUnavailable(reverseNoDeclaredSet)
		om.Note(ReasonImportsNotEnumerable, resp.ImportedNotDeclared.Detail,
			resp.ImportedNotDeclared.Remedy)
		resp.Help = &Guidance{
			Title: "This portal was started without a dependency read port",
			Detail: "The registry it was given does not implement DependencyReport, so the declared side " +
				"of this screen has no source. That is missing wiring, not a project without " +
				"dependencies — the language tiers below are still real.",
			Steps: []string{
				"repocortex ui — the shipped portal wires the full registry",
				"repocortex deps --all — the same report in the terminal",
			},
		}
		resp.Next = NextAction{
			Label: "Read it in the terminal instead", Command: "repocortex deps --all",
			Detail: "Same source, no portal wiring required.",
		}
		resp.Omissions = om
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	list, err := reader.DependencyReport(ctx, run.ID, importMatcher)
	if err != nil {
		s.fail(w, r, fmt.Errorf("web: dependency report for run %d: %w", run.ID, err))
		return
	}

	rows := make([]DependencyRow, 0, len(list))
	var unused []DependencyRow
	for _, d := range list {
		row := DependencyRow{
			SymbolID: d.SymbolID, Name: d.Name, Version: d.Version,
			Ecosystem: d.Ecosystem, Scope: d.Scope, Indirect: d.Indirect,
			Importers: d.Importers, Measured: true,
			Evidence: Evidence{FilePath: d.Manifest, Line: d.Line},
		}
		switch {
		case d.Indirect:
			resp.Counts.Indirect++
		default:
			resp.Counts.Direct++
			if d.Importers == 0 {
				row.Caveat = unusedDependencyCaveat
				unused = append(unused, row)
			}
		}
		if d.Importers > 0 {
			resp.Counts.Imported++
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Ecosystem < rows[j].Ecosystem
	})
	sort.Slice(unused, func(i, j int) bool { return unused[i].Name < unused[j].Name })

	resp.Counts.Declared = len(rows)
	resp.Counts.NotImported = len(unused)

	// The reverse direction needs the declared set, so it is computed here
	// rather than at the top of the handler: subtracting a set that has not been
	// read yet is how an empty list becomes a claim.
	reverse, total := s.reverseDirection(ctx, run.ID, list, &om)
	resp.ImportedNotDeclared = reverse
	resp.Counts.ImportedNotDeclared = total
	if !reverse.Available {
		om.Note(ReasonImportsNotEnumerable, reverse.Detail, reverse.Remedy)
	}

	resp.NotImported = capped(unused, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Unused declarations beyond the first %d (alphabetical) are not listed. "+
			"counts.declared_but_not_imported is the full figure.", maxRows), "repocortex deps")
	resp.Declared = capped(rows, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Declared dependencies beyond the first %d are not listed. counts.declared is the "+
			"full figure.", maxRows), "repocortex deps --all")
	if resp.NotImported == nil {
		resp.NotImported = []DependencyRow{}
	}
	if resp.Declared == nil {
		resp.Declared = []DependencyRow{}
	}

	switch {
	case len(rows) == 0:
		resp.Help = &Guidance{
			Title: "This index found no dependency manifest",
			Detail: depsCoverageLimit + " No manifest was recognised in this repository, so there is no " +
				"declared set to join against — which is not the same as a project with no dependencies.",
			Steps: []string{
				"repocortex index — manifests are read during indexing",
				"repocortex deps — the same report in the terminal, with the manifest list",
				"Check the language tiers below: a toolchain RepoCortex parses at `parsed` still has its manifest read, so an empty answer here really is an absent manifest",
			},
		}
		resp.Next = NextAction{
			Label:   "Confirm the manifest is one of the seven read",
			Detail:  "A project declaring dependencies in a format outside that list reports nothing here by construction.",
			Command: "repocortex deps",
		}
	case len(unused) > 0:
		resp.Next = NextAction{
			Label:   fmt.Sprintf("%d declared dependency(ies) are imported by nothing", len(unused)),
			Detail:  unusedDependencyCaveat,
			Command: "repocortex deps",
		}
	default:
		resp.Next = NextAction{
			Label:   "Every direct declaration has an importer",
			Detail:  fmt.Sprintf("%d declared, %d of them marked indirect by the manifest itself.", len(rows), resp.Counts.Indirect),
			Command: "repocortex deps --all",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// reverseUnavailable is the honest non-answer, in one shape.
//
// A struct rather than an empty list, because an empty list is a CLAIM — that
// every import path in this repository matches something a manifest declares —
// and a portal that could not perform the subtraction has not checked it.
func reverseUnavailable(detail reverseGap) ReverseDirection {
	return ReverseDirection{Rows: []ImportRow{}, Detail: detail.detail, Remedy: detail.remedy}
}

// reverseGap is one reason the subtraction could not be performed.
type reverseGap struct{ detail, remedy string }

var (
	// reverseNoRun: nothing has been indexed, so there are no imports to read.
	reverseNoRun = reverseGap{
		detail: "There is no committed index, so no import path has been recorded and nothing " +
			"can be subtracted from the declared set. This is an absent index, not a repository " +
			"whose every import is declared.",
		remedy: "repocortex index",
	}

	// reverseNoImportEnumerator: the port is unwired. Kept reachable because a
	// Registry implementation predating registry.DistinctImports takes it.
	reverseNoImportEnumerator = reverseGap{
		detail: "Import paths could not be enumerated: the read port this portal was given exposes " +
			"the declared set and the per-dependency importer counts, but not the run's distinct " +
			"import paths. The index HAS them — every import of every file is recorded — so this is " +
			"missing wiring, not a missing fact. An empty list is not published here, because an " +
			"empty list would assert that every import in this repository matches a declaration, " +
			"which nothing checked.",
		remedy: "repocortex ui — the shipped portal wires the full registry",
	}

	// reverseNoDeclaredSet: the declared half is missing, so the subtraction has
	// no subtrahend.
	reverseNoDeclaredSet = reverseGap{
		detail: "The declared set could not be read, and \"imported but never declared\" is a " +
			"subtraction: without it the remainder would be every import path in the repository, " +
			"presented as if each were a missing declaration.",
		remedy: "repocortex deps --all",
	}
)

// importsNotDeclaredCaveat states what the list is and, more importantly, what
// it is not.
//
// It is carried in the payload rather than left to the client, for the reason
// the unused-dependency caveat is: a screenshot does not carry a footnote, and
// this list is the one on this screen most easily misread as a verdict.
const importsNotDeclaredCaveat = "A PROMPT, NOT A VERDICT, and read the contents before the " +
	"count. This is every import path the index recorded that no declared dependency MATCHES, " +
	"and the index records what was imported — never who provides it. So the list also holds " +
	"standard-library paths (`java.util.Map`, `net/http`) and this repository's own packages, " +
	"neither of which any manifest would declare. It holds transitive packages too: a Maven " +
	"artifact is matched on its group prefix, so `org.projectlombok:lombok` does not match an " +
	"import of `lombok.Getter` even though it provides it."

// reverseDirection answers "what does this repository import that it never
// declared", or says exactly why it cannot.
//
// It returns the full count beside the (possibly shortened) rows, so
// counts.imported_but_not_declared is never a function of the row budget. The
// count is -1 when the question could not be answered at all — a 0 there would
// assert that every import matches a declaration.
func (s *Server) reverseDirection(ctx context.Context, runID int64,
	declared []registry.DependencyUsage, om *Omissions) (ReverseDirection, int) {
	enum, ok := s.deps.Registry.(ImportEnumerator)
	if !ok {
		return reverseUnavailable(reverseNoImportEnumerator), -1
	}
	imports, err := enum.DistinctImports(ctx, runID)
	if err != nil {
		return reverseUnavailable(reverseGap{
			detail: "The run's import paths could not be read: " + err.Error(),
			remedy: "repocortex doctor",
		}), -1
	}

	rows := make([]ImportRow, 0, len(imports))
	var local int
	for path, files := range imports {
		// A relative import names a file in this tree. No manifest declares one,
		// so listing it would be noise with a certain cause rather than a
		// finding — the one exclusion made here, because it is the one that
		// needs no knowledge of any ecosystem to be certain of.
		if isLocalImport(path) {
			local++
			continue
		}
		if declaredMatches(declared, path) {
			continue
		}
		rows = append(rows, ImportRow{Path: path, Files: files})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Files != rows[j].Files {
			return rows[i].Files > rows[j].Files
		}
		return rows[i].Path < rows[j].Path
	})

	total := len(rows)
	detail := fmt.Sprintf("%d of the %d distinct import path(s) in this run match no declared "+
		"dependency (%d relative path(s) excluded, since no manifest declares one). %s",
		total, len(imports), local, importsNotDeclaredCaveat)
	if len(declared) == 0 {
		// The subtraction is degenerate: with nothing declared, everything is
		// undeclared, and the count says more about the absent manifest than
		// about the imports. Stated first, because it changes how the whole
		// list reads.
		detail = fmt.Sprintf("NOTHING WAS SUBTRACTED: this run recognised no dependency manifest, "+
			"so every one of the %d import path(s) below is listed for want of a declared set to "+
			"match against — not because a declaration is missing. %s", total, detail)
	}

	rows = capped(rows, maxRows, om, ReasonRowBudget,
		fmt.Sprintf("Undeclared import paths beyond the first %d (ordered by importing files) are "+
			"not listed. counts.imported_but_not_declared is the full figure.", maxRows),
		"repocortex deps --all")
	if rows == nil {
		rows = []ImportRow{}
	}
	return ReverseDirection{Available: true, Rows: rows, Detail: detail}, total
}

// declaredMatches reports whether any declared dependency claims this import.
//
// The matcher is the analysis layer's, supplied here for the reason
// importMatcher documents: the ecosystem rules are not the registry's to know
// and not this package's to reimplement.
func declaredMatches(declared []registry.DependencyUsage, path string) bool {
	for _, d := range declared {
		if importMatcher(d.Name, d.Ecosystem, path) {
			return true
		}
	}
	return false
}

// isLocalImport reports whether a path names a file in this repository rather
// than a package anything could declare.
//
// Deliberately narrow. Relative and absolute paths are local in every language
// that has them, which makes this decidable without knowing the language; a
// standard-library path is NOT decidable that way (`os` is Go's standard
// library and also a PyPI distribution), so no attempt is made to guess one,
// and the caveat says so instead of a filter pretending to have done it.
func isLocalImport(path string) bool {
	return path == "" || strings.HasPrefix(path, ".") || strings.HasPrefix(path, "/")
}

// tierWhatYouGet is parse.Tier.Description() in the portal's words. The CLI's
// flags take tier ids (`--min-confidence high`), so its own description names
// the id; a reader of the portal gets the word the legend gives that id.
func tierWhatYouGet(t parse.Tier) string {
	switch t {
	case parse.TierTyped:
		return "definitions, imports, receiver and arity discrimination — references can reach the " +
			tierWord(model.ConfidenceHigh) + " tier"
	case parse.TierResolved:
		return "definitions and imports — references bind across files"
	default:
		return "definitions and search only — no cross-file references claimed"
	}
}

// languageTiers reports the three parser tiers, joined against what is actually
// in this repository.
//
// idx may be nil, in which case only the supported lists are reported — the
// tiers are a property of the binary and are true before anything is indexed.
func languageTiers(idx *structuralIndex) []LanguageTier {
	totalSymbols := 0
	if idx != nil {
		for _, n := range idx.symLangs {
			totalSymbols += n
		}
	}

	out := make([]LanguageTier, 0, 3)
	for _, t := range []parse.Tier{parse.TierTyped, parse.TierResolved, parse.TierParsed} {
		langs := parse.LanguagesInTier(t)
		row := LanguageTier{
			Tier: t.String(), WhatYouGet: tierWhatYouGet(t),
			Supported: langs, Count: len(langs),
			Present: []LanguagePresence{},
		}
		if idx != nil {
			for _, lang := range langs {
				files, symbols := idx.fileLangs[lang], idx.symLangs[lang]
				if files == 0 && symbols == 0 {
					continue
				}
				row.Present = append(row.Present, LanguagePresence{
					Language: lang, Files: files, Symbols: symbols,
				})
				row.FilesHere += files
				row.SymbolsHere += symbols
			}
			sort.Slice(row.Present, func(i, j int) bool {
				if row.Present[i].Symbols != row.Present[j].Symbols {
					return row.Present[i].Symbols > row.Present[j].Symbols
				}
				return row.Present[i].Language < row.Present[j].Language
			})
			row.PresentCount = len(row.Present)
			if totalSymbols > 0 {
				row.SymbolShare = round1(100 * float64(row.SymbolsHere) / float64(totalSymbols))
			}
		}
		out = append(out, row)
	}
	return out
}
