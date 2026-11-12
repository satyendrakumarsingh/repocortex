# Agent integration

RepoCortex is a server. Wiring it up has two halves, and only one of them is the
MCP registration.

The other half is guidance. An agent with the tools connected but nothing
telling it *when* to reach for them will keep grepping, because a tool
description is only read once the agent has already decided to look at the tool
list. Nothing in a tool schema can say "before you edit a symbol, ask for its
blast radius first". That sentence has to live in project memory.

Both halves come out of one command:

```
repocortex agent
```

---

## 1. Register the MCP server

### Claude Code, one line

```
claude mcp add repocortex -- repocortex --project /path/to/repo serve
```

`--project` is explicit on purpose. An MCP client starts the server from
whatever directory it happens to be in, and a server that quietly indexed the
wrong repository would be worse than one that failed to start.

`repocortex agent` prints this line with your project's real path substituted, so
you can paste it rather than editing it.

### Claude Code, as a plugin

This repository ships a plugin manifest at `.claude-plugin/plugin.json`. It
registers the same MCP server *and* installs the guidance as a skill, so one
step delivers both halves:

```
claude --plugin-dir /path/to/repocortex
```

`--plugin-dir` loads a plugin straight from a directory, without installing it.
To install it properly, or to hand it to a team, add the repository as a
marketplace — `.claude-plugin/marketplace.json` is there for that:

```
/plugin marketplace add /path/to/repocortex
/plugin install repocortex@repocortex
```

The manifest pins the project root with `${CLAUDE_PROJECT_DIR}`, which Claude
Code expands to the project you have open:

```json
{
  "mcpServers": {
    "repocortex": {
      "command": "repocortex",
      "args": ["--project", "${CLAUDE_PROJECT_DIR}", "serve"]
    }
  }
}
```

The plugin does **not** ship the `repocortex` binary. Build or install it first
and make sure it is on `PATH`.

### Any other MCP client

RepoCortex speaks MCP over stdio. Whatever config format your client uses, these
are the three facts it needs:

| | |
| --- | --- |
| transport | stdio |
| command | `repocortex` |
| args | `--project /path/to/repo serve` |

For a client that reads the common `mcpServers` JSON shape — including a
project-scope `.mcp.json` in the repository root:

```json
{
  "mcpServers": {
    "repocortex": {
      "command": "repocortex",
      "args": ["--project", "/path/to/repo", "serve"]
    }
  }
}
```

`repocortex agent --json` emits the same spec as machine-readable JSON, if you
would rather generate the config than write it.

---

## 2. Give the agent the guidance

```
repocortex agent --guidance >> CLAUDE.md
```

That appends a short block, delimited by `<!-- repocortex:guidance -->` markers,
that tells the agent when to use each tool and how to read what comes back. It
is deliberately short: an agent skims project memory, and a page of prose is
ignored in exactly the same way as no prose at all.

`--guidance` prints the block and nothing else — no banner, no trailing advice —
because the output is meant to land in a file you keep.

If you would rather have it as its own file:

```
repocortex agent --write docs/repocortex-guidance.md
```

`--write` refuses to replace a file that already exists. Passing `--force`
allows it and says on stderr what it discarded. **No mode of this command
modifies your global configuration** — where the integration goes is your
decision, so the default is to print and let you place it.

### What the guidance actually says

Every rule is grounded in behaviour the server has, not in aspiration:

| Rule | Why it exists |
| --- | --- |
| Call `get_task_context` first | Its own description opens `CALL THIS FIRST`. One call replaces a dozen exploratory ones. |
| Ask for a blast radius before editing | `analyze_impact` on the `standard` profile, `find_callers` otherwise. |
| `find_tests` before changing behaviour | It reports *which signal* found each test — a proven call edge outranks a filename match. |
| Treat anything below `exact` as a lead | `exact` means a compiler proved it. `high`/`medium`/`low` were inferred and may be wrong. |
| An empty result means unresolved, not absent | Relationship answers are a lower bound. An agent told "no callers" will happily delete a live method. |
| `COVERAGE TOO LOW` outranks the list under it | The server emits that literal string when under half of the repository's references are bound. |
| "No index has been built" is not a broken tool | It is a recoverable state with a one-command fix, and the server deliberately returns it as a success. |
| The index is a snapshot, not your working tree | Read files you just edited directly, or run `repocortex watch`. |

---

## 3. Check it landed

