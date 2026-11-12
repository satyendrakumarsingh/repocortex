package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- one surface
//
// EVERY PROTOCOL REACHES THE SAME TOOLS THROUGH THE SAME CODE.
//
// An agent may speak MCP over stdio, MCP over HTTP, plain HTTP with JSON
// function calls, or nothing but a shell. A tool that exists for one of those
// and not the others is a tool most agents cannot use — and a tool implemented
// once per protocol is three implementations that drift: one bounds its
// output, one forgets the coverage caveat, one validates differently.
//
// So the REST endpoint and `repocortex tool` do not call handlers. They open an
// in-process MCP client against this server and call tools/list and
// tools/call exactly as a remote agent would. Whatever a handler does — schema
// validation, bounding, untrusted-content framing, the audit
// record — the other front ends get by construction, and a parity test only
// has to compare text.
//
// The one difference is state. A protocol session is a conversation, so the
// server remembers what it already sent and elides repeats (dedup.go). A REST
// call and a CLI invocation are single questions from callers that share no
// memory; eliding a repeat there would point at an answer the caller never
// saw. Front-end sessions are therefore registered as stateless.

// ToolInfo describes one exposed tool to a non-MCP front end.
type ToolInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	// InputSchema is the JSON Schema the SDK derived from the argument type.
	InputSchema json.RawMessage `json:"input_schema"`
	// SchemaBytes is the size of the name, description and input schema: the
	// three things a client places in a model's context for every tool it
	// exposes, on every request. EstimatedTokens is that divided by four, the
	// same estimate get_task_context labels its own budget with.
	SchemaBytes     int `json:"schema_bytes"`
	EstimatedTokens int `json:"estimated_tokens"`
	// WireBytes is the whole tool definition as tools/list sends it, which
	// also carries the output schema. A client that forwards the output
	// schema to its model pays this instead of SchemaBytes; most do not.
	WireBytes int `json:"wire_bytes"`
}

// CallResult is a tool's answer as a non-MCP front end returns it.
type CallResult struct {
	// Text is the rendered answer an agent reads.
	Text string `json:"text"`
	// Structured is the same answer as data (the Response envelope).
	Structured json.RawMessage `json:"structured,omitempty"`
	// IsError is the tool's own isError flag; see the isError policy in
	// server.go for the two things it is reserved for.
	IsError bool `json:"is_error"`
}

// ErrUnknownTool reports a name that is not a tool this profile exposes.
var ErrUnknownTool = errors.New("unknown tool")

// Invoker calls this server's tools from inside the process.
type Invoker struct {
	client *sdk.ClientSession
	server *sdk.ServerSession
	srv    *Server
}

// NewInvoker connects an in-process client. frontEnd names the caller for the
// audit log ("rest", "cli"). Close it when done.
func (s *Server) NewInvoker(ctx context.Context, frontEnd string) (*Invoker, error) {
	clientT, serverT := sdk.NewInMemoryTransports()

	ss, err := s.sdk.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect in-process server: %w", err)
	}
	// Registered before the client can send anything, so the first call is
	// already stateless and already attributed.
	s.frontEnds.Store(ss, frontEnd)

	client := sdk.NewClient(&sdk.Implementation{Name: "repocortex-" + frontEnd, Version: Version}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		s.frontEnds.Delete(ss)
		_ = ss.Close()
		return nil, fmt.Errorf("mcp: connect in-process client: %w", err)
	}
	return &Invoker{client: cs, server: ss, srv: s}, nil
}

// Close ends the in-process session.
func (i *Invoker) Close() error {
	if i == nil {
		return nil
	}
	i.srv.frontEnds.Delete(i.server)
	return i.client.Close()
}

// Tools lists the tools the active profile exposes, sorted by name.
func (i *Invoker) Tools(ctx context.Context) ([]ToolInfo, error) {
	var out []ToolInfo
	for t, err := range i.client.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: encode schema for %s: %w", t.Name, err)
		}
		wire, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("mcp: encode tool %s: %w", t.Name, err)
		}
		cost := len(t.Name) + len(t.Description) + len(schema)
		out = append(out, ToolInfo{
			Name:            t.Name,
			Title:           t.Title,
			Description:     t.Description,
			InputSchema:     schema,
			SchemaBytes:     cost,
			EstimatedTokens: (cost + 3) / 4,
			WireBytes:       len(wire),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// Call runs one tool. args is a JSON object, or empty for a tool that takes
// none. An unknown name returns ErrUnknownTool.
func (i *Invoker) Call(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	canonical := name
	if !i.srv.hasTool(canonical) {
		return CallResult{}, fmt.Errorf("%w %q", ErrUnknownTool, name)
	}

	var arguments any
	if len(args) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(args, &obj); err != nil {
			return CallResult{}, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
		arguments = obj
	} else {
		arguments = map[string]any{}
	}

	res, err := i.client.CallTool(ctx, &sdk.CallToolParams{Name: canonical, Arguments: arguments})
	if err != nil {
		return CallResult{}, fmt.Errorf("mcp: call %s: %w", canonical, err)
	}

	out := CallResult{IsError: res.IsError}
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += t.Text
		}
	}
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			out.Structured = raw
		}
	}
	return out, nil
}

// hasTool reports whether the active profile registered name.
func (s *Server) hasTool(name string) bool {
	for _, t := range s.tools {
		if t == name {
			return true
		}
	}
	return false
}
