package infra

import (
	"encoding/json"
	"strings"
	"testing"
)

// manifestPaths cover every class classify recognises, plus one it must refuse:
// a file that is not a manifest cannot produce services, or a deployment report
// gains units nobody declared.
var manifestPaths = []string{
	"Dockerfile",
	"docker-compose.yml",
	"k8s/deployment.yaml",
	"charts/api/Chart.yaml",
	"charts/api/values.yaml",
	"charts/api/templates/deployment.yaml",
	"infra/main.tf",
	"README.md",
}

var knownFormats = map[Format]bool{
	FormatDockerfile: true, FormatCompose: true, FormatKubernetes: true,
	FormatHelm: true, FormatTerraform: true,
}

var knownSources = map[EnvSource]bool{
	SourceLiteral: true, SourceSecret: true, SourceConfigMap: true,
	SourceDownward: true, SourceHost: true, SourceBuildArg: true, SourceImage: true,
}

var fuzzSeeds = []string{
	dockerfileSrc,
	composeSrc,
	k8sSrc,
	terraformSrc,
	"FROM\nENV\nARG\nEXPOSE\nENTRYPOINT\n",
	"# escape=`\nFROM a `\n",
	"FROM a \\",
	"ENV =value\nENV  =\nARG =\n",
	// YAML shapes that have historically broken node walkers: an alias cycle,
	// a merge key pointing at a scalar, a document that is a scalar, and a
	// container list whose items are not mappings.
	"a: &x\n  <<: *x\n",
	"apiVersion: v1\nkind: Pod\nspec: &s\n  containers: *s\n",
	"apiVersion: v1\nkind: Pod\nspec:\n  containers:\n    - just a string\n",
	"apiVersion: v1\nkind: Secret\ndata: notamap\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: []\ndata:\n  a: b\n",
	"--- \n--- \n--- \n",
	"services: notamap\n",
	"services:\n  api:\n    ports: [\"a:b:c:d\", \"-1:70000\", \"::\"]\n",
	"services:\n  api:\n    environment: [\"=v\", \"\", \"A=B=C\"]\n",
	// Terraform shapes where brace tracking can drift.
	"resource \"a\" \"b\" {\n  x = \"}\"\n}\n",
	"resource \"a\" \"b\" {\n  p = <<EOT\n}}}}\nEOT\n}\n",
	"# }\nresource \"a\" \"b\" {}\n",
	"/* multi\nline } comment */\nmodule \"m\" {}\n",
	"}\n}\nresource \"a\" \"b\" {}\n",
	"variable \"\" {}\nvariable {}\n",
	"a << b\n",
	"",
	"{{{{",
	"\x00\x00\x00",
}

// FuzzParse hunts three things: a scanner that walks off the end of a malformed
// manifest, a unit or key that cites a location which does not exist, and — the
// one that matters — A KEY NAME THAT IS NOT A KEY NAME.
//
// The third is how a leak looks here. Every name this package emits has passed
// cleanName on its way out, so a returned name that fails plausibleName means a
// scanner concatenated text from past a delimiter into it, and the thing past
// the delimiter is the value. The one legitimate exception is a name the
// redactor replaced, which is checked for explicitly rather than tolerated by a
// loose assertion.
func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		for i := range manifestPaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := manifestPaths[int(which)%len(manifestPaths)]
		file, ok := Parse(path, []byte(src))

		if !Candidate(path) && ok {
			t.Fatalf("%q parsed as a manifest", path)
		}
		if !ok {
			// A refusal must be total. A caller that ignores ok and reads the
			// struct anyway must not find half a manifest there.
			if len(file.Units) != 0 || len(file.KeySets) != 0 || len(file.Problems) != 0 {
				t.Fatalf("ok=false but the file carries %d units, %d key sets, %d problems",
					len(file.Units), len(file.KeySets), len(file.Problems))
			}
			return
		}

		if !knownFormats[file.Format] {
			t.Fatalf("format %q is not one of the declared constants, from %q", file.Format, path)
		}
		if file.Path != path {
			t.Fatalf("file reports path %q, was given %q", file.Path, path)
		}
		if len(file.Units) > maxUnits {
			t.Fatalf("%d units, over the %d bound", len(file.Units), maxUnits)
		}

		lines := strings.Count(src, "\n") + 1
		checkLine := func(what string, line int) {
			if line < 0 || line > lines {
				t.Fatalf("%s cites line %d, outside 0..%d, from %q", what, line, lines, src)
			}
		}

		for _, p := range file.Problems {
			if p.Reason == "" {
				t.Fatalf("a problem with no reason is a report nobody can act on: %#v", p)
			}
			checkLine("problem "+p.Reason, p.Line)
		}

		for _, u := range file.Units {
			if strings.TrimSpace(u.Name) == "" {
				t.Fatalf("unit with no name: %#v from %q in %q", u, src, path)
			}
			if strings.TrimSpace(u.Type) == "" {
				t.Fatalf("unit with no type: %#v", u)
			}
			checkLine("unit "+u.Name, u.Line)
			if len(u.Env) > maxKeys {
				t.Fatalf("%d env keys on one unit, over the %d bound", len(u.Env), maxKeys)
			}
			for _, k := range u.Env {
				checkEnvKey(t, k, lines, src, path)
			}
			for _, ref := range u.EnvFrom {
				if ref.Kind != "Secret" && ref.Kind != "ConfigMap" {
					t.Fatalf("envFrom kind %q is not one of the two: %#v", ref.Kind, ref)
				}
				if strings.TrimSpace(ref.Name) == "" {
					t.Fatalf("envFrom with no name: %#v", ref)
				}
				checkLine("envFrom "+ref.Name, ref.Line)
			}
			for _, p := range u.Ports {
				if p.Container == 0 && p.Published == 0 && p.Raw == "" {
					t.Fatalf("a port that names no port at all: %#v in %#v", p, u)
				}
				if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
					t.Fatalf("protocol %q: %#v", p.Protocol, p)
				}
				// A port is persisted with its own line now, so a line that
				// does not exist becomes a row pointing at nothing rather than
				// a cosmetic defect in one report.
				checkLine("port on "+u.Name, p.Line)
			}
		}

		for _, ks := range file.KeySets {
			if strings.TrimSpace(ks.Name) == "" {
				t.Fatalf("key set with no name: %#v", ks)
			}
			if len(ks.Keys) > maxKeys {
				t.Fatalf("%d keys in one set, over the %d bound", len(ks.Keys), maxKeys)
			}
			checkLine("key set "+ks.Name, ks.Line)
			for _, k := range ks.Keys {
				checkEnvKey(t, k, lines, src, path)
			}
		}

		// Summarize runs over whatever Parse produced; a report that panics on a
		// hostile manifest is the same outage as a parser that does.
		_ = Summarize([]File{file})
	})
}

