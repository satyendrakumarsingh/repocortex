// Package infra reads deployment manifests: what the code RUNS AS.
//
// Every other extractor in internal/analyze reads source. Source says what the
// program can do; a Dockerfile, a compose file, a Kubernetes manifest and a
// Terraform module say what actually exists at run time — which services there
// are, which ports they listen on, which environment variables they are handed,
// which Secret supplies them, and which of them a change is therefore about to
// affect. A change-safety tool that reads only source cannot answer "which
// service does this code run in", because no line of the code says so.
//
// # VALUES ARE NEVER EMITTED
//
// These files hold credentials. Spec §24 draws the line at values: existence
// and location may be reported, the credential may not. This package implements
// that the way internal/analyze/config does — as an absence. EnvKey has no
// Value field, KeySet holds key NAMES, and every scanner that meets a `K=V`
// stops at the delimiter and discards the right-hand side in the same
// expression that split it.
//
// One honest difference from internal/analyze/config, which reads nothing but
// key names off raw text: this package uses a real YAML parser, so for the
// lifetime of one Parse call the decoded document tree does hold the values that
// were in the file. It has to — "which container in which workload sets which
// key, and does the key come from a Secret or from a literal" is a question
// about structure, and a line-oriented scanner answers it wrong. The tree is
// local to Parse and is never returned, and the type system is the enforcement:
// there is no field in File, Unit, EnvKey or KeySet that a value fits in. Where
// a value would be visible anyway — a container command line that ends in
// `--token=…` — the text goes through internal/security.Redact first, which is
// the project's own redactor rather than a second opinion invented here.
//
// # A FILE WE COULD NOT READ IS DATA
//
// "No Kubernetes manifests found" and "we could not parse your Kubernetes
// manifests" have opposite consequences, and a scanner that reports the first
// when the second is true is worse than no scanner. Parse never returns an
// error: a file it recognises but cannot read comes back as a File carrying
// Problems, per-file and per-document, so a caller reports it instead of
// silently indexing zero services.
//
// # What is read
//
//	Dockerfile        base image per stage, EXPOSE, ENV/ARG key names, entrypoint
//	docker-compose    services, image/build, ports, env key names, env_file, depends_on
//	Kubernetes        kind, name, image, ports, env (incl. Secret/ConfigMap refs),
//	                  envFrom, resource limits; ConfigMap/Secret KEY NAMES
//	Helm              Chart.yaml identity, values*.yaml key names; templates are
//	                  reported as unrendered rather than guessed at
//	Terraform         resource, data, module and provider blocks; variable names
//
// # What is not read, and why
//
// Helm templates are not rendered. Rendering means running Helm's template
// engine over attacker-supplied input with a values file this package does not
// have, and a half-rendered manifest produces service names that do not exist.
// They are reported as present-and-unrendered, which is the true statement.
//
// Terraform is read with a bounded block scanner, not an HCL parser. Nothing in
// go.mod parses HCL, and pulling hashicorp/hcl in for a block header would add a
// large dependency to a tool whose redaction path is deliberately
// standard-library-only. The scanner therefore reads block HEADERS —
// `resource "aws_db_instance" "main"` — and never attribute values, which has
// the useful side effect that a `password = "…"` in a .tf file has no path into
// the output at all.
//
// Kubernetes manifests written as JSON, .tfvars, CloudFormation, systemd units
// and CI definitions are not read.
package infra

