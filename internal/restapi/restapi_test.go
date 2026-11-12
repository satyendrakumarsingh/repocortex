package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/mcp"
)

// fakeInvoker stands in for the MCP server, so these tests are about the HTTP
// envelope alone. Parity with the real tools is asserted where the real tools
// are: internal/mcp's invoker tests and internal/cli's HTTP tests.
type fakeInvoker struct {
	tools    []mcp.ToolInfo
	lastName string
	lastArgs string
	result   mcp.CallResult
	callErr  error
	listErr  error
}

func (f *fakeInvoker) Tools(context.Context) ([]mcp.ToolInfo, error) {
	return f.tools, f.listErr
}

func (f *fakeInvoker) Call(_ context.Context, name string, args json.RawMessage) (mcp.CallResult, error) {
	f.lastName, f.lastArgs = name, string(args)
	if f.callErr != nil {
		return mcp.CallResult{}, f.callErr
	}
	for _, t := range f.tools {
		if t.Name == name {
			return f.result, nil
		}
	}
	return mcp.CallResult{}, fmt.Errorf("%w %q", mcp.ErrUnknownTool, name)
}

func newFake() *fakeInvoker {
	return &fakeInvoker{
		tools: []mcp.ToolInfo{
			{
				Name:            "analyze_impact",
				Description:     "Before changing a symbol, find out what depends on it. More detail follows.",
				InputSchema:     json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"]}`),
				EstimatedTokens: 40,
			},
			{
				Name:            "find_callers",
				Description:     "Find the functions and methods that call a symbol.",
				InputSchema:     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
				EstimatedTokens: 30,
			},
		},
		result: mcp.CallResult{
			Text:       "=== SYSTEM METADATA ===\n2 caller(s) of Save.",
			Structured: json.RawMessage(`{"summary":"2 caller(s) of Save."}`),
		},
	}
}

func serve(t *testing.T, inv Invoker) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(inv, Info{Project: "demo", Version: "1.2.3", Profile: "standard"}))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func decode[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("response is not the expected JSON: %v\n%s", err, body)
	}
	return v
}

func TestIndexSaysWhatThisIsAndWhereToGo(t *testing.T) {
	srv := serve(t, newFake())
	for _, path := range []string{BasePath, BasePath + "/"} {
		resp, body := do(t, http.MethodGet, srv.URL+path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d\n%s", path, resp.StatusCode, body)
		}
		got := decode[struct {
			Project string            `json:"project"`
			Version string            `json:"version"`
			Profile string            `json:"profile"`
			Links   map[string]string `json:"links"`
		}](t, body)
		if got.Project != "demo" || got.Version != "1.2.3" || got.Profile != "standard" {
			t.Errorf("index = %+v", got)
		}
		if got.Links["openapi"] != BasePath+"/openapi.json" || got.Links["tools"] != BasePath+"/tools" {
			t.Errorf("links = %v", got.Links)
		}
	}
}

