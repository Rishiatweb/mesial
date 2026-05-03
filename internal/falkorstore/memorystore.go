package falkorstore

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// MemoryPredicateKernel is the set of well-known predicates the future inference
// engine reasons about. Open-set predicates outside this set are accepted but
// inert to the engine — stored as opaque strings.
var MemoryPredicateKernel = map[string]bool{
	"is_a":              true,
	"subtype_of":        true,
	"part_of":           true,
	"equivalent_to":     true,
	"incompatible_with": true,
	"causes":            true,
	"requires":          true,
}

// EnsureGraphMeta MERGEs the singleton :GraphMeta {kind, strict}.
// kind ∈ {"code", "notes", "memory"}; strict defaults to false.
// A graph's kind is fixed at creation: re-running with a different kind is an
// error. strict can be updated freely.
func (s *Store) EnsureGraphMeta(ctx context.Context, kind string, strict bool) error {
	res, err := s.graph.Query("MATCH (g:GraphMeta) RETURN g.kind, g.strict LIMIT 1", nil, nil)
	if err != nil {
		return fmt.Errorf("reading existing GraphMeta: %w", err)
	}
	if res.Next() {
		r := res.Record()
		existing, _ := r.GetByIndex(0)
		if k, ok := existing.(string); ok && k != kind {
			return fmt.Errorf("graph already declared kind=%q, refusing to redeclare as %q", k, kind)
		}
		params := map[string]interface{}{"strict": strict}
		_, err := s.graph.Query("MATCH (g:GraphMeta) SET g.strict = $strict", params, nil)
		if err != nil {
			return fmt.Errorf("updating GraphMeta strict: %w", err)
		}
		return nil
	}
	params := map[string]interface{}{"kind": kind, "strict": strict}
	_, err = s.graph.Query("CREATE (g:GraphMeta {kind: $kind, strict: $strict})", params, nil)
	if err != nil {
		return fmt.Errorf("creating GraphMeta: %w", err)
	}
	return nil
}

// ReadGraphMeta returns the singleton's properties, or ("", false, false, nil) if absent.
func (s *Store) ReadGraphMeta(ctx context.Context) (kind string, strict bool, exists bool, err error) {
	res, err := s.graph.Query("MATCH (g:GraphMeta) RETURN g.kind, g.strict LIMIT 1", nil, nil)
	if err != nil {
		return "", false, false, fmt.Errorf("reading GraphMeta: %w", err)
	}
	if !res.Next() {
		return "", false, false, nil
	}
	r := res.Record()
	kVal, _ := r.GetByIndex(0)
	sVal, _ := r.GetByIndex(1)
	if k, ok := kVal.(string); ok {
		kind = k
	}
	if b, ok := sVal.(bool); ok {
		strict = b
	}
	return kind, strict, true, nil
}

// EnsureMemoryIndex creates the indexes used by memory operations:
//   - vector index on :Observation.vector (cosine, dim)
//   - range index on :Fact (subject, predicate)
//   - range index on :Fact (predicate, object)
//
// Idempotent — silently ignores "already indexed" / "already exists" errors.
func (s *Store) EnsureMemoryIndex(ctx context.Context, dim int) error {
	queries := []string{
		fmt.Sprintf("CREATE VECTOR INDEX FOR (o:Observation) ON (o.vector) OPTIONS {dimension:%d, similarityFunction:'cosine'}", dim),
		"CREATE INDEX FOR (f:Fact) ON (f.subject, f.predicate)",
		"CREATE INDEX FOR (f:Fact) ON (f.predicate, f.object)",
	}
	for _, q := range queries {
		_, err := s.graph.Query(q, nil, nil)
		if err != nil && (strings.Contains(err.Error(), "already indexed") || strings.Contains(err.Error(), "already exists")) {
			continue
		}
		if err != nil {
			return fmt.Errorf("creating memory index: %w", err)
		}
	}
	return nil
}

