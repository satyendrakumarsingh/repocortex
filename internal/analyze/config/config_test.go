package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestNoValueFieldExists is the security test, and it is a structural one.
//
// Spec §24 requires that secret values are never exposed. This package
// implements that as an ABSENCE rather than a filter: there is nowhere to put a
// value. A redaction list can be wrong about a key nobody anticipated; a struct
// with no field for a value cannot leak one.
//
// Asserted by reflection so the guarantee survives a future field being added
// with good intentions.
func TestNoValueFieldExists(t *testing.T) {
	for _, typ := range []any{Key{}, Usage{}} {
		rt := reflect.TypeOf(typ)
		for i := 0; i < rt.NumField(); i++ {
			name := strings.ToLower(rt.Field(i).Name)
			for _, banned := range []string{"value", "val", "content", "data", "raw"} {
				if name == banned {
					t.Fatalf("%s has a %q field: this package must never hold a "+
						"configuration value", rt.Name(), rt.Field(i).Name)
				}
			}
		}
	}
}

// TestValuesNeverAppearInOutput drives the guarantee end to end: a file full of
// real-looking credentials must produce key names and nothing else.
func TestValuesNeverAppearInOutput(t *testing.T) {
	src := []byte(`DATABASE_PASSWORD=hunter2-super-secret
API_KEY=sk_live_51H8xQpKj3nMvB2wZ
AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
PORT=8080`)

	keys := Declarations(".env", src)
	if len(keys) != 4 {
		t.Fatalf("got %d keys, want 4: %+v", len(keys), keys)
	}
	rendered := ""
	for _, k := range keys {
		rendered += k.Name + " "
	}
	for _, secret := range []string{
		"hunter2", "sk_live_51H8xQpKj3nMvB2wZ", "wJalrXUtnFEMI", "8080",
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("a configuration VALUE reached the output: %q", secret)
		}
	}

	// The sensitive ones must be labelled, without anything having been read.
	byName := map[string]Key{}
	for _, k := range keys {
		byName[k.Name] = k
	}
	for _, name := range []string{"DATABASE_PASSWORD", "API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if !byName[name].Secret {
			t.Errorf("%s not marked sensitive", name)
		}
	}
	if byName["PORT"].Secret {
		t.Error("PORT marked sensitive; the label should mean something")
	}
}

func TestDeclarationsYAMLProducesDottedPaths(t *testing.T) {
	src := []byte(`database:
  url: postgres://localhost/app
  pool:
    size: 10
  password: s3cr3t
server:
  port: 8080
`)
	keys := Declarations("application.yml", src)
	want := []string{"database", "database.url", "database.pool", "database.pool.size",
		"database.password", "server", "server.port"}
	got := names(keys)
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("missing key %q; got %v", w, got)
		}
	}
	for _, k := range keys {
		if k.Name == "database.password" && !k.Secret {
			t.Error("database.password not marked sensitive")
		}
	}
	// No value may appear anywhere.
	for _, k := range keys {
		if strings.Contains(k.Name, "postgres") || strings.Contains(k.Name, "s3cr3t") {
			t.Errorf("a value leaked into a key name: %q", k.Name)
		}
	}
}

func TestDeclarationsJSONAndTOML(t *testing.T) {
	json := Declarations("config.json", []byte(`{
  "database": { "url": "postgres://x", "poolSize": 10 },
  "featureFlags": { "newCheckout": true }
}`))
	for _, want := range []string{"database", "database.url", "database.poolSize", "featureFlags.newCheckout"} {
		if !contains(names(json), want) {
			t.Errorf("json: missing %q; got %v", want, names(json))
		}
	}
	for _, k := range json {
		if strings.Contains(k.Name, "postgres") {
			t.Errorf("a JSON value became a key: %q", k.Name)
		}
	}

	toml := Declarations("config.toml", []byte("[database]\nurl = \"postgres://x\"\npool_size = 10\n"))
	for _, want := range []string{"database.url", "database.pool_size"} {
		if !contains(names(toml), want) {
			t.Errorf("toml: missing %q; got %v", want, names(toml))
		}
	}
}

// TestBuildJSONIsNotConfiguration. Treating package.json or tsconfig.json as
// application configuration fills the report with keys like
// compilerOptions.strict that no runtime lookup will ever match.
func TestBuildJSONIsNotConfiguration(t *testing.T) {
	for _, path := range []string{"package.json", "tsconfig.json", "package-lock.json"} {
		if IsConfigFile(path) {
			t.Errorf("%s treated as application configuration", path)
		}
	}
	if !IsConfigFile("config.json") || !IsConfigFile(".env") ||
		!IsConfigFile("application.yml") || !IsConfigFile("app.properties") {
		t.Error("a real configuration file was not recognised")
	}
}