import (
	"strings"
	"unicode/utf8"

	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

// Format names the manifest family a file belongs to.
//
// This is part of the output contract — it is what the report groups by and
// what a caller filters on — so these strings are stable.
type Format string

const (
	FormatDockerfile Format = "dockerfile"
	FormatCompose    Format = "compose"
	FormatKubernetes Format = "kubernetes"
	FormatHelm       Format = "helm"
	FormatTerraform  Format = "terraform"
)

// EnvSource says where an environment variable's value comes from. The value
// itself is never read; where it comes from is often the more useful half, because
// "this key is supplied by Secret db-creds" tells you what rotating that Secret
// will restart.
type EnvSource string

const (
	// SourceLiteral: the manifest carries the value inline. The value is not read.
	SourceLiteral EnvSource = "literal"
	// SourceSecret: valueFrom.secretKeyRef — the value lives in a Kubernetes Secret.
	SourceSecret EnvSource = "secret-ref"
	// SourceConfigMap: valueFrom.configMapKeyRef.
	SourceConfigMap EnvSource = "configmap-ref"
	// SourceDownward: valueFrom.fieldRef / resourceFieldRef — supplied by the
	// cluster, not by configuration.
	SourceDownward EnvSource = "downward-api"
	// SourceHost: a compose `environment: [FOO]` entry with no `=`, which passes
	// the variable through from whatever runs docker.
	SourceHost EnvSource = "host"
	// SourceBuildArg: a Dockerfile ARG. Present at build time, not at run time.
	SourceBuildArg EnvSource = "build-arg"
	// SourceImage: a Dockerfile ENV. Baked into the image.
	SourceImage EnvSource = "image"
)

// EnvKey is one environment variable a deployment sets.
//
// THERE IS NO VALUE FIELD, and there must never be one. See the package comment.
type EnvKey struct {
	Name string    `json:"name"`
	Line int       `json:"line"`
	From EnvSource `json:"from"`
	// Ref names the Secret or ConfigMap the value is taken from, as
	// `name` or `name:key`. A name, never a value.
	Ref string `json:"ref,omitempty"`
	// Secret marks a key that either comes out of a Secret or whose NAME says it
	// holds a credential. Classifying by name requires reading nothing.
	Secret bool `json:"secret"`
}

// Port is one port a unit declares.
type Port struct {
	// Container is the port inside the container: `EXPOSE`, `containerPort`, the
	// right-hand side of a compose mapping.
	Container int `json:"container,omitempty"`
	// Published is the port reachable from outside: a compose host port, a
	// hostPort, a Service port or nodePort. Zero when the manifest publishes
	// nothing, which is itself the answer to "is this reachable".
	Published int    `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Name      string `json:"name,omitempty"`
	// Line is where the port was declared. A unit's own line is not good
	// enough: a Dockerfile's EXPOSE and a Deployment's containerPort sit many
	// lines away from the stage or container header, and "which line do I edit
	// to change this port" is the question a reader has once they have the
	// port. Zero only when the format states nothing better, in which case the
	// unit's line is the honest fallback and the caller applies it.
	Line int `json:"line,omitempty"`
	// Raw carries a declaration no number could be read from — `"${PORT}:80"`,
	// a range, a named targetPort. Reported rather than dropped: a port that
	// resolves at deploy time is still a port, and silently omitting it would
	// under-report what a service exposes.
	Raw string `json:"raw,omitempty"`
}

// SourceRef is a whole Secret or ConfigMap injected at once — Kubernetes
// `envFrom`.
//
// It is deliberately NOT expanded into EnvKeys. The keys are in the referenced
// object, which may live in another file or in no file in this repository at
// all, and inventing key names for it would be fabrication. Summarize resolves
// the reference against the KeySets actually found; what it cannot resolve is
// reported as unresolved, which is a real finding — a deployment that depends on
// configuration this repository does not contain.
type SourceRef struct {
	Kind string `json:"kind"` // "Secret" or "ConfigMap"
	Name string `json:"name"`
	// Prefix is envFrom's key prefix, applied to every key the object supplies.
	Prefix   string `json:"prefix,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Line     int    `json:"line"`
}

// Limit is one declared resource bound: cpu, memory, or an extended resource.
type Limit struct {
	Resource string `json:"resource"`
	Request  string `json:"request,omitempty"`
	Limit    string `json:"limit,omitempty"`
}

// Unit is one thing a manifest declares can run, or one infrastructure object it
// declares should exist.
//
// One type covers a compose service, a Kubernetes container, a Dockerfile build
// stage and a Terraform resource because the report's job is the same for all
// four: name it, say where it was declared, and say what it exposes and consumes.
// Type carries the distinction — "service", "Deployment", "stage",
// "resource:aws_db_instance" — rather than four near-identical structs.
type Unit struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Line int    `json:"line"`

	Image   string      `json:"image,omitempty"`
	Ports   []Port      `json:"ports,omitempty"`
	Env     []EnvKey    `json:"env,omitempty"`
	EnvFrom []SourceRef `json:"env_from,omitempty"`
	// EnvFiles are paths a compose service loads variables from. The files are
	// NOT opened here — .env files are internal/analyze/config's territory, and
	// opening one would put this package a single bug away from reading values.
	EnvFiles []string `json:"env_files,omitempty"`

	DependsOn []string `json:"depends_on,omitempty"`
	Limits    []Limit  `json:"limits,omitempty"`

	// Command is the entrypoint or command as declared, after redaction.
	Command string `json:"command,omitempty"`

	// BuildContext and Dockerfile tie a service back to the SOURCE DIRECTORY it
	// is built from. This is the one link in this package that answers "which
	// service does this code run in" directly, rather than by inference.
	BuildContext string `json:"build_context,omitempty"`
	Dockerfile   string `json:"dockerfile,omitempty"`

	// Final marks the last stage of a multi-stage Dockerfile — the one that
	// becomes the image unless a build overrides --target. The earlier stages
	// are toolchains; reporting them as things that run would be wrong.
	Final bool `json:"final,omitempty"`
}

