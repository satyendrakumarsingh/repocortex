package security

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// redactPrefix opens every placeholder.
//
// It doubles as the idempotence marker. Redaction is applied at more than one
// layer (the excerpt builder, then the MCP response), and any candidate value
// CONTAINING this prefix is wholly or partly the output of an earlier pass.
// Re-redacting it would nest placeholders, corrupt the text and invalidate the
// line/column arithmetic callers depend on — so every acceptance path, and the
// backward key walk, checks for it.
const redactPrefix = "[REDACTED:"

const (
	// minValueLen is the shortest literal treated as a credential. Below this a
	// "value" is punctuation or a type name, and redacting it only damages the
	// excerpt. No real credential is three bytes long.
	minValueLen = 4

	// minEntropyLen and entropyFloor gate the entropy signal. Entropy NEVER
	// fires on its own: base64 blobs, hashes, UUIDs and minified code are
	// everywhere in real repositories, and an entropy-only detector redacts so
	// much of a codebase that people turn it off — which is a worse outcome
	// than a slightly narrower detector. It only ever confirms a value that
	// already sits behind a credential-adjacent name (see keyWeak).
	minEntropyLen = 20
	entropyFloor  = 3.5
)

// Finding records that a credential exists, where it is, and what kind it is.
// Deliberately everything EXCEPT the value: that is the whole contract of this
// package restated as a struct.
//
// Line/Col follow model.Position — 1-indexed line, 0-indexed byte column — so a
// caller can turn a Finding into model.Evidence without adjusting anything.
type Finding struct {
	Category Category
	Line     int
	Col      int
	Length   int // byte length of the value that was removed
}

// Placeholder is the text that replaced the value.
func (f Finding) Placeholder() string { return redactPrefix + string(f.Category) + "]" }

// String renders a finding the way it is safe to log: location and kind only.
func (f Finding) String() string {
	return fmt.Sprintf("%s at %d:%d (%d bytes)", f.Category, f.Line, f.Col, f.Length)
}

// Redact replaces every secret VALUE in text with a categorised placeholder and
// returns what it found.
//
// Structure survives on purpose. "DB_PASSWORD=[REDACTED:generic-password]" still
// tells the agent that this file configures a database password and what the
// variable is called, which is the fact it can actually use; the seven bytes it
// loses are the seven bytes it must never have.
//
// Redact is idempotent: Redact(Redact(x)) == Redact(x), with no findings on the
// second pass.
func Redact(text string) (string, []Finding) {
	spans := candidates(text)
	if len(spans) == 0 {
		return text, nil
	}

	var b strings.Builder
	b.Grow(len(text) + len(spans)*32)
	findings := make([]Finding, 0, len(spans))
	var pos posTracker
	pos.reset(text)

	prev := 0
	for _, s := range spans {
		b.WriteString(text[prev:s.start])
		b.WriteString(redactPrefix)
		b.WriteString(string(s.cat))
		b.WriteByte(']')
		prev = s.end

		line, col := pos.at(s.start)
		findings = append(findings, Finding{Category: s.cat, Line: line, Col: col, Length: s.end - s.start})
	}
	b.WriteString(text[prev:])
	return b.String(), findings
}

// Detect reports the secrets in text without rewriting it, for callers that
// only need to answer "does this file contain credentials, and where" — the
// "exists: true, key: DB_PASSWORD" shape from spec §24.
func Detect(text string) []Finding {
	spans := candidates(text)
	if len(spans) == 0 {
		return nil
	}
	findings := make([]Finding, 0, len(spans))
	var pos posTracker
	pos.reset(text)
	for _, s := range spans {
		line, col := pos.at(s.start)
		findings = append(findings, Finding{Category: s.cat, Line: line, Col: col, Length: s.end - s.start})
	}
	return findings
}

// RedactFile applies path classification before content scanning.
//
// For a file whose very identity is a credential (.env, id_rsa, a kubeconfig)
// pattern matching is the wrong tool: the whole file is the secret, every line
// of it, and a pattern that misses one line leaks. So the content is suppressed
// wholesale and replaced with a statement of what was suppressed — which is
// still useful to an agent ("this service is configured by a .env with 14
// entries") and cannot leak.
func RedactFile(path, content string) (string, []Finding) {
	if !IsSensitivePath(path) {
		return Redact(content)
	}
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++ // a final line with no terminator still counts
	}
	notice := fmt.Sprintf("%s%s] %s suppressed in full: %d lines, %d bytes. "+
		"Files of this kind are credentials end to end, so RepoCortex never serves their contents.",
		redactPrefix, CategorySensitiveFile, path, lines, len(content))
	return notice, []Finding{{Category: CategorySensitiveFile, Line: 1, Col: 0, Length: len(content)}}
}

