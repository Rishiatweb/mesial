# Rationale

A decision-by-decision explainer. Every entry follows the same shape: *what we chose, why, what we rejected*.

## 1. FalkorDB instead of separate graph + vector stores

**Decision.** One database: FalkorDB. Holds both the code graph and the doc embeddings. One query language (Cypher); one connection; one consistency model.

**Why.** The interesting queries cross both kinds of data. "What docs describe this class?" is a single-edge traversal in mesial; in a split system it's a graph query, an embedding fetch, and an application-side join. FalkorDB ships native vector indices (`CREATE VECTOR INDEX FOR (c:Chunk) ON (c.vector)`), supports `db.SelectGraph(name)` for cheap multi-tenancy, and runs as a normal Redis-protocol server.

**Rejected.**
- *Neo4j + Qdrant.* Two systems, two failure modes, two ops surfaces.
- *Redis Stack with RediSearch.* Crashes on arm64 with vector indices (issue persisted across versions when this project started). Hard pass.

## 2. Per-repo graph isolation

**Decision.** One FalkorDB graph per repository, named `filepath.Base(repoPath)`. Each graph is fully self-contained: code entities, doc chunks, and the edges between them.

**Why.** A repository is the natural unit of mental scope for an agent. Most queries are about one project at a time. Per-graph isolation means:
- Identifier-name collisions across projects don't pollute each other (kg-agent's `User` and another repo's `User` don't connect).
- Wiping a repo is `delete_graph(name)` — no risk of orphaning data elsewhere.
- Schema can evolve per repo without locking everyone in.

**Rejected.**
- *Single global graph with a `repo` property on every node.* Heavier query filters everywhere; cleanup is a multi-thousand-row `DETACH DELETE`.
- *One FalkorDB instance per repo.* Operational overkill.

The global `h9s` graph still exists, reserved for cross-repo notes and (future) general memory.

## 3. `:Chunk` reused for docs (no new label)

**Decision.** Doc chunks live as `:Chunk` nodes. They are *not* additionally labeled `:Searchable`.

**Why.** `:Chunk` only existed for docs to begin with — no code path uses it. Adding it inside the per-repo graph required no schema invention. Skipping `:Searchable` is deliberate: that meta-label gates the fulltext-name index, and chunks aren't named entities — they're vector-retrieved. Marking them `:Searchable` would only insert breadcrumb-shaped strings into name-search results, which is noise.

**Rejected.**
- *`:DocChunk` distinct label.* Adds a moving part with no current benefit. Could split later if doc-chunks need to evolve independently of code-side chunks (none planned).

## 4. No `NEXT` edge between chunks

**Decision.** Within-file order is implicit via `Chunk.line_start`. Reading chunks of a file in order is a one-line query.

**Why.** `line_start` is already a total order within an `OF_FILE` cluster. Sorting is cheap. A `NEXT` edge would have to be torn down and rebuilt on every re-ingest, and it duplicates information already in the property.

**Trigger to revisit.** If a query pattern emerges that benefits from graph-native "previous chunk" / "next chunk" hops (e.g., reading three chunks before/after a specific one without sorts), add `NEXT` then.

## 5. `DOCUMENTS` targets code entities only — never `:File`

**Decision.** The linker excludes `:File` from candidate target labels. A doc that mentions `chunking.go` does *not* create `(:Chunk)-[:DOCUMENTS]->(:File)`.

**Why.** A file mention in prose is usually navigational, not semantic. "See `chunking.go`" tells you where to look, not what's documented. Including `:File` as a target inflates the edge count with low-signal links and weakens the meaning of `:DOCUMENTS` (which we want to read as "this chunk is *about* that thing").

**Rejected.**
- *Allow `:File` as target.* Higher recall, lower precision. We chose precision.

The structural relationship a chunk has to its file is captured by `:OF_FILE`, which is a different relationship (anchoring vs. describing).

