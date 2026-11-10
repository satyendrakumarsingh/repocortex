package security

import (
	"strings"
	"testing"
)

// Redaction runs over every byte RepoCortex serves, and the overwhelmingly common
// case is a file with no secrets in it at all. BenchmarkRedactClean is
// therefore the number that matters: it is paid on every response whether or
// not anything is found.

// cleanSource is ~4 KB of ordinary Go, including the shapes most likely to
// provoke a false positive (a Password field, a comment about passwords, a
// config struct) so the benchmark measures the real cost of rejecting them.
var cleanSource = buildCleanSource()

func buildCleanSource() string {
	block := `package registry

import (
	"context"
	"database/sql"
	"fmt"
)

// Credentials carries the connection settings for one datastore. The Password
// field is populated from the environment at startup and is never logged.
type Credentials struct {
	Host     string ` + "`json:\"host\"`" + `
	Port     int    ` + "`json:\"port\"`" + `
	Username string ` + "`json:\"username\"`" + `
	Password string ` + "`json:\"password\"`" + `
	Database string ` + "`json:\"database\"`" + `
}

// Open dials the datastore described by c. The caller owns the returned handle.
func Open(ctx context.Context, c Credentials) (*sql.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=require",
		c.Host, c.Port, c.Username, c.Database)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("registry: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("registry: ping: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	return db, nil
}

func authenticate(ctx context.Context, db *sql.DB, user, password string) (int64, error) {
	var id int64
	var hash string
	row := db.QueryRowContext(ctx, "SELECT id, password_hash FROM users WHERE name = $1", user)
	if err := row.Scan(&id, &hash); err != nil {
		return 0, fmt.Errorf("registry: lookup %q: %w", user, err)
	}
	if !compareHash(hash, password) {
		return 0, errInvalidCredentials
	}
	return id, nil
}
`
	var sb strings.Builder
	for sb.Len() < 4096 {
		sb.WriteString(block)
	}
	return sb.String()[:4096]
}

// secretSource is the same size with several real detections in it, so the two
// numbers are comparable.
var secretSource = buildSecretSource()

func buildSecretSource() string {
	head := `# generated deployment configuration
DB_PASSWORD=hunter2notarealpassword
AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE
AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB
DATABASE_URL=postgres://orders:s3cr3tvalue@db.internal:5432/orders
STRIPE_KEY=sk_live_0123456789abcdefEXAMPLE
`
	return head + cleanSource[:4096-len(head)]
}

func BenchmarkRedactClean(b *testing.B) {
	b.SetBytes(int64(len(cleanSource)))
	b.ReportAllocs()
	for b.Loop() {
		if _, f := Redact(cleanSource); len(f) != 0 {
			b.Fatalf("clean fixture is not clean: %v", f)
		}
	}
}

func BenchmarkRedactSecrets(b *testing.B) {
	if _, f := Redact(secretSource); len(f) < 6 {
		b.Fatalf("secret fixture only yields %d findings", len(f))
	}
	b.SetBytes(int64(len(secretSource)))
	b.ReportAllocs()
	for b.Loop() {
		Redact(secretSource)
	}
}

// BenchmarkRedactProse measures the fast path doing its job: text with no
// candidate byte at all never reaches a regexp.
func BenchmarkRedactProse(b *testing.B) {
	prose := strings.Repeat("the quick brown fox jumps over the lazy dog and keeps on running ", 64)
	b.SetBytes(int64(len(prose)))
	b.ReportAllocs()
	for b.Loop() {
		Redact(prose)
	}
}

func BenchmarkDetectClean(b *testing.B) {
	b.SetBytes(int64(len(cleanSource)))
	b.ReportAllocs()
	for b.Loop() {
		Detect(cleanSource)
	}
}

func BenchmarkIsSensitivePath(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		IsSensitivePath("internal/registry/read.go")
	}
}