// span is a byte range of text that must not be served.
type span struct {
	start, end int
	cat        Category
	// specific marks a match from a distinctive vendor pattern. When two
	// candidates collide, the specific one wins so the agent is told
	// "aws-access-key" rather than the vaguer "generic-api-key" it would get
	// from the assignment around it.
	specific bool
}

func candidates(text string) []span {
	sig := prescan(text)

	var out []span
	for i := range detectors {
		d := &detectors[i]
		if !d.guard(text, sig) {
			continue
		}
		if d.anchor != "" {
			out = appendAnchored(out, text, d)
			continue
		}
		for _, m := range d.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2], m[3] // group 1: the value, by construction
			if s >= 0 && acceptStandalone(text[s:e]) {
				out = append(out, span{start: s, end: e, cat: d.cat, specific: true})
			}
		}
	}
	if sig.assign {
		out = appendAssignments(out, text)
	}
	if len(out) < 2 {
		return out
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		if out[i].specific != out[j].specific {
			return out[i].specific
		}
		return out[i].end > out[j].end
	})

	// Keep the earliest, most specific, longest candidate and drop anything it
	// overlaps. Dropping the overlap rather than splitting it matters: a split
	// could emit a placeholder inside a secret and leave the remainder visible.
	kept := out[:0]
	end := -1
	for _, s := range out {
		if s.start < end {
			continue
		}
		kept = append(kept, s)
		end = s.end
	}
	return kept
}

// appendAnchored runs an anchored detector at each occurrence of its literal
// rather than at every byte. strings.Index is vectorised and the anchored
// regexp fails in constant time when it does not apply, which turns a full NFA
// walk of the text into a handful of short ones. Correctness is unchanged
// because the anchor is a literal prefix of every match the pattern can produce.
func appendAnchored(out []span, text string, d *detector) []span {
	for base := 0; base < len(text); {
		p := strings.Index(text[base:], d.anchor)
		if p < 0 {
			break
		}
		p += base
		m := d.re.FindStringSubmatchIndex(text[p:])
		if m == nil {
			base = p + len(d.anchor)
			continue
		}
		if s, e := p+m[2], p+m[3]; m[2] >= 0 && acceptStandalone(text[s:e]) {
			out = append(out, span{start: s, end: e, cat: d.cat, specific: true})
		}
		base = p + m[1]
	}
	return out
}

// acceptStandalone rejects a vendor-pattern match that is, or contains, output
// from an earlier pass. Contains rather than HasPrefix: a match can begin one
// byte before a placeholder ("token:A[REDACTED:jwt]" — another FuzzRedact
// find), and re-redacting that would nest placeholders and corrupt the text.
func acceptStandalone(val string) bool {
	return !strings.Contains(val, redactPrefix) && !isTemplate(val)
}

// appendAssignments finds "credential-ish name <separator> value" across every
// syntax RepoCortex indexes at once: env files (DB_PASSWORD=x), YAML and JSON
// (password: "x"), Go (password := "x"), Ruby and JS hashes (password => "x"),
// properties files and JVM flags (-Dpassword=x).
//
// It is hand-written rather than one big regexp, for two reasons. Speed is the
// obvious one — this scan runs on every byte served, and the equivalent regexp
// measured ~4 MB/s against ~500 MB/s here, because a case-insensitive keyword
// alternation with bounded key repeats compiles to a program Go's NFA has to
// walk for every input byte. The other is that the interesting decisions are
// not regular: what counts as a key, which separator implies assignment, and
// whether a value is a literal or an expression are all judgement calls, and
// they are far easier to read — and to argue about in review — as code.
func appendAssignments(out []span, text string) []span {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '=' && c != ':' {
			continue
		}
		sepEnd := i + 1
		if sepEnd < len(text) {
			switch {
			case c == ':' && text[sepEnd] == '=': // Go, Pascal
				sepEnd++
			case c == '=' && (text[sepEnd] == '>' || text[sepEnd] == '='): // Ruby/JS hash, comparison
				sepEnd++
			}
		}

		key, ok := keyBefore(text, i)
		if !ok || classifyKey(key) == keyNone {
			continue
		}
		vs, ve, quoted, ok := valueAfter(text, sepEnd)
		if !ok {
			continue
		}

		val := text[vs:ve]
		if !quoted {
			// Trailing punctuation belongs to the surrounding syntax, not to
			// the value. Redacting the comma out of `Password: pwd,` would
			// produce text that no longer parses, and callers do feed our
			// output back through language tooling.
			val = strings.TrimRight(val, ",;)}]")
			ve = vs + len(val)
		}

		tail := text[ve:]
		if n := strings.IndexByte(tail, '\n'); n >= 0 {
			tail = tail[:n]
		}

		// "=", ":=", "=>" and "==" are assignment or comparison against a
		// literal. A bare ":" is also how English writes a list, which is why
		// the two are held to different standards in plausibleValue.
		assigns := c == '=' || text[sepEnd-1] == '='

		if cat, ok := classifyAssignment(key, val, quoted, assigns, tail); ok {
			out = append(out, span{start: vs, end: ve, cat: cat})
			// Resume after the value: a secret must not also be read as a key.
			// Only on a hit — skipping a REJECTED value could step over a real
			// assignment nested inside it, e.g. `note: password=hunter2`.
			i = ve - 1
		}
	}
	return out
}

