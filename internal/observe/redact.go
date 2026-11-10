package observe

// Redaction guard.
//
// The threat is mundane and therefore likely: someone instrumenting the indexer
// writes `observe.StartSpan(ctx, "parse.file", attribute.String("path", p))`
// because that is the attribute they would want while debugging. It works, it
// looks reasonable in review, and it ships the directory layout of every
// private repository RepoCortex is pointed at to whatever collector the user
// configured. Nothing about the OTel API discourages it.
//
// So this file makes the wrong thing fail rather than merely documenting it.
// Two mechanisms, in order of preference:
//
//  1. Constructors for the attributes RepoCortex actually needs (Language,
//     SizeBucket, FileAttrs, Operation, ...). They take the sensitive input and
//     return only a coarse, non-identifying derivative. Use these.
//
//  2. A validator that every attribute passes through on its way to a span or a
//     metric. Under strict mode — on by default inside `go test` — it panics.
//     In a release binary it scrubs the value and reports the problem through
//     the OTel error handler. Fail loudly where a human will see it; fail safe
//     where they will not.
//
// WHAT IS SAFE TO ATTACH
//
//	counts, durations, depths, sizes ....... yes, any numeric value
//	language, parse status, edge kind ...... yes, via the constructors below
//	confidence level, accuracy mode ........ yes, small closed vocabularies
//	cache hit/miss, error type ............. yes
//
// WHAT IS NEVER SAFE
//
//	file paths, directory names, repo names, module paths
//	symbol names, FQNs, signatures, doc comments
//	source text of any length, including one-line snippets
//	search queries — they are the user's own words about their own code
//	config values, environment variables, collector credentials
//	anything free-form: error messages, stack frames, URLs
//
// The rule that resolves the ambiguous cases: an attribute may describe the
// *shape* of the work (how many, how long, what category) but never its
// *subject*. Spec §35.

