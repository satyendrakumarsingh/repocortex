package config

import (
	"strings"
	"testing"
)

// declPaths cover every format formatOf recognises, plus one it must refuse.
var declPaths = []string{
	".env",
	"config/app.yml",
	"config/app.json",
	"Cargo.toml",
	"src/main/resources/application.properties",
	"internal/app/main.go",
}

// usagePaths cover the languages the usage patterns are written for, plus a
// configuration file, which must never be scanned for usages.
var usagePaths = []string{
	"internal/app/main.go",
	"src/App.java",
	"app/main.py",
	"src/index.ts",
	".env",
}

// declSeeds are documents whose shapes the two scanners disagree about most:
// unterminated strings, delimiters inside names, and section headers that are
// not headers.
var declSeeds = []string{
	"DATABASE_URL=postgres://u:p@h/db\nAPI_KEY=abc\n",
	"export TOKEN=xyz\n# comment\n! also a comment\n",
	"database:\n  pool:\n    size: 10\n  password: hunter2\n",
	"a:\n - b: 1\n - c: 2\n",
	`{"database":{"password":"hunter2","pool":{"size":10}}}`,
	`{"a":{"b":1,"c":2}}`,
	`{"unterminated`,
	"[server]\nport = 8080\npassword = \"hunter2\"\n",
	"[a] = \"hunter2\"\nb = 1\n",
	"[[array.of.tables]]\nkey = 1\n",
	"::::",
	"",
}

// FuzzDeclarations hunts two things: a scanner that walks off the end of a
// malformed document, and a KEY NAME THAT IS NOT A KEY NAME.
//
// The second is the one that matters. Spec §24 is implemented here as an
// absence — there is no field to hold a value — but an absence is only as good
// as the point where each scanner stops reading. Every name this package emits
// has passed plausibleKey on its way out, so a returned name that FAILS
// plausibleKey means text from somewhere past the delimiter was concatenated
// into it, and the thing past the delimiter is the value. That is the shape a
// leak takes here, and it is why the check below is on the whole reported path
// rather than on the last segment.
func FuzzDeclarations(f *testing.F) {
	for _, s := range declSeeds {
		for i := range declPaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := declPaths[int(which)%len(declPaths)]
		keys := Declarations(path, []byte(src))
		if !IsConfigFile(path) && len(keys) != 0 {
			t.Fatalf("%d keys from %q, which is not a configuration file", len(keys), path)
		}

		lines := strings.Count(src, "\n") + 1
		for _, k := range keys {
			if k.Name == "" {
				t.Fatalf("empty key name from %q in %q", src, path)
			}
			// The load-bearing assertion: see the doc comment.
			if !plausibleKey(k.Name) {
				t.Fatalf("key %q is not a plausible key name, so it carries text from past the delimiter; from %q in %q", k.Name, src, path)
			}
			if k.Line < 1 || k.Line > lines {
				t.Fatalf("key %q on line %d, outside 1..%d, from %q", k.Name, k.Line, lines, src)
			}
			if k.Secret != IsSecretName(k.Name) {
				t.Fatalf("key %q labelled Secret=%v against its own name", k.Name, k.Secret)
			}
			// Nesting is only readable from the dotted path when no name can
			// contribute a dot of its own, so the bound is asserted on exactly
			// the documents where the reading is sound. plausibleKey admits `.`
			// inside a single name, and a flat file of dotted names would
			// otherwise look thirty levels deep.
			if !strings.Contains(src, ".") {
				if depth := strings.Count(k.Name, ".") + 1; depth > maxNesting {
					t.Fatalf("key %q nests %d levels, over the %d limit, from %q", k.Name, depth, maxNesting, src)
				}
			}
		}
		// The count bound is what keeps a generated document from turning a
		// linear scan into a quadratic one; the formats that declare it must
		// honour it.
		switch formatOf(path) {
		case formatYAML, formatJSON:
			if len(keys) > maxKeys {
				t.Fatalf("%d keys, over the %d limit", len(keys), maxKeys)
			}
		}
	})
}

