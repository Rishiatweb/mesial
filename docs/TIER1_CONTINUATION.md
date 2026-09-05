# Tier 1, Increment 2: `reanchor`, `impact`, `surface`

**Status: not yet built.** This is the continuation plan for `LIFECYCLE.md`'s Tier 1 once Increment 1 (stable identity + match-in-place ingestion — see `docs/DESIGN.md`'s stable-identity note, `docs/ARCHITECTURE.md`'s data model, `docs/IMPLEMENTATION.md` §2 item 4) has shipped and been verified. Written up now, while the design is fresh, rather than left as a bullet point — so this increment starts from a decided design instead of a cold start. Nothing here is implemented; treat every function signature and Cypher query below as a specification to build against, not existing code.

This document is unrelated to `PLAN.md` (project root) — that's a separate, larger proposal (SQLite-as-authority, bitemporal revisions) with its own adoption decision pending. This document continues the current FalkorDB-authoritative architecture that Increment 1 completes the identity story for.

## Pre-implementation spike (do first, small)

Two assumptions this increment leans on, unverified against the live stack:

1. **Vector index score sign/scale.** `db.idx.vector.queryNodes(..., similarityFunction:'cosine')` YIELDs `score`. `internal/falkorstore/store.go`'s `Search` calls it `Score` with no documented direction; `memorystore.go`'s `SearchObservations` calls the *same underlying value* `Distance` and documents "lower means closer" (confirmed correct by `TestSearchObservationsKNN`). `surface`'s `confidence.retrieval` formula (below) needs the real direction and range to normalize correctly. **Action:** query a known near/far vector pair via the existing test-harness pattern in `memorystore_test.go`, record whether the returned value increases or decreases with similarity and its observed range. Finalize the formula's sign/scale from this, not from assumption.
2. **`labels(n)` over the Go client.** Several queries below (`impact`'s dependent lookups, `surface`'s entity traversal) return `labels(e)` to recover which of `Class`/`Function`/`Method`/`Interface`/`Enum`/`Constructor` an entity is — no existing query ever calls `labels()`, since every existing query already knows the label it's asking for. **Action:** confirm `res.Record().GetByIndex(i)` deserializes a `labels()` result into a Go `[]interface{}` of strings via `falkordb-go/v2` before writing code against it. If it doesn't behave as expected, fall back to `WHERE e:Class OR e:Function OR ...` plus a per-label `RETURN` column, mirroring `FetchCodeEntities`'s existing pattern (which never needed `labels()` because it filters, not reports).

## `reanchor` — audit and on-demand re-resolution

Once Increment 1 ships, most identity resolution already happens automatically inside every `ingest_documents`/`analyze_repository` call. `reanchor` is mostly an **audit and reporting surface** over what the last ingest already resolved, plus a way to re-run that resolution on demand — e.g. after an external tool modified the graph, or to re-check sources that weren't re-ingested through the normal path.

```go
type ReanchorReport struct {
    Remapped    []RemappedChunk  `json:"remapped"`
    Ambiguous   []AmbiguousChunk `json:"ambiguous"`
    Orphaned    []OrphanedChunk  `json:"orphaned"`
    CreatedAnew []int64          `json:"created_anew"`
}

type RemappedChunk struct {
    OldChunkID    int64   `json:"old_chunk_id"`
    NewChunkID    int64   `json:"new_chunk_id"`   // always == OldChunkID -- Increment 1 never actually changes a node's ID; kept for shape compatibility with the original LIFECYCLE.md sketch, and because "resolved to the same identity" is strictly better than the ID-remap model that sketch assumed was unavoidable
    Confidence    float64 `json:"confidence"`     // 1.0 for an anchor_id match+update, 0.8 for a content_hash rename match
    EdgesReplayed int     `json:"edges_replayed"` // always 0 -- no edges ever need replaying, since the node itself never changes ID
}

type AmbiguousChunk struct {
    OldChunkID           int64   `json:"old_chunk_id"`
    CandidateNewChunkIDs []int64 `json:"candidate_new_chunk_ids"`
}

type OrphanedChunk struct {
    OldChunkID             int64   `json:"old_chunk_id"`
    ObservationIDsAffected []int64 `json:"observation_ids_affected"`
}

// Reanchor re-runs the shared match-and-classify logic for each of sources
// (or every known doc source via a new Store.ListDocSources if sources is
// empty), and translates each result into the ReanchorReport shape above.
func Reanchor(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, sources []string, strict, useVectorFallback bool) (ReanchorReport, error)
```

