package falkorstore

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// StoreChunks creates Chunk nodes with embeddings, each anchored to a File node
// via a (:Chunk)-[:OF_FILE]->(:File) edge. fileIDs must be parallel to chunks
// (one File node ID per chunk; callers typically MERGE the File once per source
// path via AddFile and replicate the ID across all chunks for that source).
// Returns count stored.
func (s *Store) StoreChunks(ctx context.Context, chunks []chunking.Chunk, vectors [][]float32, fileIDs []int64) (int, error) {
	if len(chunks) != len(vectors) {
		return 0, fmt.Errorf("chunks (%d) and vectors (%d) length mismatch", len(chunks), len(vectors))
	}
	if len(chunks) != len(fileIDs) {
		return 0, fmt.Errorf("chunks (%d) and fileIDs (%d) length mismatch", len(chunks), len(fileIDs))
	}
	if len(chunks) == 0 {
		return 0, nil
	}

	stored := 0
	for i, chunk := range chunks {
		params := map[string]interface{}{
			"file_id":    fileIDs[i],
			"breadcrumb": chunk.Breadcrumb,
			"content":    chunk.Content,
			"source":     chunk.Source,
			"line_start": chunk.LineStart,
			"line_end":   chunk.LineEnd,
			"vector":     float32ToInterface(vectors[i]),
		}

		_, err := s.graph.Query(
			"MATCH (f:File) WHERE ID(f) = $file_id "+
				"CREATE (c:Chunk {breadcrumb: $breadcrumb, content: $content, source: $source, line_start: $line_start, line_end: $line_end, vector: vecf32($vector)}) "+
				"CREATE (c)-[:OF_FILE]->(f)",
			params, nil,
		)
		if err != nil {
			return stored, fmt.Errorf("creating chunk node %d: %w", i, err)
		}
		stored++
	}
	return stored, nil
}

// BackfillChunkAnchors computes and SETs anchor_id/content_hash on every
// existing :Chunk missing them, using the ALREADY-STORED source/breadcrumb/
// content — no re-chunking, no re-reading files off disk, no re-embedding.
// Pure metadata backfill: node IDs and every edge are untouched. Safe to run
// repeatedly (WHERE c.anchor_id IS NULL makes it a no-op on chunks already
// backfilled). Intended as a one-time operational step after upgrading to
// the match-in-place ingestion path (see docs/DESIGN.md's stable-identity
// note) — without it, the first post-upgrade re-ingest would treat every
// pre-existing chunk as "no match" and orphan all of them once.
func (s *Store) BackfillChunkAnchors(ctx context.Context) (int, error) {
	res, err := s.graph.Query(
		"MATCH (c:Chunk) WHERE c.anchor_id IS NULL RETURN ID(c), c.source, c.breadcrumb, c.content",
		nil, nil,
	)
	if err != nil {
		return 0, fmt.Errorf("fetching chunks needing backfill: %w", err)
	}
	type row struct {
		id                       int64
		source, breadcrumb, body string
	}
	var rows []row
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		sourceVal, _ := r.GetByIndex(1)
		breadcrumbVal, _ := r.GetByIndex(2)
		contentVal, _ := r.GetByIndex(3)
		rows = append(rows, row{
			id:         toInt64(idVal),
			source:     stringOrEmpty(sourceVal),
			breadcrumb: stringOrEmpty(breadcrumbVal),
			body:       stringOrEmpty(contentVal),
		})
	}

	count := 0
	for _, rw := range rows {
		anchorID := chunking.ComputeAnchorID(rw.source, rw.breadcrumb)
		contentHash := chunking.ComputeContentHash(rw.body)
		params := map[string]interface{}{
			"id":           rw.id,
			"anchor_id":    anchorID,
			"content_hash": contentHash,
		}
		_, err := s.graph.Query(
			"MATCH (c:Chunk) WHERE ID(c) = $id SET c.anchor_id = $anchor_id, c.content_hash = $content_hash",
			params, nil,
		)
		if err != nil {
			return count, fmt.Errorf("backfilling chunk %d: %w", rw.id, err)
		}
		count++
	}
	return count, nil
}