func TestToolsListsEveryToolWithItsCost(t *testing.T) {
	srv := serve(t, newFake())
	resp, body := do(t, http.MethodGet, srv.URL+BasePath+"/tools", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	got := decode[struct {
		Tools []mcp.ToolInfo `json:"tools"`
		Total int            `json:"total_estimated_tokens"`
	}](t, body)
	if len(got.Tools) != 2 || got.Total != 70 {
		t.Errorf("tools %d, total %d; want 2 and 70", len(got.Tools), got.Total)
	}
}

func TestDescribeReturnsOneTool(t *testing.T) {
	srv := serve(t, newFake())
	resp, body := do(t, http.MethodGet, srv.URL+BasePath+"/tools/analyze_impact", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	if got := decode[mcp.ToolInfo](t, body); got.Name != "analyze_impact" {
		t.Errorf("described %q", got.Name)
	}

	// A tool has one name. An old spelling is simply not found.
	for _, name := range []string{"no_such_tool", "impact_analysis"} {
		resp, body = do(t, http.MethodGet, srv.URL+BasePath+"/tools/"+name, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, resp.StatusCode)
		}
		if e := decode[ErrorResponse](t, body); e.Error.Code != "unknown_tool" || !strings.Contains(e.Error.Message, "standard") {
			t.Errorf("%s: error = %+v", name, e.Error)
		}
	}
}

func TestCallForwardsArgumentsAndReturnsTextAndResult(t *testing.T) {
	fake := newFake()
	srv := serve(t, fake)

	resp, body := do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", `{"name":"Save"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	if fake.lastName != "find_callers" || fake.lastArgs != `{"name":"Save"}` {
		t.Errorf("forwarded (%q, %q)", fake.lastName, fake.lastArgs)
	}
	got := decode[CallResponse](t, body)
	if got.Tool != "find_callers" || got.IsError || !strings.Contains(got.Text, "2 caller(s)") {
		t.Errorf("response = %+v", got)
	}
	if string(got.Result) != `{"summary":"2 caller(s) of Save."}` {
		t.Errorf("structured result = %s", got.Result)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; answers describe private source and must not be cached", cc)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestCallWithNoBodyIsACallWithNoArguments(t *testing.T) {
	fake := newFake()
	srv := serve(t, fake)
	resp, body := do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	if fake.lastArgs != "" {
		t.Errorf("forwarded args %q, want none", fake.lastArgs)
	}
}

// A tool's own failure is an answer, not an HTTP failure: a client that
// retries on 5xx must not retry "you asked for a negative limit".
func TestToolFailureIs200WithTheFlagSet(t *testing.T) {
	fake := newFake()
	fake.result = mcp.CallResult{Text: "name must not be empty", IsError: true}
	srv := serve(t, fake)

	resp, body := do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", `{"name":""}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200\n%s", resp.StatusCode, body)
	}
	if got := decode[CallResponse](t, body); !got.IsError {
		t.Error("is_error should be true")
	}
}

func TestCallReturnsPlainTextWhenAsked(t *testing.T) {
	fake := newFake()
	srv := serve(t, fake)

	resp, body := do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", `{"name":"Save"}`, "Accept", "text/plain")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if body != fake.result.Text {
		t.Errorf("body = %q, want the tool's text and nothing else", body)
	}

	fake.result.IsError = true
	resp, _ = do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", `{}`, "Accept", "text/plain")
	if resp.Header.Get("X-RepoCortex-Tool-Error") != "true" {
		t.Error("a text response must still carry the tool's failure flag, in a header")
	}
}

