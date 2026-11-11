package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

const testToken = "0123456789abcdef0123456789abcdef"

// httpTestServer serves the real RepoCortex MCP server, over an empty index,
// through the HTTP handler.
func httpTestServer(t *testing.T, allowedHosts ...string) (*httptest.Server, []string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "index.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, time.Now().Unix); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv, err := devmcp.New(devmcp.Deps{
		Registry: registry.New(db),
		Graph:    graph.New(db),
		Config:   config.Config{MCPProfile: config.MCPMinimal},
	})
	if err != nil {
		t.Fatalf("build mcp server: %v", err)
	}
	h := newHTTPMCPHandler(srv.SDK(), httpServeOptions{Token: testToken, AllowedHosts: allowedHosts},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, srv.Tools()
}

type headerRT struct {
	headers map[string]string
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint string, headers map[string]string) (*sdk.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	return client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerRT{headers}},
		MaxRetries: -1,
	}, nil)
}

func listedTools(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestHTTPServesTheSameToolsAsStdio(t *testing.T) {
	ts, want := httpTestServer(t)
	cs, err := connect(t, ts.URL+mcpPath, map[string]string{"Authorization": "Bearer " + testToken})
	if err != nil {
		t.Fatalf("connect with bearer token: %v", err)
	}
	defer func() { _ = cs.Close() }()
	got := listedTools(t, cs)
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools over HTTP = %v, want %v", got, want)
	}

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_project_overview", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call tool over HTTP: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("tool call over HTTP returned no content")
	}
}

func TestHTTPAcceptsTheTokenAsAPathSegment(t *testing.T) {
	ts, _ := httpTestServer(t)
	cs, err := connect(t, ts.URL+mcpPath+"/"+testToken, nil)
	if err != nil {
		t.Fatalf("connect with token in path: %v", err)
	}
	defer func() { _ = cs.Close() }()
	if len(listedTools(t, cs)) == 0 {
		t.Fatal("no tools listed")
	}
}

func post(t *testing.T, url string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestHTTPRejectsMissingOrWrongTokens(t *testing.T) {
	ts, _ := httpTestServer(t)
	cases := []struct {
		name    string
		url     string
		headers map[string]string
		want    int
	}{
		{"no token", ts.URL + mcpPath, nil, http.StatusUnauthorized},
		{"wrong bearer", ts.URL + mcpPath, map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"not a bearer", ts.URL + mcpPath, map[string]string{"Authorization": "Basic " + testToken}, http.StatusUnauthorized},
		{"wrong path token", ts.URL + mcpPath + "/" + strings.Repeat("x", len(testToken)), nil, http.StatusUnauthorized},
		{"empty path token", ts.URL + mcpPath + "/", nil, http.StatusUnauthorized},
		{"other path", ts.URL + "/", map[string]string{"Authorization": "Bearer " + testToken}, http.StatusNotFound},
	}
	for _, c := range cases {
		if got := post(t, c.url, c.headers); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	if _, err := connect(t, ts.URL+mcpPath, nil); err == nil {
		t.Error("an MCP client connected without a token")
	}
}

func TestHTTPChecksHostAndOrigin(t *testing.T) {
	ts, _ := httpTestServer(t, "cortex.example.dev")
	auth := "Bearer " + testToken
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"rebinding host", map[string]string{"Authorization": auth, "Host": "evil.example.com"}, http.StatusForbidden},
		{"allowed tunnel host", map[string]string{"Authorization": auth, "Host": "cortex.example.dev"}, http.StatusOK},
		{"browser page origin", map[string]string{"Authorization": auth, "Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"localhost origin", map[string]string{"Authorization": auth, "Origin": "http://localhost:3000"}, http.StatusOK},
		{"allowed origin", map[string]string{"Authorization": auth, "Origin": "https://cortex.example.dev"}, http.StatusOK},
	}
	for _, c := range cases {
		got := post(t, ts.URL+mcpPath, c.headers)
		// A ping before initialize may be answered with an MCP-level error,
		// which is still a request that got past every check.
		passed := got != http.StatusForbidden && got != http.StatusUnauthorized
		if (c.want == http.StatusOK) != passed || (c.want == http.StatusForbidden && got != http.StatusForbidden) {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCheckListenAddrKeepsTheServerLocalByDefault(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7788", "localhost:7788", "[::1]:7788"} {
		if err := checkListenAddr(addr, false); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:7788", ":7788", "192.168.1.5:7788"} {
		if err := checkListenAddr(addr, false); err == nil {
			t.Errorf("%s was allowed without --allow-remote", addr)
		}
		if err := checkListenAddr(addr, true); err != nil {
			t.Errorf("%s with --allow-remote: %v", addr, err)
		}
	}
	for _, addr := range []string{"7788", "127.0.0.1", "127.0.0.1:"} {
		if err := checkListenAddr(addr, true); err == nil {
			t.Errorf("%q accepted as a listen address", addr)
		}
	}
}

func TestResolveTokenUsesTheEnvironmentOrGeneratesOne(t *testing.T) {
	t.Setenv(mcpTokenEnv, "")
	a, gen, err := resolveToken()
	if err != nil || !gen || len(a) < minTokenLen {
		t.Fatalf("generated token %q gen=%v err=%v", a, gen, err)
	}
	b, _, _ := resolveToken()
	if a == b {
		t.Error("two generated tokens are equal")
	}

	t.Setenv(mcpTokenEnv, testToken)
	if got, gen, err := resolveToken(); err != nil || gen || got != testToken {
		t.Errorf("env token: got %q gen=%v err=%v", got, gen, err)
	}

	t.Setenv(mcpTokenEnv, "short")
	if _, _, err := resolveToken(); err == nil {
		t.Error("a short token was accepted")
	}
}

func TestServeHTTPStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil),
			httpServeOptions{Addr: "127.0.0.1:0", Token: testToken}, slog.New(slog.NewTextHandler(io.Discard, nil)), &out, "from "+mcpTokenEnv)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveHTTP did not stop when its context ended")
	}
	if strings.Contains(out.String(), testToken) {
		t.Error("the token was printed")
	}
	if !strings.Contains(out.String(), "MCP over HTTP: http://127.0.0.1:") {
		t.Errorf("start-up output lacks the endpoint:\n%s", out.String())
	}
}

func TestServeHTTPRefusesARemoteAddressWithoutTheFlag(t *testing.T) {
	err := serveHTTP(context.Background(), sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil),
		httpServeOptions{Addr: "0.0.0.0:0", Token: testToken}, slog.New(slog.NewTextHandler(io.Discard, nil)), io.Discard, "")
	if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Fatalf("err = %v, want a refusal naming --allow-remote", err)
	}
}

func TestServeRejectsHTTPOnlyFlagsWithoutHTTP(t *testing.T) {
	for _, args := range [][]string{{"serve", "--allow-remote"}, {"serve", "--allowed-host", "x.example"}} {
		root := isolate(t)
		_, err := run(t, root, args...)
		if err == nil || !strings.Contains(err.Error(), "--http") {
			t.Errorf("%v: err = %v, want one naming --http", args, err)
		}
	}
}

func TestWriteTokenFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-token")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeTokenFile(path, testToken); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != testToken {
		t.Fatalf("token file = %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
		}
	}
}
