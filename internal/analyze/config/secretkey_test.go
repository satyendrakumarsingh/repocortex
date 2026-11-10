package config

import (
	"strings"
	"testing"
)

// TestCredentialShapedKeyNamesAreRedacted pins a leak the package's own
// guarantee did not cover.
//
// "Values are never read" holds — every scanner stops at the delimiter. But an
// allow-list keyed by token puts the credential on the LEFT, and the left is
// exactly what is kept. Confirmed in a built index before this fix: an AWS key
// id, a GitHub PAT and a Stripe key all reached symbols.name and symbols.fqn
// verbatim, and shipped to an agent from there.
//
// File CONTENT is protected because RedactFile runs before the search index is
// written. The symbol name bypassed that entirely.
func TestCredentialShapedKeyNamesAreRedacted(t *testing.T) {
	cases := []struct{ format, src, secret string }{
		{"app.yml", "tokens:\n  AKIAIOSFODNN7EXAMPLE: allowed\n", "AKIAIOSFODNN7EXAMPLE"},
		{"app.json", `{"tokens":{"ghp_ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ":1}}`, "ghp_ZZZZ"},
		{".env", "AKIAIOSFODNN7EXAMPLE=1\n", "AKIAIOSFODNN7EXAMPLE"},
		{"a.properties", "AKIAIOSFODNN7EXAMPLE: 1\n", "AKIAIOSFODNN7EXAMPLE"},
		{"a.toml", "[tokens]\nAKIAIOSFODNN7EXAMPLE = 1\n", "AKIAIOSFODNN7EXAMPLE"},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			for _, k := range Declarations(c.format, []byte(c.src)) {
				if strings.Contains(k.Name, c.secret) {
					t.Errorf("a credential reached a key name: %q", k.Name)
				}
			}
		})
	}
}

// TestRedactionKeepsTheStructureLegible guards the over-correction. Dropping
// the key entirely would hide that the setting exists at all; the shape is the
// useful half and carries nothing an attacker wants.
func TestRedactionKeepsTheStructureLegible(t *testing.T) {
	keys := Declarations("app.yml", []byte("tokens:\n  AKIAIOSFODNN7EXAMPLE: allowed\n"))
	var found bool
	for _, k := range keys {
		if strings.HasPrefix(k.Name, "tokens.") && strings.Contains(k.Name, "REDACTED") {
			found = true
			if !k.Secret {
				t.Error("a redacted credential key is not labelled sensitive")
			}
		}
	}
	if !found {
		t.Errorf("the nested key vanished instead of being redacted: %+v", keys)
	}
}

// TestOrdinaryKeysAreUntouched. A redactor that fires on normal names would
// make the whole report unreadable, and would be switched off within a week.
func TestOrdinaryKeysAreUntouched(t *testing.T) {
	keys := Declarations("app.yml", []byte(
		"database:\n  url: x\n  pool_size: 10\nserver:\n  port: 8080\nfeature_flags:\n  new_checkout: true\n"))
	for _, k := range keys {
		if strings.Contains(k.Name, "REDACTED") {
			t.Errorf("an ordinary configuration key was redacted: %q", k.Name)
		}
	}
	if len(keys) < 6 {
		t.Errorf("keys went missing: %+v", keys)
	}
}
