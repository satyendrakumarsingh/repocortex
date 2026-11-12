# Why Vector RAG Fails for Code: The Case for Deterministic Graph Intelligence

*An engineering perspective on why vector embeddings and semantic search break down on real codebases, and why deterministic AST graphs with calibrated confidence tiers are the only reliable foundation for AI coding agents.*

---

## 1. The Siren Song of Vector Similarity

Every engineer who has plugged an LLM into a codebase starts with the same architecture:
1. Chunk source files into 500-token windows.
2. Run them through an embedding model (`text-embedding-3-small`, Ada, etc.).
3. Store the vectors in a vector database.
4. When a user or agent asks a question, retrieve the top-10 nearest chunks by cosine similarity and dump them into the prompt.

On paper, this sounds elegant. It requires almost zero language-specific parsing, works on any programming language from day one, and handles natural language questions gracefully.

In practice—on real production repositories with 500,000 lines of code, overloaded methods, microservice boundaries, and subtle interface contracts—**it fails continuously, silently, and expensively.**

Why? Because natural language and computer software operate on completely different mathematical foundations:
- **Prose is continuous and fuzzy.** If a user manual mentions *"resetting your credentials"* and another paragraph mentions *"updating your password"*, they are semantically similar. Cosine similarity correctly bridges that vocabulary gap.
- **Code is discrete and causal.** If a function is named `processPayment()`, it does not matter how semantically similar it is to `verifyPayment()`, `logPayment()`, or `mockPayment()`. If the runtime executes `BillingGateway.settle()`, then **only** `settle()` matters. 

Embedding similarity measures **lexical and thematic proximity**. Compilers, linkers, and runtimes operate on **causal reachability**. Conflating the two is the root cause of AI coding agent hallucinations.

---

## 2. The Five Fatal Traps of Code RAG

Through months of testing agents across large polyglot enterprise codebases, we documented five failure modes that vector RAG cannot fix without fundamentally abandoning vector search:

### Trap 1: The "Unrelated Payment Marketing" Paradox (Similarity $\neq$ Causality)

Consider a real scenario from our benchmark fixtures:
A user asks an AI agent:
> *"Add idempotency token verification to our payment callback processing."*

A vector RAG system embeds this prompt and queries the vector index.
- In `internal/billing/CallbackController.java`, the code defines `handleWebhook(Request req)`: concise, dense, few comments.
- In `unrelated/marketing/MarketingReport.java`, a developer wrote a lengthy class comment explaining:
  > *"This report aggregates marketing attribution. Unrelated to payment processing callbacks, which are handled in billing."*

The vector search engine computes the dot product. Because `MarketingReport.java` repeats the words *"payment"*, *"processing"*, and *"callbacks"* multiple times in natural prose, **it scores higher in cosine similarity than the actual controller code!**

The vector database happily feeds `MarketingReport.java` into the agent's context window. The agent dutifully reads it, gets confused, greps for more files, burns tokens, and misses the actual callback handler.

> **Engineering Principle**: Graph connectivity (can symbol A reach symbol B through an actual AST call edge?) must act as an **admission gate**, not merely a ranking heuristic.

---

### Trap 2: The Blast Radius Blindspot

When an engineer or an agent modifies a function signature—say, renaming `UpdateUser(ctx, id)` or adding a mandatory parameter—the most dangerous question is:
> *"Who across the entire repository breaks if I make this change?"*

Vector search cannot answer this question. Period.
- If function `A()` calls `B()`, they may share zero vocabulary. `A()` might be a generic task scheduler `RunBatchJob()`, and `B()` might be `RotateKey()`. No embedding model on earth will score them as close neighbors.
- What an agent actually needs is a **reverse transitive reachability walk** along proven call edges.
- If an agent relies on vector search or grep to find callers, it misses callers whose function names or surrounding comments don't resemble the target. The agent submits a PR, tests pass locally because tests only ran on modified files, and staging breaks with a runtime panic.

