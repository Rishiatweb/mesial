package falkorstore

import (
	"context"
	"testing"

	"github.com/mknw/h9s/internal/chunking"
)

// TestBackfillChunkAnchors seeds a chunk the way pre-Increment-1 code would
// have (no anchor_id/content_hash), runs the backfill, and confirms both
// properties are computed correctly from the chunk's already-stored fields
// AND that the node's ID and edges are untouched — this is a pure metadata
// SET, not a recreate.
func TestBackfillChunkAnchors(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	fileID, err := store.AddFile(ctx, "/repo/docs/README.md", "README.md", ".md")
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	// Seed a legacy-shaped chunk directly (no anchor_id/content_hash), plus
	// a :MOTIVATES edge, to confirm the backfill doesn't disturb either.
	params := map[string]interface{}{
		"file_id":    fileID,
		"breadcrumb": "README > Overview",
		"content":    "This is the overview section.",
		"source":     "/repo/docs/README.md",
		"line_start": 1,
		"line_end":   5,
	}
	res, err := store.graph.Query(
		"MATCH (f:File) WHERE ID(f) = $file_id "+
			"CREATE (c:Chunk {breadcrumb: $breadcrumb, content: $content, source: $source, line_start: $line_start, line_end: $line_end}) "+
			"CREATE (c)-[:OF_FILE]->(f) "+
			"RETURN ID(c)",
		params, nil,
	)
	if err != nil {
		t.Fatalf("seeding legacy chunk: %v", err)
	}
	if !res.Next() {
		t.Fatal("expected a chunk ID back")
	}
	r := res.Record()
	idVal, _ := r.GetByIndex(0)
	chunkID := toInt64(idVal)

	obsID, err := store.AddObservation(ctx, "an observation about the overview", make([]float32, 8))
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if err := store.LinkMotivates(ctx, chunkID, obsID); err != nil {
		t.Fatalf("LinkMotivates: %v", err)
	}

	count, err := store.BackfillChunkAnchors(ctx)
	if err != nil {
		t.Fatalf("BackfillChunkAnchors: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 chunk backfilled, got %d", count)
	}

	rows, err := store.FetchChunkAnchors(ctx, "/repo/docs/README.md")
	if err != nil {
		t.Fatalf("FetchChunkAnchors: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 chunk row, got %d", len(rows))
	}
	row := rows[0]
	if row.ID != chunkID {
		t.Fatalf("node ID changed across backfill: was %d, now %d — backfill must never recreate nodes", chunkID, row.ID)
	}
	wantAnchor := chunking.ComputeAnchorID("/repo/docs/README.md", "README > Overview")
	wantHash := chunking.ComputeContentHash("This is the overview section.")
	if row.AnchorID != wantAnchor {
		t.Errorf("anchor_id = %q, want %q (computed from stored source+breadcrumb)", row.AnchorID, wantAnchor)
	}
	if row.ContentHash != wantHash {
		t.Errorf("content_hash = %q, want %q (computed from stored content)", row.ContentHash, wantHash)
	}

	// Edge must have survived untouched.
	pairs, err := store.ObservationsMotivatingChunks(ctx, []int64{chunkID})
	if err != nil {
		t.Fatalf("ObservationsMotivatingChunks: %v", err)
	}
	found := false
	for _, p := range pairs {
		if p.ChunkID == chunkID && p.ObservationID == obsID {
			found = true
		}
	}
	if !found {
		t.Error("MOTIVATES edge did not survive the backfill")
	}

	// Re-running must be a no-op (idempotent).
	count2, err := store.BackfillChunkAnchors(ctx)
	if err != nil {
		t.Fatalf("second BackfillChunkAnchors: %v", err)
	}
	if count2 != 0 {
		t.Errorf("expected second backfill run to touch 0 chunks (already backfilled), touched %d", count2)
	}
}

