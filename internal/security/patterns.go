package security

import (
	"fmt"
	"regexp"
	"strings"
)

// Category names the kind of credential a Finding describes.
//
// The value is part of the output contract, not an internal label: it is what
// the agent sees inside the placeholder. "[REDACTED:aws-access-key]" tells the
// agent an AWS key lives on this line — which is the useful half of the fact —
// without telling it the key.
type Category string

const (
	CategoryPrivateKey       Category = "private-key"
	CategoryConnectionString Category = "connection-string"
	CategoryAWSAccessKey     Category = "aws-access-key"
	CategoryAWSSecretKey     Category = "aws-secret-key"
	CategoryGitHubToken      Category = "github-token"
	CategorySlackToken       Category = "slack-token"
	CategorySlackWebhook     Category = "slack-webhook"
	CategoryStripeKey        Category = "stripe-key"
	CategoryGoogleAPIKey     Category = "google-api-key"
	CategoryJWT              Category = "jwt"
	CategoryPassword         Category = "generic-password"
	CategoryAPIKey           Category = "generic-api-key"
	CategoryToken            Category = "generic-token"
	CategorySecret           Category = "generic-secret"
	CategoryHighEntropy      Category = "high-entropy"
	CategorySensitiveFile    Category = "sensitive-file"
)

// signals are the cheap facts about a piece of text, gathered in ONE pass over
// its bytes before any regexp runs. See prescan.
type signals struct {
	assign   bool // contains '=' or ':' — assignment syntax is possible
	at       bool // contains '@' — credentials in a URL are possible
	dashRun  bool // contains "-----" — PEM armour is possible
	upperRun bool // a run of 20+ [A-Z0-9] — an AWS key ID is possible
	b64Run   bool // a run of 40+ [A-Za-z0-9+/=] — an AWS secret key is possible
}

// detector is one credential shape distinctive enough to be recognised with no
// surrounding context, plus the cheap test that decides whether its regexp is
// worth running at all.
//
// INVARIANT: each re contains exactly one capturing group, and that group is
// the secret VALUE — not the assignment, not the quotes, not the URL around it.
// Redaction replaces exactly that group, which is what preserves the structure
// the agent needs: "postgres://app:[REDACTED:connection-string]@db:5432/orders"
// still says who connects to what. init() enforces this rather than trusting
// whoever edits the table next: a stray group would silently shift what gets
// replaced.
//
// EVERY GUARD MUST BE SOUND — true whenever the regexp could match. A guard is
// an optimisation, never a policy: the moment one starts guessing, this package
// acquires exactly the false negatives it exists to prevent. Each is justified
// against its own pattern below.
//
// A NOTE ON PATTERN SHAPE, which is worth more than it looks. Go's regexp
// accelerates a pattern that begins with a plain literal (it can memchr for it)
// and walks every input position for one that does not. Measured on the same
// 4 KB input: "-----BEGIN…" runs at 5700 MB/s, the identical pattern behind a
// leading \b at 30 MB/s. So patterns here start with their literal wherever
// possible, and the leading \b is omitted deliberately — it would cost 190x to
// buy a precision we do not want anyway, since a credential embedded in a
// longer run of text is still a credential.
//
// Where a literal prefix is impossible (an alternation of vendor prefixes), the
// anchor field does the same job by hand: the literal is located with a fast
// substring search and the regexp is applied only there, anchored with ^.
type detector struct {
	cat Category
	re  *regexp.Regexp
	// anchor, when set, is a literal that every match must begin with. The
	// regexp is then applied only at occurrences of it and must start with ^.
	anchor string
	guard  func(text string, sig signals) bool
}

