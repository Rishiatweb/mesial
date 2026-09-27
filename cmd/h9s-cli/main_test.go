package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mknw/h9s/internal/falkorstore"
)

func falkorTestAddr() string {
	if addr := os.Getenv("FALKOR_ADDR"); addr != "" {
		return addr
	}
	return "localhost:6381"
}

func embeddingTestURL() string {
	if url := os.Getenv("EMBEDDING_URL"); url != "" {
		return url
	}
	return "http://localhost:8090"
}

// skipUnlessLiveInfra skips the test unless FalkorDB and the embedding
// server are both reachable — mirrors the skip convention used by
// internal/pipeline's tests, duplicated here since this is a different
// package and the check is a plain TCP dial rather than a store/client call.
func skipUnlessLiveInfra(t *testing.T) {
	t.Helper()
	addr := falkorTestAddr()
	embedAddr := strings.TrimPrefix(strings.TrimPrefix(embeddingTestURL(), "http://"), "https://")
	for _, a := range []string{addr, embedAddr} {
		conn, err := net.DialTimeout("tcp", a, 2*time.Second)
		if err != nil {
			t.Skipf("required service unreachable at %s: %v", a, err)
		}
		conn.Close()
	}
}

// TestIngestUnchangedReingestDoesNotReportNoFilesFound is the regression
// test for the stale "no files found" check: re-ingesting a source whose
// content hasn't changed produces ChunksStored == 0 && OversizedChunks == 0
// under match-in-place ingestion (nothing was created/updated/renamed), but
// is a real successful outcome, not "no .md files found".
func TestIngestUnchangedReingestDoesNotReportNoFilesFound(t *testing.T) {
	skipUnlessLiveInfra(t)

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.md")
	if err := os.WriteFile(fixture, []byte("# Fixture\n\n## Section\n\nContent that never changes.\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	repo := fmt.Sprintf("test_h9scli_ingest_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		store, err := falkorstore.NewStore(falkorTestAddr(), repo)
		if err != nil {
			return
		}
		defer store.Close()
		_ = store.DeleteGraph(context.Background())
	})

	first := runIngestSubprocess(t, dir, repo)
	if strings.Contains(first, "No .md files found") {
		t.Fatalf("first ingest unexpectedly reported no files found, output: %s", first)
	}

	second := runIngestSubprocess(t, dir, repo)
	if strings.Contains(second, "No .md files found") {
		t.Fatalf("second (unchanged) ingest reported no files found — this is the bug: output: %s", second)
	}
}

func runIngestSubprocess(t *testing.T, dir, repo string) string {
	t.Helper()
	cmd := exec.Command("go", "run", ".", "ingest", "--repo", repo, dir)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("h9s-cli ingest failed: %v\noutput: %s", err, out)
	}
	return string(out)
}