func TestBadRequestsGetAStatusAndAMachineReadableCode(t *testing.T) {
	srv := serve(t, newFake())
	base := srv.URL + BasePath

	for _, tc := range []struct {
		name, method, url, body string
		status                  int
		code                    string
	}{
		{"array body", http.MethodPost, base + "/tools/find_callers", `["Save"]`, 400, "bad_request"},
		{"truncated JSON", http.MethodPost, base + "/tools/find_callers", `{"name":`, 400, "bad_request"},
		{"unknown tool", http.MethodPost, base + "/tools/delete_everything", `{}`, 404, "unknown_tool"},
		{"wrong method on a tool", http.MethodDelete, base + "/tools/find_callers", ``, 405, "method_not_allowed"},
		{"wrong method on the list", http.MethodPost, base + "/tools", ``, 405, "method_not_allowed"},
		{"nested path", http.MethodPost, base + "/tools/find_callers/extra", `{}`, 404, "not_found"},
		{"unknown route", http.MethodGet, base + "/secrets", ``, 404, "not_found"},
		{"oversized body", http.MethodPost, base + "/tools/find_callers", `{"name":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, 413, "body_too_large"},
	} {
		resp, body := do(t, tc.method, tc.url, tc.body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d\n%s", tc.name, resp.StatusCode, tc.status, body)
			continue
		}
		if e := decode[ErrorResponse](t, body); e.Error.Code != tc.code || e.Error.Message == "" {
			t.Errorf("%s: error = %+v, want code %q and a message", tc.name, e.Error, tc.code)
		}
	}
}

// An internal failure must not hand its error string to the client.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	fake := newFake()
	fake.callErr = errors.New("sql: database /Users/someone/private/path is locked")
	srv := serve(t, fake)

	resp, body := do(t, http.MethodPost, srv.URL+BasePath+"/tools/find_callers", `{"name":"Save"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
	if strings.Contains(body, "private/path") || strings.Contains(body, "sql:") {
		t.Errorf("the internal error leaked:\n%s", body)
	}

	fake = newFake()
	fake.listErr = errors.New("boom at /secret")
	srv = serve(t, fake)
	for _, path := range []string{"/tools", "/tools/find_callers", "/openapi.json"} {
		resp, body := do(t, http.MethodGet, srv.URL+BasePath+path, "")
		if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, "/secret") {
			t.Errorf("GET %s: status %d, body %s", path, resp.StatusCode, body)
		}
	}
}

func TestOpenAPIHasOneOperationPerToolNamedAfterIt(t *testing.T) {
	srv := serve(t, newFake())
	resp, body := do(t, http.MethodGet, srv.URL+BasePath+"/openapi.json", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	doc := decode[struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			OperationID string   `json:"operationId"`
			Summary     string   `json:"summary"`
			Tags        []string `json:"tags"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Type     string   `json:"type"`
						Required []string `json:"required"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			SecuritySchemes map[string]struct {
				Scheme string `json:"scheme"`
			} `json:"securitySchemes"`
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
		Security []map[string][]string `json:"security"`
	}](t, body)

	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0 so tool schemas embed unchanged", doc.OpenAPI)
	}
	if doc.Info.Title != "RepoCortex: demo" || doc.Info.Version != "1.2.3" {
		t.Errorf("info = %+v", doc.Info)
	}
	if len(doc.Paths) != 2 {
		t.Fatalf("paths = %d, want one per tool", len(doc.Paths))
	}
	op, ok := doc.Paths[BasePath+"/tools/analyze_impact"]["post"]
	if !ok {
		t.Fatalf("no POST operation for analyze_impact: %v", doc.Paths)
	}
	if op.OperationID != "analyze_impact" {
		t.Errorf("operationId = %q; it must be the tool name so function-calling clients see MCP's names", op.OperationID)
	}
	if op.Summary != "Before changing a symbol, find out what depends on it." {
		t.Errorf("summary = %q, want the description's first sentence", op.Summary)
	}
	if len(op.Tags) != 1 || op.Tags[0] != "analyze" {
		t.Errorf("tags = %v, want the tool's verb", op.Tags)
	}
	schema := op.RequestBody.Content["application/json"].Schema
	if schema.Type != "object" || len(schema.Required) != 1 || schema.Required[0] != "symbol" {
		t.Errorf("the tool's input schema was not embedded unchanged: %+v", schema)
	}
	for _, code := range []string{"200", "400", "401", "404", "413"} {
		if _, ok := op.Responses[code]; !ok {
			t.Errorf("response %s is not documented", code)
		}
	}
	if doc.Components.SecuritySchemes["bearer"].Scheme != "bearer" || len(doc.Security) != 1 {
		t.Errorf("bearer authentication is not declared: %+v %+v", doc.Components.SecuritySchemes, doc.Security)
	}
	for _, name := range []string{"CallResponse", "Error"} {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schema %s is referenced but not defined", name)
		}
	}
}

func TestFirstSentence(t *testing.T) {
	for in, want := range map[string]string{
		"Find callers. Then more.":           "Find callers.",
		"List what a symbol calls: the rest": "List what a symbol calls",
		"Short":                              "Short",
		strings.Repeat("a", 200):             strings.Repeat("a", 120),
	} {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