// KeySet is a named collection of configuration keys a deployment supplies: a
// Kubernetes ConfigMap or Secret, a Helm values file, a compose secret
// declaration, or a Terraform variable block.
//
// KEYS ONLY. For a Kubernetes Secret this is the whole point: `data` maps key
// names to base64 credentials, and this package reads the left-hand side.
type KeySet struct {
	Name string   `json:"name"`
	Type string   `json:"type"` // "Secret", "ConfigMap", "helm-values", ...
	Line int      `json:"line"`
	Keys []EnvKey `json:"keys,omitempty"`
	// Secret marks a set whose values are credentials by construction.
	Secret bool `json:"secret,omitempty"`
}

// Problem is a file, or one document inside a file, that was recognised as a
// deployment manifest and could not be read.
//
// This is the type that makes silence honest. A caller that drops these reports
// "no manifests" for a repository whose manifests it merely failed to parse.
type Problem struct {
	// Reason is one of the Reason constants — a stable label, not prose.
	Reason string `json:"reason"`
	Line   int    `json:"line,omitempty"`
	// Detail is the parser's own message, REDACTED. A YAML error can quote the
	// text it choked on, and the text it choked on can be a credential.
	Detail string `json:"detail,omitempty"`
}

// Reasons a manifest could not be read. Stable strings: they appear in --json
// output and in the report.
const (
	ReasonTooLarge      = "too_large"
	ReasonMalformedYAML = "malformed_yaml"
	ReasonTemplated     = "templated_not_rendered"
	ReasonNoFrom        = "dockerfile_without_from"
	ReasonLimit         = "limit_reached"
	ReasonPanic         = "parser_panic"
)

