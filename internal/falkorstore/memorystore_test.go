package falkorstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mknw/h9s/internal/chunking"
)

const testDim = 8 // tiny vectors for tests; real dim is 512

// newMemoryTestStore connects to a throwaway graph for the test. Skips the test
// if FalkorDB is unreachable (so unit-test runs without infra don't fail).
func newMemoryTestStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("FALKOR_ADDR")
	if addr == "" {
		addr = "localhost:6381"
	}
	graphName := fmt.Sprintf("test_memory_%d_%s", time.Now().UnixNano(), t.Name())
	store, err := NewStore(addr, graphName)
	if err != nil {
		t.Skipf("FalkorDB unreachable at %s: %v", addr, err)
	}
	if err := store.Ping(context.Background()); err != nil {
		t.Skipf("FalkorDB ping failed at %s: %v", addr, err)
	}
	t.Cleanup(func() {
		_ = store.DeleteAllNodes(context.Background())
	})
	return store
}

func vec(n int) []float32 {
	out := make([]float32, testDim)
	for i := range out {
		out[i] = float32(n) + float32(i)*0.01
	}
	return out
}

func TestEnsureGraphMeta(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	// Fresh creation.
	if err := store.EnsureGraphMeta(ctx, "memory", false); err != nil {
		t.Fatalf("EnsureGraphMeta first call: %v", err)
	}
	kind, strict, exists, err := store.ReadGraphMeta(ctx)
	if err != nil {
		t.Fatalf("ReadGraphMeta: %v", err)
	}
	if !exists || kind != "memory" || strict != false {
		t.Errorf("after init: got kind=%q strict=%v exists=%v, want memory/false/true", kind, strict, exists)
	}

	// Idempotent re-call same kind, flip strict.
	if err := store.EnsureGraphMeta(ctx, "memory", true); err != nil {
		t.Fatalf("EnsureGraphMeta second call: %v", err)
	}
	_, strict, _, _ = store.ReadGraphMeta(ctx)
	if !strict {
		t.Errorf("strict should have been updated to true")
	}

	// Conflicting kind rejected.
	if err := store.EnsureGraphMeta(ctx, "code", false); err == nil {
		t.Errorf("expected error redeclaring memory graph as code")
	}
}

func TestReadGraphMetaAbsent(t *testing.T) {
	store := newMemoryTestStore(t)
	_, _, exists, err := store.ReadGraphMeta(context.Background())
	if err != nil {
		t.Fatalf("ReadGraphMeta on empty graph: %v", err)
	}
	if exists {
		t.Errorf("expected exists=false on empty graph")
	}
}

func TestEnsureMemoryIndexIdempotent(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestAddObservation(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatalf("EnsureMemoryIndex: %v", err)
	}

	id1, err := store.AddObservation(ctx, "first observation", vec(1))
	if err != nil {
		t.Fatalf("AddObservation 1: %v", err)
	}
	id2, err := store.AddObservation(ctx, "first observation", vec(1)) // same content
	if err != nil {
		t.Fatalf("AddObservation 2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("expected distinct IDs (observations are not deduplicated), got %d == %d", id1, id2)
	}
}

func TestAddFactMERGE(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatalf("EnsureMemoryIndex: %v", err)
	}

	id1, err := store.AddFact(ctx, "FalkorDB", "incompatible_with", "RediSearch on arm64")
	if err != nil {
		t.Fatalf("AddFact 1: %v", err)
	}
	id2, err := store.AddFact(ctx, "FalkorDB", "incompatible_with", "RediSearch on arm64")
	if err != nil {
		t.Fatalf("AddFact 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("MERGE on same triplet should return same ID; got %d != %d", id1, id2)
	}

	id3, err := store.AddFact(ctx, "FalkorDB", "is_a", "graph database")
	if err != nil {
		t.Fatalf("AddFact 3: %v", err)
	}
	if id3 == id1 {
		t.Errorf("different triplet should produce different ID; got %d == %d", id3, id1)
	}
}

func TestFindFactByTriplet(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatalf("EnsureMemoryIndex: %v", err)
	}

	want, err := store.AddFact(ctx, "X", "is_a", "Y")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.FindFactByTriplet(ctx, "X", "is_a", "Y")
	if err != nil {
		t.Fatalf("FindFactByTriplet hit: %v", err)
	}
	if got != want {
		t.Errorf("hit: got %d, want %d", got, want)
	}

	miss, err := store.FindFactByTriplet(ctx, "X", "is_a", "Z")
	if err != nil {
		t.Fatalf("FindFactByTriplet miss: %v", err)
	}
	if miss != -1 {
		t.Errorf("miss: got %d, want -1", miss)
	}
}

func TestLinkEvidenceForIdempotent(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatalf("EnsureMemoryIndex: %v", err)
	}

	obsID, err := store.AddObservation(ctx, "evidence sentence", vec(1))
	if err != nil {
		t.Fatal(err)
	}
	factID, err := store.AddFact(ctx, "S", "p", "O")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := store.LinkEvidenceFor(ctx, obsID, factID); err != nil {
			t.Fatalf("LinkEvidenceFor call %d: %v", i, err)
		}
	}
	count, err := store.CountFactEvidence(ctx, factID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 EVIDENCE_FOR edge after 3 idempotent calls, got %d", count)
	}
}

