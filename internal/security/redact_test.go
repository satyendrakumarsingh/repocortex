package security

import (
	"strings"
	"testing"
)

// Every credential in this file is syntactically valid and semantically fake.
// AWS publishes AKIAIOSFODNN7EXAMPLE for exactly this purpose; the rest follow
// the same convention (EXAMPLE / 0000 / notarealkey markers).

func TestDetectsEachPatternClass(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   Category
		secret string // must not survive into the output
	}{
		{
			name:   "aws access key id",
			input:  `aws_key = "AKIAIOSFODNN7EXAMPLE"`,
			want:   CategoryAWSAccessKey,
			secret: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:   "aws temporary session key id",
			input:  "ASIAY34FZKBOKMUTVV7A\n",
			want:   CategoryAWSAccessKey,
			secret: "ASIAY34FZKBOKMUTVV7A",
		},
		{
			name:   "aws secret access key next to its name",
			input:  `aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
			want:   CategoryAWSSecretKey,
			secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		{
			name:   "github personal access token",
			input:  "GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
			want:   CategoryGitHubToken,
			secret: "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		},
		{
			name:   "github fine-grained token",
			input:  "token: github_pat_11ABCDEFG0abcdefghijkl_0123456789abcdefghijklmnop",
			want:   CategoryGitHubToken,
			secret: "github_pat_11ABCDEFG0abcdefghijkl_0123456789abcdefghijklmnop",
		},
		{
			name:   "slack bot token",
			input:  `slack = "xoxb-000000000000-000000000000-EXAMPLEEXAMPLEEXAMPLE"`,
			want:   CategorySlackToken,
			secret: "xoxb-000000000000-000000000000-EXAMPLEEXAMPLEEXAMPLE",
		},
		{
			name:   "slack webhook url",
			input:  "hook: https://hooks.slack.com/services/T00000000/B00000000/EXAMPLEexample00",
			want:   CategorySlackWebhook,
			secret: "hooks.slack.com/services/T00000000",
		},
		{
			name:   "stripe secret key",
			input:  `STRIPE_KEY = "sk_live_0123456789abcdefEXAMPLE"`,
			want:   CategoryStripeKey,
			secret: "sk_live_0123456789abcdefEXAMPLE",
		},
		{
			name:   "google api key",
			input:  "maps: AIzaSyA00000000000000000000EXAMPLE00000",
			want:   CategoryGoogleAPIKey,
			secret: "AIzaSyA00000000000000000000EXAMPLE00000",
		},
		{
			name:   "jwt",
			input:  "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			want:   CategoryJWT,
			secret: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name: "rsa private key block",
			input: "-----BEGIN RSA PRIVATE KEY-----\n" +
				"MIIEowIBAAKCAQEA0EXAMPLEnotarealkey0000000000000000000000000000\n" +
				"AAAAB3NzaC1yc2EAAAADAQABAAABgQEXAMPLE0000000000000000000000000\n" +
				"-----END RSA PRIVATE KEY-----\n",
			want:   CategoryPrivateKey,
			secret: "MIIEowIBAAKCAQEA0EXAMPLEnotarealkey",
		},
		{
			name: "openssh private key block",
			input: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
				"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtz\n" +
				"-----END OPENSSH PRIVATE KEY-----",
			want:   CategoryPrivateKey,
			secret: "b3BlbnNzaC1rZXktdjEAAAAABG5vbmU",
		},
		{
			name: "pgp private key block",
			input: "-----BEGIN PGP PRIVATE KEY BLOCK-----\n" +
				"lQdGBGEXAMPLE00000000000000000000000000000000000000000000000000\n" +
				"-----END PGP PRIVATE KEY BLOCK-----",
			want:   CategoryPrivateKey,
			secret: "lQdGBGEXAMPLE0000",
		},
		{
			// The excerpt engine returns bounded ranges, so a key can arrive
			// with its END armour cut off. Failing open here would serve the
			// key material itself.
			name: "unterminated private key block",
			input: "-----BEGIN EC PRIVATE KEY-----\n" +
				"MHcCAQEEIEXAMPLE000000000000000000000000000000000000000000000000\n",
			want:   CategoryPrivateKey,
			secret: "MHcCAQEEIEXAMPLE000",
		},
		{
			name:   "postgres connection string",
			input:  "DATABASE_URL=postgres://orders_app:s3cr3t-pw@db.internal:5432/orders",
			want:   CategoryConnectionString,
			secret: "s3cr3t-pw",
		},
		{
			name:   "mongodb srv connection string",
			input:  `uri := "mongodb+srv://svc:EXAMPLEpw123@cluster0.abcde.mongodb.net/test"`,
			want:   CategoryConnectionString,
			secret: "EXAMPLEpw123",
		},
		{
			name:   "env file password",
			input:  "DB_PASSWORD=hunter2\n",
			want:   CategoryPassword,
			secret: "hunter2",
		},
		{
			name:   "yaml password",
			input:  "database:\n  password: sup3rs3cr3t\n",
			want:   CategoryPassword,
			secret: "sup3rs3cr3t",
		},
		{
			name:   "json api key",
			input:  `{"api_key": "abc123def456ghi789"}`,
			want:   CategoryAPIKey,
			secret: "abc123def456ghi789",
		},
		{
			name:   "properties client secret",
			input:  "oauth.client_secret=zXY9-notarealsecret-123\n",
			want:   CategorySecret,
			secret: "zXY9-notarealsecret-123",
		},
		{
			name:   "go string literal token",
			input:  "\tauthToken := \"t0k3n-notarealvalue-9876\"\n",
			want:   CategoryToken,
			secret: "t0k3n-notarealvalue-9876",
		},
		{
			name:   "ruby hash rocket",
			input:  `config[:password] => "r41ls3cr3tvalue"`,
			want:   CategoryPassword,
			secret: "r41ls3cr3tvalue",
		},
		{
			name:   "jvm system property",
			input:  "java -Dpassword=pr0dpassw0rd -jar app.jar",
			want:   CategoryPassword,
			secret: "pr0dpassw0rd",
		},
		{
			// Entropy as a SECONDARY signal: "key" alone is far too weak, so
			// the value has to look machine-generated and clear the length
			// floor before anything happens.
			name:   "weak key name plus high entropy",
			input:  "signature: aG9sZDEyMzQ1Njc4OWFiY2RlZmdo9Xk\n",
			want:   CategoryHighEntropy,
			secret: "aG9sZDEyMzQ1Njc4OWFiY2RlZmdo9Xk",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, findings := Redact(tc.input)
			if len(findings) == 0 {
				t.Fatalf("no findings\ninput: %q\noutput: %q", tc.input, out)
			}
			var got []Category
			hit := false
			for _, f := range findings {
				got = append(got, f.Category)
				if f.Category == tc.want {
					hit = true
				}
			}
			if !hit {
				t.Errorf("category = %v, want %s", got, tc.want)
			}
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret survived redaction:\n%s", out)
			}
			if !strings.Contains(out, redactPrefix) {
				t.Errorf("no placeholder in output: %q", out)
			}
		})
	}
}

// TestNegativeCases is the test that keeps the tool usable. Each input contains
// a credential-shaped NAME with no credential behind it; redacting any of them
// costs the agent real context and buys nothing.
func TestNegativeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name: "go struct field named Password",
			input: "type Credentials struct {\n" +
				"\tUsername string `json:\"username\"`\n" +
				"\tPassword string `json:\"password\" db:\"password\"`\n" +
				"\tAPIKey   string `json:\"api_key\"`\n" +
				"}\n",
		},
		{
			name: "comments mentioning passwords",
			input: "// Login compares the password against the stored bcrypt hash.\n" +
				"// The password is never logged; see the redaction notes above.\n" +
				"# password rotation happens every 90 days\n" +
				"/* the api key is provisioned by terraform, not committed */\n",
		},
		{
			name: "password as a function parameter",
			input: "func Authenticate(username, password string) error {\n" +
				"\treturn compare(username, password)\n" +
				"}\n",
		},
		{
			name: "python parameter with default",
			input: "def authenticate(self, user, password: str = None, token=None):\n" +
				"    return self._check(user, password)\n",
		},
		{
			name: "typescript interface",
			input: "interface Credentials {\n" +
				"  password: string;\n" +
				"  apiKey: string | null;\n" +
				"  token?: string\n" +
				"}\n",
		},
		{
			name:  "base64 encoded image",
			input: `<img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==" />`,
		},
		{
			name:  "base64 blob assigned to a non-credential name",
			input: "const logo = \"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==\";\n",
		},
		{
			name:  "git sha assigned to a hash field",
			input: "content_hash = \"da39a3ee5e6b4b0d3255bfef95601890afd80709\"\n",
		},
		{
			name:  "environment variable reference",
			input: "password = os.Getenv(\"DB_PASSWORD\")\napiKey = process.env.API_KEY\nsecret := cfg.ClientSecret\n",
		},
		{
			name:  "templated values",
			input: "password: ${DB_PASSWORD}\napi_key: {{ .Values.apiKey }}\ntoken: $GITHUB_TOKEN\nsecret: <your-secret-here>\n",
		},
		{
			name:  "documentation prose with a colon",
			input: "password: must be at least 8 characters and contain a digit\ntoken: issued by the auth service on login\n",
		},
		{
			name:  "struct literal filled from a variable",
			input: "cfg := Config{\n\tPassword: password,\n\tToken:    token,\n}\n",
		},
		{
			name:  "paths and urls",
			input: "PWD=/home/deploy/app\nprivate_key_path=/etc/ssl/private/server.key\ntoken_url = \"https://auth.example.com/oauth/token\"\n",
		},
		{
			name:  "map iteration and literals",
			input: "for key, value := range headers {\n\theaders[key] = value\n}\nm := map[string]string{\"key\": \"value\"}\n",
		},
		{
			name:  "empty and boolean values",
			input: "password = \"\"\nsecret: null\nrequire_token: true\napi_key: \n",
		},
		{
			name:  "public keys and certificates",
			input: "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0EXAMPLE\n-----END PUBLIC KEY-----\n-----BEGIN CERTIFICATE-----\nMIIDdzCCAl+gAwIBAgIEAgAAuTANBgkqhkiG9w0BAQUFADBaMQsw\n-----END CERTIFICATE-----\n",
		},
		{
			name:  "publishable stripe key is public by design",
			input: `const stripe = Stripe("pk_live_0123456789abcdefEXAMPLE");`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, findings := Redact(tc.input)
			if len(findings) != 0 {
				t.Errorf("false positive: %v\ninput:\n%s\noutput:\n%s", findings, tc.input, out)
			}
			if out != tc.input {
				t.Errorf("text was modified:\ngot:  %q\nwant: %q", out, tc.input)
			}
		})
	}
}