// maxKeyLen bounds the backward walk. A 200-character identifier is not a
// credential name, and the bound keeps the scan linear on hostile input.
const maxKeyLen = 80

func isKeyByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_', c == '-', c == '.', c == '$':
		return true
	}
	return false
}

// keyBefore walks back from a separator to the name being assigned, stepping
// over the punctuation that different syntaxes put in between: a comparison
// operator (password != "x"), whitespace, and one closing quote or bracket
// ("password": x, config[:password] => x).
func keyBefore(text string, sep int) (string, bool) {
	j := sep
	if j > 0 && (text[j-1] == '!' || text[j-1] == '<' || text[j-1] == '>') {
		j--
	}
	for j > 0 && (text[j-1] == ' ' || text[j-1] == '\t') {
		j--
	}
	if j > 0 && (text[j-1] == '"' || text[j-1] == '\'' || text[j-1] == '`' || text[j-1] == ']') {
		j--
	}
	end := j
	for j > 0 && isKeyByte(text[j-1]) && end-j < maxKeyLen {
		j--
	}
	if end == j {
		return "", false
	}
	// A placeholder from an earlier pass looks exactly like a key: the closer
	// walk steps over its ']' and the name walk reads its category, so
	// "pwd=[REDACTED:generic-password] =0001" would treat "generic-password" as
	// a key and redact the next value. FuzzRedact found this; it is the reason
	// idempotence has to be asserted rather than assumed.
	if strings.HasSuffix(text[:j], redactPrefix) {
		return "", false
	}
	return text[j:end], true
}

