package deps

import (
	"os"
	"strings"
	"testing"
)

func TestParseGoMod(t *testing.T) {
	m, ok := Parse("go.mod", []byte(`module repocortex

go 1.25.0

require (
	github.com/fsnotify/fsnotify v1.10.1
	github.com/spf13/cobra v1.10.2
	golang.org/x/sys v0.30.0 // indirect
)

require github.com/stretchr/testify v1.9.0
`))
	if !ok {
		t.Fatal("go.mod not recognised")
	}
	if m.Module != "repocortex" {
		t.Errorf("module = %q, want repocortex", m.Module)
	}
	byName := index(m.Dependencies)
	if byName["github.com/spf13/cobra"].Version != "v1.10.2" {
		t.Errorf("cobra version = %q", byName["github.com/spf13/cobra"].Version)
	}
	// A single-line require outside the block must be read too.
	if _, ok := byName["github.com/stretchr/testify"]; !ok {
		t.Errorf("single-line require missed: %v", names(m.Dependencies))
	}
	// Go's own marker says no first-party file imports it. Flagging it as
	// unused would report a fact the manifest already states.
	if !byName["golang.org/x/sys"].Indirect {
		t.Error("// indirect not honoured")
	}
}

func TestParseGoModOnThisRepository(t *testing.T) {
	src, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Skipf("go.mod unavailable: %v", err)
	}
	m, ok := Parse("go.mod", src)
	if !ok {
		t.Fatal("this repository's own go.mod was not parsed")
	}
	// Cross-checked against a naive independent read of the same file rather
	// than against a literal. The literal was "repocortex", and renaming the
	// module to a resolvable path broke this test — which told us nothing about
	// the parser, only that someone had written the answer down twice.
	//
	// This is still a real assertion and not a tautology: the reference read
	// below is a different implementation from Parse, so the two agreeing means
	// Parse extracted the module line correctly.
	if want := firstModuleLine(t, src); m.Module != want {
		t.Errorf("module = %q, want %q", m.Module, want)
	}
	if m.Module == "" {
		t.Error("module is empty; this repository's go.mod declares one")
	}
	byName := index(m.Dependencies)
	for _, want := range []string{
		"github.com/spf13/cobra", "github.com/fsnotify/fsnotify", "modernc.org/sqlite",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing %q from a real go.mod", want)
		}
	}
}

func TestParsePackageJSON(t *testing.T) {
	m, ok := Parse("package.json", []byte(`{
  "name": "web",
  "dependencies": { "react": "^18.2.0", "@scope/lib": "1.0.0" },
  "devDependencies": { "typescript": "^5.0.0" },
  "peerDependencies": { "react-dom": "^18.0.0" }
}`))
	if !ok {
		t.Fatal("package.json not recognised")
	}
	byName := index(m.Dependencies)
	if byName["react"].Scope != ScopeRuntime {
		t.Errorf("react scope = %q", byName["react"].Scope)
	}
	if byName["typescript"].Scope != ScopeDev {
		t.Errorf("typescript scope = %q", byName["typescript"].Scope)
	}
	// A peer dependency is supplied by the consumer, so the project genuinely
	// does not import it; flagging every one as unused would be noise.
	if !byName["react-dom"].Indirect {
		t.Error("peer dependency not marked indirect")
	}
	if byName["@scope/lib"].Version != "1.0.0" {
		t.Errorf("scoped package mishandled: %+v", byName["@scope/lib"])
	}
}

func TestParseOtherEcosystems(t *testing.T) {
	pom, ok := Parse("pom.xml", []byte(`<project>
  <dependencies>
    <dependency>
      <groupId>org.springframework.boot</groupId>
      <artifactId>spring-boot-starter-web</artifactId>
      <version>3.2.0</version>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
      <scope>test</scope>
    </dependency>
  </dependencies>
</project>`))
	if !ok {
		t.Fatal("pom.xml not recognised")
	}
	byName := index(pom.Dependencies)
	if byName["org.springframework.boot:spring-boot-starter-web"].Version != "3.2.0" {
		t.Errorf("maven version missed: %v", pom.Dependencies)
	}
	if byName["org.junit.jupiter:junit-jupiter"].Scope != ScopeDev {
		t.Error("<scope>test</scope> not mapped to dev")
	}

	req, ok := Parse("requirements.txt", []byte("requests==2.31.0\nflask>=2.0\n# a comment\n-r other.txt\n"))
	if !ok {
		t.Fatal("requirements.txt not recognised")
	}
	if len(req.Dependencies) != 2 {
		t.Errorf("requirements: got %v, want requests and flask only", names(req.Dependencies))
	}

	cargo, ok := Parse("Cargo.toml", []byte("[dependencies]\nserde = \"1.0\"\ntokio = { version = \"1.35\", features = [\"full\"] }\n\n[dev-dependencies]\ncriterion = \"0.5\"\n"))
	if !ok {
		t.Fatal("Cargo.toml not recognised")
	}
	byName = index(cargo.Dependencies)
	if byName["tokio"].Version != "1.35" {
		t.Errorf("cargo table form mishandled: %+v", byName["tokio"])
	}
	if byName["criterion"].Scope != ScopeDev {
		t.Error("dev-dependencies not mapped to dev scope")
	}

	gradle, ok := Parse("build.gradle", []byte(`dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:3.2.0'
    testImplementation "org.junit.jupiter:junit-jupiter"
}`))
	if !ok {
		t.Fatal("build.gradle not recognised")
	}
	byName = index(gradle.Dependencies)
	if byName["org.springframework.boot:spring-boot-starter-web"].Version != "3.2.0" {
		t.Errorf("gradle coordinate mishandled: %v", gradle.Dependencies)
	}
	if byName["org.junit.jupiter:junit-jupiter"].Scope != ScopeDev {
		t.Error("testImplementation not mapped to dev")
	}
}