// ChunkAnchorRow is the existing on-disk identity state of one :Chunk for a
// source, read before a re-ingest decides create/update/rename/orphan for
// each new chunk. AnchorID/ContentHash are "" for legacy chunks that predate
// BackfillChunkAnchors and haven't been backfilled yet.
type ChunkAnchorRow struct {
	ID              int64
	AnchorID        string
	ContentHash     string
	Breadcrumb      string
	LastDistilledAt int64 // 0 if never set
	OrphanedAt      int64 // 0 if not orphaned
}

// FetchChunkAnchors returns the current identity state of every :Chunk for
// source, for the caller to match new chunks against before writing.
func (s *Store) FetchChunkAnchors(ctx context.Context, source string) ([]ChunkAnchorRow, error) {
	params := map[string]interface{}{"source": source}
	res, err := s.graph.Query(
		"MATCH (c:Chunk {source: $source}) "+
			"RETURN ID(c), c.anchor_id, c.content_hash, c.breadcrumb, c.last_distilled_at, c.orphaned_at",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("fetching chunk anchors for %s: %w", source, err)
	}
	var out []ChunkAnchorRow
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		anchorVal, _ := r.GetByIndex(1)
		hashVal, _ := r.GetByIndex(2)
		breadcrumbVal, _ := r.GetByIndex(3)
		lastDistilledVal, _ := r.GetByIndex(4)
		orphanedVal, _ := r.GetByIndex(5)
		out = append(out, ChunkAnchorRow{
			ID:              toInt64(idVal),
			AnchorID:        stringOrEmpty(anchorVal),
			ContentHash:     stringOrEmpty(hashVal),
			Breadcrumb:      stringOrEmpty(breadcrumbVal),
			LastDistilledAt: toInt64(lastDistilledVal),
			OrphanedAt:      toInt64(orphanedVal),
		})
	}
	return out, nil
}

// ChunkAction describes what UpsertChunk did with one chunk.
type ChunkAction string

const (
	ChunkCreated   ChunkAction = "created"   // no match found -- a genuinely new chunk
	ChunkUpdated   ChunkAction = "updated"   // same anchor_id, content changed -- updated in place
	ChunkRenamed   ChunkAction = "renamed"   // content unchanged, moved to a new heading -- carried forward under the new anchor_id
	ChunkUnchanged ChunkAction = "unchanged" // anchor_id AND content_hash both matched -- no write at all
)

