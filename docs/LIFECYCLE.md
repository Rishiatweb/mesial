# Lifecycle

How a repository moves through mesial — from empty graph, through ingestion, through observation and fact creation, into ongoing online use. This document is **prescriptive about the design** but only the layers up through `:Fact` are implemented today; sections 5–9 describe the MCP surface and inference engine that follow PR #5 (the memory layer foundation).

Companion to [`DESIGN.md`](DESIGN.md) (the conceptual model) and [`ARCHITECTURE.md`](ARCHITECTURE.md) (the engineering view). This doc is about *time* — what happens when, in what order, and by whose action.

---

## 1. Initialization

The point at which mesial knows *of* a repository but not yet anything about it.

### Empty repository (rare in practice)

A new project with no code and no docs. Initialization writes only the singleton:

```
h9s-cli memory init --repo {name}
```

This creates `(:GraphMeta {kind, strict})` with the appropriate `kind`:
- `code` for software repositories
- `notes` for vault-style markdown collections (Obsidian, etc.)
- `memory` for the global cross-cutting graph

It also creates the indexes the corresponding kind will need (vector index on `:Chunk` / `:Observation`, range indexes on `:Fact` for `code` and `memory`; tag indexes for `notes`).

### Implicit initialization on first ingest

For the typical case (a repo with existing code or docs), `analyze_repository` and `ingest_documents` create the graph implicitly: if the per-repo graph doesn't exist, FalkorDB creates it on first write, and the ingest pipeline writes `:GraphMeta` as part of its setup.

The explicit `memory init` only needs to be called when:
- Bootstrapping the global `memory` graph (no analyze pass to trigger it implicitly)
- Asserting the kind upfront (e.g., to claim a graph as `notes` when it would otherwise be inferred as `code` from a `.git` ancestor)
- Re-affirming `strict` after changing the policy

### What's *not* in the graph yet

After initialization (without ingestion), the graph contains exactly one node (`:GraphMeta`). No code entities, no chunks, no observations, no facts. The agent can write directly to it via the memory MCP tools (Section 5+) — useful for `memory`-kind graphs that have no source material to ingest.

---

## 2. Onboarding from already-started repos

The dominant case: a real repository with existing TypeScript code and markdown docs, never seen by mesial before. One command:

```
h9s-cli analyze --path /path/to/repo
```

The orchestrator runs four passes against a graph named after the `.git` ancestor:

1. **Tree-sitter pass** — walks the source tree, parses each `.ts`/`.tsx` file, emits `:File`, `:Class`, `:Function`, `:Method`, `:Constructor`, `:Interface`, `:Enum` nodes plus `:DEFINES` edges (file → entity, class → method, etc.).
2. **LSP pass** — opens `typescript-language-server` against the same tree, resolves cross-references: `:CALLS`, `:EXTENDS`, `:IMPLEMENTS`, `:RETURNS`, `:PARAMETERS`.
3. **Doc ingestion pass** — walks the same tree for `.md` files, chunks each by heading boundary, embeds each chunk (Qwen3, 512-dim Matryoshka), creates `:Chunk` nodes with `:OF_FILE` edges.
4. **Doc-to-code linker pass** — scans every chunk for identifier mentions (backtick-fenced or PascalCase/snake_case bare tokens) that match a known `:Searchable` name, emits `:DOCUMENTS` edges.

After this completes the graph contains the full **perceptual + structural** state of the repo as it stands today. No observations, no facts. The agent has a navigable map but no opinions yet.

### Re-onboarding (idempotent)

`analyze_repository` is safe to re-run. Code entities are MERGE'd by `(name, path, src_start, src_end)` — moved code re-adds, deleted code is left as a stale node (cleanup is a future concern). Doc chunks are deleted-and-recreated per source file (`DETACH DELETE` on the source path before re-chunking), so all `:Chunk` IDs change on each run; downstream `:MOTIVATES` edges that pointed at the old chunks are dropped along with them. **Implication for online use:** observations should re-link to chunks after a doc edit, ideally automatically.

---

## 3. Ingestion (mechanical)

Already implemented. Brief enumeration:

| Asset | Pipeline | Output |
|---|---|---|
| TypeScript files | tree-sitter + LSP | `:File`, `:Class`, `:Function`, `:Method`, `:Constructor`, `:Interface`, `:Enum` + `:DEFINES`/`:CALLS`/`:EXTENDS`/`:IMPLEMENTS`/`:RETURNS`/`:PARAMETERS` |
| Markdown files | heading chunker + Qwen3 embedder | `:Chunk` (with vector or `oversized=true`) + `:OF_FILE` |
| Identifier mentions in chunks | `internal/doclinker` regex scanner | `:DOCUMENTS` edges (chunk → code entity) |

