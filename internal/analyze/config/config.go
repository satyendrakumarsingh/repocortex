// Package config finds configuration keys: what a service can be configured
// with, and which code reads each setting.
//
// # VALUES ARE NEVER READ, NEVER RETURNED, NEVER STORED
//
// Spec §24 requires that secret values are never exposed. This package does not
// implement that as a filter — it implements it as an absence. Nothing here
// returns a value, no struct below has a field to hold one, and the scanners
// stop at the delimiter. A redaction list can be wrong about a key nobody
// anticipated (`ACME_SPECIAL_SAUCE` holding a signing key); a data structure
// with nowhere to put a value cannot leak one.
//
// That is the whole design. It is worth stating plainly because the tempting
// version of this feature — "show me the config with secrets masked" — is one
// forgotten pattern away from printing a production database password into an
// agent's context window.
//
// # What is covered
//
// .env files, YAML, JSON, TOML and Java .properties for DECLARATIONS; and
// environment-variable and config-lookup calls in Go, Java, Python, JavaScript
// and TypeScript for USAGE. Between them they answer the two questions that
// matter: what can be configured, and what reads it.
//
// NOT covered: Kubernetes manifests, Helm values, Terraform and CI definitions,
// all listed in spec §24. Each is a schema of its own, and a YAML scanner that
// treats a Kubernetes manifest as flat configuration produces keys like
// `spec.template.spec.containers` that no code ever reads. Structured formats
// are read as generic nested YAML/JSON, which is honest but shallow.
package config

import (
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

// Key is one configuration setting a file declares.
//
// There is deliberately no Value field. See the package comment.
type Key struct {
	// Name is the dotted path for nested formats (`database.pool.size`), or the
	// bare name for flat ones (`DATABASE_URL`).
	Name string
	Line int
	// Secret marks a key whose NAME indicates it holds a credential. It exists
	// so a report can say "this is configured, and it is sensitive" without
	// ever having read what it holds.
	Secret bool
}

// Usage is one place in code that reads configuration.
type Usage struct {
	// Name is the key as written at the call site.
	Name string
	Line int
	// Mechanism is how it was read — "env", "spring-value", "viper", "dotenv" —
	// so a report can distinguish a process environment variable from an
	// application setting.
	Mechanism string
}

// secretNamePatterns identify a key whose name says it holds a credential.
//
// Matched against the KEY NAME only, which is the point: this classifies
// without reading anything. It is generous on purpose — a false positive costs
// a "sensitive" label on a harmless setting, while a false negative costs
// nothing here either, because no value was ever read. The label is advisory.
var secretNamePatterns = regexp.MustCompile(
	`(?i)(pass(word|wd)?|secret|token|api[_-]?key|apikey|private[_-]?key|credential|` +
		`auth|access[_-]?key|client[_-]?secret|encryption|signing|salt|dsn|` +
		`connection[_-]?string)`)

// IsSecretName reports whether a key name indicates a credential.
func IsSecretName(name string) bool { return secretNamePatterns.MatchString(name) }

// maxConfigBytes bounds a configuration file.
const maxConfigBytes = 1 << 20

// maxNesting bounds how deep a dotted key path may go, and maxKeys how many one
// file may declare.
//
// Both are correctness bounds as much as safety ones. Nothing looks up a
// setting nested thirty levels deep, so a document claiming to be is either
// generated data or hostile — and without the bound it is genuinely dangerous:
// a file of 20,000 nested keys builds a 40 KB dotted path and runs the secret
// pattern over it once per key. That is quadratic in the document, and it hung
// the test suite for a full minute before these were added.
const (
	maxNesting = 32
	maxKeys    = 20000
)

// IsConfigFile reports whether a path is a configuration file this package
// reads declarations from.
func IsConfigFile(path string) bool { return formatOf(path) != "" }

type format string

const (
	formatEnv        format = "env"
	formatYAML       format = "yaml"
	formatJSON       format = "json"
	formatTOML       format = "toml"
	formatProperties format = "properties"
)

func formatOf(path string) format {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	lower := strings.ToLower(base)

	// A dotfile env file: .env, .env.local, .env.production.
	if lower == ".env" || strings.HasPrefix(lower, ".env.") {
		return formatEnv
	}
	switch {
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"):
		return formatYAML
	case strings.HasSuffix(lower, ".properties"):
		return formatProperties
	case strings.HasSuffix(lower, ".toml"):
		return formatTOML
	case strings.HasSuffix(lower, ".json"):
		// Only files that are plausibly configuration. package.json and
		// tsconfig.json are manifests and build settings; treating them as
		// application configuration fills the report with keys like
		// `compilerOptions.strict` that no runtime lookup will ever match.
		if isBuildJSON(lower) {
			return ""
		}
		return formatJSON
	}
	return ""
}

func isBuildJSON(base string) bool {
	switch base {
	case "package.json", "package-lock.json", "tsconfig.json", "composer.json",
		"composer.lock", "jsconfig.json", "angular.json", "nx.json":
		return true
	}
	return strings.HasSuffix(base, ".lock.json")
}

// Declarations reads the configuration keys a file declares.
//
// Reads structure only. No parser here ever retains the text after a delimiter.
func Declarations(path string, src []byte) []Key {
	f := formatOf(path)
	if f == "" || len(src) == 0 || len(src) > maxConfigBytes {
		return nil
	}
	switch f {
	case formatEnv, formatProperties:
		return flatKeys(string(src))
	case formatYAML:
		return yamlKeys(string(src))
	case formatJSON:
		return jsonKeys(string(src))
	case formatTOML:
		return tomlKeys(string(src))
	}
	return nil
}

// flatKeys reads `KEY=value` and `key: value` .properties style.
//
// The split is on the FIRST delimiter and the right-hand side is discarded
// immediately — it is never assigned to a variable that outlives the loop
// iteration.
func flatKeys(src string) []Key {
	var out []Key
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		idx := strings.IndexAny(trimmed, "=:")
		if idx <= 0 {
			continue
		}
		name, ok := cleanKey(trimmed[:idx])
		if !ok {
			continue
		}
		out = append(out, Key{Name: name, Line: i + 1, Secret: IsSecretName(name)})
	}
	return out
}