// TestMatchesIsEcosystemAware. Getting this wrong in either direction is
// costly: too strict and every dependency looks unused, too loose and none does.
func TestMatchesIsEcosystemAware(t *testing.T) {
	cases := []struct {
		dep   Dependency
		imp   string
		match bool
	}{
		// Go imports the module path or a package beneath it.
		{Dependency{Name: "github.com/spf13/cobra", Ecosystem: EcosystemGo}, "github.com/spf13/cobra", true},
		{Dependency{Name: "github.com/go-git/go-git/v5", Ecosystem: EcosystemGo}, "github.com/go-git/go-git/v5/plumbing", true},
		// A prefix that is not a path boundary must NOT match.
		{Dependency{Name: "github.com/spf13/cob", Ecosystem: EcosystemGo}, "github.com/spf13/cobra", false},
		{Dependency{Name: "github.com/spf13/cobra", Ecosystem: EcosystemGo}, "github.com/spf13/pflag", false},

		{Dependency{Name: "lodash", Ecosystem: EcosystemNPM}, "lodash/fp", true},
		{Dependency{Name: "@scope/lib", Ecosystem: EcosystemNPM}, "@scope/lib", true},
		{Dependency{Name: "react", Ecosystem: EcosystemNPM}, "react-dom", false},

		{Dependency{Name: "org.springframework.boot:spring-boot-starter-web", Ecosystem: EcosystemMaven},
			"org.springframework.boot.autoconfigure.SpringBootApplication", true},
		{Dependency{Name: "org.junit.jupiter:junit-jupiter", Ecosystem: EcosystemMaven}, "org.mockito.Mock", false},

		{Dependency{Name: "PyYAML", Ecosystem: EcosystemPyPI}, "pyyaml", true},
		{Dependency{Name: "google-cloud-storage", Ecosystem: EcosystemPyPI}, "google_cloud_storage", true},

		{Dependency{Name: "serde-json", Ecosystem: EcosystemCargo}, "serde_json::Value", true},
	}
	for _, c := range cases {
		if got := Matches(c.dep, c.imp); got != c.match {
			t.Errorf("Matches(%s[%s], %q) = %v, want %v",
				c.dep.Name, c.dep.Ecosystem, c.imp, got, c.match)
		}
	}
}

func TestIsManifest(t *testing.T) {
	for _, p := range []string{"go.mod", "package.json", "svc/pom.xml", "Cargo.toml",
		"requirements.txt", "build.gradle", "pyproject.toml"} {
		if !IsManifest(p) {
			t.Errorf("%s not recognised as a manifest", p)
		}
	}
	for _, p := range []string{"go.sum", "package-lock.json", "main.go", "README.md", "Cargo.lock"} {
		if IsManifest(p) {
			t.Errorf("%s wrongly treated as a manifest", p)
		}
	}
}

func TestSafeOnHostileInput(t *testing.T) {
	for _, src := range []string{
		strings.Repeat("<dependency>", 5000),
		strings.Repeat("a = \"1\"\n", 20000),
		strings.Repeat("{", 10000),
		"\x00\x01\x02",
	} {
		for _, path := range []string{"go.mod", "package.json", "pom.xml", "Cargo.toml", "requirements.txt"} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on %s: %v", path, r)
					}
				}()
				_, _ = Parse(path, []byte(src))
			}()
		}
	}
}

func index(deps []Dependency) map[string]Dependency {
	out := make(map[string]Dependency, len(deps))
	for _, d := range deps {
		out[d.Name] = d
	}
	return out
}

func names(deps []Dependency) []string {
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		out = append(out, d.Name)
	}
	return out
}

// firstModuleLine reads the module path the naive way, as a reference for the
// parser under test to be compared against.
func firstModuleLine(t *testing.T, src []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}
