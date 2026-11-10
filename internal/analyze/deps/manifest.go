// Package deps reads dependency manifests: what a project declares it needs.
//
// # Why manifests and not lock files
//
// A lock file is the resolved transitive closure — hundreds of packages, most of
// which no line of this repository mentions. A manifest is what the authors
// chose. "You declare cobra and never import it" is an actionable finding;
// "your lock file contains golang.org/x/sys" is noise, because it is there
// because something else needs it.
//
// Lock files are therefore read only for the VERSION of a declared dependency
// where the manifest gives a range, never as a source of dependencies. What a
// project depends on is a statement its authors made.
//
// # What this deliberately does not do
//
// No vulnerability data. Spec §27 is explicit — "do not invent vulnerability
// information", and a scanner that ships a stale embedded advisory list is
// exactly that. Vulnerability lookup belongs in an integration layer against a
// live database, and reporting a version accurately is the useful half RepoCortex
// can do honestly today.
//
// No license inference either. A license is a legal fact about a package, not
// something to guess from a name.
package deps

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Ecosystem names the packaging system a manifest belongs to.
type Ecosystem string

const (
	EcosystemGo     Ecosystem = "go"
	EcosystemNPM    Ecosystem = "npm"
	EcosystemMaven  Ecosystem = "maven"
	EcosystemPyPI   Ecosystem = "pypi"
	EcosystemCargo  Ecosystem = "cargo"
	EcosystemGradle Ecosystem = "gradle"
)

// Scope separates dependencies that ship from those that only build or test.
//
// The distinction carries real weight in an unused-dependency report: an unused
// runtime dependency is dead weight in a deployed artefact, while an unused test
// dependency is usually a fixture helper invoked by a framework rather than
// imported by name.
type Scope string

const (
	ScopeRuntime Scope = "runtime"
	ScopeDev     Scope = "dev"
)

// Dependency is one declared dependency.
type Dependency struct {
	// Name is the identifier the ecosystem uses and that source imports:
	// `github.com/spf13/cobra`, `react`, `com.google.guava:guava`, `requests`.
	Name string
	// Version is as declared — a pin, a range, or empty when the manifest
	// defers to a lock file or a parent POM. Reported verbatim rather than
	// resolved, because resolving a range means running the ecosystem's solver.
	Version   string
	Ecosystem Ecosystem
	Scope     Scope
	Line      int
	// Indirect marks a dependency the manifest itself records as transitive —
	// Go's `// indirect` marker. Reporting one as unused would be noise: no
	// line of this repository is supposed to import it.
	Indirect bool
}

// Manifest is one dependency file.
type Manifest struct {
	Path         string
	Ecosystem    Ecosystem
	Dependencies []Dependency
	// Module is the project's own name where the manifest declares one. Needed
	// to tell an internal import from a third-party one: in Go, everything
	// under the module path is this repository's own code.
	Module string
}

// IsManifest reports whether a repo-relative path is a dependency manifest.
func IsManifest(path string) bool {
	return ecosystemOf(path) != ""
}

func ecosystemOf(path string) Ecosystem {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	switch base {
	case "go.mod":
		return EcosystemGo
	case "package.json":
		return EcosystemNPM
	case "pom.xml":
		return EcosystemMaven
	case "requirements.txt", "requirements-dev.txt", "pyproject.toml":
		return EcosystemPyPI
	case "Cargo.toml":
		return EcosystemCargo
	case "build.gradle", "build.gradle.kts":
		return EcosystemGradle
	}
	return ""
}

// maxManifestBytes bounds a manifest. A package.json larger than this is
// generated or a bundle; a hand-written one is measured in kilobytes.
const maxManifestBytes = 2 << 20

