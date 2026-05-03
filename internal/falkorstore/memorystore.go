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
func (s *Store) LinkEvidenceFor(ctx context.Context, obsID, factID int64) error {
	params := map[string]interface{}{"obs": obsID, "fact": factID}
	_, err := s.graph.Query(
		"MATCH (o:Observation), (f:Fact) WHERE ID(o) = $obs AND ID(f) = $fact "+
			"MERGE (o)-[:EVIDENCE_FOR]->(f)",
		params, nil,
	)
	if err != nil {
		return fmt.Errorf("linking evidence (obs=%d, fact=%d): %w", obsID, factID, err)
	}
	return nil
}

// LinkMotivates MERGEs (:Chunk)-[:MOTIVATES]->(:Observation) by IDs and sets
// chunk.last_distilled_at = current ms (only if not already set). Idempotent.
func (s *Store) LinkMotivates(ctx context.Context, chunkID, obsID int64) error {
	params := map[string]interface{}{
		"chunk": chunkID,
		"obs":   obsID,
		"now":   time.Now().UnixMilli(),
	}
	_, err := s.graph.Query(
		"MATCH (c:Chunk), (o:Observation) WHERE ID(c) = $chunk AND ID(o) = $obs "+
			"MERGE (c)-[:MOTIVATES]->(o) "+
			"SET c.last_distilled_at = coalesce(c.last_distilled_at, $now)",
		params, nil,
	)
	if err != nil {
		return fmt.Errorf("linking motivates (chunk=%d, obs=%d): %w", chunkID, obsID, err)
	}
	return nil
}

// ObservationHit is one result from KNN search over :Observation.vector.
type ObservationHit struct {
	ID      int64
	Content string
	Score   float64
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
			ID:      toInt64(id),
			Content: fmt.Sprint(content),
			Score:   toFloat64(score),
		})
	}
	return hits, nil
}

// FactRow is a triplet projection of a :Fact node.
type FactRow struct {
	ID        int64
	Subject   string
	Predicate string
	Object    string
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