var detectors = []detector{
	// PEM first: it is the longest match and must win any overlap with a
	// pattern that fires inside the base64 body.
	//
	// The alternation is deliberate. The first branch is a properly terminated
	// block; the second, preferred only when no END armour exists, runs to the
	// end of the text. An excerpt truncated mid-key is exactly the case where
	// the tidy pattern fails open, and failing open here means serving the key
	// material itself.
	//
	// Guard: the armour is five dashes. Sound.
	{
		cat: CategoryPrivateKey,
		re:  regexp.MustCompile(`(-----BEGIN[A-Z0-9 ]* PRIVATE KEY(?: BLOCK)?-----(?s:.*?-----END[^-\r\n]*-----|.*))`),
		guard: func(t string, s signals) bool {
			return s.dashRun && strings.Contains(t, "-----BEGIN")
		},
	},

	// Credentials embedded in a connection string. Only the password is
	// captured: scheme, user and host are architecture, and letting an agent
	// see architecture is the entire point of the index.
	//
	// Anchored on "://" because this guard is the one most likely to fire on an
	// innocent file — any source file with a URL in a comment and an email
	// address somewhere passes both halves of it. Scanning from each "://"
	// instead of from every byte takes that case from 167 us per 4 KB to noise.
	// The scheme itself is not checked: what identifies a credential here is the
	// "user:secret@" shape, not the four letters in front of it.
	{
		cat:    CategoryConnectionString,
		re:     regexp.MustCompile(`^://[^:@/\s"']*:([^@/\s"']+)@`),
		anchor: "://",
		guard: func(t string, s signals) bool {
			return s.at && strings.Contains(t, "://")
		},
	},

	// Guard: every AWS key ID is a four-character prefix plus sixteen more
	// [A-Z0-9], so any match implies a 20-byte run of upper-case alphanumerics.
	// That run is rare in ordinary code (underscores and lower case break it),
	// so the eleven substring searches behind it are almost never paid.
	{
		cat: CategoryAWSAccessKey,
		re:  regexp.MustCompile(`((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA|AGPA|AIDA|AIPA|ANPA|ANVA|AROA)[A-Z0-9]{16})`),
		guard: func(t string, s signals) bool {
			return s.upperRun && containsAny(t,
				"AKIA", "ASIA", "AROA", "AIDA", "AGPA", "AIPA", "ANPA", "ANVA", "ABIA", "ACCA", "A3T")
		},
	},

	// An AWS secret access key is 40 base64-ish characters with no prefix — it
	// is shape-identical to a git SHA, a base64 hash, or half the constants in
	// a crypto library. It is therefore matched ONLY next to a name that says
	// "aws … secret/key". Matching bare 40-character strings would redact most
	// real source files; see the note on entropy in redact.go.
	//
	// Guard: a 40-byte base64 run plus the literal "aws" in some case. Sound.
	{
		cat: CategoryAWSSecretKey,
		re:  regexp.MustCompile(`(?i:aws)[A-Za-z0-9_.\-]*(?i:secret|key)[A-Za-z0-9_.\-]*["']?[ \t]*[:=][ \t]*["']?([A-Za-z0-9/+=]{40})`),
		guard: func(t string, s signals) bool {
			return s.b64Run && containsFold(t, "aws")
		},
	},

	{
		cat: CategoryGitHubToken,
		re:  regexp.MustCompile(`(gh[posur]_[A-Za-z0-9]{36,})\b`),
		guard: func(t string, s signals) bool {
			return containsAny(t, "ghp_", "gho_", "ghs_", "ghu_", "ghr_")
		},
	},
	{
		cat:   CategoryGitHubToken,
		re:    regexp.MustCompile(`(github_pat_[A-Za-z0-9_]{22,})\b`),
		guard: func(t string, s signals) bool { return strings.Contains(t, "github_pat_") },
	},
	{
		cat:   CategorySlackToken,
		re:    regexp.MustCompile(`(xox[abeoprs]-[A-Za-z0-9\-]{10,})\b`),
		guard: func(t string, s signals) bool { return strings.Contains(t, "xox") },
	},
	{
		cat:   CategorySlackWebhook,
		re:    regexp.MustCompile(`(https://hooks\.slack\.com/services/[A-Za-z0-9_/+\-]{10,})`),
		guard: func(t string, s signals) bool { return strings.Contains(t, "hooks.slack.com") },
	},

	// Stripe SECRET and restricted keys only. pk_live_/pk_test_ publishable
	// keys are designed to be shipped in browser bundles; redacting them would
	// be theatre, and theatre costs the same agent context as a real finding.
	{
		cat:   CategoryStripeKey,
		re:    regexp.MustCompile(`([sr]k_(?:live|test)_[A-Za-z0-9]{10,})\b`),
		guard: func(t string, s signals) bool { return containsAny(t, "k_live_", "k_test_") },
	},
	// No trailing \b on this one or on the AWS key ID above, and that is a
	// correctness fix rather than a shortcut: both are FIXED-LENGTH patterns, so
	// inside a longer run of alphanumerics only the tail-aligned occurrence
	// satisfies a trailing boundary. Redacting it then creates a new boundary,
	// the next pass matches the twenty characters before it, and redaction stops
	// being idempotent. FuzzRedact found this with strings.Repeat("AKIA", 400).
	{
		cat:   CategoryGoogleAPIKey,
		re:    regexp.MustCompile(`(AIza[A-Za-z0-9_\-]{35})`),
		guard: func(t string, s signals) bool { return strings.Contains(t, "AIza") },
	},

	// A JWT. The signature segment may legitimately be empty (alg=none), and
	// such a token is if anything more interesting, so it is not required.
	// "eyJ" is base64 for the '{"' that opens every JWT header.
	{
		cat:   CategoryJWT,
		re:    regexp.MustCompile(`(eyJ[A-Za-z0-9_=\-]{8,}\.[A-Za-z0-9_=\-]{8,}\.[A-Za-z0-9_=\-]*)`),
		guard: func(t string, s signals) bool { return strings.Contains(t, "eyJ") },
	},
}

