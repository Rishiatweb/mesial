# Implementation

The build log and forward-looking companion to [`ONBOARDING.md`](ONBOARDING.md). Read that one first for narrative context — what mesial is, why it's shaped this way. This one answers three separate questions: what's actually built, what's next to build, and what's still an open research question that shouldn't be implemented until it's answered. It cross-references [`ARCHITECTURE.md`](ARCHITECTURE.md) rather than restating it — the data model, MCP tool table, and config table live there and only there.

## 1. Done — implementation inventory

Package-by-package, at the function level, with what's wired to MCP versus CLI-only, and what has test coverage.

| Package | Shipped | Wired to MCP? | Tests |
|---|---|---|---|
| `internal/chunking` | `ChunkFile`, `FindMarkdown`, `BuildBreadcrumb` — heading-boundary splitter | via `ingest_documents`/`analyze_repository` | none |
| `internal/embedding` | `Client.Embed` — batched llama-server client, Matryoshka truncation | via `ingest_documents`/`search_documents`/`analyze_repository` | none |
| `internal/falkorstore` (`store.go`, `codegraph.go`) | Chunk CRUD, KNN search, code-entity CRUD, `LookupEntityByPosition` | yes, all four tools | none |
| `internal/falkorstore` (`memorystore.go`) | `EnsureGraphMeta`, `AddObservation`, `AddFact`, `LinkEvidenceFor`, `LinkMotivates`, `SearchObservations`, `SearchFacts`, evidence/existence checks — 544 lines, full CRUD | **no** — no MCP tool calls into this file at all | `memorystore_test.go` |
| `internal/analyzer` | `Analyzer` interface, `TypeScriptAnalyzer` (tree-sitter), `Orchestrator` (two-pass driver) | via `analyze_repository` | none |
| `internal/lspclient` | Subprocess wrapper — `initialize`, `didOpen`, `definition`, shutdown | via `analyze_repository` (pass 2) | none |
| `internal/doclinker` | `LinkRepo`, `LinkBySource`, backtick/bare-token scanner | via `ingest_documents`/`link_docs`/`analyze_repository` | `linker_test.go` — the most thoroughly tested package in the repo |
| `internal/pipeline` | `IngestDocs`, `AnalyzeRepository`, `LinkRepo`, repo-resolution helpers, **plus all memory-layer wrappers** (`AddObservation`, `CreateFactFromObservation`, `LinkObservationEvidence`, `SearchObservations`, `SearchFacts`) | code/doc functions yes; memory functions **no** (see above) | none |
| `cmd/ingest` | MCP server, 4 tools: `analyze_repository`, `ingest_documents`, `search_documents`, `link_docs` | — | none (integration-shaped, would need a live FalkorDB + llama-server) |
| `cmd/h9s-cli` | Dev harness — `analyze`, `ingest`, `search`, `link`, `memory` (10 subcommands) | n/a, bypasses MCP entirely | none |
| `cmd/chunker` | Standalone chunk-and-print-JSON CLI | n/a | none |

**The one gap that matters most:** the memory layer (`memorystore.go` + the memory functions in `pipeline`) is fully implemented and is the only package with meaningful test coverage on the storage side, but has zero MCP surface. `cmd/ingest/main.go` registers exactly 4 `mcp.AddTool` calls and none of them touch `AddObservation`, `CreateFactFromObservation`, or `SearchFacts`. An agent talking to mesial over MCP today cannot read or write a single fact or observation — only `h9s-cli memory ...` can.

## 2. Pathway: Implementation

Ordered by dependency, not just priority — later items build on earlier ones.

