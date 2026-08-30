# Implementation

The build log and forward-looking companion to [`ONBOARDING.md`](ONBOARDING.md). Read that one first for narrative context — what mesial is, why it's shaped this way. This one answers three separate questions: what's actually built, what's next to build, and what's still an open research question that shouldn't be implemented until it's answered. It cross-references [`ARCHITECTURE.md`](ARCHITECTURE.md) rather than restating it — the data model, MCP tool table, and config table live there and only there.

## 1. Done — implementation inventory

Package-by-package, at the function level, with what's wired to MCP versus CLI-only, and what has test coverage.

| Package | Shipped | Wired to MCP? | Tests |
|---|---|---|---|
| `internal/chunking` | `ChunkFile`, `FindMarkdown`, `BuildBreadcrumb` — heading-boundary splitter | via `ingest_documents`/`analyze_repository` | none |
| `internal/embedding` | `Client.Embed` — batched llama-server client, Matryoshka truncation | via `ingest_documents`/`search_documents`/`analyze_repository` | none |
| `internal/falkorstore` (`store.go`, `codegraph.go`) | Chunk CRUD, KNN search, code-entity CRUD, `LookupEntityByPosition` | yes, original four tools | none |
| `internal/falkorstore` (`memorystore.go`) | `EnsureGraphMeta`, `AddObservation`, `AddFact`, `LinkEvidenceFor`, `LinkMotivates`, `SearchObservations`, `SearchFacts`, evidence/existence checks — 544 lines, full CRUD | **yes**, via the 5 memory tools below — verified live | `memorystore_test.go`, all 15 cases passing against a real FalkorDB (see verification note below) |
| `internal/analyzer` | `Analyzer` interface, `TypeScriptAnalyzer` (tree-sitter), `Orchestrator` (two-pass driver) | via `analyze_repository` | none |
| `internal/lspclient` | Subprocess wrapper — `initialize`, `didOpen`, `definition`, shutdown | via `analyze_repository` (pass 2) | none |
| `internal/doclinker` | `LinkRepo`, `LinkBySource`, backtick/bare-token scanner | via `ingest_documents`/`link_docs`/`analyze_repository` | `linker_test.go` — the most thoroughly tested package in the repo |
| `internal/pipeline` | `IngestDocs`, `AnalyzeRepository`, `LinkRepo`, repo-resolution helpers, **plus all memory-layer wrappers** (`AddObservation`, `CreateFactFromObservation`, `LinkObservationEvidence`, `SearchObservations`, `SearchFacts`) | code/doc functions yes; memory functions **yes, now wired** — see caveat below | none |
| `cmd/ingest` | MCP server, 9 tools: the original 4 + `add_observation`, `create_fact`, `link_evidence`, `search_observations`, `search_facts` | — | none (integration-shaped, would need a live FalkorDB + llama-server) |
| `cmd/h9s-cli` | Dev harness — `analyze`, `ingest`, `search`, `link`, `memory` (10 subcommands) | n/a, bypasses MCP entirely | none |
| `cmd/chunker` | Standalone chunk-and-print-JSON CLI | n/a | none |

**The gap that mattered most is closed, provisionally.** The memory layer now has MCP tools — `add_observation`, `create_fact`, `link_evidence`, `search_observations`, `search_facts` in `cmd/ingest/main.go`, following the same pattern as the original four. `add_observation` folds `link_motivates` entirely (an agent writing an observation almost always already knows the chunk, if any) and accepts `evidence_for_fact_ids` to link `EVIDENCE_FOR` in the same call — 6 pipeline functions collapsed to 5 tools, not a 1:1 wrap. `link_evidence` stays standalone for retroactive linking (the `propose_then_confirm` fact-generation flow, Tier 3), which is a genuinely different case from linking at write time.

