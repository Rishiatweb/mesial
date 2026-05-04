# Design

## What mesial is

A persistent service that ingests a project's code and documentation into a queryable graph, and serves that graph to AI agents over MCP.

## The name

The github repository is **mesial**, after the **mesial temporal lobe** — the region of the brain that houses the **hippocampus** (`h9s` in the codebase, binaries, and env vars). The hippocampus is the brain region most associated with **declarative memory** (what you know, what you've seen) and **spatial navigation** (place cells that map your environment).

By analogy, mesial offers AI agents three faculties for the projects they work in:

- **Knowledge** — semantic search over project documentation.
- **Memory** — durable per-repository context that survives across sessions.
- **Navigation** — graph-shaped traversal of code structure (calls, hierarchies, types) and the documents that describe it.

## What lives in mesial

Mesial recognizes three graph shapes — all FalkorDB graphs, isolated per "thing" they describe. Each carries a singleton `(:GraphMeta {kind, strict})` declaring which vocabulary it speaks: `code`, `notes`, or `memory`. `strict: false` is the default — graphs may accumulate off-vocabulary nodes when it makes sense (a `code` graph holding facts about its own code; a vault using `[[wiki-link]]` syntax inside a code repo's `docs/`).

### Code+docs graphs (one per repository)

For each repository it knows about, mesial maintains a single graph containing three interleaved layers:

**Code structure.** Files, classes, functions, methods, constructors, interfaces, enums. Edges for definition (`DEFINES`), call (`CALLS`), inheritance (`EXTENDS`, `IMPLEMENTS`), and type relations (`RETURNS`, `PARAMETERS`).

**Documents.** Markdown files chunked at heading boundaries. Each chunk carries a 512-dim embedding (Qwen3-Embedding-0.6B truncated via Matryoshka), an `OF_FILE` edge to its source, and a `breadcrumb` of headings. Sections too large for a single useful embedding are stored without a vector but keep their content for graph traversal.

**The bridge.** Every chunk is scanned for identifier mentions (`EventViewImpl`, `getPullRequest`, ...) — both as inline backtick spans and as bare tokens that match a code-entity name. Each match emits a `(:Chunk)-[:DOCUMENTS]->(:CodeEntity)` edge. The result: from any code symbol you can find the docs that describe it; from any chunk you can find the code it talks about.

### Vault graphs (one per note collection, planned)

For markdown-only collections — Obsidian vaults, personal note systems, long-form design archives — the meaningful relationships are different: notes link to each other via `[[wiki-link]]` syntax, and `#tag` references group them into emergent topics. Vault graphs reuse the same `:File` and `:Chunk` shape, but trade `:DOCUMENTS` (chunk → code entity) for `:LINKS_TO` (chunk → file) and `:TAGGED` (chunk/file → tag), and add a `:Tag:Searchable` node label.

A repository can be both at once: TypeScript code and a `docs/` directory using wiki-link cross-references. The two schemas compose without conflict in the same graph (this is exactly what `strict: false` permits). Schema details in [`ARCHITECTURE.md`](ARCHITECTURE.md#notes--vault-graphs-planned).

### Memory graph (one global, planned)

Holds what an agent has learned: how the user prefers to work, how a process behaves, what was observed during prior sessions. Three node labels carry distinct epistemological roles:

- **`:Observation`** — a sentence (rarely two) about an episode or pattern. Free-form, embedded for KNN recall, written cheaply during conversation. Episodic.
- **`:Fact`** — a triplet `(subject, predicate, object)`. Distilled from observations, durable, slow to change, *not* embedded — its semantic locality is inherited through `:EVIDENCE_FOR` edges to the observations that back it. Semantic.
- **`:Protocol`** — procedural memory; how-to. Schema TBD.

Hippocampus framing is literal here: observations consolidate into facts via `:EVIDENCE_FOR`, mirroring how episodic memories consolidate into semantic ones during sleep. **No fact enters the graph without ≥ 1 supporting observation** — distillation is the only entry path, which keeps the semantic layer grounded.

A predicate kernel of seven well-known relations (`is_a`, `subtype_of`, `part_of`, `equivalent_to`, `incompatible_with`, `causes`, `requires`) gives a future inference engine something to chew on; arbitrary predicates are allowed but inert to the engine. Schema details and the full edge set in [`ARCHITECTURE.md`](ARCHITECTURE.md#memory-facts-observations-protocols-planned).

### Two layers, one graph: perceptual vs conceptual

Across all three graph kinds, mesial separates a **perceptual** layer (literal text + embeddings) from a **conceptual** layer (structured nodes queried by name and structure). The two layers interlock through edges:

```
:Fact ← :EVIDENCE_FOR ← :Observation -[:MOTIVATES]→ :Chunk -[:DOCUMENTS]→ :CodeEntity
```

The conceptual layer (`:Fact`, `:CodeEntity`, `:File`) doesn't carry its own embeddings. It inherits semantic locality through its anchors in the perceptual layer (`:Observation`, `:Chunk`). Asking "what do I know about deployment?" is a vector search over observations and chunks, then a structural traversal into the facts and code entities they touch — two hops, one embedding, no duplicated representation.

Most observations about a repo's code live in that repo's graph (`strict: false` permits this); the dedicated `memory` graph holds only what's genuinely cross-cutting. Schema details in [`ARCHITECTURE.md`](ARCHITECTURE.md#memory-facts-observations-protocols-planned).

## What an agent does with it

The MCP surface is intentionally narrow:

- **`analyze_repository`** — full re-analysis of a repo: TS code → markdown → linker.
- **`ingest_documents`** — incremental ingestion of one or more `.md` paths into a repo's graph.
- **`search_documents`** — vector similarity over chunks, optionally scoped to a repo.
- **`link_docs`** — re-run the doc-to-code linker (idempotent; useful after code changes).
- **`query_graph`** / **`query_graph_readonly`** (via the gateway) — direct Cypher.

The mental model is *spatial*. An agent doesn't grep a repository; it **navigates** it. From a topic (a doc chunk) to the code entities that realize it. From a code entity to the docs that describe it. Through call graphs to find related code without reading source files.

## What it isn't

Deliberate non-goals, with reasoning in [`RATIONALE.md`](RATIONALE.md):

- **Not an interpretive linker.** Connections are made by surface match — identifier names that appear in both a doc and the code graph. Connecting "the auth flow" prose to `AuthService` requires an agent's understanding, not the memory's. Mesial provides anchors; the agent does the bridging.
- **Not cross-repo.** Each repository has its own graph. Cross-repo queries would need a federated layer; not built.
- **Not (yet) a general-memory store.** The schema for the `memory` graph (facts, observations, protocols) is designed but unimplemented; agent-orchestrated distillation tooling is deferred until the data design is fully ratified. See [`ARCHITECTURE.md`](ARCHITECTURE.md#memory-facts-observations-protocols-planned).
- **Not a code-search engine.** Code entities aren't embedded — they're queried structurally. "Find code semantically similar to X" is a different shape of problem.

## Where to go next

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — the engineering view: services, packages, pipelines, data model.
- [`RATIONALE.md`](RATIONALE.md) — decision-by-decision explainer.