---

### Trap 3: Symbol Collision and the "Close()" Illusion

In any production codebase with more than 50 packages, common identifier names collide constantly:
- `Close()` appears on database connections, HTTP response bodies, file handles, channel wrappers, and metrics flushers.
- `Init()` or `Start()` appears in dozens of subsystems.
- `Process()` or `Execute()` exists on command handlers, workers, pipelines, and CLI commands.

To a vector model, all instances of `db.Close()` and `file.Close()` live in the same neighborhood of vector space. 
When an agent asks *"find references to Close"*, vector retrieval retrieves a noisy salad of unrelated clean-up methods.

To resolve a reference correctly, you need:
1. **Lexical scoping** (what package is imported?)
2. **Type attribution** (what is the receiver type of `db`?)
3. **AST arity checking** (how many arguments does the call pass?)
4. **Interface satisfaction** (does this struct implement that interface?)

None of these four operations are vectors. They are discrete relational algebra queries over an Abstract Syntax Tree.

---

### Trap 4: Context Window Pollution and the "Lost in the Middle" Tax

A typical vector RAG pipeline retrieves the top 10–20 chunks (often 500–1,000 tokens each) to maximize recall. That dumps **10,000 to 20,000 tokens of raw file text** into the agent's prompt on every turn.

This introduces two severe penalties:
1. **Financial and Latency Tax**: At current frontier model pricing, 20k tokens per prompt across a 30-turn agent coding session burns \$1.50–\$5.00 per task and adds 5–15 seconds of time-to-first-token per request.
2. **Cognitive Degradation ("Lost in the Middle")**: Extensive academic research demonstrates that LLM reasoning degrades sharply when critical needles are surrounded by large haystacks of irrelevant code. When the context is polluted with 80% irrelevant snippets, the agent produces weaker reasoning and introduces subtle logic bugs.

RepoCortex flips this equation. Instead of returning whole files or raw text chunks:
- It returns **compact symbol signatures and 2-hop subgraphs**.
- A full task context (the target file, exact callers, related database tables, and the specific test file) typically costs **under 800 tokens**.
- The agent gets 95% less noise and 100% higher causal precision.

---

### Trap 5: The Zero-Provenance Lie

When a vector search engine returns a list of results, each result carries a float score:
`[Score: 0.8421, File: payment.go]`

What does `0.8421` mean to an autonomous agent?
- Does it mean the compiler proved that `payment.go` executes this code?
- Does it mean an import statement matches?
- Does it mean someone mentioned "payment" in a TODO comment three years ago?

The agent cannot tell. Because every result is presented with equal epistemic weight, the agent treats heuristic hunches as absolute facts. It deletes code thinking nobody calls it, or writes callers that fail at link time.

---

## 3. The RepoCortex Alternative: Deterministic Graph Intelligence

We built RepoCortex because we were tired of watching agents flounder in vector soup. We wanted a tool built on the same principles as compilers, linkers, and databases: **deterministic truth, provable provenance, and bounded memory.**

```
+-------------------------------------------------------------------------+
|                        THE REPOCORTEX GRAPH PIPELINE                      |
+-------------------------------------------------------------------------+
|                                                                         |
|   Source Code (.go, .ts, .py, .java, .rs, .c, etc.)                     |
|        │                                                                |
|        ▼ (Tree-Sitter / SCIP Indexers)                                  |
|   AST Extraction (Pure Go, 206 grammars, zero CGO)                      |
|        │                                                                |
|        ▼                                                                |
|   Symbol & Reference Registry (SQLite relational tables)                |
|        │                                                                |
|        ▼ (Multi-pass resolution: Arity, Imports, Receivers, SCIP)       |
|   Resolved Knowledge Graph with 4-Tier Provenance:                      |
|        ├── exact  : Compiler type-checker proven (100% precision)       |
|        ├── high   : Unambiguous import + type discriminator (87-99%)   |
|        ├── medium : Multi-candidate heuristic lead (5-37%)              |
|        └── low    : Loose identifier match (Filtered out)               |
|        │                                                                |
|        ▼                                                                |
|   Compressed Sparse Row (CSR) Memory Graph (<250 MB RAM)                |
|        │                                                                |
|        ▼ (Sub-millisecond traversals)                                   |
|   Model Context Protocol (MCP) Server -> AI Agent (Cursor/Claude/etc.)  |
+-------------------------------------------------------------------------+
```