func TestRedactPreservesStructure(t *testing.T) {
	tests := []struct{ in, want string }{
		{"DB_PASSWORD=hunter2", "DB_PASSWORD=[REDACTED:generic-password]"},
		{`aws_key = "AKIAIOSFODNN7EXAMPLE"`, `aws_key = "[REDACTED:aws-access-key]"`},
		{
			"DATABASE_URL=postgres://orders_app:s3cr3tpw@db.internal:5432/orders",
			"DATABASE_URL=postgres://orders_app:[REDACTED:connection-string]@db.internal:5432/orders",
		},
		{"  password: hunter2  # prod", "  password: [REDACTED:generic-password]  # prod"},
	}
	for _, tc := range tests {
		if got, _ := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
		}
	}
}

// TestRedactIsIdempotent guards the two-layer case: the excerpt builder redacts,
// then the MCP response layer redacts again. A second pass must be a no-op, not
// a nested placeholder.
func TestRedactIsIdempotent(t *testing.T) {
	inputs := []string{
		"DB_PASSWORD=hunter2\napi_key: abc123def456ghi789\n",
		"DATABASE_URL=postgres://app:s3cr3tpw@db:5432/orders\n",
		`aws_key = "AKIAIOSFODNN7EXAMPLE"` + "\n",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0EXAMPLE000\n-----END RSA PRIVATE KEY-----\n",
		"token: eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk\n",
	}
	for _, in := range inputs {
		once, first := Redact(in)
		if len(first) == 0 {
			t.Fatalf("nothing detected in %q", in)
		}
		twice, second := Redact(once)
		if twice != once {
			t.Errorf("not idempotent:\nfirst:  %q\nsecond: %q", once, twice)
		}
		if len(second) != 0 {
			t.Errorf("second pass reported %v on already-redacted text %q", second, once)
		}
	}
}