1. **Expose the memory layer via MCP.** Add 5–6 `mcp.AddTool` registrations to `cmd/ingest/main.go` mirroring the existing four: `add_observation`, `create_fact`, `link_evidence`, `search_observations`, `search_facts`. Every function these tools would call already exists in `internal/pipeline/memory.go` — this is wiring, not new logic. Zero blockers, highest leverage-to-effort ratio in the whole roadmap. This is LIFECYCLE.md's Tier 1 item "`add_observation` runtime capture," and it's the one Tier 1 item with literally nothing else it depends on.
2. **`surface(query|path, depth) → context_subgraph`.** The most-called primitive per LIFECYCLE.md — semantic search over chunks/observations, then graph traversal to assemble a context bundle. MVP response shape is already spec'd (versioned JSON: `chunks[]`, `entities[]`, `observations[]`, `confidence{}`) — see LIFECYCLE.md §"surface response shape."
3. **`impact(entity, kinds[]) → dependent_set`.** Edge-traversal-as-impact — reduces "what depends on X?" to a graph query over existing `:CALLS`/`:DOCUMENTS`/`:EVIDENCE_FOR` edges. No new storage needed, purely a read-side MCP tool over data already being written.
4. **Stable identity + `reanchor`.** Blocked on issue #8's design landing first (see §4 below) — this is where "Implementation" and "Research" pathways meet. Once the identity scheme (`content_hash` + `breadcrumb_hash`) is decided, implementation is: add the two hash properties to `:Chunk` on write, then build the match-fallback-orphan algorithm LIFECYCLE.md sketches.
5. **`hygiene_queue(kind) → items[]`.** Time-driven maintenance queues — depends on the health metrics in §3 below existing to query against.
6. **`verify` (5 scopes: logical, structural, evidence, anchor, coverage).** Anchor and structural scopes are buildable now (pure Cypher against existing edges); evidence and logical scopes want an LLM-assisted check and are naturally later.
7. **Fact-generation flow, `:Protocol` ingestion, `:Test`/`:Failure` ingestion.** Tier 3 — the last two are explicitly gated on issues #6 and #9 being ratified first (§4).

## 3. Pathway: Productionizing

From `ROADMAP.md`'s "Other near-term" and `ARCHITECTURE.md`'s "Caveats and known limits" sections — the work that makes mesial reliable at scale rather than functionally complete.

- **Push the `ingest` Docker image to a registry** for cross-machine use (currently built locally via `Dockerfile.ingest` only).
- **Incremental re-indexing** — detect changed files via git diff, re-ingest only those, instead of every `analyze_repository` call re-walking the whole tree. LIFECYCLE.md's "diff as synchronization signal" section (`CodeEntityChanged`/`ChunkChanged`/etc. event table) is the design for this.
- **Hybrid search** (lexical + vector) in `search_documents` — pure KNN today; adding a lexical pass would catch exact-identifier queries KNN sometimes misses.
- **LSP process pooling.** Today, every single `analyze_repository` call spawns a fresh `typescript-language-server` subprocess — a few seconds of cold start every time, always. Pooling by workspace root would remove this cost for repeated analysis of the same repo.
- **Transactional `StoreChunks`.** Chunks are written one at a time; a failure mid-batch leaves a partial file state (self-healing on next re-ingest, but not atomic).
- **Stale `:File` cleanup on rename.** Renaming a `.md` or source file leaves the old `:File` node behind with no chunks attached — acceptable today, a real cleanup problem once repos churn more.
- **Health metrics + evaluation hook** (LIFECYCLE.md) — `documented_entity_ratio`, `chunk_anchor_stability_rate`, `facts_with_valid_evidence`, `stale_doc_candidates`, etc., plus a `mesial/eval/` fixture directory (`golden_surface_queries/`, `golden_impact_queries/`, ...) so tuning the clustering algorithm, the linker, or `reanchor` has a regression harness instead of vibes. This is the observability layer productionizing actually depends on — you can't productionize what you can't measure.

## 4. Pathway: Research — open design questions

Framed the way `RATIONALE.md` §11 frames deferred work: not a todo list, a boundary. Each of these needs a decision before it can become a Pathway 2 (Implementation) item — building ahead of the decision means building the wrong shape.