// yamlKeys walks indentation to produce dotted paths.
//
// A real YAML parser would be more correct on anchors, flow mappings and
// multi-line scalars. It would also require holding parsed VALUES in memory,
// which this package exists not to do. Indentation tracking reads keys and
// nothing else.
func yamlKeys(src string) []Key {
	var out []Key
	type level struct {
		indent int
		name   string
	}
	var stack []level

	for i, raw := range strings.Split(src, "\n") {
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(raw), "- ") {
			// A sequence item. Its contents are values, not settings a caller
			// looks up by name.
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		trimmed := strings.TrimSpace(raw)
		colon := strings.Index(trimmed, ":")
		if colon <= 0 {
			continue
		}
		name, ok := cleanKey(trimmed[:colon])
		if !ok {
			continue
		}

		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		path := name
		if len(stack) > 0 && len(stack) < maxNesting {
			parts := make([]string, 0, len(stack)+1)
			for _, l := range stack {
				parts = append(parts, l.name)
			}
			path = strings.Join(parts, ".") + "." + name
		}

		// A key with nothing after the colon is a parent; one with content is a
		// leaf. Both are reported — `database` is a real prefix a caller may
		// bind a whole struct to — but only the text BEFORE the colon is ever
		// looked at.
		if len(out) < maxKeys {
			out = append(out, Key{Name: path, Line: i + 1, Secret: IsSecretName(path)})
		}
		if len(stack) < maxNesting {
			stack = append(stack, level{indent: indent, name: name})
		}
	}
	return out
}

