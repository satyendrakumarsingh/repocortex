package deps

import (
	"strings"
	"testing"
)

// manifestPaths cover every ecosystem ecosystemOf recognises, plus one path it
// must refuse: a file that is not a manifest cannot produce dependencies, or an
// unused-dependency report gains entries from a file nobody declared anything
// in.
var manifestPaths = []string{
	"go.mod",
	"package.json",
	"pom.xml",
	"Cargo.toml",
	"requirements.txt",
	"requirements-dev.txt",
	"build.gradle",
	"pyproject.toml",
	"README.md",
}

// knownEcosystems is the closed set a Manifest may report.
var knownEcosystems = map[Ecosystem]bool{
	EcosystemGo: true, EcosystemNPM: true, EcosystemMaven: true,
	EcosystemPyPI: true, EcosystemCargo: true, EcosystemGradle: true,
}

// FuzzParse hunts the fabricated dependency.
//
// A manifest is written by whoever owns the repository being indexed, and the
// output feeds an unused-dependency report. A Dependency with no name is the
// worst outcome that report can carry: it renders as an empty row, it matches
// no import under any rule in Matches, and it therefore appears as a declared
// dependency that nothing uses — a finding nobody can act on, sitting next to
// findings that are real and pulling trust away from them. Every returned name
// must be something a person could look up.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"module repocortex\n\ngo 1.25\n\nrequire (\n\tgithub.com/spf13/cobra v1.10.2\n\tgolang.org/x/sys v0.47.0 // indirect\n)\n",
		"require github.com/x/y v1.2.3\n",
		`{"name":"app","dependencies":{"react":"^18.0.0"},"devDependencies":{"jest":"29"},"peerDependencies":{"vue":"3"}}`,
		// The degenerate manifests: a declaration whose name is absent or blank.
		`{"dependencies":{"":"1.0.0"}}`,
		`{"peerDependencies":{" ":"1"}}`,
		"<dependency><groupId>com.google.guava</groupId><artifactId>guava</artifactId><version>33</version><scope>test</scope></dependency>",
		"<dependency><groupId> </groupId><artifactId>a</artifactId></dependency>",
		"[dependencies]\nserde = \"1.0\"\ntokio = { version = \"1\", features = [\"full\"] }\n",
		"requests==2.31.0\nflask[async]>=2\n-r other.txt\n# comment\n",
		"[project]\ndependencies = [\n  \"requests>=2\",\n]\n",
		"implementation 'com.google.guava:guava:33.0'\ntestImplementation(\"junit:junit\")\n",
		"implementation ':'\n",
		"implementation ' '\n",
		"",
		"{{{{",
		"<<<<",
	} {
		for i := range manifestPaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := manifestPaths[int(which)%len(manifestPaths)]
		m, ok := Parse(path, []byte(src))

		if !IsManifest(path) && ok {
			t.Fatalf("%q parsed as a manifest", path)
		}
		if !ok {
			// A refusal must be total. A caller that ignores ok and reads the
			// struct anyway — which is what happens the first time this is used
			// in a loop — must not find half a manifest there.
			if len(m.Dependencies) != 0 || m.Module != "" {
				t.Fatalf("ok=false but the manifest carries %d dependencies and module %q", len(m.Dependencies), m.Module)
			}
			return
		}

		if !knownEcosystems[m.Ecosystem] {
			t.Fatalf("ecosystem %q is not one of the declared constants, from %q", m.Ecosystem, path)
		}
		if m.Path != path {
			t.Fatalf("manifest reports path %q, was given %q", m.Path, path)
		}

		lines := strings.Count(src, "\n") + 1
		for _, d := range m.Dependencies {
			if strings.TrimSpace(d.Name) == "" {
				t.Fatalf("dependency with no name: %#v from %q in %q", d, src, path)
			}
			if d.Ecosystem != m.Ecosystem {
				t.Fatalf("dependency ecosystem %q under manifest ecosystem %q: %#v", d.Ecosystem, m.Ecosystem, d)
			}
			if d.Scope != ScopeRuntime && d.Scope != ScopeDev {
				t.Fatalf("scope %q is neither runtime nor dev: %#v", d.Scope, d)
			}
			// Line 0 means "not located", which the npm reader really can
			// return for a key whose source spelling used an escape. A line past
			// the end of the file is different: it cites a location that is not
			// there.
			if d.Line < 0 || d.Line > lines {
				t.Fatalf("line %d outside 0..%d: %#v from %q", d.Line, lines, d, src)
			}
			// Matches is the whole point of having a name. A dependency that
			// cannot match its own name would be reported unused however many
			// times the repository imports it.
			if !Matches(d, d.Name) && d.Ecosystem != EcosystemMaven && d.Ecosystem != EcosystemGradle {
				t.Fatalf("dependency does not match its own name: %#v", d)
			}
		}
	})
}