func checkEnvKey(t *testing.T, k EnvKey, lines int, src, path string) {
	t.Helper()
	if k.Name == "" {
		t.Fatalf("empty key name from %q in %q", src, path)
	}
	// The load-bearing assertion. See the doc comment on FuzzParse.
	if !plausibleName(k.Name) && !IsRedacted(k.Name) {
		t.Fatalf("key %q is not a plausible name, so it carries text from past the delimiter; from %q in %q",
			k.Name, src, path)
	}
	if !knownSources[k.From] {
		t.Fatalf("env source %q is not one of the declared constants: %#v", k.From, k)
	}
	if k.From == SourceSecret && !k.Secret {
		t.Fatalf("a key out of a Secret is sensitive whatever it is called: %#v", k)
	}
	if k.Line < 0 || k.Line > lines {
		t.Fatalf("key %q cites line %d, outside 0..%d, from %q", k.Name, k.Line, lines, src)
	}
}

// FuzzParseNeverEmitsValue is the security property, stated as a property.
//
// The KEY is fuzzed and the VALUE is a fixed sentinel, planted on the right of
// the delimiter in each format. Whatever the fuzzer does to the key, the
// sentinel must never appear in the serialised result — which is the exact
// claim `cortex infra` makes to its users, tested rather than asserted.
//
// The key is filtered to characters that cannot restructure the document, so
// the fuzzer explores name handling without being able to move the sentinel to
// the left of a delimiter — where it would legitimately BE a key, and the
// failure would be the test's rather than the parser's.
func FuzzParseNeverEmitsValue(f *testing.F) {
	for _, seed := range []string{
		"DB_PASSWORD", "a", "", "....", "x-y_z", "AKIAIOSFODNN7EXAMPLE",
		strings.Repeat("k", 300), "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		f.Add(seed)
	}

	const sentinel = "hunter2-super-secret-sentinel"

	f.Fuzz(func(t *testing.T, rawKey string) {
		key := keyish(rawKey)
		if key == "" {
			return
		}

		for _, c := range []struct{ path, src string }{
			{"Dockerfile", "FROM alpine\nENV " + key + "=" + sentinel + "\nARG " + key + "2=" + sentinel + "\n"},
			{"docker-compose.yml", "services:\n  api:\n    image: a\n    environment:\n      " +
				key + ": " + sentinel + "\n"},
			{"docker-compose.yml", "services:\n  api:\n    image: a\n    environment:\n      - " +
				key + "=" + sentinel + "\n"},
			{"k8s/d.yaml", "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n" +
				"    - name: c\n      image: i\n      env:\n        - name: " + key +
				"\n          value: " + sentinel + "\n"},
			{"k8s/s.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  " +
				key + ": " + sentinel + "\n"},
			{"charts/api/values.yaml", key + ": " + sentinel + "\n"},
			{"infra/main.tf", "resource \"aws_db_instance\" \"main\" {\n  " + key + " = \"" + sentinel + "\"\n}\n"},
		} {
			file, ok := Parse(c.path, []byte(c.src))
			if !ok {
				continue
			}
			blob, err := json.Marshal(file)
			if err != nil {
				t.Fatalf("%s: marshal: %v", c.path, err)
			}
			if strings.Contains(string(blob), sentinel) {
				t.Fatalf("%s LEAKED the value with key %q:\n%s", c.path, key, blob)
			}
		}
	})
}

// keyish reduces a fuzzed string to something that can only ever be a key: no
// newline, quote, colon, brace or backslash, so the document's shape is fixed
// and only the name varies.
func keyish(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_', r == '-', r == '.':
			b.WriteRune(r)
		}
		if b.Len() >= 300 {
			break
		}
	}
	return b.String()
}
