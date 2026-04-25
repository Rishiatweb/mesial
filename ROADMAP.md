# h9s Roadmap

## Done

- [x] Docker Compose: Redis Stack (port 6380) + MCP Gateway (port 8812)
- [x] MCP catalog: redis (45 tools), context7, github (26 tools), markitdown
- [x] Nix devshell with Go + docker-mcp
- [x] Go markdown chunker CLI (`cmd/chunker/`)
- [x] Global Claude Code MCP registration (`--scope user`)

## Next

### Embedding model
- Download `Qwen/Qwen3-Embedding-0.6B-GGUF` (Q6_K) into `models/`
- Run via `llama-server --embedding -m models/... --port 8090`
- Output dim: 1024 (Matryoshka — can truncate to 512/256 if needed)
- Pooling: last-token (matches Qwen3-Embedding)

### Redis data structure
- Design key schema for document chunks (e.g., `doc:{source}:{line_start}`)
- Create HNSW vector index: `create_vector_index_hash` with dim=1024, COSINE
- Fields per hash: breadcrumb, content, source, line_start, line_end, vector

### Go MCP server (ingestion)
- Restructure `cmd/chunker/` into an MCP server exposing `ingest_documents` tool
- Chunks markdown by heading boundaries (reuse existing chunker logic)
- Calls `llama-server /v1/embeddings` for each chunk
- Writes hashes + vectors directly to Redis via `go-redis`
- Package as container image for the compose stack
- Future: push to registry for cross-project use

### Retrieval integration
- Claude Code queries: embed query via llama-server, then `vector_search_hash`
- Consider a CLAUDE.md instruction or hook for automatic context retrieval