// FuzzUsages hunts a lookup site reported with a key nobody could look up.
//
// A Usage names a setting the code reads, and the report puts it next to the
// declarations. A name that plausibleKey would refuse never matches a
// declaration, so it appears as a setting read by code and declared nowhere —
// which reads as a missing configuration entry and sends someone to add one.
func FuzzUsages(f *testing.F) {
	for _, s := range []string{
		`v := os.Getenv("DATABASE_URL")`,
		`viper.GetString("server.port")`,
		"@Value(\"${payment.api.key:default}\")",
		`System.getenv("TOKEN")`,
		`env.getProperty("spring.datasource.url")`,
		`os.environ["SECRET"]`,
		`os.environ.get('SECRET')`,
		`os.getenv("SECRET")`,
		"process.env.API_KEY",
		`process.env["API_KEY"]`,
		`config.Get("feature.flag")`,
		`os.Getenv("")`,
		`os.Getenv(" ")`,
		"",
	} {
		for i := range usagePaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := usagePaths[int(which)%len(usagePaths)]
		usages := Usages(path, []byte(src))
		if IsConfigFile(path) && len(usages) != 0 {
			t.Fatalf("%d usages from %q: a configuration file declares, it does not read", len(usages), path)
		}

		lines := strings.Count(src, "\n") + 1
		for _, u := range usages {
			if u.Name == "" || !plausibleKey(u.Name) {
				t.Fatalf("usage %q is not a key anything could declare, from %q in %q", u.Name, src, path)
			}
			// Mechanism is how a report tells a process environment variable
			// from an application setting; an unlabelled usage collapses that
			// distinction silently.
			if u.Mechanism == "" {
				t.Fatalf("usage %q with no mechanism, from %q", u.Name, src)
			}
			if u.Line < 1 || u.Line > lines {
				t.Fatalf("usage %q on line %d, outside 1..%d, from %q", u.Name, u.Line, lines, src)
			}
		}
	})
}

// canary is a value no key name can legitimately contain. It stands in for a
// production password: if it ever comes back inside a Key.Name, this package
// has read past a delimiter, and the thing it read past a delimiter in a real
// repository is a credential.
const canary = "SUPERSECRETCANARY"

// FuzzDeclarationValues hunts the §24 leak directly.
//
// FuzzDeclarations infers a leak from the SHAPE of a returned name. This target
// proves it: the document is well formed, the key names are fixed and known, and
// only the VALUE varies. Whatever the value contains — delimiters, brackets,
// quotes, another assignment — the set of names that may come back is closed,
// because the scanners are supposed to stop at the delimiter and nothing past it
// is supposed to have a route into the output.
//
// Values containing a line break are skipped rather than tested: a newline ends
// the record in every one of these formats, so the bytes after it genuinely ARE
// a new declaration and reporting them is correct behaviour, not a leak.
func FuzzDeclarationValues(f *testing.F) {
	for _, v := range []string{
		"hunter2",
		`"quoted"`,
		"a=b",
		"a:b",
		"] = x",
		"]\nkey2 = ",
		"{}[]",
		"#comment",
		"",
		" ",
	} {
		for i := 0; i < 5; i++ {
			f.Add(uint8(i), v)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, value string) {
		if strings.ContainsAny(value, "\r\n") {
			return
		}
		path, doc := valueDoc(int(which)%5, value)
		for _, k := range Declarations(path, []byte(doc)) {
			if strings.Contains(k.Name, canary) {
				t.Fatalf("VALUE LEAKED into key %q: value %q, document %q", k.Name, value, doc)
			}
			switch k.Name {
			case "parent", "leaf", "parent.leaf":
			default:
				t.Fatalf("key %q is not one of the names the document declares; value %q, document %q", k.Name, value, doc)
			}
		}
	})
}

// valueDoc builds a well-formed document of one format whose only value is the
// canary followed by the fuzzed text.
func valueDoc(which int, value string) (path, doc string) {
	v := canary + value
	switch which {
	case 0:
		return ".env", "leaf=" + v + "\n"
	case 1:
		return "app.yml", "parent:\n  leaf: " + v + "\n"
	case 2:
		return "app.json", `{"parent":{"leaf":` + quoteJSON(v) + "}}"
	case 3:
		return "app.toml", "[parent]\nleaf = \"" + strings.ReplaceAll(v, `"`, "") + "\"\n"
	default:
		return "app.properties", "leaf: " + v + "\n"
	}
}

// quoteJSON escapes just enough to keep the document parseable, without
// encoding/json — the point is to vary the value, not to hand the scanner a
// pre-sanitised one.
func quoteJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