**Vector-similarity fallback** (the genuinely fuzzy tier, out of scope for a mechanical match): applies only to chunks that end up `Orphaned` *and* have at least one incoming `:MOTIVATES` — losing those silently would matter; a truly unused orphan doesn't need this. When `useVectorFallback` is true: embed the orphaned chunk's stored content (one extra embed call per affected chunk, bounded by how many were actually orphaned — expected rare), KNN among that source's `CreatedAnew` chunks only (not the whole graph — needs a Go-side post-filter over the existing global `Search`, since scoping the KNN query itself to "just these node IDs" isn't cheap to express; revisit with a dedicated indexed query only if usage shows the post-filter is too slow). Above a similarity threshold, add the candidate to `Ambiguous` — **never auto-relink**. Per Invariant 3 (online use is the primary write path) and the `propose_then_confirm` commit-gating pattern, `reanchor` only *surfaces* a suggestion; a human or the calling agent decides.

Register as `reanchor(repo?, path?, changed_sources?, strict?, use_vector_fallback?)`, following the exact existing MCP tool pattern (jsonschema-tagged input struct, `mcp.AddTool`, repo resolution via `pipeline.ResolveRepoGraphName`, JSON output via `json.MarshalIndent` matching `search_observations`/`search_facts`'s style since the report is structured data).

## `impact(entity_id, kinds?, include_evidence?) → dependent_set`

**Spec gap, resolved rather than silently invented:** `LIFECYCLE.md`'s example — `impact(symbol, kinds=[CALLS, DOCUMENTS, EVIDENCE_FOR, ABOUT])` — references an `ABOUT` edge that doesn't exist anywhere in the shipped schema, and `EVIDENCE_FOR` doesn't touch `:CodeEntity` at all (it's `Observation → Fact` only). Neither can be a literal incoming-edge kind on a code entity. `impact` therefore splits the concern in two:

- `kinds` accepts only the four real, literal incoming-edge kinds on a `:CodeEntity`: `CALLS`, `EXTENDS`, `IMPLEMENTS`, `DOCUMENTS`. An invalid kind (e.g. `"ABOUT"`, copied verbatim from the LIFECYCLE.md example) returns an explicit, named error — not a silent empty result.
- `include_evidence` (default `true`) toggles a separate, clearly-distinguished 2-hop composite walk: `entity ←DOCUMENTS– chunk ←MOTIVATES– observation –EVIDENCE_FOR→ fact`. This is "impact on grounded claims" — what LIFECYCLE.md was gesturing at with `EVIDENCE_FOR`/`ABOUT` — expressed against the real schema instead of edge names that don't apply to entities.

```go
type EntityRef struct {
    ID    int64
    Label string // resolved from labels(e), see the spike above
    Name  string
    Path  string
}

// FindIncomingByKind returns nodes with an incoming edge of kind into entityID.
// kind must be validated against the four real kinds by the caller before
// this runs (interpolated into Cypher -- same safety pattern already used by
// ConnectEntities's relation parameter).
func (s *Store) FindIncomingByKind(ctx context.Context, entityID int64, kind string) ([]EntityRef, error)

type EvidenceImpactRow struct {
    ChunkID, ObservationID, FactID          int64
    Subject, Predicate, Object              string
}

func (s *Store) FindEvidenceImpact(ctx context.Context, entityID int64) ([]EvidenceImpactRow, error)
// MATCH (e)<-[:DOCUMENTS]-(c:Chunk)<-[:MOTIVATES]-(o:Observation)-[:EVIDENCE_FOR]->(f:Fact)
// WHERE ID(e) = $entity_id
// RETURN ID(c), ID(o), ID(f), f.subject, f.predicate, f.object

type ImpactResult struct {
    EntityID int64                             `json:"entity_id"`
    Direct   map[string][]EntityRef            `json:"direct"`             // kind -> dependents, only requested+valid kinds present
    Evidence []EvidenceImpactRow               `json:"evidence,omitempty"` // present only if include_evidence
}

func Impact(ctx context.Context, store *falkorstore.Store, entityID int64, kinds []string, includeEvidence bool) (ImpactResult, error)
```