func init() {
	for _, d := range detectors {
		if n := d.re.NumSubexp(); n != 1 {
			panic(fmt.Sprintf("security: detector %s must have exactly one capturing group (the value), got %d", d.cat, n))
		}
	}
}

// dottedIdentRE recognises a value that is a reference rather than a literal:
// PropTypes.string, os.Getenv, cfg.Password, process.env.DB_PASSWORD. Redacting
// these produces pure noise — the name of a variable is not a credential, and
// hiding it destroys exactly the wiring an agent is trying to follow.
var dottedIdentRE = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)+$`)

// schemeRE spots a bare URL. A URL with no "@" carries no credential, and
// endpoints are architecture worth showing; the credential-bearing form is
// handled by CategoryConnectionString above.
var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://`)

// upperAlnum and b64Byte back the run-length signals in prescan.
var upperAlnum, b64Byte = func() (u, b [256]bool) {
	for c := 0; c < 256; c++ {
		switch {
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			u[c], b[c] = true, true
		case c >= 'a' && c <= 'z', c == '+', c == '/', c == '=':
			b[c] = true
		}
	}
	return u, b
}()

// prescan is the fast path, and the reason redaction is affordable at all.
//
// Go's regexp is RE2: linear and un-exploitable, but only around 4 MB/s on
// patterns this shape. Running eleven of them plus a generic assignment scanner
// over every byte RepoCortex serves costs milliseconds per file. Running a single
// byte-classification pass and a handful of SIMD substring searches costs
// microseconds, and answers "could anything here match?" for the overwhelmingly
// common case: a source file with no credentials in it.
//
// The output is only ever used to SKIP work, so every flag is a necessary
// condition of some pattern, never a sufficient one. See the guard comments in
// the detector table for the per-pattern argument.
func prescan(text string) signals {
	var sig signals
	var dash, upper, b64 int
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case '=', ':':
			sig.assign = true
		case '@':
			sig.at = true
		}
		if c == '-' {
			if dash++; dash >= 5 {
				sig.dashRun = true
			}
		} else {
			dash = 0
		}
		if upperAlnum[c] {
			if upper++; upper >= 20 {
				sig.upperRun = true
			}
		} else {
			upper = 0
		}
		if b64Byte[c] {
			if b64++; b64 >= 40 {
				sig.b64Run = true
			}
		} else {
			b64 = 0
		}
	}
	return sig
}

func containsAny(text string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

// containsFold is an ASCII case-insensitive substring search that allocates
// nothing. strings.Contains(strings.ToLower(text), needle) would copy every
// byte RepoCortex serves, on the hot path, to answer a question we ask once.
// needle must be lower-case.
func containsFold(text, needle string) bool {
	if len(needle) == 0 || len(text) < len(needle) {
		return len(needle) == 0
	}
	last := len(text) - len(needle)
	for i := 0; i <= last; i++ {
		if lowerASCII(text[i]) != needle[0] {
			continue
		}
		j := 1
		for ; j < len(needle); j++ {
			if lowerASCII(text[i+j]) != needle[j] {
				break
			}
		}
		if j == len(needle) {
			return true
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