// TestBackfillChunkAnchorsHandlesNilProperties covers a legacy :Chunk row
// missing breadcrumb (nil property, e.g. a pre-migration oversized chunk).
// Before this fix, BackfillChunkAnchors read it via raw fmt.Sprint(nil),
// producing the literal string "<nil>" baked into anchor_id/content_hash —
// a value a fresh ingest computing ComputeAnchorID(source, "") would never
// reproduce, permanently breaking match-in-place for that row.
func TestBackfillChunkAnchorsHandlesNilProperties(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	fileID, err := store.AddFile(ctx, "/repo/docs/legacy.md", "legacy.md", ".md")
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	params := map[string]interface{}{
		"file_id": fileID,
		"content": "Body with no breadcrumb on record.",
		"source":  "/repo/docs/legacy.md",
	}
	_, err = store.graph.Query(
		"MATCH (f:File) WHERE ID(f) = $file_id "+
			"CREATE (c:Chunk {content: $content, source: $source}) "+ // breadcrumb intentionally omitted -> nil property
			"CREATE (c)-[:OF_FILE]->(f)",
		params, nil,
	)
	if err != nil {
		t.Fatalf("seeding chunk with nil breadcrumb: %v", err)
	}

	if _, err := store.BackfillChunkAnchors(ctx); err != nil {
		t.Fatalf("BackfillChunkAnchors: %v", err)
	}

	rows, err := store.FetchChunkAnchors(ctx, "/repo/docs/legacy.md")
	if err != nil {
		t.Fatalf("FetchChunkAnchors: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 chunk row, got %d", len(rows))
	}
	wantAnchor := chunking.ComputeAnchorID("/repo/docs/legacy.md", "")
	if rows[0].AnchorID != wantAnchor {
		t.Errorf("anchor_id = %q, want %q (computed treating nil breadcrumb as \"\", not the string \"<nil>\")", rows[0].AnchorID, wantAnchor)
	}
}

// TestUpsertChunkClaimedIDsPreventsDoubleMatchInStep1 is the regression test
// for UpsertChunk's Step 1 (exact anchor_id match) not checking claimedIDs:
// two existing rows sharing the same anchor_id (duplicate heading text
// produces identical breadcrumbs -> identical ComputeAnchorID) must each
// resolve to a DIFFERENT existing row when two new chunks are upserted in
// the same pass, not both collapse onto whichever row Step 1 finds first.
func TestUpsertChunkClaimedIDsPreventsDoubleMatchInStep1(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	fileID, err := store.AddFile(ctx, "/repo/docs/dup.md", "dup.md", ".md")
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}

	const source = "/repo/docs/dup.md"
	const sharedAnchor = "shared-anchor-id" // simulates a real collision (duplicate heading text)

	seedA := chunking.Chunk{Breadcrumb: "Dup > Fixed", Content: "old content A", Source: source}
	seedB := chunking.Chunk{Breadcrumb: "Dup > Fixed", Content: "old content B", Source: source}
	idA, err := store.createChunk(ctx, source, seedA, nil, sharedAnchor, chunking.ComputeContentHash(seedA.Content), false, fileID)
	if err != nil {
		t.Fatalf("seeding chunk A: %v", err)
	}
	idB, err := store.createChunk(ctx, source, seedB, nil, sharedAnchor, chunking.ComputeContentHash(seedB.Content), false, fileID)
	if err != nil {
		t.Fatalf("seeding chunk B: %v", err)
	}

	existing, err := store.FetchChunkAnchors(ctx, source)
	if err != nil {
		t.Fatalf("FetchChunkAnchors: %v", err)
	}
	if len(existing) != 2 {
		t.Fatalf("expected 2 existing rows, got %d", len(existing))
	}

	// Two new chunks, same shared anchor_id, both content changed from what's
	// on disk -- the scenario that must land as two distinct ChunkUpdated
	// results, not the second call re-matching (and overwriting) row A again.
	claimedIDs := map[int64]bool{}
	newA := chunking.Chunk{Breadcrumb: "Dup > Fixed", Content: "new content A", Source: source}
	newB := chunking.Chunk{Breadcrumb: "Dup > Fixed", Content: "new content B", Source: source}

	gotIDA, actionA, err := store.UpsertChunk(ctx, source, newA, nil, sharedAnchor, chunking.ComputeContentHash(newA.Content), false, fileID, existing, claimedIDs)
	if err != nil {
		t.Fatalf("UpsertChunk A: %v", err)
	}
	gotIDB, actionB, err := store.UpsertChunk(ctx, source, newB, nil, sharedAnchor, chunking.ComputeContentHash(newB.Content), false, fileID, existing, claimedIDs)
	if err != nil {
		t.Fatalf("UpsertChunk B: %v", err)
	}

	if actionA != ChunkUpdated || actionB != ChunkUpdated {
		t.Fatalf("expected both to be ChunkUpdated, got A=%s B=%s", actionA, actionB)
	}
	if gotIDA == gotIDB {
		t.Fatalf("both chunks resolved to the same node (%d) — the second call re-matched an already-claimed row instead of falling through to the other candidate", gotIDA)
	}
	if !(gotIDA == idA || gotIDA == idB) || !(gotIDB == idA || gotIDB == idB) {
		t.Fatalf("resolved IDs (%d, %d) don't match the two seeded rows (%d, %d)", gotIDA, gotIDB, idA, idB)
	}

	rows, err := store.FetchChunks(ctx, source)
	if err != nil {
		t.Fatalf("FetchChunks: %v", err)
	}
	contents := map[int64]string{}
	for _, r := range rows {
		contents[r.ID] = r.Content
	}
	if contents[gotIDA] != newA.Content {
		t.Errorf("node %d content = %q, want %q — was it overwritten by the other chunk?", gotIDA, contents[gotIDA], newA.Content)
	}
	if contents[gotIDB] != newB.Content {
		t.Errorf("node %d content = %q, want %q — was it overwritten by the other chunk?", gotIDB, contents[gotIDB], newB.Content)
	}
}