**Verification, not just a caveat lifted:** `go build ./...` and `go vet ./...` both pass clean (required installing a 64-bit mingw-w64 toolchain — the system's existing `gcc` was 32-bit-only and couldn't satisfy cgo, which tree-sitter needs). `go test ./...` — `internal/doclinker` and `internal/falkorstore` (15/15 cases, including all memory-layer tests) pass against a real FalkorDB container. Beyond that, a live end-to-end pass against the actual MCP server: started `cmd/ingest` in HTTP mode against real FalkorDB + llama-server, did the MCP `initialize` handshake, called `tools/list` (all 9 schemas well-formed), then `tools/call add_observation` with `evidence_for_fact_ids` set — confirmed the observation was created, `similar_facts` correctly surfaced the right existing fact via KNN, and the `EVIDENCE_FOR` edge was actually written (checked independently via `h9s-cli memory evidence`, not just trusting the tool's own response). Also confirmed error paths: blank `text` rejected cleanly, `create_fact` against a nonexistent `observation_id` correctly rejected with `:Observation N not found` — the no-orphan-facts invariant holds at the MCP boundary, not just in the Go layer.

**`motivates_chunk_id` — since verified.** Installed `typescript-language-server` and ingested a real fixture doc to get a live chunk ID, then called `add_observation` via MCP with `motivates_chunk_id` set. Response said `motivates_linked: true`; independently confirmed via direct Cypher (`MATCH (o:Observation)-[:MOTIVATES]->(c:Chunk) ...`) that the edge and `c.last_distilled_at` were both actually written — not just trusting the tool's own claim.

**`analyze_repository`'s LSP path — investigated, found a real pre-existing issue, not mine to fix here.** Installed `typescript-language-server` and called `analyze_repository` against a small real fixture repo. Pass 1 (tree-sitter) completed correctly (5 entities parsed). Pass 2 (LSP) hung indefinitely. Isolated the cause with zero mesial code involved — piped a bare LSP `initialize` request directly at `typescript-language-server --stdio` outside of mesial entirely, using a proper persistent bidirectional pipe (not a one-shot redirect, which gives a false negative): the server process starts, consumes real memory (grew to 46MB, so it's doing work), and never responds — not even an error, just silence, for 45+ seconds. This reproduces independent of `internal/lspclient`/`internal/analyzer` — whatever's wrong is in the `typescript-language-server`/Node/Windows interaction in this specific environment, not in code this PR touches. `analyze_repository`'s LSP-dependent path remains genuinely unverified here, but the reason is now understood rather than just "didn't have the tool installed."

## 2. Pathway: Implementation

Ordered by dependency, not just priority — later items build on earlier ones.

1. **~~Expose the memory layer via MCP~~ — done, and verified live.** `add_observation`, `create_fact`, `link_evidence`, `search_observations`, `search_facts` are registered in `cmd/ingest/main.go`, built clean, and confirmed working end-to-end against a real FalkorDB + llama-server + an actual MCP `tools/call` round-trip (§1 has the detail). This was LIFECYCLE.md's Tier 1 item "`add_observation` runtime capture" and the one Tier 1 item with nothing else blocking it — now the first Tier 1 item actually closed out.
2. **`surface(query|path, depth) → context_subgraph`.** The most-called primitive per LIFECYCLE.md — semantic search over chunks/observations, then graph traversal to assemble a context bundle. MVP response shape is already spec'd (versioned JSON: `chunks[]`, `entities[]`, `observations[]`, `confidence{}`) — see LIFECYCLE.md §"surface response shape."
3. **`impact(entity, kinds[]) → dependent_set`.** Edge-traversal-as-impact — reduces "what depends on X?" to a graph query over existing `:CALLS`/`:DOCUMENTS`/`:EVIDENCE_FOR` edges. No new storage needed, purely a read-side MCP tool over data already being written.
4. **Stable identity + `reanchor`.** Blocked on issue #8's design landing first (see §4 below) — this is where "Implementation" and "Research" pathways meet. Once the identity scheme (`content_hash` + `breadcrumb_hash`) is decided, implementation is: add the two hash properties to `:Chunk` on write, then build the match-fallback-orphan algorithm LIFECYCLE.md sketches.
5. **`hygiene_queue(kind) → items[]`.** Time-driven maintenance queues — depends on the health metrics in §3 below existing to query against.
6. **`verify` (5 scopes: logical, structural, evidence, anchor, coverage).** Anchor and structural scopes are buildable now (pure Cypher against existing edges); evidence and logical scopes want an LLM-assisted check and are naturally later.
7. **Fact-generation flow, `:Protocol` ingestion, `:Test`/`:Failure` ingestion.** Tier 3 — the last two are explicitly gated on issues #6 and #9 being ratified first (§4).

## 3. Pathway: Productionizing

From `ROADMAP.md`'s "Other near-term" and `ARCHITECTURE.md`'s "Caveats and known limits" sections — the work that makes mesial reliable at scale rather than functionally complete. None of this blocks anyone from using mesial today; it's what turns "works when I ran it" into "works unattended, for other people, indefinitely." Ordered roughly by how much it currently costs someone using the tool.