func TestUsagesAcrossLanguages(t *testing.T) {
	cases := []struct {
		path, src, want, mechanism string
	}{
		{"main.go", `port := os.Getenv("SERVER_PORT")`, "SERVER_PORT", "env"},
		{"main.go", `v := viper.GetString("database.url")`, "database.url", "viper"},
		{"C.java", `@Value("${database.url}") private String url;`, "database.url", "spring-value"},
		{"C.java", `String s = System.getenv("API_KEY");`, "API_KEY", "env"},
		{"a.py", `url = os.environ["DATABASE_URL"]`, "DATABASE_URL", "env"},
		{"a.py", `url = os.getenv("DATABASE_URL")`, "DATABASE_URL", "env"},
		{"a.js", `const port = process.env.PORT;`, "PORT", "env"},
		{"a.ts", `const k = process.env["API_KEY"];`, "API_KEY", "env"},
	}
	for _, c := range cases {
		got := Usages(c.path, []byte(c.src))
		if len(got) == 0 {
			t.Errorf("%s: no usage found in %q", c.path, c.src)
			continue
		}
		var found bool
		for _, u := range got {
			if u.Name == c.want && u.Mechanism == c.mechanism {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %s via %s, got %+v", c.path, c.want, c.mechanism, got)
		}
	}
}

// TestConfigFilesDeclareButDoNotConsume. Scanning a .env for usage patterns
// would report the file as its own consumer, which is nonsense.
func TestConfigFilesDeclareButDoNotConsume(t *testing.T) {
	if u := Usages(".env", []byte("FOO=os.Getenv(\"BAR\")")); len(u) != 0 {
		t.Errorf("a config file reported usages: %+v", u)
	}
	if u := Usages("README.md", []byte(`os.Getenv("FOO")`)); len(u) != 0 {
		t.Errorf("a non-source file reported usages: %+v", u)
	}
}

func TestSafeOnHostileInput(t *testing.T) {
	for _, src := range []string{
		strings.Repeat("{", 20000),
		strings.Repeat(`"a":`, 20000),
		strings.Repeat("  ", 50000) + "k: v",
		strings.Repeat("\"", 20000),
		"\x00\x01\x02",
	} {
		for _, path := range []string{".env", "a.yml", "a.json", "a.toml", "a.go"} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on %s: %v", path, r)
					}
				}()
				_ = Declarations(path, []byte(src))
				_ = Usages(path, []byte(src))
			}()
		}
	}
}

func names(keys []Key) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Name)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestCommentedCodeIsNotAConfigRead pins false positives found on a real Go
// service.
//
// The scanner is line-based and regex-driven, so a comment describing an env
// read looked identical to one. The reported case produced a configuration key
// literally named "...", attributed to a line that executes nothing:
//
//	// (mirrors os.Getenv("...", "10000")-style defaulting used across …
func TestCommentedCodeIsNotAConfigRead(t *testing.T) {
	src := []byte(`package db

// envOrDefault reads a string env var, falling back to def when unset (mirrors
// os.Getenv("...", "10000")-style defaulting used across the Python services;
func envOrDefault(key, def string) string {
	if v := os.Getenv("REAL_KEY"); v != "" {
		return v
	}
	# os.Getenv("HASH_COMMENTED")
	return def
}

func withURL() string {
	// a marker inside a literal must not truncate the line
	return os.Getenv("HAS_//_IN_DEFAULT")
}
`)
	got := map[string]bool{}
	for _, u := range Usages("pool.go", src) {
		got[u.Name] = true
	}

	if got["..."] {
		t.Error(`a key named "..." was read from a comment`)
	}
	if got["HASH_COMMENTED"] {
		t.Error("a key was read from a # comment")
	}
	if !got["REAL_KEY"] {
		t.Error("REAL_KEY was lost; stripping comments must not remove real call sites")
	}
	if !got["HAS_//_IN_DEFAULT"] {
		t.Error("a // inside a string literal truncated the line")
	}
}

// TestPlausibleKeyRejectsPurePunctuation pins the narrower rule directly.
func TestPlausibleKeyRejectsPurePunctuation(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"DATABASE_URL", true},
		{"spring.datasource.url", true},
		{"a", true},
		{"...", false},
		{"--", false},
		{"//", false},
		{"", false},
	} {
		if got := plausibleKey(tc.in); got != tc.want {
			t.Errorf("plausibleKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
