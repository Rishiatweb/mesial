# h9s

Document memory service for development. Runs as a Docker Compose stack that any project can connect to for document ingestion and semantic search via MCP.

Uses FalkorDB (graph database with vector search), Qwen3-Embedding-0.6B for embeddings, and the MCP protocol for tool access.

## Quick start

```bash
# Enter devshell (provides go, docker-mcp)
nix develop

# Create config from template
cp configs/template.mcp-config.yaml configs/mcp-config.yaml
# Edit configs/mcp-config.yaml to set your GitHub personal access token

# Start services (FalkorDB, Redis Stack, MCP Gateway)
make up

# Start embedding server (in a separate terminal)
make embed

# Start ingest server in HTTP mode (in a separate terminal)
make ingest
```

Services: FalkorDB on `localhost:6381`, Redis Stack on `localhost:6380`, MCP Gateway on `localhost:8812`, Ingest server on `localhost:8091`.

## MCP tools

The ingest server exposes two tools via MCP:

- **`ingest_documents`** — takes file/directory paths, chunks markdown by heading boundaries, embeds with Qwen3-Embedding, stores as graph nodes in FalkorDB with cosine vector index.
- **`search_documents`** — takes a natural language query, embeds it, returns the top-k most similar chunks.

## Chunker CLI

Splits markdown files by heading boundaries. Outputs structured JSON to stdout.

```bash
go run ./cmd/chunker /path/to/docs/
go run ./cmd/chunker README.md
```

Each chunk includes a heading breadcrumb, content, source file path, and line range.

## Connecting from other projects

Register the h9s MCP Gateway as an MCP endpoint in Claude Code:

```bash
claude mcp add --transport http --scope user h9s-memory http://localhost:8812/mcp
```

The gateway exposes FalkorDB graph queries, Redis tools, and the h9s ingest/search tools.
