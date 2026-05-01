# h9s

Document memory and code graph service for development. Runs as a Docker Compose stack that any project can connect to via MCP for document ingestion, semantic search, and TypeScript code graph analysis.

Uses FalkorDB (graph database with vector search), Qwen3-Embedding-0.6B for embeddings, tree-sitter and `typescript-language-server` for code analysis, and the MCP protocol for tool access.

## Quick start

```bash
# Enter devshell (provides go, docker-mcp, typescript-language-server)
nix develop

# Create config from template
cp configs/template.mcp-config.yaml configs/mcp-config.yaml
# Edit configs/mcp-config.yaml to set your GitHub personal access token

# Start services (FalkorDB, ingest, MCP Gateway)
make up

# Start embedding server (in a separate terminal)
make embed

# Start ingest server in HTTP mode (in a separate terminal)
make ingest
```

Services: FalkorDB on `localhost:6381`, MCP Gateway on `localhost:8812`, Ingest server on `localhost:8091`.

## MCP tools

The ingest server exposes three tools via MCP:

- **`ingest_documents`** — takes file/directory paths, chunks markdown by heading boundaries, embeds with Qwen3-Embedding, stores as graph nodes in FalkorDB with cosine vector index.
- **`search_documents`** — takes a natural language query, embeds it, returns the top-k most similar chunks.
- **`analyze_repository`** — parses a TypeScript/TSX codebase with tree-sitter, resolves symbols via `typescript-language-server`, and stores the code graph (File, Class, Function, Method, Constructor, Interface, Enum nodes; DEFINES, CALLS, EXTENDS, IMPLEMENTS, RETURNS, PARAMETERS edges) in a per-repo FalkorDB graph.

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

The gateway exposes FalkorDB graph queries and the h9s ingest, search, and analyze tools.