func TestFindingLineAndColumn(t *testing.T) {
	input := "package main\n" + // line 1
		"\n" + // line 2
		"const dsn = \"postgres://u:pw12345@h/db\"\n" + // line 3
		"var apiKey = \"live_abc123def456\"\n" // line 4

	_, findings := Redact(input)
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want 2", findings)
	}

	// Line 3, column of "pw12345" inside the connection string.
	wantCol := strings.Index("const dsn = \"postgres://u:pw12345@h/db\"", "pw12345")
	if findings[0].Line != 3 || findings[0].Col != wantCol {
		t.Errorf("finding 0 = %d:%d, want 3:%d", findings[0].Line, findings[0].Col, wantCol)
	}
	if findings[0].Length != len("pw12345") {
		t.Errorf("finding 0 length = %d, want %d", findings[0].Length, len("pw12345"))
	}

	wantCol = strings.Index(`var apiKey = "live_abc123def456"`, "live_abc123def456")
	if findings[1].Line != 4 || findings[1].Col != wantCol {
		t.Errorf("finding 1 = %d:%d, want 4:%d", findings[1].Line, findings[1].Col, wantCol)
	}

	// Line/Col must locate the value in the ORIGINAL text, so a caller can turn
	// a Finding into model.Evidence without adjusting anything.
	lines := strings.Split(input, "\n")
	for _, f := range findings {
		line := lines[f.Line-1]
		if f.Col+f.Length > len(line) {
			t.Fatalf("%s runs past the end of line %d", f, f.Line)
		}
	}
}