## 6. Linker rule: backtick OR (length ≥ 4 AND ≥ 2 case segments)

**Decision.** Two paths to a link:
- **Backtick-fenced** identifier (e.g., `` `EventViewImpl` ``): always emits an edge if the name matches a code entity. No length, no segment requirement.
- **Bare** identifier (e.g., `EventViewImpl` mid-prose): must have `len ≥ 4` AND at least two case segments. PascalCase qualifies if it has an internal lower→upper transition (`EventViewImpl`) or an upper-run followed by a lowercase letter (`XMLParser`). Any camelCase qualifies. Snake_case qualifies if it has at least two non-empty parts (`get_pull_request`, `MAX_RETRY`).

**Why.** We sampled real `kg-agent` docs and found backtick-only mode would miss roughly 80% of references — disciplined backtick-fencing isn't a thing in real-world prose, even technical. The 2-segment rule then excludes single-word common nouns (`User`, `Item`, `Configuration`) that frequently collide with code symbol names without referring to them. The combination preserves recall on real docs without polluting the graph with `User → User` noise.

**Rejected.**
- *Backtick-only.* Too lossy on real docs.
- *Allow all-caps acronyms (`JSON`, `URL`) bare.* Too many false positives; backticks handle them cleanly.
- *Fulltext-index lookup.* Changes tokenization (camelCase splitting, stemming) — harder to reason about precision.

A `strict` flag is exposed on `ingest_documents` and `link_docs` to flip behavior to backtick-only when the user wants tight semantics.

## 7. Two-pass code analysis (tree-sitter, then LSP)

**Decision.** Pass 1 (tree-sitter) builds the structural skeleton — every entity by shape. Pass 2 (LSP via `typescript-language-server`) resolves cross-references and emits `:CALLS`, `:EXTENDS`, `:IMPLEMENTS`, `:RETURNS`, `:PARAMETERS`.

**Why.**
- Tree-sitter is fast, deterministic, and language-aware enough to identify entities by their AST shape. Its grammar, not its semantics, is what we need for definition extraction.
- LSP knows TypeScript's resolution rules — type aliases, re-exports, `tsconfig` paths, the works. Reimplementing that would be huge and brittle.
- Splitting the passes lets pass 1 succeed even if the LSP fails to start (returns a partial result rather than nothing).
- The interface is language-agnostic: a Go or Python analyzer would slot into the same shape.

**Rejected.**
- *All tree-sitter.* Can't follow imports correctly; pass 2 edges would be unreliable.
- *All LSP.* No schema control over what counts as an entity.
- *TypeScript Compiler API.* TS-specific and harder to extend; would lock us into Node tooling.

## 8. Oversized chunks: stored, not embedded

**Decision.** Chunks longer than 6000 characters (~1500 tokens) are stored as `:Chunk` nodes with `oversized: true` and full content, but no vector. Vector search ignores them naturally. The linker still scans them.

**Why.** Three reasons stack up:
- *Embedding quality.* A 10k-char section embedded into a single 512-dim vector loses the discriminative signal we want from KNN. The embedding becomes "vaguely about this whole topic" — worse for retrieval than skipping it.
- *Server limits.* `llama-server` has a context-size ceiling. Long chunks risk crashing the server (we hit this on the `kg-agent` README).
- *Information preservation.* The linker scans `c.content`, not `c.vector`. Storing the chunk preserves identifier mentions for the doc-to-code edges we care about. The chunk shows up in graph traversals and reads of "give me everything in this file"; only KNN ignores it.

**Rejected.**
- *Skip silently.* Loses linker coverage on the section. The 304-line "Guardrailed Agent" example would emit zero `DOCUMENTS` edges despite being identifier-rich.
- *Truncate to 6000 chars and embed the head.* Silent information loss; the embedding misrepresents the section.
- *Recursive sub-chunking on paragraph boundaries.* The right long-term move (would let everything be vectorized at appropriate granularity), but real work. Punted because the current design doesn't lose anything important.