// AddObservation CREATEs an :Observation with content, vector, created_at=now (ms).
// Observations are intentionally NOT deduplicated — they record events; the same
// sentence written twice is two events. Returns the new node ID.
func (s *Store) AddObservation(ctx context.Context, content string, vector []float32) (int64, error) {
	params := map[string]interface{}{
		"content":    content,
		"created_at": time.Now().UnixMilli(),
		"vector":     float32ToInterface(vector),
	}
	res, err := s.graph.Query(
		"CREATE (o:Observation {content: $content, created_at: $created_at, vector: vecf32($vector)}) RETURN ID(o)",
		params, nil,
	)
	if err != nil {
		return 0, fmt.Errorf("adding observation: %w", err)
	}
	if !res.Next() {
		return 0, fmt.Errorf("adding observation: no result returned")
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// AddFact MERGEs a :Fact on (subject, predicate, object) — strict case-sensitive
// string equality. Returns the existing or newly created node ID. created_at is
// set only on first insert; re-MERGE is a no-op on properties.
func (s *Store) AddFact(ctx context.Context, subject, predicate, object string) (int64, error) {
	params := map[string]interface{}{
		"subject":    subject,
		"predicate":  predicate,
		"object":     object,
		"created_at": time.Now().UnixMilli(),
	}
	res, err := s.graph.Query(
		"MERGE (f:Fact {subject: $subject, predicate: $predicate, object: $object}) "+
			"ON CREATE SET f.created_at = $created_at "+
			"RETURN ID(f)",
		params, nil,
	)
	if err != nil {
		return 0, fmt.Errorf("adding fact: %w", err)
	}
	if !res.Next() {
		return 0, fmt.Errorf("adding fact: no result returned")
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// FindFactByTriplet returns the ID of an existing fact matching the triplet, or -1.
func (s *Store) FindFactByTriplet(ctx context.Context, subject, predicate, object string) (int64, error) {
	params := map[string]interface{}{
		"subject":   subject,
		"predicate": predicate,
		"object":    object,
	}
	res, err := s.graph.Query(
		"MATCH (f:Fact {subject: $subject, predicate: $predicate, object: $object}) RETURN ID(f) LIMIT 1",
		params, nil,
	)
	if err != nil {
		return -1, fmt.Errorf("finding fact: %w", err)
	}
	if !res.Next() {
		return -1, nil
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// LinkEvidenceFor MERGEs (:Observation)-[:EVIDENCE_FOR]->(:Fact) by IDs. Idempotent.
//
// Verifies both endpoints exist with the right labels before merging — guards
// the no-orphan-facts invariant (silent failure here would let stale or wrong
// IDs slip through and leave the agent thinking it had linked evidence when it
// hadn't). Returns an explicit error naming whichever side is missing.
func (s *Store) LinkEvidenceFor(ctx context.Context, obsID, factID int64) error {
	obsExists, factExists, err := s.checkObsAndFactExist(ctx, obsID, factID)
	if err != nil {
		return fmt.Errorf("validating endpoints (obs=%d, fact=%d): %w", obsID, factID, err)
	}
	if !obsExists {
		return fmt.Errorf("linking evidence: :Observation %d not found", obsID)
	}
	if !factExists {
		return fmt.Errorf("linking evidence: :Fact %d not found", factID)
	}
	params := map[string]interface{}{"obs": obsID, "fact": factID}
	_, err = s.graph.Query(
		"MATCH (o:Observation), (f:Fact) WHERE ID(o) = $obs AND ID(f) = $fact "+
			"MERGE (o)-[:EVIDENCE_FOR]->(f)",
		params, nil,
	)
	if err != nil {
		return fmt.Errorf("linking evidence (obs=%d, fact=%d): %w", obsID, factID, err)
	}
	return nil
}

// checkObsAndFactExist verifies the labelled nodes exist by ID. Used by edge
// creators to give explicit "not found" errors instead of silently no-op'ing.
func (s *Store) checkObsAndFactExist(ctx context.Context, obsID, factID int64) (obsExists, factExists bool, err error) {
	params := map[string]interface{}{"obs": obsID, "fact": factID}
	res, err := s.graph.Query(
		"OPTIONAL MATCH (o:Observation) WHERE ID(o) = $obs "+
			"OPTIONAL MATCH (f:Fact) WHERE ID(f) = $fact "+
			"RETURN o IS NOT NULL, f IS NOT NULL",
		params, nil,
	)
	if err != nil {
		return false, false, err
	}
	if !res.Next() {
		return false, false, fmt.Errorf("existence check returned no rows")
	}
	r := res.Record()
	oVal, _ := r.GetByIndex(0)
	fVal, _ := r.GetByIndex(1)
	o, _ := oVal.(bool)
	f, _ := fVal.(bool)
	return o, f, nil
}

// checkChunkAndObsExist mirrors checkObsAndFactExist for the :MOTIVATES edge.
func (s *Store) checkChunkAndObsExist(ctx context.Context, chunkID, obsID int64) (chunkExists, obsExists bool, err error) {
	params := map[string]interface{}{"chunk": chunkID, "obs": obsID}
	res, err := s.graph.Query(
		"OPTIONAL MATCH (c:Chunk) WHERE ID(c) = $chunk "+
			"OPTIONAL MATCH (o:Observation) WHERE ID(o) = $obs "+
			"RETURN c IS NOT NULL, o IS NOT NULL",
		params, nil,
	)
	if err != nil {
		return false, false, err
	}
	if !res.Next() {
		return false, false, fmt.Errorf("existence check returned no rows")
	}
	r := res.Record()
	cVal, _ := r.GetByIndex(0)
	oVal, _ := r.GetByIndex(1)
	c, _ := cVal.(bool)
	o, _ := oVal.(bool)
	return c, o, nil
}

// LinkMotivates MERGEs (:Observation)-[:MOTIVATES]->(:Chunk) by IDs and sets
// chunk.last_distilled_at = current ms (only if not already set). Idempotent.
//
// Direction is intentional: an observation *motivates* a chunk's existence in
// the docs — chunks are documented manifestations of observations the agent has
// made. A chunk without any incoming :MOTIVATES is unverified material; reading
// it must produce an observation (which is then linked via this edge) for the
// chunk to be considered "alive" in the system.
//
// Verifies both endpoints exist with the right labels before merging — same
// rationale as LinkEvidenceFor.
func (s *Store) LinkMotivates(ctx context.Context, chunkID, obsID int64) error {
	chunkExists, obsExists, err := s.checkChunkAndObsExist(ctx, chunkID, obsID)
	if err != nil {
		return fmt.Errorf("validating endpoints (chunk=%d, obs=%d): %w", chunkID, obsID, err)
	}
	if !chunkExists {
		return fmt.Errorf("linking motivates: :Chunk %d not found", chunkID)
	}
	if !obsExists {
		return fmt.Errorf("linking motivates: :Observation %d not found", obsID)
	}
	params := map[string]interface{}{
		"chunk": chunkID,
		"obs":   obsID,
		"now":   time.Now().UnixMilli(),
	}
	_, err = s.graph.Query(
		"MATCH (c:Chunk), (o:Observation) WHERE ID(c) = $chunk AND ID(o) = $obs "+
			"MERGE (o)-[:MOTIVATES]->(c) "+
			"SET c.last_distilled_at = coalesce(c.last_distilled_at, $now)",
		params, nil,
	)
	if err != nil {
		return fmt.Errorf("linking motivates (obs=%d, chunk=%d): %w", obsID, chunkID, err)
	}
	return nil
}

// ObservationHit is one result from KNN search over :Observation.vector.
// Distance is cosine distance — lower means closer / more semantically similar.
type ObservationHit struct {
	ID       int64   `json:"id"`
	Content  string  `json:"content"`
	Distance float64 `json:"distance"`
}

// SearchObservations runs KNN over :Observation.vector.
func (s *Store) SearchObservations(ctx context.Context, queryVec []float32, k int) ([]ObservationHit, error) {
	params := map[string]interface{}{
		"k":   k,
		"vec": float32ToInterface(queryVec),
	}
	res, err := s.graph.Query(
		"CALL db.idx.vector.queryNodes('Observation', 'vector', $k, vecf32($vec)) YIELD node, score "+
			"RETURN ID(node), node.content, score",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("observation vector search: %w", err)
	}
	var hits []ObservationHit
	for res.Next() {
		r := res.Record()
		id, _ := r.GetByIndex(0)
		content, _ := r.GetByIndex(1)
		score, _ := r.GetByIndex(2)
		hits = append(hits, ObservationHit{
			ID:       toInt64(id),
			Content:  fmt.Sprint(content),
			Distance: toFloat64(score),
		})
	}
	return hits, nil
}

// FactRow is a triplet projection of a :Fact node.
type FactRow struct {
	ID        int64  `json:"id"`
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

// ObservationRecord is the full inspector projection of a :Observation, used
// by show-obs (CLI) and any future MCP read tool. last_distilled_at is 0 when
// the observation has not yet motivated any chunk.
type ObservationRecord struct {
	ID              int64  `json:"id"`
	Content         string `json:"content"`
	CreatedAt       int64  `json:"created_at"`
	LastDistilledAt int64  `json:"last_distilled_at,omitempty"`
}

// FactRecord is the full inspector projection of a :Fact (triplet + lifecycle).
// last_verified_at is 0 when no verification has been recorded yet.
type FactRecord struct {
	ID             int64  `json:"id"`
	Subject        string `json:"subject"`
	Predicate      string `json:"predicate"`
	Object         string `json:"object"`
	CreatedAt      int64  `json:"created_at"`
	LastVerifiedAt int64  `json:"last_verified_at,omitempty"`
}

// ErrNotFound signals a single-record fetch that found no node with the given ID.
var ErrNotFound = fmt.Errorf("not found")

// GetObservation returns the full record for an :Observation by ID.
func (s *Store) GetObservation(ctx context.Context, id int64) (ObservationRecord, error) {
	params := map[string]interface{}{"id": id}
	res, err := s.graph.Query(
		"MATCH (o:Observation) WHERE ID(o) = $id "+
			"RETURN ID(o), o.content, o.created_at, o.last_distilled_at",
		params, nil,
	)
	if err != nil {
		return ObservationRecord{}, fmt.Errorf("fetching observation %d: %w", id, err)
	}
	if !res.Next() {
		return ObservationRecord{}, ErrNotFound
	}
	r := res.Record()
	idVal, _ := r.GetByIndex(0)
	contentVal, _ := r.GetByIndex(1)
	createdVal, _ := r.GetByIndex(2)
	distilledVal, _ := r.GetByIndex(3)
	return ObservationRecord{
		ID:              toInt64(idVal),
		Content:         fmt.Sprint(contentVal),
		CreatedAt:       toInt64(createdVal),
		LastDistilledAt: toInt64(distilledVal),
	}, nil
}

// GetFact returns the full record for a :Fact by ID.
func (s *Store) GetFact(ctx context.Context, id int64) (FactRecord, error) {
	params := map[string]interface{}{"id": id}
	res, err := s.graph.Query(
		"MATCH (f:Fact) WHERE ID(f) = $id "+
			"RETURN ID(f), f.subject, f.predicate, f.object, f.created_at, f.last_verified_at",
		params, nil,
	)
	if err != nil {
		return FactRecord{}, fmt.Errorf("fetching fact %d: %w", id, err)
	}
	if !res.Next() {
		return FactRecord{}, ErrNotFound
	}
	r := res.Record()
	idVal, _ := r.GetByIndex(0)
	sVal, _ := r.GetByIndex(1)
	pVal, _ := r.GetByIndex(2)
	oVal, _ := r.GetByIndex(3)
	createdVal, _ := r.GetByIndex(4)
	verifiedVal, _ := r.GetByIndex(5)
	return FactRecord{
		ID:             toInt64(idVal),
		Subject:        fmt.Sprint(sVal),
		Predicate:      fmt.Sprint(pVal),
		Object:         fmt.Sprint(oVal),
		CreatedAt:      toInt64(createdVal),
		LastVerifiedAt: toInt64(verifiedVal),
	}, nil
}

// EvidenceForFact returns all :Observation nodes that back the given fact via
// :EVIDENCE_FOR. Used by show-fact to display the evidence chain backward.
func (s *Store) EvidenceForFact(ctx context.Context, factID int64) ([]ObservationRecord, error) {
	params := map[string]interface{}{"fact": factID}
	res, err := s.graph.Query(
		"MATCH (o:Observation)-[:EVIDENCE_FOR]->(f:Fact) WHERE ID(f) = $fact "+
			"RETURN ID(o), o.content, o.created_at, o.last_distilled_at "+
			"ORDER BY o.created_at",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("fetching evidence for fact %d: %w", factID, err)
	}
	var out []ObservationRecord
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		contentVal, _ := r.GetByIndex(1)
		createdVal, _ := r.GetByIndex(2)
		distilledVal, _ := r.GetByIndex(3)
		out = append(out, ObservationRecord{
			ID:              toInt64(idVal),
			Content:         fmt.Sprint(contentVal),
			CreatedAt:       toInt64(createdVal),
			LastDistilledAt: toInt64(distilledVal),
		})
	}
	return out, nil
}

// FactsBackedByObservations returns distinct :Fact nodes that have :EVIDENCE_FOR
// from any of the given observation IDs.
func (s *Store) FactsBackedByObservations(ctx context.Context, obsIDs []int64) ([]FactRow, error) {
	if len(obsIDs) == 0 {
		return nil, nil
	}
	ids := make([]interface{}, len(obsIDs))
	for i, id := range obsIDs {
		ids[i] = id
	}
	params := map[string]interface{}{"ids": ids}
	res, err := s.graph.Query(
		"MATCH (o:Observation)-[:EVIDENCE_FOR]->(f:Fact) "+
			"WHERE ID(o) IN $ids "+
			"RETURN DISTINCT ID(f), f.subject, f.predicate, f.object",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("fetching backed facts: %w", err)
	}
	var rows []FactRow
	for res.Next() {
		r := res.Record()
		id, _ := r.GetByIndex(0)
		subj, _ := r.GetByIndex(1)
		pred, _ := r.GetByIndex(2)
		obj, _ := r.GetByIndex(3)
		rows = append(rows, FactRow{
			ID:        toInt64(id),
			Subject:   fmt.Sprint(subj),
			Predicate: fmt.Sprint(pred),
			Object:    fmt.Sprint(obj),
		})
	}
	return rows, nil
}

// CountFactEvidence returns how many :Observation nodes back the fact via :EVIDENCE_FOR.
// Used to verify the no-orphan-facts invariant.
func (s *Store) CountFactEvidence(ctx context.Context, factID int64) (int, error) {
	params := map[string]interface{}{"fact": factID}
	res, err := s.graph.Query(
		"MATCH (o:Observation)-[:EVIDENCE_FOR]->(f:Fact) WHERE ID(f) = $fact RETURN count(o)",
		params, nil,
	)
	if err != nil {
		return 0, fmt.Errorf("counting fact evidence: %w", err)
	}
	if !res.Next() {
		return 0, nil
	}
	r := res.Record()
	n, _ := r.GetByIndex(0)
	return int(toInt64(n)), nil
}

// SearchFacts is a structural query — exact match on any combination of
// subject/predicate/object (empty string = wildcard for that field). Returns at
// most limit rows.
func (s *Store) SearchFacts(ctx context.Context, subject, predicate, object string, limit int) ([]FactRow, error) {
	if limit <= 0 {
		limit = 50
	}
	var conds []string
	params := map[string]interface{}{"limit": limit}
	if subject != "" {
		conds = append(conds, "f.subject = $subject")
		params["subject"] = subject
	}
	if predicate != "" {
		conds = append(conds, "f.predicate = $predicate")
		params["predicate"] = predicate
	}
	if object != "" {
		conds = append(conds, "f.object = $object")
		params["object"] = object
	}
	q := "MATCH (f:Fact)"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " RETURN ID(f), f.subject, f.predicate, f.object LIMIT $limit"
	res, err := s.graph.Query(q, params, nil)
	if err != nil {
		return nil, fmt.Errorf("searching facts: %w", err)
	}
	var rows []FactRow
	for res.Next() {
		r := res.Record()
		id, _ := r.GetByIndex(0)
		subj, _ := r.GetByIndex(1)
		pred, _ := r.GetByIndex(2)
		obj, _ := r.GetByIndex(3)
		rows = append(rows, FactRow{
			ID:        toInt64(id),
			Subject:   fmt.Sprint(subj),
			Predicate: fmt.Sprint(pred),
			Object:    fmt.Sprint(obj),
		})
	}
	return rows, nil
}