// Parse reads one manifest.
func Parse(path string, src []byte) (Manifest, bool) {
	eco := ecosystemOf(path)
	if eco == "" || len(src) == 0 || len(src) > maxManifestBytes {
		return Manifest{}, false
	}
	m := Manifest{Path: path, Ecosystem: eco}

	switch eco {
	case EcosystemGo:
		m.Module, m.Dependencies = parseGoMod(string(src))
	case EcosystemNPM:
		m.Module, m.Dependencies = parsePackageJSON(src)
	case EcosystemMaven:
		m.Dependencies = parsePOM(string(src))
	case EcosystemPyPI:
		if strings.HasSuffix(path, "pyproject.toml") {
			m.Dependencies = parsePyProject(string(src))
		} else {
			m.Dependencies = parseRequirements(string(src), strings.Contains(path, "dev"))
		}
	case EcosystemCargo:
		m.Dependencies = parseCargo(string(src))
	case EcosystemGradle:
		m.Dependencies = parseGradle(string(src))
	}

	if len(m.Dependencies) == 0 && m.Module == "" {
		return m, false
	}
	return m, true
}

// ---------------------------------------------------------------- Go

var (
	goModuleLine = regexp.MustCompile(`^module\s+(\S+)`)
	goRequire    = regexp.MustCompile(`^\s*([^\s/][^\s]*)\s+(v\S+)`)
)

func parseGoMod(src string) (module string, deps []Dependency) {
	inBlock := false
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if m := goModuleLine.FindStringSubmatch(trimmed); m != nil {
			module = m[1]
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
			continue
		case trimmed == ")":
			inBlock = false
			continue
		case strings.HasPrefix(trimmed, "require "):
			trimmed = strings.TrimPrefix(trimmed, "require ")
		case !inBlock:
			continue
		}
		m := goRequire.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		deps = append(deps, Dependency{
			Name: m[1], Version: m[2], Ecosystem: EcosystemGo,
			Scope: ScopeRuntime, Line: i + 1,
			// Go's own marker for a dependency no first-party file imports.
			// Flagging one as unused would be reporting a fact the manifest
			// already states.
			Indirect: strings.Contains(trimmed, "// indirect"),
		})
	}
	return module, deps
}

// ---------------------------------------------------------------- npm

