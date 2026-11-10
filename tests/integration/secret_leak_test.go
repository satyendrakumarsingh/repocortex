package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Secret-leak tests.
//
// These exist because a redaction engine that nothing calls is not a security
// control. `internal/security` was written, fully tested, and imported by
// NOTHING — every credential in a repository was being indexed verbatim and
// could be served to an agent. Unit tests of the redaction package all passed
// throughout.
//
// So these tests deliberately assert at the SYSTEM boundary: index a repository
// containing planted credentials, then ask through the real MCP interface and
// require that no secret value appears anywhere in the response.

// canaries are obviously-fake credentials. Each must never appear in any
// agent-facing output.
var canaries = map[string]string{
	"aws_secret":     "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
	"github_token":   "ghp_16CharactersLongTokenAAAAAAAAAAAAAAA",
	"db_password":    "sup3rSecretDbP4ssw0rd",
	"stripe_key":     "sk_live_51AbCdEfGhIjKlMnOpQrStUvWxYz",
	"conn_string_pw": "hunter2wombat",
}

func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		// A credential committed into ordinary source — the common real case.
		"src/config.go": `package config

// Deployment settings.
const (
	Region      = "us-east-1"
	AWSSecret   = "` + canaries["aws_secret"] + `"
	GithubToken = "` + canaries["github_token"] + `"
)

func Connect() string {
	return "postgres://app:` + canaries["conn_string_pw"] + `@db.internal:5432/orders"
}
`,
		// A whole-file secret. Path classification should suppress it entirely
		// rather than rely on matching each line.
		".env": "DB_PASSWORD=" + canaries["db_password"] + "\nSTRIPE_KEY=" + canaries["stripe_key"] + "\n",

		// Ordinary code that must survive untouched — over-redaction that
		// destroys real source makes the tool useless.
		"src/service.go": `package config

// Password validation rules. The word password appearing in prose or as a
// field name is not a secret.
type Credentials struct {
	Username string
	Password string
}

func (c Credentials) Validate() error { return nil }
`,
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func indexRepo(t *testing.T, root string) (*registry.Registry, int64) {
	t.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	walker, err := fs.NewWalker(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := pipeline.NewRegistrySink(w)
	if _, err := pipeline.NewRunner(walker, pipeline.Options{}).Run(ctx, sink); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("secrets redacted during indexing: %d", sink.SecretsRedacted())
	return reg, run.ID
}

// TestSecretsNeverEnterTheIndex is the first barrier. A credential that never
// reaches storage cannot leak from a backup, a future tool, or a bug.
func TestSecretsNeverEnterTheIndex(t *testing.T) {
	ctx := context.Background()
	root := writeRepo(t)
	reg, runID := indexRepo(t, root)

	files, err := reg.Files(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		excerpt, err := reg.FileExcerpt(ctx, runID, f.Path, 1, 500)
		if err != nil {
			continue // no indexed text for this file
		}
		for name, secret := range canaries {
			if strings.Contains(excerpt, secret) {
				t.Errorf("SECRET IN INDEX: %s (%s) stored verbatim in %s", name, secret, f.Path)
			}
		}
	}
}

// TestSecretsNeverReachAnAgent is the barrier that actually matters: content
// crossing this boundary can leave the machine.
func TestSecretsNeverReachAnAgent(t *testing.T) {
	ctx := context.Background()
	root := writeRepo(t)
	reg, _ := indexRepo(t, root)

	cfg := config.Defaults()
	cfg.ProjectName = "leaktest"
	srv, err := devmcp.New(devmcp.Deps{Registry: reg, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()

	sess, err := sdk.NewClient(&sdk.Implementation{Name: "probe", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Query the way an attacker-or-curious agent would: search for the
	// credentials directly, then for the files holding them.
	queries := []string{
		"password", "secret", "token", "AWSSecret", "postgres", "config", "Credentials",
	}
	for _, q := range queries {
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{
			Name: "search_code", Arguments: map[string]any{"query": q},
		})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		for name, secret := range canaries {
			if strings.Contains(text.String(), secret) {
				t.Errorf("SECRET LEAKED TO AGENT via search_code(%q): %s = %s", q, name, secret)
			}
		}
	}

	// And through symbol lookup, which returns a different code path.
	for _, sym := range []string{"AWSSecret", "GithubToken", "Connect", "Credentials"} {
		res, err := sess.CallTool(ctx, &sdk.CallToolParams{
			Name: "get_symbol", Arguments: map[string]any{"name": sym},
		})
		if err != nil {
			continue
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		for name, secret := range canaries {
			if strings.Contains(text.String(), secret) {
				t.Errorf("SECRET LEAKED TO AGENT via get_symbol(%q): %s = %s", sym, name, secret)
			}
		}
	}
}

// TestRedactionDoesNotDestroyOrdinaryCode guards the other failure mode. A
// redactor that eats real source is not safe, it is useless, and the pressure
// to disable it would then be entirely reasonable.
func TestRedactionDoesNotDestroyOrdinaryCode(t *testing.T) {
	ctx := context.Background()
	root := writeRepo(t)
	reg, runID := indexRepo(t, root)

	excerpt, err := reg.FileExcerpt(ctx, runID, "src/service.go", 1, 500)
	if err != nil {
		t.Fatalf("ordinary source file is not indexed at all: %v", err)
	}
	for _, want := range []string{"Credentials", "Username", "Password", "Validate"} {
		if !strings.Contains(excerpt, want) {
			t.Errorf("redaction removed ordinary code: %q missing from src/service.go", want)
		}
	}
	if strings.Contains(excerpt, "REDACTED") {
		t.Errorf("a file with no secrets was redacted:\n%s", excerpt)
	}
}
