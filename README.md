# mesial

Document memory and code graph service for AI agents working in codebases.

The repository is named **mesial** after the *mesial temporal lobe* — the region of the brain that houses the **hippocampus** (`h9s` in the codebase). The hippocampus is the brain region most associated with declarative memory and spatial navigation. Mesial gives agents three faculties for the projects they work in:

- **Knowledge** — semantic search over project documentation
- **Memory** — durable per-repository context across sessions
- **Navigation** — graph-shaped traversal of code structure and the docs that describe it

Mesial parses TypeScript with tree-sitter + `typescript-language-server`, chunks markdown by heading boundaries, and stores both into a per-repository FalkorDB graph. Doc chunks are linked to the code entities they mention via a `DOCUMENTS` edge, so an agent can navigate from a topic to its code, or from a code entity to the docs that describe it.

For background, see [`docs/DESIGN.md`](docs/DESIGN.md). For the engineering view, [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). For the why behind specific choices, [`docs/RATIONALE.md`](docs/RATIONALE.md).

## Quick start

Requires Nix with flakes (`nix develop` or direnv via `.envrc`).

```bash
# First-time config — copy template and add your GitHub PAT
cp configs/template.mcp-config.yaml configs/mcp-config.yaml
# (edit configs/mcp-config.yaml)

# Three processes — three terminals:
make up      # FalkorDB + MCP gateway in docker
make embed   # llama-server with Qwen3-Embedding-0.6B (foreground)
make ingest  # the Go MCP server (HTTP on :8091, foreground)
```

For development the typical setup is FalkorDB in Docker, llama-server and the ingest server on the host. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#process-model) for the alternatives.

Services land at:

| Port | Service |
|------|---------|
| `:6381` | FalkorDB |
| `:8090` | llama-server (embeddings) |
| `:8091` | ingest server (Streamable HTTP MCP) |
| `:8812` | MCP gateway |

## Connecting an agent

Mesial exposes its tools through the docker MCP gateway. Register it once in Claude Code (or any MCP-aware client):

```bash
claude mcp add --transport http --scope user h9s-memory http://localhost:8812/mcp
```

The agent then sees these tools:

| Tool | Purpose |
|------|---------|
| `analyze_repository` | Full ingestion of a repo: TS code graph + markdown chunks + linker |
| `ingest_documents` | Add markdown files to a repo's graph |
| `search_documents` | Semantic search by query (with optional repo scope) |
| `link_docs` | Re-run the doc-to-code linker on an existing graph |
| `query_graph` / `query_graph_readonly` | Direct Cypher (via the gateway) |

## Common tasks

### Analyze a repository

```jsonc
// analyze_repository
{ "path": "/path/to/your/repo", "ignore": [] }
```

Parses TS, ingests markdown, runs the linker. Stores everything in a graph named after `filepath.Base(path)`. Re-running is safe — chunks are detached + recreated, edges are MERGE-based.

### Add documentation to a repo's graph

```jsonc
// ingest_documents
{ "paths": ["/path/to/repo/docs"] }
```

Repo is auto-detected from the nearest `.git` ancestor. Pass `"repo": "name"` to override. Pass `"strict": true` to only link backtick-fenced identifiers (skip bare-token matching).

### Search across a repo's docs

```jsonc
// search_documents
{ "query": "how does the harness pattern work?", "repo": "kg-agent", "k": 5 }
```

Returns the top-k chunks by vector similarity. Omit both `repo` and `path` to search the global `h9s` graph (cross-repo notes).

### Find what docs describe a class

```cypher
MATCH (c:Chunk)-[:DOCUMENTS]->(:Class {name: "EventViewImpl"})
RETURN c.source, c.line_start, c.breadcrumb, c.content
```

### Find every method called by a function

```cypher
MATCH (:Function {name: "ingestDocsToStore"})-[:CALLS]->(target)
RETURN labels(target), target.name, target.path
```

### Read a doc file's chunks in order

```cypher
MATCH (c:Chunk)-[:OF_FILE]->(f:File {name: "ARCHITECTURE.md"})
RETURN c.breadcrumb, c.line_start, c.content
ORDER BY c.line_start
```

## Layout

```
cmd/
  chunker/        markdown chunker as a CLI
  ingest/         the MCP server
internal/
  chunking/       heading-boundary markdown chunker
  embedding/      llama-server client (batched, Matryoshka-truncated)
  falkorstore/    FalkorDB graph operations (chunks + code graph)
  analyzer/       Analyzer interface + TS analyzer + two-pass orchestrator
  lspclient/      typescript-language-server client
  doclinker/      doc-to-code identifier-mention linker
configs/          MCP gateway catalog and config template
docs/             architecture, design, rationale
```

## Development

```bash
go build ./...                          # all binaries
go test ./...                           # currently: doclinker unit tests
go run ./cmd/chunker /path/to/file.md   # standalone chunker
```

The ingest server has two transport modes:

```bash
H9S_TRANSPORT=stdio go run ./cmd/ingest   # stdio (when called by an MCP client directly)
H9S_TRANSPORT=http  go run ./cmd/ingest   # HTTP on :8091 (the dev path; this is what `make ingest` does)
```

See [`docs/ARCHITECTURE.md#configuration`](docs/ARCHITECTURE.md#configuration) for all env vars.
