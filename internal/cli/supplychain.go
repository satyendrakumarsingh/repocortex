package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/deps"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/supplychain"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/security"

	"github.com/spf13/cobra"
)

// `sbom` and `vulns`: the declared dependencies as a document other tools
// read, and those dependencies against advisories the user supplies.
//
// Both are thin. What a version state is, how a range is evaluated and when
// "not imported" may be said all live in internal/capability/supplychain; this
// file opens the index, hands the capability its inputs, and prints.
//
// Neither command touches the network, and `vulns` ships no advisory data. See
// the package comment of internal/capability/supplychain for why.

// maxLockfileBytes bounds a manifest or lockfile read from the working tree.
// A package-lock.json for a large application is tens of megabytes.
const maxLockfileBytes = 128 << 20

// repoFileReader reads regular files beneath the project root, by their
// repository-relative path.
//
// Lockfiles are read from the working tree, not the index: the index skips
// files it has no use for parsing, and a lockfile is exactly that. The paths
// asked for are always a fixed filename joined to a manifest's directory.
// security.ValidatePath resolves symbolic links and rejects anything that
// lands outside the root, so a repository cannot point "package-lock.json" at
// a file elsewhere on the machine — the same guard `arch adr` reads through.
func repoFileReader(root string) supplychain.FileReader {
	return func(rel string) ([]byte, error) {
		full, err := security.ValidatePath(root, rel)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(full) //nolint:gosec // G703: contained under the repository root by ValidatePath
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", rel)
		}
		if info.Size() > maxLockfileBytes {
			return nil, fmt.Errorf("%s is larger than %d MB", rel, maxLockfileBytes>>20)
		}
		return os.ReadFile(full) //nolint:gosec // G304: contained under the repository root by ValidatePath
	}
}

// declaredComponents reads the index's dependency rows and resolves each to a
// component.
func declaredComponents(ctx stdctx.Context, s *session, g *Global) ([]supplychain.Component, *supplychain.Resolver, error) {
	// A nil matcher: only the declarations are wanted here. Importers is left
	// unmeasured and is not read.
	rows, err := s.reg.DependencyReport(ctx, s.runID, nil)
	if err != nil {
		return nil, nil, err
	}
	declared := make([]supplychain.Declared, 0, len(rows))
	for _, d := range rows {
		declared = append(declared, supplychain.Declared{
			Name: d.Name, Ecosystem: deps.Ecosystem(d.Ecosystem), Version: d.Version,
			Scope: d.Scope, Manifest: d.Manifest, Line: d.Line, Indirect: d.Indirect,
		})
	}
	resolver := supplychain.NewResolver(repoFileReader(g.Config().RepoRoot))
	return resolver.ResolveAll(declared), resolver, nil
}

// ---------------------------------------------------------------- sbom

