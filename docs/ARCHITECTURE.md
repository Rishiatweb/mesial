# Architecture

The engineering view of mesial: services, processes, packages, pipelines, data model.

## Service topology

```
┌─────────────────┐     stdio / streamable HTTP    ┌────────────────────┐
│ Claude Code     │───────────────────────────────▶│ MCP Gateway        │
│ (or other MCP   │           :8812                 │ docker mcp-gateway │
│  client)        │                                 └────────┬───────────┘
└─────────────────┘                                          │
                                                              │ proxies tool calls
                                                              ▼
                                              ┌──────────────────────────┐
                                              │ ingest server (Go)       │
                                              │ streamable HTTP :8091    │
                                              │ cmd/ingest               │
                                              └──┬───────────────────┬───┘
                                                 │                   │
                                  embeddings via │                   │ FalkorDB
                                  POST /v1/embed │                   │ protocol
                                                 ▼                   ▼
                                       ┌──────────────────┐  ┌────────────────────┐
                                       │ llama-server     │  │ FalkorDB           │
                                       │ :8090            │  │ :6381 (graph + vec)│
                                       │ Qwen3-Embedding  │  │ falkordb-h9s       │
                                       │ -0.6B Q8_0 GGUF  │  │ (named per-repo    │
                                       └──────────────────┘  │  graphs)           │
                                                              └────────────────────┘
```

Each box is a process. FalkorDB and the MCP gateway run in Docker; llama-server and the ingest server can run either in Docker (`make up`) or as host processes for development (`make embed`, `make ingest`).

## Process model

| Process | Where | Started by | Purpose |
|---|---|---|---|
| **falkordb** | Docker (`falkordb-h9s`) | `docker compose up -d falkordb` | Graph database with built-in vector indices. Per-repo graphs via `db.SelectGraph(name)`. State persisted in the `falkor_data` volume. |
| **mcp-gateway** | Docker | `docker compose up -d mcp-gateway` | `docker/mcp-gateway` running streamable HTTP on `:8812`. Reads `configs/custom-catalog.yaml` (tool definitions) and `configs/mcp-config.yaml` (secrets). |
| **ingest server** | Docker (`h9s-ingest`) or host (`make ingest`) | `docker compose up -d ingest` or `go run ./cmd/ingest` | The Go MCP server. Talks to FalkorDB and llama-server. Streamable HTTP on `:8091`. |
| **llama-server** | Host | `make embed` | Serves Qwen3-Embedding-0.6B over OpenAI-compatible `/v1/embeddings`. `--ctx-size 8192` configured for long doc chunks. |

For development, the typical setup is:
- FalkorDB in Docker (`docker compose up -d falkordb`)
- llama-server on host (`make embed`)
- ingest server on host (`make ingest`) — uses `go run` so each restart picks up code changes.

The Docker `ingest` container is for production / "always-on" use; it would conflict with the host server on port 8091, so leave it stopped during dev.

## Go package layout

| Package | Responsibility |
|---|---|
| `cmd/ingest` | MCP server entrypoint. Wires tools, holds the global store, and implements the request handlers including helpers `resolveRepoGraphName` and `ingestDocsToStore`. |
| `cmd/chunker` | Standalone CLI over `internal/chunking`. Outputs JSON chunks to stdout. |
| `internal/chunking` | Markdown chunker. Splits on `#`–`######` heading boundaries; each chunk carries breadcrumb, content, source path, and line range. |
| `internal/embedding` | Client for llama-server's `/v1/embeddings`. Batches in groups of 32, applies Matryoshka truncation to 512-dim. |
| `internal/falkorstore` | All FalkorDB operations. `store.go` covers chunks; `codegraph.go` covers code entities and relationships. Same `Store` type, same package. |
| `internal/analyzer` | Language-agnostic `Analyzer` interface plus a TypeScript implementation. The two-pass `Orchestrator` runs tree-sitter then LSP. |
| `internal/lspclient` | Subprocess client for `typescript-language-server`. Handles JSON-RPC framing, `initialize`, `didOpen`, `definition`, and shutdown. |
| `internal/doclinker` | Doc-to-code identifier-mention linker. Pure logic over the store. |
| `configs/` | MCP gateway catalog (`custom-catalog.yaml`, committed) and config template (`template.mcp-config.yaml`). The real config (with secrets) is gitignored. |