// UpsertChunk resolves ONE new chunk against existing state for its source
// and either no-ops, updates an existing node in place, or creates a new
// node — it never deletes. anchorID/contentHash must be precomputed by the
// caller (chunking.ComputeAnchorID / ComputeContentHash) before this call,
// against the chunk's identity as it exists in this ingest pass — matching
// happens against `existing`, the state FetchChunkAnchors already read for
// this source, not against a fresh query per chunk.
//
// Matching order (both steps skip any row already in claimedIDs, so two new
// chunks in the same ingest pass can never both resolve to the same old row —
// e.g. two chunks that happen to compute the same anchor_id, such as
// duplicate heading text):
//  1. Exact anchor_id match in existing. Among unclaimed rows sharing that
//     anchor_id, prefer one whose content_hash also matches (truly
//     unchanged) over any other unclaimed same-anchor row; ties within each
//     preference go to the lowest ID. This keeps the choice independent of
//     `existing`'s iteration order, so a genuinely-unchanged duplicate-anchor
//     chunk is never mistaken for an update just because a different
//     unclaimed row of the same anchor_id happened to come first. If the
//     chosen row's content_hash matches: no write, return (existingID,
//     ChunkUnchanged). If it differs: SET content/breadcrumb/line_start/
//     line_end/content_hash/vector in place on that node — every incoming
//     edge (:MOTIVATES, :DOCUMENTS) survives untouched, last_distilled_at is
//     NOT touched. Return (existingID, ChunkUpdated).
//  2. No anchor_id match: look for an existing row whose content_hash
//     matches — same content under a different heading. Exactly one
//     candidate: carry it forward under the new anchor_id (SET
//     anchor_id/breadcrumb/line_start/line_end/vector in place, clear any
//     stale orphaned_at). More than one candidate: pick by longest common
//     breadcrumb-segment prefix (split on " > "), then lowest ID — the
//     unchosen candidates are marked ambiguous_at (not orphaned; they might
//     be legitimate duplicated content) for a later `reanchor` pass to
//     surface, but the write path still completes deterministically. Either
//     way, return (chosenID, ChunkRenamed).
//  3. No match at all: CREATE a new node with anchor_id/content_hash set,
//     :OF_FILE to fileID. Return (newID, ChunkCreated).
//
// claimedIDs accumulates IDs already resolved by earlier calls in the same
// reingestSource pass.
func (s *Store) UpsertChunk(ctx context.Context, source string, chunk chunking.Chunk, vector []float32, anchorID, contentHash string, oversized bool, fileID int64, existing []ChunkAnchorRow, claimedIDs map[int64]bool) (id int64, action ChunkAction, err error) {
	plan := ChooseChunkMatch(existing, claimedIDs, anchorID, contentHash, chunk.Breadcrumb)

	switch plan.Action {
	case ChunkUnchanged:
		claimedIDs[plan.Row.ID] = true
		return plan.Row.ID, ChunkUnchanged, nil

	case ChunkUpdated:
		if err := s.updateChunkInPlace(ctx, plan.Row.ID, chunk, anchorID, contentHash, oversized, vector); err != nil {
			return 0, "", fmt.Errorf("updating chunk %d in place: %w", plan.Row.ID, err)
		}
		claimedIDs[plan.Row.ID] = true
		return plan.Row.ID, ChunkUpdated, nil

	case ChunkRenamed:
		if err := s.updateChunkInPlace(ctx, plan.Row.ID, chunk, anchorID, contentHash, oversized, vector); err != nil {
			return 0, "", fmt.Errorf("renaming chunk %d in place: %w", plan.Row.ID, err)
		}
		claimedIDs[plan.Row.ID] = true
		for _, ambID := range plan.Ambiguous {
			if err := s.markAmbiguous(ctx, ambID); err != nil {
				return 0, "", fmt.Errorf("marking chunk %d ambiguous: %w", ambID, err)
			}
		}
		return plan.Row.ID, ChunkRenamed, nil

	default: // ChunkCreated
		newID, err := s.createChunk(ctx, source, chunk, vector, anchorID, contentHash, oversized, fileID)
		if err != nil {
			return 0, "", fmt.Errorf("creating chunk: %w", err)
		}
		claimedIDs[newID] = true
		return newID, ChunkCreated, nil
	}
}

// ChunkMatch is the outcome of ChooseChunkMatch's side-effect-free planning:
// what UpsertChunk would do for one incoming chunk, without doing it. Row is
// only meaningful when Found is true (i.e. Action is not ChunkCreated).
// Ambiguous carries the IDs of other Step-2 rename candidates that lost the
// tie-break, for the caller to mark ambiguous_at — planning never writes.
type ChunkMatch struct {
	Row       ChunkAnchorRow
	Action    ChunkAction
	Ambiguous []int64
	Found     bool
}