// jsonKeys reads key names by scanning for the `"name":` shape and tracking
// object depth for the dotted path.
//
// Deliberately not encoding/json: unmarshalling would materialise every value
// in memory, and this package's guarantee is that it never holds one.
//
// The scan is hand-written and strictly LINEAR. An earlier version matched the
// key shape with a regex against src[i:], which allocates a fresh substring of
// the whole remaining document at every quote character — quadratic, and on a
// pathological 80 KB input it hung the test suite outright. Reading the string,
// then the delimiter, costs one pass.
func jsonKeys(src string) []Key {
	var (
		out         []Key
		stack       []string
		depthAtPush []int
		line        = 1
		depth       int
	)

	// pop unwinds the path stack. The two call sites need DIFFERENT conditions,
	// and conflating them was a bug: closing a brace pops what is strictly
	// deeper than the new depth, while a new key pops its own SIBLINGS too.
	// With one shared condition, {"a":{"b":1,"c":2}} reported `a.b.c` — the
	// sibling `b` was still on the stack when `c` arrived.
	pop := func(minDepth int) {
		for len(depthAtPush) > 0 && depthAtPush[len(depthAtPush)-1] >= minDepth {
			depthAtPush = depthAtPush[:len(depthAtPush)-1]
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\n':
			line++
			continue
		case '{':
			depth++
			continue
		case '}':
			depth--
			pop(depth + 1)
			continue
		case '"':
		default:
			continue
		}

		// Read the quoted string in one pass, counting newlines so the line
		// number stays right even for a multi-line value.
		startLine := line
		j := i + 1
		for ; j < len(src); j++ {
			if src[j] == '\\' {
				j++
				continue
			}
			if src[j] == '"' {
				break
			}
			if src[j] == '\n' {
				line++
			}
		}
		if j >= len(src) {
			break // unterminated string: nothing further can be read
		}
		text := src[i+1 : j]

		// A key is a string followed by a colon. Anything else is a value, and
		// its contents are never looked at again.
		//
		// The newlines between the string and whatever follows it are counted
		// into `gap` and only committed once this turns out to BE a key. On the
		// other branch the scan rewinds to the closing quote, so the main loop
		// walks those same bytes again — counting them here as well made every
		// key after a multi-line gap report a line number further and further
		// past the end of the file, which points a reader at a line that does
		// not exist.
		k := j + 1
		gap := 0
		for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\r' || src[k] == '\n') {
			if src[k] == '\n' {
				gap++
			}
			k++
		}
		if k >= len(src) || src[k] != ':' {
			i = j
			continue
		}
		line += gap

		pop(depth)
		// A segment that is not a plausible key name is pushed as "" rather
		// than dropped. It still has to occupy the stack, because the stack
		// mirrors object depth and removing an entry would re-parent everything
		// below it under its grandparent — a dotted path that is not in the
		// document. The blank marks the whole subtree unnameable, which is the
		// honest reading: a lookup path one of whose segments cannot be written
		// is not a path any caller can use. Joining it anyway put the segment's
		// raw text into the report as part of a key name, and in a document
		// whose quoting is malformed that text is a VALUE — `{"a":"secret":{}}`
		// reported keys beginning `secret.`, which is the one thing §24 says
		// this package can never do.
		// Through cleanKey, not plausibleKey, so this path gets the credential
		// redaction every other format already had. A JSON object keyed by
		// token — an allow-list — otherwise put the token itself into
		// symbols.name, which is exactly the leak §24 exists to prevent.
		seg, ok := cleanKey(text)
		if !ok {
			seg = ""
		}
		if seg != "" && len(stack) < maxNesting && nameable(stack) && len(out) < maxKeys {
			path := seg
			if len(stack) > 0 {
				path = strings.Join(stack, ".") + "." + seg
			}
			out = append(out, Key{Name: path, Line: startLine, Secret: IsSecretName(path)})
		}
		if len(stack) < maxNesting {
			stack = append(stack, seg)
			depthAtPush = append(depthAtPush, depth)
		}
		i = k
	}
	return out
}

// nameable reports whether every enclosing segment can appear in a key name.
func nameable(stack []string) bool {
	for _, s := range stack {
		if s == "" {
			return false
		}
	}
	return true
}

// tomlKeys reads `[section]` headers and `key = ` assignments.
func tomlKeys(src string) []Key {
	var out []Key
	section := ""
	for i, raw := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			// A section header is `[name]` or `[[name]]` and NOTHING else.
			//
			// Taking the whole line as the section is how a value reached a key
			// name: `[a] = "hunter2"` is not a header, but trimming brackets off
			// it left the section `a] = "hunter2"`, and every key below was then
			// reported as `a] = "hunter2".port`. Spec §24 says a value has
			// nowhere in this package to go, and that held only as long as no
			// scanner read past a delimiter — this one did. A header that cannot
			// be read names nothing, so the keys under it are reported bare
			// rather than under a section built out of a secret.
			section = ""
			if inner, ok := cleanKey(strings.Trim(trimmed, "[]")); ok && strings.HasSuffix(trimmed, "]") {
				section = inner
			}
			continue
		}
		eq := strings.Index(trimmed, "=")
		if eq <= 0 {
			continue
		}
		name, ok := cleanKey(trimmed[:eq])
		if !ok {
			continue
		}
		path := name
		if section != "" {
			path = section + "." + name
		}
		out = append(out, Key{Name: path, Line: i + 1, Secret: IsSecretName(path)})
	}
	return out
}

// cleanKey normalises one name segment and reports whether it is usable.
//
// The quotes come off. `"database"` in TOML, `'database'` in YAML and
// `database` are the same setting, and a caller looking one up writes it
// without them — but the quotes survived into the dotted path, where
// `server."port"` matched no lookup and, worse, was no longer a name this
// package could re-validate. Every emitted Name is a join of segments that
// passed through here, so plausibleKey holds for the whole path and not merely
// for its last piece.
func cleanKey(name string) (string, bool) {
	name = strings.Trim(strings.TrimSpace(name), `"'`)
	if !plausibleKey(name) {
		return "", false
	}
	// A KEY can itself be a credential.
	//
	// This package's guarantee is that no value is ever read, and that holds —
	// every scanner stops at the delimiter. But an allow-list keyed by token
	// (`tokens: { AKIA...: allowed }`) puts the credential on the LEFT, and the
	// left is exactly what is kept. Confirmed in a built index: an AWS key id, a
	// GitHub PAT and a Stripe key all reached symbols.name and symbols.fqn
	// verbatim, and shipped to the agent from there.
	//
	// The file's CONTENT is protected because RedactFile runs before the search
	// index is written; the symbol name bypassed that entirely, which is why the
	// same detector is applied here rather than a second pattern list. One
	// definition of "this looks like a credential", used on both paths.
	//
	// Redacted rather than dropped: `tokens.[REDACTED:aws-access-key]` still
	// says the setting exists and what shape it is, which is the useful half,
	// while carrying nothing an attacker wants.
	if findings := security.Detect(name); len(findings) > 0 {
		return findings[0].Placeholder(), true
	}
	return name, true
}

