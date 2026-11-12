package mcp

// The CLI answers "what HTTP endpoints does this service expose", "which code
// reads and writes each table" and "where is this config key read" today, and
// before this file an agent over MCP could not ask any of them. Those are
// three of the things an agent most often rebuilds by grepping, which is
// exactly the cost this project exists to remove.
//
// Each handler is a thin query over the same Registry report the matching CLI
// command reads — EndpointReport for `repocortex api`, TableUsageReport for
// `repocortex db`, ConfigReport for `repocortex config` — plus an optional
// in-memory filter. No query logic is duplicated: the store call is the one
// internal/cli/apidb.go and internal/cli/configdeps.go already make.
//
// None of these need the call graph, so they register in registerTools()
// rather than registerCapabilities(): an index whose references were never
// resolved should still answer "what endpoints exist", exactly as
// get_project_overview and search_code already do.

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- endpoints

type apiEndpointArgs struct {
	Method string `json:"method,omitempty" jsonschema:"Filter to one HTTP method, e.g. GET. Case-insensitive."`
}

func (s *Server) findAPIEndpoints(ctx context.Context, req *sdk.CallToolRequest, args apiEndpointArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	endpoints, err := s.deps.Registry.EndpointReport(ctx, runID)
	if err != nil {
		return failFrom(err, "endpoint report: %v", err)
	}

	// Endpoints declared in test files are real routes in real code but are
	// NOT this service's API surface — the same split newAPICmd applies in
	// internal/cli/apidb.go, via the same capability.IsTestPath.
	var live []registry.Endpoint
	testFixtures := 0
	for _, e := range endpoints {
		if capability.IsTestPath(e.FilePath) {
			testFixtures++
			continue
		}
		live = append(live, e)
	}

	if method := strings.ToUpper(strings.TrimSpace(args.Method)); method != "" {
		var filtered []registry.Endpoint
		for _, e := range live {
			if strings.HasPrefix(strings.ToUpper(e.Signature), method+" ") {
				filtered = append(filtered, e)
			}
		}
		live = filtered
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%d endpoint(s). Discovery covers %s.",
			len(live), strings.Join(frameworkNames(), ", ")),
	}

	unhandled := 0
	for _, e := range live {
		why := "handler: " + e.Handler
		if e.HandlerPath != "" && e.HandlerPath != e.FilePath {
			why += " (" + e.HandlerPath + ")"
		}
		if e.Handler == "" {
			why = "no handler recorded: the route is real, but the framework's syntax did " +
				"not name the function"
			unhandled++
		}
		resp.Items = append(resp.Items, Item{
			Kind: "endpoint",
			Name: e.Signature,
			Path: e.FilePath,
			Line: e.Line,
			Why:  why,
		})
	}

	if len(live) == 0 {
		resp.Notes = append(resp.Notes, "No HTTP endpoints found. Route discovery covers "+
			strings.Join(frameworkNames(), ", ")+". A service defined through OpenAPI, "+
			"GraphQL, gRPC or gateway configuration will report nothing here — that is a "+
			"coverage limit, not a finding about the code.")
	}
	if unhandled > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d endpoint(s) have no handler recorded.", unhandled))
	}
	if testFixtures > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d further route(s) are declared in test files and excluded — they are fixtures "+
				"standing up a server for a test, not this service's API surface.", testFixtures))
	}
	return s.reply(ctx, req, resp)
}

// frameworkNames renders api.SupportedFrameworks() as strings, mirroring
// internal/cli.frameworkList without importing the cli package.
func frameworkNames() []string {
	fws := api.SupportedFrameworks()
	names := make([]string, 0, len(fws))
	for _, f := range fws {
		names = append(names, string(f))
	}
	return names
}

// ---------------------------------------------------------------- db access

type dbAccessArgs struct {
	Table string `json:"table,omitempty" jsonschema:"Filter to one table name. Case-insensitive."`
}