// ChooseChunkMatch runs UpsertChunk's full matching decision — Steps 1
// through 3 — as a pure function over existing/claimed, with no database
// access. It exists so two callers can't drift out of lockstep by each
// re-implementing (or partially re-implementing) the same matching rule:
// UpsertChunk calls this to decide what to do, then performs the
// corresponding write; pipeline.reingestSource calls this ahead of time,
// against the same existing/claimed state it will present to UpsertChunk
// later, to predict which chunks are truly unchanged (and so don't need
// embedding) — including Step 2 rename matches, not just Step 1 anchor
// matches. A prediction pass that only simulated Step 1 would leave Step-2-
// claimed rows unreserved, letting an unrelated later chunk in the same pass
// wrongly predict an anchor+hash match against a row Step 2 is about to
// claim for a different chunk.
//
// Matching order (both steps skip any row already in claimed, so two chunks
// in the same pass — real or predicted — can never both resolve to the same
// old row):
//  1. Exact anchor_id match. Among unclaimed rows sharing anchorID, prefer
//     one whose content_hash also matches (truly unchanged) over any other
//     unclaimed same-anchor row; ties within each preference tier go to the
//     lowest ID. Content_hash match -> ChunkUnchanged. Otherwise ->
//     ChunkUpdated.
//  2. No anchor_id match: look for unclaimed rows whose content_hash matches
//     under a different anchor_id — same content under a different heading.
//     Exactly one candidate: ChunkRenamed. More than one: pick by longest
//     common breadcrumb-segment prefix (split on " > ") against breadcrumb,
//     then lowest ID; the rest are returned in Ambiguous. Either way,
//     ChunkRenamed.
//  3. No match at all: ChunkCreated (Found is false; there is no Row).
func ChooseChunkMatch(existing []ChunkAnchorRow, claimed map[int64]bool, anchorID, contentHash, breadcrumb string) ChunkMatch {
	if chosen, ok := chooseAnchorMatch(existing, claimed, anchorID, contentHash); ok {
		if chosen.ContentHash == contentHash {
			return ChunkMatch{Row: chosen, Action: ChunkUnchanged, Found: true}
		}
		return ChunkMatch{Row: chosen, Action: ChunkUpdated, Found: true}
	}

	var candidates []ChunkAnchorRow
	for _, row := range existing {
		if claimed[row.ID] {
			continue
		}
		if row.ContentHash != "" && row.ContentHash == contentHash && row.AnchorID != anchorID {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) > 0 {
		chosen := candidates[0]
		bestPrefix := -1
		for _, cand := range candidates {
			prefix := commonBreadcrumbPrefixLen(cand.Breadcrumb, breadcrumb)
			if prefix > bestPrefix || (prefix == bestPrefix && cand.ID < chosen.ID) {
				bestPrefix = prefix
				chosen = cand
			}
		}
		var ambiguous []int64
		for _, cand := range candidates {
			if cand.ID != chosen.ID {
				ambiguous = append(ambiguous, cand.ID)
			}
		}
		return ChunkMatch{Row: chosen, Action: ChunkRenamed, Ambiguous: ambiguous, Found: true}
	}

	return ChunkMatch{Action: ChunkCreated, Found: false}
}

// chooseAnchorMatch picks, among rows in existing that are not in claimed and
// whose AnchorID equals anchorID, the one ChooseChunkMatch's Step 1 claims:
// prefer a row whose ContentHash also equals contentHash (truly unchanged)
// over any other unclaimed same-anchor row; break ties in either preference
// tier by lowest ID. Returns ok=false if no unclaimed row has that anchor_id.
func chooseAnchorMatch(existing []ChunkAnchorRow, claimed map[int64]bool, anchorID, contentHash string) (ChunkAnchorRow, bool) {
	var chosen ChunkAnchorRow
	haveChosen := false
	haveExact := false
	for _, row := range existing {
		if claimed[row.ID] || row.AnchorID == "" || row.AnchorID != anchorID {
			continue
		}
		exact := row.ContentHash == contentHash
		switch {
		case !haveChosen:
			chosen, haveChosen, haveExact = row, true, exact
		case exact && !haveExact:
			chosen, haveExact = row, true
		case exact == haveExact && row.ID < chosen.ID:
			chosen = row
		}
	}
	return chosen, haveChosen
}

// updateChunkInPlace SETs content/breadcrumb/position/content_hash/vector on
// an existing node, WITHOUT touching last_distilled_at, and clears
// orphaned_at/ambiguous_at (a node being written to again is no longer
// orphaned or ambiguous). anchor_id is always re-set (a no-op for the
// same-anchor update case, the actual change for the rename case).
func (s *Store) updateChunkInPlace(ctx context.Context, id int64, chunk chunking.Chunk, anchorID, contentHash string, oversized bool, vector []float32) error {
	params := map[string]interface{}{
		"id":           id,
		"content":      chunk.Content,
		"breadcrumb":   chunk.Breadcrumb,
		"line_start":   chunk.LineStart,
		"line_end":     chunk.LineEnd,
		"anchor_id":    anchorID,
		"content_hash": contentHash,
	}
	setClause := "SET c.content = $content, c.breadcrumb = $breadcrumb, c.line_start = $line_start, " +
		"c.line_end = $line_end, c.anchor_id = $anchor_id, c.content_hash = $content_hash, " +
		"c.orphaned_at = NULL, c.ambiguous_at = NULL"
	if oversized {
		setClause += ", c.oversized = true"
	} else if len(vector) > 0 {
		params["vector"] = float32ToInterface(vector)
		setClause += ", c.vector = vecf32($vector), c.oversized = NULL"
	}
	_, err := s.graph.Query(
		"MATCH (c:Chunk) WHERE ID(c) = $id "+setClause,
		params, nil,
	)
	return err
}

// markAmbiguous sets ambiguous_at=now on a chunk that lost a content_hash
// rename-match tie-break to another candidate. Never orphans or deletes it —
// it might be legitimately duplicated content, not a stale copy.
func (s *Store) markAmbiguous(ctx context.Context, id int64) error {
	params := map[string]interface{}{"id": id, "now": time.Now().UnixMilli()}
	_, err := s.graph.Query(
		"MATCH (c:Chunk) WHERE ID(c) = $id SET c.ambiguous_at = $now",
		params, nil,
	)
	return err
}

// createChunk CREATEs a genuinely new :Chunk node with identity fields set
// and a :OF_FILE edge to fileID.
func (s *Store) createChunk(ctx context.Context, source string, chunk chunking.Chunk, vector []float32, anchorID, contentHash string, oversized bool, fileID int64) (int64, error) {
	params := map[string]interface{}{
		"file_id":      fileID,
		"source":       source,
		"breadcrumb":   chunk.Breadcrumb,
		"content":      chunk.Content,
		"line_start":   chunk.LineStart,
		"line_end":     chunk.LineEnd,
		"anchor_id":    anchorID,
		"content_hash": contentHash,
	}
	props := "source: $source, breadcrumb: $breadcrumb, content: $content, line_start: $line_start, " +
		"line_end: $line_end, anchor_id: $anchor_id, content_hash: $content_hash"
	if oversized {
		props += ", oversized: true"
	} else if len(vector) > 0 {
		params["vector"] = float32ToInterface(vector)
		props += ", vector: vecf32($vector)"
	}
	res, err := s.graph.Query(
		"MATCH (f:File) WHERE ID(f) = $file_id "+
			"CREATE (c:Chunk {"+props+"}) "+
			"CREATE (c)-[:OF_FILE]->(f) "+
			"RETURN ID(c)",
		params, nil,
	)
	if err != nil {
		return 0, err
	}
	if !res.Next() {
		return 0, fmt.Errorf("creating chunk: no result returned")
	}
	r := res.Record()
	idVal, _ := r.GetByIndex(0)
	return toInt64(idVal), nil
}

// MarkOrphanedChunks sets orphaned_at=now on every :Chunk for source whose ID
// is NOT in keepIDs and isn't already orphaned. Never deletes. Returns the
// newly-orphaned IDs (needed to report which observations are affected via
// ObservationsMotivatingChunks).
func (s *Store) MarkOrphanedChunks(ctx context.Context, source string, keepIDs []int64, now int64) ([]int64, error) {
	params := map[string]interface{}{
		"source":   source,
		"keep_ids": int64ToInterface(keepIDs),
		"now":      now,
	}
	res, err := s.graph.Query(
		"MATCH (c:Chunk {source: $source}) "+
			"WHERE NOT ID(c) IN $keep_ids AND c.orphaned_at IS NULL "+
			"SET c.orphaned_at = $now "+
			"RETURN ID(c)",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("marking orphaned chunks for %s: %w", source, err)
	}
	var out []int64
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		out = append(out, toInt64(idVal))
	}
	return out, nil
}