func parsePackageJSON(src []byte) (name string, deps []Dependency) {
	var pkg struct {
		Name            string            `json:"name"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		PeerDeps        map[string]string `json:"peerDependencies"`
	}
	if err := json.Unmarshal(src, &pkg); err != nil {
		return "", nil
	}
	// A blank key is dropped. JSON permits `{"": "1.0.0"}` and npm itself would
	// refuse to install it, so it is never a dependency a person declared — but
	// it decodes cleanly, and it used to reach the unused-dependency report as
	// an empty row that matches no import under any rule in Matches. That row
	// reads as "declared and never used", which is a finding about a package
	// that does not exist.
	add := func(m map[string]string, scope Scope, indirect bool) {
		for n, v := range m {
			if strings.TrimSpace(n) == "" {
				continue
			}
			deps = append(deps, Dependency{
				Name: n, Version: v, Ecosystem: EcosystemNPM, Scope: scope,
				Indirect: indirect, Line: jsonLineOf(string(src), n),
			})
		}
	}
	add(pkg.Dependencies, ScopeRuntime, false)
	add(pkg.DevDependencies, ScopeDev, false)
	// Peer dependencies are declared but supplied by the consumer, so the
	// project genuinely does not import them. Marked indirect so an unused
	// report does not flag every one.
	add(pkg.PeerDeps, ScopeRuntime, true)
	return pkg.Name, deps
}

// jsonLineOf finds the line a key appears on, so a report can cite a location.
// A linear scan rather than a positional parser: manifests are small, and a
// JSON decoder that tracked offsets would be a lot of machinery for one column
// of output.
func jsonLineOf(src, key string) int {
	needle := `"` + key + `"`
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	return 0
}

// ---------------------------------------------------------------- Maven

var (
	pomDependency = regexp.MustCompile(`(?s)<dependency>(.*?)</dependency>`)
	pomGroup      = regexp.MustCompile(`<groupId>\s*([^<]+?)\s*</groupId>`)
	pomArtifact   = regexp.MustCompile(`<artifactId>\s*([^<]+?)\s*</artifactId>`)
	pomVersion    = regexp.MustCompile(`<version>\s*([^<]+?)\s*</version>`)
	pomScope      = regexp.MustCompile(`<scope>\s*([^<]+?)\s*</scope>`)
)

func parsePOM(src string) []Dependency {
	var out []Dependency
	for _, m := range pomDependency.FindAllStringSubmatchIndex(src, -1) {
		block := src[m[2]:m[3]]
		g := firstGroup(pomGroup, block)
		a := firstGroup(pomArtifact, block)
		if g == "" || a == "" {
			continue
		}
		scope := ScopeRuntime
		if s := firstGroup(pomScope, block); s == "test" || s == "provided" {
			scope = ScopeDev
		}
		out = append(out, Dependency{
			// Maven coordinates are group:artifact. Java source imports the
			// PACKAGE, which usually but not always matches the group, so
			// usage matching is prefix-based rather than exact — see
			// deps.Matches.
			Name: g + ":" + a, Version: firstGroup(pomVersion, block),
			Ecosystem: EcosystemMaven, Scope: scope,
			Line: lineOfOffset(src, m[0]),
		})
	}
	return out
}

// ---------------------------------------------------------------- Python

var requirementLine = regexp.MustCompile(`^([A-Za-z0-9._-]+)\s*(\[[^\]]*\])?\s*([=<>!~]=?.*)?$`)

func parseRequirements(src string, dev bool) []Dependency {
	scope := ScopeRuntime
	if dev {
		scope = ScopeDev
	}
	var out []Dependency
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "-") {
			// A leading dash is an option line (-r, -e, --index-url), not a
			// package.
			continue
		}
		if i := strings.Index(trimmed, "#"); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		m := requirementLine.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		out = append(out, Dependency{
			Name: m[1], Version: strings.TrimSpace(m[3]),
			Ecosystem: EcosystemPyPI, Scope: scope, Line: i + 1,
		})
	}
	return out
}

var pyprojectDep = regexp.MustCompile(`^\s*["']?([A-Za-z0-9._-]+)["']?\s*(?:[=<>!~]=?\s*([^"',]*))?`)

func parsePyProject(src string) []Dependency {
	var out []Dependency
	inDeps, dev := false, false
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "["):
			inDeps = false
			dev = strings.Contains(trimmed, "dev") || strings.Contains(trimmed, "test")
			continue
		case strings.HasPrefix(trimmed, "dependencies"):
			inDeps = true
			continue
		}
		if !inDeps || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "]" || trimmed == "}" {
			inDeps = false
			continue
		}
		m := pyprojectDep.FindStringSubmatch(strings.Trim(trimmed, `",`))
		if m == nil || m[1] == "" {
			continue
		}
		scope := ScopeRuntime
		if dev {
			scope = ScopeDev
		}
		out = append(out, Dependency{
			Name: m[1], Version: strings.TrimSpace(m[2]),
			Ecosystem: EcosystemPyPI, Scope: scope, Line: i + 1,
		})
	}
	return out
}

// ---------------------------------------------------------------- Cargo

var cargoDep = regexp.MustCompile(`^([A-Za-z0-9._-]+)\s*=\s*(?:"([^"]*)"|\{[^}]*version\s*=\s*"([^"]*)")`)

func parseCargo(src string) []Dependency {
	var out []Dependency
	section := ""
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section = strings.Trim(trimmed, "[]")
			continue
		}
		if !strings.HasSuffix(section, "dependencies") {
			continue
		}
		m := cargoDep.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		version := m[2]
		if version == "" {
			version = m[3]
		}
		scope := ScopeRuntime
		if strings.Contains(section, "dev") || strings.Contains(section, "build") {
			scope = ScopeDev
		}
		out = append(out, Dependency{
			Name: m[1], Version: version, Ecosystem: EcosystemCargo,
			Scope: scope, Line: i + 1,
		})
	}
	return out
}

// ---------------------------------------------------------------- Gradle

var gradleDep = regexp.MustCompile(
	`^\s*(implementation|api|compileOnly|runtimeOnly|testImplementation|testCompileOnly|annotationProcessor)\s*\(?\s*["']([^"']+)["']`)