func TestDetectMatchesRedact(t *testing.T) {
	input := "DB_PASSWORD=hunter2\nnothing := here\napi_key: abc123def456ghi789\n"
	_, want := Redact(input)
	got := Detect(input)
	if len(got) != len(want) {
		t.Fatalf("Detect = %v, Redact = %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("finding %d: Detect = %v, Redact = %v", i, got[i], want[i])
		}
	}
}

func TestRedactFileSuppressesSensitivePaths(t *testing.T) {
	content := "DB_PASSWORD=hunter2\nAPI_KEY=abc123\nDEBUG=true\n"
	out, findings := RedactFile("config/.env", content)

	if len(findings) != 1 || findings[0].Category != CategorySensitiveFile {
		t.Fatalf("findings = %v, want one sensitive-file finding", findings)
	}
	for _, leak := range []string{"hunter2", "abc123", "DEBUG=true"} {
		if strings.Contains(out, leak) {
			t.Errorf("suppressed file leaked %q: %s", leak, out)
		}
	}
	// The agent should still learn that the file exists and how big it is.
	if !strings.Contains(out, "config/.env") || !strings.Contains(out, "3 lines") {
		t.Errorf("notice lost the useful facts: %s", out)
	}

	// A normal path falls through to pattern redaction.
	out, findings = RedactFile("internal/db/conn.go", "password := \"hunter2andmore\"\n")
	if len(findings) != 1 || findings[0].Category == CategorySensitiveFile {
		t.Fatalf("findings = %v, want a pattern finding", findings)
	}
	if strings.Contains(out, "hunter2andmore") {
		t.Errorf("secret survived: %s", out)
	}
}

