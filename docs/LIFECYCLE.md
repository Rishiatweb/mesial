# Lifecycle

How a repository moves through mesial — over time, with the agent in the loop.

This document is **online-first**: most of mesial's value comes from constant background utility during ordinary developer work, not from a one-time bootstrap pass. The bootstrap (deterministic ingestion) is the substrate; online use is the point. The earlier draft of this doc had that backwards — substrate was front-loaded and online use was an appendix. The current order corrects that.

Companion to [`DESIGN.md`](DESIGN.md) (the conceptual model) and [`ARCHITECTURE.md`](ARCHITECTURE.md) (the engineering view). This doc is about *time* — what happens when, in what order, by whose action.

---

## The seven-loop spine

Every interaction with mesial — bootstrap or online — is some composition of seven loops:

```
1. Anchor    Text/code becomes graph nodes with stable identity that survive churn.
2. Surface   A task / query / file produces a useful context subgraph.
3. Act       The agent edits, tests, investigates, reviews, decides.
4. Capture   Important discoveries from the act become observations.
5. Distill   Repeated or high-confidence observations become facts (or protocols).
6. Verify    Facts, anchors, evidence, and coverage are checked against current code.
7. Repair    Broken anchors, stale docs, stale facts, coverage gaps become queues, not silent decay.
```

The design earlier in this project's history under-emphasized **Repair**. A memory system that cannot repair itself becomes archaeological — verification finds nothing wrong because the verification target has drifted. A memory system that continuously repairs itself becomes infrastructure. The lifecycle below treats repair as first-class.

## Core invariants

What must remain true for mesial to be trustworthy:

1. **Durable claims are evidence-backed.**
   No `:Fact` exists without at least one `:Observation` linked via `:EVIDENCE_FOR`. Free-floating LLM assertions don't enter the conceptual layer.

2. **Anchors are repairable.**
   Observations and facts must survive ordinary code/doc churn. Chunk regeneration cannot silently disconnect them; re-anchoring is a built-in operation, not a manual rescue.

3. **Online use is the primary write path.**
   The highest-value memory is captured during real developer work — when the agent has just paid the cost of understanding something. Offline corpus distillation is a fallback, not the default.

4. **Verification is scoped.**
   Logical, structural, evidence, anchor, and coverage verification are separate checks with separate signals. Lumping them masks failures.

5. **Repair is a first-class lifecycle phase.**
   Broken anchors, stale docs, stale facts, and coverage gaps become explicit queues that the agent (or scheduled jobs) work through. Decay is observable, not silent.

These invariants govern every concrete decision below.

---

## Online lifecycle (primary)

The everyday loop. Agent does work; mesial is in the loop continuously, both as a context provider (read) and as a memory accumulator (write).

### Conceptual frame: three modes

Mesial usage decomposes into three modes:

- **Semantic mode** — vector search over chunks/observations to find relevant material. *"What does the codebase say about X?"*
- **Logical mode** — structural queries over the code graph + (eventually) inference over facts. *"What are the consequences of changing X?"*
- **Combined mode** — semantic search to find candidates, then logical traversal/inference to validate or expand. *"Find code likely related to X, then check whether changing it would break invariants."*

The interesting questions are mostly *combined*. Semantic alone is grep-with-vibes. Logical alone is grep-with-structure. The combination is what justifies the architecture.

### Agent moments

The online lifecycle is organized around **agent moments** — the points in real work when an agent should call mesial. Each moment names: the trigger, the primitive call, what mesial returns, what the agent does with it.

#### Moment 1: Task starts

- *Trigger*: agent receives a new task (bug report, feature request, refactor brief, etc.).
- *Call*: `surface(task_description, depth=full)`.
- *Returns*: a context subgraph — relevant chunks, code entities, observations, facts, with confidence indicators.
- *Agent action*: load the subgraph as initial context. This replaces 50–80% of the "where do I start?" exploratory grep loop.

#### Moment 2: File opened

- *Trigger*: agent opens a file it hasn't seen this session.
- *Call*: `surface(file_path, depth=tour)`.
- *Returns*: file's defined entities, their cross-references (calls, extends, implements), documenting chunks, observations and facts about those entities.
- *Agent action*: structured tour rather than blind read. *"This file defines X (extends Y, called by Z); X is documented in chunk Q which says ..."*.

#### Moment 3: Symbol edited

- *Trigger*: agent changes a function/method/class signature, removes an entity, etc.
- *Call*: `impact(symbol, kinds=[CALLS, DOCUMENTS, EVIDENCE_FOR, ABOUT])`.
- *Returns*: dependents stratified by edge kind: callers (must update), documenting chunks (review docs), observations and facts that mention the symbol (re-verify).
- *Agent action*: a checklist of impact sites, prioritized. Optionally followed by `verify(facts_about_symbol)` after the change to flag broken claims.