func parseGradle(src string) []Dependency {
	var out []Dependency
	for i, line := range strings.Split(src, "\n") {
		m := gradleDep.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		coord := m[2]
		scope := ScopeRuntime
		if strings.HasPrefix(m[1], "test") || m[1] == "compileOnly" ||
			m[1] == "annotationProcessor" {
			scope = ScopeDev
		}
		// group:artifact:version — the version is optional when a BOM supplies it.
		//
		// A blank group or artifact is refused rather than carried. Gradle's
		// syntax admits `implementation ':'` and `implementation ' '`, and
		// neither resolves to anything; taken at face value they produced
		// dependencies named ":" and " ", which match no import under the
		// group-prefix rule in Matches and so appear in an unused-dependency
		// report as artefacts nobody can look up or remove.
		parts := strings.Split(coord, ":")
		name, version := strings.TrimSpace(coord), ""
		if len(parts) >= 2 {
			group, artifact := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			if group == "" || artifact == "" {
				continue
			}
			name = group + ":" + artifact
		}
		if len(parts) >= 3 {
			version = strings.TrimSpace(parts[2])
		}
		if name == "" {
			continue
		}
		out = append(out, Dependency{
			Name: name, Version: version, Ecosystem: EcosystemGradle,
			Scope: scope, Line: i + 1,
		})
	}
	return out
}

// ---------------------------------------------------------------- matching

// Matches reports whether an import path uses a declared dependency.
//
// The rule differs per ecosystem because the relationship between a package
// name and what source writes differs per ecosystem, and getting this wrong in
// either direction is costly: too strict and every dependency looks unused, too
// loose and nothing does.
func Matches(dep Dependency, importPath string) bool {
	if importPath == "" {
		return false
	}
	switch dep.Ecosystem {
	case EcosystemGo:
		// Go imports the module path or a package beneath it.
		// `github.com/go-git/go-git/v5/plumbing` uses `github.com/go-git/go-git/v5`.
		return importPath == dep.Name || strings.HasPrefix(importPath, dep.Name+"/")
	case EcosystemNPM:
		// `import x from "lodash/fp"` uses `lodash`. Scoped packages
		// (`@scope/pkg`) contain a slash themselves, so a prefix test is right
		// for both.
		return importPath == dep.Name || strings.HasPrefix(importPath, dep.Name+"/")
	case EcosystemMaven, EcosystemGradle:
		// Java imports a package, not a coordinate. The group is USUALLY the
		// package prefix — `com.google.guava:guava` is imported as
		// `com.google.common.*`, which it is NOT. So this matches on the group
		// prefix and accepts that some libraries rename their packages; a
		// missed match reports a used dependency as unused, which is why the
		// unused report is worded as a prompt to check rather than a verdict.
		group := dep.Name
		if i := strings.Index(group, ":"); i >= 0 {
			group = group[:i]
		}
		return importPath == group || strings.HasPrefix(importPath, group+".")
	case EcosystemPyPI:
		// PyPI distribution names and import names differ freely
		// (`PyYAML` -> `yaml`, `beautifulsoup4` -> `bs4`). Normalising the
		// separators handles the common case; the rest is why the report is a
		// prompt, not a verdict.
		norm := func(s string) string {
			return strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(s))
		}
		d, imp := norm(dep.Name), norm(importPath)
		return imp == d || strings.HasPrefix(imp, d+"_") ||
			strings.HasPrefix(imp, d+".") || strings.HasPrefix(importPath, dep.Name+".")
	case EcosystemCargo:
		// Rust replaces hyphens with underscores in crate identifiers.
		crate := strings.ReplaceAll(dep.Name, "-", "_")
		root := importPath
		if i := strings.Index(root, "::"); i >= 0 {
			root = root[:i]
		}
		return root == crate || root == dep.Name
	}
	return false
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func lineOfOffset(src string, off int) int {
	if off > len(src) {
		return 0
	}
	return strings.Count(src[:off], "\n") + 1
}
