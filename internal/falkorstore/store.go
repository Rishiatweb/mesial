package falkorstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/FalkorDB/falkordb-go/v2"
	"github.com/mknw/h9s/internal/chunking"
)

// Store wraps a FalkorDB graph for vector-indexed chunk storage.
type Store struct {
	db    *falkordb.FalkorDB
	graph *falkordb.Graph
}

// NewStore connects to FalkorDB at addr and selects the given graph name.
func NewStore(addr, graphName string) (*Store, error) {
	db, err := falkordb.FalkorDBNew(&falkordb.ConnectionOption{
		Addr: addr,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to FalkorDB: %w", err)
	}
	graph := db.SelectGraph(graphName)
	return &Store{db: db, graph: graph}, nil
}

// Close is a no-op placeholder; falkordb-go doesn't expose a Close method.
func (s *Store) Close() error {
	return nil
}

// Ping checks FalkorDB connectivity by running a trivial query.
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.graph.Query("RETURN 1", nil, nil)
	return err
}

// EnsureIndex creates the vector index on Chunk.vector if it doesn't already exist.
func (s *Store) EnsureIndex(ctx context.Context, dim int) error {
	q := fmt.Sprintf(
		"CREATE VECTOR INDEX FOR (c:Chunk) ON (c.vector) OPTIONS {dimension:%d, similarityFunction:'cosine'}",
		dim,
	)
	_, err := s.graph.Query(q, nil, nil)
	if err != nil && strings.Contains(err.Error(), "already indexed") {
		return nil
	}
	return err
}

// StoreChunks creates Chunk nodes with embeddings. Returns count stored.
func (s *Store) StoreChunks(ctx context.Context, chunks []chunking.Chunk, vectors [][]float32) (int, error) {
	if len(chunks) != len(vectors) {
		return 0, fmt.Errorf("chunks (%d) and vectors (%d) length mismatch", len(chunks), len(vectors))
	}
	if len(chunks) == 0 {
		return 0, nil
	}

	stored := 0
	for i, chunk := range chunks {
		params := map[string]interface{}{
			"breadcrumb": chunk.Breadcrumb,
			"content":    chunk.Content,
			"source":     chunk.Source,
			"line_start": chunk.LineStart,
			"line_end":   chunk.LineEnd,
			"vector":     float32ToInterface(vectors[i]),
		}

		_, err := s.graph.Query(
			"CREATE (:Chunk {breadcrumb: $breadcrumb, content: $content, source: $source, line_start: $line_start, line_end: $line_end, vector: vecf32($vector)})",
			params, nil,
		)
		if err != nil {
			return stored, fmt.Errorf("creating chunk node %d: %w", i, err)
		}
		stored++
	}
	return stored, nil
}

// DeleteBySource removes all Chunk nodes for a given source path.
func (s *Store) DeleteBySource(ctx context.Context, source string) (int, error) {
	params := map[string]interface{}{"source": source}
	res, err := s.graph.Query(
		"MATCH (c:Chunk {source: $source}) DELETE c",
		params, nil,
	)
	if err != nil {
		return 0, err
	}
	return res.NodesDeleted(), nil
}

// SearchResult holds a single vector search hit.
type SearchResult struct {
	Breadcrumb string
	Content    string
	Source     string
	LineStart  int64
	LineEnd    int64
	Score      float64
}

// Search performs KNN vector search and returns matching chunks.
func (s *Store) Search(ctx context.Context, queryVec []float32, k int) ([]SearchResult, error) {
	params := map[string]interface{}{
		"k":   k,
		"vec": float32ToInterface(queryVec),
	}

	res, err := s.graph.Query(
		"CALL db.idx.vector.queryNodes('Chunk', 'vector', $k, vecf32($vec)) YIELD node, score "+
			"RETURN node.breadcrumb, node.content, node.source, node.line_start, node.line_end, score",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}

	var results []SearchResult
	for res.Next() {
		r := res.Record()
		breadcrumb, _ := r.GetByIndex(0)
		content, _ := r.GetByIndex(1)
		source, _ := r.GetByIndex(2)
		lineStart, _ := r.GetByIndex(3)
		lineEnd, _ := r.GetByIndex(4)
		score, _ := r.GetByIndex(5)

		results = append(results, SearchResult{
			Breadcrumb: fmt.Sprint(breadcrumb),
			Content:    fmt.Sprint(content),
			Source:     fmt.Sprint(source),
			LineStart:  toInt64(lineStart),
			LineEnd:    toInt64(lineEnd),
			Score:      toFloat64(score),
		})
	}
	return results, nil
}

// float32ToInterface converts []float32 to []interface{} for Cypher parameter serialization.
func float32ToInterface(v []float32) []interface{} {
	out := make([]interface{}, len(v))
	for i, f := range v {
		out[i] = float64(f)
	}
	return out
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func toFloat64(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	default:
		return 0
	}
}