#### Moment 4: Unexpected behavior found / convention learned

- *Trigger*: agent notices something during work — a function silently truncates inputs, a flag gates an unexpected path, a convention is stricter than docs say, etc.
- *Call*: `add_observation(text)` (single-shot; KNN against existing observations runs server-side).
- *Returns*: the observation ID + similar existing observations + facts those observations back. The KNN pass surfaces whether this is a known issue or a fresh discovery.
- *Agent action*: optionally refine the observation given the surfaced context, then commit via `confirm_observation`. The user is the commit gate.

#### Moment 5: Before commit / PR

- *Trigger*: agent is about to create a commit or open a PR.
- *Calls*: `verify_changed_entities(file_paths)` + `verify_docs_for_changed_entities(file_paths)`.
- *Returns*: facts that would now be contradicted by the change; chunks documenting changed entities (likely stale); observations about the entities (re-verify candidates).
- *Agent action*: address the report before opening the PR — update docs, retract or amend stale facts, re-affirm observations that survived the change.

#### Moment 6: Graph drift detected (post-merge / scheduled)

- *Trigger*: a merge changed source files; or a scheduled drift check.
- *Calls*: `reanchor(repo, changed_sources)` followed by `hygiene_queue("stale_docs" | "broken_anchors" | "stale_facts")`.
- *Returns*: re-anchored chunk/observation pairs (with confidence); orphaned anchors that need human review; queues of items to re-verify.
- *Agent action*: clear queues opportunistically (when otherwise idle) or pull explicitly during dedicated maintenance windows.

### Agent trigger policy

The same content as the moments above, condensed into an operating manual the agent skill can follow without ambiguity:

| Trigger | Primitive |
|---|---|
| Task starts | `surface(task)` |
| File opened | `surface(file_path)` |
| Symbol edited | `impact(symbol)` |
| Unexpected behavior found / convention learned | `add_observation(text)` |
| Before commit / PR | `verify_changed_entities(files)` + `verify_docs_for_changed_entities(files)` |
| After merge / on schedule | `reanchor(changed_sources)` + `hygiene_queue(kind)` |

If the agent skill follows this policy, every commit cycle automatically passes through Anchor → Surface → Act → Capture → Verify → Repair without the agent having to remember which call to make when.

### Backbone primitives

Six primitives compose every scenario above:

| Primitive | Signature (sketch) | Implements |
|---|---|---|
| `surface` | `surface(query|path, depth) → context_subgraph` | Pattern 1 (semantic-then-traverse) |
| `propose_then_confirm` | `propose_X(items) → session_id, proposed[]; confirm_X(session_id, accepted[]) → committed_ids` | Pattern 3 (KNN-cluster-confirm) |
| `impact` | `impact(entity, kinds[]) → dependent_set` | Pattern 4 (edge-traversal as impact) |
| `verify` | `verify(scope, target_ids?) → report` | Pattern 2 (triplet-against-code) |
| `hygiene_queue` | `hygiene_queue(kind) → items[]` | Pattern 5 (time-driven maintenance) |
| `reanchor` | `reanchor(repo, changed_sources?) → ReanchorReport` | Repair loop |

