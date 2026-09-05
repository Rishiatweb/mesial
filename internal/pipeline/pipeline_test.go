package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mknw/h9s/internal/embedding"
	"github.com/mknw/h9s/internal/falkorstore"
)

// newIngestTestStore connects to a throwaway graph for the test, skipping if
// FalkorDB is unreachable — mirrors the pattern already used in
// internal/falkorstore's tests (newMemoryTestStore), duplicated here since
// falkorstore's helper is unexported and this is a different package.
func newIngestTestStore(t *testing.T) *falkorstore.Store {
	t.Helper()
	addr := os.Getenv("FALKOR_ADDR")
	if addr == "" {
		addr = "localhost:6381"
	}
	graphName := fmt.Sprintf("test_pipeline_%d_%s", time.Now().UnixNano(), sanitize(t.Name()))
	store, err := falkorstore.NewStore(addr, graphName)
	if err != nil {
		t.Skipf("FalkorDB unreachable at %s: %v", addr, err)
	}
	if err := store.Ping(context.Background()); err != nil {
		t.Skipf("FalkorDB ping failed at %s: %v", addr, err)
	}
	if err := store.EnsureIndex(context.Background(), EmbeddingDim); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := EnsureMemoryReady(context.Background(), store, "code"); err != nil {
		t.Fatalf("EnsureMemoryReady: %v", err)
	}
	t.Cleanup(func() {
		_ = store.DeleteAllNodes(context.Background())
	})
	return store
}

func sanitize(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out[i] = c
		} else {
			out[i] = '_'
		}
	}
	return string(out)
}

func testEmbedder(t *testing.T) *embedding.Client {
	t.Helper()
	url := os.Getenv("EMBEDDING_URL")
	if url == "" {
		url = "http://localhost:8090"
	}
	embedder := embedding.NewClient(url, EmbeddingDim)
	if _, err := embedder.Embed(context.Background(), []string{"connectivity check"}); err != nil {
		t.Skipf("llama-server unreachable at %s: %v", url, err)
	}
	return embedder
}

// motivatesEdgeExists is a direct Cypher check, independent of any pipeline
// function, that an (:Observation)-[:MOTIVATES]->(:Chunk) edge exists
// between the given IDs — used so these tests confirm the actual graph
// state, not just what a function claimed to do.
func motivatesEdgeExists(t *testing.T, store *falkorstore.Store, obsID, chunkID int64) bool {
	t.Helper()
	rows, err := store.ObservationsMotivatingChunks(context.Background(), []int64{chunkID})
	if err != nil {
		t.Fatalf("ObservationsMotivatingChunks: %v", err)
	}
	for _, r := range rows {
		if r.ChunkID == chunkID && r.ObservationID == obsID {
			return true
		}
	}
	return false
}

func chunkAnchorRow(t *testing.T, store *falkorstore.Store, source string, chunkID int64) falkorstore.ChunkAnchorRow {
	t.Helper()
	rows, err := store.FetchChunkAnchors(context.Background(), source)
	if err != nil {
		t.Fatalf("FetchChunkAnchors: %v", err)
	}
	for _, r := range rows {
		if r.ID == chunkID {
			return r
		}
	}
	t.Fatalf("chunk %d not found in FetchChunkAnchors for %s", chunkID, source)
	return falkorstore.ChunkAnchorRow{}
}