func newSBOMCmd(g *Global) *cobra.Command {
	var (
		timestamp string
		name      string
	)
	cmd := &cobra.Command{
		Use:   "sbom",
		Short: "Write the declared dependencies as a CycloneDX SBOM",
		Long: `Writes a CycloneDX 1.5 JSON document of the dependencies this project
DECLARES, and nothing else, on standard output.

  repocortex sbom > sbom.cdx.json

Every component carries a package URL for its ecosystem and the version exactly
as the manifest or a lockfile states it. Where no single version is known, the
component says so in the property repocortex:versionState:

  pinned      the manifest states one exact version
  locked      the manifest gives a range; a lockfile beside it states the version
  range       declared as a range, and no lockfile pins it
  unpinned    no version constraint at all, or one managed elsewhere (a parent POM, a BOM)
  unresolved  a property placeholder, a Git or path source, a local replacement

A component that is not pinned or locked has a package URL WITHOUT a version:
a range is not a release, and a scanner handed "pkg:npm/lodash@^4.17.20" looks
up a release that does not exist.

The output is deterministic — sorted, no serial number, no timestamp unless
--timestamp sets one — so the file can be committed and its diff is a diff of
the dependencies.

DECLARED dependencies only. A package that appears only in a lockfile, the
transitive closure, is not listed, and the document states that itself
(compositions: incomplete). Manifests read: go.mod, package.json, pom.xml,
build.gradle, requirements.txt, pyproject.toml, Cargo.toml. Lockfiles read for
versions: package-lock.json, npm-shrinkwrap.json, yarn.lock, Cargo.lock,
poetry.lock, uv.lock, gradle.lockfile. pnpm-lock.yaml is not read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timestamp != "" {
				if _, err := time.Parse(time.RFC3339, timestamp); err != nil {
					return fmt.Errorf("--timestamp %q: want RFC 3339, for example 2026-01-02T15:04:05Z", timestamp)
				}
			}
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			components, resolver, err := declaredComponents(ctx, s, g)
			if err != nil {
				return err
			}
			if name == "" {
				name = projectName(components, resolver)
			}
			bom, summary := supplychain.BuildSBOM(components, supplychain.SBOMOptions{
				Name: name, Timestamp: timestamp, Lockfiles: resolver.LockfilesRead(),
			})
			if err := bom.Encode(cmd.OutOrStdout()); err != nil {
				return err
			}

			// The account of the document goes to the log, on stderr: stdout
			// is the document and only the document.
			if summary.Components == 0 {
				g.Log().Warn("the index holds no declared dependency; the SBOM lists no components",
					"manifests_read", "go.mod, package.json, pom.xml, build.gradle, requirements.txt, pyproject.toml, Cargo.toml")
				return nil
			}
			if open := summary.Components - summary.Concrete; open > 0 {
				g.Log().Warn("some components have no single version: a scanner fed this SBOM cannot match them",
					"components", summary.Components, "without_a_concrete_version", open,
					"range", summary.ByState[supplychain.StateRange],
					"unpinned", summary.ByState[supplychain.StateUnpinned],
					"unresolved", summary.ByState[supplychain.StateUnresolved])
			} else {
				g.Log().Info("sbom written", "components", summary.Components,
					"scope", "declared dependencies only")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&timestamp, "timestamp", "",
		"write this RFC 3339 time as metadata.timestamp (default: none, so the output is reproducible)")
	cmd.Flags().StringVar(&name, "name", "",
		"name of the project the SBOM describes (default: the root manifest's own module or package name)")
	return cmd
}

// projectName picks the name the shallowest manifest declares for itself: the
// go.mod module path, the package.json name. The checkout directory is not
// used — it differs between machines, and the document must not.
func projectName(components []supplychain.Component, resolver *supplychain.Resolver) string {
	seen := map[string]bool{}
	var manifests []string
	for _, c := range components {
		if !seen[c.Manifest] {
			seen[c.Manifest] = true
			manifests = append(manifests, c.Manifest)
		}
	}
	sort.Slice(manifests, func(i, j int) bool {
		di, dj := strings.Count(manifests[i], "/"), strings.Count(manifests[j], "/")
		if di != dj {
			return di < dj
		}
		return manifests[i] < manifests[j]
	})
	for _, m := range manifests {
		if n := resolver.ModuleName(m); n != "" {
			return n
		}
	}
	return ""
}

// ---------------------------------------------------------------- vulns

// vulnsReport is the JSON shape of `repocortex vulns`.
type vulnsReport struct {
	IndexRevision int64 `json:"index_revision"`
	supplychain.Report
}

func newVulnsCmd(g *Global) *cobra.Command {
	var (
		advisories []string
		failOn     string
		ignore     []string
		showAll    bool
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "vulns",
		Short: "Check declared dependencies against advisories you supply, and whether the code uses them",
		Long: `Reads OSV-format advisories (https://ossf.github.io/osv-schema/) from files
YOU supply, lists the declared dependencies that fall in an affected range, and
says for each what the index shows about its use.

  repocortex vulns --advisories osv.json
  repocortex vulns --advisories ./osv-dump/ --fail-on imported

NO NETWORK, NO BUILT-IN ADVISORIES. This command never downloads a
vulnerability database and ships none: a list compiled into a binary is stale
the day it is built. Without --advisories it reports nothing and says how to
obtain them.

Version status, per advisory and dependency:

  affected            the version was compared and is in an affected range
  could_not_compare   the comparison could not be made — a range with no
                      lockfile, a placeholder, a Git commit range. This is
                      NEVER reported as unaffected.
  not_affected        every range was compared and the version is outside all
                      of them (counted; --all or --json lists them)

Ordering is the ecosystem's own: semver for npm, Go and Cargo, PEP 440 for PyPI,
Maven's ComparableVersion for Maven and Gradle.

Reachability, for every affected or uncompared dependency:

  referenced                    a file that imports the package references a
                                symbol the advisory names
  imported_symbol_use_unknown   imported; the advisory names no symbol, or the
                                index could not bind one. Not a clearance.
  not_imported                  no indexed file imports it. Said only where a
                                package name determines its import path: Go, npm,
                                and well-known PyPI distributions.
  import_use_unknown            no import matched, where that shows nothing: an
                                ecosystem whose names do not determine import
                                paths (Maven, Gradle, Cargo, most PyPI), or an
                                index with no source that could import it

The report ends with its own coverage: which languages record imports, which
source files were not fully parsed, and that only DECLARED dependencies were
checked — a package that appears only in a lockfile is not examined. One
exception is reported rather than dropped: a Go or npm package an advisory
names that no manifest declares but the indexed code imports anyway. It has no
declared version, so it is always could_not_compare.

--fail-on makes the exit status a gate (1 when it fails):

  referenced   a named symbol is referenced
  imported     the package is imported, or whether it is could not be determined
  affected     any dependency an advisory matches

A dependency whose version could not be compared fails a gate exactly as an
affected one does, and so does an advisory file that could not be read: a gate
must not pass on a check that was not made. --ignore leaves an advisory out by
id or alias, and the report lists what was ignored.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var gate supplychain.GateLevel
			if failOn != "" {
				level, ok := supplychain.ParseGateLevel(failOn)
				if !ok {
					return fmt.Errorf("--fail-on %q: want referenced, imported or affected", failOn)
				}
				gate = level
				// A gate with nothing to check always passes, which is the
				// one thing a gate must never do silently.
				if len(advisories) == 0 {
					return errors.New("--fail-on needs --advisories: with no advisories there is nothing to " +
						"check, and a gate that checked nothing must not pass")
				}
			}

			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			components, _, err := declaredComponents(ctx, s, g)
			if err != nil {
				return err
			}

			if len(advisories) == 0 {
				if g.JSON {
					return emitJSON(out, map[string]any{
						"index_revision":           s.runID,
						"checked":                  false,
						"declared_dependencies":    len(components),
						"how_to_obtain_advisories": advisorySources(),
					})
				}
				printNoAdvisories(out, components)
				return nil
			}

			set, err := supplychain.LoadAdvisories(advisories)
			if err != nil {
				return err
			}
			rep, err := supplychain.Analyze(ctx, components, set,
				&supplyIndex{reg: s.reg, runID: s.runID, lines: map[string][]string{}},
				supplychain.Options{Ignore: ignore, ImportLanguages: importBindingLanguages()})
			if err != nil {
				return err
			}

			if g.JSON {
				if err := emitJSON(out, vulnsReport{IndexRevision: s.runID, Report: rep}); err != nil {
					return err
				}
			} else {
				printVulns(out, rep, showAll, limit)
			}
			return vulnsGate(rep, gate)
		},
	}
	cmd.Flags().StringArrayVar(&advisories, "advisories", nil,
		"OSV JSON file, or directory of them, to check against (repeatable)")
	cmd.Flags().StringVar(&failOn, "fail-on", "",
		"exit non-zero when a finding at this reachability or stronger exists (referenced|imported|affected)")
	cmd.Flags().StringSliceVar(&ignore, "ignore", nil, "advisory id or alias to leave out (repeatable, comma-separated)")
	cmd.Flags().BoolVar(&showAll, "all", false, "list the not-affected comparisons and every importing file")
	cmd.Flags().IntVarP(&limit, "limit", "n", 5, "importing files, and symbol references, shown per finding")
	return cmd
}