`reanchor` is new compared to the previous draft — promoted from "implementation detail" to a backbone primitive because anchor stability is load-bearing for every other primitive (see [Anchor stability](#anchor-stability-and-re-anchoring) below).

### `surface` response shape (MVP + extension)

`surface` is the most-called primitive — its response shape needs to be designed for evolution from day one. MCP response schemas are easy to add fields to (clients ignoring unknown fields), hard to restructure once shipped.

**MVP shape (v1):**

```json
{
  "version": 1,
  "chunks": [{"id": "...", "content": "...", "source": "...", "breadcrumb": "..."}],
  "entities": [{"id": "...", "label": "...", "name": "...", "path": "..."}],
  "observations": [{"id": "...", "content": "...", "fact_ids": ["..."]}],
  "confidence": {
    "retrieval": 0.0,
    "coverage": 0.0,
    "staleness": 0.0
  }
}
```

`confidence` is intentionally a block (not three top-level fields) so it can grow without polluting the root.

**Reserved for v2+ (added without breaking clients):**

- `facts[]` — full triplets, not just IDs
- `risks[]` — surfaced from facts that mark deprecated/incompatible/unstable
- `open_questions[]` — areas with low coverage relevant to the query
- `suggested_next_queries[]` — agent skill hints for follow-up

The discipline: **never remove or repurpose a field**, always extend. Versioning lets clients negotiate (`accept-version` or equivalent) for major changes.

### Recurring patterns

The backbone primitives implement six patterns that show up across scenarios:

#### Pattern 1: Semantic-then-traverse (workhorse)
`vector search → graph traversal → context assembly`. Used by every reactive load (Moments 1, 2, partially 3).

#### Pattern 2: Triplet-against-code (the bridge)
`fact triplet → resolve subject/object to code entities → check relation in code graph`. The bridge between semantic and logical layers. Used by `verify` (most scopes), Moment 5.

#### Pattern 3: KNN-then-cluster-then-confirm (human-in-loop primitive)
`group similar items → present to user → batch commit confirmed`. The MCP elicitation pattern. Used by all distillation flows (bootstrap interview, runtime observation capture, fact generation, conflict resolution).

#### Pattern 4: Edge-traversal-as-impact (change analysis)
`from changed entity → traverse incoming edges → enumerate dependents`. Reduces "what depends on X?" to a graph query. Used by Moment 3, refactoring, PR review, dead-code detection.

#### Pattern 5: Time-driven maintenance queues (hygiene)
`property timestamp + age threshold → review queue`. Used by `hygiene_queue` for stale docs, orphan observations, fact verification staleness.

#### Pattern 6: Combined semantic + logical (most powerful)
`semantic to find candidates → logical to validate or expand`. Most of the high-leverage scenarios live here.

### Prioritization

Updated to put online use ahead of corpus distillation. The previous tier order had bootstrap interview as Tier 1; this version moves it later, after the online primitives.

**Tier 1 — must-have for "useful coding agent"**:
- `surface` (MVP shape) — Moment 1, Moment 2
- Stable chunk/entity identity + `reanchor` — anchor invariant must hold before any deep memory work
- `add_observation` runtime capture (Moment 4) — the primary write path per Invariant 3
- `impact` (Moment 3) — change-management primitive, daily-use payoff

**Tier 2 — force multipliers**:
- `hygiene_queue` (Moment 6) — keeps Tier 1 from rotting
- Bootstrap interview as one of multiple population paths (Section: [Population strategies](#population-strategies))
- Diff-driven population from PR events
- `verify` (anchor + structural scopes — Moment 5)

**Tier 3 — advanced reasoning**:
- Fact generation flow over accumulated observations
- `verify` (logical + evidence scopes)
- `:Protocol` ingestion and consumption ([Issue #6](https://github.com/mknw/mesial/issues/6))
- `:Test` / `:Failure` ingestion ([Issue #9](https://github.com/mknw/mesial/issues/9))

**Tier 4 — late / specialized**:
- Doc generation, migration planning, security audit, feature-flag mapping, dead-code detection
- Self-monitoring meta-loops
- Forward chaining inference (`dlpfc`'s deeper reasoning)

The most leveraged near-term outcome is not "the graph can infer deep truths." It is: **a coding agent starts a task, gets the right context, changes code, updates the graph, and leaves the next agent smarter than it was.**

---

## Anchor stability and re-anchoring

The load-bearing risk for the memory layer. `analyze_repository` deletes-and-recreates `:Chunk` nodes on each run (the chunk IDs change), which silently drops every `:MOTIVATES` edge that pointed at the old chunks. Observations and facts then become disconnected from the documented evidence that grounded them. Verification will appear correct while the underlying ground has shifted.

### Stable identity properties

Add to `:Chunk` (similar treatment for `:CodeEntity`):

- `content_hash` — hash of normalized chunk content (whitespace-normalized so trivial reformatting doesn't invalidate)
- `breadcrumb_hash` — hash of the heading path
- Composite key: `source_path + breadcrumb_hash + content_hash` survives most edits

For `:CodeEntity`: a stable identity beyond `(name, path, src_start, src_end)` — likely `(name, path, signature_hash)` so line shifts don't break identity.

### Re-anchoring algorithm (sketch)

When chunks are regenerated:
1. Match new chunks to old by `source_path + breadcrumb_hash` (high confidence)
2. Fall back to `source_path + content_hash` (handles renamed sections)
3. Fall back to vector similarity over old/new embeddings + shared `:DOCUMENTS` targets (lower confidence)
4. Surface unmappable old chunks as orphaned for review
5. Replay `:MOTIVATES` edges from old → new with confidence scores attached

Below a confidence threshold, re-linked observations queue for human review via `propose_then_confirm`.

A remap must also carry forward `:Chunk.last_distilled_at` from the old node to the new one. That property exists to answer "has this content actually been engaged with" (§ Health metrics); resetting it on every reanchor would make freshly-reanchored chunks look unverified again immediately after a routine doc edit, which would corrupt `chunks_without_observations` and the coverage queue right when reanchoring is supposed to be *preserving* prior verification, not erasing it. `created_anew` chunks (no old chunk to remap from) correctly start with no `last_distilled_at`, same as any new chunk today.

### `reanchor` primitive

```
reanchor(repo, changed_sources?) → ReanchorReport {
  remapped: [{old_chunk_id, new_chunk_id, confidence, edges_replayed}],
  ambiguous: [{old_chunk_id, candidate_new_chunk_ids}],
  orphaned: [{old_chunk_id, observation_ids_affected}],
  created_anew: [new_chunk_ids],
}
```

Full design in **[Issue #8](https://github.com/mknw/mesial/issues/8)**. Implementation lands before fact-generation work — Invariant 2 demands it.

---

## Diff as synchronization signal

Treat git diff as a first-class **event source**. A merged change emits typed events the maintenance system consumes:

| Event | Source | Triggered actions |
|---|---|---|
| `CodeEntityChanged` | tree-sitter delta | `verify_changed_entity`, queue documenting chunks for `verify_docs_for_changed_entity` |
| `CodeEntityMoved` | LSP rename detection + signature_hash match | Update entity identity; re-link `:DOCUMENTS` edges to the moved entity |
| `CodeEntityDeleted` | tree-sitter absence | Mark documenting chunks as orphaned; mark facts mentioning the entity as `needs_reverification` |
| `ChunkChanged` | content_hash mismatch on re-chunk | `reanchor` for that chunk; queue dependent observations for confidence check |
| `ChunkMoved` | source_path or breadcrumb change | Re-anchor with `breadcrumb_hash` match |
| `ChunkDeleted` | source removed | Orphan the `:MOTIVATES` edges; surface affected observations |
| `DocMentionsChanged` | linker re-scan finds new/removed identifier mentions | Update `:DOCUMENTS` edges; queue affected entities for `verify_docs_for_changed_entity` |
| `TestTouched` | file under `/test/` changed | (Future, [Issue #9](https://github.com/mknw/mesial/issues/9)) re-link `:EXERCISES` edges |

Events feed `impact`, `hygiene_queue`, `verify`, and `reanchor`. **Maintenance is event-driven first, scheduled second.** Scheduled queues exist as the safety net; events exist for the common case.

This is the bridge between static graph and living codebase — the mechanism by which Invariant 5 (repair) is operationalized.

---

## Population strategies

The conceptual layer (observations, facts, protocols) gets populated through several paths, not just the bootstrap interview. Per Invariant 3, **work-driven capture is the primary path**; the others backstop and supplement it.

### Strategy 1: Work-driven observation capture (primary)

What it is: when the agent finishes a meaningful work unit — fixed a bug, validated a hypothesis, learned a convention — call `add_observation` immediately with what was learned. KNN surfaces neighbors and related facts; user confirms via elicitation.

High-value trigger moments:
- After fixing a bug
- After a failed hypothesis
- After discovering a convention
- After editing a public API
- After resolving an ambiguity
- After running tests and learning what matters
- After reading a file with no existing observations

The agent skill embeds prompts for these moments. Each one is a memory deposit.

Why this is the primary path: the best memory is created when the agent has just paid the cost of understanding something. Capturing then is cheap; capturing later is much harder.

### Strategy 2: PR / diff-driven extraction

What it is: every diff is a structured source of candidate observations.

Diff candidates:
- Changed public API signature → "the public surface of X changed"
- Changed function behavior → behavioral observations from the agent's understanding of the change
- New dependency edge (new import) → architectural fact candidate
- Deleted entity → cleanup observations
- New test coverage → "X is now tested for behavior Y"
- Docs changed without code → "docs updated to reflect Z"
- Code changed without docs → docs-out-of-sync observation

Per-event hooks emit candidates that go through `propose_then_confirm` like any other.

### Strategy 3: Test and runtime trace ingestion ([Issue #9](https://github.com/mknw/mesial/issues/9))

Tests are executable documentation. Failures captured during runs are gold-standard episodic observations.

Rough shape (full design in Issue #9):

```
:Test {name, file_path, framework}
:Failure {id, observed_at, error_text}
(:Test)-[:EXERCISES]->(:Function | :Method | :Class)
(:Failure)-[:OBSERVED_IN]->(:Test)
(:Observation)-[:MOTIVATES]->(:Failure)
```

Value: "what tests exercise this function?" and "where has this code failed before?" become first-class queries instead of grep + log trawls.

### Strategy 4: Commit-message and PR-review ingestion

Cheap source of rationale that docs rarely capture. Commit messages and PR descriptions/reviews populate `:Observation` nodes with source provenance. Most teams already write these; mesial just files them.

### Strategy 5: Protocol mining ([Issue #6](https://github.com/mknw/mesial/issues/6))

Procedural knowledge — "how to do X" — is often more valuable for coding agents than declarative facts. Examples: "How to add a new MCP tool", "How to debug LSP failures", "How to release the Docker image".

Proposed v1 schema (see Issue #6 for full discussion):

```
:Protocol {name, goal, preconditions[], steps[], success_signals[], failure_modes[], related_entities[]}
```

`related_entities` lets `impact(entity)` surface relevant protocols when an entity is touched.

### Strategy 6: Assertion extraction from docs

LLM-assisted extraction of normative sentences from existing docs:
- "must" / "never" / "requires" / "is responsible for" / "is not" / "only" / "deprecated" / "source of truth"

These become *fact candidates*, not facts. They pass through `propose_then_confirm` like everything else.

### Strategy 7: Query-driven memory

When `surface` returns low-confidence or sparse context, the agent prompts: "Should I create an observation from what I just learned?" Captures memory exactly where the graph failed to help — directly addresses coverage gaps.

### Strategy 8: Bootstrap interview (offline, when needed)

What it is: read the entire docs corpus, group chunks for global thinking, propose observations spanning each group, propose facts from observation clusters. The original lifecycle treated this as the primary populating force; Invariant 3 demotes it to one option among many — useful for *initial* population on a mature, undocumented-in-mesial repo, but not the daily mode.

#### Cluster identification (used by Strategy 8 chunks and Strategy 8 / Tier 3 facts)

Both the chunk interview and the fact-generation step need **clusters of related items** — same algorithm, different inputs. The clustering must satisfy three constraints simultaneously:

1. **Coherence** — items in a cluster should overlap topically.
2. **Coverage** — every item should land in some cluster (no orphans during distillation).
3. **Bounded size** — each cluster must fit in an LLM context window with headroom.

No off-the-shelf algorithm satisfies all three cleanly. Mesial uses a **greedy budget-aware KNN walk with structural boosting**:

**Inputs**: items with vectors + structural metadata; per-cluster budget (chars or tokens); similarity function.

**Similarity function** (returns roughly `[0, 1.5]`; higher = more related):
- Base: cosine similarity between embeddings, mapped to `[0, 1]`.
- Structural bonus when sharing a neighbor:
  - For chunks: `+0.3` if they share a `:DOCUMENTS` target or breadcrumb prefix; `+0.2` if same source file.
  - For observations: `+0.3` if they share a `:MOTIVATES` target chunk; `+0.2` if motivated chunks are in the same file.

**Algorithm**:
1. KNN-10 over each item's vector (one query against FalkorDB's vector index).
2. Seed order: items sorted by **structural degree** — chunks by outgoing `:DOCUMENTS`-edge count (information-rich first); observations by outgoing `:EVIDENCE_FOR`-edge count (or `:MOTIVATES` count if no facts yet).
3. Pick next seed: the highest-degree unassigned item.
4. Greedy expand: among the seed's KNN-10, take the highest-similarity unassigned neighbor (above min threshold of `0.3`), add to cluster, deduct from budget. Repeat until budget exhausted or no qualified neighbor remains.
5. Emit cluster, mark items assigned, return to step 3.
6. Singleton post-pass: if a cluster has only one item but budget remains, try to merge into the nearest cluster with budget headroom; otherwise emit as a single-item group.

**Why not k-means / HDBSCAN / Louvain?** k-means needs `k` upfront (size is driven by budget, not `k`); HDBSCAN finds natural density clusters but ignores budget; Louvain produces good global structure but needs post-processing for size and brings a dependency. Greedy KNN-walk: simple, deterministic, no library, swappable later.

**Recomputation**: clusters are ephemeral, built per round, never stored. Cost is `O(N × 10)` per round.

**Tunables** (exposed as flags / MCP args):
- `--budget-chars` (default 24000, ≈ 6 KB tokens)
- `--min-similarity` (default 0.3 for chunks, 0.4 for observations — tighter for cleaner facts)
- `--seed-by` (default `degree`; alternatives: `random`, `created_at`)

#### Tool flow for the bootstrap interview

```
propose_observations(repo, chunk_ids[], granularity?)
  → returns { session_id, proposed: [ {text, motivates_chunk_ids[]} ] }

confirm_observations(session_id, accepted: [...], rejected_indexes: [...])
  → returns { observation_ids[], motivates_edges_created }
```

For facts:

```
propose_facts(repo, observation_ids[], target_count?)
  → returns { session_id, proposed: [ {subject, predicate, object, evidence_obs_ids[]} ] }

confirm_facts(session_id, accepted: [...], rejected_indexes: [...])
  → returns { fact_ids[], evidence_edges_created }
```

`evidence_obs_ids` on each proposed fact enforces Invariant 1 upstream — a fact can't be confirmed without an observation backing it.

Sessions are in-memory in the ingest server, TTL ~10 minutes. Expired sessions are restarted from `last_distilled_at` markers on chunks/observations, so progress isn't lost.

#### Group sizing for the interview

- Per group: 2–5 chunks, totalling roughly 4–6 KB tokens.
- Target observations per group: `round(total_group_chars / 1200)`, scaled by `granularity / 5` (default granularity 5; range 0–10).
- A group of 5 × 1500-char chunks → ~6 observations at default granularity, ~12 at granularity 10.

---

## Verification (scoped)

Per Invariant 4, verification is decomposed into separate scopes with separate signals. Each scope answers a different question.

### Scope: Logical verification

Question: do facts contradict each other?

For predicates with functional semantics (`is_a`, `equivalent_to`), look for siblings that conflict:

```cypher
MATCH (f1:Fact {subject:$s, predicate:'is_a'})
MATCH (f2:Fact {subject:$s, predicate:'is_a'})
WHERE f1.object <> f2.object
RETURN f1, f2
```

For `incompatible_with` pairs: also check that no other fact asserts `requires` or `causes` between the same subjects.

Per the "no stored contradictions" rule, surfaced contradictions are *resolved immediately* by the agent + user, not persisted as edges.

### Scope: Structural verification

Question: do facts that mention code entities match the code graph?

For triplets where both subject and object map to known entities:

| Triplet | Code-graph check |
|---|---|
| `(X, subtype_of, Y)` where both classes | `(X)-[:EXTENDS]->(Y)` exists |
| `(X, subtype_of, Y)` where Y is interface | `(X)-[:IMPLEMENTS]->(Y)` exists |
| `(X, part_of, Y)` where X method, Y class | `(Y)-[:DEFINES]->(X)` exists |
| `(X, requires, Y)` both functions | `(X)-[:CALLS*1..k]->(Y)` exists (transitive) |

Mismatch: claim-without-structural-backing — either the code changed (fact stale) or the fact was wrong.

### Scope: Evidence verification

Question: does the evidence supporting a fact actually support it?

For each `:Fact`, walk its incoming `:EVIDENCE_FOR` edges to observations, walk those observations' outgoing `:MOTIVATES` to chunks. Check:
- Do the observation contents semantically support the fact's triplet? (LLM check, can be batched)
- Do the chunks the observations are grounded in still exist?
- Are the chunks' contents still consistent with the observations? (`content_hash` comparison + LLM check on drift)

A fact whose evidence has decayed gets flagged for re-verification, not auto-pruned.

### Scope: Anchor verification

Question: do anchors still point to the right things?

- `:Observation`-`:MOTIVATES`-`:Chunk`: does the observation still describe the chunk's current content?
- `:Chunk`-`:DOCUMENTS`-`:CodeEntity`: was the link a name collision? Was it a meaningful description that now points at unrelated code?
- `:CodeEntity` rename detection: does an entity's `signature_hash` match a recently-deleted entity's hash? → propose rename.

Confidence-scored. Below threshold queues for review.

### Scope: Coverage verification

Question: what important parts of the system lack documentation, observations, or facts?

Surface absences:
- High-centrality `:CodeEntity` (many incoming `:CALLS`) with no `:DOCUMENTS` → undocumented hot spot
- `:Chunk` with no `:MOTIVATES` → unverified docs
- `:Observation` > 30 days old with no `:EVIDENCE_FOR` → episodic-only (prune candidate)
- `:Fact` whose subject doesn't appear in any backing observation's text → spurious fact

These are health signals, not errors. They drive `hygiene_queue`.

### Practical near-term checks (vs. forward chaining)

Forward chaining inference (the `dlpfc` deep reasoning) is **deferred** until the graph has sufficient quality and density. Near-term, the practical verification surface is:

```
verify_changed_entity(entity_id) → report                  // Moment 5 trigger
verify_docs_for_changed_entity(entity_id) → stale_chunks   // Moment 5
verify_observation_anchors(observation_ids?) → drift_report // anchor scope
verify_fact_against_code(fact_ids?) → mismatches            // structural scope
verify_coverage(repo, signal_kinds?) → gaps                 // coverage scope
```

These five cover Tier 2 and most of Tier 3. Forward chaining (`:ENTAILS` / `:Rule` reification) lands later when there's enough fact density to make derivations meaningful.

---

## Substrate construction (the bootstrap chronology)

The deterministic ingestion that underlies everything above. Order matters: tree-sitter, LSP, docs, linker, then observation/fact layers can be populated.

### 1. Initialization

The point at which mesial knows *of* a repository but not yet anything about it.

#### Empty repository

```
h9s-cli memory init --repo {name}
```

Writes the singleton `:GraphMeta`:

```
:GraphMeta {
  kind,                      // "code" | "notes" | "memory"
  strict,                    // default false
  schema_version,            // current schema version (for migrations)
  embedding_model_version    // identifier for the embedding model used (e.g., "qwen3-0.6b-q8")
}
```

`schema_version` and `embedding_model_version` are essential for future migrations and reproducibility — checking either of these against current values surfaces graphs that need re-embedding or schema migration.

Also creates the indexes the corresponding kind needs.

#### Implicit initialization

For typical onboarding, `analyze_repository` and `ingest_documents` create the graph on first write. Explicit `memory init` is only needed for the global `memory` graph, asserting kind upfront, or re-affirming `strict`.

### 2. Onboarding from existing repos

```
h9s-cli analyze --path /path/to/repo
```

Four passes:

1. **Tree-sitter** — `:File`, `:Class`, `:Function`, `:Method`, `:Constructor`, `:Interface`, `:Enum` + `:DEFINES` edges.
2. **LSP** — `:CALLS`, `:EXTENDS`, `:IMPLEMENTS`, `:RETURNS`, `:PARAMETERS`.
3. **Doc ingestion** — markdown chunked by heading boundary, embedded, `:Chunk` + `:OF_FILE`.
4. **Doc-to-code linker** — `:DOCUMENTS` edges via identifier-mention scan.

After this completes, the graph has its **perceptual + structural** state. Observations and facts are added through the population strategies above.

#### Re-onboarding caveats

Idempotent but with the chunk-churn risk that motivated [anchor stability](#anchor-stability-and-re-anchoring). Until that work lands, observations should re-link to chunks after a doc edit; with `reanchor` in place, this becomes automatic.

### 3. Ingestion mechanics

Mechanical, deterministic. Already implemented:

| Asset | Pipeline | Output |
|---|---|---|
| TypeScript files | tree-sitter + LSP | `:File`, code entities, structural edges |
| Markdown files | heading chunker + Qwen3 embedder | `:Chunk` (vector or `oversized=true`) + `:OF_FILE` |
| Identifier mentions | `internal/doclinker` regex scanner | `:DOCUMENTS` edges |

### Ingestion report (new)

Each ingestion run emits a **health report** so it's clear whether the result is usable, not just whether the run completed:

```json
{
  "files_processed": 0,
  "entities_count": 0,
  "calls_edges_count": 0,
  "chunks_count": 0,
  "oversized_chunks_count": 0,
  "documents_edges_count": 0,
  "documented_entity_ratio": 0.0,
  "chunks_with_documents_edges_ratio": 0.0,
  "ambiguous_DOCUMENTS_edges": 0,
  "unlinked_chunks_count": 0
}
```

Same report shape feeds into the [health metrics](#health-metrics) the maintenance loop watches.

---

## Health metrics

Per Invariant 5 (repair as first-class), the system's health is measurable. Metrics are emitted during ingestion (`analyze_repository`), maintenance jobs (`hygiene_queue` runs), and on-demand (`mesial health --repo X`).

| Metric | What it measures | When emitted | Read by |
|---|---|---|---|
| `documented_entity_ratio` | fraction of `:CodeEntity` with ≥1 `:DOCUMENTS` incoming | ingestion + on-demand | coverage queue, `surface` confidence |
| `chunk_anchor_stability_rate` | fraction of `:Chunk` whose `content_hash` matched after re-ingest | ingestion (re-runs only) | `reanchor` decisions |
| `observations_reanchored_confidently` | count of observations re-anchored with high confidence after last `reanchor` | post-`reanchor` | maintenance dashboard |
| `facts_with_valid_evidence` | count of `:Fact` where evidence verification passed | `verify_evidence` runs | trust signal for `verify` reports |
| `facts_verified_recently` | count of `:Fact` with `last_verified_at` within N days | scheduled | staleness queue size |
| `chunks_without_observations` | count of `:Chunk` with no incoming `:MOTIVATES` | scheduled / on-demand | coverage queue |
| `entities_without_docs` | count of `:CodeEntity` with no incoming `:DOCUMENTS` | scheduled / on-demand | coverage queue |
| `ambiguous_DOCUMENTS_edges` | count of chunks with `:DOCUMENTS` edges to multiple entities sharing the same name | linker pass | accuracy signal for `surface` |
| `stale_doc_candidates` | count of `:Chunk` whose `:DOCUMENTS` target's source has changed since `last_distilled_at` | scheduled / on-merge | hygiene queue, Moment 5 |

Aggregated as a single JSON document; trends watched over time. Without these, "is mesial working?" devolves into vibes.

---

## Evaluation hook

Lightweight fixtures, not a research benchmark. Just enough to prevent vibes-driven iteration when tuning similarity, structural boosts, or re-anchoring confidence.

A `mesial/eval/` directory (or equivalent) carries:

```
golden_surface_queries/
  <task_prompt>.expected.json   // expected chunk_ids, entity_ids, observation_ids returned by surface()
golden_impact_queries/
  <entity_id>.expected.json     // expected dependent_set
golden_stale_doc_cases/
  <repo_snapshot>.expected.json // expected stale_doc_candidates after a known doc change
golden_reanchor_cases/
  <chunk_change>.expected.json  // expected remap with confidence
golden_contradictions/
  <fact_pair>.expected.json     // expected verify_logical findings
```

Five categories, one fixture each at minimum. CI runs them on PRs that touch:
- The clustering algorithm
- The similarity function
- The linker
- `reanchor`
- `verify` (any scope)

Expectations drift over time as the model improves; updates require explicit fixture changes (visible in PRs, reviewable). The point is not to lock in current behavior but to make changes deliberate.

---

## Final summary

Mesial's lifecycle has three temporal phases, in order of increasing leverage:

**Substrate construction** (Sections "Substrate construction" 1–3, mostly mechanical, mostly already implemented). A repository goes from nothing to a graph containing its full perceptual + structural state. This is necessary infrastructure; without it, nothing else works, but having it alone is just a fancy index. `:GraphMeta` carries `schema_version` and `embedding_model_version` for migration safety. Each ingestion emits a health report, not just a "completed" flag.

**Online use** (the front-loaded section). The everyday loop: agent does work, mesial provides context (read) and accumulates memory (write). Six backbone primitives (`surface`, `propose_then_confirm`, `impact`, `verify`, `hygiene_queue`, `reanchor`) compose into six **agent moments** (Task starts, File opened, Symbol edited, Unexpected behavior found, Before commit/PR, Graph drift detected). The agent skill follows a **trigger policy** that maps each moment to a primitive call — turning the lifecycle from "scenarios" into an operating manual. `surface`'s response shape is versioned for evolution from day one. Anchor stability is treated as load-bearing; `reanchor` is promoted to a backbone primitive. Diff events (CodeEntityChanged, ChunkMoved, etc.) drive maintenance event-first, scheduled second.

**Memory population** happens through eight strategies, with **work-driven observation capture as the primary path** (per Invariant 3). The bootstrap interview is one strategy among many — useful for initial population on mature undocumented repos, but not the daily mode. Other strategies (PR/diff-driven extraction, test/runtime ingestion via Issue #9, commit-message ingestion, protocol mining via Issue #6, assertion extraction, query-driven memory) each populate the graph from a different angle. Every strategy passes through `propose_then_confirm` so the user is always the commit gate. Verification is **scoped** (logical, structural, evidence, anchor, coverage) — separate checks with separate signals; forward chaining is deferred until fact density justifies it.

Five **operational invariants** govern the design:
1. Durable claims are evidence-backed.
2. Anchors are repairable.
3. Online use is the primary write path.
4. Verification is scoped.
5. Repair is a first-class lifecycle phase.

Three **architectural commitments** make the invariants enforceable:
1. **Layer separation** — perceptual (chunks, observations, embedded) vs. conceptual (entities, facts, structurally queryable) — keeps each layer's access pattern clean and lets them compose without representational duplication.
2. **No-orphan-facts** — facts only enter the graph through distillation with at least one supporting observation — keeps the conceptual layer grounded in evidence.
3. **MCP elicitation as the universal commit gate** — the user is the commit gate for observations, facts, derivations, contradiction resolutions, re-anchoring decisions — keeps the system trustworthy without requiring the LLM to be infallible.

Three **open design issues**, with different blast radii — not equally blocking:
- [Issue #8](https://github.com/mknw/mesial/issues/8) — Anchor stability and re-anchoring. Broadly load-bearing: it gates `reanchor`, a **Tier-1** backbone primitive, so it must land before any Tier-1 memory work ships.
- [Issue #6](https://github.com/mknw/mesial/issues/6) — `:Protocol` schema (procedural memory). Gates only its own Tier-3 feature (`:Protocol` ingestion/consumption); doesn't block fact generation, `verify`, or anything in Tier 1–2.
- [Issue #9](https://github.com/mknw/mesial/issues/9) — Test and runtime trace ingestion. Same scope as #6 — gates only `:Test`/`:Failure` ingestion, nothing upstream of it.

The right next step after this document is to design the **Tier 1 MCP tools** — `surface` (MVP shape), `add_observation` runtime capture, `impact`, and `reanchor` — and ship them. With those in place, the agent's online-use loop closes and every commit cycle becomes a memory deposit.

The most leveraged near-term outcome remains: **a coding agent starts a task, gets the right context, changes code, updates the graph, and leaves the next agent smarter than it was.** Everything in this lifecycle serves that outcome.