This is the deterministic side of mesial — same inputs, same outputs, no LLM in the loop. Sections 4 onward bring the LLM into the pipeline as the source of *interpretation*.

---

## 4. Observation creation: the "interview"

Observations don't fall out of files mechanically. They're produced by an LLM reading chunks and writing what it notices. The naive approach — show the agent one chunk and ask "what do you observe?" — produces shallow, locally-scoped observations. The interview approach forces global thinking by presenting **groups of related chunks** and asking the agent to write observations that span them.

### Grouping strategy

Three signals available, used in priority order:

1. **Vector similarity (KNN over chunk embeddings).** Surfaces chunks that talk about overlapping topics regardless of file location. Use as the primary clustering signal.
2. **Shared `:DOCUMENTS` targets.** Chunks that document the same code entity belong together — they're describing the same subject from different angles. Use as a tie-breaker / merge signal on top of KNN clusters.
3. **Breadcrumb prefix locality.** Chunks from the same heading section in the same file. Use as a fallback when neither KNN nor `:DOCUMENTS` produces a meaningful group (e.g., for files with no code references).

### Group size

The constraint is the agent's context window. A group of chunks plus the agent's reply (the observations) plus any system prompts must fit. Targeting:

- **Per group: 2–5 chunks**, totalling roughly 4,000–6,000 tokens of chunk content.
- **At one extreme**, very dense chunks (long technical docs) → 2 chunks per group.
- **At the other**, short FAQ-style chunks → 5 per group.

The interview orchestrator computes group sizes by accumulating chunk char counts into a budget of ~24 KB per group (≈ 6 KB tokens), then forms groups via greedy KNN-walk: start with seed chunk, add KNN neighbors until budget exceeded, emit group, repeat.

### How many observations per group

Function of chunk content density. The orchestrator suggests a *target* and lets the agent decide:

```
target_observations = round(total_group_chars / 1200)
```

So a group of 5 × 1500-char chunks (= 7500 chars) → suggest ~6 observations. The agent may produce fewer (group is sparse) or more (group is dense). A floor of 1 and a ceiling of `2 × target` keeps it bounded. The example you raised — 5 chunks → ~20 observations — corresponds to dense chunks (~1200 chars per observation × 20 = 24 KB of source content), which fits at the upper end of the budget.

### Granularity tunable

The observations-per-chunk ratio is exposed as a setting (0=terse, 5=balanced, 10=verbose). Default 5. Higher granularity means smaller observations (each more focused), lower means broader observations (each spanning more material). The orchestrator scales the `target_observations` formula by `granularity / 5`.

### What an observation looks like

A single sentence (rarely two), high signal-to-noise, anchored to one or more chunks via `:MOTIVATES`. Examples:

- "The doc-to-code linker excludes `:File` from `:DOCUMENTS` targets because file-level mentions are too broad to be useful." (motivates 1 chunk)
- "All MCP tools follow the same handler signature `func(ctx, req, Input) (*Result, any, error)` from the modelcontextprotocol/go-sdk." (motivates 2 chunks: one in cmd/ingest/main.go-related doc, one in ARCHITECTURE.md)

The interview is best run **after all docs have been ingested** so the KNN clusters are well-formed. Running it incrementally (per-file as docs land) loses the global perspective the grouping is designed to capture.

---

## 5. Observation creation phase: user-in-the-loop via MCP

The interview generates *proposed* observations. They don't enter the graph until the user confirms. This is enforced via the MCP **elicitation** mechanism (the spec's standard way for a server to request human input through the client).

### Tool flow per group

```
propose_observations(repo, chunk_ids[], granularity?)
  → returns { session_id, proposed: [ {text, motivates_chunk_ids[]} ] }
```

The MCP server runs the LLM call internally (via sampling, in clients that support it) or formulates a prompt for the agent to execute and pass back. Either way, it returns the proposed observations to the client.