// File is everything one manifest declared.
type File struct {
	Path     string    `json:"path"`
	Format   Format    `json:"format"`
	Units    []Unit    `json:"units,omitempty"`
	KeySets  []KeySet  `json:"key_sets,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
}

// Bounds. Every one of these is a correctness bound as much as a safety one:
// past them the input is generated or hostile, and a report with 40,000 rows in
// it is not a report anyone reads.
const (
	// maxManifestBytes bounds one file. A hand-written manifest is kilobytes; a
	// rendered bundle can be larger, hence the room.
	maxManifestBytes = 4 << 20
	// maxDocs bounds documents in one multi-document YAML stream.
	maxDocs = 512
	// maxUnits and maxKeys bound one file's contribution to the report.
	maxUnits = 2000
	maxKeys  = 5000
	// maxFieldBytes bounds any single emitted string.
	maxFieldBytes = 400
	// maxNameBytes bounds a key name. Longer than this is not a name.
	maxNameBytes = 200
)

// Candidate reports whether a path is worth reading as a deployment manifest.
//
// Name-based, so a caller can filter a directory walk without opening anything.
// It is deliberately loose for YAML: a plain `.yaml` cannot be classified
// without looking inside it, so every one is a candidate and Parse decides.
func Candidate(path string) bool { return classify(path) != classNone }

type class int

const (
	classNone class = iota
	classDockerfile
	classCompose
	classTerraform
	classHelmChart
	classHelmValues
	classHelmTemplate
	classYAML // needs content to decide: Kubernetes, or an unrelated YAML file
)

func classify(path string) class {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	if base == "" {
		return classNone
	}

	switch {
	case base == "dockerfile", base == "containerfile",
		strings.HasSuffix(base, ".dockerfile"),
		strings.HasPrefix(base, "dockerfile.") && !isSourceExtension(base):
		return classDockerfile
	case strings.HasSuffix(base, ".tf"):
		return classTerraform
	}

	yamlish := strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml")
	inTemplates := strings.Contains(p, "/templates/") || strings.HasPrefix(p, "templates/")
	if !yamlish {
		// A .tpl only counts inside a chart's templates directory, where Helm
		// gives it meaning. Elsewhere it is anybody's template language.
		if strings.HasSuffix(base, ".tpl") && inTemplates {
			return classHelmTemplate
		}
		return classNone
	}

	switch {
	case base == "chart.yaml", base == "chart.yml":
		return classHelmChart
	case base == "values.yaml", base == "values.yml",
		strings.HasPrefix(base, "values-"), strings.HasPrefix(base, "values."):
		return classHelmValues
	case inTemplates:
		return classHelmTemplate
	case strings.HasPrefix(base, "docker-compose."), strings.HasPrefix(base, "compose."):
		return classCompose
	}
	return classYAML
}

func formatOf(c class) Format {
	switch c {
	case classDockerfile:
		return FormatDockerfile
	case classCompose:
		return FormatCompose
	case classTerraform:
		return FormatTerraform
	case classHelmChart, classHelmValues, classHelmTemplate:
		return FormatHelm
	case classYAML:
		return FormatKubernetes
	}
	return ""
}

// Parse reads one deployment manifest.
//
// It returns ok=false only when the path is not a manifest at all, or when a
// generic YAML file turns out to be something other than Kubernetes. It does
// NOT return false for a manifest it failed to read: that comes back as a File
// with Problems, because a caller has to be able to tell "absent" from
// "unreadable".
func Parse(path string, src []byte) (File, bool) {
	c := classify(path)
	if c == classNone || len(src) == 0 {
		return File{}, false
	}

	if len(src) > maxManifestBytes {
		// An oversized generic YAML is only reported if it looks like a
		// manifest. Reporting every large YAML in a repository as an unreadable
		// Kubernetes file would bury the ones that are.
		if c == classYAML && !looksKubernetes(src[:min(len(src), 1<<16)]) {
			return File{}, false
		}
		return File{
			Path: path, Format: formatOf(c),
			Problems: []Problem{{Reason: ReasonTooLarge}},
		}, true
	}

	switch c {
	case classDockerfile:
		return parseDockerfile(path, src), true
	case classTerraform:
		return parseTerraform(path, src), true
	case classCompose:
		return parseCompose(path, src), true
	case classHelmChart:
		return parseChart(path, src), true
	case classHelmValues:
		return parseValues(path, src), true
	case classHelmTemplate:
		return parseHelmTemplate(path, src), true
	case classYAML:
		return parseGenericYAML(path, src)
	}
	return File{}, false
}

// looksKubernetes is a text sniff, not a parse.
//
// It exists for the case that matters most: a manifest whose YAML is broken
// cannot be classified by parsing it, and that is exactly the file that must not
// be dropped in silence.
func looksKubernetes(src []byte) bool {
	s := string(src)
	return strings.Contains(s, "apiVersion:") && strings.Contains(s, "kind:")
}

func looksTemplated(src []byte) bool { return strings.Contains(string(src), "{{") }

// ---------------------------------------------------------------- redaction

// safe prepares any string for the report: bounded, then run through the
// project's redactor.
//
// Redact rather than a local pattern list, for the reason internal/security's
// doc comment gives — one definition of "this looks like a credential", used
// everywhere — and Redact rather than Detect because these are structured
// strings. `/app --token=ghp_…` keeps its shape and loses its token, which is
// the half a reader needs and the half they must not have.
func safe(s string) string {
	s = clip(strings.TrimSpace(s), maxFieldBytes)
	if s == "" {
		return ""
	}
	out, _ := security.Redact(s)
	return out
}

// cleanName normalises a key or identifier and reports whether it is usable.
//
// The same shape as internal/analyze/config.cleanKey, and for the same reason:
// every name this package emits has been through here, so a name that fails
// plausibleName means a scanner read past a delimiter — and the thing past the
// delimiter is the value. That is the invariant FuzzParse asserts.
func cleanName(name string) (string, bool) {
	name = strings.Trim(strings.TrimSpace(name), `"'`)
	if !plausibleName(name) {
		return "", false
	}
	// A KEY can itself be a credential — an allow-list keyed by token puts the
	// secret on the left, and the left is what is kept.
	if f := security.Detect(name); len(f) > 0 {
		return f[0].Placeholder(), true
	}
	return name, true
}