Kept ID-based (`entity_id`), matching the `*_id` convention every existing memory tool already uses (`observation_id`, `fact_id`, `motivates_chunk_id`) — a name+path convenience resolver could be layered on later without breaking this shape; callers that need to discover an ID first already have `FetchCodeEntities`/`LookupEntityByPosition`.

## `surface(query?, path?, depth?, max_chars?, k?) → context_subgraph`

The MVP response shape is frozen (verbatim from `LIFECYCLE.md`) — every design choice below targets this exact shape, never a superset visible at the top level:

```json
{
  "version": 1,
  "chunks": [{"id": "...", "content": "...", "source": "...", "breadcrumb": "..."}],
  "entities": [{"id": "...", "label": "...", "name": "...", "path": "..."}],
  "observations": [{"id": "...", "content": "...", "fact_ids": ["..."]}],
  "confidence": {"retrieval": 0.0, "coverage": 0.0, "staleness": 0.0}
}
```

**Query mode** (LIFECYCLE.md Moment 1 — task starts): embed `query`; `Search` (KNN, chunks) + `SearchObservations` (KNN, observations); a new `EntitiesForChunks(chunkIDs)` traversal (`MATCH (c:Chunk)-[:DOCUMENTS]->(e) WHERE ID(c) IN $chunk_ids`) for `entities[]`; the existing `FactsBackedByObservations` for each observation's `fact_ids` — project only the ID, discarding subject/predicate/object (deliberately kept out of the frozen v1 shape, reserved for the documented v2 `facts[]` field rather than duplicated early).

**Path mode** (Moment 2 — file opened): the important nuance — a `.ts` file's own `:File` node has no `:Chunk` pointed at it directly; its documentation lives in separate `.md` chunks reached only via the *reverse* `DOCUMENTS` traversal, never `OF_FILE`.
1. `FetchEntitiesByFile(path)` → direct entity set.
2. `FetchChunks(ctx, path)` (own chunks, if `path` is itself markdown) **union** a new `ChunksDocumentingEntities(path)` (`MATCH (c:Chunk)-[:DOCUMENTS]->(e:Searchable {path: $path})`) → chunk set, deduplicated.
3. A new `ObservationsForChunks(chunkIDs)` → observation set, same `fact_ids` projection as query mode.

When both `query` and `path` are given, `query` wins (task framing takes precedence — the two moments are mutually exclusive triggers in practice).