// plausibleKey rejects tokens that cannot be configuration key names, which is
// what keeps prose in a YAML comment or a JSON string out of the report.
//
// A quote is not one of the permitted characters, and it used to be tolerated
// at the ends of a name. That leniency was load-bearing in the wrong direction:
// the quotes were trimmed for the CHECK but kept in the emitted name, so
// `'port` passed here and then produced the dotted path `server.'port` — a name
// this package would itself refuse if handed it back, and one no lookup can
// match. Formats where quoting is part of the syntax strip it in cleanKey
// before asking; everywhere else a quote in a name means the scan read
// something that is not a name.
func plausibleKey(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	// At least one letter or digit. Punctuation alone is never a key, and
	// without this a Go comment reading `os.Getenv("...", "10000")-style`
	// contributed a configuration key literally named "..." to the report.
	if !strings.ContainsFunc(name, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}) {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- usage

var usagePatterns = []struct {
	mechanism string
	re        *regexp.Regexp
}{
	// Go
	{"env", regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"([^"]+)"`)},
	{"viper", regexp.MustCompile(`viper\.\w+\(\s*"([^"]+)"`)},
	// Java / Spring
	{"spring-value", regexp.MustCompile(`@Value\s*\(\s*"\$\{([^:}]+)`)},
	{"env", regexp.MustCompile(`System\.getenv\(\s*"([^"]+)"`)},
	{"spring-property", regexp.MustCompile(`(?:getProperty|getRequiredProperty)\(\s*"([^"]+)"`)},
	// Python
	{"env", regexp.MustCompile(`os\.environ(?:\.get)?\s*[\[(]\s*["']([^"']+)["']`)},
	{"env", regexp.MustCompile(`os\.getenv\(\s*["']([^"']+)["']`)},
	// JavaScript / TypeScript
	{"env", regexp.MustCompile(`process\.env\.([A-Za-z_][A-Za-z0-9_]*)`)},
	{"env", regexp.MustCompile(`process\.env\[\s*["']([^"']+)["']`)},
	// Generic application config lookup, deliberately last so a more specific
	// mechanism claims the line first.
	{"config", regexp.MustCompile(`(?:^|[^\w.])config\.(?:Get\w*|get)\(\s*["']([^"']+)["']`)},
}

// Usages finds configuration reads in one source file.
func Usages(path string, src []byte) []Usage {
	if len(src) == 0 || len(src) > maxConfigBytes*4 {
		return nil
	}
	if formatOf(path) != "" || !sourceLike(path) {
		// A configuration file declares; it does not read. Scanning one for
		// usage patterns would report a `.env` line as its own consumer.
		return nil
	}

	var out []Usage
	seen := make(map[string]struct{})
	for i, line := range strings.Split(string(src), "\n") {
		line = stripLineComment(line)
		for _, p := range usagePatterns {
			for _, m := range p.re.FindAllStringSubmatch(line, -1) {
				name := strings.TrimSpace(m[1])
				if name == "" || !plausibleKey(name) {
					continue
				}
				key := name + "\x00" + itoa(i+1)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, Usage{Name: name, Line: i + 1, Mechanism: p.mechanism})
			}
		}
	}
	return out
}

// stripLineComment removes a trailing line comment, tracking quotes so that a
// marker inside a string literal is left alone.
//
// A comment is documentation, not a call site. The scanner is line-based and
// regex-driven, so without this it reads code that is explicitly not running:
// on a real Go service, a comment describing `os.Getenv("...", "10000")-style`
// defaulting was reported as a configuration read, attributed to a line that
// executes nothing.
//
// Only the unambiguous markers are handled — `//` and `#`. A `--` comment is
// SQL's, and this scanner only ever sees source files.
func stripLineComment(line string) string {
	var inSingle, inDouble, inBack bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inSingle:
			if c == '\'' && (i == 0 || line[i-1] != '\\') {
				inSingle = false
			}
		case inDouble:
			if c == '"' && (i == 0 || line[i-1] != '\\') {
				inDouble = false
			}
		case inBack:
			if c == '`' {
				inBack = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '`':
			inBack = true
		case c == '#':
			return line[:i]
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

func sourceLike(path string) bool {
	for _, ext := range []string{".go", ".java", ".py", ".js", ".ts", ".tsx",
		".jsx", ".mjs", ".kt", ".rb", ".rs", ".cs"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
