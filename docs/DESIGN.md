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

Mesial recognizes two graph shapes — both are FalkorDB graphs, isolated per "thing" they describe.

### Code+docs graphs (one per repository)

For each repository it knows about, mesial maintains a single graph containing three interleaved layers:

**Code structure.** Files, classes, functions, methods, constructors, interfaces, enums. Edges for definition (`DEFINES`), call (`CALLS`), inheritance (`EXTENDS`, `IMPLEMENTS`), and type relations (`RETURNS`, `PARAMETERS`).

**Documents.** Markdown files chunked at heading boundaries. Each chunk carries a 512-dim embedding (Qwen3-Embedding-0.6B truncated via Matryoshka), an `OF_FILE` edge to its source, and a `breadcrumb` of headings. Sections too large for a single useful embedding are stored without a vector but keep their content for graph traversal.

**The bridge.** Every chunk is scanned for identifier mentions (`EventViewImpl`, `getPullRequest`, ...) — both as inline backtick spans and as bare tokens that match a code-entity name. Each match emits a `(:Chunk)-[:DOCUMENTS]->(:CodeEntity)` edge. The result: from any code symbol you can find the docs that describe it; from any chunk you can find the code it talks about.

### Vault graphs (one per note collection, planned)

For markdown-only collections — Obsidian vaults, personal note systems, long-form design archives — the meaningful relationships are different: notes link to each other via `[[wiki-link]]` syntax, and `#tag` references group them into emergent topics. Vault graphs reuse the same `:File` and `:Chunk` shape, but trade `:DOCUMENTS` (chunk → code entity) for `:LINKS_TO` (chunk → file) and `:TAGGED` (chunk/file → tag), and add a `:Tag:Searchable` node label.

A repository can be both at once: TypeScript code and a `docs/` directory using wiki-link cross-references. The two schemas compose without conflict in the same graph. Schema details in [`ARCHITECTURE.md`](ARCHITECTURE.md#notes--vault-graphs-planned).

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
- **Not a general-memory store.** User preferences and cross-project facts will live in a separate `h9s` graph with a different data model (planned).
- **Not a code-search engine.** Code entities aren't embedded — they're queried structurally. "Find code semantically similar to X" is a different shape of problem.

## Where to go next

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — the engineering view: services, packages, pipelines, data model.
- [`RATIONALE.md`](RATIONALE.md) — decision-by-decision explainer.