```
repocortex agent --check
```

It reports six things, reading only — it never creates what it finds missing:

- **index** — present, committed, non-empty
- **resolution** — how much of the call graph is actually bound
- **mcp tools** — which profile is active and what it exposes
- **mcp registration** — whether this repository's `.mcp.json` names `repocortex`
- **guidance** — whether `CLAUDE.md` or `AGENTS.md` carries the marker
- **plugin manifest** — whether `.claude-plugin/plugin.json` is present

Nothing missing is reported as a failure, and the command exits zero unless the
index is genuinely unreadable. An un-integrated project is the normal starting
state, not a malfunction — the same judgement the MCP server makes about
`isError`. Add `--json` to consume it from a script.

Note that `--check` sees the **project scope** only. A registration you added
with `claude mcp add` at user or local scope lives outside the repository, so it
is invisible here; the check says "not visible from this repository" rather than
claiming you have not registered it.

---

## What to expect once it is wired up

### The tool surface is small on purpose

```
$ repocortex serve --list-tools
MCP profile "minimal" exposes 7 tool(s):
  get_project_overview
  search_code
  find_symbol
  get_symbol
  find_references
  find_callers
  get_task_context
```

Every exposed schema is injected into the agent's context on *every* request, so
a wide tool surface spends the very context RepoCortex exists to save — and a long
tool list measurably degrades tool selection. Seven is the default, and it is a
product decision rather than a limitation.

`analyze_impact` and `find_tests` need the `standard` profile. Set it in
`.repocortex.yml`:

```yaml
mcp:
  profile: standard
```

```
$ repocortex serve --list-tools
MCP profile "standard" exposes 9 tool(s):
  get_project_overview
  search_code
  find_symbol
  get_symbol
  find_references
  find_callers
  get_task_context
  analyze_impact
  find_tests
```

The guidance block adds **no tools**. It is text in project memory, paid for
once, that teaches the agent an ordering over the tools already there.

### There must be an index

The server refuses to start without one, and an empty index answers every
question with nothing:

```
repocortex init
repocortex index
repocortex resolve
```

`resolve` is the step people skip. `index` extracts references but cannot bind
them — that needs the complete symbol table, which does not exist until every
file is parsed. Until `resolve` runs, `find_callers` and `find_references`
return nothing, and *nothing is the answer most easily mistaken for a fact*.
`repocortex agent --check` calls this out as its own row for that reason.

### Answers carry their own caveats

RepoCortex does not hand an agent a tidy list built on evidence it does not have.
A relationship answer on a poorly resolved index arrives with the verdict
*before* the list:

```
COVERAGE TOO LOW — caller lookup for Go is unreliable here: 31% of this index's
16973 references are bound to a definition. Run 'repocortex resolve' to raise it.
Do not present what follows as an answer to the question asked.
```

And a recoverable condition arrives as a **successful** call whose text says what
to do next, never as a tool error:

```
No index has been built for this project yet, so there is nothing to answer from.
Run 'repocortex index' in the project root, then ask again. This is not a tool
failure: RepoCortex is working and the index is simply absent.
```

That distinction is load-bearing. A model that sees a tool fail early concludes
the toolset is broken and stops calling it for the rest of the session, so an
error is reserved for security refusals and genuine malfunction. The guidance
tells the agent to read these as instructions rather than as failures.

While the first index of a project is being built, the same mechanism answers
that the index is *still being built* (wait, and do not start a second one)
rather than "no index has been built". Once a run is committed, an answer taken
from it carries a note when a newer run is being built, because that answer may
not reflect recent edits.

---

## Keeping it current

The index describes the last committed run, not your working tree.

```
repocortex watch      # re-index on change
repocortex status     # revision, counts, resolution coverage
```

For files you edited moments ago, read them directly — the guidance says so, so
the agent will not report a stale definition as current.

---

## The CLI is the same engine

Every MCP tool has a command-line counterpart, which is the fastest way to see
*why* an agent received what it received:

```
repocortex context "Add idempotency to payment callback processing"   # get_task_context, with exclusions
repocortex callers <symbol>                                           # find_callers
repocortex references <symbol>                                        # find_references
repocortex symbol <name>                                              # find_symbol
repocortex search <query>                                             # search_code
repocortex status                                                     # get_project_overview
```

`repocortex context --show-excluded` lists everything considered and left out with
the reason it was cut, which is the part an agent never shows you.