## Pipelines

### Code analysis (two passes)

1. **Tree-sitter** (`analyzer.Orchestrator.pass1`): walk the repo, parse `.ts`/`.tsx` with the typescript and tsx grammars. Build per-file `EntityInfo` trees, MERGE `:File:Searchable` and entity nodes (`:Class`, `:Function`, `:Method`, `:Constructor`, `:Interface`, `:Enum` — all dual-labeled `:Searchable`), connect parents to children via `:DEFINES` edges. Collect unresolved symbols (call sites, extends targets, return-type references, parameter types) for pass 2.
2. **LSP** (`pass2`): start `typescript-language-server --stdio`. For each unresolved symbol, send `textDocument/definition`. Parse the returned location; look up the target entity by file + line via `store.LookupEntityByPosition` (which returns the innermost entity covering that line); emit the appropriate edge — `:CALLS`, `:EXTENDS`, `:IMPLEMENTS`, `:RETURNS`, or `:PARAMETERS`. If LSP fails to start, pass 1 results are kept and pass 2 is skipped.

### Doc ingestion (per source file)

1. **Chunk** by heading boundaries (`chunking.ChunkFile`).
2. **MERGE** the `:File:Searchable` node for the source `.md` (`store.AddFile`).
3. **DETACH DELETE** prior chunks for this source (`store.DeleteBySource`) — picks up renames and edits.
4. **Partition** chunks: ≤ 6000 chars → embed; > 6000 chars → store without vector.
5. **Embed** the normal partition in batches of 32 (`embedding.Client.Embed`); truncate to 512-dim.
6. **Store**: `store.StoreChunks` for normal (with vectors and `:OF_FILE` edges); `store.StoreOversizedChunks` for the rest (no vector, `oversized=true`).
7. **Link** by source: `doclinker.Linker.LinkBySource(source, strict)`.

### Doc-to-code linker