// plausibleName rejects tokens that cannot be an environment variable, key or
// object name.
func plausibleName(name string) bool {
	if name == "" || len(name) > maxNameBytes {
		return false
	}
	if !strings.ContainsFunc(name, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}) {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

// IsRedacted reports whether a name was replaced by a redaction placeholder.
// Callers that re-validate names need it; so does FuzzParse.
func IsRedacted(s string) bool { return strings.HasPrefix(s, "[REDACTED:") }

// secretNamePatterns is deliberately borrowed in spirit from
// internal/analyze/config: a key whose NAME says credential is labelled without
// anything being read. Duplicated as a small local matcher rather than exported
// from config, because config's list is tuned for application settings and this
// one has to catch the container-shaped names too.
func isSecretName(name string) bool {
	l := strings.ToLower(name)
	for _, needle := range []string{
		"password", "passwd", "secret", "token", "apikey", "api_key", "api-key",
		"private_key", "privatekey", "private-key", "credential", "auth",
		"access_key", "accesskey", "access-key", "client_secret", "encryption",
		"signing", "salt", "dsn", "connection_string", "connectionstring",
		"cert", "keystore", "passphrase",
	} {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// newEnvKey builds an EnvKey from a raw name, or reports that the name was not
// one. The single construction point: nothing else in this package may append
// to Unit.Env.
func newEnvKey(raw string, line int, from EnvSource, ref string) (EnvKey, bool) {
	name, ok := cleanName(raw)
	if !ok {
		return EnvKey{}, false
	}
	return EnvKey{
		Name: name, Line: line, From: from, Ref: safe(ref),
		// A key out of a Secret is secret whatever it is called. Otherwise the
		// name is all there is to go on, which is the point.
		Secret: from == SourceSecret || isSecretName(name),
	}, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary: a half-written rune in a report renders as a
	// replacement character and, worse, is not the text that was in the file.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// isSourceExtension reports whether a `dockerfile.<x>` name is really source code.
//
// `Dockerfile.prod` and `Dockerfile.alpine` are a real convention, so the prefix
// rule has to exist. But it also matched `dockerfile.go` — this package's own
// parser — and `cortex infra` duly reported that Go file as a container build
// with two stages named `stage1` and `stage2` at lines 83 and 85. Every one of
// those "facts" was invented from Go source.
//
// That is the failure this whole codebase is built to avoid: a fabricated fact
// is worse than a missing one, because a reader cannot tell it apart from a real
// one. A deployment report is a particularly bad place for it, since nobody
// cross-checks a Dockerfile they did not know they had.
//
// Name alone cannot separate `Dockerfile.prod` from `Dockerfile.go` in general —
// "prod" and "go" are both just words. So the rule is the narrow, checkable one:
// if the suffix is a known source or asset extension, it is not a Dockerfile.
// A variant genuinely named after a language ("Dockerfile.rs" for a Rust
// service) is misclassified as source, which costs one manifest; the alternative
// costs invented infrastructure in every polyglot repository.
func isSourceExtension(base string) bool {
	i := strings.LastIndex(base, ".")
	if i < 0 {
		return false
	}
	switch base[i+1:] {
	case "go", "js", "ts", "tsx", "jsx", "py", "rb", "java", "kt", "kts",
		"rs", "c", "h", "cc", "cpp", "hpp", "cs", "php", "swift", "scala",
		"sh", "bash", "ps1", "sql", "md", "txt", "json", "yaml", "yml",
		"toml", "xml", "html", "css", "lock", "sum", "mod", "test":
		return true
	}
	return false
}
