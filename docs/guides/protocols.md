# Protocols: reach the same tools from any agent

RepoCortex serves one set of tools over four surfaces. Pick the one your agent
can speak; the tools, their arguments and their answers are the same on each.

| Your agent | Use | Start it with |
|---|---|---|
| Speaks MCP and runs on your machine (Claude Code, Cursor, Codex, most others) | MCP over stdio | `repocortex install --rules` |
| Accepts only a remote MCP server (ChatGPT, Android Studio) | MCP over Streamable HTTP | `repocortex serve --http 127.0.0.1:7788` |
| Calls HTTP functions, or you are writing a script or bot | REST with JSON | `repocortex serve --http 127.0.0.1:7788` |
| Has a shell and nothing else (Aider, a CI step) | The command line | `repocortex tool call <tool> key=value` |

## Contents

- [One surface, one implementation](#one-surface-one-implementation)
- [MCP](#mcp)
- [REST](#rest)
- [OpenAPI](#openapi)
- [Command line](#command-line)
- [Profiles and what a tool costs](#profiles-and-what-a-tool-costs)
- [Audit log](#audit-log)
- [Security](#security)

## One surface, one implementation

The REST endpoint and `repocortex tool` do not have their own handlers. Each opens
an in-process MCP client against the server and calls `tools/list` and
`tools/call` exactly as a remote agent would. So a tool's argument validation,
its output bounds, the framing that marks repository text as untrusted and the
audit record are the same on every surface, because there is only one of each.

Two things differ, on purpose:

- **MCP sessions remember; REST and shell calls do not.** In an MCP session the
  server elides source it already sent in that conversation. A REST call or a
  shell invocation is a single question, so it always gets the full answer.
- **`repocortex tool` defaults to the `full` profile.** A profile keeps tool schemas
  out of a model's context. A shell pays nothing for a tool it does not run.

## MCP

Over stdio, register the server with your agent:

```bash
repocortex install --rules
```

Or add it by hand:

```json
{
  "mcpServers": {
    "repocortex": { "command": "repocortex", "args": ["--project", "/absolute/path/to/repo", "serve"] }
  }
}
```

Over HTTP, for clients that take only a URL:

```bash
repocortex serve --http 127.0.0.1:7788
```

The endpoint is `/mcp`. Send the token as `Authorization: Bearer <token>`, or as
the last path segment (`/mcp/<token>`) when the client cannot set headers. The
token comes from `REPOCORTEX_MCP_TOKEN`, or is generated for the run and written to
`.repocortex/mcp-token`. Details: [coding agents guide](coding-agents.md#remote-mcp-over-http).

The server also sends:

- **Instructions**, once per session: where to start, what the confidence tiers
  license, and that an empty result is not proof. Under 900 bytes, and they name
  only tools the active profile exposes.
- **Three prompts** a user can start by name: `plan_refactor`, `analyze_change`
  and `get_oriented`. Each is the sequence of tool calls an experienced user
  would ask for.

## REST

The same listener serves the tools as plain HTTP at `/api/v1`.

| Method and path | Returns |
|---|---|
| `GET /api/v1` | What this server is, and links to the rest |
| `GET /api/v1/tools` | Every tool the active profile exposes, with its input schema and cost |
| `GET /api/v1/tools/{name}` | One tool |
| `POST /api/v1/tools/{name}` | Calls it. The body is the JSON arguments object |
| `GET /api/v1/openapi.json` | The same surface as an OpenAPI 3.1 document |
| `GET /healthz` | `{"status":"ok"}`. No token needed; reveals nothing else |

Call a tool:

```bash
curl -s -H "Authorization: Bearer $REPOCORTEX_MCP_TOKEN" \
     -d '{"name":"ParseConfig","min_confidence":"high"}' \
     http://127.0.0.1:7788/api/v1/tools/find_callers
```

The response:

```json
{
  "tool": "find_callers",
  "text": "=== SYSTEM METADATA ===\n2 caller(s) of ParseConfig ...",
  "result": { "summary": "2 caller(s) of ParseConfig ...", "items": [ ... ] },
  "is_error": false
}
```

`text` is what an MCP client receives. `result` is the same answer as data. Send
`Accept: text/plain` to get the text alone, ready to place in a model's context.

### What the status code means

| Status | Meaning |
|---|---|
| 200 | The tool answered. This includes "nothing found" and a tool's own refusal; check `is_error` |
| 400 | The body is not a JSON object |
| 401 | The token is missing or wrong |
| 403 | The `Host` or `Origin` is not allowed |
| 404 | No tool has that name on the active profile |
| 405 | Wrong method for the route |
| 413 | The arguments are over 1 MB |

A tool's own failure is reported in the body, not as a 4xx or 5xx, for the same
reason MCP reports it inside a successful response: a client that retries on a
server error must not retry a question whose answer is "that limit is negative".

## OpenAPI

`GET /api/v1/openapi.json` is generated from the registered tools on every
request, so it cannot drift from them. Each tool is one `POST` operation whose
`operationId` is the tool name and whose request schema is the tool's input
schema, unchanged. A client that turns operations into functions ends up with
the names an MCP client sees.

```bash
curl -s -H "Authorization: Bearer $REPOCORTEX_MCP_TOKEN" \
     http://127.0.0.1:7788/api/v1/openapi.json > repocortex-openapi.json
```

## Command line

```bash
repocortex tool list                       # every tool, its profile and its cost
repocortex tool describe trace_call_path   # description and arguments
repocortex tool call find_callers name=ParseConfig min_confidence=high
```

Arguments are `key=value` pairs, one JSON object, or both (pairs win):

```bash
repocortex tool call get_task_context --args '{"task":"add retry to the webhook sender"}'
git diff | repocortex tool call minimize_diff diff=@-
repocortex tool call list_dead_code path=internal/billing/ limit=10
```

| Form | Sent as |
|---|---|
| `key=value` | A string, unless the value is `true`, `false`, `null` or a number |
| `key:=<json>` | The JSON value, for arrays and objects |
| `key=@path` | The file's contents as a string; `@-` reads standard input |

`--json` prints `{tool, text, result, is_error}`. The exit status is 1 when the
tool reports a failure and 0 otherwise, including when the answer is "nothing
found".

For a person at a terminal, the task commands (`repocortex callers`, `repocortex
dead-code`, `repocortex hotspots`) print tables instead. `repocortex tool` is for agents
and for seeing exactly what an agent was shown.

## Profiles and what a tool costs

Every exposed tool's name, description and input schema are placed in the
model's context on every request. `repocortex tool list` prints that cost, measured
on this build:

| Profile | Tools | Schema per request |
|---|---|---|
| `minimal` (default for `serve`) | 7 | about 1,700 tokens |
| `standard` | 18 | about 4,400 tokens |
| `full` | 25 | about 6,400 tokens |

Tokens are estimated as bytes divided by four. Set the profile in `.repocortex.yml`:

```yaml
mcp: { profile: standard }
```

The REST endpoint follows the same profile, because an OpenAPI document handed
to a model is a list of functions with the same cost.

A client that also forwards each tool's output schema to its model pays a
little more: about 7,700 tokens on `full`. Every tool advertises one compact
output schema; a full copy per tool used to make that figure about 21,500.
`repocortex tool list --json` reports both as `schema_bytes` and `wire_bytes`.

## Audit log

Record every tool call, on any surface, as one JSON line:

```bash
repocortex serve --audit-log .repocortex/audit.jsonl
export REPOCORTEX_AUDIT_LOG=~/.repocortex-audit.jsonl    # or for every invocation
```

```json
{"time":"2026-11-12T09:41:07Z","transport":"rest","tool":"find_callers","arguments":{"name":"ParseConfig"},"duration_ms":4,"outcome":"ok","result_bytes":1911}
```

| Field | Meaning |
|---|---|
| `transport` | `mcp`, `rest` or `cli` |
| `tool` | The tool that was called |
| `arguments` | As sent, cut at 2 KB and marked `arguments_truncated` |
| `outcome` | `ok`, `tool_error` or `protocol_error` |
| `result_bytes` | The size of the answer |

The log records the call and never the answer. The answer is repository content,
and copying it into a second file would double the places private source can
leak from. The file is created readable only by you.

The log is a flag and an environment variable, not a key in `.repocortex.yml`. That
file is part of the repository, and a repository must not be able to switch off
the record of what an agent read from it.

## Security

Both HTTP surfaces sit behind one guard:

- **A token on every request**, compared in constant time. REST accepts it only
  as a bearer header: a token in a path ends up in access logs.
- **A `Host` allowlist**, which blocks DNS rebinding. Loopback names are always
  allowed; `--allowed-host` adds a tunnel or proxy hostname.
- **An `Origin` check**, so a web page in your browser cannot drive the server.
- **Loopback only** unless you pass `--allow-remote`.

Responses carry `Cache-Control: no-store`. An internal failure returns a fixed
message, never the underlying error text. Threat model: [security](../security.md).