func TestMultipleFindingsInOneFile(t *testing.T) {
	input := "# deploy\n" +
		"DB_PASSWORD=hunter2\n" +
		"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n" +
		"harmless=value\n" +
		"GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB\n"

	out, findings := Redact(input)
	if len(findings) != 3 {
		t.Fatalf("findings = %v, want 3", findings)
	}
	wantLines := []int{2, 3, 5}
	for i, f := range findings {
		if f.Line != wantLines[i] {
			t.Errorf("finding %d on line %d, want %d", i, f.Line, wantLines[i])
		}
	}
	if !strings.Contains(out, "harmless=value") {
		t.Errorf("non-secret line was disturbed:\n%s", out)
	}
	if strings.Count(out, redactPrefix) != 3 {
		t.Errorf("expected 3 placeholders:\n%s", out)
	}
}

func TestEntropyNeverFiresAlone(t *testing.T) {
	// A high-entropy blob with no credential-adjacent name must be left alone,
	// however random it looks. Entropy is corroboration, never evidence.
	blob := "aG9sZDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6"
	for _, in := range []string{
		"const checksum = \"" + blob + "\"\n",
		"digest: " + blob + "\n",
		blob + "\n",
	} {
		if _, findings := Redact(in); len(findings) != 0 {
			t.Errorf("entropy fired alone on %q: %v", in, findings)
		}
	}
	if h := shannonEntropy(blob); h < 3.5 {
		t.Fatalf("test blob entropy %.2f is below the floor; it no longer tests what it claims", h)
	}
}

// TestPrescanGuardsAreSound is the test that keeps the fast path honest. A
// guard exists only to skip work; if one is ever false while its pattern would
// have matched, this package silently stops detecting that credential. So every
// detector is run with its guard bypassed and the results compared.
func TestPrescanGuardsAreSound(t *testing.T) {
	inputs := []string{
		"AKIAIOSFODNN7EXAMPLE",
		"ASIAY34FZKBOKMUTVV7A no separators here",
		"just some lower case prose with no punctuation at all",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"postgres://u:pw12345@h/db",
		"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		"github_pat_11ABCDEFG0abcdefghijkl_0123456789abcdefghijklmnop",
		"xoxb-000000000000-000000000000-EXAMPLEEXAMPLEEXAMPLE",
		"https://hooks.slack.com/services/T00000000/B00000000/EXAMPLEexample00",
		"sk_live_0123456789abcdefEXAMPLE",
		"rk_test_0123456789abcdefEXAMPLE",
		"AIzaSyA00000000000000000000EXAMPLE00000",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij",
		"DB_PASSWORD=hunter2",
		"",
	}
	for _, in := range inputs {
		sig := prescan(in)
		for i := range detectors {
			d := &detectors[i]
			if d.guard(in, sig) {
				continue
			}
			// An anchored pattern is written with ^ and only ever applied at its
			// literal, so it has to be probed the same way the scanner does.
			if d.anchor != "" {
				if spans := appendAnchored(nil, in, d); len(spans) > 0 {
					t.Errorf("guard for %s skipped text its own pattern matches: %q", d.cat, in)
				}
				continue
			}
			if loc := d.re.FindStringIndex(in); loc != nil {
				t.Errorf("guard for %s skipped text its own pattern matches at %v: %q", d.cat, loc, in)
			}
		}
	}

	// And it must actually skip: prose with no candidate byte reaches nothing.
	sig := prescan("just some lower case prose with no punctuation at all")
	if sig.assign || sig.at || sig.dashRun || sig.upperRun || sig.b64Run {
		t.Errorf("prescan raised a signal on plain prose: %+v", sig)
	}
}
