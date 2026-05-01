package falkorstore

import (
	"context"
	"fmt"
	"strings"
)

// EnsureCodeIndex creates full-text and range indexes for code graph nodes.
// Idempotent — silently ignores "already indexed" errors.
func (s *Store) EnsureCodeIndex(ctx context.Context) error {
	queries := []string{
		"CREATE FULLTEXT INDEX FOR (s:Searchable) ON (s.name)",
		"CREATE INDEX FOR (f:File) ON (f.name, f.ext)",
	}
	for _, q := range queries {
		_, err := s.graph.Query(q, nil, nil)
		if err != nil && (strings.Contains(err.Error(), "already indexed") || strings.Contains(err.Error(), "already exists")) {
			continue
		}
		if err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}
	return nil
}

// AddFile MERGEs a File:Searchable node and returns its FalkorDB node ID.
func (s *Store) AddFile(ctx context.Context, path, name, ext string) (int64, error) {
	params := map[string]interface{}{
		"path": path,
		"name": name,
		"ext":  ext,
	}
	res, err := s.graph.Query(
		"MERGE (f:File:Searchable {path: $path, name: $name, ext: $ext}) RETURN ID(f)",
		params, nil,
	)
	if err != nil {
		return 0, fmt.Errorf("adding file node: %w", err)
	}
	if !res.Next() {
		return 0, fmt.Errorf("adding file node: no result returned")
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// AddEntity MERGEs an entity node with dual labels (entity type + Searchable)
// and returns its FalkorDB node ID. The label parameter must be from a
// controlled set (Function, Class, Method, Interface, Enum, Constructor).
func (s *Store) AddEntity(ctx context.Context, label, name, doc, path string, srcStart, srcEnd int) (int64, error) {
	params := map[string]interface{}{
		"name":      name,
		"path":      path,
		"src_start": srcStart,
		"src_end":   srcEnd,
		"doc":       doc,
	}
	q := fmt.Sprintf(
		"MERGE (c:%s:Searchable {name: $name, path: $path, src_start: $src_start, src_end: $src_end}) "+
			"SET c.doc = $doc RETURN ID(c)",
		label,
	)
	res, err := s.graph.Query(q, params, nil)
	if err != nil {
		return 0, fmt.Errorf("adding entity node %s %q: %w", label, name, err)
	}
	if !res.Next() {
		return 0, fmt.Errorf("adding entity node: no result returned")
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// ConnectEntities MERGEs a directed relationship between two nodes by their IDs.
// The relation parameter must be from a controlled set (DEFINES, CALLS, EXTENDS,
// IMPLEMENTS, RETURNS, PARAMETERS, DOCUMENTS).
func (s *Store) ConnectEntities(ctx context.Context, relation string, srcID, destID int64, props map[string]interface{}) error {
	params := map[string]interface{}{
		"src_id":  srcID,
		"dest_id": destID,
	}
	q := fmt.Sprintf(
		"MATCH (src), (dest) WHERE ID(src) = $src_id AND ID(dest) = $dest_id "+
			"MERGE (src)-[e:%s]->(dest)",
		relation,
	)
	if len(props) > 0 {
		params["props"] = props
		q += " SET e += $props"
	}
	_, err := s.graph.Query(q, params, nil)
	if err != nil {
		return fmt.Errorf("connecting entities %s: %w", relation, err)
	}
	return nil
}

// LookupEntityByPosition finds an entity by file path and source line.
// Returns the node ID of the innermost entity containing the given line,
// or -1 if no entity is found.
func (s *Store) LookupEntityByPosition(ctx context.Context, path string, line int) (int64, error) {
	params := map[string]interface{}{
		"path": path,
		"line": line,
	}
	res, err := s.graph.Query(
		"MATCH (e:Searchable {path: $path}) WHERE e.src_start <= $line AND e.src_end >= $line "+
			"RETURN ID(e) ORDER BY (e.src_end - e.src_start) ASC LIMIT 1",
		params, nil,
	)
	if err != nil {
		return -1, fmt.Errorf("looking up entity: %w", err)
	}
	if !res.Next() {
		return -1, nil
	}
	r := res.Record()
	id, _ := r.GetByIndex(0)
	return toInt64(id), nil
}

// DeleteAllNodes removes all nodes and relationships from the current graph.
// Used for full re-indexing of a repository.
func (s *Store) DeleteAllNodes(ctx context.Context) error {
	_, err := s.graph.Query("MATCH (n) DETACH DELETE n", nil, nil)
	if err != nil {
		return fmt.Errorf("deleting all nodes: %w", err)
	}
	return nil
}
