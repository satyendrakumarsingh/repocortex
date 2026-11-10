# RepoCortex documentation

## Start here

| Document | The question it answers |
|---|---|
| [GUIDE.md](GUIDE.md) | I have twenty minutes — which of the four paths (wire into an agent, blast radius before a refactor, architecture in CI, check the accuracy) do I take, and what do I type? |
| [USER_GUIDE.md](USER_GUIDE.md) | What can the whole platform do end to end — events, blindspots, refactor plans, pruning, the MCP tool list, the portal and the CI gate — in one read? |
| [user-manual.md](user-manual.md) | What does each part of the tool do, organised by what I am trying to do; and the complete configuration reference for both config files |
| [CLI.md](CLI.md) | Every command, every flag, what each refuses to claim, and real output |
| [agent-integration.md](agent-integration.md) | How does an AI coding agent get the tools AND the guidance to reach for them? |
| [FAQ.md](FAQ.md) | The questions people actually ask, including the ones about what it cannot do |
| [troubleshooting.md](troubleshooting.md) | Something printed an error — what caused it and what do I do? Keyed on the literal messages |

## Understanding what it claims

| Document | The question it answers |
|---|---|
| [accuracy.md](accuracy.md) | How right are the edges, per language and per confidence tier, measured against a compiler — and how were the numbers produced? |
| [architecture.md](architecture.md) | What are the layers, why is each boundary where it is, and what is actually built? |
| [NON-GOALS.md](NON-GOALS.md) | What does it deliberately not do, and where in the code is each refusal enforced? |
| [BENCHMARKING.md](BENCHMARKING.md) | How is retrieval quality scored, and what would make the number lie? |
| [corpus.md](corpus.md) | Which real repositories is it held to in CI, and why those? |

## Working on RepoCortex

| Document | The question it answers |
|---|---|
| [development.md](development.md) | How do I set up, and which of the five test tiers does my change need to satisfy? |
| [git-workflow.md](git-workflow.md) | What does a commit message here have to contain, and why can a shipped migration never be edited? |
| [performance.md](performance.md) | What does each operation actually cost, and what measurement chose each default? |
| [security.md](security.md) | What is the threat model, which controls enforce it, and — explicitly — what is not protected? |

## Maintainer records

Audit records, design specs, implementation plans, competitive teardowns and
the UI preview mock are kept outside the public tree, under `docs/internal/`,
which is gitignored. They are point-in-time working records for maintainers —
agent-run audits and plans that name machines and repositories that are not
public — not documentation for someone using the tool. Nothing a user needs
lives there.

Release history is in [../CHANGELOG.md](../CHANGELOG.md). Contribution rules are
in [../CONTRIBUTING.md](../CONTRIBUTING.md). The engineering standards these
documents are audited against are in [../CLAUDE.md](../CLAUDE.md).

## What "verified" means here, and what it does not

Claims in these documents are checked by running something — building the binary,
indexing a copy of this repository in a temporary directory, executing the named
test or benchmark — not by reading the code and inferring. Where a number could
not be reproduced, the document says so and withholds the number rather than
repeating it; [performance.md](performance.md) has two such cases.

That is not the same as a guarantee of currency. These files are edited by hand
and the code is not, so any of them can drift between one audit and the next. The
specific way it drifts is worth knowing: a statement that something is *missing*
survives long after the gap closes, because nothing prompts an edit when a feature
lands. Both `architecture.md` and `development.md` carried a list of empty
placeholder packages that had shipped months earlier.

Where a document and the code disagree, the code is the answer. Where two
documents disagree, the one that names the command it ran is the better bet.