func TestLinkMotivatesSetsLastDistilledAtOnce(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureCodeIndex(ctx); err != nil {
		t.Fatalf("EnsureCodeIndex: %v", err)
	}
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatalf("EnsureMemoryIndex: %v", err)
	}

	fileID, err := store.AddFile(ctx, "/tmp/x.md", "x.md", ".md")
	if err != nil {
		t.Fatal(err)
	}
	chunkIDs, err := store.StoreChunks(ctx, fakeChunks(1), [][]float32{vec(1)}, []int64{fileID})
	if err != nil {
		t.Fatal(err)
	}
	_ = chunkIDs

	// Get the chunk's actual ID from the store.
	rows, err := store.FetchChunks(ctx, "/tmp/x.md")
	if err != nil || len(rows) != 1 {
		t.Fatalf("FetchChunks: rows=%d err=%v", len(rows), err)
	}
	chunkID := rows[0].ID

	obsID, err := store.AddObservation(ctx, "obs", vec(2))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.LinkMotivates(ctx, chunkID, obsID); err != nil {
		t.Fatalf("LinkMotivates 1: %v", err)
	}
	first, err := readChunkLastDistilledAt(store, chunkID)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatalf("last_distilled_at should be set after first MOTIVATES")
	}

	// Re-link should be a no-op on the timestamp.
	time.Sleep(2 * time.Millisecond)
	obsID2, _ := store.AddObservation(ctx, "obs2", vec(3))
	if err := store.LinkMotivates(ctx, chunkID, obsID2); err != nil {
		t.Fatalf("LinkMotivates 2: %v", err)
	}
	second, err := readChunkLastDistilledAt(store, chunkID)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("last_distilled_at should not change on subsequent MOTIVATES; first=%d second=%d", first, second)
	}
}

func readChunkLastDistilledAt(s *Store, chunkID int64) (int64, error) {
	res, err := s.graph.Query(
		"MATCH (c:Chunk) WHERE ID(c) = $id RETURN c.last_distilled_at",
		map[string]interface{}{"id": chunkID}, nil,
	)
	if err != nil {
		return 0, err
	}
	if !res.Next() {
		return 0, fmt.Errorf("no chunk")
	}
	v, _ := res.Record().GetByIndex(0)
	return toInt64(v), nil
}

func TestSearchObservationsKNN(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatal(err)
	}

	want, _ := store.AddObservation(ctx, "near", vec(1))
	_, _ = store.AddObservation(ctx, "far", vec(100))

	hits, err := store.SearchObservations(ctx, vec(1), 2)
	if err != nil {
		t.Fatalf("SearchObservations: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].ID != want {
		t.Errorf("expected closest hit to be %d, got %d (content=%q)", want, hits[0].ID, hits[0].Content)
	}
}

func TestFactsBackedByObservations(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatal(err)
	}

	obs1, _ := store.AddObservation(ctx, "o1", vec(1))
	obs2, _ := store.AddObservation(ctx, "o2", vec(2))
	fact1, _ := store.AddFact(ctx, "A", "is_a", "B")
	fact2, _ := store.AddFact(ctx, "C", "causes", "D")

	_ = store.LinkEvidenceFor(ctx, obs1, fact1)
	_ = store.LinkEvidenceFor(ctx, obs2, fact1) // both observations back fact1
	_ = store.LinkEvidenceFor(ctx, obs2, fact2) // only obs2 backs fact2

	rows, err := store.FactsBackedByObservations(ctx, []int64{obs1, obs2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 distinct facts, got %d: %+v", len(rows), rows)
	}

	rowsObs1Only, err := store.FactsBackedByObservations(ctx, []int64{obs1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rowsObs1Only) != 1 || rowsObs1Only[0].ID != fact1 {
		t.Errorf("obs1 only: expected [fact1], got %+v", rowsObs1Only)
	}
}

func TestSearchFacts(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()
	if err := store.EnsureMemoryIndex(ctx, testDim); err != nil {
		t.Fatal(err)
	}
	_, _ = store.AddFact(ctx, "FalkorDB", "is_a", "graph database")
	_, _ = store.AddFact(ctx, "FalkorDB", "supports", "vector search")
	_, _ = store.AddFact(ctx, "Redis", "is_a", "key-value store")

	all, err := store.SearchFacts(ctx, "", "is_a", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("predicate=is_a: expected 2 rows, got %d: %+v", len(all), all)
	}

	specific, err := store.SearchFacts(ctx, "FalkorDB", "is_a", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(specific) != 1 || specific[0].Object != "graph database" {
		t.Errorf("subject=FalkorDB predicate=is_a: expected one [graph database] row, got %+v", specific)
	}

	none, err := store.SearchFacts(ctx, "Nope", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("expected no matches, got %+v", none)
	}
}

// fakeChunks builds n minimal chunks for tests that need :Chunk nodes.
func fakeChunks(n int) []chunking.Chunk {
	out := make([]chunking.Chunk, n)
	for i := 0; i < n; i++ {
		out[i] = chunking.Chunk{
			Breadcrumb: "test",
			Content:    fmt.Sprintf("content %d", i),
			Source:     "/tmp/x.md",
			LineStart:  i*10 + 1,
			LineEnd:    i*10 + 5,
		}
	}
	return out
}