The client **renders the proposals to the user** (this is the surface the agent doesn't get to bypass). User options per item:
- **Accept as-is**
- **Edit** the text or change which chunks it motivates
- **Reject**
- **Add freeform note** (becomes a new observation in the same batch)

### Confirmation submit

```
confirm_observations(session_id, accepted: [ {text, motivates_chunk_ids[]} ], rejected_indexes: [...])
  → returns { observation_ids: [...], motivates_edges_created: int }
```

Server side:
- For each accepted observation: `AddObservation(text)` → `LinkObservationMotivatesChunk(obs_id, chunk_id)` for each motivated chunk
- Returns the new IDs so the agent can refer to them in subsequent calls

This is `batch_observations` in disguise — the batch is exactly the user-confirmed set from one interview round.

### Loop until exhaustion

The interview iterates: present a group, get confirmed observations, present the next group, etc. The orchestrator skips groups whose chunks are *already* well-covered (`last_distilled_at` recent) so re-runs don't duplicate effort.

### Session lifecycle

`session_id` is in-memory state in the ingest server, TTL ~10 minutes. If the user takes longer to review, the session expires and a fresh `propose_observations` call is needed (the orchestrator can resume from the chunk where it left off via `last_distilled_at`).

### What this gives us

By the end of the observation phase, every chunk that the agent + user thought worth covering has 1–N observations motivating it. Chunks that *no* observation motivated remain `:MOTIVATES`-incoming-empty — flagged as unverified material (the rule from `ARCHITECTURE.md`: a chunk without observations is unverified content).

---

## 6. Opt-in: "generate facts from these observations?"

Before fact generation begins, the user is asked. This gate exists for two reasons:
- Fact generation is a deeper LLM commitment (more groups, more triplets, more review work).
- The user may want to stop at observations only — observations alone are useful for embedding-based recall and don't require the structural overhead of facts.

```
prompt_fact_generation(repo)
  → elicits {generate: bool, reason?}
```

If the user declines, the lifecycle parks at the observation layer. Re-prompted next time they explicitly invoke a fact-generation flow. If they accept, the orchestrator proceeds to Section 7.

The framing of the prompt matters: "Generate facts from these observations? This enables the inference engine and contradiction detection." Sets the right expectations — facts unlock logical reasoning the embedding layer can't do.

---

## 7. Fact generation flow

Same multi-turn pattern as observations, one layer up.

### Clustering observations

Group observations using the same KNN-over-vector approach used for chunks in Section 4. Each cluster represents a *theme* — observations that together describe one aspect of the system.

- Per cluster: 5–15 observations (smaller than chunk groups since observations are denser per byte).
- Aim for clusters where the centroid distance is tight — loose clusters yield noisy facts.

### Per-cluster proposal

```
propose_facts(repo, observation_ids[], target_count?)
  → returns {
      session_id,
      proposed: [
        { subject, predicate, object, evidence_obs_ids[] }
      ]
    }
```

The LLM is given:
- The observation contents in the cluster
- The kernel predicates list (`is_a`, `subtype_of`, `part_of`, `equivalent_to`, `incompatible_with`, `causes`, `requires`)
- A note that open-set predicates are allowed but inert to the inference engine

The LLM proposes triplets that compress the cluster's content. Each proposed fact carries `evidence_obs_ids` — which observations in the cluster back it (enforces no-orphan-facts upstream).

### User confirmation

Same MCP elicitation pattern as observations. Per-fact:
- Accept
- Edit subject/predicate/object
- Reject
- Re-attribute evidence (change which observations back this fact)

### Submit

```
confirm_facts(session_id, accepted: [...], rejected_indexes: [...])
  → returns { fact_ids: [...], evidence_edges_created: int }
```

Server side: `CreateFactFromObservation` for each accepted fact, with the listed `evidence_obs_ids` linked via `:EVIDENCE_FOR`.

### Loop

Repeat for each cluster until all observations have been considered. Observations that didn't fit any proposed fact remain unattached on the fact layer (which is fine — they may be too specific, too unique, or rejected by the user).

### What this gives us

By the end of fact generation, the memory graph has a structured **conceptual layer** (facts) connected to a **perceptual layer** (observations and chunks) via well-defined edges. The graph is now ready for inference.

---

## 8. Inference engine first pass: `dlpfc`

Once facts exist, the inference engine (provisionally named `dlpfc` after the dorsolateral prefrontal cortex — classical deductive reasoning) runs a verification pass to check the ontology's consistency against the underlying code.

Five complementary strategies, each surfacing a different class of issue:

### Strategy A: Reverse anchoring

For each fact, walk evidence backward to ground:

```
:Fact → :EVIDENCE_FOR ← :Observation → :MOTIVATES → :Chunk → :DOCUMENTS → :CodeEntity
```

If the fact mentions a subject/object that maps to a `:CodeEntity` name, check the entity exists. If a fact says `(EventViewImpl, is_a, Class)` but no `:Class {name:'EventViewImpl'}` exists in the code graph, surface as **stale-fact**. Likely a renamed or removed entity.

### Strategy B: Triplet validation against code structure

For triplets where both subject and object map to known code entities, check the relationship in the code graph. Mappings:

| Triplet | Code-graph check |
|---|---|
| `(X, subtype_of, Y)` where X, Y are classes | `MATCH (X)-[:EXTENDS]->(Y)` |
| `(X, subtype_of, Y)` where Y is interface | `MATCH (X)-[:IMPLEMENTS]->(Y)` |
| `(X, part_of, Y)` where X is method, Y is class | `MATCH (Y)-[:DEFINES]->(X)` |
| `(X, requires, Y)` where both are functions | `MATCH (X)-[:CALLS]->(Y)` (transitive over k hops) |

Mismatch surfaced as **claim-without-structural-backing**. Either the code changed (fact stale) or the fact was wrong (re-verify).

### Strategy C: Forward chaining over axiom edges

Apply the reserved axiom edges (`:ENTAILS`, `:INSTANCE_OF`, `:SUBTYPE_OF`, `:Rule` reification) to derive new facts. Example:

- Existing facts: `(Method, subtype_of, CodeEntity)` and `(getPullRequest, is_a, Method)`
- Derivable: `(getPullRequest, is_a, CodeEntity)`

Surface derivations as **candidate facts** for the user to ratify. Doesn't auto-write — keeps the no-orphan-facts invariant (a derived fact has no observation backing it; promoting requires the user to confirm and the engine generates a synthetic observation citing the derivation).

### Strategy D: Contradiction scan

For predicates with functional semantics (each subject has at most one object), look for siblings that conflict:

```cypher
MATCH (f1:Fact {subject:$s, predicate:'is_a'})
MATCH (f2:Fact {subject:$s, predicate:'is_a'})
WHERE f1.object <> f2.object
RETURN f1, f2
```

Same for `equivalent_to` (transitive symmetric — partition violation) and pairs related via `incompatible_with` (X incompatible_with Y, but a fact also asserts X requires Y).

Surface as **contradiction**. Rule from `ARCHITECTURE.md`: contradictions get *resolved immediately*, not stored — the engine surfaces them; the user decides which fact stands.

### Strategy E: Coverage-gap detection

Surface absences:

- `:CodeEntity` nodes with no `:DOCUMENTS` incoming → undocumented code
- `:Chunk` with no `:MOTIVATES` incoming → unverified docs (per the rule)
- `:Observation` with no `:EVIDENCE_FOR` outgoing → episodic-only, may be pruning candidate
- `:Fact` whose subject doesn't appear in any backing observation's text → spurious fact

These aren't logical errors but health signals. Drive maintenance queues.

### Output format

The first pass returns a structured report:

```
{
  "stale_facts": [...],        // strategy A
  "unsupported_claims": [...], // strategy B
  "candidate_derivations": [...], // strategy C
  "contradictions": [...],     // strategy D
  "coverage_gaps": {...}       // strategy E
}
```

The agent walks each section with the user, applying corrections via the same `confirm_facts` flow (accept candidates, retract stale facts, resolve contradictions).

---

## 9. Online lifecycle

The phases above (1–8) are the **bootstrap**. Online lifecycle is what happens during normal agent work, when an agent is solving a task and mesial is in the loop.

This is the largest section and the most consequential — most of mesial's value comes from being a constant background utility, not a one-time setup pass.

### 9.1 Conceptual frame

Three modes of mesial usage during work:

- **Semantic mode** — vector search over chunks/observations to find relevant material. "What does the codebase say about X?"
- **Logical mode** — structural queries over the code graph + inference over facts. "What are the consequences of changing X?"
- **Combined mode** — semantic to find candidates, logical to validate or expand. "Find code likely related to X, then check whether changing it would break invariants."

The interesting questions are mostly *combined*. Semantic alone is grep-with-vibes; logical alone is grep-with-structure. The combination is what justifies the architecture.

### 9.2 Scenarios

Brainstormed exhaustively, organized by category. Each scenario names: what triggers it, what mesial does, what the agent does with the result.

#### A. Reactive context loading

**A1. Task initiation**
- *Trigger*: agent receives task ("fix the auth bug")
- *Mesial*: vector search the task description over chunks → top-K → traverse `:DOCUMENTS` to entities → traverse `:MOTIVATES` to relevant observations → traverse `:EVIDENCE_FOR` to relevant facts
- *Agent gets*: a focused subgraph (chunks + entities + observations + facts) loaded as initial context, replacing 80% of the "where do I start?" exploration

**A2. Reading a new file**
- *Trigger*: agent opens file F that it hasn't seen this session
- *Mesial*: lookup `:File {path: F}` → traverse `:DEFINES` to entities → traverse `:CALLS`/`:EXTENDS`/`:IMPLEMENTS` to neighbors → for each entity, find documenting chunks + their observations
- *Agent gets*: structured tour: "this file defines X (extends Y, called by Z); X is documented in chunk Q which says ..."

**A3. Following a stack trace**
- *Trigger*: error message mentions function F
- *Mesial*: `:Function {name: F}` → traverse incoming `:CALLS` (callers) and `:DOCUMENTS` (descriptions) → observations about F
- *Agent gets*: caller context + documented behavior + episodic notes (e.g., "X has been observed to fail under condition Y")

#### B. Authoring assistance

**B1. Convention checking before writing**
- *Trigger*: agent about to add a new function/method/class
- *Mesial*: vector search proposed name + signature over existing code-related observations and facts
- *Agent gets*: similar prior code, conventions, naming patterns
- *Combined-mode*: facts like `(Service classes, requires, dependency injection via constructor)` surfaced if relevant

**B2. Refactoring blast radius**
- *Trigger*: agent considering changing function/class signature, removing entity, etc.
- *Mesial*: from target entity, traverse outward:
  - Incoming `:CALLS` → callers (must update)
  - `:DOCUMENTS` → chunks documenting it (must update if behavior changes)
  - `:EVIDENCE_FOR` → observations mentioning it (need re-verification)
  - `:Fact` triplets where subject or object equals target → facts that may invalidate
- *Agent gets*: a checklist of impact sites stratified by type
- *Logical extension*: dlpfc runs strategy B (triplet validation) post-change to flag broken claims

**B3. Hypothesized change validation**
- *Trigger*: agent: "what if EventViewImpl implemented Cache instead of Storage?"
- *Mesial (semantic)*: search docs for prose mentioning EventViewImpl + Storage relationship
- *Mesial (logical via dlpfc)*: facts about EventViewImpl — derive consequences via forward chaining; check if change implies contradictions
- *Agent gets*: combined report — docs to update + logical inconsistencies surfaced

**B4. Similar-code lookup**
- *Trigger*: agent has a problem to solve, suspects a similar pattern exists
- *Mesial*: vector search problem description over docs + observations → DOCUMENTS targets in code → "have we done this before?"
- *Agent gets*: pointers to existing implementations + observations about how they work

#### C. Investigation & debugging

**C1. Bug investigation**
- *Trigger*: bug report or test failure
- *Mesial (semantic)*: search bug description over observations → past episodic notes about similar issues
- *Mesial (logical)*: facts about invariants of the involved functions — does any fact say "X never returns null"? If so, the invariant has been broken (or was wrong)
- *Agent gets*: prior occurrences + claimed invariants to test

**C2. Performance investigation**
- *Trigger*: "this endpoint is slow"
- *Mesial (semantic)*: past observations about performance issues
- *Mesial (logical)*: walk `:CALLS` chain from the endpoint → for each function in the chain, check facts about its performance characteristics
- *Agent gets*: prioritized list of investigation candidates with prior knowledge attached

**C3. Decision archaeology**
- *Trigger*: "why was X done this way?"
- *Mesial*: semantic search across observations → episodic facts about the decision
- *Agent gets*: surfaced rationales from past sessions, instead of guessing

#### D. Review & quality

**D1. PR review**
- *Trigger*: agent reviewing a diff
- *Mesial*: for each changed entity, find documenting chunks → check if docs match new behavior
- *Mesial (logical)*: for each new function, propose facts and check against existing facts (surface incoming changes that contradict invariants)
- *Agent gets*: review comments grounded in known context, not just style

**D2. Architecture compliance**
- *Trigger*: rule encoded as fact, e.g. `(data layer, incompatible_with, UI component imports)`
- *Mesial (logical via dlpfc)*: scan PR for new edges that violate the constraint
- *Agent gets*: explicit violation report — "this PR adds an import from data → UI which is forbidden"

**D3. API surface stability**
- *Trigger*: PR changes a function signature
- *Mesial*: lookup facts about the function — `(X, is_a, public API)`, `(X, is_a, stable interface)`
- *Mesial (logical)*: contradiction surfaced — changing a stable interface
- *Agent gets*: backward-compatibility flag at review time

**D4. Test discovery**
- *Trigger*: agent making changes to function F
- *Mesial*: incoming `:CALLS` to F where the file path matches `/test/` → tests exercising F
- *Agent gets*: focused test list (run/update these)

#### E. Documentation flow

**E1. Documentation generation**
- *Trigger*: "document function F"
- *Mesial*: find related entities via `:CALLS`/`:DOCUMENTS` → existing chunks for style examples → facts about F to include in the doc
- *Agent gets*: structured starting point for the doc, not a blank page

**E2. Stale doc detection**
- *Trigger*: scheduled or post-merge
- *Mesial*: chunks documenting code entities that have changed (compare `:DOCUMENTS`-targets' `src_start`/`src_end` against current source) → mark for review
- *Agent gets*: list of doc sections to refresh

**E3. Migration planning**
- *Trigger*: "we're moving from library A to library B"
- *Mesial (semantic)*: search "library A" usage over chunks
- *Mesial (logical)*: code entities that import/use A's APIs; facts about A's behavior to port to B
- *Agent gets*: structured migration list with priorities

#### F. Memory hygiene (background / scheduled)

**F1. Embedding refresh on doc change**
- *Trigger*: file change watcher detects `.md` edit
- *Mesial*: re-chunk → re-embed only changed chunks → keep existing chunk IDs where boundary unchanged, drop+recreate where changed
- *Side effect*: `:MOTIVATES` edges to dropped chunks are gone — affected observations need re-linking

**F2. Embedding model upgrade**
- *Trigger*: switching to a new embedding model
- *Mesial*: full background re-embed; may need vector index rebuild if dimension changes
- *Strategy*: do per-graph, in batches; keep old index live until new one is fully populated, then atomic swap

**F3. Observation aging**
- *Schedule*: periodic
- *Mesial*: `MATCH (o:Observation) WHERE NOT (o)-[:EVIDENCE_FOR]->() AND o.created_at < timestamp() - N` → prune candidates
- *Agent action*: review and confirm prune

**F4. Fact verification staleness**
- *Schedule*: periodic
- *Mesial*: `MATCH (f:Fact) WHERE f.last_verified_at < timestamp() - N` → re-verification queue
- *Agent action*: re-validate against current code via dlpfc strategy B

**F5. Conflict surfacing**
- *Schedule*: post-fact-creation, periodic
- *Mesial*: dlpfc strategy D scans for contradictions
- *Agent action*: resolve at runtime, never store the conflict

#### G. Knowledge accumulation (during work)

**G1. Runtime observation capture**
- *Trigger*: agent notices something during work ("function X silently truncates inputs > 1024")
- *Mesial*: agent calls `add_observation` (single, no group context) → KNN against existing observations → surfaces neighbors and any related facts
- *Agent gets*: chance to refine the observation before commit; user-in-the-loop confirms

**G2. Cross-task transfer**
- *Trigger*: task B touches code that task A previously made observations about
- *Mesial*: A's observations are persisted in the repo's memory; A2-style context loading surfaces them automatically when B reads the file
- *Result*: don't repeat past mistakes (the canonical example: "remember that X is fragile under Y conditions")

**G3. Self-monitoring**
- *Trigger*: mesial keeps observations about itself (the user prefers MOTIVATES over GROUNDED_IN, etc.)
- *Mesial*: observations in the global `memory` graph apply to mesial's own development
- *Result*: meta-loop where mesial improves its own use over time

#### H. Onboarding & explanation

**H1. Architecture explanation**
- *Trigger*: "how does the auth flow work?"
- *Mesial (semantic)*: search "auth flow" over chunks
- *Mesial (logical)*: traverse `:CALLS` from entry point to build a flow
- *Mesial (logical)*: facts about auth (`AuthService is_a SecurityComponent`, `Login requires AuthService`) for relationship summary
- *Agent gets*: structured walk-through, not a file listing

**H2. New contributor onboarding**
- *Trigger*: someone new asking high-level questions
- *Combined*: semantic for prose, logical for structure, facts for invariants
- *Agent assembles* a tailored intro from the graph

#### I. Specialized analyses

**I1. Security audit**
- *Trigger*: scheduled audit or specific concern
- *Mesial*: search observations/facts mentioning auth, crypto, input validation → cross-reference with code touching these areas
- *Logical*: facts about security invariants — verify against current code

**I2. Feature flag mapping**
- *Trigger*: planning to remove flag F
- *Mesial*: facts and observations referencing F → cleanup checklist

**I3. Dead-code detection**
- *Trigger*: cleanup pass
- *Mesial*: code entities with no incoming `:CALLS` AND no incoming `:DOCUMENTS` → likely dead
- *Logical extension*: check facts mentioning the entity — facts about it suggest it's still relevant; lack of facts is corroborating evidence for "dead"

### 9.3 Patterns and regularities

Across the scenarios, six patterns recur:

#### Pattern 1: Semantic-then-traverse (the workhorse)

`vector search → graph traversal → context assembly`

Used in: A1, A2, A3, B1, B4, C1, C2, C3, D1, E1, E3, H1, H2.

Almost every reactive operation starts with vector search to identify candidates, then traverses graph edges to build out the relevant subgraph. This is the **spatial navigation** promise of mesial — agents don't grep, they walk.

#### Pattern 2: Triplet-against-code (the bridge)

`fact triplet → resolve subject/object to code entities → check relation in code graph`

Used in: B2, B3, C1, D2, D3, I1.

The bridge between semantic and logical. Facts make claims about the world; code is ground truth. Mismatch is the engine's signal.

#### Pattern 3: KNN-then-cluster-then-confirm (the human-in-loop primitive)

`group similar items → present to user → batch commit confirmed`

Used in: Sections 4–7 (interview, fact gen), G1 (runtime obs).

Whenever the LLM is generating durable state, this pattern surfaces it for review before commit. The most reusable interaction primitive.

#### Pattern 4: Edge-traversal-as-impact (the change-analysis primitive)

`from changed entity → traverse incoming edges → enumerate dependents`

Used in: B2 (refactoring), D1 (PR review), D2 (compliance), D4 (test discovery), E2 (stale docs), E3 (migration), F1 (embed refresh), I3 (dead code).

Almost any "what depends on X?" question reduces to this. The graph schema's edge density determines the depth of analysis available.

#### Pattern 5: Time-driven maintenance queues (the hygiene primitive)

`property timestamp + age threshold → review queue`

Used in: F1, F3, F4 (memory hygiene), E2 (stale docs).

Every node carries lifecycle timestamps; background jobs convert those into work queues. Without these, the graph rots.

#### Pattern 6: Combined semantic + logical (the most powerful)

`semantic to find candidates → logical to validate or expand`

Used in: B3, C1, C2, D1, D3, H1, H2, I1.

The combinations are where mesial earns its keep. Either alone is decent; together they're qualitatively different.

### 9.4 Backbone design patterns

The patterns above suggest five **backbone** primitives — interfaces the rest of the system composes from:

#### 1. `surface(query, depth) → context_subgraph`
The semantic-then-traverse implementation. Inputs: free-text query, traversal depth (chunks only, +entities, +facts). Output: a slice of the graph as a JSON object the agent can consume directly. **Used by**: every reactive load. **Implements**: Pattern 1.

#### 2. `propose_then_confirm(items, action_kind) → committed_ids`
Generic multi-turn confirm flow. Wraps MCP elicitation. **Used by**: observation interview, fact generation, runtime obs capture, conflict resolution. **Implements**: Pattern 3.

#### 3. `impact(entity, kinds) → dependent_set`
Edge-traversal walker, parameterized by which edge types to follow. **Used by**: refactoring, PR review, compliance, dead-code, test discovery. **Implements**: Pattern 4.

#### 4. `verify(fact_ids, strategies) → report`
Inference engine entry point, runs the strategies from Section 8 against a fact set. **Used by**: dlpfc passes, PR review, post-fact-creation. **Implements**: Pattern 2 + parts of 6.

#### 5. `hygiene_queue(kind) → items`
Time-and-state-driven query producer. **Used by**: F1–F5, E2. **Implements**: Pattern 5.

These five primitives cover most of the scenarios. Building them well means most scenarios fall out as compositions.

### 9.5 Prioritization

Tier 1 — **must-have for "useful agent"** (immediate ROI, every task benefits):
- Primitive 1 (`surface`) — Pattern 1 — enables A1, A2, A3, B1, B4, H1
- Primitive 2 (`propose_then_confirm`) for observation creation — Sections 4–5 — enables runtime observation capture
- The MCP tools that wrap these into a usable surface

Tier 2 — **force-multipliers** (build on Tier 1's accumulated memory):
- Primitive 3 (`impact`) — Pattern 4 — enables B2, D1, D4, E2
- Primitive 5 (`hygiene_queue`) — Pattern 5 — keeps the system from rotting
- Fact generation flow (Sections 6–7)

Tier 3 — **advanced reasoning** (the dlpfc payoff):
- Primitive 4 (`verify`) — Pattern 2 — Section 8 strategies
- Combined-mode scenarios B3, C1, C2, D2, D3, I1

Tier 4 — **late / specialized**:
- E1 (doc generation), E3 (migration planning)
- I1 (security audit), I2 (feature flag), I3 (dead code)
- G3 (self-monitoring meta-loops)

Tier 1 is a few months of work but unlocks daily-use value immediately. Tier 2 follows naturally and starts paying compound interest. Tier 3 needs the inference engine which is its own design pass. Tier 4 is opportunistic — built when a concrete need arises.

### 9.6 Update strategies during online use

A few cross-cutting questions raised in the request:

**Q: Auto-fetch relevant info at task start?**
Yes, via Primitive 1 — the agent skill calls `surface(task_description, depth=full)` at task initiation. Returns a subgraph the agent loads into context before doing anything else. Cost: one embedding + a few graph queries; payoff: the agent skips most exploratory grep.

**Q: Update references at task end?**
Two updates worth doing:
- For each file the agent edited, queue stale-doc detection (E2) to find chunks documenting changed entities.
- For observations the agent made during the task, enqueue fact-generation candidacy (Section 7 trigger) — observations cluster naturally per-task.

**Q: Re-process all chunk embeddings?**
Only on embedding-model upgrade (F2), not per task. Doc edits (F1) re-embed only the changed file's chunks. The vector index can be rebuilt incrementally — FalkorDB's CREATE VECTOR INDEX is idempotent, and existing nodes' vectors remain valid until re-embedded.

**Q: Hypothesized-change consistency?**
Scenario B3 — combined-mode. The agent calls `surface(change_description)` for the doc/code context, then `verify(facts_about_target_entity, strategies=[B,C,D])` to find structural inconsistencies. Returns a "would this break?" report.

**Q: Refactoring blast radius?**
Scenario B2 — `impact(target_entity, kinds=[CALLS, DOCUMENTS, EVIDENCE_FOR])`. Returns dependents grouped by kind. The agent knows: "8 callers (must update), 3 documenting chunks (review docs), 2 facts whose subject is this entity (re-verify)."

---

## Final summary

Mesial's lifecycle has three temporal phases, in order of increasing leverage:

**Bootstrap** (Sections 1–3, mostly mechanical, mostly already implemented). A repository goes from nothing to a graph containing its full perceptual + structural state: code entities, doc chunks, identifier-mention edges. This is the table stakes — without it, nothing else works, but having it alone is just a fancy index.

**Distillation** (Sections 4–8, the LLM-in-the-loop core, the work ahead). The agent and user collaborate to interpret the bootstrapped graph: read chunks in groups → propose observations → user confirms → propose facts from observation clusters → user confirms → inference engine verifies the result against ground truth. The interview pattern (groups of chunks for global thinking, multi-turn user confirmation) is the mechanism that converts dead documentation into structured, queryable knowledge. Each step is bounded by user authorization via MCP elicitation — the human is always in the commit path, even when the LLM does the proposing.

**Online use** (Section 9, the daily-driver, the value capture). With the graph populated, the agent uses it constantly — at task start to load context, during work to check conventions and impact, after work to refresh stale references. Five backbone primitives (`surface`, `propose_then_confirm`, `impact`, `verify`, `hygiene_queue`) compose into ~25 distinct scenarios spanning reactive loading, authoring, investigation, review, documentation, hygiene, and accumulation. The most powerful scenarios are *combined*: semantic search for candidates, logical inference for validation. Tier 1 (semantic + impact + observation capture) delivers daily-use value within the first wave of MCP tools; Tiers 2–4 layer on increasingly sophisticated reasoning as the memory graph matures.

Three architectural commitments make this work:
1. **Layer separation** — perceptual (chunks, observations, embedded) vs. conceptual (entities, facts, structurally queryable) — keeps each layer's access pattern clean and lets them compose without representational duplication.
2. **No-orphan-facts** — facts only enter the graph via the distillation flow with at least one supporting observation — keeps the conceptual layer grounded in evidence rather than free-floating LLM assertions.
3. **MCP elicitation everywhere LLM proposes durable state** — the user is the commit gate for observations, facts, derivations, and contradiction resolutions — keeps the system trustworthy without requiring the LLM to be infallible.

The MCP tool surface that follows this document — `surface`, `propose_observations`/`confirm_observations`, `propose_facts`/`confirm_facts`, `impact`, `verify`, plus the lower-level inspectors and search tools shipped in PR #5 — is the concrete API that makes the lifecycle executable. The design is cohesive: every tool in the surface is justified by at least two scenarios in Section 9, and the five backbone primitives factor most of the work cleanly.

The right next step after this document is to design the Tier 1 MCP tools (`surface` + the propose/confirm pair for observations) in detail and ship them. With those in place, the agent can both consume context and contribute observations during real work — and the loop closes.