// valueAfter reads the literal following a separator. An unterminated quote is
// treated as an unquoted value rather than as no value at all: excerpts are
// truncated to a line budget, and a key cut off mid-string is exactly when
// failing open would serve the secret.
func valueAfter(text string, from int) (start, end int, quoted, ok bool) {
	j := from
	for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	if j >= len(text) {
		return 0, 0, false, false
	}
	if q := text[j]; q == '"' || q == '\'' || q == '`' {
		for k := j + 1; k < len(text); k++ {
			switch text[k] {
			case '\\':
				k++ // an escaped quote does not close the literal
			case q:
				return j + 1, k, true, true
			case '\n', '\r':
				k = len(text) // unterminated on this line
			}
		}
		j++
	}
	start = j
	for j < len(text) && !isSpaceByte(text[j]) {
		j++
	}
	if j == start {
		return 0, 0, false, false
	}
	return start, j, false, true
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

type keyClass int

const (
	keyNone keyClass = iota
	keyWeak
	keyStrong
)

// strongKeywords name things that only ever hold credentials. A literal behind
// one of these is redacted on sight.
var strongKeywords = []string{
	"password", "passwd", "passphrase", "pwd",
	"secret", "token", "credential",
	"apikey", "accesskey", "privatekey", "signingkey", "encryptionkey",
}

// weakKeywords name things that USUALLY hold something harmless — a map key, an
// auth strategy name, a session cookie name. These require entropy corroboration
// before anything is redacted, which is the only role entropy plays here.
var weakKeywords = []string{"key", "auth", "pass", "session", "signature", "cookie", "bearer"}

// keywordStart marks the bytes that begin some keyword above. Most names in
// real code begin no keyword at any offset, and one table lookup per byte
// retires them for the price of a fraction of one substring scan — worth having
// because classifyKey runs once per '=' or ':' in every file served.
var keywordStart = func() (t [256]bool) {
	for _, list := range [][]string{strongKeywords, weakKeywords} {
		for _, w := range list {
			t[w[0]] = true
			t[w[0]-('a'-'A')] = true
		}
	}
	return t
}()

func classifyKey(key string) keyClass {
	possible := false
	for i := 0; i < len(key) && !possible; i++ {
		possible = keywordStart[key[i]]
	}
	if !possible {
		return keyNone
	}
	for _, s := range strongKeywords {
		if keyContains(key, s) {
			return keyStrong
		}
	}
	for _, s := range weakKeywords {
		if keyContains(key, s) {
			return keyWeak
		}
	}
	return keyNone
}

func classifyAssignment(key, val string, quoted, assigns bool, tail string) (Category, bool) {
	class := classifyKey(key)
	if class == keyNone {
		return "", false
	}
	if !plausibleValue(key, val, quoted, assigns, tail) {
		return "", false
	}
	if class == keyStrong {
		return categoryForKey(key), true
	}
	// Weak name: entropy has to earn it, and only above a length floor. A short
	// high-entropy string is just an identifier.
	if len(val) >= minEntropyLen && looksEncoded(val) && shannonEntropy(val) >= entropyFloor {
		return CategoryHighEntropy, true
	}
	return "", false
}

func categoryForKey(key string) Category {
	switch {
	case keyContains(key, "password"), keyContains(key, "passwd"),
		keyContains(key, "passphrase"), keyContains(key, "pwd"):
		return CategoryPassword
	case keyContains(key, "apikey"), keyContains(key, "accesskey"):
		return CategoryAPIKey
	case keyContains(key, "token"):
		return CategoryToken
	default:
		return CategorySecret
	}
}

// keyContains reports whether needle (lower-case) appears in key, folding case
// and ignoring the separators people use interchangeably — so api_key, API-KEY,
// apiKey and api.key are all one thing.
//
// It compares in place rather than normalising into a new string because this
// runs once per '=' or ':' in every file served, and a per-separator allocation
// would put the garbage collector on the hot path.
func keyContains(key, needle string) bool {
	for i := 0; i < len(key); i++ {
		j, k := i, 0
		for ; j < len(key) && k < len(needle); j++ {
			c := key[j]
			if c == '_' || c == '-' || c == '.' || c == '$' {
				continue
			}
			if lowerASCII(c) != needle[k] {
				break
			}
			k++
		}
		if k == len(needle) {
			return true
		}
	}
	return false
}

// plausibleValue decides whether a literal is a credential or a piece of code
// that merely sits next to a credential-shaped name.
//
// This function is the reason the tool is usable. Without it, `password` as a
// struct field, a function parameter, a TypeScript type or a word in a comment
// all become redactions, the output turns to swiss cheese and someone disables
// redaction entirely — which is the actual failure we are guarding against. The
// bar is still deliberately low: when a rule below is a close call it lets the
// redaction happen.
func plausibleValue(key, val string, quoted, assigns bool, tail string) bool {
	if len(val) < minValueLen {
		return false
	}
	if strings.Contains(val, redactPrefix) {
		return false // wholly or partly redacted already; idempotence
	}
	if isTemplate(val) {
		return false
	}
	if nonSecretWords[strings.ToLower(val)] {
		return false
	}
	// A URL with no "@" carries no credential and is architecture worth
	// showing. The credential-bearing form is caught by the connection-string
	// pattern, which redacts only the password inside it.
	if schemeRE.MatchString(val) && !strings.Contains(val, "@") {
		return false
	}
	if quoted {
		// A quoted literal behind a credential name is the classic secret. No
		// further structural excuse is accepted.
		return true
	}

	// Unquoted values are where code lives, so the checks are stricter.
	switch val[0] {
	case '{', '[', '(', '<', '&', '*', '|', '>':
		return false // a structure, an expression or a YAML block indicator
	}
	if strings.ContainsAny(val, "()") {
		return false // a call: os.Getenv("PASSWORD"), config.get(...)
	}
	if dottedIdentRE.MatchString(val) {
		return false // a reference: process.env.PASSWORD, cfg.Password
	}
	if looksLikePath(val) {
		return false // PWD=/home/x, private_key_path=/etc/ssl/key.pem
	}
	if strings.EqualFold(lastSegment(key), val) {
		// `Password: password,` — a variable named after the field it fills.
		// A literal identical to its own key is idiomatic Go/JS/Python, and is
		// how struct literals and keyword arguments are written everywhere.
		return false
	}
	if assigns {
		// "PASSWORD=x" is assignment syntax and nothing else, so what follows
		// on the line is irrelevant — which is what catches command lines like
		// `java -Dpassword=pr0d -jar app.jar`, a genuine leak vector in READMEs
		// and CI config.
		return true
	}
	// A bare ":" is ambiguous with English. An unquoted scalar runs to the end
	// of its line in every format that has unquoted scalars (YAML, properties),
	// so if prose follows, this is prose: "password: must be at least 8
	// characters" is documentation. A trailing comment is still a value.
	return tailIsQuiet(tail)
}

func tailIsQuiet(tail string) bool {
	t := strings.TrimLeft(tail, " \t,;)}]")
	switch {
	case t == "":
		return true
	case strings.HasPrefix(t, "#"), strings.HasPrefix(t, "//"), strings.HasPrefix(t, "/*"),
		strings.HasPrefix(t, "--"), strings.HasPrefix(t, "<!--"):
		return true
	}
	return false
}

// isTemplate spots values that are shaped like a placeholder rather than a
// secret: ${DB_PASSWORD}, {{ .Values.password }}, $PASSWORD, <your-key-here>,
// ****, xxxxxxxx. These are what a repository contains INSTEAD of a secret, and
// redacting them hides the wiring while protecting nothing.
func isTemplate(val string) bool {
	if val == "" {
		return true
	}
	if val[0] == '$' {
		return true
	}
	if strings.Contains(val, "${") || strings.Contains(val, "{{") ||
		strings.Contains(val, "%(") || strings.Contains(val, "<%") || strings.Contains(val, "$(") {
		return true
	}
	if val[0] == '<' && val[len(val)-1] == '>' {
		return true
	}
	for i := 1; i < len(val); i++ {
		if val[i] != val[0] {
			return false
		}
	}
	return true // every byte identical: ****, xxxx, ......
}

// nonSecretWords are literals that appear behind credential names in every
// codebase and are never credentials: booleans, nulls, and the type names from
// schemas and interface declarations.
var nonSecretWords = func() map[string]bool {
	m := make(map[string]bool)
	for _, w := range []string{
		"true", "false", "null", "nil", "none", "nan", "undefined", "undef",
		"empty", "unset", "n/a", "na", "yes", "no", "on", "off", "auto",
		"required", "optional", "todo", "tbd", "fixme", "unknown",
		"string", "str", "text", "number", "int", "integer", "long", "float",
		"double", "bool", "boolean", "any", "object", "array", "list", "map",
		"dict", "varchar", "byte", "bytes", "char", "date", "datetime",
		"timestamp", "uuid", "guid", "void", "self", "this", "default",
		"example", "sample", "dummy", "placeholder", "redacted", "hidden",
		"masked", "omitted", "value", "input", "output", "type",
	} {
		m[w] = true
	}
	return m
}()

func looksLikePath(val string) bool {
	if strings.HasPrefix(val, "./") || strings.HasPrefix(val, "../") || strings.HasPrefix(val, "~/") {
		return true
	}
	// An absolute path with more than one component. A password could in
	// principle look like this; a filesystem path almost always does.
	return val[0] == '/' && strings.Count(val, "/") >= 2 && !strings.Contains(val, ":")
}

func lastSegment(key string) string {
	if i := strings.LastIndexAny(key, "._-$"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// looksEncoded reports whether val is drawn from an encoding alphabet — base64,
// base64url or hex — which every machine-generated credential is and most
// English words, paths and identifiers are not.
func looksEncoded(val string) bool {
	var letters, digits int
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		case c >= '0' && c <= '9':
			digits++
		case c == '+', c == '/', c == '=', c == '_', c == '-':
		default:
			return false
		}
	}
	// Both classes present: "supersecretpassphrase" is not an encoded blob, and
	// neither is a decimal literal.
	return letters > 0 && digits > 0
}

// shannonEntropy returns bits per byte. Uniform base64 sits near 5-6, hex near
// 4, English prose near 3-4 — which is precisely why this number is never
// allowed to decide anything on its own.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// posTracker converts ascending byte offsets to 1-indexed line and 0-indexed
// column in a single amortised pass. Findings are sorted, so the scan never
// rewinds; the clean path never constructs one at all.
type posTracker struct {
	text      string
	off       int
	line      int
	lineStart int
}

func (p *posTracker) reset(text string) {
	p.text, p.off, p.line, p.lineStart = text, 0, 1, 0
}

func (p *posTracker) at(off int) (line, col int) {
	for ; p.off < off; p.off++ {
		if p.text[p.off] == '\n' {
			p.line++
			p.lineStart = p.off + 1
		}
	}
	return p.line, off - p.lineStart
}