// TestReingestSource_EditUnderSameHeadingPreservesIdentityAndEvidence is the
// scenario this entire increment exists to fix: an :Observation is attached
// to a chunk via :MOTIVATES, the chunk's content is edited (same heading),
// and the edge — and the chunk's node ID — must survive the re-ingest.
// Before this increment, IngestDocs deleted and recreated every chunk on
// every call, which silently dropped this edge every time.
func TestReingestSource_EditUnderSameHeadingPreservesIdentityAndEvidence(t *testing.T) {
	store := newIngestTestStore(t)
	embedder := testEmbedder(t)
	ctx := context.Background()

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.md")
	original := "# Fixture\n\n## Setup\n\nOriginal paragraph about setup steps.\n"
	if err := os.WriteFile(fixture, []byte(original), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	report1, err := reingestSource(ctx, store, embedder, fixture, false)
	if err != nil {
		t.Fatalf("first reingestSource: %v", err)
	}
	// Only one chunk: the "# Fixture" heading has no content before "## Setup"
	// follows it, and chunking.ChunkFile skips heading levels with empty
	// content — so this fixture produces exactly one chunk, "Fixture > Setup".
	if report1 == nil || len(report1.Created) != 1 {
		t.Fatalf("expected 1 created chunk on first ingest, got %+v", report1)
	}

	// Find the "Setup" chunk's ID (the one we'll attach an observation to).
	anchors, err := store.FetchChunkAnchors(ctx, fixture)
	if err != nil {
		t.Fatalf("FetchChunkAnchors: %v", err)
	}
	var setupChunkID int64
	for _, a := range anchors {
		if a.Breadcrumb == "Fixture > Setup" {
			setupChunkID = a.ID
		}
	}
	if setupChunkID == 0 {
		t.Fatalf("could not find 'Fixture > Setup' chunk among %+v", anchors)
	}

	obsID, err := AddObservation(ctx, store, embedder, "setup steps are documented here", 5)
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if err := LinkObservationMotivatesChunk(ctx, store, obsID.ObservationID, setupChunkID); err != nil {
		t.Fatalf("LinkObservationMotivatesChunk: %v", err)
	}
	if !motivatesEdgeExists(t, store, obsID.ObservationID, setupChunkID) {
		t.Fatal("MOTIVATES edge not found immediately after linking — test setup is broken")
	}

	beforeRow := chunkAnchorRow(t, store, fixture, setupChunkID)
	if beforeRow.LastDistilledAt == 0 {
		t.Fatal("expected last_distilled_at to be set after LinkObservationMotivatesChunk")
	}

	// Edit the paragraph under the SAME heading.
	edited := "# Fixture\n\n## Setup\n\nRevised paragraph — the setup steps changed.\n"
	if err := os.WriteFile(fixture, []byte(edited), 0o644); err != nil {
		t.Fatalf("editing fixture: %v", err)
	}

	report2, err := reingestSource(ctx, store, embedder, fixture, false)
	if err != nil {
		t.Fatalf("second reingestSource: %v", err)
	}

	if len(report2.Updated) != 1 || report2.Updated[0] != setupChunkID {
		t.Fatalf("expected the Setup chunk (%d) to be reported as Updated, got report: %+v", setupChunkID, report2)
	}
	if len(report2.Created) != 0 {
		t.Errorf("expected no newly-created chunks on an edit-only re-ingest, got %+v", report2.Created)
	}
	if len(report2.Orphaned) != 0 {
		t.Errorf("expected no orphaned chunks on an edit-only re-ingest, got %+v", report2.Orphaned)
	}

	// The core assertions: same node ID, edge intact, content_hash changed,
	// last_distilled_at UNCHANGED.
	if !motivatesEdgeExists(t, store, obsID.ObservationID, setupChunkID) {
		t.Fatal("MOTIVATES edge was lost after editing the chunk's content under the same heading — this is exactly the bug this increment fixes")
	}
	afterRow := chunkAnchorRow(t, store, fixture, setupChunkID)
	if afterRow.ContentHash == beforeRow.ContentHash {
		t.Error("expected content_hash to change after editing the paragraph")
	}
	if afterRow.LastDistilledAt != beforeRow.LastDistilledAt {
		t.Errorf("expected last_distilled_at to be preserved across an in-place update, got %d before / %d after", beforeRow.LastDistilledAt, afterRow.LastDistilledAt)
	}
}

// TestReingestSource_RenameUnderNewHeadingCarriesIdentityForward covers the
// second resolution path: identical content moved under a different
// heading. The chunk must be recognized as the same chunk (by content_hash),
// carried forward under the new anchor_id, with its evidence intact — not
// treated as an orphan-plus-new-creation pair.
func TestReingestSource_RenameUnderNewHeadingCarriesIdentityForward(t *testing.T) {
	store := newIngestTestStore(t)
	embedder := testEmbedder(t)
	ctx := context.Background()

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.md")
	body := "A paragraph whose content never changes, only its heading does.\n"
	original := "# Fixture\n\n## Old Heading\n\n" + body
	if err := os.WriteFile(fixture, []byte(original), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	report1, err := reingestSource(ctx, store, embedder, fixture, false)
	if err != nil {
		t.Fatalf("first reingestSource: %v", err)
	}
	anchors, _ := store.FetchChunkAnchors(ctx, fixture)
	var targetID int64
	for _, a := range anchors {
		if a.Breadcrumb == "Fixture > Old Heading" {
			targetID = a.ID
		}
	}
	if targetID == 0 {
		t.Fatalf("could not find the target chunk among %+v (report: %+v)", anchors, report1)
	}

	obsID, err := AddObservation(ctx, store, embedder, "this paragraph is stable content", 5)
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if err := LinkObservationMotivatesChunk(ctx, store, obsID.ObservationID, targetID); err != nil {
		t.Fatalf("LinkObservationMotivatesChunk: %v", err)
	}

	renamed := "# Fixture\n\n## New Heading\n\n" + body
	if err := os.WriteFile(fixture, []byte(renamed), 0o644); err != nil {
		t.Fatalf("renaming heading: %v", err)
	}

	report2, err := reingestSource(ctx, store, embedder, fixture, false)
	if err != nil {
		t.Fatalf("second reingestSource: %v", err)
	}

	if len(report2.Renamed) != 1 || report2.Renamed[0] != targetID {
		t.Fatalf("expected the chunk (%d) to be reported as Renamed, got: %+v", targetID, report2)
	}
	if !motivatesEdgeExists(t, store, obsID.ObservationID, targetID) {
		t.Fatal("MOTIVATES edge was lost across a heading rename with unchanged content")
	}
	afterRow := chunkAnchorRow(t, store, fixture, targetID)
	if afterRow.Breadcrumb != "Fixture > New Heading" {
		t.Errorf("expected breadcrumb updated to the new heading, got %q", afterRow.Breadcrumb)
	}
}

// TestReingestSource_DeletedSectionIsOrphanedNotDropped covers the third
// path: content removed entirely. The old chunk must be marked orphaned,
// not deleted — its evidence must remain queryable, even if flagged for
// review.
func TestReingestSource_DeletedSectionIsOrphanedNotDropped(t *testing.T) {
	store := newIngestTestStore(t)
	embedder := testEmbedder(t)
	ctx := context.Background()

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.md")
	original := "# Fixture\n\n## Setup\n\nSetup content.\n\n## Teardown\n\nTeardown content.\n"
	if err := os.WriteFile(fixture, []byte(original), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := reingestSource(ctx, store, embedder, fixture, false); err != nil {
		t.Fatalf("first reingestSource: %v", err)
	}
	anchors, _ := store.FetchChunkAnchors(ctx, fixture)
	var teardownID int64
	for _, a := range anchors {
		if a.Breadcrumb == "Fixture > Teardown" {
			teardownID = a.ID
		}
	}
	if teardownID == 0 {
		t.Fatalf("could not find the Teardown chunk among %+v", anchors)
	}

	obsID, err := AddObservation(ctx, store, embedder, "teardown removes temp resources", 5)
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if err := LinkObservationMotivatesChunk(ctx, store, obsID.ObservationID, teardownID); err != nil {
		t.Fatalf("LinkObservationMotivatesChunk: %v", err)
	}

	// Remove the Teardown section entirely.
	trimmed := "# Fixture\n\n## Setup\n\nSetup content.\n"
	if err := os.WriteFile(fixture, []byte(trimmed), 0o644); err != nil {
		t.Fatalf("trimming fixture: %v", err)
	}

	report2, err := reingestSource(ctx, store, embedder, fixture, false)
	if err != nil {
		t.Fatalf("second reingestSource: %v", err)
	}

	if len(report2.Orphaned) != 1 || report2.Orphaned[0] != teardownID {
		t.Fatalf("expected the Teardown chunk (%d) to be reported as Orphaned, got: %+v", teardownID, report2)
	}
	// The node — and its evidence — must still exist, just marked.
	row := chunkAnchorRow(t, store, fixture, teardownID)
	if row.OrphanedAt == 0 {
		t.Error("expected orphaned_at to be set on the removed chunk")
	}
	if !motivatesEdgeExists(t, store, obsID.ObservationID, teardownID) {
		t.Fatal("MOTIVATES edge to an orphaned (not deleted) chunk should still exist")
	}
}