import (
	"fmt"
	"math"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Registered attribute keys. Only these bypass the key denylist; everything
// else a call site invents must survive it. Keeping the list short is the
// point — each entry is a reviewed decision that this dimension cannot
// identify a user or their code.
const (
	AttrLanguage     = "cortex.language"         // "go", "python", "other"
	AttrSizeBucket   = "cortex.file.size_bucket" // "1k", "64k", "1m+"
	AttrParseStatus  = "cortex.parse_status"     // ok | partial | failed | skipped
	AttrConfidence   = "cortex.confidence"       // exact | high | medium | low
	AttrAccuracyMode = "cortex.accuracy_mode"    // fast | auto | exact
	AttrEdgeKind     = "cortex.edge_kind"        // CALLS, IMPORTS, ...
	AttrEntityKind   = "cortex.entity_kind"      // function, file, ...
	AttrTool         = "cortex.mcp.tool"         // RepoCortex's own tool names
	AttrStatus       = "cortex.status"           // ok | error | denied | timeout
	AttrCacheHit     = "cortex.cache.hit"        // bool
	AttrOperation    = "db.operation"            // select | insert | migrate | ...
	AttrErrorType    = "error.type"              // Go type name, set by RecordError
)

var registeredKeys = map[string]struct{}{
	AttrLanguage:     {},
	AttrSizeBucket:   {},
	AttrParseStatus:  {},
	AttrConfidence:   {},
	AttrAccuracyMode: {},
	AttrEdgeKind:     {},
	AttrEntityKind:   {},
	AttrTool:         {},
	AttrStatus:       {},
	AttrCacheHit:     {},
	AttrOperation:    {},
	AttrErrorType:    {},
}

// redactedValue replaces a value the guard refused, in builds that do not
// panic. It is deliberately not the original with characters masked: a masked
// path still discloses its length and structure.
const redactedValue = "<redacted>"

// maxValueLen caps categorical values. 40 characters is longer than any
// language name, status or SQL verb, and far shorter than a useful path,
// identifier or code fragment — which is the discriminating property, not the
// exact number.
const maxValueLen = 40

// unknownToken is what any value that fails normalisation becomes. Bucketing
// the unrecognised into one category keeps a hostile filename from becoming a
// metric dimension of its own.
const unknownToken = "other"

// strict controls the guard's response to an unsafe attribute: panic when set,
// scrub when not.
//
// It defaults to on inside test binaries, via testing.Testing(), which the
// linker sets. Importing `testing` from non-test code is unusual and costs
// about 130 KB of binary; that is a fair price for the property that a
// contributor who attaches a path finds out from a red test rather than from a
// user's collector. A test that never runs against the guard would prove
// nothing (CONTRIBUTING: "A gate that has only ever been seen passing proves
// nothing").
var strict atomic.Bool

func init() { strict.Store(testing.Testing()) }

// SetStrict switches panic-on-unsafe-attribute and returns the previous
// setting. Exported so a test in another package can assert the guard's
// behaviour in both modes, and so a fuzz target can turn panics off.
func SetStrict(on bool) bool { return strict.Swap(on) }

// UnsafeAttrError reports an attribute the guard refused.
//
// It carries the key and the reason but never the value. A rejected value is by
// definition something that should not be copied around, and error strings end
// up in CI logs, issue reports and, ironically, telemetry.
type UnsafeAttrError struct {
	Key    string
	Reason string
}

func (e *UnsafeAttrError) Error() string {
	return fmt.Sprintf("unsafe telemetry attribute %q: %s (value withheld); "+
		"use observe's attribute constructors — see redact.go", e.Key, e.Reason)
}

// CheckAttr reports whether an attribute is safe to export.
func CheckAttr(kv attribute.KeyValue) error {
	key := string(kv.Key)
	if _, ok := registeredKeys[key]; !ok {
		if reason := unsafeKey(key); reason != "" {
			return &UnsafeAttrError{Key: key, Reason: reason}
		}
	}
	// Values are checked even for registered keys. Registration says the
	// dimension is safe, not that a caller who bypassed the constructor put
	// something safe in it.
	switch kv.Value.Type() {
	case attribute.STRING:
		if reason := unsafeValue(kv.Value.AsString()); reason != "" {
			return &UnsafeAttrError{Key: key, Reason: reason}
		}
	case attribute.STRINGSLICE:
		for _, s := range kv.Value.AsStringSlice() {
			if reason := unsafeValue(s); reason != "" {
				return &UnsafeAttrError{Key: key, Reason: reason}
			}
		}
	default:
		// Numeric and boolean values are counts, durations and flags. They
		// cannot carry a path or a snippet, and cardinality is bounded by the
		// aggregation, so they pass.
	}
	return nil
}

// CheckAttrs validates a batch, returning the first problem.
func CheckAttrs(attrs []attribute.KeyValue) error {
	for _, kv := range attrs {
		if err := CheckAttr(kv); err != nil {
			return err
		}
	}
	return nil
}

// deniedKeySubstrings are the words that show up on attributes carrying things
// RepoCortex must not export. Matching on substrings is intentionally blunt:
// `file_path`, `srcPath` and `repo.path` should all be caught without anyone
// maintaining an exhaustive list.
var deniedKeySubstrings = []string{
	"path", "file", "dir", "folder", "repo", "module",
	"content", "source", "snippet", "code", "text", "body", "line",
	"symbol", "identifier", "signature", "fqn", "name", "query", "sql",
	"secret", "token", "password", "passwd", "credential", "apikey", "api_key",
	"auth", "cookie", "session", "email", "user", "home", "host", "url", "uri", "env",
}

func unsafeKey(key string) string {
	lower := strings.ToLower(key)
	for _, bad := range deniedKeySubstrings {
		if strings.Contains(lower, bad) {
			return fmt.Sprintf("key contains %q, which names data RepoCortex does not export", bad)
		}
	}
	return ""
}

// secretPrefixes are the well-known shapes of issued credentials. Cheap to
// check, and they catch the case where a token was pasted into a config value
// that then became an attribute.
var secretPrefixes = []string{
	"sk-", "sk_live", "sk_test", "pk_live", "rk_live",
	"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_",
	"xoxb-", "xoxp-", "xapp-", "AKIA", "ASIA", "ya29.",
	"eyJ",        // a JWT header, base64 of {"
	"-----BEGIN", // any PEM block
	"Bearer ", "Basic ",
}

// unsafeValue returns why a string value must not be exported, or "".
//
// The checks run cheapest and most specific first so the reported reason is the
// one a human would give.
func unsafeValue(v string) string {
	if v == "" || v == redactedValue {
		return ""
	}
	if len(v) > maxValueLen {
		return fmt.Sprintf("value is %d bytes; attributes carry categories, not text (max %d)", len(v), maxValueLen)
	}
	for _, p := range secretPrefixes {
		if strings.HasPrefix(v, p) {
			return "value has the shape of a credential"
		}
	}
	if reason := pathLike(v); reason != "" {
		return reason
	}
	if strings.Contains(v, "@") {
		return "value looks like an address or an identity"
	}
	if reason := charsetViolation(v); reason != "" {
		return reason
	}
	if len(v) >= 20 && shannonBits(v) > 3.5 {
		return "value is high-entropy and may be a key, hash or token"
	}
	return ""
}

func pathLike(v string) string {
	switch {
	case strings.ContainsAny(v, `/\`):
		return "value contains a path separator"
	case strings.HasPrefix(v, "~"), strings.HasPrefix(v, "."):
		return "value looks like a filesystem path"
	case strings.Contains(v, ".."):
		return "value looks like a relative path"
	case len(v) >= 2 && v[1] == ':' && isASCIILetter(v[0]):
		return "value looks like a Windows drive path"
	}
	return ""
}

// charsetViolation restricts values to a token alphabet. Anything with a space,
// a quote, a newline or punctuation beyond a handful of separators is prose,
// code or a serialised structure — none of which belongs in an attribute.
// The offending byte is described, not quoted: the reason string travels into
// CI logs and issue reports, and a rejected value should not be reassembled
// from them one character at a time.
func charsetViolation(v string) string {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if isTokenByte(b) {
			continue
		}
		switch {
		case b == ' ', b == '\t', b == '\n', b == '\r':
			return "value contains whitespace, so it is free text rather than a category"
		case b < 0x20 || b == 0x7f:
			return "value contains a control character"
		case b >= 0x80:
			return "value contains non-ASCII bytes, so it is not a category"
		default:
			return "value contains punctuation outside the categorical alphabet"
		}
	}
	return ""
}

func isTokenByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-', b == '+', b == '#', b == '.':
		return true
	}
	return false
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// shannonBits is entropy per byte. A hex digest or base64 token sits above 3.5;
// "objective-c", "migrate" and "CONFIGURED_BY" sit well below it. The threshold
// only ever fires on values already 20 bytes or longer, where a legitimate
// category would be surprising.
func shannonBits(v string) float64 {
	var freq [256]int
	for i := 0; i < len(v); i++ {
		freq[v[i]]++
	}
	n := float64(len(v))
	var bits float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}

// guardName validates a span or metric name. Names are written by RepoCortex
// developers, so this catches the case where one is built from data —
// `StartSpan(ctx, "parse "+file.Path)` — which would leak through the span name
// rather than through an attribute.
func guardName(name string) {
	if name == "" {
		refuse(&UnsafeAttrError{Key: "span.name", Reason: "name must not be empty"})
		return
	}
	if reason := unsafeValue(name); reason != "" {
		refuse(&UnsafeAttrError{Key: "span.name", Reason: reason})
	}
}

// guardAttrs validates attributes in place, scrubbing rejected values when not
// in strict mode. It mutates its argument, so callers pass a copy they own.
func guardAttrs(attrs []attribute.KeyValue) {
	for i := range attrs {
		err := CheckAttr(attrs[i])
		if err == nil {
			continue
		}
		refuse(err)
		attrs[i] = attribute.String(string(attrs[i].Key), redactedValue)
	}
}

// refuse panics in strict mode and reports otherwise.
//
// The asymmetry is the whole design. A panic during `go test` is a bug report
// delivered to the person who can fix it. A panic in a user's `cortex index`
// would be telemetry — an optional feature — crashing the tool, which is
// unacceptable for something that exists only to observe.
func refuse(err error) {
	if strict.Load() {
		panic(err)
	}
	otel.Handle(err)
}

// ---------------------------------------------------------------------------
// Safe attribute constructors. Prefer these to attribute.String everywhere.
// ---------------------------------------------------------------------------

// Language reports a source language as a coarse category.
//
// The input usually originates in a filename extension, which is
// attacker-controlled the moment RepoCortex indexes an untrusted repository (see
// CONTRIBUTING, invariant 3). A file called `secret-<token>.go` must not be
// able to turn its own name into a telemetry dimension, so anything outside the
// known set collapses to "other" — which also bounds metric cardinality.
func Language(lang string) attribute.KeyValue {
	return attribute.String(AttrLanguage, normaliseLanguage(lang))
}

func normaliseLanguage(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	if _, ok := knownLanguages[l]; ok {
		return l
	}
	return unknownToken
}

// knownLanguages is the closed vocabulary for the language dimension. Adding a
// language here is a deliberate act; forgetting to only costs a metric label.
var knownLanguages = map[string]struct{}{
	"go": {}, "python": {}, "javascript": {}, "typescript": {}, "java": {},
	"kotlin": {}, "ruby": {}, "rust": {}, "c": {}, "cpp": {}, "csharp": {},
	"php": {}, "swift": {}, "scala": {}, "shell": {}, "sql": {}, "html": {},
	"css": {}, "json": {}, "yaml": {}, "toml": {}, "markdown": {}, "proto": {},
	"terraform": {}, "dockerfile": {}, "other": {},
}

// extToLanguage maps the extensions RepoCortex recognises. It is intentionally
// separate from the parser registry: this table exists to bound a telemetry
// dimension, and must not grow a dependency on the parse layer.
var extToLanguage = map[string]string{
	".go": "go",
	".py": "python", ".pyi": "python",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript",
	".java": "java", ".kt": "kotlin", ".kts": "kotlin",
	".rb": "ruby", ".rs": "rust",
	".c": "c", ".h": "c",
	".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp", ".hh": "cpp",
	".cs": "csharp", ".php": "php", ".swift": "swift", ".scala": "scala",
	".sh": "shell", ".bash": "shell", ".zsh": "shell",
	".sql": "sql", ".html": "html", ".htm": "html", ".css": "css",
	".json": "json", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml",
	".md": "markdown", ".proto": "proto", ".tf": "terraform",
}

// LanguageOf derives a language category from a file path, exporting nothing
// else about it.
func LanguageOf(filePath string) attribute.KeyValue {
	ext := strings.ToLower(path.Ext(filePath))
	if lang, ok := extToLanguage[ext]; ok {
		return attribute.String(AttrLanguage, lang)
	}
	if strings.EqualFold(path.Base(filePath), "dockerfile") {
		return attribute.String(AttrLanguage, "dockerfile")
	}
	return attribute.String(AttrLanguage, unknownToken)
}

// sizeBuckets are exclusive upper bounds in bytes, paired with their labels.
// Powers of four from 1 KiB give useful resolution across the range real source
// files occupy without the label itself being a size.
var sizeBuckets = []struct {
	max   int64
	label string
}{
	{1 << 10, "1k"},
	{1 << 12, "4k"},
	{1 << 14, "16k"},
	{1 << 16, "64k"},
	{1 << 18, "256k"},
	{1 << 20, "1m"},
}

// SizeBucket reports a file size as a bucket label rather than a number.
//
// An exact byte count is close to a fingerprint: combined with a language and a
// timestamp it can identify a specific file in a public repository, and across
// a whole index it reconstructs a recognisable profile of the codebase. The
// bucket answers every question telemetry legitimately has ("are we slow on
// large files?") and none of the questions it does not.
func SizeBucket(bytes int64) attribute.KeyValue {
	return attribute.String(AttrSizeBucket, sizeBucketLabel(bytes))
}

func sizeBucketLabel(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	for _, b := range sizeBuckets {
		if bytes < b.max {
			return b.label
		}
	}
	return "1m+"
}

// FileAttrs is the only sanctioned way to describe a file in telemetry: its
// language and its size bucket, and nothing that could locate it.
//
// This exists so that the natural thing to write at a call site — "attach the
// file" — produces safe output. `observe.FileAttrs(f.Path, f.SizeBytes)` reads
// like it exports the path; it does not, and cannot.
func FileAttrs(filePath string, sizeBytes int64) []attribute.KeyValue {
	return []attribute.KeyValue{LanguageOf(filePath), SizeBucket(sizeBytes)}
}

// Operation labels a database operation ("select", "insert", "migrate"). The
// value is normalised to a bare token so a caller cannot pass a whole SQL
// statement — which would carry table names, and through them the schema of the
// indexed repository.
func Operation(op string) attribute.KeyValue {
	return attribute.String(AttrOperation, normaliseToken(op))
}

// Tool labels an MCP tool by RepoCortex's own name for it. Tool names come from
// RepoCortex's registry, not from the caller's payload, so they are safe — but
// they are normalised anyway, because an MCP client supplies the name on the
// wire and an unknown one must not become a metric dimension.
func Tool(name string) attribute.KeyValue {
	return attribute.String(AttrTool, normaliseToken(name))
}

// Status labels an outcome: "ok", "error", "denied", "timeout".
func Status(s string) attribute.KeyValue {
	return attribute.String(AttrStatus, normaliseToken(s))
}

// ParseStatus labels a parse outcome from model.File.ParseStatus.
func ParseStatus(s string) attribute.KeyValue {
	return attribute.String(AttrParseStatus, normaliseToken(s))
}

// Confidence labels a model.Confidence level. Taking a string keeps this
// package free of a dependency on pkg/model, which would otherwise pull the
// domain vocabulary into the telemetry layer.
func Confidence(c string) attribute.KeyValue {
	return attribute.String(AttrConfidence, normaliseToken(c))
}

// AccuracyMode labels the indexing mode: "fast", "auto" or "exact".
func AccuracyMode(m string) attribute.KeyValue {
	return attribute.String(AttrAccuracyMode, normaliseToken(m))
}

// EdgeKind labels a model.EdgeKind such as CALLS or IMPORTS.
func EdgeKind(k string) attribute.KeyValue {
	return attribute.String(AttrEdgeKind, normaliseToken(k))
}

// EntityKind labels a model.EntityKind such as function or file.
func EntityKind(k string) attribute.KeyValue {
	return attribute.String(AttrEntityKind, normaliseToken(k))
}

// CacheHit labels a cache outcome.
func CacheHit(hit bool) attribute.KeyValue {
	return attribute.Bool(AttrCacheHit, hit)
}

// maxTokenLen bounds a normalised token. Longer than any legitimate operation,
// status or tool name.
const maxTokenLen = 24

// normaliseToken reduces an arbitrary string to a short lowercase token, or to
// "other" if it is not one.
//
// This is the second line of defence behind the validator: even if a caller
// reaches a constructor with something unexpected, what reaches the exporter is
// a bounded token. Note that it does not truncate — a truncated path is still a
// path, and truncation would also silently create high-cardinality labels.
func normaliseToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > maxTokenLen {
		return unknownToken
	}
	for i := 0; i < len(s); i++ {
		if !isTokenByte(s[i]) {
			return unknownToken
		}
	}
	if pathLike(s) != "" {
		return unknownToken
	}
	return s
}