The threshold (6000 chars) is a single constant in `cmd/ingest/main.go::oversizedChunkChars`. Easy to tune.

## 9. Repo resolution: explicit → `.git` walk-up → error

**Decision.** For `ingest_documents` and `link_docs`:
1. If `repo` is given explicitly, use it.
2. Else walk up from the input path (`Paths[0]` for ingest) looking for a `.git` directory; use `filepath.Base` of that ancestor.
3. Else error. No global fallback.

For `search_documents`, the same chain runs but the final fallback is the global `H9S_GRAPH` instead of error.

**Why.** Silently writing repo docs into the global graph because the agent forgot to include a path would be a memorable bug. Erroring forces intent. Search is asymmetric because cross-cutting queries against the global graph are genuinely useful (notes, scratchpads), and the cost of "you searched the wrong graph" is just empty results, not corrupted state.

**Rejected.**
- *Auto-detect from process CWD.* MCP server CWD is fixed at startup and rarely correlates with the agent's task. Magic that's almost never useful.
- *Always require explicit `repo`.* Annoying for an agent to remember at every call. The `.git` walk-up is the right default.

## 10. The boundary between mesial and the agent

**Decision.** Mesial does mechanical, surface-level operations: chunking, embedding, structural parsing, name-equality matching. The agent does interpretation: deciding what a doc means, judging whether a connection is meaningful, ranking results.

**Why.** The two have fundamentally different shapes. Mechanical operations are deterministic, parallelizable, and cacheable — perfect for a persistent service. Interpretive operations depend on context, instructions, and the model's understanding of the user's goal. Putting interpretation in mesial would require running an LLM inside the memory layer, which makes the layer slow, expensive, and stateful in unwanted ways.

**Concrete consequences.**
- The doc-to-code linker is mechanical (does `EventViewImpl` appear in chunk content?), not interpretive (does this chunk *describe* `EventViewImpl`?). The agent reads the linked chunk and decides for itself.
- Search ranks by vector similarity, not by reasoning. The agent re-ranks with its own judgment if needed.
- The ingest pipeline is opaque: documents go in, structure comes out. Style guides, formatting preferences, "what's worth saving" — all the agent's job.

This is the load-bearing distinction. Adding interpretive layers to mesial is the path to it becoming "small AI service that's eaten by the next general agent." Keeping mesial mechanical keeps it useful indefinitely.

## 11. What's deliberately not built (yet)

| Capability | Why not now |
|---|---|
| **Multiple languages.** | Nothing prevents it — the `Analyzer` interface is language-agnostic. Just no demand yet. Python and Go would each be a few hundred lines + grammar. |
| **Cross-repo queries.** | Would require either a federated query layer or a single global graph (rejected, see #2). Add when there's demand from real workflows. |
| **General memory graph.** | User preferences, cross-project facts, agent-side scratch. Different data model — likely small typed nodes, no embeddings. Will live in a separate graph (`h9s` global), not blended with code/docs. |
| **Code embeddings.** | Code entities aren't vectorized; they're queried structurally. If "find code semantically similar to X" becomes a real need, that's a future extension — probably embedding `:Function` `:doc` strings and storing alongside structure. |
| **Aho–Corasick / better linker algorithm.** | The current `O(chunks × names)` scan is fine to ~10k × 10k. Switch when scale demands. |
| **Stale `:File` cleanup on rename.** | Acceptable to leak old `:File` nodes. Cleanup is its own design problem (also affects renamed code files); not blocking anything. |
| **Multi-path repo consistency for ingest.** | Already enforced — paths in a single `ingest_documents` call must share a `.git` ancestor. Explicit error is the contract. |
| **Per-chunk `index of total` properties.** | Computable on demand (`count(c)` on the file's chunks). No use case yet that's better served by stored values than by query. |

These aren't "todo" items. They're the boundary of the current design — the decisions to push back on if a future requirement seems to need them.