### Registry-published `ingest` image

Today `Dockerfile.ingest` only builds locally (`docker compose up -d ingest` builds from `context: .` every time). No image is published anywhere. Cross-machine or CI use means either rebuilding from source every time or manually `docker save`/`docker load`-ing a tarball. Fix is mechanical: pick a registry (GHCR is the obvious default given this already lives on GitHub), add a build-and-push step, pin a tag scheme (git SHA, or semver once there's a release cadence).

### LSP process pooling

Every single `analyze_repository` call spawns a fresh `typescript-language-server --stdio` subprocess (`internal/analyzer/orchestrator.go`, `pass2`) and tears it down at the end of the call (`lsp.Shutdown`). Cold start is a few seconds, every time, even for a repo you just analyzed thirty seconds ago. For occasional use this is a shrug; for anything approaching "re-analyze on every save" or a CI job that calls `analyze_repository` per PR, it's real, avoidable latency. Fix: pool by workspace root, keep a warm LSP process per repo, tear down on idle timeout. Complication worth flagging before starting: `typescript-language-server` holds project state (open documents, `tsconfig` resolution) — a pooled process needs `didClose`/`didOpen` bookkeeping between calls so stale state from a previous analysis doesn't leak into the next one.

### Incremental re-indexing

`analyze_repository` re-walks and re-parses every file in the repo on every call — no notion of "only what changed." For a large repo, or for the "call this on every commit" workflow the online-first lifecycle assumes, this is wasted work that scales with repo size instead of change size. `LIFECYCLE.md`'s "Diff as synchronization signal" section (the `CodeEntityChanged`/`CodeEntityMoved`/`ChunkChanged`/etc. event table) is the actual design for this — it's not a new idea to work out, it's a design already written, waiting for someone to wire git-diff detection into `internal/pipeline` and route the changed-file set through the existing per-file ingestion path instead of the full walk.

### Hybrid (lexical + vector) search

`search_documents` is pure KNN over `:Chunk.vector` today. Vector search is bad at exact-identifier recall — searching for `"EnsureMemoryIndex"` should obviously surface the chunk that names it verbatim, but a 512-dim embedding doesn't guarantee that ranks first. A lexical pass (even something as simple as a FalkorDB fulltext index on `Chunk.content`, unioned with the vector results and re-ranked) would catch the class of query where the user already knows the exact term they're looking for. Low-risk, additive — doesn't change existing behavior, just adds a second ranking signal.

### Transactional `StoreChunks`

`falkorstore.StoreChunks` writes one `:Chunk` node per Cypher query, in a loop, no transaction wrapping the batch. A failure partway through (network blip, FalkorDB restart) leaves a file's chunks partially written. It's self-healing — the next `ingest_documents`/`analyze_repository` call on that file does a full `DeleteBySource` + re-create — but between the failure and the next re-ingest, that file's graph state is inconsistent (some chunks present, some missing, `:DOCUMENTS` edges pointing at a mix of stale and fresh nodes). Worth wrapping in a single multi-statement Cypher transaction (FalkorDB supports this) once this stops being a "rare, self-heals anyway" risk and starts being a "happens often enough to notice" one.

### Stale `:File` cleanup on rename

Renaming a `.md` or source file leaves the old `:File` node behind, orphaned — no chunks or entities point at it anymore, but it's never deleted. Harmless at small scale (a few stray nodes), a real accumulating-cruft problem for a repo with meaningful churn over time. Fix needs a decision first, not just code: how do you *detect* a rename vs. a delete-and-unrelated-create? (Same question issue #8 is already working through for `:CodeEntity` renames via `signature_hash` matching — this is plausibly the same mechanism applied to `:File`, not a separate design.)

### Health metrics + evaluation hook

`LIFECYCLE.md`'s health-metrics table (`documented_entity_ratio`, `chunk_anchor_stability_rate`, `facts_with_valid_evidence`, `stale_doc_candidates`, `chunks_without_observations`, ...) and its proposed `mesial/eval/` fixture directory (`golden_surface_queries/`, `golden_impact_queries/`, `golden_reanchor_cases/`, ...) are both fully specified in the doc — nothing to design here, only to build. This is the one productionizing item that's a genuine *prerequisite* for the others rather than independent: you can't safely tune the clustering algorithm, the linker's precision/recall balance, or `reanchor`'s confidence thresholds without a regression harness telling you whether a change made things better or worse. Every other item on this list is optional polish; this one is what keeps future changes from being vibes-driven.

## 4. Pathway: Research — open design questions

Framed the way `RATIONALE.md` §11 frames deferred work: not a todo list, a boundary. Each of these needs a decision before it can become a Pathway 2 (Implementation) item — building ahead of the decision means building the wrong shape. All three filed issues are explicitly scoped "design only, no implementation" in their own issue bodies — that scoping is deliberate, not an oversight to route around.

### Issue #6 — `:Protocol` schema for procedural memory

**Why it exists.** `:Protocol` is the third memory node label alongside `:Fact` and `:Observation` — reserved by name in PR #5 (the memory-layer-foundation PR) but given no schema. It's meant to hold procedural knowledge — "how to do X" — which the issue argues is often more valuable for a coding agent than declarative facts: examples given are "how to add a new MCP tool," "how to debug LSP failures," "how to release the Docker image." Distinct from `:Fact` (a semantic claim) and `:Observation` (an episodic event) — this is the "how," not the "what" or "when."

**Source.** A simplified subset of [P-Plan](https://www.opmw.org/model/p-plan/) (process-plan ontology, extends PROV-O) — take what's needed, drop the rest.

**Minimal sketch** (from the issue, not final):
```
:Protocol      {name, description, created_at, last_verified_at?}
:Step          {name, description}
:Variable      {name, type?}

(:Protocol)-[:HAS_STEP]->(:Step)
(:Protocol)-[:STARTS_WITH]->(:Step)
(:Step)-[:PRECEDES]->(:Step)            // DAG, not just linear
(:Step)-[:CONSUMES]->(:Variable)
(:Step)-[:PRODUCES]->(:Variable)
```

**Open questions:**
- **Branching/conditional steps.** BPMN territory — P-Plan doesn't model conditionals natively. Add a `:DECIDES_ON` edge, or leave branching as an agent-side concern the schema doesn't try to capture?
- **Variable typing.** Free string, a controlled vocabulary, or a pointer to a `:Fact` triplet (so a step's input/output can be a structurally-checkable claim, not just a label)?
- **Step granularity.** Is a `:Step` always atomic, or can protocols nest — P-Plan supports sub-plan composition (`p-plan:isSubPlanOfPlan`) natively, so this is "do we use that" more than "is it possible."
- **Linkage to facts/observations.** Does a `:Protocol` carry `:EVIDENCE_FOR`-style backing — e.g., observations of successful runs consolidating into confidence that the protocol is correct? Or is it evidence-free by nature (a protocol is prescriptive, not a claim to be verified)?
- **Versioning.** Protocols evolve as the codebase does. Supersession via a dedicated edge (old protocol points at its replacement), or replace-in-place with history lost?

**What would unblock it:** the issue itself suggests the trigger — "at least one concrete protocol use case (e.g., the deploy runbook for mesial itself) drives the shape." Not a research task to resolve in the abstract; a real protocol to model first, then generalize from.

### Issue #8 — anchor stability and re-anchoring spec

**Why it exists.** The load-bearing risk for the entire memory layer, not a nice-to-have. `analyze_repository`/`ingest_documents` `DETACH DELETE`s and recreates `:Chunk` nodes on every re-run (`falkorstore.DeleteBySource` → `StoreChunks`) — FalkorDB assigns fresh internal IDs on create, so any `:MOTIVATES` edge pointing at the old chunk ID is silently dropped. An observation stays in the graph looking healthy, disconnected from the doc that grounded it, with nothing surfacing the disconnect. `ONBOARDING.md` §15 walks through this with the actual code. Every other memory feature (fact verification, `surface`, coverage metrics) silently assumes anchors survive re-ingest; today they don't.

**Mechanism, already sketched** (not the open part):
1. **Stable identity properties** on `:Chunk`: `content_hash` (whitespace-normalized, so trivial reformatting doesn't invalidate it), `breadcrumb_hash` (heading path), composite key `source_path + breadcrumb_hash + content_hash`.
2. **Re-anchoring algorithm** — match new chunks to old by composite key (high confidence) → fall back to `source_path + content_hash` alone (catches renamed sections) → fall back to vector similarity over old/new embeddings plus shared `:DOCUMENTS` targets (lowest confidence) → surface anything unmappable as orphaned for review.
3. A `reanchor(repo, changed_sources?) → ReanchorReport` primitive returning `remapped`/`ambiguous`/`orphaned`/`created_anew`, with `:MOTIVATES` edges replayed from old chunk to new.

**Open questions — the actual research gap:**
- **Confidence thresholds.** Below what score does a re-linked observation queue for human review via `propose_then_confirm` instead of auto-remapping? No default is proposed anywhere yet.
- **`:CodeEntity` stable identity.** The equivalent problem for code, not just docs — today identity is `(name, path, src_start, src_end)`, which breaks on any line-shift edit. Proposed direction is `(name, path, signature_hash)`, but what exactly feeds the hash (parameter types? return type? just the signature text?) isn't decided.
- **`last_distilled_at` carry-forward.** Added as a fix during PR review on this fork (see PR #5's commit `186489b`) but worth restating here as an open implementation detail: a successful remap must carry `Chunk.last_distilled_at` from the old node to the new one, or every reanchor makes freshly-verified chunks look unverified again.

### Issue #9 — test and runtime trace ingestion (`:Test`, `:Failure`, `:EXERCISES`)

**Why it exists.** Tests are executable documentation; a captured failure is a gold-standard episodic observation. Without this, two extremely high-frequency agent questions have no structural answer: *"what tests should I run after changing function F?"* (today: grep test directories for callers — slow, lossy) and *"where has this code failed before?"* (today: trawl CI logs or git history). With the schema below, both become graph traversals: `(:Function {name:F})<-[:EXERCISES]-(:Test)` and `(:Function {name:F})<-[:OBSERVED_IN]-(:Failure)<-[:MOTIVATES]-(:Observation)`.

**Rough schema** (explicitly not final — full design pass needed):
```
:Test    {name, file_path, framework}
:Failure {id, observed_at, error_text, stack_summary?}

(:Test)-[:EXERCISES]->(:Function | :Method | :Class)
(:Failure)-[:OBSERVED_IN]->(:Test)
(:Observation)-[:MOTIVATES]->(:Failure)
```

**Open questions:**
- **Granularity.** One `:Test` per test function, per `describe`/`context` block, or per file? Affects both graph size and how precise "what tests exercise this" answers can be.
- **`EXERCISES` inference.** Static analysis (parse call graphs inside test bodies — cheap, some false negatives on indirection) vs. runtime instrumentation (precise, requires actually running tests with tracing) vs. both, with static as the cheap default and runtime as an opt-in precision upgrade.
- **`:Failure` deduplication.** The same error recurring across 5 runs — one `:Failure` node with a `run_count` property, or five separate nodes forming a timeline? Affects whether "where has this failed before" reads as a count or a history.
- **Lifecycle.** When does a `:Failure` age out — on resolution (the fix commit lands)? When git history drops the relevant commits entirely? Never, and let `hygiene_queue` surface old ones instead of deleting?
- **Observation-linkage tension.** An agent's fix-observation should `:MOTIVATES` a `:Failure` — but `:Failure` describes something in the past, while the existing `(:Observation)-[:MOTIVATES]->(:Chunk)` semantic is present-tense ("this chunk's current content is grounded"). Does the same edge type stretch to cover both senses cleanly, or does this need its own edge?

**Explicitly out of scope for the issue itself:** implementation, and any specific test-framework integration (Jest, Vitest, `go test`, pytest) — those come after the schema design, as per-framework adapters.

### Deferred without a filed issue yet

Named in `RATIONALE.md` §11 / `ARCHITECTURE.md`, not yet formalized as GitHub issues:

- **Forward-chaining inference** over `:ENTAILS`/`:INSTANCE_OF`/`:SUBTYPE_OF` and reified `:Rule` nodes (`LIFECYCLE.md`'s `dlpfc` deep-reasoning tier, Tier 4). Deferred deliberately until fact density is high enough for derivations to mean anything — inferring over a sparse fact graph produces conclusions with no real support.
- **Cross-repo / federated queries.** Would need either a query-fan-out layer across per-repo graphs, or a single global graph — the latter already rejected (`RATIONALE.md` decision 2: identifier collisions across projects, messy cleanup). No design started on the fan-out alternative.
- **Multi-language analyzers** (Python, Go, etc.). Not actually blocked on a design question — the `Analyzer` interface (`internal/analyzer/analyzer.go`) is already language-agnostic by construction. Adding a language is "a grammar + symbol-extraction visitor + LSP wiring," per `RATIONALE.md` §11 — genuinely just unstarted work, not an open question.

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
