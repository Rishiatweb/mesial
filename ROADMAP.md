# h9s Roadmap

## Done

- [x] Docker Compose: Redis Stack (port 6380) + MCP Gateway (port 8812)
- [x] MCP catalog: redis (45 tools), context7, github (26 tools), markitdown
- [x] Nix devshell with Go + docker-mcp
- [x] Go markdown chunker CLI (`cmd/chunker/`)
- [x] Global Claude Code MCP registration (`--scope user`)
- [x] Embedding model: `Qwen3-Embedding-0.6B-Q8_0.gguf` (1024-dim, Matryoshka) in `models/`
- [x] FalkorDB vector store: Chunk nodes with 512-dim cosine vector index
- [x] Go MCP server (`cmd/ingest/`): `ingest_documents` + `search_documents` tools
- [x] Chunking library extracted to `internal/chunking/`
- [x] Embedding client (`internal/embedding/`) for llama-server `/v1/embeddings`
- [x] FalkorDB store (`internal/falkorstore/`) with vector index + KNN search
- [x] Dockerfile.ingest + compose service + MCP catalog registration

- [x] TypeScript code graph analyzer (`internal/analyzer/`) — tree-sitter pass 1 + LSP pass 2
- [x] Language-agnostic `Analyzer` interface for future Python/Go support
- [x] FalkorDB code graph methods (`internal/falkorstore/codegraph.go`)
- [x] LSP client (`internal/lspclient/`) for typescript-language-server
- [x] MCP tool `analyze_repository` — per-repo graph isolation
- [x] Graph schema: File, Class, Function, Method, Constructor, Interface, Enum + Searchable meta-label
- [x] Relationships: DEFINES, CALLS, EXTENDS, IMPLEMENTS, RETURNS, PARAMETERS

## Next

### Embedding model runtime
- Run via `llama-server --embedding -m models/Qwen3-Embedding-0.6B-Q8_0.gguf --port 8090`
- Consider adding llama-server to compose stack or Nix devshell

### Retrieval integration
- Claude Code queries: embed query via `search_documents` tool
- Consider a CLAUDE.md instruction or hook for automatic context retrieval

### Future
- Push ingest image to registry for cross-project use
- Add non-markdown format support (via markitdown conversion)
- DOCUMENTS edges linking doc chunks to code entities
- Python and Go analyzers
- Incremental re-indexing (detect changed files via git)

## Notes

- RediSearch vector indexing crashes on arm64/Apple Silicon (tested Redis Stack 7.4, Redis 8.6). Pivoted to FalkorDB which works correctly on arm64.
- Using 512-dim Matryoshka truncation (from native 1024) — adequate for document retrieval.
