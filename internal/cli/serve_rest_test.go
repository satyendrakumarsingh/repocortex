package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/restapi"
)

// These tests serve a REAL index through the real handler stack: the HTTP
// guard, the REST envelope, the in-process invoker and the MCP server. The
// claim under test is the one the design rests on — one tool surface, three
// protocols — so the central assertion compares the bytes an MCP client
// receives over HTTP with the bytes a REST client receives for the same call.

// restTestServer serves queryFixture's index over HTTP with both front ends.
func restTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	root := queryFixture(t)

	g := &Global{Project: root, Quiet: true}
	cmd := NewRoot()
	cmd.SetArgs([]string{"--project", root, "--quiet", "version"})
	cmd.SetOut(io.Discard)
	if err := g.setup(cmd); err != nil {
		t.Fatalf("configure: %v", err)
	}

	ctx := context.Background()
	srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPFull})
	if err != nil {
		t.Fatalf("open agent server: %v", err)
	}
	t.Cleanup(closeServer)

	inv, err := srv.NewInvoker(ctx, "rest")
	if err != nil {
		t.Fatalf("invoker: %v", err)
	}
	t.Cleanup(func() { _ = inv.Close() })

	api := restapi.Handler(inv, restapi.Info{Project: "fixture", Version: "test", Profile: "full"})
	h := newHTTPHandler(srv.SDK(), api, httpServeOptions{Token: testToken},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func restDo(t *testing.T, method, url, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

var bearer = map[string]string{"Authorization": "Bearer " + testToken}

// The REST API is behind the same token as MCP. Unlike MCP it does not accept
// the token in the path: a caller that can make a REST request can set a
// header, and a token in a path is a token in every access log.
func TestRESTRequiresTheBearerToken(t *testing.T) {
	ts := restTestServer(t)

	if status, _ := restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/tools", "", nil); status != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", status)
	}
	wrong := map[string]string{"Authorization": "Bearer " + strings.Repeat("x", len(testToken))}
	if status, _ := restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/tools", "", wrong); status != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", status)
	}
	if status, _ := restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/"+testToken, "", nil); status == http.StatusOK {
		t.Error("the token must not be accepted as a path segment on the REST API")
	}
	if status, body := restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/tools", "", bearer); status != http.StatusOK {
		t.Errorf("right token: status %d\n%s", status, body)
	}
}

// The liveness probe needs no credential and reveals nothing.
func TestHealthzIsOpenAndSaysNothing(t *testing.T) {
	ts := restTestServer(t)
	status, body := restDo(t, http.MethodGet, ts.URL+healthPath, "", nil)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("healthz = %d %q", status, body)
	}
	if status, _ := restDo(t, http.MethodPost, ts.URL+healthPath, "", nil); status != http.StatusMethodNotAllowed {
		t.Errorf("POST healthz = %d, want 405", status)
	}
}

// The same Host and Origin checks guard both front ends.
func TestRESTSharesTheHostAndOriginGuard(t *testing.T) {
	ts := restTestServer(t)
	url := ts.URL + restapi.BasePath + "/tools"

	evil := map[string]string{"Authorization": "Bearer " + testToken, "Origin": "https://evil.example"}
	if status, _ := restDo(t, http.MethodGet, url, "", evil); status != http.StatusForbidden {
		t.Errorf("a foreign Origin reached the REST API: status %d, want 403", status)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host reached the REST API: status %d, want 403", resp.StatusCode)
	}
}

// One tool surface, three protocols: an MCP client over HTTP and a REST client
// must receive the same text for the same call, tool for tool.
func TestRESTAndMCPReturnTheSameAnswer(t *testing.T) {
	ts := restTestServer(t)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"find_callers", map[string]any{"name": "Save"}},
		{"find_callees", map[string]any{"name": "Settle"}},
		{"trace_call_path", map[string]any{"from": "Settle", "to": "Save"}},
		{"analyze_impact", map[string]any{"symbol": "Save"}},
		{"find_symbol", map[string]any{"name": "PaymentService"}},
		{"list_dead_code", map[string]any{}},
		{"get_resolution_coverage", map[string]any{}},
	} {
		// A fresh MCP session per call, so the protocol side has no session
		// history and the comparison is of one answer against one answer.
		cs, err := connect(t, ts.URL+mcpPath, bearer)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
		_ = cs.Close()
		if err != nil {
			t.Fatalf("%s over MCP: %v", tc.tool, err)
		}
		var viaMCP strings.Builder
		for _, c := range res.Content {
			if text, ok := c.(*sdk.TextContent); ok {
				viaMCP.WriteString(text.Text)
			}
		}

		payload, _ := json.Marshal(tc.args)
		status, body := restDo(t, http.MethodPost, ts.URL+restapi.BasePath+"/tools/"+tc.tool, string(payload), bearer)
		if status != http.StatusOK {
			t.Fatalf("%s over REST: status %d\n%s", tc.tool, status, body)
		}
		var viaREST restapi.CallResponse
		if err := json.Unmarshal([]byte(body), &viaREST); err != nil {
			t.Fatalf("%s over REST: not JSON: %v\n%s", tc.tool, err, body)
		}

		if stripAge(viaREST.Text) != stripAge(viaMCP.String()) {
			t.Errorf("%s: REST and MCP disagree\n--- REST\n%s\n--- MCP\n%s", tc.tool, viaREST.Text, viaMCP.String())
		}
		if viaREST.IsError != res.IsError {
			t.Errorf("%s: is_error %v over REST, %v over MCP", tc.tool, viaREST.IsError, res.IsError)
		}
		if viaREST.Text == "" {
			t.Errorf("%s: empty answer", tc.tool)
		}
	}
}

// And the third protocol: the shell prints the same text again.
func TestCLIToolCallMatchesREST(t *testing.T) {
	root := queryFixture(t)
	viaCLI, err := run(t, root, "tool", "call", "find_callers", "name=Save")
	if err != nil {
		t.Fatalf("tool call: %v\n%s", err, viaCLI)
	}
	// The fixture is rebuilt per test, so compare the parts that do not depend
	// on which temporary directory it was built in: the answer lines.
	for _, want := range []string{"2 caller(s) of Save", "- calls Settle", "- calls Refund"} {
		if !strings.Contains(viaCLI, want) {
			t.Errorf("CLI answer is missing %q:\n%s", want, viaCLI)
		}
	}
}

// The OpenAPI document is generated from the tools actually served, so it
// must list exactly them.
func TestOpenAPIListsExactlyTheServedTools(t *testing.T) {
	ts := restTestServer(t)

	status, body := restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/tools", "", bearer)
	if status != http.StatusOK {
		t.Fatalf("tools: %d\n%s", status, body)
	}
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatal(err)
	}

	status, body = restDo(t, http.MethodGet, ts.URL+restapi.BasePath+"/openapi.json", "", bearer)
	if status != http.StatusOK {
		t.Fatalf("openapi: %d\n%s", status, body)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) != len(listed.Tools) || len(listed.Tools) == 0 {
		t.Fatalf("openapi documents %d operations, the server lists %d tools", len(doc.Paths), len(listed.Tools))
	}
	for _, tool := range listed.Tools {
		op := doc.Paths[restapi.BasePath+"/tools/"+tool.Name]["post"]
		if op.OperationID != tool.Name {
			t.Errorf("tool %q has no matching operation in the OpenAPI document", tool.Name)
		}
	}
}
