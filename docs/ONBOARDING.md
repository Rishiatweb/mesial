# Onboarding

A field guide for the next engineer touching this repo — what mesial is, why it looks the way it does, and where it actually stands today. Companion to [`DESIGN.md`](DESIGN.md) (concepts), [`ARCHITECTURE.md`](ARCHITECTURE.md) (engineering view), [`RATIONALE.md`](RATIONALE.md) (decision log), and [`LIFECYCLE.md`](LIFECYCLE.md) (the roadmap). This document exists to connect those to each other and to the actual state of the code, git history, and open issues — read it first, then go deep on whichever of the others you need.

> **A note on which repo this describes.** Most of this doc, including the PR/issue numbers below, was written against upstream `mknw/mesial`'s state. This fork (`Rishiatweb/mesial`) has since diverged: its mirror of upstream PR #10 (fork PR #5) is **merged**, carrying two additional fixes upstream's version doesn't have — see `docs/IMPLEMENTATION.md` §5 for the fork-vs-upstream breakdown. Where this doc says "PR #10" or "unmerged," that's upstream's status, not necessarily this fork's.

## 1. Start here: the 60-second version

Mesial is a small Go service that reads a codebase and its markdown, and turns both into one queryable graph in FalkorDB — one graph per repository. It exposes that graph to AI coding agents over MCP. That's the whole job description.

Concretely, it does two mechanical things extremely well, and is in the middle of building a third:

- **Parses TypeScript** with tree-sitter and `typescript-language-server`, storing files, classes, functions, methods, interfaces, and enums as graph nodes, connected by `DEFINES`, `CALLS`, `EXTENDS`, `IMPLEMENTS`, `RETURNS`, and `PARAMETERS` edges.
- **Chunks and embeds markdown** by heading boundary, storing each chunk as a vector-searchable node, and links every chunk to the code entities it mentions by scanning for identifier names — so an agent can jump from a doc paragraph to the class it describes, or the other way around.
- **Is building a memory layer** — durable, evidence-backed facts an agent learns while working, distinct from the mechanical code/doc graph above. The schema and storage layer are done. The part that lets an agent actually reach it — MCP tools — is not, yet. Details in [§10](#10-the-memory-layer) and [§14](#14-state-of-the-world-honestly).

If you read nothing else: mesial does the boring, deterministic, mechanical work (parsing, chunking, embedding, name-matching) and leaves all interpretation — what a doc *means*, whether a connection is meaningful, what's worth remembering — to the agent using it. That boundary is the single most load-bearing design decision in the project ([§12](#12-decisions-and-their-rejected-alternatives), decision 10), and it explains almost every other choice once you see it.

## 2. The name is not decoration

The repository is called **mesial**, after the *mesial temporal lobe* — the brain region that houses the hippocampus. The hippocampus is specifically associated with **declarative memory** (what you know, what you've seen) and **spatial navigation** (place cells mapping your environment) — not a loose metaphor, the whole architecture is organized around those two faculties plus a third the README adds: **knowledge** (semantic search over docs).

Naming quirk worth knowing before it confuses you: the *binaries and Go module* are named `h9s` — short for hi(ppocampu)s, the way i18n abbreviates internationalization. `go.mod` declares `module github.com/mknw/h9s`, every internal package imports as `github.com/mknw/h9s/internal/...`, and the CLI is `h9s-cli`. The GitHub repo and public docs say **mesial**. Same project, two names — `h9s` is the "in the building" nickname.

## 3. The mental model

Mesial recognizes three graph *shapes*, all stored as FalkorDB graphs, one graph per "thing" being described. Every graph carries a singleton `(:GraphMeta {kind, strict})` node declaring which vocabulary it speaks:

| Kind | Status | Subject | Core labels |
|---|---|---|---|
| `code` | shipped | One repository's code + docs | `:File :Class :Function :Method :Constructor :Interface :Enum :Chunk` |
| `notes` | planned | A markdown-only vault (Obsidian-style) | `:File :Chunk :Tag`, wiki-links instead of code mentions |
| `memory` | built, unreachable | Cross-cutting facts an agent has learned | `:Fact :Observation :Protocol` |

The deeper idea threading through all three is a split between a **perceptual layer** (raw text, vector-embedded, retrieved by similarity) and a **conceptual layer** (structured nodes, retrieved by name and traversal). The conceptual layer never carries its own embeddings — it borrows semantic locality from the perceptual layer it's anchored to:

```
:Fact  ←[:EVIDENCE_FOR]─  :Observation  ─[:MOTIVATES]→  :Chunk  ─[:DOCUMENTS]→  :CodeEntity
 (conceptual)              (perceptual)                (perceptual)              (conceptual)
```

**Why this matters:** asking "what do I know about deployment?" becomes one vector search over embedded nodes (chunks, observations), then a cheap structural hop into the conceptual nodes they touch. Two hops, one embedding round-trip, no duplicated representation. This is the pattern every interesting query in mesial reduces to — `LIFECYCLE.md` calls it *Pattern 6: combined semantic + logical*, and flags it as the thing that actually justifies the graph-database architecture over a plain vector store.

## 4. Running it: topology & boot sequence

Four processes, three of which you run yourself in dev:

```
Claude Code / any MCP client
        │  streamable HTTP :8812
        ▼
 docker mcp-gateway  ──reads──  configs/custom-catalog.yaml + configs/mcp-config.yaml
        │  proxies tool calls
        ▼
 cmd/ingest (Go, :8091)  ──POST /v1/embeddings──▶  llama-server :8090 (Qwen3-Embedding-0.6B)
        │
        ▼
 FalkorDB :6381  (one graph per repo, named graphs via db.SelectGraph)
```

| Port | Service | Where it runs | Started by |
|---|---|---|---|
| `6381` | FalkorDB | Docker | `make up` |
| `8090` | llama-server (embeddings) | host | `make embed` |
| `8091` | ingest server (MCP) | host (dev) or Docker (prod) | `make ingest` |
| `8812` | MCP gateway | Docker | `make up` |

First boot:

```bash
nix develop                                  # or let direnv handle it via .envrc
cp configs/template.mcp-config.yaml configs/mcp-config.yaml
# edit configs/mcp-config.yaml — set your GitHub PAT

make up      # terminal 1 — FalkorDB + gateway, backgrounded
make embed   # terminal 2 — llama-server, foreground, needs models/Qwen3-Embedding-0.6B-Q8_0.gguf
make ingest  # terminal 3 — the Go MCP server, foreground, hot-reloads via `go run`

claude mcp add --transport http --scope user h9s-memory http://localhost:8812/mcp
```

The Docker `ingest` container (defined in the compose file) is the "always-on" production path; it binds the same port `8091` as the dev host process, so during development leave it stopped and use `make ingest` instead.

## 5. The data model, node by node

Every repository lands in its own FalkorDB graph, named `filepath.Base(repoPath)` by default.

**Nodes**

| Label | Key properties | Written by |
|---|---|---|
| `:File:Searchable` | `path, name, ext` | code analysis pass 1 & doc ingestion |
| `:Class :Function :Method :Constructor :Interface :Enum` (each also `:Searchable`) | `name, path, src_start, src_end, doc` | tree-sitter, pass 1 |
| `:Chunk` (not `:Searchable`) | `breadcrumb, content, source, line_start, line_end, vector` (or `oversized:true`), `last_distilled_at` | doc ingestion |

**Why `:Chunk` skips `:Searchable`:** `:Searchable` gates the fulltext name index. Chunks aren't named entities retrieved by name — they're retrieved by vector similarity. Tagging them `:Searchable` would just dump breadcrumb-shaped strings into name-search results as noise.

**Edges**

| Edge | From → To | Source |
|---|---|---|
| `:DEFINES` | parent entity → child entity | tree-sitter, pass 1 |
| `:CALLS` | function/method → callee | LSP, pass 2 |
| `:EXTENDS` | class→base class, interface→base interface | LSP, pass 2 |
| `:IMPLEMENTS` | class → interface | LSP, pass 2 |
| `:RETURNS` | function/method → return-type entity | LSP, pass 2 |
| `:PARAMETERS` | function/method → parameter-type entity | LSP, pass 2 |
| `:OF_FILE` | chunk → file | doc ingestion |
| `:DOCUMENTS` | chunk → code entity (never `:File`) | doc linker |

No `:NEXT` edge chains chunks in reading order — `line_start` is already a total order within a file:

```cypher
MATCH (c:Chunk)-[:OF_FILE]->(f:File {name: $name})
RETURN c ORDER BY c.line_start
```

A stored edge would have to be torn down and rebuilt on every re-ingest for information already sitting in a property.

## 6. Pipeline I: reading code

`analyze_repository` runs a deliberate two-pass split.

**Pass 1 — tree-sitter (structure).** Walks every `.ts`/`.tsx` file (skipping `node_modules`, `.git`, `dist`, etc. via `DefaultIgnore`), parses with tree-sitter's TypeScript/TSX grammars, and recognizes six AST node kinds as entities: `function_declaration`, `class_declaration`, `method_definition` (a method named `constructor` gets relabeled `Constructor`), `interface_declaration`, `type_alias_declaration` (filed as `Interface`), and `enum_declaration` — plus `export const foo = () => {...}` arrow functions fished out of their `lexical_declaration` wrapper and treated as `Function`. Each entity is MERGEd as a node, connected to its parent via `:DEFINES`, and every unresolved reference inside it (call, `extends`, return type, parameter type) is recorded as a `Symbol{Key, Path, Row, Col}` for pass 2.

**Pass 2 — LSP (meaning).** Spawns `typescript-language-server --stdio`, opens every analyzed file over LSP, and for each unresolved `Symbol` sends `textDocument/definition`. The result resolves to a specific entity via `LookupEntityByPosition` — finds the *innermost* entity whose `src_start..src_end` range contains the target line (ordered by range size ascending, so a method wins over its enclosing class). Becomes a `:CALLS`/`:EXTENDS`/`:IMPLEMENTS`/`:RETURNS`/`:PARAMETERS` edge.

**Why split it at all:** tree-sitter is fast and deterministic but knows nothing about TypeScript's actual resolution rules — type aliases, re-exports, `tsconfig` path mapping. The LSP already has those rules; reimplementing them would be its own project. Splitting also buys graceful degradation: if the LSP subprocess fails to start, pass 1's structural graph is still returned — you get a skeleton with no edges instead of nothing.

Cost of this design: a fresh `typescript-language-server` process spins up on every `analyze_repository` call — a few seconds of cold start each time. Nothing pools it yet.

## 7. Pipeline II: reading docs

Per source markdown file, in order (`internal/pipeline.IngestDocs`):

1. **Chunk** by heading boundary. `internal/chunking` scans line by line with one regex (`^(#{1,6})\s+(.+)$`), tracks the current heading text at each of 6 levels, flushes a chunk on each new heading or EOF. Each chunk carries a `breadcrumb` — the joined chain of active headings, e.g. `Architecture > Data model > Node labels`.
2. **MERGE the `:File` node**, then **`DETACH DELETE` every prior chunk** for that source path. This is how re-ingest picks up edits and renames — and it's also exactly what breaks anchor stability (see [§15](#15-the-roadmap-online-first-memory)).
3. **Partition**: content ≤ 6000 characters gets embedded; longer content is flagged oversized.
4. **Embed** the normal partition — `breadcrumb + "\n" + content`, batched 32 at a time — via llama-server, truncated from Qwen3's native 1024 dimensions to 512 via Matryoshka truncation (slicing the vector; Matryoshka-trained embeddings keep a prefix meaningful).
5. **Store.** Normal chunks get vector + `:OF_FILE`; oversized chunks get the same shape minus `vector`, plus `oversized:true`. KNN ignores them by construction; graph traversal and the linker see them like any chunk.
6. **Link.** `doclinker.LinkBySource` scans this file's fresh chunks against the full code-entity name table ([§8](#8-the-linker-mesials-cleverest-160-lines)).

**Why oversized chunks are stored, not skipped** — three reasons converge: *embedding quality* (a 10,000-char section crammed into one 512-dim vector becomes "vaguely about everything," worse for KNN than skipping); *server limits* (llama-server has a context ceiling — hit for real against a 304-line README example); *linker coverage* (the linker reads `content`, not `vector` — skipping storage would silently drop every `DOCUMENTS` edge that section should produce). The threshold is one constant, `OversizedChunkChars = 6000` in `internal/pipeline/pipeline.go`.

## 8. The linker: mesial's cleverest 160 lines

`internal/doclinker` answers one question: does this chunk of prose mention that code entity? Two paths to a link, resolved per token:

| Form | Rule | Example |
|---|---|---|
| Backtick-fenced | Always links if the name matches a known entity — no length/shape requirement | `` `User` `` links, `` `init` `` links |
| Bare (mid-prose) | Must be **≥ 4 characters** AND have **≥ 2 case segments** | `EventViewImpl` ✓, `getPullRequest` ✓, `get_pull_request` ✓, `XMLParser` ✓ — but `User` ✗, `Configuration` ✗, `init` ✗, `JSON` ✗ |

"Two case segments" means an internal camelCase/PascalCase boundary (lower→upper transition, or an upper-run followed by lowercase — the clause that lets `XMLParser` qualify despite starting with a 3-letter acronym run), or snake_case with ≥2 non-empty parts. Triple-fenced code blocks are stripped before any of this runs, so a code sample's local variable names don't leak into the bare-token pass.

**Why this exact threshold:** sampled real docs from another internal repo (`kg-agent`) and found backtick-only linking would miss ~80% of real references. The 2-segment rule keeps that permissiveness from becoming noise — it excludes single-word common nouns (`User`, `Item`, `Configuration`) that constantly collide with real class names, without needing a stopword list. It's shape-based, not vocabulary-based, so it needs no maintenance as the codebase grows.

`:DOCUMENTS` targets code entities only — `:File` is explicitly excluded from the linker's candidate set. A doc saying "see `chunking.go`" is a navigational pointer, not a semantic description; including files would inflate edge count with links that don't mean "this chunk is *about* that thing." (The structural chunk→file relationship is `:OF_FILE`, a different edge with a different meaning.)

Real assertions from `internal/doclinker/linker_test.go`:

```go
isLinkableBare("EventViewImpl")  // true  — PascalCase, internal boundary
isLinkableBare("XMLParser")      // true  — upper-run + lowercase
isLinkableBare("MAX_RETRY")      // true  — 2 non-empty snake segments
isLinkableBare("User")           // false — single segment, common noun
isLinkableBare("JSON")           // false — all-caps acronym, no boundary
isLinkableBare("AB")             // false — below length floor
isLinkableBare("__init__")       // false — zero non-empty segments
```

## 9. The MCP surface today

Four tools, all defined in `cmd/ingest/main.go`, all thin adapters over `internal/pipeline` — the MCP server and `h9s-cli` (dev harness) call the exact same functions and can never drift in behavior from each other.

| Tool | Input | Does |
|---|---|---|
| `analyze_repository` | `{path, ignore?, repo?}` | Full two-pass code analysis + doc ingestion + full re-link, one graph per repo |
| `ingest_documents` | `{paths[], repo?, strict?}` | Chunk + embed + store + per-source link; all paths must share one `.git` ancestor |
| `search_documents` | `{query, k?, repo?, path?}` | KNN over `:Chunk.vector`; falls back to the global graph if neither `repo` nor `path` given |
| `link_docs` | `{repo?, path?, strict?}` | Re-runs the linker over an existing graph; idempotent |

**Repo resolution — the same chain, three places.** `ingest_documents` and `link_docs` resolve the target graph: explicit `repo` argument wins; otherwise walk up from the input path looking for a `.git` directory and use its basename; otherwise **error** — no silent fallback. `search_documents` runs the identical chain but falls back to the global `H9S_GRAPH` (default `"h9s"`) instead of erroring.

**Why search gets a fallback and writes don't:** silently writing a repo's docs into the global graph because an agent forgot to pass a path would be a nasty bug to track down — cross-contaminated data with no error pointing at the cause. Erroring forces intent on writes. Search is asymmetric on purpose: cross-cutting queries against the global graph are legitimately useful, and "searched the wrong graph" just means empty results — annoying, not corrupting.

## 10. The memory layer

The part of mesial that's mid-construction — worth walking through both what's there and what's missing.

**Three node types, three epistemological roles:**

- **`:Observation`** — a sentence, rarely two. Free-form, embedded, cheap to write. The episodic layer — "what happened."
- **`:Fact`** — a `(subject, predicate, object)` triplet. Durable, slow-changing, *not* embedded. The semantic layer — "what's true."
- **`:Protocol`** — procedural, "how to." Reserved label, schema TBD (issue #6).

The hippocampus framing is literal: observations consolidate into facts via `:EVIDENCE_FOR`, echoing how episodic memory consolidates into semantic memory during sleep. **No `:Fact` enters the graph without at least one backing `:Observation`** — enforced in code, not just convention: `pipeline.CreateFactFromObservation` is the *only* function that creates a fact, and it MERGEs the triplet and writes the required `:EVIDENCE_FOR` edge in the same call. No separate "create a bare fact" path exists anywhere in the CLI or pipeline.

**Why `:MOTIVATES` points from observation to chunk, not the other way** — a subtle call worth internalizing. A chunk doesn't earn trust by existing in the docs; it earns trust by being *motivated* by an observation the agent made or re-affirmed. A chunk with zero incoming `:MOTIVATES` edges is unverified material — nobody has engaged with it yet. When an agent reads such a chunk, its job is either to write an observation about it (anchoring via this edge — read-time verification) or flag it for removal. Documentation isn't authoritative because it's written down; it's authoritative because someone checked it and said so.

**What's actually implemented:** full CRUD, in Go, tested. `internal/falkorstore/memorystore.go` (544 lines) has the FalkorDB layer — `EnsureGraphMeta`, `AddObservation`, `AddFact` (MERGE on triplet), `LinkEvidenceFor`, `LinkMotivates`, `SearchObservations` (KNN), `SearchFacts` (structural), plus existence-checked edge writers that fail loudly instead of silently no-op'ing on a bad ID. `internal/pipeline/memory.go` wraps it into agent-shaped operations — notably `AddObservation`, which embeds the text, KNNs against *existing* observations before inserting the new one (so it never surfaces itself as its own neighbor), then collects and returns the facts those neighbors back.

Try it today — CLI-only:

```bash
h9s-cli memory init --repo my-project
h9s-cli memory add-obs --repo my-project --text "auth middleware treats expiry as strict less-than"
h9s-cli memory create-fact --repo my-project --obs 12 \
  --subject "auth middleware" --predicate "requires" --object "token not expired"
h9s-cli memory search-obs --repo my-project "token expiry"
h9s-cli memory evidence --repo my-project --fact 3
```

**What's not there yet — verified by reading the code, not assumed:** `cmd/ingest/main.go` registers exactly four MCP tools, and none touch the memory layer. Everything above — `add-obs`, `create-fact`, `search-obs`, all ten `memory` subcommands — is reachable only through direct CLI invocation of `h9s-cli`. An agent talking to mesial over MCP today cannot write or read a single observation or fact. The storage and pipeline layers are done; the door to reach them from an agent hasn't been built. This is the single most concrete, scoped, "pick this up and go" gap in the whole project — see [§16](#16-where-to-actually-start).

## 11. Package map

| Package | Responsibility |
|---|---|
| `internal/chunking` | Heading-boundary markdown splitter. One file, one regex, stdlib only. |
| `internal/embedding` | llama-server `/v1/embeddings` client. Batches of 32, Matryoshka truncation. |
| `internal/falkorstore` | `store.go` (chunks), `codegraph.go` (code entities), `memorystore.go` (Fact/Observation/GraphMeta). All FalkorDB Cypher lives here — nowhere else touches the DB driver directly. |
| `internal/analyzer` | Language-agnostic `Analyzer` interface + `typescript.go` (tree-sitter) + `orchestrator.go` (two-pass driver). |
| `internal/lspclient` | `typescript-language-server` subprocess wrapper. JSON-RPC framing, `initialize`, `didOpen`, `definition`, shutdown. |
| `internal/doclinker` | Chunk-to-entity identifier scanner. Pure logic over the store. |
| `internal/pipeline` | The composition layer. Everything `cmd/ingest` and `cmd/h9s-cli` call funnels through here — why the two front-ends can't drift. |
| `cmd/ingest` | MCP server entrypoint. stdio or streamable-HTTP transport, four tools registered. |
| `cmd/h9s-cli` | Direct dev harness — same pipeline, no MCP round-trip. Fastest way to test a change. |
| `cmd/chunker` | Standalone chunker CLI. Outputs JSON — useful for eyeballing how a doc will chunk before ingesting it for real. |

## 12. Decisions and their rejected alternatives

Condensed from `docs/RATIONALE.md`, which is worth reading in full — every entry follows "what we chose, why, what we rejected," and it's the fastest way to avoid re-litigating a settled argument.

| Decision | Rejected |
|---|---|
| One database (FalkorDB) for graph + vectors | Neo4j+Qdrant (two ops surfaces); Redis Stack/RediSearch (crashes on arm64 with vector indices) |
| One FalkorDB graph per repo | Single global graph with a `repo` property (heavier filters, messy cleanup); one FalkorDB instance per repo (operational overkill) |
| `:Chunk` reused for docs, no `:Searchable` | A distinct `:DocChunk` label — no current benefit, adds a moving part |
| No `:NEXT` edge between chunks | Stored explicitly — `line_start` already gives a free total order |
| `:DOCUMENTS` never targets `:File` | Allowing it — higher recall, much lower precision, cheapens the edge's meaning |
| Linker: backtick OR (len≥4 AND 2+ case segments) | Backtick-only (misses ~80% of real refs); allow bare acronyms (too many false positives); fulltext-index lookup (unpredictable tokenization) |
| Two-pass analysis: tree-sitter then LSP | All tree-sitter (can't follow imports reliably); all LSP (no schema control over what's an entity); TS Compiler API (locks into Node tooling) |
| Oversized chunks stored, not embedded | Skip silently (loses linker coverage); truncate-and-embed-head (silent misrepresentation) |
| Repo resolution: explicit → `.git` walk-up → error | Auto-detect from process CWD (rarely correlates with the task); always require explicit repo (annoying to remember every call) |
| Mesial is mechanical; the agent interprets | Putting interpretation inside mesial — makes the layer slow, stateful, and "just another small AI service the next general agent eats" |

## 13. What mesial deliberately is not

- **Not an interpretive linker.** The doc-linker connects on surface identifier match, never on meaning. Connecting "the auth flow" to `AuthService` in prose that never says the class name is the agent's job, not mesial's.
- **Not cross-repo.** Every repository is an island graph. A federated query layer across repos isn't built, and there's no plan to bolt one on without real demand driving the shape.
- **Not (yet) a general-memory store an agent can reach.** See [§10](#10-the-memory-layer) — the schema exists, the door doesn't.
- **Not a code-search engine.** Code entities are queried structurally, by name and traversal — never embedded. "Find code semantically similar to X" is a different kind of problem mesial hasn't taken on.

## 14. State of the world, honestly

Pulled directly from git history, open issues, and the open pull request — not from what the docs claim, which turns out to matter (see the callout at the end of this section).

**Merged and live.** PRs #2 through #5: documents layer + linker, the `h9s-cli`/`internal/pipeline` split, ROADMAP + vault-graph schema docs, and the memory-layer foundation. Four MCP tools running today, code + doc graph fully working end to end.

**Open PR #10 (upstream) — `docs/LIFECYCLE.md`.** 684 lines. A design document, not code — the online-first operating manual for how mesial should behave over time (full breakdown in [§15](#15-the-roadmap-online-first-memory)). Upstream's checklist is blocked on one unchecked box: *"Reviewer to check the rewrite addresses the PR #7 review feedback adequately."* Nothing else is gating it there. (PR #7 was accidentally closed when its base branch got deleted post-merge — GitHub auto-closes instead of retargeting — so #10 is #7's content, replayed on the same branch, with the review thread preserved on the original.)

**This fork has already moved past that.** The fork's mirror (PR #5, `Rishiatweb/mesial`) went through an actual review pass — it surfaced two real issues (a tiering overstatement in the closing summary, and a missing `last_distilled_at` carry-forward rule in `reanchor`), both fixed, and the PR was merged into this fork's `main`. `docs/LIFECYCLE.md` on this repo is that corrected version, not upstream's. If those fixes should go back upstream, that's a separate decision — it's not this fork's repo to push to.

**Three open issues, all explicitly scoped "design only":**

| Issue | What | Why it's not blocking anything today |
|---|---|---|
| #6 | `:Protocol` schema (procedural memory, P-Plan-inspired) | Explicitly deferred until a concrete protocol use case exists to drive the shape |
| #8 | Anchor stability & re-anchoring spec | Design-only per the issue body; flagged as the **load-bearing risk** for the whole memory layer (see next section) |
| #9 | `:Test`/`:Failure` ingestion | Design pass only — schema and ingestion strategy still open questions |

**The real gap.** None of the three issues above are what's stopping a working engineer today. The memory layer having zero MCP exposure (§10) is. It's not filed as an issue anywhere — you only find it by reading `cmd/ingest/main.go` and counting tool registrations.

**Physician, heal thyself.** `CLAUDE.md` — the file meant to orient an agent working in this repo — still says *"No tests or CI configured yet."* That was true when it was written; it's not true now: `internal/doclinker/linker_test.go` and `internal/falkorstore/memorystore_test.go` both exist, and the README already documents `go test ./...` as covering doclinker. Mesial's entire purpose is catching exactly this kind of drift — a doc that stops matching the code it describes, silently, with nothing flagging it. `CLAUDE.md` going stale relative to its own README is a small, low-stakes, genuinely fitting instance of the problem LIFECYCLE.md's `hygiene_queue` and `stale_doc_candidates` metric (§15) are designed to catch. Also a fine two-minute first PR if you want one that requires zero ramp-up.

## 15. The roadmap: online-first memory

This section summarizes `docs/LIFECYCLE.md` (merged on this fork via PR #5, corrected from upstream's PR #10) for a first read rather than as a spec — treat it as "here's the plan," not "here's what runs today." The primitives it describes (`surface`, `impact`, `reanchor`, etc.) are still unimplemented regardless of which repo's copy of the doc you're reading — merging the doc didn't build the tools.

**The seven-loop spine:**

```
1. Anchor   Text/code becomes graph nodes with stable identity that survive churn.
2. Surface  A task/query/file produces a useful context subgraph.
3. Act      The agent edits, tests, investigates, decides.
4. Capture  Important discoveries from the act become observations.
5. Distill  Repeated or high-confidence observations become facts (or protocols).
6. Verify   Facts, anchors, evidence, and coverage are checked against current code.
7. Repair   Broken anchors, stale docs, stale facts, coverage gaps → queues, not silent decay.
```

Earlier drafts under-weighted step 7. A memory system that can't repair itself becomes archaeological — verification finds nothing wrong only because the thing it's verifying against has already drifted out from under it.

**Five invariants:**

1. **Durable claims are evidence-backed** — no `:Fact` without ≥1 `:EVIDENCE_FOR`. Already enforced in code (§10).
2. **Anchors are repairable** — chunk regeneration must not silently disconnect observations/facts. *Not yet true* — see below.
3. **Online use is the primary write path** — memory captured during real work beats a one-time bootstrap interview.
4. **Verification is scoped** — logical, structural, evidence, anchor, and coverage checks are separate signals, not lumped into one.
5. **Repair is first-class** — decay becomes an explicit queue, never silence.

**The anchor-stability problem, made concrete.** Look at what re-ingesting a doc file actually does, in `internal/pipeline/pipeline.go`:

```go
if _, err := store.DeleteBySource(ctx, source); err != nil { ... }   // DETACH DELETE — old :Chunk nodes gone
...
n, err := store.StoreChunks(ctx, normal, vectors, fileIDs)           // CREATE — brand new nodes, brand new IDs
```

FalkorDB node IDs aren't stable across a delete-and-recreate. So: an agent reads a chunk, writes an observation, links it via `(:Observation)-[:MOTIVATES]->(:Chunk)`. Someone edits that doc — even a trivial rewording. `analyze_repository` (or `ingest_documents`) runs again. The old chunk node is deleted; a new one is created with a new ID. The `:MOTIVATES` edge pointed at the old ID — it's gone. The observation is still in the graph, looking healthy, **silently disconnected from the doc it was grounded in**. Nothing errors, nothing logs it. `verify` would report the fact as fine, because `verify` only checks what it's pointed at, and the ground underneath already moved.

Issue #8's fix, in sketch: give chunks a stable identity beyond their DB node ID — `content_hash` (whitespace-normalized) plus `breadcrumb_hash`, composited with `source_path`. On re-ingest, a `reanchor` operation matches new chunks to old ones by that composite key first, falls back to content-hash-only (catches renamed sections), then to vector similarity over old/new embeddings plus shared `:DOCUMENTS` targets (lowest confidence), and replays every `:MOTIVATES` edge onto the new node. Anything unmappable surfaces as orphaned for review rather than silently dropping.

**Six agent moments → six primitives:**

| Trigger | Primitive |
|---|---|
| Task starts | `surface(task)` |
| File opened | `surface(file_path)` |
| Symbol edited | `impact(symbol)` |
| Unexpected behavior found / convention learned | `add_observation(text)` |
| Before commit / PR | `verify_changed_entities` + `verify_docs_for_changed_entities` |
| After merge / on schedule | `reanchor(changed_sources)` + `hygiene_queue(kind)` |

If an agent skill follows this table mechanically, every commit cycle passes through the full spine without the agent needing to remember which call applies when. The actual product thesis buried in the design doc: **"a coding agent starts a task, gets the right context, changes code, updates the graph, and leaves the next agent smarter than it was."** Everything else in LIFECYCLE.md serves that one sentence.

**Priority tiers:**

| Tier | Contents | Current state |
|---|---|---|
| 1 — must-have | `surface` (MVP shape), stable identity + `reanchor`, `add_observation` as MCP, `impact` | not built — the real next milestone |
| 2 — force multipliers | `hygiene_queue`, bootstrap interview as one path among several, diff-driven population, `verify` (anchor + structural) | not built |
| 3 — advanced reasoning | Fact generation from observations, `verify` (logical + evidence), `:Protocol` (#6), `:Test`/`:Failure` (#9) | not built, design pending |
| 4 — late / specialized | Doc generation, migration planning, forward-chaining inference | deferred on purpose |

## 16. Where to actually start

Ranked by how much you can get done before hitting a design decision that isn't yours to make yet:

1. **Wire the memory layer into `cmd/ingest`.** Everything the MCP tools need already exists in `internal/pipeline/memory.go` — `AddObservation`, `CreateFactFromObservation`, `LinkObservationEvidence`, `SearchObservations`, `SearchFacts`. This is copying the pattern the four existing tools already use (thin `mcp.AddTool` adapter → pipeline call → format result), not inventing anything. Highest leverage-to-effort ratio in the repo right now, and it's Tier 1 work per LIFECYCLE.md's own prioritization. Still true on this fork even though LIFECYCLE.md itself is now merged here — merging the doc didn't wire the tools.
2. **~~Review and merge PR #10~~ — done on this fork.** Fork PR #5 went through review, picked up two real fixes, and merged. Upstream's PR #10 is still open with the original (uncorrected) content, in case that's relevant to you.
3. **Anchor stability (issue #8), once someone actually needs it.** The design sketch above is solid, but building `reanchor` before there's memory data worth protecting is building insurance for a house that doesn't exist yet. Do #1 first — once agents are writing real observations through MCP, the churn problem stops being theoretical and this jumps to the top.
4. **Leave #6 and #9 alone for now.** Both issues say so themselves — "design only," deferred until a concrete use case exists. Implementing ahead of that isn't initiative, it's skipping a step the previous author deliberately built into the process.

One more thing worth noticing about how this repo was built: every merged PR is scoped to one coherent slice — "documents layer," "dev harness," "memory foundation" — with a design doc landing alongside or ahead of the code it describes. The commit history reads like someone who writes the rationale down *before* getting attached to an implementation. If you're picking up where this leaves off, matching that rhythm (small design note → review → focused PR) will fit the codebase a lot better than a big surprise diff, even a good one.

## 17. Glossary

| Term | Meaning |
|---|---|
| `h9s` | The project's internal nickname — Go module and binary name. Same project as "mesial." |
| Breadcrumb | The joined chain of active markdown headings above a chunk, e.g. `Architecture > Data model`. |
| Matryoshka truncation | Slicing an embedding vector's leading dimensions (1024 → 512) and keeping it meaningful, because the model was trained for exactly that. |
| `strict:false` | Default `:GraphMeta` setting — graphs may accumulate off-vocabulary nodes (a code graph holding a stray `:Fact`) without rejection. |
| Oversized chunk | >6000 chars — stored with full content, no vector, still scanned by the linker. |
| No-orphan-facts | Every `:Fact` must have ≥1 `:EVIDENCE_FOR` edge from an `:Observation`, enforced by there being no code path that creates a bare fact. |
| Anchor stability | Whether an edge pointing at a chunk/entity still resolves correctly after that chunk/entity is re-ingested and re-created with a new node ID. Currently: it doesn't (§15). |
| Backbone primitive | One of six planned MCP operations (`surface`, `propose_then_confirm`, `impact`, `verify`, `hygiene_queue`, `reanchor`) every agent-facing scenario composes from. |

---

*Originally compiled from upstream `mknw/mesial` at `main @ fff0d40`, PR #10 (`feat/memory-mcp`), and issues #6/#8/#9 — every claim traces to a file, a commit, or an issue body, not to inference about intent. Since then this fork merged its own mirror (PR #5, with two fixes upstream doesn't have) — see the note at the top of this document and `docs/IMPLEMENTATION.md` §5 for what's changed.*