### The Four Confidence Tiers

RepoCortex never lies about how much it knows. Every edge in the graph is tagged with its provenance:

| Confidence | Source | Typical Precision | What the Agent Does |
|---|---|---|---|
| `exact` | Compiler-backed SCIP indexers (`scip-go`, `scip-typescript`, etc.) | **100.0%** | Safe for automated refactoring, renames, and blocking CI gates. |
| `high` | Tree-Sitter AST with explicit package imports and receiver matching | **86.8% – 99.5%** | Reliable primary guidance for code exploration and planning. |
| `medium` | Multi-candidate type inference or package-level heuristics | **5.7% – 37.5%** | Treated as investigatory leads; never acted upon without verification. |
| `low` | Loose identifier matches across unrelated packages | **0.0% – 2.8%** | Filtered by default. Exposed only when explicitly requested. |

When an agent calls `repocortex`, it doesn't get a vague similarity score. It gets:
> *"Symbol `SubmitOrder` reaches `ChargeCard` at Depth 2 (Confidence: `exact`, proven by compiler)."*

If an edge is heuristic (`medium`), RepoCortex attaches an advisory notice:
> *"Warning: Interface dispatch could not be proven at compile time. Verify candidates before modifying."*

---

## 4. Does Vector Search Have Any Role?

Honest engineering requires acknowledging trade-offs. Is vector search completely useless in software engineering?

No. Vector search and BM25 lexical search excel at **unstructured natural language discovery**:
- *"Where in this codebase do we calculate state sales tax?"*
- *"Find the configuration that controls retry backoff for Redis."*

In RepoCortex, we use full-text search (SQLite FTS5 BM25) for exactly that: **finding candidate entry points** from a user's natural language task prompt.

**Crucially, however, search is only the candidate finder. It is never the resolver.**
Once candidates are found, RepoCortex immediately projects them onto the deterministic graph:
1. Candidate nodes are validated against AST symbol definitions.
2. Unconnected noise (like marketing comments or logs) is pruned because it has zero graph degree to the rest of the system.
3. The true execution cluster is traversed along proven edges.
4. Only the connected, verified subgraph is emitted to the agent.

---

## 5. Summary: Why We Chose SQLite Over a Vector DB

| Dimension | Vector RAG | RepoCortex Deterministic Graph |
|---|---|---|
| **Underlying Math** | High-dimensional float cosine distance | Discrete relational graph algebra & AST traversals |
| **Edge Precision** | Uncalibrated probabilities (0.0 to 1.0) | Mathematically verified confidence tiers (`exact`, `high`, etc.) |
| **Transitive Blast Radius** | Impossible (Semantic similarity does not chain) | Sub-millisecond recursive walks with hub pruning |
| **Token Consumption** | 10k–30k tokens per query (raw file chunks) | 500–1,500 tokens per query (subgraph signatures) |
| **Storage & Dependencies** | Python service, vector DB, API keys, GPU | Single self-contained static binary, pure Go, local SQLite |
| **Privacy & Security** | Source code often sent to third-party embedding APIs | **100% offline**, zero telemetry, runs air-gapped on your laptop |
| **Refactoring Safety** | Hallucination hazard; unsafe for edits | Atomic rollback with multi-file AST validation |

We didn't build RepoCortex to jump on an AI hype cycle. We built it because software engineering is a discipline of precision. Your tools should be as rigorous as the code you write.