// ObservationsMotivatingChunks returns (chunkID, observationID) pairs for
// every :MOTIVATES edge whose target is in chunkIDs — used to report which
// observations are affected when a chunk is newly orphaned.
func (s *Store) ObservationsMotivatingChunks(ctx context.Context, chunkIDs []int64) ([]struct{ ChunkID, ObservationID int64 }, error) {
	if len(chunkIDs) == 0 {
		return nil, nil
	}
	params := map[string]interface{}{"chunk_ids": int64ToInterface(chunkIDs)}
	res, err := s.graph.Query(
		"MATCH (o:Observation)-[:MOTIVATES]->(c:Chunk) WHERE ID(c) IN $chunk_ids RETURN ID(c), ID(o)",
		params, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("finding observations motivating chunks: %w", err)
	}
	var out []struct{ ChunkID, ObservationID int64 }
	for res.Next() {
		r := res.Record()
		chunkVal, _ := r.GetByIndex(0)
		obsVal, _ := r.GetByIndex(1)
		out = append(out, struct{ ChunkID, ObservationID int64 }{
			ChunkID:       toInt64(chunkVal),
			ObservationID: toInt64(obsVal),
		})
	}
	return out, nil
}

// commonBreadcrumbPrefixLen returns how many leading " > "-separated
// segments two breadcrumbs share — used to break ties when more than one
// old chunk matches a new chunk's content_hash during a rename resolution.
func commonBreadcrumbPrefixLen(a, b string) int {
	as := strings.Split(a, " > ")
	bs := strings.Split(b, " > ")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

func stringOrEmpty(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func int64ToInterface(v []int64) []interface{} {
	out := make([]interface{}, len(v))
	for i, n := range v {
		out[i] = n
	}
	return out
}

// DeleteBySource removes all Chunk nodes for a given source path.
//
// NOT used by the ingest path since the match-in-place rewrite (see
// docs/DESIGN.md's stable-identity note) — UpsertChunk/MarkOrphanedChunks
// handle re-ingestion without deleting anything, precisely so :MOTIVATES
// edges survive ordinary edits. This remains a lower-level primitive for an
// explicit "hard delete a source" operation; it bypasses orphan-marking
// entirely, so anything reaching for it should understand it is NOT the
// anchor-preserving path.
func (s *Store) DeleteBySource(ctx context.Context, source string) (int, error) {
	params := map[string]interface{}{"source": source}
	res, err := s.graph.Query(
		"MATCH (c:Chunk {source: $source}) DETACH DELETE c",
		params, nil,
	)
	if err != nil {
		return 0, err
	}
	return res.NodesDeleted(), nil
}

// StoreOversizedChunks creates :Chunk nodes for sections that exceed the size
// threshold the caller enforces. They carry full content and OF_FILE anchoring
// (so the linker can scan them for identifier mentions and graph traversal
// surfaces them) but have no vector — they are invisible to KNN search. Marks
// each node with oversized=true.
func (s *Store) StoreOversizedChunks(ctx context.Context, chunks []chunking.Chunk, fileIDs []int64) (int, error) {
	if len(chunks) != len(fileIDs) {
		return 0, fmt.Errorf("chunks (%d) and fileIDs (%d) length mismatch", len(chunks), len(fileIDs))
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	stored := 0
	for i, chunk := range chunks {
		params := map[string]interface{}{
			"file_id":    fileIDs[i],
			"breadcrumb": chunk.Breadcrumb,
			"content":    chunk.Content,
			"source":     chunk.Source,
			"line_start": chunk.LineStart,
			"line_end":   chunk.LineEnd,
		}
		_, err := s.graph.Query(
			"MATCH (f:File) WHERE ID(f) = $file_id "+
				"CREATE (c:Chunk {breadcrumb: $breadcrumb, content: $content, source: $source, line_start: $line_start, line_end: $line_end, oversized: true}) "+
				"CREATE (c)-[:OF_FILE]->(f)",
			params, nil,
		)
		if err != nil {
			return stored, fmt.Errorf("creating oversized chunk node %d: %w", i, err)
		}
		stored++
	}
	return stored, nil
}

// ChunkRow is a minimal projection of a :Chunk node for linker scanning.
type ChunkRow struct {
	ID      int64
	Content string
}

// FetchChunks returns chunks for linking. If source is empty, returns all
// chunks in the graph; otherwise filters by c.source. Orphaned chunks
// (orphaned_at set) are excluded — they're marked for review, not live
// content, and re-linking them would keep asserting new DOCUMENTS edges from
// stale material every re-ingest.
func (s *Store) FetchChunks(ctx context.Context, source string) ([]ChunkRow, error) {
	var (
		res    *falkordb.QueryResult
		err    error
		params = map[string]interface{}{}
	)
	if source == "" {
		res, err = s.graph.Query("MATCH (c:Chunk) WHERE c.orphaned_at IS NULL RETURN ID(c), c.content", nil, nil)
	} else {
		params["source"] = source
		res, err = s.graph.Query("MATCH (c:Chunk {source: $source}) WHERE c.orphaned_at IS NULL RETURN ID(c), c.content", params, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching chunks: %w", err)
	}
	var out []ChunkRow
	for res.Next() {
		r := res.Record()
		idVal, _ := r.GetByIndex(0)
		contentVal, _ := r.GetByIndex(1)
		out = append(out, ChunkRow{
			ID:      toInt64(idVal),
			Content: fmt.Sprint(contentVal),
		})
	}
	return out, nil
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
