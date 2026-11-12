package restapi

import (
	"encoding/json"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/mcp"
)

// OpenAPI builds an OpenAPI 3.1 document for the given tools.
//
// It is GENERATED from the tools the server actually registered, on every
// request, and never written down. A hand-maintained specification is a second
// description of the tool surface, and the first time a parameter is renamed
// it becomes a wrong one that a client generator then turns into wrong code.
//
// 3.1 rather than 3.0 because 3.1's schema object IS JSON Schema, so each
// tool's input schema is embedded unchanged; 3.0 would need it rewritten into
// a dialect with different nullability and no `$schema`.
//
// Each tool becomes one POST operation whose operationId is the tool name, so
// a function-calling client that maps operations to functions ends up with the
// same names an MCP client sees.
func OpenAPI(tools []mcp.ToolInfo, info Info) map[string]any {
	paths := map[string]any{}
	for _, t := range tools {
		var schema any = map[string]any{"type": "object"}
		if len(t.InputSchema) > 0 {
			var parsed any
			if err := json.Unmarshal(t.InputSchema, &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		paths[BasePath+"/tools/"+t.Name] = map[string]any{
			"post": map[string]any{
				"operationId": t.Name,
				"summary":     firstSentence(t.Description),
				"description": t.Description,
				"tags":        []string{verbOf(t.Name)},
				"requestBody": map[string]any{
					"required": false,
					"content": map[string]any{
						"application/json": map[string]any{"schema": schema},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "The tool answered. Check is_error for the tool's own failure flag.",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"$ref": "#/components/schemas/CallResponse"},
							},
							"text/plain": map[string]any{
								"schema": map[string]any{"type": "string"},
							},
						},
					},
					"400": errorRef("The body is not a JSON object."),
					"401": errorRef("The bearer token is missing or wrong."),
					"404": errorRef("No tool has this name on the active profile."),
					"413": errorRef("The arguments are larger than the limit."),
				},
			},
		}
	}

	title := "RepoCortex"
	if info.Project != "" {
		title += ": " + info.Project
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   title,
			"version": orDefault(info.Version, "dev"),
			"description": "The code-graph tools RepoCortex serves to AI coding agents, as plain HTTP. " +
				"Each operation is one tool; the same tools are served over MCP at /mcp and from the " +
				"command line as `repocortex tool call`. Answers mark every relationship exact, high, medium " +
				"or low, and an empty result never means nothing exists.",
			"license": map[string]any{"name": "Apache-2.0", "identifier": "Apache-2.0"},
		},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearer": map[string]any{
					"type":        "http",
					"scheme":      "bearer",
					"description": "The token from REPOCORTEX_MCP_TOKEN, or the one written to .repocortex/mcp-token.",
				},
			},
			"schemas": map[string]any{
				"CallResponse": map[string]any{
					"type":     "object",
					"required": []string{"tool", "text", "is_error"},
					"properties": map[string]any{
						"tool":     map[string]any{"type": "string", "description": "The tool that answered, by its current name."},
						"text":     map[string]any{"type": "string", "description": "The rendered answer, as an MCP client receives it."},
						"result":   map[string]any{"type": "object", "description": "The same answer as structured data."},
						"is_error": map[string]any{"type": "boolean", "description": "The tool's own failure flag: a refusal, a malformed call or a malfunction."},
					},
				},
				"Error": map[string]any{
					"type":     "object",
					"required": []string{"error"},
					"properties": map[string]any{
						"error": map[string]any{
							"type":     "object",
							"required": []string{"code", "message"},
							"properties": map[string]any{
								"code":    map[string]any{"type": "string"},
								"message": map[string]any{"type": "string"},
							},
						},
					},
				},
			},
		},
	}
}

func errorRef(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/Error"},
			},
		},
	}
}

// firstSentence is the operation summary: OpenAPI tooling shows it in lists,
// where a full tool description (a paragraph written for a model) is unusable.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	if i := strings.Index(s, ": "); i > 0 && i < 120 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// verbOf groups operations by the tool's leading verb, which is the naming
// convention's own taxonomy (see mcp.ToolVerbs).
func verbOf(name string) string {
	verb, _, _ := strings.Cut(name, "_")
	return verb
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