`max_chars` truncates cumulative `chunks[].content` (stop once the running total would exceed the budget). Default when unset: **8000** — a fresh, deliberately arbitrary constant, *not* reused from either existing-but-different budget in the codebase (`pipeline.OversizedChunkChars`, 6000, answers "can this be usefully embedded"; the bootstrap interview's `--budget-chars`, 24000, answers "how much fits one LLM clustering round" — neither answers "how much context should one `surface` call return").

`depth` (`"default"` | `"tour"`) only widens `k`/`max_chars` in v1 — it does not add new response fields, since the frozen shape has nowhere to put richer tour-specific data (per-entity call/extend counts, etc.); those are exactly the kind of `v2+` additions LIFECYCLE.md already reserves space for.

### Confidence formulas

- **`retrieval`**: `clamp(1 - avg(returned_score_or_distance), 0, 1)`, averaged over all chunk + observation hits returned. **Depends on the spike above** — if the live score is confirmed already-normalized similarity in `[0,1]`, the formula is just `avg(score)`; if confirmed cosine distance, use `clamp(1 - avg(distance)/2, 0, 1)`. Do not guess; record which branch was confirmed true in a code comment next to the implementation.
- **`coverage`**: `documented_entities_returned / total_entities_returned`, scoped to this call's `entities[]` only (not the whole repo). `0` if `entities[]` is empty — the conservative default; an empty result set has no coverage to claim. Needs one new tiny helper, `DocumentedEntityIDs(ids []int64) ([]int64, error)` (`MATCH (c:Chunk)-[:DOCUMENTS]->(e) WHERE ID(e) IN $ids RETURN DISTINCT ID(e)`).
- **`staleness`**: for every returned chunk whose source is a locally-readable file (best-effort `os.Stat`/`os.Open` — the same filesystem-access assumption `analyze_repository`/`ingest_documents` already make), re-run `chunking.ChunkFile` on just that source, find the chunk whose current breadcrumb matches the surfaced one, recompute `content_hash`, compare. `staleness = stale_count / measurable_count`, where unreadable/unresolvable chunks are excluded from *both* numerator and denominator (never counted as "fresh"). If `measurable_count == 0`, return `0`. **Named limitation, not hidden:** the frozen schema requires `staleness` to always be present, so `0` is the only schema-compliant fallback — it conflates "confirmed fresh" with "couldn't be measured." A natural v2 extension (not built here) would add a sibling `staleness_measured` field to disambiguate.

Register as `surface(query?, path?, depth?, max_chars?, k?, repo?)`, output via `json.MarshalIndent` (structured contract, not prose — matches `search_observations`/`search_facts`, not `search_documents`'s Markdown-report style).

## Decision log (for review, not silently resolved elsewhere)

1. `impact`'s `kinds` supports only the four real edge kinds; `EVIDENCE_FOR`/`ABOUT` handled via a separate `include_evidence` flag, not invented as literal edge names.
2. `impact` stays ID-based, not name+path — consistency with existing memory-tool conventions.
3. `surface`: `query` takes precedence over `path` when both are given.
4. `surface`'s `max_chars` default (8000) is a fresh constant, not reused from either existing-but-semantically-different budget in the codebase.
5. `surface`'s `retrieval` formula's exact sign/scale is contingent on the pre-implementation spike's empirical result — not guessed.
6. `surface`'s `staleness = 0` fallback conflates "measured fresh" with "unmeasurable" — accepted as the only schema-compliant value for v1, with a named (not built) v2 extension path.
7. `depth="tour"` only widens `k`/`max_chars` in v1; it does not add new response fields, to keep the frozen MVP shape exactly as specified.
8. `reanchor`'s scoped vector-KNN (source-restricted, not whole-graph) is a Go-side post-filter over the existing global `Search` for v1 — revisit with a dedicated indexed query only if usage shows the post-filter doesn't scale.

## Verification plan

Same confirmed-working infra as Increment 1 (Go 1.25 + 64-bit mingw-w64 for cgo, FalkorDB via Docker, `llama-server` + the already-downloaded embedding model, `typescript-language-server` via npm) — no mocking layer.

- **`reanchor`**: build a fixture with ≥2 sources; run with `changed_sources` omitted (exercises `ListDocSources`) and with an explicit list. Assert a no-op source produces empty buckets, an in-place update lands in `remapped` with `confidence=1.0`, a rename lands in `remapped` with `confidence=0.8`. Construct one deliberate ambiguous case (two old chunks sharing a `content_hash`, one new chunk matching by content) and assert it lands in `ambiguous` — and that plain `ingest_documents` on the same fixture still completes without erroring (Increment 1's deterministic tie-break keeps the write path unblocked even though `reanchor`'s report flags the case).
- **`impact`**: seed a function `A` calling `B`, a class `C extends D`, and a chunk `DOCUMENTS`-ing `A` with an observation's evidence chain to a fact. `impact(B, kinds=["CALLS"])` → `A` under `Direct["CALLS"]`. `impact(D, kinds=["EXTENDS"])` → `C`. `impact(A, include_evidence=true)` → the fact surfaces via the composite walk. `impact(kinds=["ABOUT"])` → an explicit, named error.
- **`surface`**: (a) query mode against a known chunk/entity/observation/fact chain — assert all four sections populate and `fact_ids` contains the right ID without full triplet fields leaking into the v1 shape; (b) path mode against a `.ts` fixture with a documenting `.md` chunk — assert the chunk is found via the reverse `DOCUMENTS` traversal, not `OF_FILE` (easy to get backwards, worth a dedicated test); (c) staleness — ingest a fixture, surface it (`staleness=0`), edit the file on disk without re-ingesting, surface again (`staleness=1.0` for that chunk) — the one scenario where "surface sees drift `reanchor` hasn't resolved yet" is exactly the intended signal.