func (s *Server) findDBAccess(ctx context.Context, req *sdk.CallToolRequest, args dbAccessArgs) (*sdk.CallToolResult, *Response, error) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	usage, err := s.deps.Registry.TableUsageReport(ctx, runID)
	if err != nil {
		return failFrom(err, "table usage report: %v", err)
	}

	if table := strings.ToLower(strings.TrimSpace(args.Table)); table != "" {
		var filtered []registry.TableUsage
		for _, u := range usage {
			if u.Table == table {
				filtered = append(filtered, u)
			}
		}
		usage = filtered
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%d table(s).", len(usage)),
	}

	var external, uncertain int
	for _, u := range usage {
		declared := "declared in this repository"
		if !u.Declared {
			declared = "used but not declared in this repository — most likely owned by " +
				"another service"
			external++
		}
		uncertain += u.Uncertain
		resp.Items = append(resp.Items, Item{
			Kind: "table",
			Name: u.Table,
			Why:  fmt.Sprintf("%d read(s), %d write(s); %s", u.Readers, u.Writers, declared),
		})
	}

	if len(usage) == 0 {
		if args.Table != "" {
			resp.Notes = append(resp.Notes, fmt.Sprintf(
				"No access recorded for table %q. SQL is recognised in .sql files and in "+
					"string literals in source; queries built by an ORM's fluent API, or "+
					"assembled far from where they run, are not visible to a static scan.",
				args.Table))
		} else {
			resp.Notes = append(resp.Notes,
				"No database access found. SQL is recognised in .sql files and in string "+
					"literals in source; queries built by an ORM's fluent API, or assembled "+
					"far from where they run, are not visible to a static scan — that is a "+
					"coverage limit, not a finding about the code.")
		}
	}
	if external > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d table(s) are not declared by any migration here — most likely owned by "+
				"another service.", external))
	}
	if uncertain > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d statement(s) were assembled at run time; the table is real, but whether that "+
				"exact statement executes as written is not something a static read can "+
				"establish.", uncertain))
	}
	return s.reply(ctx, req, resp)
}

// ---------------------------------------------------------------- config key

type configKeyArgs struct {
	Key string `json:"key" jsonschema:"The configuration key to look up, e.g. DATABASE_URL."`
}

func (s *Server) getConfigKey(ctx context.Context, req *sdk.CallToolRequest, args configKeyArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Key) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty key, then call get_config_key again.",
			"key must not be empty")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	keys, err := s.deps.Registry.ConfigReport(ctx, runID)
	if err != nil {
		return failFrom(err, "config report: %v", err)
	}

	var found *registry.ConfigKeyUsage
	for i := range keys {
		if strings.EqualFold(keys[i].Name, args.Key) {
			found = &keys[i]
			break
		}
	}

	resp := &Response{IndexRevision: runID, IndexStatus: status}
	if found == nil {
		resp.Summary = fmt.Sprintf("No configuration key named %q.", args.Key)
		resp.Notes = append(resp.Notes,
			"No such key is declared or read anywhere in the index. Declarations are read "+
				"from .env, YAML, JSON, TOML and .properties files; usage from os.Getenv, "+
				"@Value, process.env and similar lookups. Kubernetes, Helm, Terraform and CI "+
				"definitions are not read, so a key set only there will not appear here — "+
				"that is a coverage limit, not proof the key is unused.")
		return s.reply(ctx, req, resp)
	}

	where := "not declared anywhere in this repository"
	if found.Declared {
		where = fmt.Sprintf("declared at %s:%d", found.FilePath, found.Line)
	}
	resp.Summary = fmt.Sprintf("%s: %d reader(s), %s.", found.Name, found.Readers, where)
	resp.Items = append(resp.Items, Item{
		Kind: "config_key",
		Name: found.Name,
		Path: found.FilePath,
		Line: found.Line,
		Why:  fmt.Sprintf("%d reader(s); %s", found.Readers, where),
	})

	if !found.Declared {
		resp.Notes = append(resp.Notes,
			"READ BUT NOT DECLARED: a service that needs this key and finds nothing declaring "+
				"it fails at start-up.")
	} else if found.Readers == 0 {
		resp.Notes = append(resp.Notes,
			"Declared and never read by any code this scan recognises — either dead "+
				"configuration or a lookup this scan cannot see.")
	}
	if found.Secret {
		resp.Notes = append(resp.Notes,
			"This key's NAME indicates a credential. No configuration VALUE was read to "+
				"produce this report — the response has no field that could hold one.")
	}
	return s.reply(ctx, req, resp)
}