// TestFetchChunksExcludesOrphaned is the regression test for FetchChunks
// re-linking orphaned chunks: a chunk marked orphaned_at must not come back
// from FetchChunks, since the linker calls it right after MarkOrphanedChunks
// in the same reingestSource pass and would otherwise keep asserting new
// DOCUMENTS edges from stale, marked-for-review content.
func TestFetchChunksExcludesOrphaned(t *testing.T) {
	store := newMemoryTestStore(t)
	ctx := context.Background()

	fileID, err := store.AddFile(ctx, "/repo/docs/orphan.md", "orphan.md", ".md")
	if err != nil {
		t.Fatalf("AddFile: %v", err)
	}
	const source = "/repo/docs/orphan.md"

	live := chunking.Chunk{Breadcrumb: "Orphan > Live", Content: "still relevant", Source: source}
	stale := chunking.Chunk{Breadcrumb: "Orphan > Stale", Content: "no longer relevant", Source: source}
	liveID, err := store.createChunk(ctx, source, live, nil, "anchor-live", chunking.ComputeContentHash(live.Content), false, fileID)
	if err != nil {
		t.Fatalf("seeding live chunk: %v", err)
	}
	staleID, err := store.createChunk(ctx, source, stale, nil, "anchor-stale", chunking.ComputeContentHash(stale.Content), false, fileID)
	if err != nil {
		t.Fatalf("seeding stale chunk: %v", err)
	}

	if _, err := store.MarkOrphanedChunks(ctx, source, []int64{liveID}, 1234); err != nil {
		t.Fatalf("MarkOrphanedChunks: %v", err)
	}

	rows, err := store.FetchChunks(ctx, source)
	if err != nil {
		t.Fatalf("FetchChunks(source): %v", err)
	}
	for _, r := range rows {
		if r.ID == staleID {
			t.Fatalf("FetchChunks(source) returned the orphaned chunk %d", staleID)
		}
	}
	foundLive := false
	for _, r := range rows {
		if r.ID == liveID {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatal("FetchChunks(source) should still return the non-orphaned chunk")
	}

	allRows, err := store.FetchChunks(ctx, "")
	if err != nil {
		t.Fatalf("FetchChunks(\"\"): %v", err)
	}
	for _, r := range allRows {
		if r.ID == staleID {
			t.Fatalf("FetchChunks(\"\") returned the orphaned chunk %d", staleID)
		}
	}
}
