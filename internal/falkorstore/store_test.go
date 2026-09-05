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
