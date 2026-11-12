// Package restapi serves RepoCortex's tools over plain HTTP and JSON.
//
// # Why this exists beside MCP
//
// MCP is the best way for an agent to reach the index, and many callers cannot
// use it: a model wired up with HTTP function calling, an internal bot, a CI
// step with curl, a language with no MCP client. For them the tools are the
// same and only the envelope differs, so this package is an envelope and
// nothing more.
//
// It holds no handlers of its own. Every call is forwarded to the MCP server
// through an in-process client (mcp.Invoker), which means a tool's validation,
// output bounds, untrusted-content framing and audit record apply here because
// they apply there. The alternative — a second set
// of handlers that drifts from the first — is the failure this design removes.
//
// # Routes
//
//	GET  /api/v1                 what this server is, and where to go next
//	GET  /api/v1/tools           every tool the active profile exposes
//	GET  /api/v1/tools/{name}    one tool: description and input schema
//	POST /api/v1/tools/{name}    call it; the body is the JSON arguments object
//	GET  /api/v1/openapi.json    the same surface as an OpenAPI 3.1 document
//
// Authentication, the Host allowlist and the Origin check are the caller's
// (internal/cli wraps this handler in the same guard as the MCP endpoint), so
// there is one definition of who may talk to the server.
//
// # What a response means
//
// A tool that answered returns 200, including when the answer is "that symbol
// is not in the index" — the tool worked and said so. The tool's own failure
// flag is carried in the body as is_error, exactly as MCP carries isError,
// rather than mapped onto a status code: a client that retries on 5xx must not
// retry a question whose answer is "you asked for a negative limit".
// Status codes describe the HTTP exchange: 400 for a body that is not a JSON
// object, 404 for a tool that does not exist, 405 for the wrong method, 413
// for a body over the limit.
package restapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/mcp"
)

// BasePath is where the API is mounted.
const BasePath = "/api/v1"

// MaxBodyBytes bounds a call's arguments. The largest legitimate argument is a
// diff for minimize_diff; a megabyte is far beyond one a model can use.
const MaxBodyBytes = 1 << 20

// Invoker is the part of mcp.Invoker this package uses. Declared here, at the
// consumer, so the HTTP layer can be tested against a fake without a database.
type Invoker interface {
	Tools(ctx context.Context) ([]mcp.ToolInfo, error)
	Call(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error)
}

var _ Invoker = (*mcp.Invoker)(nil)

// Info identifies the server in the index document and the OpenAPI document.
type Info struct {
	Project string // project name, as configured
	Version string // binary version
	Profile string // active tool profile
}

// CallResponse is the body of a successful tool call.
type CallResponse struct {
	Tool string `json:"tool"`
	// Text is the rendered answer, the same text an MCP client receives.
	Text string `json:"text"`
	// Result is the same answer as structured data.
	Result json.RawMessage `json:"result,omitempty"`
	// IsError is the tool's own failure flag. See the package comment.
	IsError bool `json:"is_error"`
}

// ErrorResponse is the body of a non-2xx response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody names what went wrong in terms a client can branch on.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Handler returns the API's handler. Mount it at BasePath and BasePath + "/".
func Handler(inv Invoker, info Info) http.Handler {
	h := &handler{inv: inv, info: info}
	mux := http.NewServeMux()
	mux.HandleFunc(BasePath, h.index)
	mux.HandleFunc(BasePath+"/", h.index)
	mux.HandleFunc(BasePath+"/tools", h.tools)
	mux.HandleFunc(BasePath+"/tools/", h.tool)
	mux.HandleFunc(BasePath+"/openapi.json", h.openAPI)
	return mux
}

type handler struct {
	inv  Invoker
	info Info
}

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != BasePath && r.URL.Path != BasePath+"/" {
		writeError(w, http.StatusNotFound, "not_found", "no such route; see "+BasePath)
		return
	}
	if !allow(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "RepoCortex",
		"project": h.info.Project,
		"version": h.info.Version,
		"profile": h.info.Profile,
		"links": map[string]string{
			"tools":   BasePath + "/tools",
			"call":    BasePath + "/tools/{name}",
			"openapi": BasePath + "/openapi.json",
		},
	})
}

func (h *handler) tools(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	tools, err := h.inv.Tools(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not list tools")
		return
	}
	total := 0
	for _, t := range tools {
		total += t.EstimatedTokens
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile":                h.info.Profile,
		"tools":                  tools,
		"total_estimated_tokens": total,
	})
}

func (h *handler) tool(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, BasePath+"/tools/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusNotFound, "not_found", "no such route; tools are at "+BasePath+"/tools/{name}")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.describe(w, r, name)
	case http.MethodPost:
		h.call(w, r, name)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET to describe a tool or POST to call it")
	}
}

func (h *handler) describe(w http.ResponseWriter, r *http.Request, name string) {
	tools, err := h.inv.Tools(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not list tools")
		return
	}
	for _, t := range tools {
		if t.Name == name {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeError(w, http.StatusNotFound, "unknown_tool", unknownToolMessage(name, h.info.Profile))
}

func (h *handler) call(w http.ResponseWriter, r *http.Request, name string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large",
				fmt.Sprintf("arguments are limited to %d bytes", MaxBodyBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}

	// An empty body is a call with no arguments, which several tools take.
	args := json.RawMessage(strings.TrimSpace(string(body)))
	if len(args) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(args, &obj); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"the body must be a JSON object of the tool's arguments, for example {\"name\":\"ParseConfig\"}")
			return
		}
	}

	res, err := h.inv.Call(r.Context(), name, args)
	if err != nil {
		if errors.Is(err, mcp.ErrUnknownTool) {
			writeError(w, http.StatusNotFound, "unknown_tool", unknownToolMessage(name, h.info.Profile))
			return
		}
		// Anything else is the transport between this handler and the tool
		// failing. The detail is logged by the server; a client gets a stable
		// message rather than an internal error string.
		writeError(w, http.StatusInternalServerError, "internal", "the tool could not be called")
		return
	}

	// A caller that asks for text gets the answer alone, ready to place in a
	// model's context without unwrapping.
	if wantsText(r) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if res.IsError {
			w.Header().Set("X-RepoCortex-Tool-Error", "true")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, res.Text)
		return
	}
	writeJSON(w, http.StatusOK, CallResponse{
		Tool:    name,
		Text:    res.Text,
		Result:  res.Structured,
		IsError: res.IsError,
	})
}

func (h *handler) openAPI(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	tools, err := h.inv.Tools(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not list tools")
		return
	}
	writeJSON(w, http.StatusOK, OpenAPI(tools, h.info))
}

func unknownToolMessage(name, profile string) string {
	return fmt.Sprintf("no tool named %q on the %q profile; GET %s/tools lists the ones available",
		name, profile, BasePath)
}

func wantsText(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/plain") && !strings.Contains(accept, "application/json")
}

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method || (method == http.MethodGet && r.Method == http.MethodHead) {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this route accepts "+method)
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The index describes private source. Nothing here may be cached by an
	// intermediary or sniffed into another content type.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}