**Issue #6 — `:Protocol` schema**, open questions verbatim from the issue:
- Branching/conditional steps — BPMN territory; P-Plan doesn't model conditionals natively. New `:DECIDES_ON` edge, or leave it an agent-side concern?
- Variable typing — free string, controlled vocabulary, or a pointer to a `:Fact` triplet?
- Step granularity — is a `:Step` always atomic, or can protocols nest (P-Plan supports sub-plan composition)?
- Linkage to facts/observations — does a `:Protocol` carry `:EVIDENCE_FOR`-style backing from successful-run observations?
- Versioning — protocols evolve; supersession via a separate edge, or replace-in-place?
- Explicitly out of scope until a concrete protocol use case (the issue suggests mesial's own deploy runbook) exists to drive the shape.

**Issue #8 — anchor stability & re-anchoring.** The mechanism is sketched (composite `content_hash`/`breadcrumb_hash` key, fallback chain, confidence scoring) — see `ONBOARDING.md` §15 for the worked example of why it's needed. Open questions: exact confidence thresholds for auto-remap vs. human review via `propose_then_confirm`; the equivalent stable-idREADMEentity scheme for `:CodeEntity` beyond `(name, path, src_start, src_end)` — likely `(name, path, signature_hash)` so line-shift edits don't break identity, but the hash's exact inputs aren't decided.

**Issue #9 — `:Test`/`:Failure` ingestion**, open questions verbatim:
- Granularity — one `:Test` per test function, per `describe` block, or per file?
- `EXERCISES` inference — static analysis (cheap, imprecise) vs. runtime instrumentation (precise, expensive) vs. both?
- `:Failure` deduplication — same error across 5 runs: one node with a `run_count`, or five nodes?
- Lifecycle — when does a `:Failure` age out? On resolution? When git history drops the relevant commits?
- Observation linkage — an agent's fix-observation should `:MOTIVATES` a `:Failure` that's already in the past; does this clash with the observation→chunk `:MOTIVATES` semantic where the chunk is a present-tense documented manifestation?

**Deferred without an open issue yet, but named in `RATIONALE.md` §11 / `ARCHITECTURE.md`:**
- Forward-chaining inference over `:ENTAILS`/`:INSTANCE_OF`/`:SUBTYPE_OF` and reified `:Rule` nodes (LIFECYCLE.md's `dlpfc` deep-reasoning tier) — deferred until fact density is high enough for derivations to mean anything.
- Cross-repo / federated queries — would need either a query-fan-out layer or a single global graph (the latter already rejected, see `RATIONALE.md` decision 2).
- Multi-language analyzers (Python, Go) — the `Analyzer` interface is already language-agnostic; no design question blocks this, just no demand yet.

## 5. Open PRs, issues, and branches — acute descriptions

**Two repos, two number sequences.** The table below is **upstream** (`mknw/mesial`)'s tracker, as swept at the time this doc was first written. This fork (`Rishiatweb/mesial`) mirrors most of this under its own issue/PR numbers, and has already diverged in one important way — see the fork-status table right after.

| # | Type | Branch | State | What it actually is |
|---|---|---|---|---|
| 10 | PR | `feat/memory-mcp` | **open, upstream** | `docs/LIFECYCLE.md` rewrite (684 lines) — online-first operating manual. Blocked on one unchecked review-checklist item: "reviewer to check the rewrite addresses PR #7 feedback." Supersedes #7, which was closed by an accidental base-branch deletion (GitHub auto-closes rather than retargets), not by rejection — #7's review thread is preserved and referenced. |
| 8 | Issue | — | **open, upstream** | Anchor stability & re-anchoring spec. Design-only. Flagged in PR #7's review as the **load-bearing risk** for the entire memory layer — every other memory feature silently assumes chunk identity survives re-ingest, and today it doesn't (§4 above, worked example in `ONBOARDING.md` §15). |
| 9 | Issue | — | **open, upstream** | `:Test`/`:Failure`/`:EXERCISES` ingestion design. Raised alongside #8 in the same PR #7 review, as an alternative high-leverage population strategy. Explicitly full-design-pass scope, no implementation. |
| 6 | Issue | — | **open, upstream** | `:Protocol` schema design (simplified P-Plan). Reserved as a label in PR #5 with no schema; this issue is the schema-design follow-up. Deferred until a concrete protocol use case exists. |
| 1 | Issue | — | closed | "Epistemic layer: DOCUMENTS edges and high-level retrieval verbs" — resolved by PR #2 (documents layer + linker). |
| 7 | PR | `feat/memory-mcp` | closed | Original LIFECYCLE.md draft + review. Closed by accident (base branch deleted post-merge), not by rejection. Content lives on in #10. |
| 5 | PR | `feat/memory-layer-foundation` | merged | `:Fact`/`:Observation`/`:GraphMeta` + `h9s-cli memory` subcommand — everything inventoried in §1 above as "done, CLI-only." |
| 4 | PR | `docs/roadmap-notes-schema` | merged | ROADMAP refresh + vault-graph schema documentation. |
| 3 | PR | `feat/mcp-sdk-harness` | merged | `cmd/h9s-cli` dev harness + the `internal/pipeline` extraction that keeps MCP and CLI behavior in lock-step. |
| 2 | PR | `feat/documents-layer` | merged | Documents layer (chunking + embedding + storage) and the doc-to-code linker; docs split into `DESIGN.md`/`ARCHITECTURE.md`/`RATIONALE.md`. |

**Remote branches, upstream** (as swept): `feat/memory-mcp` (PR #10, open — the only unmerged branch with live work at the time), `docs/roadmap-notes-schema`, `feat/documents-layer`, `feat/mcp-sdk-harness` (all three merged; stale remote branches GitHub hasn't auto-deleted).

### Fork status (`Rishiatweb/mesial`) — where this repo has diverged from the table above

| Fork # | Mirrors upstream | State on this fork | Divergence |
|---|---|---|---|
| PR #5 | #10 (`feat/memory-mcp`) | **merged** | Went through an actual review pass on this fork: found a tiering overstatement in LIFECYCLE.md's closing summary and a missing `last_distilled_at` carry-forward rule in `reanchor`. Both fixed (commit `186489b`) before merging. `docs/LIFECYCLE.md` on this fork's `main` is that corrected version — upstream #10 still carries the original text. |
| PR #6 | n/a (fork-only) | open | This doc + `ONBOARDING.md` + README/ROADMAP/DESIGN cross-reference fixes. |
| Issue #2 | #6 | open | `:Protocol` schema — unchanged from upstream. |
| Issue #3 | #8 | open | Anchor stability — unchanged from upstream. |
| Issue #4 | #9 | open | `:Test`/`:Failure` ingestion — unchanged from upstream. |
| Issue #1 | #1 | closed | Mirrors upstream's closed state. |

Upstream issues #6/#8/#9 (mirrored as fork #2/#3/#4) are unaffected by any of this — same open design questions either way, this fork just has its own copies to track against.

## 6. Cross-references

- Data model (nodes, edges, indices) → [`ARCHITECTURE.md` — Data model](ARCHITECTURE.md#data-model)
- MCP tool surface (inputs, behavior) → [`ARCHITECTURE.md` — MCP tool surface](ARCHITECTURE.md#mcp-tool-surface)
- Config / env vars → [`ARCHITECTURE.md` — Configuration](ARCHITECTURE.md#configuration)
- Full decision-by-decision rationale → [`RATIONALE.md`](RATIONALE.md)
- The online-first lifecycle this pathway list is drawn from → [`LIFECYCLE.md`](LIFECYCLE.md) (merged on this fork via PR #5; upstream's copy on `feat/memory-mcp`/PR #10 still has the pre-fix text)

---

*Every claim above traces to a source file, a test file, or a live GitHub issue/PR/branch read directly — not to inference about intent. Re-verify the "Open PRs, issues, and branches" table before relying on it if meaningful time has passed; it reflects a live snapshot, not a fact fixed for all time.*
