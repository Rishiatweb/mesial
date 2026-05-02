# Mesial Roadmap

## What works today

### Code + docs per-repo graphs
- TypeScript code analysis via tree-sitter (pass 1) + `typescript-language-server` (pass 2)
- Node labels: `:File`, `:Class`, `:Function`, `:Method`, `:Constructor`, `:Interface`, `:Enum` (all `:Searchable`)
- Edges: `:DEFINES`, `:CALLS`, `:EXTENDS`, `:IMPLEMENTS`, `:RETURNS`, `:PARAMETERS`
- Markdown chunking by heading boundaries (`internal/chunking`)
- 512-dim embeddings (Qwen3-Embedding-0.6B, Matryoshka-truncated from 1024) via llama-server
- `:Chunk` nodes with vector index (cosine, FalkorDB native)
- `:OF_FILE` edges anchoring chunks to their source `.md` files
- `:DOCUMENTS` edges from chunks to mentioned code entities, via the identifier-mention linker (`internal/doclinker`)
- Oversized-chunk handling: chunks > 6000 chars stored without a vector, full content preserved, still scanned by the linker
- Per-repo graph isolation (one FalkorDB graph per repository, named after `.git` ancestor or explicit override)

### MCP surface
- `analyze_repository` — full code + docs + linker pass
- `ingest_documents` — incremental markdown ingestion
- `search_documents` — vector similarity, with optional `repo`/`path` scoping and global fallback
- `link_docs` — re-run the doc-to-code linker

### Infrastructure
- Docker Compose: FalkorDB + MCP gateway
- Embedding model: Qwen3-Embedding-0.6B-Q8_0.gguf, llama-server with `--ctx-size 8192`
- Nix devshell (Go, docker-mcp, typescript-language-server)
- Two-binary architecture: `cmd/ingest` (MCP server) and `cmd/h9s-cli` (direct dev harness), both adapters over `internal/pipeline`

### Documentation
- `docs/DESIGN.md` (high-level), `docs/ARCHITECTURE.md` (engineering view), `docs/RATIONALE.md` (decision-by-decision)

## Next

### Notes / vault graphs

A second graph shape for markdown-only collections (Obsidian vaults, personal note systems, design-doc archives) where the rich relationships are wiki-links and tags rather than code mentions.

- Extract `[[wiki-link]]` references during ingestion → `(:Chunk)-[:LINKS_TO]->(:File)` edges
- Extract `#tag` references → `:Tag:Searchable {name}` nodes + `:TAGGED` edges (chunk-source for inline, file-source for YAML front-matter)
- Resolve wiki-links by file-name match within the same vault graph; drop unresolved links silently
- Co-exist cleanly with code+docs graphs — wiki-link/tag extraction is additive on every `.md` ingestion, no-op for files that don't use the syntax
- Schema documented in [`docs/ARCHITECTURE.md#notes--vault-graphs-planned`](docs/ARCHITECTURE.md#notes--vault-graphs-planned)

### Other near-term
- Push the `ingest` Docker image to a registry for cross-machine use
- Incremental re-indexing — detect changed files via git, ingest only those
- Hybrid search (lexical + vector) in `search_documents` for high-precision queries

## Future

- **Python and Go analyzers.** The `Analyzer` interface is language-agnostic; adding a language is a grammar + symbol-extraction visitor + LSP wiring.
- **Cross-repo / cross-vault federated queries.** A query layer that fans out to multiple FalkorDB graphs and merges results.
- **General memory graph.** User preferences, cross-project facts, agent-side scratch. Different data model (small typed nodes, no embeddings) — lives in the global `h9s` graph.
- **Recursive sub-chunking on paragraph boundaries.** Splits oversized sections so every section can be vectorized at appropriate granularity.
- **Code embeddings.** Vectorize `Function.doc` strings (or short summaries) for "find code semantically similar to X" use cases.
- **Pre-computed similarity edges.** `(:Chunk)-[:SIMILAR_TO {score}]->(:Chunk)` written at ingest time. Partially redundant with on-demand KNN; deferred until a concrete use case demands stored similarity.
- **`:File` rename cleanup.** Remove orphaned `:File` nodes whose source has moved.

## Notes

- RediSearch vector indexing crashes on arm64/Apple Silicon (Redis Stack 7.4, Redis 8.6). Pivoted to FalkorDB which works correctly on arm64.
- 512-dim Matryoshka truncation from native 1024 — adequate for retrieval at this scale.
