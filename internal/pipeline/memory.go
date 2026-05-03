package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
)

// MemoryGraphKind is the canonical kind value for the global memory graph.
// Per-repo and per-vault graphs may also accumulate :Fact / :Observation nodes
// when strict=false; those graphs declare kind="code" or "notes" respectively.
const MemoryGraphKind = "memory"

// SimilarFact is a fact found via KNN over existing observations — surfaced to
// agents during distillation so they can decide whether to link the new
// observation as additional evidence or propose a new triplet.
type SimilarFact struct {
	ID        int64
	Subject   string
	Predicate string
	Object    string
}

// AddObservationResult is what AddObservation returns to its caller (CLI or MCP).
type AddObservationResult struct {
	ObservationID int64
	SimilarFacts  []SimilarFact
}

// EnsureMemoryReady ensures the GraphMeta singleton exists with the right kind,
// the :Observation vector index is created, and the :Fact range indexes are in
// place. Idempotent. Call once per graph open.
func EnsureMemoryReady(ctx context.Context, store *falkorstore.Store, kind string) error {
	if err := store.EnsureGraphMeta(ctx, kind, false); err != nil {
		return fmt.Errorf("ensuring graph meta (kind=%q): %w", kind, err)
	}
	if err := store.EnsureMemoryIndex(ctx, EmbeddingDim); err != nil {
		return fmt.Errorf("ensuring memory index: %w", err)
	}
	return nil
}

// AddObservation embeds the text, creates an :Observation, then KNN-searches
// existing observations and collects the facts those observations back via
// :EVIDENCE_FOR. The agent uses SimilarFacts to decide what to link or whether
// to create a new fact via CreateFactFromObservation.
//
// k controls how many nearest existing observations to consider when collecting
// related facts. The new observation itself is excluded from the result.
func AddObservation(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, text string, k int) (AddObservationResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return AddObservationResult{}, fmt.Errorf("observation text is empty")
	}
	if k <= 0 {
		k = 5
	}

	vectors, err := embedder.Embed(ctx, []string{text})
	if err != nil {
		return AddObservationResult{}, fmt.Errorf("embedding observation: %w", err)
	}
	if len(vectors) != 1 {
		return AddObservationResult{}, fmt.Errorf("embedder returned %d vectors, want 1", len(vectors))
	}
	vec := vectors[0]

	// KNN over EXISTING observations (before we add the new one) so the new
	// observation never appears as its own neighbor. Pull k+1 to give us
	// headroom for filtering, then trim.
	hits, err := store.SearchObservations(ctx, vec, k+1)
	if err != nil {
		return AddObservationResult{}, fmt.Errorf("searching similar observations: %w", err)
	}

	obsID, err := store.AddObservation(ctx, text, vec)
	if err != nil {
		return AddObservationResult{}, fmt.Errorf("adding observation: %w", err)
	}

	if len(hits) == 0 {
		return AddObservationResult{ObservationID: obsID}, nil
	}
	neighbors := make([]int64, 0, len(hits))
	for _, h := range hits {
		if h.ID == obsID {
			continue
		}
		neighbors = append(neighbors, h.ID)
		if len(neighbors) >= k {
			break
		}
	}
	if len(neighbors) == 0 {
		return AddObservationResult{ObservationID: obsID}, nil
	}

	rows, err := store.FactsBackedByObservations(ctx, neighbors)
	if err != nil {
		return AddObservationResult{}, fmt.Errorf("collecting facts from neighbors: %w", err)
	}
	similar := make([]SimilarFact, len(rows))
	for i, r := range rows {
		similar[i] = SimilarFact{
			ID:        r.ID,
			Subject:   r.Subject,
			Predicate: r.Predicate,
			Object:    r.Object,
		}
	}
	return AddObservationResult{ObservationID: obsID, SimilarFacts: similar}, nil
}

// LinkObservationEvidence MERGEs :EVIDENCE_FOR edges from obsID to each factID.
// All factIDs must reference existing :Fact nodes (no implicit creation — use
// CreateFactFromObservation for new facts to enforce no-orphan-facts).
func LinkObservationEvidence(ctx context.Context, store *falkorstore.Store, obsID int64, factIDs []int64) error {
	for _, factID := range factIDs {
		if err := store.LinkEvidenceFor(ctx, obsID, factID); err != nil {
			return fmt.Errorf("linking obs=%d to fact=%d: %w", obsID, factID, err)
		}
	}
	return nil
}

// CreateFactFromObservation is the only entry path for new facts. It MERGEs the
// triplet (creating or finding the :Fact) and writes the required :EVIDENCE_FOR
// edge from obsID. Returns the fact's ID.
//
// Enforces the no-orphan-facts invariant: facts cannot enter the graph without
// at least one supporting observation. Open-set predicates are accepted; only
// kernel predicates are inference-actionable later.
func CreateFactFromObservation(ctx context.Context, store *falkorstore.Store, obsID int64, subject, predicate, object string) (int64, error) {
	subject = strings.TrimSpace(subject)
	predicate = strings.TrimSpace(predicate)
	object = strings.TrimSpace(object)
	if subject == "" || predicate == "" || object == "" {
		return 0, fmt.Errorf("triplet fields must be non-empty (got s=%q p=%q o=%q)", subject, predicate, object)
	}
	factID, err := store.AddFact(ctx, subject, predicate, object)
	if err != nil {
		return 0, fmt.Errorf("merging fact: %w", err)
	}
	if err := store.LinkEvidenceFor(ctx, obsID, factID); err != nil {
		return 0, fmt.Errorf("linking evidence from obs=%d: %w", obsID, err)
	}
	return factID, nil
}

// LinkChunkMotivatesObservation writes the :MOTIVATES edge anchoring an
// observation to the chunk that prompted it. Idempotent; sets the chunk's
// last_distilled_at timestamp on first link only.
func LinkChunkMotivatesObservation(ctx context.Context, store *falkorstore.Store, chunkID, obsID int64) error {
	return store.LinkMotivates(ctx, chunkID, obsID)
}

// SearchObservations is the read-only KNN-over-observations entry point.
// Embeds the query and returns the top-k hits.
func SearchObservations(ctx context.Context, store *falkorstore.Store, embedder *embedding.Client, query string, k int) ([]falkorstore.ObservationHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is empty")
	}
	if k <= 0 {
		k = 5
	}
	vectors, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedder returned %d vectors, want 1", len(vectors))
	}
	return store.SearchObservations(ctx, vectors[0], k)
}

// SearchFacts is a thin pipeline wrapper over the structural fact search. Empty
// strings act as wildcards; limit defaults to 50 if non-positive.
func SearchFacts(ctx context.Context, store *falkorstore.Store, subject, predicate, object string, limit int) ([]falkorstore.FactRow, error) {
	return store.SearchFacts(ctx, subject, predicate, object, limit)
}
