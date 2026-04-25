# h9s

Redis-backed document memory service for development. Runs as a standalone Docker Compose stack that any project can connect to for document retrieval via MCP.

## Quick start

```bash
# Enter devshell (provides go, docker-mcp)
nix develop

# Create config from template
cp configs/template.mcp-config.yaml configs/mcp-config.yaml

# Start services
docker compose up -d

# Verify
docker compose ps
```

Redis is available on `localhost:6380`. MCP Gateway on `localhost:8812`.

## Chunker

Splits markdown files by heading boundaries. Outputs structured JSON to stdout.

```bash
go run ./cmd/chunker /path/to/docs/
go run ./cmd/chunker README.md
```

Each chunk includes a heading breadcrumb, content, source file path, and line range.

## Connecting from other projects

Add the h9s MCP Gateway endpoint (`http://localhost:8812`) to your project's MCP configuration. The gateway exposes Redis tools (get, set, json_get, json_set, vector search, etc.).
