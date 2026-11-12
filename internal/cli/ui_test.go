package cli

import (
	stdctx "context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"

	"github.com/spf13/cobra"
)

// `repocortex ui` is the one command whose defaults are a security decision, so
// most of what is worth testing here is about the bind address and about the
// command actually serving what it claims to serve.

func TestUILoopbackDetection(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"127.0.0.1":   true,
		"127.5.5.5":   true,
		"::1":         true,
		"[::1]":       true,
		"localhost":   true,
		"LOCALHOST":   true,
		"":            true,
		"0.0.0.0":     false,
		"::":          false,
		"10.0.0.5":    false,
		"192.168.1.9": false,
		// An unresolvable name is treated as NOT loopback: when the answer is
		// unknown, the safe assumption is the one that keeps the Host guard on.
		"some-host-that-does-not-resolve": false,
	}
	for host, want := range cases {
		if got := isLoopbackAddr(host); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestUIDisplayURLRewritesWildcardBinds(t *testing.T) {
	t.Parallel()
	addr := &net.TCPAddr{IP: net.IPv4zero, Port: 4321}
	for _, host := range []string{"0.0.0.0", "::", "[::]", ""} {
		got := displayURL(host, addr)
		if !strings.Contains(got, "localhost:4321") {
			t.Errorf("displayURL(%q) = %q, want a pasteable localhost URL", host, got)
		}
	}
	if got := displayURL("127.0.0.1", addr); got != "http://127.0.0.1:4321/" {
		t.Errorf("displayURL(127.0.0.1) = %q", got)
	}
}

func TestUIPrintURLUsesLoopbackByDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out, err := runUICommand(t, dir, "--print-url")
	if err != nil {
		t.Fatalf("ui --print-url: %v", err)
	}
	// The default must be loopback. If this ever becomes 0.0.0.0 the portal
	// starts publishing an entire codebase's structure to the network the
	// moment someone runs it on a shared machine.
	if !strings.Contains(out, "http://127.0.0.1:7777/") {
		t.Errorf("default URL = %q, want http://127.0.0.1:7777/", strings.TrimSpace(out))
	}
}

func TestUIRejectsAnOutOfRangePort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := runUICommand(t, dir, "--port", "70000"); err == nil {
		t.Fatal("--port 70000 was accepted")
	}
}

func TestUIWithoutAnIndexNamesTheFix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := runUICommand(t, dir)
	if err == nil {
		t.Fatal("ui started against a directory with no index")
	}
	// There is no database to open at all here, so the guidance has to come
	// from the command. (Once a store exists, the portal itself answers an
	// unindexed repository with guidance rather than an error — that case is
	// covered in internal/web.)
	if !strings.Contains(err.Error(), "repocortex init") || !strings.Contains(err.Error(), "repocortex index") {
		t.Errorf("error does not name the fix: %v", err)
	}
}

// TestUIServesTheReadOnlyPortalOverAnEphemeralPort starts the real command
// against a real (empty but committed) index and drives it over HTTP.
//
// This is the only test that proves the wiring: that the CLI opens the index
// read-only, builds the capability dependencies the screens need, binds where it
// says it binds, and answers.
func TestUIServesTheReadOnlyPortalOverAnEphemeralPort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedIndex(t, dir)

	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	defer cancel()

	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- execUI(ctx, dir, buf, "--port", "0", "--json") }()

	url := waitForURL(t, buf)

	// Health: the server is up and says what it is.
	var health map[string]any
	getJSON(t, url+"api/health", &health)
	if health["status"] != "ok" {
		t.Errorf("health status = %v", health["status"])
	}
	if health["read_only"] != true {
		t.Error("the portal does not report itself read-only")
	}
	if health["bound_to_local"] != true {
		t.Error("a default bind must report bound_to_local")
	}

	// The four screens answer over the wire.
	for _, path := range []string{"api/overview", "api/graph", "api/blast?symbol=x", "api/governance"} {
		var m map[string]any
		getJSON(t, url+path, &m)
		if m["meta"] == nil {
			t.Errorf("%s returned no meta block", path)
		}
	}

	// The shell and its assets are served from the binary.
	body := getText(t, url)
	if !strings.Contains(body, "<canvas") {
		t.Error("the served shell has no canvas")
	}
	if code := statusOf(t, http.MethodPost, url+"api/overview"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST returned %d, want 405: the portal is read-only", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ui exited with %v, want a clean shutdown on context cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ui did not shut down within 10s of cancellation")
	}
}

// ---------------------------------------------------------------- helpers

// runUICommand runs `ui` to completion and returns stdout. Only for the flag
// paths that do not listen.
func runUICommand(t *testing.T, project string, args ...string) (string, error) {
	t.Helper()
	buf := &syncBuffer{}
	err := execUI(stdctx.Background(), project, buf, args...)
	return buf.String(), err
}

// execUI builds the minimal command tree `ui` needs.
//
// A hand-built root rather than NewRoot(): registering the command in root.go is
// somebody else's change, and a test that depended on it would fail for a reason
// that has nothing to do with this command. The persistent --json flag is
// declared here because the real root declares it, and `ui` reads Global.JSON.
func execUI(ctx stdctx.Context, project string, out io.Writer, args ...string) error {
	g := &Global{Project: project}
	root := &cobra.Command{Use: "repocortex-test", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&g.JSON, "json", false, "emit machine-readable JSON on stdout")
	cmd := newUICmd(g)
	root.AddCommand(cmd)
	if err := g.setup(cmd); err != nil {
		return err
	}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(append([]string{"ui"}, args...))
	return root.ExecuteContext(ctx)
}

// seedIndex creates a migrated store with one committed run, which is the
// minimum a reader can point the portal at.
func seedIndex(t *testing.T, project string) {
	t.Helper()
	ctx := stdctx.Background()
	dir := filepath.Join(project, IndexDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, IndexFile)})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.Migrate(ctx, db.DB, time.Now().Unix); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO index_runs (status, committed, started_unix, finished_unix, accuracy_mode)
		VALUES ('ready', 1, ?, ?, 'fast')`, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

// waitForURL polls the command's JSON output for the address it bound to. The
// port is ephemeral, so the listener is the only thing that knows it.
func waitForURL(t *testing.T, buf *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var m map[string]any
		if err := json.Unmarshal([]byte(buf.String()), &m); err == nil {
			if u, ok := m["url"].(string); ok && u != "" {
				if m["read_only"] != true {
					t.Errorf("startup JSON does not report read_only: %v", m)
				}
				if m["loopback"] != true {
					t.Errorf("a default bind reported loopback=%v", m["loopback"])
				}
				return u
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ui never printed a URL; output so far: %q", buf.String())
	return ""
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	body := getText(t, url)
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("GET %s: decode %q: %v", url, body, err)
	}
}

func getText(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", url, res.StatusCode, body)
	}
	return string(body)
}

func statusOf(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// syncBuffer is a bytes.Buffer the test can read while the command writes.
// Without the lock this is a data race the -race gate would (correctly) fail on.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