For each chunk's content:
- **Strip** triple-fenced code blocks (so identifiers inside code samples don't leak as bare matches).
- **Extract** inline backtick spans → always-eligible candidates (no length / segment rule).
- **Tokenize** the rest. Each token is a candidate iff `len(token) ≥ 4` AND it has at least two case segments (PascalCase with internal lower→upper or upper-run→lower boundary, any camelCase, snake_case with two non-empty parts).
- **Look up** every candidate in a `name → []NodeID` map built from a single graph query: `MATCH (s:Searchable) WHERE s:Class OR s:Function OR s:Method OR s:Interface OR s:Enum OR s:Constructor RETURN ID(s), s.name`. (`:File` is excluded — files aren't valid `DOCUMENTS` targets.)
- **MERGE** `(:Chunk)-[:DOCUMENTS]->(target)` for every match. Idempotent — re-running asserts the same edges.

A `strict` flag flips the rule to backtick-only.

### Combined `analyze_repository` flow

```
analyze_repository(path, ignore)
  → resolve graph name = filepath.Base(path)
  → open per-repo Store
  → analyzer.Orchestrator.Analyze(path, ignore)         // pass 1 + pass 2
  → repoStore.EnsureIndex(512)                           // chunk vector index
  → ingestDocsToStore(repoStore, embedder, [path], ignore, false)
      → walks .md files, honoring `ignore` set
      → per source: chunk, AddFile, DeleteBySource, Embed, StoreChunks, StoreOversizedChunks, LinkBySource
  → doclinker.LinkRepo(repoStore, false)                 // catches edges to entities discovered after chunks
  → return summary
```

## Per-repo graph isolation

Each repository gets one FalkorDB graph, named `filepath.Base(repoPath)`. Resolution rules in `cmd/ingest/main.go::resolveRepoGraphName`:

1. Explicit `repo` arg, else
2. Innermost `.git` directory ancestor of the input path, else
3. Error.

`ingest_documents` and `link_docs` follow this strictly. `search_documents` adds a fallback to the global `H9S_GRAPH` (default `"h9s"`) when neither `repo` nor `path` is given — so cross-cutting queries against the global graph remain possible.

## Data model

### Node labels

| Label | Properties | Indices |
|---|---|---|
| `:File:Searchable` | `path`, `name`, `ext` | range on `(name, ext)`, fulltext on `Searchable.name` |
| `:Class` `:Function` `:Method` `:Constructor` `:Interface` `:Enum` (each `:Searchable`) | `name`, `path`, `src_start`, `src_end`, `doc` | fulltext on `Searchable.name` |
| `:Chunk` (not `:Searchable`) | `breadcrumb`, `content`, `source`, `line_start`, `line_end`, `vector` (or `oversized: true` when no vector) | vector (cosine, 512-dim) on `Chunk.vector` |

### Edges

| Edge | From → To | Source |
|---|---|---|
| `:DEFINES` | parent entity → child entity | tree-sitter pass 1 |
| `:CALLS` | function/method → callee | LSP pass 2 |
| `:EXTENDS` | class → base class; interface → base interface | LSP pass 2 |
| `:IMPLEMENTS` | class → interface | LSP pass 2 |
| `:RETURNS` | function/method → return-type entity | LSP pass 2 |
| `:PARAMETERS` | function/method → parameter-type entity | LSP pass 2 |
| `:OF_FILE` | chunk → file | doc ingestion |
| `:DOCUMENTS` | chunk → code entity (excluding `:File`) | doc linker |

### Within-file chunk order

Implicit: `c.line_start` is a total order within an `OF_FILE` cluster. Read in order with:

```cypher
MATCH (c:Chunk)-[:OF_FILE]->(f:File {name: $name})
RETURN c ORDER BY c.line_start
```

No explicit `NEXT` edge is maintained.

## Notes / vault graphs (planned)

The graphs above are *repository-shaped* — code paired with the documentation that describes it. A second graph shape, **vault graphs**, handles markdown-only collections (Obsidian vaults, personal note systems, long-form design-doc archives) where the meaningful cross-chunk relationships are wiki-links and tags rather than mentions of code symbols.

The existing labels (`:File:Searchable`, `:Chunk`) and edges (`:OF_FILE`) carry over unchanged. The additions are:

### New labels

| Label | Properties | Indices |
|---|---|---|
| `:Tag:Searchable` | `name` (string; the tag value with the leading `#` stripped — hierarchical tags like `project/foo/bar` stored verbatim as a single name, no parent/child edges) | fulltext on `Searchable.name` |

### New edges

| Edge | From → To | Source |
|---|---|---|
| `:LINKS_TO` | chunk → file | inline `[[note-name]]` wiki-links extracted from chunk content |
| `:TAGGED` | chunk → tag | inline `#tag` references in chunk content |
| `:TAGGED` | file → tag | YAML front-matter `tags: [...]` (file-wide metadata) |

The `:TAGGED` edge is dual-source on purpose. Inline tags are inherently chunk-scoped — `#performance` mentioned in one section says nothing about the other sections of the same file. Front-matter tags are file-wide metadata. The reader disambiguates by looking at the source-side label.

### Wiki-link resolution

`[[note-name]]` resolves to a `:File` whose stem (the basename without the `.md` extension) equals `note-name`. Variants:

- `[[note-name#heading]]` — heading suffix is dropped at resolution time; the link still targets the file. (A future refinement could resolve to a specific `:Chunk` whose breadcrumb ends with the heading.)
- `[[note-name|alias]]` — display alias is ignored; link target is `note-name`.
- `[[unresolved]]` — silently dropped if no matching `:File` exists in the same vault graph. No edge written, no error.

The `:Searchable` fulltext index on `name` covers the lookup.

### Tag normalization

Leading `#` is stripped. Hierarchical tags (`#project/foo/bar`) become a single `:Tag {name: "project/foo/bar"}` — flat, no parent edges. Front-matter tags from YAML (`tags: [foo, bar/baz]`) produce file-source `:TAGGED` edges; inline `#tag` references produce chunk-source ones.

### Differences from code+docs graphs

- **No `.git` walk-up.** A vault root is given explicitly (via the `repo` argument); the graph is named after that. There's no implicit ancestor lookup since vaults aren't generally git repositories.
- **No code entities, no `:DOCUMENTS` edges.** The doc-to-code linker is a no-op against a pure vault graph (no `:Searchable` code-entity nodes to match identifier mentions against). Wiki-links and tags are the cross-chunk relationships that matter.
- **Note count and turnover.** Vaults often hold orders of magnitude more files than repos and change continuously. Re-ingest performance becomes a real consideration; incremental ingestion (detect changed files, re-process only those) is more pressing here than for code repos.

### Co-existing with code+docs graphs

A repository can be both code project *and* note system: TypeScript files plus a `docs/` directory using `[[wiki-link]]` syntax for cross-references. The same FalkorDB graph holds everything — code nodes, doc chunks, wiki-link edges, tag nodes — with no schema collision. Wiki-link and tag extraction runs as an additive pass on every `.md` ingestion; files that don't use the syntax produce no extra edges.

### Future relationships (out of scope for the initial vault implementation)

- **`:SIMILAR_TO {score}`** — pre-computed chunk-to-chunk vector-similarity edges, written at ingest time. Partially redundant with on-demand KNN; deferred until a concrete use case demands stored similarity.
- **`:MENTIONS`** — fuzzy name match across notes (analogous to the doc→code linker, but with `:File.name` as the candidate set). Considered if wiki-link-only proves too sparse on poorly-linked vaults.
- **`:Chunk`-anchored wiki-link targets** — resolving `[[note#heading]]` to a specific chunk rather than the file. Requires breadcrumb-tail matching at link time.

## MCP tool surface

Defined in `cmd/ingest/main.go`. Inputs are Go structs with `jsonschema` tags; the SDK generates the JSON Schema clients see.

| Tool | Input shape | Behavior |
|---|---|---|
| `analyze_repository` | `{path, ignore}` | Two-pass code analysis + doc ingestion + full re-link. Per-repo graph. |
| `ingest_documents` | `{paths, repo?, strict?}` | Chunk + embed + store + per-source link. Resolves repo from explicit `repo` or `.git` walk-up of `paths[0]`. All paths must share the same repo. |
| `search_documents` | `{query, k?, repo?, path?}` | Embed query, KNN over `:Chunk.vector`. Targets per-repo graph if `repo` or `path` given; falls back to global `H9S_GRAPH`. |
| `link_docs` | `{repo?, path?, strict?}` | Re-runs `LinkRepo` over an existing per-repo graph. Idempotent. |

## Configuration

| Env var | Default | Used by |
|---|---|---|
| `FALKOR_ADDR` | `localhost:6381` | ingest server connecting to FalkorDB |
| `EMBEDDING_URL` | `http://localhost:8090` | ingest server calling llama-server |
| `H9S_GRAPH` | `h9s` | default/global graph name (search fallback) |
| `H9S_TRANSPORT` | `stdio` | `stdio` or `http` |
| `H9S_PORT` | `8091` | port when `H9S_TRANSPORT=http` |
| `H9S_LSP_CMD` | `typescript-language-server` | LSP binary path (override for non-PATH installs) |

## Caveats and known limits

- **Single-language analyzer.** Only TypeScript/TSX is parsed today. The `Analyzer` interface is built to accept more languages.
- **Single-process LSP.** Each `analyze_repository` call spawns a fresh `typescript-language-server`. Cold start is a few seconds; could be pooled but isn't.
- **No transactional `StoreChunks`.** Chunks are stored one at a time; partial failure leaves a partial file. Re-ingestion heals it.
- **Stale `:File` on rename.** Renaming a `.md` leaves the old `:File` node behind (with no chunks). Cleanup is a future concern that would also affect renamed code files.
- **Name-collision links.** A chunk that mentions `init` (or any name shared by multiple entities) emits one edge per matching entity. Documented behavior; ranking by file proximity is a future polish.