// vulnsGate turns --fail-on into an exit status. With no level it never fails.
func vulnsGate(rep supplychain.Report, level supplychain.GateLevel) error {
	if level == "" {
		return nil
	}
	reasons := rep.Gate(level)
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf("%s (--fail-on %s)", strings.Join(reasons, "; "), level)
}

// advisorySources says where OSV records come from. Text only: nothing here is
// fetched.
func advisorySources() []string {
	return []string{
		"osv-scanner (https://google.github.io/osv-scanner/) writes OSV records for a project: " +
			"`osv-scanner scan source --format json --output osv.json .`, then `repocortex vulns --advisories osv.json`",
		"an OSV dump, one ecosystem per archive: https://osv-vulnerabilities.storage.googleapis.com/<ecosystem>/all.zip " +
			"(Go, npm, PyPI, crates.io, Maven) — unzip it and pass the directory",
		"one advisory as JSON from https://api.osv.dev/v1/vulns/<id>, saved to a file",
	}
}

func printNoAdvisories(out io.Writer, components []supplychain.Component) {
	fmt.Fprintln(out, "No advisories were given, so nothing was checked.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "repocortex never downloads vulnerability data and ships none. Supply OSV-format")
	fmt.Fprintln(out, "records (https://ossf.github.io/osv-schema/) as local files with --advisories:")
	fmt.Fprintln(out)
	for _, src := range advisorySources() {
		fmt.Fprintf(out, "  - %s\n", src)
	}
	fmt.Fprintln(out)
	if len(components) == 0 {
		fmt.Fprintln(out, "The index holds no declared dependency. Manifests read: go.mod, package.json, "+
			"pom.xml, build.gradle, requirements.txt, pyproject.toml, Cargo.toml.")
		return
	}
	fmt.Fprintf(out, "%d declared dependency(ies) would be checked (%s). `repocortex sbom` writes them as "+
		"CycloneDX for any scanner.\n", len(components), ecosystemCounts(components))
}

func ecosystemCounts(components []supplychain.Component) string {
	counts := map[string]int{}
	for _, c := range components {
		counts[string(c.Ecosystem)]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s %d", n, counts[n]))
	}
	return strings.Join(parts, ", ")
}

func reachLabel(s supplychain.ReachState) string {
	switch s {
	case supplychain.ReachReferenced:
		return "REFERENCED"
	case supplychain.ReachImported:
		return "IMPORTED, SYMBOL USE UNKNOWN"
	case supplychain.ReachNotImported:
		return "NOT IMPORTED in the indexed code"
	}
	return "IMPORT USE UNKNOWN"
}

func printVulns(out io.Writer, rep supplychain.Report, showAll bool, limit int) {
	sum := rep.Summary
	fmt.Fprintf(out, "%d advisory record(s) from %d file(s) checked against %d declared dependency(ies)\n\n",
		sum.Advisories, rep.Advisories.Files, sum.Dependencies)

	section := func(title string, status supplychain.MatchStatus) {
		var list []supplychain.Finding
		for _, f := range rep.Findings {
			if f.Status == status {
				list = append(list, f)
			}
		}
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(out, "%s (%d)\n", title, len(list))
		for _, f := range list {
			printFinding(out, f, showAll, limit)
		}
	}
	section("AFFECTED", supplychain.MatchAffected)
	section("COULD NOT COMPARE — not the same as unaffected", supplychain.MatchUnknown)

	switch {
	case len(rep.Findings) == 0 && sum.NotAffected == 0:
		fmt.Fprintln(out, "No declared dependency is named by these advisories.")
	case len(rep.Findings) == 0:
		fmt.Fprintln(out, "No declared dependency is in an affected range.")
	}
	if sum.AdvisoriesNamingNothing > 0 {
		fmt.Fprintf(out, "%d of %d advisory record(s) name no DECLARED dependency. That is not the same as "+
			"not present: a lockfile-only package is not examined.\n", sum.AdvisoriesNamingNothing, sum.Advisories)
	}
	if sum.NotAffected > 0 {
		fmt.Fprintf(out, "NOT AFFECTED (%d): named by an advisory, version compared, outside every affected range.",
			sum.NotAffected)
		if !showAll {
			fmt.Fprintln(out, " --all lists them.")
		} else {
			fmt.Fprintln(out)
			for _, c := range rep.NotAffected {
				fmt.Fprintf(out, "  %s %s  %s  %s — %s\n", c.Package, c.Version, c.Manifest, c.Advisory, c.Reason)
			}
		}
	}

	fmt.Fprintln(out, "\nWhat this report covers")
	for _, ec := range rep.Coverage {
		fmt.Fprintf(out, "  %s: %d declared, %d with a concrete version; ordering: %s\n",
			ec.Ecosystem, ec.Dependencies, ec.Concrete, ec.VersionScheme)
		fmt.Fprintf(out, "    \"not imported\" is claimed: %s\n", ec.NotImportedIsClaimed)
		fmt.Fprintf(out, "    symbols: %s\n", ec.SymbolBinding)
		for _, lc := range ec.Languages {
			if lc.Files == 0 {
				continue
			}
			binds := "imports recorded"
			if !lc.BindsImports {
				binds = "imports NOT recorded"
			}
			fmt.Fprintf(out, "    %s: %d file(s), %s, %d not fully parsed\n", lc.Language, lc.Files, binds, lc.NotParsed)
		}
		for _, f := range ec.FilesNotParsed {
			fmt.Fprintf(out, "      not fully parsed (%s): %s\n", f.Status, f.Path)
		}
		if more := ec.FilesNotParsedTotal - len(ec.FilesNotParsed); more > 0 {
			fmt.Fprintf(out, "      ... and %d more (--json lists the first %d)\n", more, len(ec.FilesNotParsed))
		}
		for _, n := range ec.Notes {
			fmt.Fprintf(out, "    note: %s\n", n)
		}
	}
	if len(rep.Coverage) == 0 {
		fmt.Fprintln(out, "  The index holds no declared dependency. Manifests read: go.mod, package.json, "+
			"pom.xml, build.gradle, requirements.txt, pyproject.toml, Cargo.toml.")
	}

	adv := rep.Advisories
	fmt.Fprintf(out, "\nAdvisories: %d record(s) used; skipped %d withdrawn, %d duplicate, %d JSON file(s) with no OSV record\n",
		adv.Records, adv.Withdrawn, adv.Duplicates, adv.NotOSV)
	if len(adv.Ignored) > 0 {
		fmt.Fprintf(out, "  ignored by --ignore: %s\n", strings.Join(adv.Ignored, ", "))
	}
	for _, p := range adv.Unreadable {
		fmt.Fprintf(out, "  UNREADABLE, so possibly a missing advisory: %s (%s)\n", p.Path, p.Reason)
	}
	if adv.GoStandardLibrary > 0 {
		fmt.Fprintf(out, "  %d advisory(ies) are about Go's standard library or toolchain: NOT evaluated, the Go "+
			"version is not a declared dependency. govulncheck covers them.\n", adv.GoStandardLibrary)
	}
	if len(adv.OtherEcosystems) > 0 {
		names := make([]string, 0, len(adv.OtherEcosystems))
		for n := range adv.OtherEcosystems {
			names = append(names, fmt.Sprintf("%s %d", n, adv.OtherEcosystems[n]))
		}
		sort.Strings(names)
		fmt.Fprintf(out, "  affected entries for ecosystems not matched here: %s\n", strings.Join(names, ", "))
	}

	fmt.Fprintln(out)
	for _, c := range rep.Caveats {
		fmt.Fprintf(out, "%s\n", c)
	}
}

func printFinding(out io.Writer, f supplychain.Finding, showAll bool, limit int) {
	version := f.Package.Version
	if version == "" {
		version = fmt.Sprintf("(%s", f.Package.State)
		if f.Package.Declared != "" {
			version += " " + f.Package.Declared
		}
		version += ")"
	}
	id := f.Advisory
	if len(f.Aliases) > 0 {
		id += " (" + strings.Join(f.Aliases, ", ") + ")"
	}
	where := fmt.Sprintf("%s:%d", f.Package.Manifest, f.Package.Line)
	if f.Package.Manifest == "" {
		where = "NOT DECLARED in any manifest"
	}
	fmt.Fprintf(out, "  %s %s  [%s]  %s  %s\n", f.Package.Name, version, f.Package.Ecosystem, where, id)
	if f.Summary != "" {
		fmt.Fprintf(out, "    %s\n", f.Summary)
	}
	if f.Severity != "" {
		fmt.Fprintf(out, "    severity (as the advisory states it): %s\n", f.Severity)
	}
	fmt.Fprintf(out, "    version: %s\n", f.Reason)
	if len(f.FixedIn) > 0 {
		fmt.Fprintf(out, "    fixed in: %s\n", strings.Join(f.FixedIn, ", "))
	}
	r := f.Reachability
	fmt.Fprintf(out, "    use: %s — %s\n", reachLabel(r.State), r.Detail)
	refs := r.SymbolReferences
	if !showAll && limit >= 0 && len(refs) > limit {
		refs = refs[:limit]
	}
	for _, ref := range refs {
		fmt.Fprintf(out, "      %s:%d  %s\n", ref.File, ref.Line, ref.Evidence)
	}
	if more := len(r.SymbolReferences) - len(refs); more > 0 {
		fmt.Fprintf(out, "      ... and %d more (--all, or --json)\n", more)
	}
	if r.ImportingFileCount > 0 {
		fmt.Fprintf(out, "    imported by %d file(s):\n", r.ImportingFileCount)
		shown := r.ImportingFiles
		if !showAll && limit >= 0 && len(shown) > limit {
			shown = shown[:limit]
		}
		for _, site := range shown {
			via := ""
			if site.Via != "" {
				via = "  (" + site.Via + ")"
			}
			fmt.Fprintf(out, "      %s:%d  %s%s\n", site.File, site.Line, site.Path, via)
		}
		if more := r.ImportingFileCount - len(shown); more > 0 {
			fmt.Fprintf(out, "      ... and %d more (--all, or --json)\n", more)
		}
	}
	fmt.Fprintln(out)
}

// ---------------------------------------------------------------- index adapter

// supplyIndex adapts the registry to the Index the capability declares.
type supplyIndex struct {
	reg   *registry.Registry
	runID int64
	// lines caches the indexed text of files whose lines are asked for. A nil
	// entry records that the index holds no text for the file.
	lines map[string][]string
}

func (x *supplyIndex) ImportSites(ctx stdctx.Context) ([]supplychain.ImportSite, error) {
	rows, err := x.reg.ImportSites(ctx, x.runID)
	if err != nil {
		return nil, err
	}
	out := make([]supplychain.ImportSite, 0, len(rows))
	for _, r := range rows {
		out = append(out, supplychain.ImportSite{
			File: r.File, Line: r.Line, Path: r.Path, Alias: r.Alias, Language: r.Language,
		})
	}
	return out, nil
}

func (x *supplyIndex) References(ctx stdctx.Context, names []string) ([]supplychain.RefSite, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := x.reg.ReferencesNamed(ctx, x.runID, names)
	if err != nil {
		return nil, err
	}
	out := make([]supplychain.RefSite, 0, len(rows))
	for _, r := range rows {
		out = append(out, supplychain.RefSite{
			File: r.File, Line: r.Line, Name: r.Name, Receiver: r.Receiver, Kind: r.Kind,
		})
	}
	return out, nil
}

// wholeFile asks FileExcerpt for every line of a file.
const wholeFile = 1 << 30

func (x *supplyIndex) LineText(ctx stdctx.Context, file string, line int) (string, bool) {
	lines, cached := x.lines[file]
	if !cached {
		// Read from the index, as every other answer here is: the line numbers
		// belong to the indexed text, not to whatever is on disk now.
		if text, err := x.reg.FileExcerpt(ctx, x.runID, file, 1, wholeFile); err == nil {
			lines = strings.Split(text, "\n")
		}
		x.lines[file] = lines
	}
	if line < 1 || line > len(lines) {
		return "", false
	}
	return lines[line-1], true
}

func (x *supplyIndex) Files(ctx stdctx.Context) ([]supplychain.FileInfo, error) {
	rows, err := x.reg.Files(ctx, x.runID)
	if err != nil {
		return nil, err
	}
	out := make([]supplychain.FileInfo, 0, len(rows))
	for _, f := range rows {
		out = append(out, supplychain.FileInfo{Path: f.Path, Language: f.Language, ParseStatus: f.ParseStatus})
	}
	return out, nil
}

// importBindingLanguages lists the languages whose imports the index records,
// derived as `repocortex languages` derives its tiers (an import query or a
// tree walk: Rust has only the walk). A
// single-file component delegates its script block to a language that has an
// import query, so its imports are recorded too.
func importBindingLanguages() []string {
	var out []string
	for lang := range parse.Tiers() {
		if parse.ReadsImports(lang) || lang == "vue" || lang == "svelte" {
			out = append(out, lang)
		}
	}
	sort.Strings(out)
	return out
}
