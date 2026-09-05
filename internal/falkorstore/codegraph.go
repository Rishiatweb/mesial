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
//
// The MERGE key is (label, name, path, parent_name) — NOT src_start/src_end.
// parent_name is the enclosing entity's name for nested entities (Method,
// Constructor) and "" for top-level entities (Class, Function, Interface,
// Enum). Two reasons this shape, not a simpler one:
//   - Dropping src_start/src_end from the key (moving them to SET instead)
//     is the actual fix for line-shift fragility: an unrelated edit earlier
//     in the same file that shifts every subsequent line number no longer
//     changes an entity's identity, it just updates its recorded position.
//   - parent_name is required alongside that fix, not optional: without it,
//     two different classes in one file that each define a same-named
//     method (e.g. `class A { foo() {} }` and `class B { foo() {} }`) would
//     MERGE onto the same node once src_start/src_end stop disambiguating
//     them, silently collapsing two distinct methods into one.
//
// signatureHash may be "" — it's accepted now so callers don't need a
// separate signature-setting call once entity-signature extraction lands
// (see docs/TIER1_CONTINUATION.md), but nothing populates it yet.
func (s *Store) AddEntity(ctx context.Context, label, name, doc, path, parentName string, srcStart, srcEnd int, signatureHash string) (int64, error) {
	params := map[string]interface{}{
		"name":        name,
		"path":        path,
		"parent_name": parentName,
		"src_start":   srcStart,
		"src_end":     srcEnd,
		"doc":         doc,
		"sig_hash":    signatureHash,
	}
	q := fmt.Sprintf(
		"MERGE (c:%s:Searchable {name: $name, path: $path, parent_name: $parent_name}) "+
			"SET c.doc = $doc, c.src_start = $src_start, c.src_end = $src_end, c.signature_hash = $sig_hash "+
			"RETURN ID(c)",
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

// CodeEntity is a name-indexed reference to a Searchable code entity, used by
// the doc linker to build a name → IDs lookup table.
type CodeEntity struct {
	ID   int64
	Name string
}

// FetchCodeEntities returns all code entity nodes (Class, Function, Method,
// Interface, Enum, Constructor) — explicitly excluding :File, which is not a
// valid DOCUMENTS link target per the data model.
func (s *Store) FetchCodeEntities(ctx context.Context) ([]CodeEntity, error) {
	res, err := s.graph.Query(
		"MATCH (s:Searchable) "+
			"WHERE s:Class OR s:Function OR s:Method OR s:Interface OR s:Enum OR s:Constructor "+
			"RETURN ID(s), s.name",
		nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("fetching code entities: %w", err)
	}
	var out []CodeEntity
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		nameVal, _ := r.GetByIndex(1)
		out = append(out, CodeEntity{
			ID:   toInt64(idVal),
			Name: fmt.Sprint(nameVal),
		})
	}
	return out, nil
}
