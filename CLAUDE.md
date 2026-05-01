# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is h9s

Document memory and code graph service for development. Runs as a Docker Compose stack that other projects connect to via MCP. Two capabilities: (1) ingests markdown, embeds with Qwen3-Embedding, stores vectors in FalkorDB for semantic search; (2) analyzes TypeScript codebases with tree-sitter + LSP, stores structural code graphs (files, classes, functions, interfaces, relationships) in FalkorDB.

## Development environment

Requires Nix with flakes enabled. Enter the devshell (provides Go, docker-client, docker-mcp, typescript-language-server):

```bash
nix develop          # or let direnv activate via .envrc
```

Start/stop the service stack:

```bash
docker compose up -d    # FalkorDB (6381) + MCP Gateway (8812)
docker compose down
```

First-time setup requires copying the config template and inserting a GitHub PAT:

```bash
cp configs/template.mcp-config.yaml configs/mcp-config.yaml
# Edit configs/mcp-config.yaml to set your GitHub personal access token
```

## Build and run

```bash
go build ./...                # build all binaries
go run ./cmd/chunker <path>   # chunker CLI — accepts .md files or directories
go run ./cmd/ingest           # MCP ingestion server (stdio mode by default)
```

Run the ingest server in HTTP mode for development:

```bash
FALKOR_ADDR=localhost:6381 EMBEDDING_URL=http://localhost:8090 H9S_TRANSPORT=http go run ./cmd/ingest
```

Env vars for `cmd/ingest`: `FALKOR_ADDR` (default `localhost:6381`), `EMBEDDING_URL` (default `http://localhost:8090`), `H9S_GRAPH` (default `h9s`), `H9S_TRANSPORT` (`stdio`|`http`), `H9S_PORT` (default `8091`), `H9S_LSP_CMD` (default `typescript-language-server`).

Embedding model must be running separately:

```bash
llama-server --embedding -m models/Qwen3-Embedding-0.6B-Q8_0.gguf --port 8090
```

No tests or CI configured yet.

## Architecture

**Services** (docker-compose.yaml):
- **falkordb** — Graph database with vector search. Port 6381. Stores document chunks and code graph nodes.
- **ingest** — Go MCP server. Exposes `ingest_documents`, `search_documents`, and `analyze_repository` tools. HTTP on port 8091.
- **mcp-gateway** — Docker MCP Gateway exposing all tools over HTTP on port 8812.

**Go packages:**
- `internal/chunking/` — splits markdown by heading boundaries (h1-h6). Exports `Chunk` type, `ChunkFile`, `FindMarkdown`, `BuildBreadcrumb`.
- `internal/embedding/` — HTTP client for llama-server `/v1/embeddings`. Batches in groups of 32, truncates to configured dim, returns `[][]float32`.
- `internal/falkorstore/` — FalkorDB graph wrapper. Chunk operations: `EnsureIndex`, `StoreChunks`, `Search` (KNN). Code graph operations: `EnsureCodeIndex`, `AddFile`, `AddEntity`, `ConnectEntities`, `LookupEntityByPosition`.
- `internal/analyzer/` — Language-agnostic `Analyzer` interface + TypeScript implementation using tree-sitter. Two-pass `Orchestrator`: pass 1 (tree-sitter AST → entities + DEFINES edges), pass 2 (LSP → CALLS, EXTENDS, IMPLEMENTS, RETURNS, PARAMETERS edges).
- `internal/lspclient/` — LSP subprocess client for `typescript-language-server`. Manages initialization, DidOpen, Definition requests, and shutdown.
- `cmd/chunker/` — CLI wrapper over `internal/chunking`. Outputs JSON to stdout.
- `cmd/ingest/` — MCP server using `modelcontextprotocol/go-sdk`. Three tools: `ingest_documents`, `search_documents`, `analyze_repository`.

**Data model** — Per-repo FalkorDB graphs. Document graph ("h9s"): `:Chunk` nodes with vector (512-dim cosine). Code graph (named per repo): `File`, `Class`, `Function`, `Method`, `Constructor`, `Interface`, `Enum` nodes with `Searchable` meta-label. Relationships: `DEFINES`, `CALLS`, `EXTENDS`, `IMPLEMENTS`, `RETURNS`, `PARAMETERS`.

**Embedding** — Qwen3-Embedding-0.6B (Q8_0 GGUF, native 1024-dim, Matryoshka-truncated to 512) via llama-server on port 8090.

**Nix** (flake.nix) — pins nixpkgs-25.11-darwin (aarch64-darwin), builds docker-mcp v0.37.0 from GitHub releases, provides devshell with Go + Docker + docker-mcp + typescript-language-server.

## Key files

- `internal/chunking/chunking.go` — core markdown chunking logic
- `internal/embedding/client.go` — llama-server embedding client
- `internal/falkorstore/store.go` — FalkorDB vector index + chunk storage
- `internal/falkorstore/codegraph.go` — FalkorDB code graph node/edge operations
- `internal/analyzer/analyzer.go` — Analyzer interface + shared types (EntityInfo, FileInfo, SymbolKey)
- `internal/analyzer/typescript.go` — TypeScript tree-sitter parser + symbol extraction
- `internal/analyzer/orchestrator.go` — two-pass analysis pipeline
- `internal/lspclient/client.go` — LSP subprocess client
- `cmd/ingest/main.go` — MCP server wiring tools together
- `configs/custom-catalog.yaml` — MCP server and tool definitions (committed)
- `configs/template.mcp-config.yaml` — config template with secret placeholders
- `docker-compose.yaml` — service stack (falkordb, ingest, mcp-gateway)
- `Dockerfile.ingest` — multi-stage build for the ingest server
- `flake.nix` — Nix devshell and docker-mcp derivation
- `ROADMAP.md` — status and planned work
